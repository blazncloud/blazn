package sandboxcontroller

import (
	"context"
	"errors"
	"reflect"
	"time"

	"github.com/blazncloud/blazn/internal/sandboxcontrol"
)

// SandboxControlAdapter is the deliberately narrow Kubernetes mutation seam.
// It excludes generic Pod operations so the controller cannot fall back to an
// unmanaged workload when the Sandbox CRD or admission chain is unavailable.
type SandboxControlAdapter interface {
	EnsureCreated(context.Context, sandboxcontrol.CreateRequest, string) (sandboxcontrol.SandboxRecord, sandboxcontrol.OperationReceipt, error)
	Get(context.Context, string, string, string) (sandboxcontrol.SandboxRecord, error)
	ObserveAdmission(context.Context, sandboxcontrol.CreateRequest, sandboxcontrol.SandboxRecord, *sandboxcontrol.AdmissionObservation) (sandboxcontrol.AdmissionObservation, error)
	Delete(context.Context, string, string, string, string, string, string, string) (sandboxcontrol.OperationReceipt, error)
	Finalize(context.Context, string, string, string, string, string, string, []sandboxcontrol.ArtifactExport, string) (sandboxcontrol.OperationReceipt, error)
	ObserveAbsence(context.Context, sandboxcontrol.AdmissionObservation) error
}

type KubernetesBackendConfig struct {
	Adapter                 SandboxControlAdapter
	Health                  func(context.Context) error
	ArtifactExportSupported bool
	AbsencePollInterval     time.Duration
}

type KubernetesBackend struct {
	adapter                 SandboxControlAdapter
	health                  func(context.Context) error
	artifactExportSupported bool
	absencePollInterval     time.Duration
}

func NewKubernetesBackend(config KubernetesBackendConfig) (*KubernetesBackend, error) {
	if config.Adapter == nil || config.Health == nil {
		return nil, errors.New("Kubernetes backend dependencies are required")
	}
	if config.AbsencePollInterval == 0 {
		config.AbsencePollInterval = 100 * time.Millisecond
	}
	if config.AbsencePollInterval < time.Millisecond || config.AbsencePollInterval > time.Minute {
		return nil, errors.New("Kubernetes backend absence poll interval is invalid")
	}
	return &KubernetesBackend{adapter: config.Adapter, health: config.Health,
		artifactExportSupported: config.ArtifactExportSupported,
		absencePollInterval:     config.AbsencePollInterval}, nil
}

func (b *KubernetesBackend) Health(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := b.health(ctx); err != nil {
		return backendFailure("backend_unavailable", "sandbox backend health check failed", true, false, err)
	}
	return nil
}

func (b *KubernetesBackend) EnsureCreated(ctx context.Context, item WorkItem) (BackendState, error) {
	request, err := b.request(item)
	if err != nil {
		return BackendState{}, err
	}
	if err := ctx.Err(); err != nil {
		return BackendState{}, err
	}
	record, receipt, err := b.adapter.EnsureCreated(ctx, request, "")
	if err != nil {
		return BackendState{}, classifyAdapter("create", err)
	}
	if err := verifyReceipt(receipt, sandboxcontrol.OperationCreate, request, record); err != nil {
		return BackendState{}, backendFailure("backend_identity_mismatch", "sandbox create receipt identity changed", false, true, err)
	}
	return stateFromRecord(record), nil
}

func (b *KubernetesBackend) Observe(ctx context.Context, item WorkItem) (BackendState, error) {
	request, err := b.request(item)
	if err != nil {
		return BackendState{}, err
	}
	record := expectedRecord(item)
	observation, err := b.adapter.ObserveAdmission(ctx, request, record, nil)
	if err != nil {
		return BackendState{}, classifyAdapter("observe", err)
	}
	if err := verifyObservation(item, observation); err != nil {
		return BackendState{}, backendFailure("backend_identity_mismatch", "backend admission identity changed", false, true, err)
	}
	record.UID = observation.Sandbox.UID
	record.ResourceVersion = observation.Sandbox.ResourceVersion
	record.State = sandboxcontrol.StateReady
	identity := observation.Workload
	return BackendState{Record: record, Admission: &identity, AdmissionObservation: &observation,
		Exists: true, Ready: true}, nil
}

func (b *KubernetesBackend) BeginDelete(ctx context.Context, item WorkItem) (BackendState, error) {
	request, err := b.request(item)
	if err != nil {
		return BackendState{}, err
	}
	if item.BackendUID == nil || item.BackendResourceVersion == nil || item.Admission == nil {
		return BackendState{}, backendFailure("cleanup_identity_missing", "cleanup lacks exact backend evidence", true, true, nil)
	}
	record := expectedRecord(item)
	observation, err := b.adapter.ObserveAdmission(ctx, request, record, nil)
	if err != nil {
		return BackendState{}, classifyCleanupObservation(err)
	}
	if err := verifyObservation(item, observation); err != nil {
		return BackendState{}, backendFailure("backend_identity_mismatch", "cleanup backend identity changed", false, true, err)
	}
	artifacts, digest, err := artifactContract(request.Artifacts)
	if err != nil {
		return BackendState{}, backendFailure("invalid_work_item", "sandbox artifact contract is invalid", false, true, err)
	}
	deleteReceipt, err := b.adapter.Delete(ctx, "controller-"+item.OperationID, item.WorkspaceID, item.RequestedBy,
		item.SandboxID, *item.BackendUID, *item.BackendResourceVersion, digest)
	if err != nil {
		return BackendState{}, classifyAdapter("cleanup", err)
	}
	if err := verifyReceipt(deleteReceipt, sandboxcontrol.OperationDelete, request, record); err != nil {
		return BackendState{}, backendFailure("cleanup_identity_mismatch", "cleanup delete receipt identity changed", false, true, err)
	}
	live, err := b.adapter.Get(ctx, item.WorkspaceID, item.RequestedBy, item.SandboxID)
	if err != nil {
		return BackendState{}, backendFailure("cleanup_state_ambiguous", "cleanup state could not be proven", true, true, err)
	}
	if live.UID != *item.BackendUID || !live.Deleting || live.ArtifactContractDigest != digest || !reflect.DeepEqual(live.Artifacts, artifacts) {
		return BackendState{}, backendFailure("cleanup_identity_mismatch", "cleanup backend identity changed", false, true, nil)
	}
	identity := observation.Workload
	return BackendState{Record: live, Admission: &identity, AdmissionObservation: &observation,
		Exists: true, Deleting: true, CleanupFinalizerPresent: hasFinalizer(live.Finalizers)}, nil
}

func (b *KubernetesBackend) Finalize(ctx context.Context, item WorkItem, state BackendState) (CleanupResult, error) {
	if state.AdmissionObservation == nil || item.BackendUID == nil || item.Admission == nil {
		return CleanupResult{}, backendFailure("cleanup_evidence_unavailable", "cleanup absence evidence is unavailable after restart", true, true, nil)
	}
	if err := verifyObservation(item, *state.AdmissionObservation); err != nil {
		return CleanupResult{}, backendFailure("cleanup_identity_mismatch", "cleanup backend identity changed", false, true, err)
	}
	request, err := b.request(item)
	if err != nil {
		return CleanupResult{}, err
	}
	artifacts, digest, err := artifactContract(request.Artifacts)
	if err != nil {
		return CleanupResult{}, backendFailure("invalid_work_item", "sandbox artifact contract is invalid", false, true, err)
	}
	receipt, err := b.adapter.Finalize(ctx, "controller-"+item.OperationID, item.WorkspaceID,
		item.RequestedBy, item.SandboxID, *item.BackendUID, state.Record.ResourceVersion, artifacts, digest)
	if err != nil {
		return CleanupResult{}, classifyAdapter("cleanup", err)
	}
	if err := verifyReceipt(receipt, sandboxcontrol.OperationFinalize, request, state.Record); err != nil {
		return CleanupResult{}, backendFailure("cleanup_identity_mismatch", "cleanup receipt identity changed", false, true, err)
	}
	for {
		err = b.adapter.ObserveAbsence(ctx, *state.AdmissionObservation)
		if err == nil {
			ids := make([]string, 0, len(receipt.Artifacts))
			for _, artifact := range receipt.Artifacts {
				ids = append(ids, artifact.ObjectKey)
			}
			return CleanupResult{ArtifactIDs: ids, WarningCodes: []string{}, CleanupComplete: true,
				ArtifactExportComplete: true, GrantsRevoked: true, BackendDestroyed: true}, nil
		}
		var adapterErr *sandboxcontrol.AdapterError
		if !errors.As(err, &adapterErr) || adapterErr.Code != sandboxcontrol.ErrCleanupIncomplete {
			return CleanupResult{}, classifyAdapter("cleanup", err)
		}
		if !wait(ctx, b.absencePollInterval) {
			return CleanupResult{}, ctx.Err()
		}
	}
}

func (b *KubernetesBackend) request(item WorkItem) (sandboxcontrol.CreateRequest, error) {
	if err := validateWorkItem(item); err != nil {
		return sandboxcontrol.CreateRequest{}, backendFailure("invalid_work_item", "controller work item is invalid", false, true, err)
	}
	if len(item.Sources) != 0 {
		return sandboxcontrol.CreateRequest{}, backendFailure("sources_unsupported", "source materialization is not supported by the Kubernetes backend", false, false, nil)
	}
	if !b.artifactExportSupported {
		for _, artifact := range item.Artifacts {
			if artifact.Required {
				return sandboxcontrol.CreateRequest{}, backendFailure("artifact_export_unsupported", "required artifact export transport is unavailable", false, false, nil)
			}
		}
	}
	artifacts := make([]sandboxcontrol.ArtifactExport, len(item.Artifacts))
	for index, artifact := range item.Artifacts {
		artifacts[index] = sandboxcontrol.ArtifactExport{Name: artifact.Name, Path: artifact.Path,
			MediaType: artifact.MediaType, Required: artifact.Required}
	}
	return sandboxcontrol.CreateRequest{RequestID: "controller-" + item.OperationID, Name: item.SandboxID,
		WorkspaceID: item.WorkspaceID, OwnerID: item.RequestedBy, Image: item.ImageDigest,
		Command: append([]string(nil), item.Command...), Architecture: item.Architecture,
		RuntimeClassName: "", TrustLevel: sandboxcontrol.TrustApprovedPOC, NonSensitive: true,
		CPURequest: item.Resources.CPURequest, MemoryRequest: item.Resources.MemoryRequest,
		EphemeralStorageRequest: item.Resources.EphemeralRequest, CPULimit: item.Resources.CPULimit,
		MemoryLimit: item.Resources.MemoryLimit, EphemeralStorageLimit: item.Resources.EphemeralLimit,
		ExpiresAt: item.ExpiresAt, Artifacts: artifacts}, nil
}

func expectedRecord(item WorkItem) sandboxcontrol.SandboxRecord {
	record := sandboxcontrol.SandboxRecord{Name: item.SandboxID, Namespace: sandboxcontrol.Namespace,
		WorkspaceID: item.WorkspaceID, OwnerID: item.RequestedBy, QueueName: sandboxcontrol.QueueName}
	if item.BackendUID != nil {
		record.UID = *item.BackendUID
	}
	if item.BackendResourceVersion != nil {
		record.ResourceVersion = *item.BackendResourceVersion
	}
	return record
}

func stateFromRecord(record sandboxcontrol.SandboxRecord) BackendState {
	return BackendState{Record: record, Exists: true, Ready: false, Deleting: record.Deleting,
		CleanupFinalizerPresent: hasFinalizer(record.Finalizers)}
}

func verifyObservation(item WorkItem, observation sandboxcontrol.AdmissionObservation) error {
	if observation.Sandbox.APIVersion != sandboxcontrol.APIVersion || observation.Sandbox.Kind != sandboxcontrol.Kind ||
		observation.Sandbox.Namespace != sandboxcontrol.Namespace || observation.Sandbox.Name != item.SandboxID ||
		observation.Sandbox.UID == "" || observation.Sandbox.ResourceVersion == "" ||
		observation.Pod.APIVersion != "v1" || observation.Pod.Kind != "Pod" ||
		observation.Pod.Namespace != sandboxcontrol.Namespace || observation.Pod.Name == "" ||
		observation.Pod.UID == "" || observation.Pod.ResourceVersion == "" ||
		observation.Workload.APIVersion != sandboxcontrol.AdmissionAPIVersion ||
		observation.Workload.Namespace != sandboxcontrol.Namespace || observation.Workload.Name == "" ||
		observation.Workload.UID == "" || observation.Workload.ResourceVersion == "" ||
		observation.Workload.WorkspaceID != item.WorkspaceID || observation.Workload.SandboxID != item.SandboxID ||
		observation.Workload.Owner.UID != observation.Sandbox.UID || !observation.Workload.Admitted ||
		observation.Workload.Condition.Type != "Admitted" || observation.Workload.Condition.Status != "True" {
		return errors.New("admission observation does not match the work item")
	}
	if item.BackendUID != nil && observation.Sandbox.UID != *item.BackendUID {
		return errors.New("sandbox UID changed")
	}
	if item.BackendResourceVersion != nil && observation.Sandbox.ResourceVersion != *item.BackendResourceVersion {
		return errors.New("sandbox resource version changed")
	}
	if item.Admission != nil && !reflect.DeepEqual(observation.Workload, *item.Admission) {
		return errors.New("workload admission identity changed")
	}
	return nil
}

func verifyReceipt(receipt sandboxcontrol.OperationReceipt, operation sandboxcontrol.Operation, request sandboxcontrol.CreateRequest, record sandboxcontrol.SandboxRecord) error {
	if err := sandboxcontrol.ValidateReceipt(receipt); err != nil {
		return err
	}
	if receipt.Operation != operation || receipt.RequestID != request.RequestID || receipt.Name != request.Name ||
		receipt.Namespace != sandboxcontrol.Namespace || receipt.WorkspaceID != request.WorkspaceID ||
		receipt.OwnerID != request.OwnerID || receipt.UID != record.UID || receipt.QueueName != sandboxcontrol.QueueName {
		return errors.New("adapter receipt does not match the requested identity")
	}
	return nil
}

func artifactContract(artifacts []sandboxcontrol.ArtifactExport) ([]sandboxcontrol.ArtifactExport, string, error) {
	return sandboxcontrol.CanonicalArtifactContract(artifacts)
}

func hasFinalizer(values []string) bool {
	for _, value := range values {
		if value == sandboxcontrol.CleanupFinalizer {
			return true
		}
	}
	return false
}

func classifyCleanupObservation(err error) error {
	var adapterErr *sandboxcontrol.AdapterError
	if errors.As(err, &adapterErr) && adapterErr.Code == sandboxcontrol.ErrNotFound {
		return backendFailure("cleanup_evidence_unavailable", "cleanup absence evidence is unavailable after restart", true, true, err)
	}
	return classifyAdapter("cleanup", err)
}

func classifyAdapter(operation string, err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var adapterErr *sandboxcontrol.AdapterError
	if !errors.As(err, &adapterErr) {
		return backendFailure("backend_transport_failure", "sandbox backend transport failed", true, false, err)
	}
	switch adapterErr.Code {
	case sandboxcontrol.ErrBackend:
		return backendFailure("backend_transport_failure", "sandbox backend request failed", true, false, err)
	case sandboxcontrol.ErrConflict, sandboxcontrol.ErrIdentityBoundary, sandboxcontrol.ErrResourceVersionStale:
		return backendFailure("backend_identity_mismatch", "sandbox backend identity could not be proven", false, true, err)
	case sandboxcontrol.ErrCleanupIncomplete, sandboxcontrol.ErrArtifactExport:
		return backendFailure("cleanup_incomplete", "sandbox cleanup could not be proven complete", operation == "cleanup", true, err)
	case sandboxcontrol.ErrNotFound:
		return backendFailure("backend_not_found", "sandbox backend identity is absent", operation != "cleanup", operation == "cleanup", err)
	default:
		return backendFailure("backend_request_rejected", "sandbox backend rejected the operation", false, false, err)
	}
}

func backendFailure(code, message string, retryable, ambiguous bool, cause error) error {
	return &Failure{Code: code, SafeMessage: message, Retryable: retryable, Ambiguous: ambiguous, Cause: cause}
}
