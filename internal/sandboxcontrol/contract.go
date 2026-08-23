package sandboxcontrol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	APIVersion             = "agents.x-k8s.io/v1beta1"
	Kind                   = "Sandbox"
	Namespace              = "blazn-poc-sandboxes"
	QueueName              = "blazn-poc"
	QueueLabel             = "kueue.x-k8s.io/queue-name"
	ManagedLabel           = "blazn.dev/managed"
	WorkspaceLabel         = "blazn.dev/workspace"
	OwnerLabel             = "blazn.dev/owner"
	SandboxIDLabel         = "blazn.dev/sandbox-id"
	CleanupFinalizer       = "sandboxes.blazn.dev/artifact-cleanup"
	ServiceAccountName     = "blazn-sandbox-runner"
	ReceiptSchema          = "blazn.dev/sandbox-adapter-receipt/v1"
	AdmissionAPIVersion    = "kueue.x-k8s.io/v1beta1"
	CreateIntentAnnotation = "sandboxes.blazn.dev/create-intent-digest"
	ArtifactSchema         = "blazn.dev/sandbox-artifact/v1"
	OrchestrationNotice    = "orchestration isolation only; approved non-sensitive POC workloads"
)

type ErrorCode string

const (
	ErrInvalidRequest       ErrorCode = "sandbox_invalid_request"
	ErrIdentityBoundary     ErrorCode = "sandbox_identity_boundary"
	ErrQueueRequired        ErrorCode = "sandbox_queue_required"
	ErrRuntimeUntrusted     ErrorCode = "sandbox_runtime_untrusted"
	ErrConflict             ErrorCode = "sandbox_conflict"
	ErrNotFound             ErrorCode = "sandbox_not_found"
	ErrBackend              ErrorCode = "sandbox_backend_failure"
	ErrArtifactExport       ErrorCode = "sandbox_artifact_export_failed"
	ErrCleanupIncomplete    ErrorCode = "sandbox_cleanup_incomplete"
	ErrResourceVersionStale ErrorCode = "sandbox_resource_version_stale"
)

type AdapterError struct {
	Code       ErrorCode
	Status     int
	SafeDetail string
	Cause      error
}

func (e *AdapterError) Error() string {
	if e.SafeDetail == "" {
		return string(e.Code)
	}
	return string(e.Code) + ": " + e.SafeDetail
}

func (e *AdapterError) Unwrap() error { return e.Cause }

type TrustLevel string

const (
	TrustApprovedPOC TrustLevel = "approved_non_sensitive_poc"
	TrustUntrusted   TrustLevel = "untrusted"
)

type SandboxState string

const (
	StatePending  SandboxState = "pending"
	StateQueued   SandboxState = "queued"
	StateStarting SandboxState = "starting"
	StateReady    SandboxState = "ready"
	StateFailed   SandboxState = "failed"
	StateStopping SandboxState = "stopping"
	StateDeleted  SandboxState = "deleted"
)

type RuntimeCapability struct {
	Name          string
	Handler       string
	Architectures []string
	Hardened      bool
	Qualified     bool
}

type ArtifactExport struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	MediaType string `json:"mediaType"`
	Required  bool   `json:"required"`
}

type ArtifactReceipt struct {
	SchemaVersion string `json:"schemaVersion"`
	Name          string `json:"name"`
	ObjectKey     string `json:"objectKey"`
	SHA256        string `json:"sha256"`
	Size          int64  `json:"size"`
	ExportedAt    string `json:"exportedAt"`
}

type CreateRequest struct {
	RequestID               string
	Name                    string
	WorkspaceID             string
	OwnerID                 string
	Image                   string
	Command                 []string
	Architecture            string
	RuntimeClassName        string
	TrustLevel              TrustLevel
	NonSensitive            bool
	CPURequest              string
	MemoryRequest           string
	EphemeralStorageRequest string
	CPULimit                string
	MemoryLimit             string
	EphemeralStorageLimit   string
	ExpiresAt               time.Time
	Artifacts               []ArtifactExport
}

type SandboxStatus struct {
	State              SandboxState `json:"state"`
	Reason             string       `json:"reason,omitempty"`
	Message            string       `json:"message,omitempty"`
	IsolationNotice    string       `json:"isolationNotice,omitempty"`
	Ready              bool         `json:"ready"`
	ResourceVersion    string       `json:"resourceVersion"`
	ObservedGeneration int64        `json:"observedGeneration"`
}

type SandboxRecord struct {
	Name                   string            `json:"name"`
	Namespace              string            `json:"namespace"`
	UID                    string            `json:"uid"`
	ResourceVersion        string            `json:"resourceVersion"`
	Generation             int64             `json:"generation"`
	WorkspaceID            string            `json:"workspaceId"`
	OwnerID                string            `json:"ownerId"`
	QueueName              string            `json:"queueName"`
	RuntimeClassName       string            `json:"runtimeClassName,omitempty"`
	TrustLevel             TrustLevel        `json:"trustLevel"`
	State                  SandboxState      `json:"state"`
	Deleting               bool              `json:"deleting"`
	Finalizers             []string          `json:"finalizers"`
	Artifacts              []ArtifactExport  `json:"artifacts"`
	ArtifactContractDigest string            `json:"artifactContractDigest"`
	CreateIntentDigest     string            `json:"createIntentDigest"`
	Labels                 map[string]string `json:"labels"`
}

type Operation string

const (
	OperationCreate   Operation = "create"
	OperationDelete   Operation = "delete"
	OperationFinalize Operation = "finalize"
)

type OperationReceipt struct {
	SchemaVersion          string            `json:"schemaVersion"`
	ReceiptID              string            `json:"receiptId"`
	RequestID              string            `json:"requestId"`
	Operation              Operation         `json:"operation"`
	Namespace              string            `json:"namespace"`
	Name                   string            `json:"name"`
	UID                    string            `json:"uid"`
	ResourceVersion        string            `json:"resourceVersion"`
	WorkspaceID            string            `json:"workspaceId"`
	OwnerID                string            `json:"ownerId"`
	QueueName              string            `json:"queueName"`
	RuntimeClass           string            `json:"runtimeClass,omitempty"`
	State                  SandboxState      `json:"state"`
	Admission              *WorkloadIdentity `json:"admission,omitempty"`
	Artifacts              []ArtifactReceipt `json:"artifacts"`
	ArtifactContractDigest string            `json:"artifactContractDigest"`
	ObservedAt             string            `json:"observedAt"`
	Digest                 string            `json:"digest"`
}

// WorkloadIdentity is the exact admitted Kueue Workload observation. The UID,
// not a caller-selected name, is the opaque admission ID persisted by the
// control plane; the remaining fields make that identity independently
// auditable and fence observations of a replaced Workload.
type WorkloadIdentity struct {
	APIVersion      string                `json:"apiVersion"`
	Namespace       string                `json:"namespace"`
	Name            string                `json:"name"`
	UID             string                `json:"uid"`
	ResourceVersion string                `json:"resourceVersion"`
	ClusterQueue    string                `json:"clusterQueue"`
	Owner           SandboxOwnerReference `json:"owner"`
	WorkspaceID     string                `json:"workspaceId"`
	SandboxID       string                `json:"sandboxId"`
	Admitted        bool                  `json:"admitted"`
	Condition       AdmissionCondition    `json:"condition"`
	Digest          string                `json:"digest"`
}

// SandboxOwnerReference is the immutable controller owner reference observed
// on the admitted Workload. Name or label correlation alone is insufficient.
type SandboxOwnerReference struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Controller bool   `json:"controller"`
}

type AdmissionCondition struct {
	Type   string `json:"type"`
	Status string `json:"status"`
}

type WatchEvent struct {
	Type    string
	Sandbox SandboxRecord
}

var (
	dnsLabelPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	requestPattern  = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,128}$`)
	imagePattern    = regexp.MustCompile(`^[A-Za-z0-9._:/-]+@sha256:[0-9a-f]{64}$`)
	quantityPattern = regexp.MustCompile(`^[1-9][0-9]*(?:m|Ki|Mi|Gi|Ti)?$`)
	mediaPattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9.+-]*/[a-z0-9][a-z0-9.+-]*$`)
	digestPattern   = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	objectIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	dnsNamePattern  = regexp.MustCompile(`^(?:[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?)(?:\.(?:[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?))*$`)
)

func ValidateCreate(request CreateRequest, runtimes map[string]RuntimeCapability) error {
	if !requestPattern.MatchString(request.RequestID) || !dnsLabelPattern.MatchString(request.Name) || !dnsLabelPattern.MatchString(request.WorkspaceID) || !dnsLabelPattern.MatchString(request.OwnerID) {
		return adapterError(ErrInvalidRequest, 400, "request or identity fields are invalid", nil)
	}
	if !imagePattern.MatchString(request.Image) || len(request.Command) == 0 || len(request.Command) > 32 {
		return adapterError(ErrInvalidRequest, 400, "image or command is invalid", nil)
	}
	for _, argument := range request.Command {
		if argument == "" || len(argument) > 1024 || strings.ContainsRune(argument, '\x00') {
			return adapterError(ErrInvalidRequest, 400, "command argument is invalid", nil)
		}
	}
	if request.Architecture != "amd64" && request.Architecture != "arm64" {
		return adapterError(ErrInvalidRequest, 400, "architecture is invalid", nil)
	}
	for _, quantity := range []string{request.CPURequest, request.MemoryRequest, request.EphemeralStorageRequest, request.CPULimit, request.MemoryLimit, request.EphemeralStorageLimit} {
		if !quantityPattern.MatchString(quantity) {
			return adapterError(ErrInvalidRequest, 400, "resource quantity is invalid", nil)
		}
	}
	if request.ExpiresAt.IsZero() || !request.ExpiresAt.After(time.Now().Add(-time.Minute)) {
		return adapterError(ErrInvalidRequest, 400, "expiry is invalid", nil)
	}
	if err := validateArtifactExports(request.Artifacts); err != nil {
		return err
	}
	switch request.TrustLevel {
	case TrustApprovedPOC:
		if !request.NonSensitive {
			return adapterError(ErrRuntimeUntrusted, 403, "orchestration-only runtime requires approved non-sensitive input", nil)
		}
		if request.RuntimeClassName != "" {
			if err := validateRuntime(request, runtimes, false); err != nil {
				return err
			}
		}
	case TrustUntrusted:
		if err := validateRuntime(request, runtimes, true); err != nil {
			return err
		}
	default:
		return adapterError(ErrRuntimeUntrusted, 403, "trust level is invalid", nil)
	}
	return nil
}

// createIntentDigest is the adapter's internal idempotency boundary. It binds
// every CreateRequest field after set-like values have been canonicalized.
func createIntentDigest(request CreateRequest) (string, error) {
	canonicalArtifacts, _, err := CanonicalArtifactContract(request.Artifacts)
	if err != nil {
		return "", err
	}
	request.Artifacts = canonicalArtifacts
	type canonicalCreateIntent struct {
		Schema                  string           `json:"schema"`
		RequestID               string           `json:"requestId"`
		Name                    string           `json:"name"`
		WorkspaceID             string           `json:"workspaceId"`
		OwnerID                 string           `json:"ownerId"`
		Image                   string           `json:"image"`
		Command                 []string         `json:"command"`
		Architecture            string           `json:"architecture"`
		RuntimeClassName        string           `json:"runtimeClassName"`
		TrustLevel              TrustLevel       `json:"trustLevel"`
		NonSensitive            bool             `json:"nonSensitive"`
		CPURequest              string           `json:"cpuRequest"`
		MemoryRequest           string           `json:"memoryRequest"`
		EphemeralStorageRequest string           `json:"ephemeralStorageRequest"`
		CPULimit                string           `json:"cpuLimit"`
		MemoryLimit             string           `json:"memoryLimit"`
		EphemeralStorageLimit   string           `json:"ephemeralStorageLimit"`
		ExpiresAt               string           `json:"expiresAt"`
		Artifacts               []ArtifactExport `json:"artifacts"`
	}
	encoded, err := json.Marshal(canonicalCreateIntent{
		Schema: "blazn.dev/sandbox-create-intent/v1", RequestID: request.RequestID,
		Name: request.Name, WorkspaceID: request.WorkspaceID, OwnerID: request.OwnerID,
		Image: request.Image, Command: append([]string(nil), request.Command...),
		Architecture: request.Architecture, RuntimeClassName: request.RuntimeClassName,
		TrustLevel: request.TrustLevel, NonSensitive: request.NonSensitive,
		CPURequest: request.CPURequest, MemoryRequest: request.MemoryRequest,
		EphemeralStorageRequest: request.EphemeralStorageRequest, CPULimit: request.CPULimit,
		MemoryLimit: request.MemoryLimit, EphemeralStorageLimit: request.EphemeralStorageLimit,
		ExpiresAt: request.ExpiresAt.UTC().Format(time.RFC3339Nano), Artifacts: canonicalArtifacts,
	})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func validateArtifactExports(artifacts []ArtifactExport) error {
	if len(artifacts) > 32 {
		return adapterError(ErrInvalidRequest, 400, "artifact export limit exceeded", nil)
	}
	seen := map[string]bool{}
	for _, artifact := range artifacts {
		if !dnsLabelPattern.MatchString(artifact.Name) || seen[artifact.Name] || !strings.HasPrefix(artifact.Path, "/workspace/artifacts/") || path.Clean(artifact.Path) != artifact.Path || strings.Contains(artifact.Path, "..") || !mediaPattern.MatchString(artifact.MediaType) {
			return adapterError(ErrInvalidRequest, 400, "artifact export is invalid", nil)
		}
		seen[artifact.Name] = true
	}
	return nil
}

func validateRuntime(request CreateRequest, runtimes map[string]RuntimeCapability, hardened bool) error {
	capability, exists := runtimes[request.RuntimeClassName]
	if request.RuntimeClassName == "" || !dnsLabelPattern.MatchString(request.RuntimeClassName) || !exists || capability.Name != request.RuntimeClassName || !dnsLabelPattern.MatchString(capability.Handler) || !capability.Qualified || (hardened && !capability.Hardened) {
		return adapterError(ErrRuntimeUntrusted, 403, "runtime capability is not qualified for requested trust", nil)
	}
	for _, architecture := range capability.Architectures {
		if architecture == request.Architecture {
			return nil
		}
	}
	return adapterError(ErrRuntimeUntrusted, 403, "runtime capability does not support architecture", nil)
}

func ValidateReceipt(receipt OperationReceipt) error {
	if receipt.SchemaVersion != ReceiptSchema || !requestPattern.MatchString(receipt.RequestID) || receipt.ReceiptID != receipt.RequestID+":"+string(receipt.Operation) || !dnsLabelPattern.MatchString(receipt.Name) || receipt.Namespace != Namespace || !objectIDPattern.MatchString(receipt.UID) || !objectIDPattern.MatchString(receipt.ResourceVersion) || !dnsLabelPattern.MatchString(receipt.WorkspaceID) || !dnsLabelPattern.MatchString(receipt.OwnerID) || receipt.QueueName != QueueName || !digestPattern.MatchString(receipt.ArtifactContractDigest) || !digestPattern.MatchString(receipt.Digest) {
		return fmt.Errorf("sandbox adapter receipt identity is invalid")
	}
	if receipt.Operation != OperationCreate && receipt.Operation != OperationDelete && receipt.Operation != OperationFinalize {
		return fmt.Errorf("sandbox adapter receipt operation is invalid")
	}
	if receipt.Admission != nil {
		if receipt.Operation != OperationCreate || validateWorkloadIdentity(*receipt.Admission, receipt, true) != nil {
			return fmt.Errorf("sandbox adapter receipt admission identity is invalid")
		}
	}
	if receipt.Operation == OperationDelete && receipt.State != StateStopping || receipt.Operation == OperationFinalize && receipt.State != StateDeleted || receipt.Operation == OperationCreate && (receipt.State == StateStopping || receipt.State == StateDeleted) {
		return fmt.Errorf("sandbox adapter receipt state is incoherent")
	}
	if _, err := time.Parse(time.RFC3339Nano, receipt.ObservedAt); err != nil {
		return fmt.Errorf("sandbox adapter receipt timestamp is invalid")
	}
	seenArtifacts := map[string]bool{}
	artifactPrefix := "workspaces/" + receipt.WorkspaceID + "/sandboxes/" + receipt.Name + "/artifacts/"
	for _, artifact := range receipt.Artifacts {
		if artifact.SchemaVersion != ArtifactSchema || !dnsLabelPattern.MatchString(artifact.Name) || seenArtifacts[artifact.Name] || artifact.ObjectKey != artifactPrefix+artifact.Name || path.Clean(artifact.ObjectKey) != artifact.ObjectKey || strings.Contains(artifact.ObjectKey, "..") || !digestPattern.MatchString(artifact.SHA256) || artifact.Size < 0 {
			return fmt.Errorf("sandbox artifact receipt is invalid")
		}
		seenArtifacts[artifact.Name] = true
		if _, err := time.Parse(time.RFC3339Nano, artifact.ExportedAt); err != nil {
			return fmt.Errorf("sandbox artifact timestamp is invalid")
		}
	}
	expected, err := receiptDigest(receipt)
	if err != nil || expected != receipt.Digest {
		return fmt.Errorf("sandbox adapter receipt digest mismatch")
	}
	return nil
}

// AttachAdmissionIdentity returns a newly digested create receipt bound to the
// exact admitted Workload observation. It refuses mutable name-only identity.
func AttachAdmissionIdentity(receipt OperationReceipt, identity WorkloadIdentity) (OperationReceipt, error) {
	if err := ValidateReceipt(receipt); err != nil {
		return OperationReceipt{}, err
	}
	if receipt.Operation != OperationCreate || receipt.Admission != nil {
		return OperationReceipt{}, fmt.Errorf("admission identity can only be attached once to a create receipt")
	}
	identity.Digest = ""
	if err := validateWorkloadIdentity(identity, receipt, false); err != nil {
		return OperationReceipt{}, err
	}
	identity.Digest = workloadIdentityDigest(identity)
	receipt.Admission = &identity
	digest, err := receiptDigest(receipt)
	if err != nil {
		return OperationReceipt{}, err
	}
	receipt.Digest = digest
	return receipt, nil
}

// ValidateTerminalCreateReceipt is the stricter persistence boundary used by
// the controller after admission. Adapter create receipts remain valid before
// Kueue has produced an admitted Workload observation.
func ValidateTerminalCreateReceipt(receipt OperationReceipt) error {
	if err := ValidateReceipt(receipt); err != nil {
		return err
	}
	if receipt.Operation != OperationCreate || receipt.Admission == nil {
		return fmt.Errorf("terminal sandbox create receipt requires an exact admission identity")
	}
	return nil
}

func validateWorkloadIdentity(identity WorkloadIdentity, receipt OperationReceipt, requireDigest bool) error {
	if identity.APIVersion != AdmissionAPIVersion || identity.Namespace != receipt.Namespace || len(identity.Name) > 253 ||
		!dnsNamePattern.MatchString(identity.Name) || !objectIDPattern.MatchString(identity.UID) ||
		!objectIDPattern.MatchString(identity.ResourceVersion) || len(identity.ClusterQueue) > 253 ||
		!dnsNamePattern.MatchString(identity.ClusterQueue) || identity.WorkspaceID != receipt.WorkspaceID ||
		identity.SandboxID != receipt.Name || identity.Owner.APIVersion != APIVersion || identity.Owner.Kind != Kind ||
		identity.Owner.Name != receipt.Name || identity.Owner.UID != receipt.UID || !identity.Owner.Controller ||
		!identity.Admitted || identity.Condition.Type != "Admitted" || identity.Condition.Status != "True" ||
		(requireDigest && identity.Digest != workloadIdentityDigest(identity)) {
		return fmt.Errorf("sandbox admission Workload identity is invalid")
	}
	return nil
}

func workloadIdentityDigest(identity WorkloadIdentity) string {
	canonical := strings.Join([]string{
		"sandbox-workload-admission-v1", identity.APIVersion, identity.Namespace, identity.Name,
		identity.UID, identity.ResourceVersion, identity.ClusterQueue, identity.Owner.APIVersion,
		identity.Owner.Kind, identity.Owner.Name, identity.Owner.UID, fmt.Sprintf("%t", identity.Owner.Controller),
		identity.WorkspaceID, identity.SandboxID, fmt.Sprintf("%t", identity.Admitted),
		identity.Condition.Type, identity.Condition.Status,
	}, "\n")
	digest := sha256.Sum256([]byte(canonical))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func admissionObservationDigest(observation AdmissionObservation) string {
	canonical := strings.Join([]string{
		"sandbox-admission-observation-v1",
		observation.Sandbox.APIVersion, observation.Sandbox.Kind, observation.Sandbox.Namespace,
		observation.Sandbox.Name, observation.Sandbox.UID, observation.Sandbox.ResourceVersion,
		observation.Pod.APIVersion, observation.Pod.Kind, observation.Pod.Namespace,
		observation.Pod.Name, observation.Pod.UID, observation.Pod.ResourceVersion,
		observation.Workload.APIVersion, observation.Workload.Namespace, observation.Workload.Name,
		observation.Workload.UID, observation.Workload.ResourceVersion, observation.Workload.ClusterQueue,
		observation.Workload.Owner.APIVersion, observation.Workload.Owner.Kind,
		observation.Workload.Owner.Name, observation.Workload.Owner.UID,
		fmt.Sprintf("%t", observation.Workload.Owner.Controller), observation.Workload.WorkspaceID,
		observation.Workload.SandboxID, fmt.Sprintf("%t", observation.Workload.Admitted),
		observation.Workload.Condition.Type, observation.Workload.Condition.Status,
		observation.Workload.Digest,
	}, "\n")
	digest := sha256.Sum256([]byte(canonical))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func NewReceipt(requestID string, operation Operation, sandbox SandboxRecord, artifacts []ArtifactReceipt, now time.Time) (OperationReceipt, error) {
	receipt := OperationReceipt{
		SchemaVersion: ReceiptSchema, ReceiptID: requestID + ":" + string(operation), RequestID: requestID,
		Operation: operation, Namespace: sandbox.Namespace, Name: sandbox.Name, UID: sandbox.UID,
		ResourceVersion: sandbox.ResourceVersion, WorkspaceID: sandbox.WorkspaceID, OwnerID: sandbox.OwnerID,
		QueueName: sandbox.QueueName, RuntimeClass: sandbox.RuntimeClassName, State: sandbox.State,
		Artifacts: append([]ArtifactReceipt(nil), artifacts...), ArtifactContractDigest: sandbox.ArtifactContractDigest, ObservedAt: now.UTC().Format(time.RFC3339Nano),
	}
	sort.Slice(receipt.Artifacts, func(i, j int) bool { return receipt.Artifacts[i].Name < receipt.Artifacts[j].Name })
	digest, err := receiptDigest(receipt)
	if err != nil {
		return receipt, err
	}
	receipt.Digest = digest
	return receipt, nil
}

func CanonicalArtifactContract(artifacts []ArtifactExport) ([]ArtifactExport, string, error) {
	canonical := make([]ArtifactExport, len(artifacts))
	copy(canonical, artifacts)
	if err := validateArtifactExports(canonical); err != nil {
		return nil, "", err
	}
	sort.Slice(canonical, func(i, j int) bool { return canonical[i].Name < canonical[j].Name })
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return nil, "", err
	}
	digest := sha256.Sum256(encoded)
	return canonical, "sha256:" + hex.EncodeToString(digest[:]), nil
}

func receiptDigest(receipt OperationReceipt) (string, error) {
	copy := receipt
	copy.Digest = ""
	encoded, err := json.Marshal(copy)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func adapterError(code ErrorCode, status int, detail string, cause error) error {
	_ = status
	return &AdapterError{Code: code, Status: errorStatus(code), SafeDetail: detail, Cause: cause}
}

func errorStatus(code ErrorCode) int {
	switch code {
	case ErrInvalidRequest:
		return 400
	case ErrIdentityBoundary, ErrNotFound:
		return 404
	case ErrRuntimeUntrusted:
		return 403
	case ErrConflict, ErrCleanupIncomplete, ErrResourceVersionStale:
		return 409
	case ErrQueueRequired, ErrBackend, ErrArtifactExport:
		return 502
	default:
		return 500
	}
}
