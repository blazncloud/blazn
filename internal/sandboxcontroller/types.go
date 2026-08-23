package sandboxcontroller

import (
	"context"
	"errors"
	"time"

	"github.com/blazncloud/blazn/internal/sandboxcontrol"
)

type Source struct {
	Name, URL, Destination, Commit string
	Writable                       bool
}

type Artifact struct {
	Name, Path, MediaType string
	Required              bool
}

type Resources struct {
	CPURequest, MemoryRequest, EphemeralRequest string
	CPULimit, MemoryLimit, EphemeralLimit       string
}

type WorkItem struct {
	OperationID, WorkspaceID, SandboxID, RequestedBy                              string
	OperationType                                                                 string
	ExpectedSandboxVersion                                                        int64
	LeaseToken                                                                    string
	LeaseExpiresAt                                                                time.Time
	LeaseRemaining                                                                time.Duration
	LeaseDeadline                                                                 time.Time
	Attempt                                                                       int
	AllocationMode, DesiredState, Architecture                                    string
	TemplateVersionID, TemplateDigest, VariantName, ImageIndexDigest, ImageDigest string
	PlacementProfile                                                              string
	Command                                                                       []string
	Resources                                                                     Resources
	QueueName                                                                     string
	AdmissionID, BackendUID, BackendResourceVersion                               *string
	ExpiresAt                                                                     time.Time
	Sources                                                                       []Source
	Artifacts                                                                     []Artifact
	Admission                                                                     *sandboxcontrol.WorkloadIdentity
}

type LeaseWindow struct {
	DatabaseNow time.Time
	ExpiresAt   time.Time
	Remaining   time.Duration
	Deadline    time.Time
}

type Completion struct {
	Status                                                                      string
	ExpectedBackendUID, ExpectedBackendResourceVersion, ExpectedAdmissionDigest *string
	CleanupComplete, ArtifactExportComplete, GrantsRevoked, BackendDestroyed    bool
	ArtifactIDs, WarningCodes                                                   []string
	Error                                                                       *SafeError
}

type SafeError struct{ Code, Message, RequestID string }
type RetryOutcome string

const (
	RetryScheduled   RetryOutcome = "retry_scheduled"
	RecoveryRequired RetryOutcome = "recovery_required"
	Fenced           RetryOutcome = "fenced"
)

type Store interface {
	Claim(context.Context, string, int) (*WorkItem, error)
	Renew(context.Context, string, string, string, int) (LeaseWindow, bool, error)
	BindBackend(context.Context, string, string, string, sandboxcontrol.SandboxRecord, sandboxcontrol.WorkloadIdentity) (bool, error)
	Retry(context.Context, string, string, string, int, SafeError) (RetryOutcome, error)
	Complete(context.Context, string, string, string, Completion) (bool, error)
	EnqueueExpired(context.Context, int) (int, error)
	Health(context.Context) error
	Close() error
}

type BackendState struct {
	Record                                           sandboxcontrol.SandboxRecord
	Admission                                        *sandboxcontrol.WorkloadIdentity
	AdmissionObservation                             *sandboxcontrol.AdmissionObservation
	Exists, Ready, Deleting, CleanupFinalizerPresent bool
}
type CleanupResult struct {
	ArtifactIDs, WarningCodes                              []string
	CleanupComplete, ArtifactExportComplete, GrantsRevoked bool
	BackendDestroyed                                       bool
}
type Backend interface {
	Health(context.Context) error
	EnsureCreated(context.Context, WorkItem) (BackendState, error)
	Observe(context.Context, WorkItem) (BackendState, error)
	BeginDelete(context.Context, WorkItem) (BackendState, error)
	Finalize(context.Context, WorkItem, BackendState) (CleanupResult, error)
}

type Failure struct {
	Code, SafeMessage    string
	Retryable, Ambiguous bool
	Cause                error
}

func (e *Failure) Error() string { return e.Code + ": " + e.SafeMessage }
func (e *Failure) Unwrap() error { return e.Cause }
func BackendFailure(err error) (*Failure, bool) {
	var value *Failure
	return value, errors.As(err, &value)
}

type Config struct {
	WorkerID         string
	Lease            time.Duration
	RenewEvery       time.Duration
	PollEvery        time.Duration
	OperationTimeout time.Duration
	IdleDelay        time.Duration
	RetryDelay       time.Duration
	ExpiryEvery      time.Duration
	ExpiryBatch      int
}
