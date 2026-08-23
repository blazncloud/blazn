package activation

import (
	"context"
	"errors"

	"github.com/blazncloud/blazn/internal/proxy/credential"
	"github.com/blazncloud/blazn/internal/proxy/listener"
	"github.com/blazncloud/blazn/internal/proxy/router"
	"github.com/blazncloud/blazn/internal/proxy/state"
	"github.com/blazncloud/blazn/internal/proxycontract"
)

type ListenerProofProvider interface {
	Proof(context.Context, string, string, ListenerMetadata) (state.ListenerIdentity, state.LiveListenerProof, error)
}

var (
	ErrPolicyInvalid         = router.ErrPolicyInvalid
	ErrCredentialUnavailable = router.ErrCredentialUnavailable
	ErrListenerUnavailable   = listener.ErrUnavailable
)

// EmbeddedListenerFactory is the core/test bridge to the merged authenticated
// loopback runtime. It is not a production session-listener factory: credential
// resolution and exact process identity remain injected, and the runtime dies
// with its caller rather than supplying durable child/control proof.
type EmbeddedListenerFactory struct {
	Address            string
	Router             router.Config
	Identity           ListenerProofProvider
	CredentialResolver credential.SnapshotResolver
}

func (f EmbeddedListenerFactory) Start(ctx context.Context, policy proxycontract.Policy, digest string, metadata ListenerMetadata) (ManagedListener, error) {
	if f.Identity == nil {
		return nil, ErrUnavailable
	}
	config := f.Router
	if f.CredentialResolver != nil {
		snapshot, err := f.CredentialResolver.Resolve(ctx, policy)
		if err != nil {
			return nil, errors.Join(ErrCredentialUnavailable, err)
		}
		config.Credentials = snapshot
	}
	config.Policy, config.PolicyDigest, config.ActivationID = policy, digest, metadata.ActivationID
	runtime, err := listener.Start(listener.Config{Address: f.Address, Router: config})
	if err != nil {
		return nil, err
	}
	identity, proof, err := f.Identity.Proof(ctx, runtime.Address(), runtime.ListenerKeyFingerprint(), metadata)
	if err != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		_ = runtime.Shutdown(cleanup)
		return nil, errors.Join(ErrListenerUnavailable, err)
	}
	if identity.PID < 1 || identity.ProcessStartIdentity == "" || identity.ExecutableIdentity == "" || identity.Address != runtime.Address() || identity.ListenerKeyFingerprint != runtime.ListenerKeyFingerprint() || proof != listenerProof(identity, metadata) {
		cleanup, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		_ = runtime.Shutdown(cleanup)
		return nil, errors.Join(ErrListenerUnavailable, errors.New("listener identity does not match the authenticated runtime"))
	}
	return &embeddedListener{Runtime: runtime, identity: identity, proof: proof}, nil
}

type embeddedListener struct {
	Runtime  *listener.Runtime
	identity state.ListenerIdentity
	proof    state.LiveListenerProof
}

func (e *embeddedListener) Identity() state.ListenerIdentity { return e.identity }
func (e *embeddedListener) Inspect(context.Context) (state.LiveListenerProof, bool, error) {
	select {
	case <-e.Runtime.Done():
		return state.LiveListenerProof{}, false, nil
	default:
		return e.proof, true, nil
	}
}
func (e *embeddedListener) ChildEnvironment(base []string) ([]string, error) {
	return e.Runtime.ChildEnvironment(base)
}
func (e *embeddedListener) Shutdown(ctx context.Context) error { return e.Runtime.Shutdown(ctx) }
