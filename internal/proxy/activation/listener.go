package activation

import (
	"context"
	"errors"

	"github.com/blazncloud/blazn/internal/proxy/listener"
	"github.com/blazncloud/blazn/internal/proxy/router"
	"github.com/blazncloud/blazn/internal/proxy/state"
	"github.com/blazncloud/blazn/internal/proxycontract"
)

type ListenerProofProvider interface {
	Proof(context.Context, string, string, ListenerMetadata) (state.ListenerIdentity, state.LiveListenerProof, error)
}

// EmbeddedListenerFactory is the platform-neutral bridge to the merged
// authenticated loopback runtime. Credential resolution and exact process
// identity remain injected so this slice cannot invent platform authority.
type EmbeddedListenerFactory struct {
	Address  string
	Router   router.Config
	Identity ListenerProofProvider
}

func (f EmbeddedListenerFactory) Start(ctx context.Context, policy proxycontract.Policy, digest string, metadata ListenerMetadata) (ManagedListener, error) {
	if f.Identity == nil {
		return nil, ErrUnavailable
	}
	config := f.Router
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
		return nil, err
	}
	if identity.PID < 1 || identity.ProcessStartIdentity == "" || identity.ExecutableIdentity == "" || identity.Address != runtime.Address() || identity.ListenerKeyFingerprint != runtime.ListenerKeyFingerprint() || proof != listenerProof(identity, metadata) {
		cleanup, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		_ = runtime.Shutdown(cleanup)
		return nil, errors.New("listener identity does not match the authenticated runtime")
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
