package process

import (
	"context"
	"errors"
	"io"
	"path/filepath"
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

type StartRequest struct {
	Metadata    Metadata
	Policy      []byte
	Credentials []Credential
}

type Managed struct {
	controller *Controller
	child      Child
	identity   state.ListenerIdentity
	proof      state.LiveListenerProof
	token      string
}

func (m *Managed) Identity() state.ListenerIdentity { return m.identity }
func (m *Managed) Proof() state.LiveListenerProof   { return m.proof }
func (m *Managed) ListenerToken() string            { return m.token }
func (m *Managed) String() string                   { return "[REDACTED managed proxy process]" }
func (m *Managed) GoString() string                 { return "[REDACTED managed proxy process]" }

type Controller struct {
	Platform         Platform
	HandshakeTimeout time.Duration
	ControlTimeout   time.Duration
	StopGrace        time.Duration
	mu               sync.Mutex
	known            map[int]knownListener
}

type knownListener struct {
	controlAddress string
	executablePath string
	publicKey      string
	listenerToken  string
	proof          state.LiveListenerProof
	child          Child
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
	return &Managed{controller: c, child: child, identity: identity, proof: proof, token: handshake.ListenerToken}, nil
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
	c.mu.Unlock()
	if !ok || evidence.ExecutablePath != known.executablePath || !evidenceMatchesProof(evidence, known.proof) {
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
	}
	c.mu.Lock()
	delete(c.known, expected.PID)
	c.mu.Unlock()
	return nil
}

func (c *Controller) verifyHandshake(ctx context.Context, pid int, metadata Metadata, expectedChallenge string, value Handshake) (state.LiveListenerProof, Evidence, error) {
	if value.Version != ProtocolVersion || value.Kind != "handshake" || !validListenerAddress(value.Address) || value.ControlAddress == "" || len(value.ControlAddress) > 256 || validListenerToken(value.ListenerToken) == false || value.Challenge != expectedChallenge || !validChallenge(value.Challenge) {
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
	if response.Version != ProtocolVersion || response.Kind != "control_response" || response.Action != action || response.Challenge != challenge || !response.Accepted || response.PublicKey != known.publicKey {
		return state.LiveListenerProof{}, ErrUnauthorized
	}
	if err := verifyResponse(response.PublicKey, response.Signature, "control_response", action, challenge, response.Proof, true); err != nil {
		return state.LiveListenerProof{}, err
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
