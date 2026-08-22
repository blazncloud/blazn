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
	APIVersion          = "agents.x-k8s.io/v1beta1"
	Kind                = "Sandbox"
	Namespace           = "blazn-poc-sandboxes"
	QueueName           = "blazn-poc"
	QueueLabel          = "kueue.x-k8s.io/queue-name"
	ManagedLabel        = "blazn.dev/managed"
	WorkspaceLabel      = "blazn.dev/workspace"
	OwnerLabel          = "blazn.dev/owner"
	SandboxIDLabel      = "blazn.dev/sandbox-id"
	CleanupFinalizer    = "sandboxes.blazn.dev/artifact-cleanup"
	ServiceAccountName  = "blazn-sandbox-runner"
	ReceiptSchema       = "blazn.dev/sandbox-adapter-receipt/v1"
	ArtifactSchema      = "blazn.dev/sandbox-artifact/v1"
	OrchestrationNotice = "orchestration isolation only; approved non-sensitive POC workloads"
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
	RequestID        string
	Name             string
	WorkspaceID      string
	OwnerID          string
	Image            string
	Command          []string
	Architecture     string
	RuntimeClassName string
	TrustLevel       TrustLevel
	NonSensitive     bool
	CPURequest       string
	MemoryRequest    string
	CPULimit         string
	MemoryLimit      string
	ExpiresAt        time.Time
	Artifacts        []ArtifactExport
}

type SandboxStatus struct {
	State              SandboxState `json:"state"`
	Reason             string       `json:"reason,omitempty"`
	Message            string       `json:"message,omitempty"`
	Ready              bool         `json:"ready"`
	ResourceVersion    string       `json:"resourceVersion"`
	ObservedGeneration int64        `json:"observedGeneration"`
}

type SandboxRecord struct {
	Name             string            `json:"name"`
	Namespace        string            `json:"namespace"`
	UID              string            `json:"uid"`
	ResourceVersion  string            `json:"resourceVersion"`
	Generation       int64             `json:"generation"`
	WorkspaceID      string            `json:"workspaceId"`
	OwnerID          string            `json:"ownerId"`
	QueueName        string            `json:"queueName"`
	RuntimeClassName string            `json:"runtimeClassName,omitempty"`
	TrustLevel       TrustLevel        `json:"trustLevel"`
	State            SandboxState      `json:"state"`
	Deleting         bool              `json:"deleting"`
	Finalizers       []string          `json:"finalizers"`
	Artifacts        []ArtifactExport  `json:"artifacts"`
	Labels           map[string]string `json:"labels"`
}

type Operation string

const (
	OperationCreate   Operation = "create"
	OperationDelete   Operation = "delete"
	OperationFinalize Operation = "finalize"
)

type OperationReceipt struct {
	SchemaVersion   string            `json:"schemaVersion"`
	ReceiptID       string            `json:"receiptId"`
	RequestID       string            `json:"requestId"`
	Operation       Operation         `json:"operation"`
	Namespace       string            `json:"namespace"`
	Name            string            `json:"name"`
	UID             string            `json:"uid"`
	ResourceVersion string            `json:"resourceVersion"`
	WorkspaceID     string            `json:"workspaceId"`
	OwnerID         string            `json:"ownerId"`
	QueueName       string            `json:"queueName"`
	RuntimeClass    string            `json:"runtimeClass,omitempty"`
	State           SandboxState      `json:"state"`
	Artifacts       []ArtifactReceipt `json:"artifacts"`
	ObservedAt      string            `json:"observedAt"`
	Digest          string            `json:"digest"`
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
	for _, quantity := range []string{request.CPURequest, request.MemoryRequest, request.CPULimit, request.MemoryLimit} {
		if !quantityPattern.MatchString(quantity) {
			return adapterError(ErrInvalidRequest, 400, "resource quantity is invalid", nil)
		}
	}
	if request.ExpiresAt.IsZero() || !request.ExpiresAt.After(time.Now().Add(-time.Minute)) {
		return adapterError(ErrInvalidRequest, 400, "expiry is invalid", nil)
	}
	if len(request.Artifacts) > 32 {
		return adapterError(ErrInvalidRequest, 400, "artifact export limit exceeded", nil)
	}
	seen := map[string]bool{}
	for _, artifact := range request.Artifacts {
		if !dnsLabelPattern.MatchString(artifact.Name) || seen[artifact.Name] || !strings.HasPrefix(artifact.Path, "/workspace/artifacts/") || path.Clean(artifact.Path) != artifact.Path || strings.Contains(artifact.Path, "..") || !mediaPattern.MatchString(artifact.MediaType) {
			return adapterError(ErrInvalidRequest, 400, "artifact export is invalid", nil)
		}
		seen[artifact.Name] = true
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

func validateRuntime(request CreateRequest, runtimes map[string]RuntimeCapability, hardened bool) error {
	capability, exists := runtimes[request.RuntimeClassName]
	if request.RuntimeClassName == "" || !exists || capability.Name != request.RuntimeClassName || capability.Handler == "" || !capability.Qualified || (hardened && !capability.Hardened) {
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
	if receipt.SchemaVersion != ReceiptSchema || !requestPattern.MatchString(receipt.RequestID) || receipt.ReceiptID != receipt.RequestID+":"+string(receipt.Operation) || !dnsLabelPattern.MatchString(receipt.Name) || receipt.Namespace != Namespace || receipt.UID == "" || receipt.ResourceVersion == "" || !dnsLabelPattern.MatchString(receipt.WorkspaceID) || !dnsLabelPattern.MatchString(receipt.OwnerID) || receipt.QueueName != QueueName || !digestPattern.MatchString(receipt.Digest) {
		return fmt.Errorf("sandbox adapter receipt identity is invalid")
	}
	if receipt.Operation != OperationCreate && receipt.Operation != OperationDelete && receipt.Operation != OperationFinalize {
		return fmt.Errorf("sandbox adapter receipt operation is invalid")
	}
	if receipt.Operation == OperationDelete && receipt.State != StateStopping || receipt.Operation == OperationFinalize && receipt.State != StateDeleted || receipt.Operation == OperationCreate && (receipt.State == StateStopping || receipt.State == StateDeleted) {
		return fmt.Errorf("sandbox adapter receipt state is incoherent")
	}
	if _, err := time.Parse(time.RFC3339Nano, receipt.ObservedAt); err != nil {
		return fmt.Errorf("sandbox adapter receipt timestamp is invalid")
	}
	seenArtifacts := map[string]bool{}
	for _, artifact := range receipt.Artifacts {
		if artifact.SchemaVersion != ArtifactSchema || !dnsLabelPattern.MatchString(artifact.Name) || seenArtifacts[artifact.Name] || artifact.ObjectKey == "" || !digestPattern.MatchString(artifact.SHA256) || artifact.Size < 0 {
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

func NewReceipt(requestID string, operation Operation, sandbox SandboxRecord, artifacts []ArtifactReceipt, now time.Time) (OperationReceipt, error) {
	receipt := OperationReceipt{
		SchemaVersion: ReceiptSchema, ReceiptID: requestID + ":" + string(operation), RequestID: requestID,
		Operation: operation, Namespace: sandbox.Namespace, Name: sandbox.Name, UID: sandbox.UID,
		ResourceVersion: sandbox.ResourceVersion, WorkspaceID: sandbox.WorkspaceID, OwnerID: sandbox.OwnerID,
		QueueName: sandbox.QueueName, RuntimeClass: sandbox.RuntimeClassName, State: sandbox.State,
		Artifacts: append([]ArtifactReceipt(nil), artifacts...), ObservedAt: now.UTC().Format(time.RFC3339Nano),
	}
	sort.Slice(receipt.Artifacts, func(i, j int) bool { return receipt.Artifacts[i].Name < receipt.Artifacts[j].Name })
	digest, err := receiptDigest(receipt)
	if err != nil {
		return receipt, err
	}
	receipt.Digest = digest
	return receipt, nil
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
	return &AdapterError{Code: code, Status: status, SafeDetail: detail, Cause: cause}
}
