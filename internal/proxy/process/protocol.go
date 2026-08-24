// Package process defines the bounded parent/child protocol for a durable
// proxy listener. Platform process discovery and service publication are
// deliberately outside this package.
package process

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/blazncloud/blazn/internal/proxy/state"
)

const (
	ProtocolVersion     = "proxy-process/v1"
	MaxBootstrapBytes   = 256 << 10
	MaxHandshakeBytes   = 32 << 10
	MaxControlBytes     = 32 << 10
	MaxCredentialBytes  = 4096
	MaxReplayChallenges = 256
	DefaultHandshakeTTL = 10 * time.Second
	DefaultControlTTL   = 5 * time.Second
)

var (
	ErrUnavailable                = errors.New("proxy process core is unavailable")
	ErrProtocol                   = errors.New("proxy process protocol rejected")
	ErrUnauthorized               = errors.New("proxy listener control unauthorized")
	ErrSpawnUnsupported           = errors.New("proxy process spawn is unsupported on this platform")
	ErrRestartMaterialUnavailable = errors.New("protected proxy listener restart material is unavailable")
)

var (
	activationIDPattern       = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	executableIdentityPattern = regexp.MustCompile(`^dev:[0-9]+/inode:[1-9][0-9]*$`)
)

type Metadata struct {
	ActivationID    string `json:"activationId"`
	Nonce           string `json:"nonce"`
	Generation      int64  `json:"generation"`
	OwnerUID        int    `json:"ownerUid"`
	Mode            string `json:"mode"`
	SessionIdentity string `json:"sessionIdentity"`
	BinaryPath      string `json:"binaryPath"`
	BinaryDigest    string `json:"binaryDigest"`
}

// Credential is bootstrap-only material. Values must never be formatted,
// logged, placed in argv/environment, or retained by the controller.
type Credential struct {
	Reference string `json:"reference"`
	Value     []byte `json:"value"`
}

type Bootstrap struct {
	Version     string          `json:"version"`
	Kind        string          `json:"kind"`
	Challenge   string          `json:"challenge"`
	Metadata    Metadata        `json:"metadata"`
	Policy      json.RawMessage `json:"policy"`
	Credentials []Credential    `json:"credentials"`
}

func (Bootstrap) String() string   { return "[REDACTED proxy process bootstrap]" }
func (Bootstrap) GoString() string { return "[REDACTED proxy process bootstrap]" }

type WireProof struct {
	PID                    int    `json:"pid"`
	ProcessStartIdentity   string `json:"processStartIdentity"`
	ExecutableIdentity     string `json:"executableIdentity"`
	BinaryDigest           string `json:"binaryDigest"`
	ListenerKeyFingerprint string `json:"listenerKeyFingerprint"`
	ActivationNonce        string `json:"activationNonce"`
	OwnerUID               int    `json:"ownerUid"`
	Generation             int64  `json:"generation"`
	Mode                   string `json:"mode"`
	SessionIdentity        string `json:"sessionIdentity"`
}

type Handshake struct {
	Version        string    `json:"version"`
	Kind           string    `json:"kind"`
	Address        string    `json:"address"`
	ControlAddress string    `json:"controlAddress"`
	ListenerToken  string    `json:"listenerToken"`
	Environment    []string  `json:"environment"`
	PublicKey      string    `json:"publicKey"`
	Proof          WireProof `json:"proof"`
	Challenge      string    `json:"challenge"`
	Signature      string    `json:"signature"`
}

func (Handshake) String() string   { return "[REDACTED proxy process handshake]" }
func (Handshake) GoString() string { return "[REDACTED proxy process handshake]" }

type ControlRequest struct {
	Version       string `json:"version"`
	Kind          string `json:"kind"`
	Action        string `json:"action"`
	Challenge     string `json:"challenge"`
	Authenticator string `json:"authenticator"`
}

type ControlResponse struct {
	Version   string    `json:"version"`
	Kind      string    `json:"kind"`
	Action    string    `json:"action"`
	Challenge string    `json:"challenge"`
	Proof     WireProof `json:"proof"`
	PublicKey string    `json:"publicKey"`
	Signature string    `json:"signature"`
	Accepted  bool      `json:"accepted"`
}

func (ControlResponse) String() string   { return "[REDACTED proxy process response]" }
func (ControlResponse) GoString() string { return "[REDACTED proxy process response]" }

func proofToWire(value state.LiveListenerProof) WireProof {
	return WireProof{PID: value.PID, ProcessStartIdentity: value.ProcessStartIdentity, ExecutableIdentity: value.ExecutableIdentity, BinaryDigest: value.BinaryDigest, ListenerKeyFingerprint: value.ListenerKeyFingerprint, ActivationNonce: value.ActivationNonce, OwnerUID: value.OwnerUID, Generation: value.Generation, Mode: value.Mode, SessionIdentity: value.SessionIdentity}
}

func proofFromWire(value WireProof) state.LiveListenerProof {
	return state.LiveListenerProof{PID: value.PID, ProcessStartIdentity: value.ProcessStartIdentity, ExecutableIdentity: value.ExecutableIdentity, BinaryDigest: value.BinaryDigest, ListenerKeyFingerprint: value.ListenerKeyFingerprint, ActivationNonce: value.ActivationNonce, OwnerUID: value.OwnerUID, Generation: value.Generation, Mode: value.Mode, SessionIdentity: value.SessionIdentity}
}

func fingerprint(public ed25519.PublicKey) string {
	sum := sha256.Sum256(public)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func fingerprintFromEncodedPublicKey(value string) string {
	public, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil || len(public) != ed25519.PublicKeySize || base64.RawURLEncoding.EncodeToString(public) != value {
		return ""
	}
	return fingerprint(ed25519.PublicKey(public))
}

func validChildEnvironment(values []string, address string) bool {
	if len(values) != len(state.EnvironmentNames) || !validListenerAddress(address) {
		return false
	}
	parsed := make(map[string]string, len(values))
	for _, entry := range values {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || !validEnvironmentName(name) || strings.ContainsAny(value, "\x00\r\n") {
			return false
		}
		if _, duplicate := parsed[name]; duplicate {
			return false
		}
		parsed[name] = value
	}
	base := "http://" + address
	token := parsed["OPENAI_API_KEY"]
	return parsed["OPENAI_BASE_URL"] == base+"/v1" && parsed["ANTHROPIC_BASE_URL"] == base &&
		validListenerToken(token) && parsed["ANTHROPIC_API_KEY"] == token && parsed["ANTHROPIC_AUTH_TOKEN"] == token
}

func validEnvironmentName(name string) bool {
	for _, allowed := range state.EnvironmentNames {
		if name == allowed {
			return true
		}
	}
	return false
}

func freshChallenge() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func validChallenge(value string) bool {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(value)
	return err == nil && len(raw) == 32 && base64.RawURLEncoding.EncodeToString(raw) == value
}

type signedPayload struct {
	Version   string    `json:"version"`
	Kind      string    `json:"kind"`
	Action    string    `json:"action"`
	Challenge string    `json:"challenge"`
	Proof     WireProof `json:"proof"`
	Accepted  bool      `json:"accepted"`
}

type controlAuthenticationPayload struct {
	Version   string    `json:"version"`
	Kind      string    `json:"kind"`
	Action    string    `json:"action"`
	Challenge string    `json:"challenge"`
	Proof     WireProof `json:"proof"`
}

func signResponse(private ed25519.PrivateKey, kind, action, challenge string, proof WireProof, accepted bool) (string, error) {
	payload, err := json.Marshal(signedPayload{Version: ProtocolVersion, Kind: kind, Action: action, Challenge: challenge, Proof: proof, Accepted: accepted})
	if err != nil {
		return "", ErrProtocol
	}
	return base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, payload)), nil
}

func verifyResponse(publicText, signature, kind, action, challenge string, proof WireProof, accepted bool) error {
	public, err := base64.RawURLEncoding.Strict().DecodeString(publicText)
	if err != nil || len(public) != ed25519.PublicKeySize || fingerprint(ed25519.PublicKey(public)) != proof.ListenerKeyFingerprint || !validChallenge(challenge) {
		return ErrUnauthorized
	}
	sig, err := base64.RawURLEncoding.Strict().DecodeString(signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return ErrUnauthorized
	}
	payload, err := json.Marshal(signedPayload{Version: ProtocolVersion, Kind: kind, Action: action, Challenge: challenge, Proof: proof, Accepted: accepted})
	if err != nil || !ed25519.Verify(ed25519.PublicKey(public), payload, sig) {
		return ErrUnauthorized
	}
	return nil
}

func authenticateControlRequest(token, action, challenge string, proof WireProof) (string, error) {
	key, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || len(key) < 32 || len(key) > 128 {
		zeroBytes(key)
		return "", ErrUnauthorized
	}
	defer zeroBytes(key)
	payload, err := json.Marshal(controlAuthenticationPayload{Version: ProtocolVersion, Kind: "control_request", Action: action, Challenge: challenge, Proof: proof})
	if err != nil {
		return "", ErrProtocol
	}
	defer zeroBytes(payload)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(payload)
	sum := mac.Sum(nil)
	defer zeroBytes(sum)
	return base64.RawURLEncoding.EncodeToString(sum), nil
}

func verifyControlRequest(token string, request ControlRequest, proof WireProof) error {
	if request.Version != ProtocolVersion || request.Kind != "control_request" || (request.Action != "inspect" && request.Action != "stop") || !validChallenge(request.Challenge) {
		return ErrUnauthorized
	}
	expected, err := authenticateControlRequest(token, request.Action, request.Challenge, proof)
	if err != nil {
		return ErrUnauthorized
	}
	expectedRaw, expectedErr := base64.RawURLEncoding.Strict().DecodeString(expected)
	actualRaw, actualErr := base64.RawURLEncoding.Strict().DecodeString(request.Authenticator)
	defer zeroBytes(expectedRaw)
	defer zeroBytes(actualRaw)
	if expectedErr != nil || actualErr != nil || len(actualRaw) != sha256.Size || !hmac.Equal(expectedRaw, actualRaw) {
		return ErrUnauthorized
	}
	return nil
}

func writeFrame(writer io.Writer, maximum int, value any) error {
	return writeFrameObserved(writer, maximum, value, nil)
}

func writeFrameContext(ctx context.Context, writer io.WriteCloser, maximum int, value any) error {
	done := make(chan error, 1)
	go func() { done <- writeFrame(writer, maximum, value) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = writer.Close()
		<-done
		return errors.Join(ErrProtocol, ctx.Err())
	}
}

func writeFrameObserved(writer io.Writer, maximum int, value any, observe func([]byte)) error {
	payload, err := json.Marshal(value)
	if observe != nil {
		observe(payload)
	}
	defer zeroBytes(payload)
	if err != nil || len(payload) == 0 || len(payload) > maximum {
		return ErrProtocol
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(payload)))
	if err := writeAll(writer, size[:]); err != nil {
		return errors.Join(ErrProtocol, err)
	}
	if err := writeAll(writer, payload); err != nil {
		return errors.Join(ErrProtocol, err)
	}
	return nil
}

func readFrame(reader io.Reader, maximum int, target any) error {
	return readFrameObserved(reader, maximum, target, nil)
}

func readFrameObserved(reader io.Reader, maximum int, target any, observe func([]byte)) error {
	var size [4]byte
	if _, err := io.ReadFull(reader, size[:]); err != nil {
		return errors.Join(ErrProtocol, err)
	}
	length := int(binary.BigEndian.Uint32(size[:]))
	if length < 2 || length > maximum {
		return ErrProtocol
	}
	payload := make([]byte, length)
	if observe != nil {
		observe(payload)
	}
	defer zeroBytes(payload)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return errors.Join(ErrProtocol, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return ErrProtocol
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ErrProtocol
	}
	return nil
}

func writeAll(writer io.Writer, value []byte) error {
	for len(value) > 0 {
		count, err := writer.Write(value)
		if err != nil {
			return err
		}
		if count <= 0 || count > len(value) {
			return io.ErrShortWrite
		}
		value = value[count:]
	}
	return nil
}

func validateMetadata(value Metadata) error {
	if !activationIDPattern.MatchString(value.ActivationID) || !validChallenge(value.Nonce) || value.Generation < 1 || value.OwnerUID < 0 || (value.Mode != "session" && value.Mode != "scoped_run") || value.SessionIdentity == "" || len(value.SessionIdentity) > 256 || !filepath.IsAbs(value.BinaryPath) || filepath.Clean(value.BinaryPath) != value.BinaryPath || !validDigest(value.BinaryDigest) {
		return ErrProtocol
	}
	return nil
}

func validDigest(value string) bool {
	if len(value) != 71 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	_, err := hex.DecodeString(value[7:])
	return err == nil
}

func validateBootstrap(value Bootstrap) error {
	if value.Version != ProtocolVersion || value.Kind != "bootstrap" || !validChallenge(value.Challenge) || validateMetadata(value.Metadata) != nil || len(value.Policy) < 2 || !json.Valid(value.Policy) || len(value.Credentials) == 0 || len(value.Credentials) > 256 {
		return ErrProtocol
	}
	seen := map[string]bool{}
	for _, item := range value.Credentials {
		if item.Reference == "" || len(item.Reference) > 256 || len(item.Value) == 0 || len(item.Value) > MaxCredentialBytes || seen[item.Reference] {
			return ErrProtocol
		}
		seen[item.Reference] = true
	}
	return nil
}

func validListenerAddress(value string) bool {
	host, portText, err := net.SplitHostPort(value)
	if err != nil {
		return false
	}
	address, err := netip.ParseAddr(host)
	if err != nil || !address.Unmap().IsLoopback() {
		return false
	}
	port, err := strconv.Atoi(portText)
	return err == nil && port >= 1 && port <= 65535
}

func validListenerToken(value string) bool {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(value)
	defer zeroBytes(raw)
	return err == nil && len(raw) >= 32 && len(raw) <= 128 && base64.RawURLEncoding.EncodeToString(raw) == value
}

func validOpaqueIdentity(value string) bool {
	return value != "" && len(value) <= 256 && !strings.ContainsAny(value, "\x00\r\n")
}

func validLiveProof(value state.LiveListenerProof) bool {
	return value.PID > 0 &&
		value.OwnerUID >= 0 &&
		validOpaqueIdentity(value.ProcessStartIdentity) &&
		executableIdentityPattern.MatchString(value.ExecutableIdentity) &&
		validDigest(value.BinaryDigest) &&
		validDigest(value.ListenerKeyFingerprint) &&
		validChallenge(value.ActivationNonce) &&
		value.Generation > 0 &&
		(value.Mode == "session" || value.Mode == "scoped_run") &&
		validOpaqueIdentity(value.SessionIdentity)
}

func zeroBootstrap(value *Bootstrap) {
	if value == nil {
		return
	}
	for index := range value.Credentials {
		for byteIndex := range value.Credentials[index].Value {
			value.Credentials[index].Value[byteIndex] = 0
		}
	}
}

func zeroBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

// readFrameContext makes cancellation close an owned pipe/socket. This both
// bounds stalled peers and prevents a goroutine from surviving cancellation.
func readFrameContext(ctx context.Context, reader io.ReadCloser, maximum int, target any) error {
	done := make(chan error, 1)
	go func() { done <- readFrame(reader, maximum, target) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = reader.Close()
		<-done
		return errors.Join(ErrProtocol, ctx.Err())
	}
}

func safeError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return errors.Join(ErrUnavailable, err)
	}
	if errors.Is(err, ErrUnauthorized) {
		return ErrUnauthorized
	}
	if errors.Is(err, ErrSpawnUnsupported) {
		return ErrSpawnUnsupported
	}
	return fmt.Errorf("%w: listener protocol failure", ErrUnavailable)
}
