// Package credential resolves policy destination references into an
// activation-local, immutable snapshot. Platform credential stores implement
// Backend outside this package; this core never persists or publishes secrets.
package credential

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/blazncloud/blazn/internal/proxycontract"
)

const MaxCredentialBytes = 4096
const maxReferenceBytes = 256

var (
	ErrUnavailable = errors.New("proxy destination credential is unavailable")
	canonicalRefRE = regexp.MustCompile(`^(node-route|workspace-vault)://[A-Za-z0-9](?:[A-Za-z0-9._~-]*[A-Za-z0-9])?(?:/[A-Za-z0-9](?:[A-Za-z0-9._~-]*[A-Za-z0-9])?)*$`)
)

type FailureKind string

const (
	FailureInvalidReference   FailureKind = "invalid_reference"
	FailureBackendUnavailable FailureKind = "backend_unavailable"
	FailureInvalidCredential  FailureKind = "invalid_credential"
	FailureCancelled          FailureKind = "cancelled"
)

// UnavailableError classifies resolution failures without retaining or
// formatting a credential reference, backend error, or secret value.
type UnavailableError struct {
	Kind  FailureKind
	cause error
}

func (e *UnavailableError) Error() string    { return ErrUnavailable.Error() }
func (e *UnavailableError) String() string   { return ErrUnavailable.Error() }
func (e *UnavailableError) GoString() string { return "[REDACTED credential unavailable]" }
func (e *UnavailableError) Unwrap() []error {
	if e.cause == nil {
		return []error{ErrUnavailable}
	}
	return []error{ErrUnavailable, e.cause}
}

// credentialFailureKind seals the set of backend errors whose type and cause
// are safe for the resolver to retain. Arbitrary backend errors are still
// collapsed to FailureBackendUnavailable without a cause.
func (e *UnavailableError) credentialFailureKind() FailureKind { return e.Kind }

type safeBackendFailure interface {
	error
	credentialFailureKind() FailureKind
}

// Backend is the platform-neutral boundary implemented by a node-route or
// workspace-vault secret store. The returned buffer transfers to the caller.
type Backend interface {
	Lookup(context.Context, string) ([]byte, error)
}

type Provider interface {
	DestinationCredential(context.Context, string) (string, error)
}

type SnapshotResolver interface {
	Resolve(context.Context, proxycontract.Policy) (*Snapshot, error)
}

// Resolver dispatches canonical references to the backend selected for their
// scheme. One Resolve call defines one listener lifetime.
type Resolver struct {
	NodeRoute      Backend
	WorkspaceVault Backend
}

// Snapshot is an immutable, concurrency-safe router credential provider.
// Its fields are intentionally unexported so results, events, and state cannot
// serialize destination references or secret values.
type Snapshot struct {
	values map[string][]byte
}

func (*Snapshot) String() string   { return "[REDACTED credential snapshot]" }
func (*Snapshot) GoString() string { return "[REDACTED credential snapshot]" }

type request struct {
	ref     string
	backend Backend
}

type resolution struct {
	ref   string
	value []byte
	err   error
}

// Resolve validates and de-duplicates every route credential in policy before
// issuing exactly one lookup per unique canonical reference. Unique lookups
// run concurrently and all transient backend buffers are best-effort zeroed.
func (r Resolver) Resolve(ctx context.Context, policy proxycontract.Policy) (*Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, unavailable(FailureCancelled, err)
	}
	requests := make([]request, 0, len(policy.Routes))
	seen := make(map[string]struct{}, len(policy.Routes))
	for _, route := range policy.Routes {
		backend, err := r.backendFor(route.DestinationClass, route.CredentialRef)
		if err != nil {
			return nil, err
		}
		if _, duplicate := seen[route.CredentialRef]; duplicate {
			continue
		}
		seen[route.CredentialRef] = struct{}{}
		requests = append(requests, request{ref: route.CredentialRef, backend: backend})
	}
	if len(requests) == 0 {
		return nil, unavailable(FailureInvalidReference, nil)
	}

	lookupCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan resolution, len(requests))
	for _, item := range requests {
		item := item
		go func() {
			raw, err := item.backend.Lookup(lookupCtx, item.ref)
			if err != nil {
				zero(raw)
				kind, cause := classifyBackendFailure(lookupCtx, err)
				results <- resolution{err: unavailable(kind, cause)}
				return
			}
			if err := lookupCtx.Err(); err != nil {
				zero(raw)
				results <- resolution{err: unavailable(FailureCancelled, err)}
				return
			}
			if err := validateValue(raw); err != nil {
				zero(raw)
				results <- resolution{err: err}
				return
			}
			value := append([]byte(nil), raw...)
			zero(raw)
			results <- resolution{ref: item.ref, value: value}
		}()
	}

	values := make(map[string][]byte, len(requests))
	var firstErr error
	for range requests {
		result := <-results
		if result.err != nil {
			if firstErr == nil {
				firstErr = result.err
				cancel()
			}
			continue
		}
		if firstErr != nil {
			zero(result.value)
			continue
		}
		values[result.ref] = result.value
	}
	if firstErr != nil {
		for _, value := range values {
			zero(value)
		}
		return nil, firstErr
	}
	return &Snapshot{values: values}, nil
}

func classifyBackendFailure(ctx context.Context, err error) (FailureKind, error) {
	if contextErr := ctx.Err(); contextErr != nil {
		return FailureCancelled, contextErr
	}
	var safeFailure safeBackendFailure
	if !errors.As(err, &safeFailure) {
		return FailureBackendUnavailable, nil
	}
	switch kind := safeFailure.credentialFailureKind(); kind {
	case FailureInvalidReference, FailureBackendUnavailable, FailureInvalidCredential, FailureCancelled:
		return kind, safeFailure
	default:
		return FailureBackendUnavailable, nil
	}
}

func (r Resolver) backendFor(class proxycontract.DestinationClass, ref string) (Backend, error) {
	if !isCanonicalReference(ref) {
		return nil, unavailable(FailureInvalidReference, nil)
	}
	scheme, _, _ := strings.Cut(ref, "://")
	var backend Backend
	switch class {
	case proxycontract.DestinationLocalNode:
		if scheme != "node-route" {
			return nil, unavailable(FailureInvalidReference, nil)
		}
		backend = r.NodeRoute
	case proxycontract.DestinationCompany, proxycontract.DestinationProvider, proxycontract.DestinationBlaznCloud:
		if scheme != "workspace-vault" {
			return nil, unavailable(FailureInvalidReference, nil)
		}
		backend = r.WorkspaceVault
	default:
		return nil, unavailable(FailureInvalidReference, nil)
	}
	if backend == nil {
		return nil, unavailable(FailureBackendUnavailable, nil)
	}
	return backend, nil
}

func (s *Snapshot) DestinationCredential(ctx context.Context, ref string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", unavailable(FailureCancelled, err)
	}
	if s == nil || !isCanonicalReference(ref) {
		return "", unavailable(FailureInvalidReference, nil)
	}
	value, ok := s.values[ref]
	if !ok {
		return "", unavailable(FailureBackendUnavailable, nil)
	}
	// Conversion produces a distinct immutable value for the request path.
	return string(append([]byte(nil), value...)), nil
}

func isCanonicalReference(ref string) bool {
	return len(ref) > 0 && len(ref) <= maxReferenceBytes && canonicalRefRE.MatchString(ref)
}

func validateValue(value []byte) error {
	if len(value) == 0 || len(value) > MaxCredentialBytes {
		return unavailable(FailureInvalidCredential, nil)
	}
	for _, current := range value {
		if current == 0 || current == '\r' || current == '\n' {
			return unavailable(FailureInvalidCredential, nil)
		}
	}
	return nil
}

func unavailable(kind FailureKind, cause error) error {
	return &UnavailableError{Kind: kind, cause: cause}
}

func zero(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
