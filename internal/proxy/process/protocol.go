// Package process defines the bounded parent/child protocol for a durable
// proxy listener. Platform process discovery and service publication are
// deliberately outside this package.
package process

import (
	"context"
	"crypto/ed25519"
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
	DefaultHandshakeTTL = 10 * time.Second
	DefaultControlTTL   = 5 * time.Second
)

var (
	ErrUnavailable  = errors.New("proxy process core is unavailable")
	ErrProtocol     = errors.New("proxy process protocol rejected")
	ErrUnauthorized = errors.New("proxy listener control unauthorized")
)

var activationIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

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
	PublicKey      string    `json:"publicKey"`
	Proof          WireProof `json:"proof"`
	Challenge      string    `json:"challenge"`
	Signature      string    `json:"signature"`
}

func (Handshake) String() string   { return "[REDACTED proxy process handshake]" }
func (Handshake) GoString() string { return "[REDACTED proxy process handshake]" }

type ControlRequest struct {
	Version   string `json:"version"`
	Kind      string `json:"kind"`
	Action    string `json:"action"`
	Challenge string `json:"challenge"`
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

func writeFrame(writer io.Writer, maximum int, value any) error {
	payload, err := json.Marshal(value)
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
	var size [4]byte
	if _, err := io.ReadFull(reader, size[:]); err != nil {
		return errors.Join(ErrProtocol, err)
	}
	length := int(binary.BigEndian.Uint32(size[:]))
	if length < 2 || length > maximum {
		return ErrProtocol
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return errors.Join(ErrProtocol, err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
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
	if value.Version != ProtocolVersion || value.Kind != "bootstrap" || validateMetadata(value.Metadata) != nil || len(value.Policy) < 2 || !json.Valid(value.Policy) || len(value.Credentials) == 0 || len(value.Credentials) > 256 {
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
	return err == nil && len(raw) >= 32 && len(raw) <= 128 && base64.RawURLEncoding.EncodeToString(raw) == value
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
	return fmt.Errorf("%w: listener protocol failure", ErrUnavailable)
}
