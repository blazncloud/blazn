package sandboxcontroller

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"time"

	"github.com/blazncloud/blazn/internal/sandboxcontrol"
	"github.com/blazncloud/blazn/internal/sandboxio"
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
	HelperImage             string
	SourceRuntime           *KubernetesSourceRuntime
	AbsencePollInterval     time.Duration
}

type KubernetesBackend struct {
	adapter                 SandboxControlAdapter
	health                  func(context.Context) error
	artifactExportSupported bool
	helperImage             string
	sourceRuntime           *KubernetesSourceRuntime
	absencePollInterval     time.Duration
	createLocksMu           sync.Mutex
	createLocks             map[string]*kubernetesCreateLock
	evidenceMu              sync.Mutex
	evidence                map[string]kubernetesBackendEvidence
}

type kubernetesCreateLock struct {
	mu   sync.Mutex
	refs int
}

type kubernetesBackendEvidence struct {
	record sandboxcontrol.SandboxRecord
}

func NewKubernetesBackend(config KubernetesBackendConfig) (*KubernetesBackend, error) {
	if config.Adapter == nil || config.Health == nil || !immutableImagePattern.MatchString(config.HelperImage) {
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
		helperImage:             config.HelperImage,
		sourceRuntime:           config.SourceRuntime,
		absencePollInterval:     config.AbsencePollInterval,
		createLocks:             make(map[string]*kubernetesCreateLock),
		evidence:                make(map[string]kubernetesBackendEvidence)}, nil
}

func (b *KubernetesBackend) PrepareSourceBootstrap(ctx context.Context, item WorkItem, observation sandboxcontrol.AdmissionObservation) error {
	if b.sourceRuntime == nil {
		return backendFailure("sources_unsupported", "source materialization runtime is unavailable", false, true, nil)
	}
	return b.sourceRuntime.Prepare(ctx, item, observation)
}

func (b *KubernetesBackend) MaterializeSources(ctx context.Context, item WorkItem, observation sandboxcontrol.AdmissionObservation) (sandboxio.SourceMaterializationReceipt, error) {
	if b.sourceRuntime == nil {
		return sandboxio.SourceMaterializationReceipt{}, backendFailure("sources_unsupported", "source materialization runtime is unavailable", false, true, nil)
	}
	return b.sourceRuntime.Materialize(ctx, item, observation)
}

func (b *KubernetesBackend) RestrictSourceRuntime(ctx context.Context, item WorkItem, observation sandboxcontrol.AdmissionObservation, receipt sandboxio.SourceMaterializationReceipt) error {
	if b.sourceRuntime == nil {
		return backendFailure("sources_unsupported", "source materialization runtime is unavailable", false, true, nil)
	}
	return b.sourceRuntime.Restrict(ctx, item, observation, receipt)
}

func (b *KubernetesBackend) ReleaseSources(ctx context.Context, item WorkItem, observation sandboxcontrol.AdmissionObservation, receipt sandboxio.SourceMaterializationReceipt) error {
	if b.sourceRuntime == nil {
		return backendFailure("sources_unsupported", "source materialization runtime is unavailable", false, true, nil)
	}
	return b.sourceRuntime.Release(ctx, item, observation, receipt)
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
	unlock := b.lockCreate(item)
	defer unlock()
	if err := ctx.Err(); err != nil {
		return BackendState{}, err
	}
	trusted, retained := b.retainedRecord(item)
	expectedUID := ""
	if retained {
		expectedUID = trusted.UID
	}
	record, receipt, err := b.adapter.EnsureCreated(ctx, request, expectedUID)
	if err != nil {
		return BackendState{}, classifyAdapter("create", err)
	}
	if err := verifyReceipt(receipt, sandboxcontrol.OperationCreate, request, record); err != nil {
		return BackendState{}, backendFailure("backend_identity_mismatch", "sandbox create receipt identity changed", false, true, err)
	}
	if err := verifyLiveRecord(item, request, record, retainedRecord(retained, trusted), false, true); err != nil {
		return BackendState{}, backendFailure("backend_identity_mismatch", "sandbox create identity changed", false, true, err)
	}
	b.retainRecord(item, record)
	return stateFromRecord(record), nil
}

func (b *KubernetesBackend) Observe(ctx context.Context, item WorkItem, expected *sandboxcontrol.AdmissionObservation) (BackendState, error) {
	request, err := b.request(item)
	if err != nil {
		return BackendState{}, err
	}
	trusted, retained := b.retainedRecord(item)
	if item.BackendUID == nil && !retained {
		return BackendState{}, backendFailure("backend_identity_unavailable", "sandbox create identity is unavailable after restart", false, true, nil)
	}
	record, err := b.adapter.Get(ctx, item.WorkspaceID, item.RequestedBy, item.SandboxID)
	if err != nil {
		return BackendState{}, classifyAdapter("observe", err)
	}
	if err := verifyLiveRecord(item, request, record, retainedRecord(retained, trusted), item.BackendUID != nil, true); err != nil {
		return BackendState{}, backendFailure("backend_identity_mismatch", "backend identity changed before admission", false, true, err)
	}
	if record.State != sandboxcontrol.StateReady {
		return stateFromRecord(record), nil
	}
	observation, err := b.adapter.ObserveAdmission(ctx, request, record, expected)
	if err != nil {
		return BackendState{}, classifyAdapter("observe", err)
	}
	if err := verifyObservation(item, observation); err != nil {
		return BackendState{}, backendFailure("backend_identity_mismatch", "backend admission identity changed", false, true, err)
	}
	if observation.Sandbox.UID != record.UID || observation.Sandbox.ResourceVersion != record.ResourceVersion {
		return BackendState{}, backendFailure("backend_identity_mismatch", "backend admission record changed", false, true, nil)
	}
	return BackendState{Record: record, AdmissionObservation: &observation,
		Exists: true, Ready: true}, nil
}

func (b *KubernetesBackend) BeginDelete(ctx context.Context, item WorkItem, expected *sandboxcontrol.AdmissionObservation) (BackendState, error) {
	request, err := b.request(item)
	if err != nil {
		return BackendState{}, err
	}
	if item.BackendUID == nil || item.BackendResourceVersion == nil || expected == nil {
		return BackendState{}, backendFailure("cleanup_identity_missing", "cleanup lacks exact backend evidence", true, true, nil)
	}
	if err := verifyObservation(item, *expected); err != nil {
		return BackendState{}, backendFailure("cleanup_identity_mismatch", "persisted cleanup identity is invalid", false, true, err)
	}
	record, err := b.adapter.Get(ctx, item.WorkspaceID, item.RequestedBy, item.SandboxID)
	if err != nil {
		var adapterErr *sandboxcontrol.AdapterError
		if errors.As(err, &adapterErr) && adapterErr.Code == sandboxcontrol.ErrNotFound {
			if err := b.adapter.ObserveAbsence(ctx, *expected); err != nil {
				return BackendState{}, classifyCleanupObservation(err)
			}
			return BackendState{AdmissionObservation: expected}, nil
		}
		return BackendState{}, classifyCleanupObservation(err)
	}
	if err := verifyLiveRecord(item, request, record, nil, !record.Deleting, true); err != nil {
		return BackendState{}, backendFailure("cleanup_identity_mismatch", "cleanup backend identity changed", false, true, err)
	}
	if record.Deleting {
		return BackendState{Record: record, AdmissionObservation: expected,
			Exists: true, Deleting: true, CleanupFinalizerPresent: hasFinalizer(record.Finalizers)}, nil
	}
	observation, err := b.adapter.ObserveAdmission(ctx, request, record, expected)
	if err != nil {
		return BackendState{}, classifyCleanupObservation(err)
	}
	if err := verifyObservation(item, observation); err != nil {
		return BackendState{}, backendFailure("backend_identity_mismatch", "cleanup backend identity changed", false, true, err)
	}
	if observation.Sandbox.UID != record.UID || observation.Sandbox.ResourceVersion != record.ResourceVersion {
		return BackendState{}, backendFailure("backend_identity_mismatch", "cleanup admission record changed", false, true, nil)
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
	if err := verifyLiveRecord(item, request, live, &record, false, true); err != nil ||
		live.ResourceVersion == *item.BackendResourceVersion || !live.Deleting ||
		live.ArtifactContractDigest != digest || !reflect.DeepEqual(live.Artifacts, artifacts) {
		return BackendState{}, backendFailure("cleanup_identity_mismatch", "cleanup backend identity changed", false, true, nil)
	}
	return BackendState{Record: live, AdmissionObservation: &observation,
		Exists: true, Deleting: true, CleanupFinalizerPresent: hasFinalizer(live.Finalizers)}, nil
}

func (b *KubernetesBackend) Finalize(ctx context.Context, item WorkItem, state BackendState, expected *sandboxcontrol.AdmissionObservation) (CleanupResult, error) {
	if item.BackendUID == nil || expected == nil {
		return CleanupResult{}, backendFailure("cleanup_evidence_unavailable", "cleanup absence evidence is unavailable after restart", true, true, nil)
	}
	if state.AdmissionObservation == nil {
		return CleanupResult{}, backendFailure("cleanup_evidence_unavailable", "cleanup state lacks persisted observation", true, true, nil)
	}
	if !reflect.DeepEqual(*state.AdmissionObservation, *expected) {
		return CleanupResult{}, backendFailure("cleanup_identity_mismatch", "cleanup state did not retain persisted observation", false, true, nil)
	}
	if err := verifyObservation(item, *expected); err != nil {
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
	if !state.Exists || !state.Deleting || !state.CleanupFinalizerPresent {
		return CleanupResult{}, backendFailure("cleanup_evidence_unavailable", "cleanup completion receipt is unavailable after restart", true, true, nil)
	}
	receipt, err := b.adapter.Finalize(ctx, "controller-"+item.OperationID, item.WorkspaceID,
		item.RequestedBy, item.SandboxID, *item.BackendUID, state.Record.ResourceVersion, artifacts, digest)
	if err != nil {
		return CleanupResult{}, classifyAdapter("cleanup", err)
	}
	if err := verifyReceipt(receipt, sandboxcontrol.OperationFinalize, request, state.Record); err != nil {
		return CleanupResult{}, backendFailure("cleanup_identity_mismatch", "cleanup receipt identity changed", false, true, err)
	}
	ids := make([]string, 0, len(receipt.Artifacts))
	for _, artifact := range receipt.Artifacts {
		ids = append(ids, artifact.ObjectKey)
	}
	result := CleanupResult{ArtifactIDs: ids, WarningCodes: []string{}, CleanupComplete: true,
		ArtifactExportComplete: true, GrantsRevoked: true, BackendDestroyed: true}
	for {
		err = b.adapter.ObserveAbsence(ctx, *expected)
		if err == nil {
			return result, nil
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

func verifyLiveRecord(item WorkItem, request sandboxcontrol.CreateRequest, record sandboxcontrol.SandboxRecord,
	trusted *sandboxcontrol.SandboxRecord, requirePersistedVersion, requireFinalizer bool) error {
	artifacts, digest, err := artifactContract(request.Artifacts)
	if err != nil {
		return err
	}
	if record.Name != item.SandboxID || record.Namespace != sandboxcontrol.Namespace || record.WorkspaceID != item.WorkspaceID ||
		record.OwnerID != item.RequestedBy || record.QueueName != sandboxcontrol.QueueName || record.UID == "" ||
		record.ResourceVersion == "" || record.RuntimeClassName != "" || record.TrustLevel != sandboxcontrol.TrustApprovedPOC ||
		record.ArtifactContractDigest != digest || !reflect.DeepEqual(record.Artifacts, artifacts) ||
		requireFinalizer && !hasFinalizer(record.Finalizers) {
		return errors.New("live Sandbox record does not match the trusted work item")
	}
	if trusted != nil && (record.UID != trusted.UID || trusted.CreateIntentDigest != "" && record.CreateIntentDigest != trusted.CreateIntentDigest) {
		return errors.New("live Sandbox record changed from the created identity")
	}
	if item.BackendUID != nil && record.UID != *item.BackendUID {
		return errors.New("live Sandbox UID changed")
	}
	if requirePersistedVersion && item.BackendResourceVersion != nil && record.ResourceVersion != *item.BackendResourceVersion {
		return errors.New("live Sandbox resource version changed before mutation")
	}
	return nil
}

func retainedRecord(ok bool, record sandboxcontrol.SandboxRecord) *sandboxcontrol.SandboxRecord {
	if !ok {
		return nil
	}
	copy := cloneSandboxRecord(record)
	return &copy
}

func evidenceKey(item WorkItem) string {
	return item.WorkspaceID + "\x00" + item.RequestedBy + "\x00" + item.SandboxID
}

func (b *KubernetesBackend) lockCreate(item WorkItem) func() {
	key := evidenceKey(item)
	b.createLocksMu.Lock()
	lock := b.createLocks[key]
	if lock == nil {
		lock = &kubernetesCreateLock{}
		b.createLocks[key] = lock
	}
	lock.refs++
	b.createLocksMu.Unlock()
	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()
		b.createLocksMu.Lock()
		defer b.createLocksMu.Unlock()
		lock.refs--
		if lock.refs == 0 && b.createLocks[key] == lock {
			delete(b.createLocks, key)
		}
	}
}

func (b *KubernetesBackend) retainRecord(item WorkItem, record sandboxcontrol.SandboxRecord) {
	b.evidenceMu.Lock()
	defer b.evidenceMu.Unlock()
	entry := b.evidence[evidenceKey(item)]
	entry.record = cloneSandboxRecord(record)
	b.evidence[evidenceKey(item)] = entry
}

func (b *KubernetesBackend) retainedRecord(item WorkItem) (sandboxcontrol.SandboxRecord, bool) {
	b.evidenceMu.Lock()
	defer b.evidenceMu.Unlock()
	entry, ok := b.evidence[evidenceKey(item)]
	if !ok || entry.record.UID == "" {
		return sandboxcontrol.SandboxRecord{}, false
	}
	return cloneSandboxRecord(entry.record), true
}

func cloneSandboxRecord(record sandboxcontrol.SandboxRecord) sandboxcontrol.SandboxRecord {
	copy := record
	copy.Finalizers = append([]string(nil), record.Finalizers...)
	if record.Artifacts != nil {
		copy.Artifacts = append([]sandboxcontrol.ArtifactExport{}, record.Artifacts...)
	}
	copy.Labels = make(map[string]string, len(record.Labels))
	for key, value := range record.Labels {
		copy.Labels[key] = value
	}
	return copy
}

func (b *KubernetesBackend) request(item WorkItem) (sandboxcontrol.CreateRequest, error) {
	if err := validateWorkItem(item); err != nil {
		return sandboxcontrol.CreateRequest{}, backendFailure("invalid_work_item", "controller work item is invalid", false, true, err)
	}
	if len(item.Sources) != 0 && b.sourceRuntime == nil {
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
	var sources []sandboxcontrol.SourceMount
	if len(item.Sources) != 0 {
		sources = make([]sandboxcontrol.SourceMount, len(item.Sources))
		for index, source := range item.Sources {
			sources[index] = sandboxcontrol.SourceMount{Name: source.Name, URL: source.URL, Destination: source.Destination,
				Commit: source.Commit, Writable: source.Writable}
		}
	}
	helperImage := ""
	if len(item.Sources) != 0 || len(artifacts) != 0 {
		helperImage = b.helperImage
	}
	return sandboxcontrol.CreateRequest{RequestID: "controller-" + item.OperationID, Name: item.SandboxID,
		WorkspaceID: item.WorkspaceID, OwnerID: item.RequestedBy, Image: item.ImageDigest,
		HelperImage: helperImage,
		Command:     append([]string(nil), item.Command...), Architecture: item.Architecture,
		RuntimeClassName: "", TrustLevel: sandboxcontrol.TrustApprovedPOC, NonSensitive: true,
		CPURequest: item.Resources.CPURequest, MemoryRequest: item.Resources.MemoryRequest,
		EphemeralStorageRequest: item.Resources.EphemeralRequest, CPULimit: item.Resources.CPULimit,
		MemoryLimit: item.Resources.MemoryLimit, EphemeralStorageLimit: item.Resources.EphemeralLimit,
		ExpiresAt: item.ExpiresAt, Sources: sources, Artifacts: artifacts}, nil
}

func stateFromRecord(record sandboxcontrol.SandboxRecord) BackendState {
	return BackendState{Record: record, Exists: true, Ready: false, Deleting: record.Deleting,
		CleanupFinalizerPresent: hasFinalizer(record.Finalizers)}
}

func verifyObservation(item WorkItem, observation sandboxcontrol.AdmissionObservation) error {
	if err := sandboxcontrol.ValidateAdmissionObservation(observation); err != nil {
		return err
	}
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
	if item.AdmissionObservation != nil && !reflect.DeepEqual(observation, *item.AdmissionObservation) {
		return errors.New("admission observation changed")
	}
	return nil
}

func verifyReceipt(receipt sandboxcontrol.OperationReceipt, operation sandboxcontrol.Operation, request sandboxcontrol.CreateRequest, record sandboxcontrol.SandboxRecord) error {
	if err := sandboxcontrol.ValidateReceipt(receipt); err != nil {
		return err
	}
	_, artifactDigest, err := artifactContract(request.Artifacts)
	if err != nil {
		return err
	}
	if receipt.Operation != operation || receipt.RequestID != request.RequestID || receipt.Name != request.Name ||
		receipt.Namespace != sandboxcontrol.Namespace || receipt.WorkspaceID != request.WorkspaceID ||
		receipt.OwnerID != request.OwnerID || receipt.UID != record.UID || receipt.QueueName != sandboxcontrol.QueueName ||
		receipt.RuntimeClass != request.RuntimeClassName || receipt.ArtifactContractDigest != artifactDigest ||
		operation != sandboxcontrol.OperationFinalize && receipt.ResourceVersion != record.ResourceVersion ||
		operation == sandboxcontrol.OperationFinalize && receipt.ResourceVersion == record.ResourceVersion {
		return errors.New("adapter receipt does not match the requested identity")
	}
	if operation != sandboxcontrol.OperationFinalize && len(receipt.Artifacts) != 0 {
		return errors.New("non-finalize receipt returned artifact evidence")
	}
	if operation == sandboxcontrol.OperationFinalize {
		expected := make(map[string]sandboxcontrol.ArtifactExport, len(request.Artifacts))
		seen := make(map[string]bool, len(receipt.Artifacts))
		for _, artifact := range request.Artifacts {
			expected[artifact.Name] = artifact
		}
		for _, artifact := range receipt.Artifacts {
			if _, ok := expected[artifact.Name]; !ok || seen[artifact.Name] {
				return errors.New("finalize receipt returned unexpected artifact evidence")
			}
			seen[artifact.Name] = true
		}
		for name, artifact := range expected {
			if artifact.Required && !seen[name] {
				return errors.New("finalize receipt omitted required artifact evidence")
			}
		}
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
	case sandboxcontrol.ErrArtifactExport:
		return backendFailure("cleanup_incomplete", "sandbox artifact export did not complete", operation == "cleanup", false, err)
	case sandboxcontrol.ErrCleanupIncomplete:
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
