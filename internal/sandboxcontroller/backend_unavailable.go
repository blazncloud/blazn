package sandboxcontroller

import (
	"context"

	"github.com/blazncloud/blazn/internal/sandboxcontrol"
)

type unavailableBackend struct{}

// NewUnavailableBackend keeps the executable fail-closed until the separately
// reviewed Kubernetes backend is installed. It never performs cluster I/O.
func NewUnavailableBackend() Backend { return unavailableBackend{} }

func (unavailableBackend) Health(context.Context) error { return backendUnavailable() }

func (unavailableBackend) EnsureCreated(context.Context, WorkItem) (BackendState, error) {
	return BackendState{}, backendUnavailable()
}

func (unavailableBackend) Observe(context.Context, WorkItem, *sandboxcontrol.AdmissionObservation) (BackendState, error) {
	return BackendState{}, backendUnavailable()
}

func (unavailableBackend) BeginDelete(context.Context, WorkItem, *sandboxcontrol.AdmissionObservation) (BackendState, error) {
	return BackendState{}, backendUnavailable()
}

func (unavailableBackend) Finalize(context.Context, WorkItem, BackendState, *sandboxcontrol.AdmissionObservation) (CleanupResult, error) {
	return CleanupResult{}, backendUnavailable()
}

func backendUnavailable() error {
	return &Failure{
		Code:        "backend_unavailable",
		SafeMessage: "sandbox backend is not installed",
		Retryable:   true,
	}
}
