package activation

import (
	"context"

	"github.com/blazncloud/blazn/internal/proxy/listener"
	"github.com/blazncloud/blazn/internal/proxy/router"
	"github.com/blazncloud/blazn/internal/proxy/state"
	"github.com/blazncloud/blazn/internal/proxycontract"
)

type ListenerIdentityProvider interface {
	Identity(context.Context, string, string, ListenerMetadata) (state.ListenerIdentity, error)
}

// EmbeddedListenerFactory is the platform-neutral bridge to the merged
// authenticated loopback runtime. Credential resolution and exact process
// identity remain injected so this slice cannot invent platform authority.
type EmbeddedListenerFactory struct {
	Address  string
	Router   router.Config
	Identity ListenerIdentityProvider
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
	identity, err := f.Identity.Identity(ctx, runtime.Address(), runtime.ListenerKeyFingerprint(), metadata)
	if err != nil {
		_ = runtime.Shutdown(context.Background())
		return nil, err
	}
	return &embeddedListener{Runtime: runtime, identity: identity}, nil
}

type embeddedListener struct {
	Runtime  *listener.Runtime
	identity state.ListenerIdentity
}

func (e *embeddedListener) Identity() state.ListenerIdentity { return e.identity }
func (e *embeddedListener) ChildEnvironment(base []string) ([]string, error) {
	return e.Runtime.ChildEnvironment(base)
}
func (e *embeddedListener) Shutdown(ctx context.Context) error { return e.Runtime.Shutdown(ctx) }
