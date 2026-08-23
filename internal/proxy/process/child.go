package process

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"

	"github.com/blazncloud/blazn/internal/proxy/state"
)

const ChildCommand = "__proxy-listener-core"

func IsChildInvocation(args []string) bool {
	return len(args) == 2 && args[0] == ChildCommand && args[1] == ProtocolVersion
}

type Runtime interface {
	Address() string
	ControlAddress() string
	ListenerToken() string
	Identity() (pid int, processStartIdentity, executableIdentity string)
	ServeControl(context.Context, func(context.Context, ControlRequest) (ControlResponse, error)) error
	Shutdown(context.Context) error
}

type RuntimeFactory interface {
	Start(context.Context, Bootstrap) (Runtime, error)
}

type RuntimeFactoryFunc func(context.Context, Bootstrap) (Runtime, error)

func (f RuntimeFactoryFunc) Start(ctx context.Context, bootstrap Bootstrap) (Runtime, error) {
	return f(ctx, bootstrap)
}

// RunChild owns the bootstrap buffers and zeroes credentials when Start
// returns. A RuntimeFactory must copy only the values it needs in memory.
func RunChild(ctx context.Context, bootstrapReader io.ReadCloser, handshakeWriter io.WriteCloser, factory RuntimeFactory) error {
	if bootstrapReader == nil || handshakeWriter == nil || factory == nil {
		return ErrUnavailable
	}
	defer bootstrapReader.Close()
	defer handshakeWriter.Close()
	var bootstrap Bootstrap
	if err := readFrameContext(ctx, bootstrapReader, MaxBootstrapBytes, &bootstrap); err != nil {
		return safeError(err)
	}
	if err := validateBootstrap(bootstrap); err != nil {
		zeroBootstrap(&bootstrap)
		return err
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		zeroBootstrap(&bootstrap)
		return ErrUnavailable
	}
	runtime, err := factory.Start(ctx, bootstrap)
	zeroBootstrap(&bootstrap)
	if err != nil {
		return safeError(err)
	}
	defer runtime.Shutdown(context.Background())
	pid, startIdentity, executableIdentity := runtime.Identity()
	proof := state.LiveListenerProof{PID: pid, ProcessStartIdentity: startIdentity, ExecutableIdentity: executableIdentity, BinaryDigest: bootstrap.Metadata.BinaryDigest, ListenerKeyFingerprint: fingerprint(public), ActivationNonce: bootstrap.Metadata.Nonce, OwnerUID: bootstrap.Metadata.OwnerUID, Generation: bootstrap.Metadata.Generation, Mode: bootstrap.Metadata.Mode, SessionIdentity: bootstrap.Metadata.SessionIdentity}
	wire := proofToWire(proof)
	challenge, err := freshChallenge()
	if err != nil {
		return ErrUnavailable
	}
	signature, err := signResponse(private, "handshake", "start", challenge, wire, true)
	if err != nil {
		return err
	}
	handshake := Handshake{Version: ProtocolVersion, Kind: "handshake", Address: runtime.Address(), ControlAddress: runtime.ControlAddress(), ListenerToken: runtime.ListenerToken(), PublicKey: base64.RawURLEncoding.EncodeToString(public), Proof: wire, Challenge: challenge, Signature: signature}
	if err := writeFrame(handshakeWriter, MaxHandshakeBytes, handshake); err != nil {
		return safeError(err)
	}
	_ = handshakeWriter.Close()
	handler := func(callCtx context.Context, request ControlRequest) (ControlResponse, error) {
		if request.Version != ProtocolVersion || request.Kind != "control_request" || (request.Action != "inspect" && request.Action != "stop") || !validChallenge(request.Challenge) {
			return ControlResponse{}, ErrUnauthorized
		}
		signature, err := signResponse(private, "control_response", request.Action, request.Challenge, wire, true)
		if err != nil {
			return ControlResponse{}, ErrUnavailable
		}
		response := ControlResponse{Version: ProtocolVersion, Kind: "control_response", Action: request.Action, Challenge: request.Challenge, Proof: wire, PublicKey: base64.RawURLEncoding.EncodeToString(public), Signature: signature, Accepted: true}
		if request.Action == "stop" {
			go runtime.Shutdown(context.Background())
		}
		return response, nil
	}
	if err := runtime.ServeControl(ctx, handler); err != nil && !errors.Is(err, context.Canceled) {
		return safeError(err)
	}
	return nil
}

type unavailableFactory struct{}

func (unavailableFactory) Start(context.Context, Bootstrap) (Runtime, error) {
	return nil, ErrUnavailable
}

// DefaultChildMain is intentionally unavailable until a reviewed platform
// listener/control-socket adapter is wired. It accepts only inherited anonymous
// pipe descriptors; no bootstrap fields are accepted through argv/environment.
func DefaultChildMain(ctx context.Context, bootstrapReader io.ReadCloser, handshakeWriter io.WriteCloser) error {
	return RunChild(ctx, bootstrapReader, handshakeWriter, unavailableFactory{})
}
