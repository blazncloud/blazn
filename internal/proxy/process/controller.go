package process

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/blazncloud/blazn/internal/proxy/state"
)

// Evidence is observed independently from the process table and executable,
// never accepted from the listener itself.
type Evidence struct {
	PID                  int
	OwnerUID             int
	ProcessStartIdentity string
	ExecutablePath       string
	ExecutableIdentity   string
	BinaryDigest         string
}

type SpawnRequest struct {
	Executable string
	Argv       []string
	Bootstrap  io.Reader
	Handshake  io.Writer
}

type Child interface {
	PID() int
	Terminate() error
	Kill() error
	Wait(context.Context) error
}

// Platform is injected so this core is testable without starting or signaling
// a real process. Implementations must use anonymous pipes for Bootstrap and
// Handshake and must not add bootstrap data to argv or environment.
type Platform interface {
	Spawn(context.Context, SpawnRequest) (Child, error)
	Evidence(context.Context, int) (Evidence, bool, error)
	DialControl(context.Context, int, string) (io.ReadWriteCloser, error)
}

// PersistedListener is the minimum restart material a protected state owner
// must supply to rediscover a listener. PublicKey may be empty when the caller
// has a persisted key fingerprint: the signed response key is then bound to
// that fingerprint before authority is registered.
type PersistedListener struct {
	Proof          state.LiveListenerProof
	ControlAddress string
	ExecutablePath string
	PublicKey      string
	ListenerToken  string
}

// RestartDiscovery validates caller-supplied protected restart material
// against fresh OS evidence and the authenticated control protocol.
type RestartDiscovery interface {
	Discover(context.Context, PersistedListener) (state.LiveListenerProof, bool, error)
}

type StartRequest struct {
	Metadata    Metadata
	Policy      []byte
	Credentials []Credential
}

type Managed struct {
	controller  *Controller
	child       Child
	identity    state.ListenerIdentity
	proof       state.LiveListenerProof
	token       string
	environment []string
}

func (m *Managed) Identity() state.ListenerIdentity { return m.identity }
func (m *Managed) Proof() state.LiveListenerProof   { return m.proof }
func (m *Managed) ListenerToken() string            { return m.token }
func (m *Managed) ChildEnvironment(base []string) ([]string, error) {
	if m == nil || !validChildEnvironment(m.environment, m.identity.Address) {
		return nil, ErrUnavailable
	}
	result := make([]string, 0, len(base)+len(m.environment))
	for _, entry := range base {
		name, _, _ := strings.Cut(entry, "=")
		if !validEnvironmentName(name) {
			result = append(result, entry)
		}
	}
	return append(result, m.environment...), nil
}
func (m *Managed) Inspect(ctx context.Context) (state.LiveListenerProof, bool, error) {
	if m == nil || m.controller == nil {
		return state.LiveListenerProof{}, false, ErrUnavailable
	}
	return m.controller.Inspect(ctx, m.proof.PID)
}
func (m *Managed) Shutdown(ctx context.Context) error {
	if m == nil || m.controller == nil {
		return ErrUnavailable
	}
	return m.controller.Stop(ctx, m.proof)
}
func (m *Managed) String() string   { return "[REDACTED managed proxy process]" }
func (m *Managed) GoString() string { return "[REDACTED managed proxy process]" }

type Controller struct {
	Platform         Platform
	HandshakeTimeout time.Duration
	ControlTimeout   time.Duration
	StopGrace        time.Duration
	mu               sync.Mutex
	known            map[int]knownListener
	expected         map[int]expectedListener
}

type expectedListener struct {
	proof          state.LiveListenerProof
	executablePath string
}

type knownListener struct {
	controlAddress string
	executablePath string
	publicKey      string
	listenerToken  string
	proof          state.LiveListenerProof
	child          Child
}

// Expect registers non-authoritative state evidence for recovery. It grants no
// signal or control authority: it only lets Inspect classify fresh OS evidence
// that contradicts the exact recorded identity as safe PID reuse/absence.
func (c *Controller) Expect(proof state.LiveListenerProof, executablePath string) error {
	if c == nil || !validLiveProof(proof) || !filepath.IsAbs(executablePath) || filepath.Clean(executablePath) != executablePath {
		return ErrUnavailable
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.expected == nil {
		c.expected = make(map[int]expectedListener)
	}
	c.expected[proof.PID] = expectedListener{proof: proof, executablePath: executablePath}
	return nil
}

func (c *Controller) Start(ctx context.Context, request StartRequest) (_ *Managed, err error) {
	if c == nil || c.Platform == nil || validateMetadata(request.Metadata) != nil {
		return nil, ErrUnavailable
	}
	handshakeChallenge, challengeErr := freshChallenge()
	if challengeErr != nil {
		return nil, ErrUnavailable
	}
	bootstrap := Bootstrap{Version: ProtocolVersion, Kind: "bootstrap", Challenge: handshakeChallenge, Metadata: request.Metadata, Policy: append([]byte(nil), request.Policy...), Credentials: cloneCredentials(request.Credentials)}
	if validateBootstrap(bootstrap) != nil {
		zeroBootstrap(&bootstrap)
		return nil, ErrProtocol
	}
	bootstrapReader, bootstrapWriter := io.Pipe()
	handshakeReader, handshakeWriter := io.Pipe()
	child, err := c.Platform.Spawn(ctx, SpawnRequest{Executable: request.Metadata.BinaryPath, Argv: []string{request.Metadata.BinaryPath, "__proxy-listener-core", ProtocolVersion}, Bootstrap: bootstrapReader, Handshake: handshakeWriter})
	if err != nil || child == nil {
		_ = bootstrapReader.Close()
		_ = bootstrapWriter.Close()
		_ = handshakeReader.Close()
		_ = handshakeWriter.Close()
		zeroBootstrap(&bootstrap)
		if err == nil {
			return nil, ErrUnavailable
		}
		return nil, safeError(err)
	}
	writeDone := make(chan error, 1)
	writeComplete := false
	cleanup := true
	defer func() {
		_ = bootstrapWriter.Close()
		if !writeComplete {
			<-writeDone
		}
		zeroBootstrap(&bootstrap)
		_ = handshakeReader.Close()
		if cleanup {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), c.stopGrace())
			defer cancel()
			_ = c.cleanupChild(cleanupCtx, child)
		}
	}()
	go func() {
		writeDone <- writeFrame(bootstrapWriter, MaxBootstrapBytes, bootstrap)
		_ = bootstrapWriter.Close()
	}()
	handshakeCtx, cancel := context.WithTimeout(ctx, c.handshakeTimeout())
	defer cancel()
	var handshake Handshake
	if err := readFrameContext(handshakeCtx, handshakeReader, MaxHandshakeBytes, &handshake); err != nil {
		return nil, safeError(err)
	}
	var writeErr error
	select {
	case writeErr = <-writeDone:
		writeComplete = true
	case <-handshakeCtx.Done():
		_ = bootstrapWriter.CloseWithError(handshakeCtx.Err())
		writeErr = <-writeDone
		writeComplete = true
		return nil, safeError(errors.Join(writeErr, handshakeCtx.Err()))
	}
	if writeErr != nil {
		return nil, safeError(writeErr)
	}
	proof, evidence, err := c.verifyHandshake(handshakeCtx, child.PID(), request.Metadata, handshakeChallenge, handshake)
	if err != nil {
		return nil, err
	}
	identity := state.ListenerIdentity{PID: proof.PID, ProcessStartIdentity: proof.ProcessStartIdentity, ExecutableIdentity: proof.ExecutableIdentity, Address: handshake.Address, ListenerKeyFingerprint: proof.ListenerKeyFingerprint}
	c.mu.Lock()
	if c.known == nil {
		c.known = map[int]knownListener{}
	}
	c.known[proof.PID] = knownListener{controlAddress: handshake.ControlAddress, executablePath: request.Metadata.BinaryPath, publicKey: handshake.PublicKey, listenerToken: handshake.ListenerToken, proof: proof, child: child}
	c.mu.Unlock()
	_ = evidence
	cleanup = false
	return &Managed{controller: c, child: child, identity: identity, proof: proof, token: handshake.ListenerToken, environment: append([]string(nil), handshake.Environment...)}, nil
}

func (c *Controller) Inspect(ctx context.Context, pid int) (state.LiveListenerProof, bool, error) {
	if c == nil || c.Platform == nil || pid < 1 {
		return state.LiveListenerProof{}, false, ErrUnavailable
	}
	evidence, live, err := c.Platform.Evidence(ctx, pid)
	if err != nil || !live {
		return state.LiveListenerProof{}, live, safeError(err)
	}
	c.mu.Lock()
	known, ok := c.known[pid]
	expected, expectedOK := c.expected[pid]
	c.mu.Unlock()
	if !ok {
		if expectedOK && (evidence.ExecutablePath != expected.executablePath || !evidenceMatchesProof(evidence, expected.proof)) {
			return state.LiveListenerProof{}, false, nil
		}
		// A live same-UID process at the recorded PID is not proof that it is
		// ours. Fail closed unless the caller first authenticates restart material
		// through Discover, so recovery neither signals the PID nor restores
		// publication beneath an unverified listener.
		return state.LiveListenerProof{}, false, ErrRestartMaterialUnavailable
	}
	if evidence.ExecutablePath != known.executablePath || !evidenceMatchesProof(evidence, known.proof) {
		return state.LiveListenerProof{}, false, nil
	}
	proof, err := c.control(ctx, pid, "inspect", known)
	if err != nil {
		return state.LiveListenerProof{}, false, err
	}
	if proof != known.proof || evidence.ExecutablePath != known.executablePath || !evidenceMatchesProof(evidence, proof) {
		return state.LiveListenerProof{}, false, ErrUnauthorized
	}
	return proof, true, nil
}

func (c *Controller) Discover(ctx context.Context, persisted PersistedListener) (state.LiveListenerProof, bool, error) {
	if c == nil || c.Platform == nil || !validLiveProof(persisted.Proof) || !filepath.IsAbs(persisted.ExecutablePath) || filepath.Clean(persisted.ExecutablePath) != persisted.ExecutablePath || !filepath.IsAbs(persisted.ControlAddress) || filepath.Clean(persisted.ControlAddress) != persisted.ControlAddress || len(persisted.ControlAddress) > 256 || !validListenerToken(persisted.ListenerToken) {
		return state.LiveListenerProof{}, false, ErrUnavailable
	}
	evidence, live, err := c.Platform.Evidence(ctx, persisted.Proof.PID)
	if err != nil || !live {
		return state.LiveListenerProof{}, live, safeError(err)
	}
	if evidence.ExecutablePath != persisted.ExecutablePath || !evidenceMatchesProof(evidence, persisted.Proof) {
		return state.LiveListenerProof{}, false, ErrUnauthorized
	}
	known := knownListener{controlAddress: persisted.ControlAddress, executablePath: persisted.ExecutablePath, publicKey: persisted.PublicKey, listenerToken: persisted.ListenerToken, proof: persisted.Proof}
	proof, err := c.control(ctx, persisted.Proof.PID, "inspect", known)
	if err != nil || proof != persisted.Proof {
		return state.LiveListenerProof{}, false, errors.Join(ErrUnauthorized, err)
	}
	c.mu.Lock()
	if c.known == nil {
		c.known = map[int]knownListener{}
	}
	c.known[persisted.Proof.PID] = known
	delete(c.expected, persisted.Proof.PID)
	c.mu.Unlock()
	return proof, true, nil
}

func (c *Controller) Stop(ctx context.Context, expected state.LiveListenerProof) error {
	current, live, err := c.Inspect(ctx, expected.PID)
	if err != nil {
		return err
	}
	if !live || current != expected {
		return ErrUnauthorized
	}
	c.mu.Lock()
	known := c.known[expected.PID]
	c.mu.Unlock()
	proof, err := c.control(ctx, expected.PID, "stop", known)
	if err != nil || proof != expected {
		return errors.Join(ErrUnauthorized, err)
	}
	if known.child != nil {
		waitCtx, cancel := context.WithTimeout(ctx, c.stopGrace())
		defer cancel()
		if err := known.child.Wait(waitCtx); err != nil {
			if cleanupErr := c.cleanupChild(ctx, known.child); cleanupErr != nil {
				return safeError(cleanupErr)
			}
		}
	} else if err := c.waitForDiscoveredExit(ctx, expected, known.executablePath); err != nil {
		// Keep the authenticated authority registered. Recovery must be able to
		// retry a stalled exit, while a reused PID must never be signaled.
		return err
	}
	c.mu.Lock()
	delete(c.known, expected.PID)
	c.mu.Unlock()
	return nil
}

func (c *Controller) waitForDiscoveredExit(ctx context.Context, expected state.LiveListenerProof, executablePath string) error {
	waitCtx, cancel := context.WithTimeout(ctx, c.stopGrace())
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		evidence, live, err := c.Platform.Evidence(waitCtx, expected.PID)
		if err != nil {
			return safeError(err)
		}
		if !live {
			return nil
		}
		if evidence.ExecutablePath != executablePath || !evidenceMatchesProof(evidence, expected) {
			return ErrUnauthorized
		}
		select {
		case <-ticker.C:
		case <-waitCtx.Done():
			return safeError(waitCtx.Err())
		}
	}
}

func (c *Controller) verifyHandshake(ctx context.Context, pid int, metadata Metadata, expectedChallenge string, value Handshake) (state.LiveListenerProof, Evidence, error) {
	if value.Version != ProtocolVersion || value.Kind != "handshake" || !validListenerAddress(value.Address) || value.ControlAddress == "" || len(value.ControlAddress) > 256 || validListenerToken(value.ListenerToken) == false || !validChildEnvironment(value.Environment, value.Address) || value.Challenge != expectedChallenge || !validChallenge(value.Challenge) {
		return state.LiveListenerProof{}, Evidence{}, ErrProtocol
	}
	proof := proofFromWire(value.Proof)
	if err := verifyResponse(value.PublicKey, value.Signature, "handshake", "start", value.Challenge, value.Proof, true); err != nil {
		return state.LiveListenerProof{}, Evidence{}, err
	}
	evidence, live, err := c.Platform.Evidence(ctx, pid)
	if err != nil || !live {
		return state.LiveListenerProof{}, Evidence{}, safeError(err)
	}
	if !validLiveProof(proof) || !validEvidence(evidence) || proof.PID != pid || proof.OwnerUID != metadata.OwnerUID || proof.BinaryDigest != metadata.BinaryDigest || proof.ActivationNonce != metadata.Nonce || proof.Generation != metadata.Generation || proof.Mode != metadata.Mode || proof.SessionIdentity != metadata.SessionIdentity || evidence.ExecutablePath != metadata.BinaryPath || !evidenceMatchesProof(evidence, proof) {
		return state.LiveListenerProof{}, Evidence{}, ErrUnauthorized
	}
	return proof, evidence, nil
}

func (c *Controller) control(ctx context.Context, pid int, action string, known knownListener) (state.LiveListenerProof, error) {
	challenge, err := freshChallenge()
	if err != nil {
		return state.LiveListenerProof{}, ErrUnavailable
	}
	controlCtx, cancel := context.WithTimeout(ctx, c.controlTimeout())
	defer cancel()
	connection, err := c.Platform.DialControl(controlCtx, pid, known.controlAddress)
	if err != nil {
		return state.LiveListenerProof{}, safeError(err)
	}
	defer connection.Close()
	authenticator, err := authenticateControlRequest(known.listenerToken, action, challenge, proofToWire(known.proof))
	if err != nil {
		return state.LiveListenerProof{}, err
	}
	if err := writeFrameContext(controlCtx, connection, MaxControlBytes, ControlRequest{Version: ProtocolVersion, Kind: "control_request", Action: action, Challenge: challenge, Authenticator: authenticator}); err != nil {
		return state.LiveListenerProof{}, safeError(err)
	}
	var response ControlResponse
	if err := readFrameContext(controlCtx, connection, MaxControlBytes, &response); err != nil {
		return state.LiveListenerProof{}, safeError(err)
	}
	if response.Version != ProtocolVersion || response.Kind != "control_response" || response.Action != action || response.Challenge != challenge || !response.Accepted || (known.publicKey != "" && response.PublicKey != known.publicKey) {
		return state.LiveListenerProof{}, ErrUnauthorized
	}
	if err := verifyResponse(response.PublicKey, response.Signature, "control_response", action, challenge, response.Proof, true); err != nil {
		return state.LiveListenerProof{}, err
	}
	if fingerprintFromEncodedPublicKey(response.PublicKey) != known.proof.ListenerKeyFingerprint {
		return state.LiveListenerProof{}, ErrUnauthorized
	}
	return proofFromWire(response.Proof), nil
}

func evidenceMatchesProof(evidence Evidence, proof state.LiveListenerProof) bool {
	return validEvidence(evidence) && validLiveProof(proof) && evidence.PID == proof.PID && evidence.OwnerUID == proof.OwnerUID && evidence.ProcessStartIdentity == proof.ProcessStartIdentity && evidence.ExecutableIdentity == proof.ExecutableIdentity && evidence.BinaryDigest == proof.BinaryDigest
}

func validEvidence(evidence Evidence) bool {
	return evidence.PID > 0 &&
		evidence.OwnerUID >= 0 &&
		validOpaqueIdentity(evidence.ProcessStartIdentity) &&
		filepath.IsAbs(evidence.ExecutablePath) &&
		filepath.Clean(evidence.ExecutablePath) == evidence.ExecutablePath &&
		executableIdentityPattern.MatchString(evidence.ExecutableIdentity) &&
		validDigest(evidence.BinaryDigest)
}

func (c *Controller) cleanupChild(ctx context.Context, child Child) error {
	if child == nil {
		return nil
	}
	bounded, cancelBounded := context.WithTimeout(ctx, c.stopGrace())
	defer cancelBounded()
	_ = child.Terminate()
	graceCtx, cancel := context.WithTimeout(bounded, c.stopGrace()/2)
	err := child.Wait(graceCtx)
	cancel()
	if err == nil {
		return nil
	}
	_ = child.Kill()
	return child.Wait(bounded)
}

func (c *Controller) handshakeTimeout() time.Duration {
	if c.HandshakeTimeout > 0 {
		return c.HandshakeTimeout
	}
	return DefaultHandshakeTTL
}
func (c *Controller) controlTimeout() time.Duration {
	if c.ControlTimeout > 0 {
		return c.ControlTimeout
	}
	return DefaultControlTTL
}
func (c *Controller) stopGrace() time.Duration {
	if c.StopGrace > 0 {
		return c.StopGrace
	}
	return 5 * time.Second
}

func cloneCredentials(values []Credential) []Credential {
	result := make([]Credential, len(values))
	for index := range values {
		result[index] = Credential{Reference: values[index].Reference, Value: append([]byte(nil), values[index].Value...)}
	}
	return result
}
