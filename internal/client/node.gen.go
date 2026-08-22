// Code generated from the Blazn node contracts; DO NOT EDIT.
// OpenAPI SHA256: 075126546f4277f5b3def6381746c9bbc6b222c9408cf17e03950d5075b60571
// NodeInstallPlan SHA256: 111984c682128e09a2caba46d405feb848c34e65ded478dbc49d9e74a677341e
// NodeInstallReceipt SHA256: cdfd07ec5c7fde1aa4501e006cdf8ddb060e7af33ab329af89de247d1c29a1e4
// NodeOperationReceipt SHA256: 95445951f5fb917e80668e45e0a82ebbed24735b575a16e8fdad56824214c79b

package client

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/gowebpki/jcs"
)

const (
	NodeSchemaVersion        = "nodes/v1alpha1"
	NodeEnrollmentTokenKeyID = "node-enrollment/v1"
	NodeJoinCredentialKeyID  = "node-join-credential/v1"
	nodeMaxJSONBytes         = 8 << 20
)

type NodePlatform string
type NodeArchitecture string
type NodeEnrollmentMode string
type NodeLifecycleState string
type NodeTrustState string
type NodeOperationType string
type NodeOperationStatus string

const (
	NodePlatformLinux NodePlatform       = "linux"
	NodePlatformMacOS NodePlatform       = "macos"
	NodeArchAMD64     NodeArchitecture   = "amd64"
	NodeArchARM64     NodeArchitecture   = "arm64"
	NodeModeFresh     NodeEnrollmentMode = "fresh"
	NodeModeAdopt     NodeEnrollmentMode = "adopt"
)

type CreateNodeEnrollmentRequest struct {
	Name         string             `json:"name"`
	Mode         NodeEnrollmentMode `json:"mode"`
	Platform     NodePlatform       `json:"platform"`
	Architecture NodeArchitecture   `json:"architecture,omitempty"`
}

type NodeEnrollmentSecret struct {
	ID         string `json:"id"`
	Token      string `json:"token"`
	TokenKeyID string `json:"tokenKeyId"`
	ExpiresAt  string `json:"expiresAt"`
	Replayed   bool   `json:"replayed"`
}

type ExchangeNodeEnrollmentRequest struct {
	Token              string             `json:"token"`
	MachineFingerprint string             `json:"machineFingerprint"`
	NodePublicKey      string             `json:"nodePublicKey"`
	Platform           NodePlatform       `json:"platform"`
	Architecture       NodeArchitecture   `json:"architecture"`
	KubernetesBinding  *KubernetesBinding `json:"kubernetesBinding,omitempty"`
}

type Node struct {
	ID                string             `json:"id"`
	WorkspaceID       string             `json:"workspaceId"`
	Name              string             `json:"name"`
	Kind              string             `json:"kind"`
	Platform          NodePlatform       `json:"platform"`
	Architecture      NodeArchitecture   `json:"architecture"`
	LifecycleState    NodeLifecycleState `json:"lifecycleState"`
	TrustState        NodeTrustState     `json:"trustState"`
	AgentEligible     bool               `json:"agentEligible"`
	Version           int64              `json:"version"`
	CapabilityVersion *int64             `json:"capabilityVersion"`
	Identity          *NodeIdentity      `json:"identity"`
	KubernetesBinding *KubernetesBinding `json:"kubernetesBinding,omitempty"`
	CreatedAt         string             `json:"createdAt"`
	UpdatedAt         string             `json:"updatedAt"`
}

type NodeIdentity struct {
	Generation           int64  `json:"generation"`
	PublicKeyFingerprint string `json:"publicKeyFingerprint"`
	Status               string `json:"status"`
	IssuedAt             string `json:"issuedAt"`
	ExpiresAt            string `json:"expiresAt"`
}

type NodeList struct {
	Items []Node `json:"items"`
}

type KubernetesBinding struct {
	ClusterID       string `json:"clusterId"`
	NodeName        string `json:"nodeName"`
	NodeUID         string `json:"nodeUid"`
	ResourceVersion string `json:"resourceVersion"`
}

type CreateNodeOperationRequest struct {
	Type            NodeOperationType `json:"type"`
	ExpectedVersion int64             `json:"expectedVersion"`
	Parameters      json.RawMessage   `json:"parameters"`
}

type NodeLabelParameters struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type KubernetesMutationParameters struct {
	ClusterID               string `json:"clusterId"`
	ExpectedNodeUID         string `json:"expectedNodeUid"`
	ExpectedResourceVersion string `json:"expectedResourceVersion"`
}

type NodeDrainParameters struct {
	KubernetesMutationParameters
	WorkspaceID     string `json:"workspaceId"`
	DeadlineSeconds int64  `json:"deadlineSeconds"`
}

type NodeRemoveParameters struct {
	KubernetesMutationParameters
	Confirm          bool `json:"confirm"`
	PreserveHostData bool `json:"preserveHostData"`
}

type NodeUpdateParameters struct {
	TargetVersion string `json:"targetVersion"`
}

type NodeOperation struct {
	ID                  string                `json:"id"`
	NodeID              string                `json:"nodeId"`
	Type                NodeOperationType     `json:"type"`
	Status              NodeOperationStatus   `json:"status"`
	ExpectedNodeVersion int64                 `json:"expectedNodeVersion"`
	Result              map[string]any        `json:"result"`
	Error               *ErrorBody            `json:"error"`
	Receipt             *NodeOperationReceipt `json:"receipt"`
	CreatedAt           string                `json:"createdAt"`
}

type NodeHeartbeat struct {
	NodeID             string         `json:"nodeId"`
	IdentityGeneration int64          `json:"identityGeneration"`
	BootID             string         `json:"bootId"`
	Sequence           int64          `json:"sequence"`
	SentAt             string         `json:"sentAt"`
	CapabilityDigest   string         `json:"capabilityDigest"`
	Capability         NodeCapability `json:"capability"`
}

type NodeCapability struct {
	Version         int64                  `json:"version"`
	Host            NodeHostCapacity       `json:"host"`
	Worker          NodeWorkerCapacity     `json:"worker"`
	SandboxBackends []string               `json:"sandboxBackends"`
	RuntimeClasses  []string               `json:"runtimeClasses"`
	LocalModels     []LocalModelCapability `json:"localModels"`
}

type NodeHostCapacity struct {
	Platform     NodePlatform         `json:"platform"`
	Architecture NodeArchitecture     `json:"architecture"`
	CPUMillis    int64                `json:"cpuMillis"`
	MemoryBytes  int64                `json:"memoryBytes"`
	DiskBytes    int64                `json:"diskBytes"`
	Accelerators []NodeAccelerator    `json:"accelerators"`
	Health       NodeCapabilityHealth `json:"health"`
}

type NodeWorkerCapacity struct {
	Platform               NodePlatform         `json:"platform"`
	Architecture           NodeArchitecture     `json:"architecture"`
	AllocatableCPUMillis   int64                `json:"allocatableCpuMillis"`
	AllocatableMemoryBytes int64                `json:"allocatableMemoryBytes"`
	AllocatableDiskBytes   int64                `json:"allocatableDiskBytes"`
	Labels                 map[string]string    `json:"labels"`
	Limits                 NodeCapabilityLimits `json:"limits"`
	Health                 NodeCapabilityHealth `json:"health"`
	KubernetesBinding      KubernetesBinding    `json:"kubernetesBinding"`
}

type NodeAccelerator struct {
	Kind  string `json:"kind"`
	Count int64  `json:"count"`
}

type NodeCapabilityLimits struct {
	MaxConcurrentSandboxes int64 `json:"maxConcurrentSandboxes"`
	MaxConcurrentAgents    int64 `json:"maxConcurrentAgents"`
}

type NodeCapabilityHealth struct {
	Status      string   `json:"status"`
	ReasonCodes []string `json:"reasonCodes"`
}

type LocalModelCapability struct {
	RouteID          string   `json:"routeId"`
	DisplayName      string   `json:"displayName"`
	Model            string   `json:"model"`
	Protocol         string   `json:"protocol"`
	EndpointClass    string   `json:"endpointClass"`
	Capabilities     []string `json:"capabilities"`
	DataBoundary     string   `json:"dataBoundary"`
	Healthy          bool     `json:"healthy"`
	MaxConcurrency   int64    `json:"maxConcurrency"`
	MaxContextTokens int64    `json:"maxContextTokens"`
	MaxOutputTokens  int64    `json:"maxOutputTokens"`
}

type JoinCredentialRequest struct {
	EnrollmentID             string `json:"enrollmentId"`
	PlanID                   string `json:"planId"`
	PlanDigest               string `json:"planDigest"`
	NodeID                   string `json:"nodeId"`
	MachineFingerprint       string `json:"machineFingerprint"`
	NodePublicKeyFingerprint string `json:"nodePublicKeyFingerprint"`
}

type JoinCredential struct {
	IssuanceID string `json:"issuanceId"`
	Credential string `json:"credential"`
	ExpiresAt  string `json:"expiresAt"`
	ClusterID  string `json:"clusterId"`
	WorkerOnly bool   `json:"workerOnly"`
	Replayed   bool   `json:"replayed"`
}

type ConsumeJoinCredentialRequest struct {
	NodeID          string `json:"nodeId"`
	EnrollmentID    string `json:"enrollmentId"`
	PlanID          string `json:"planId"`
	JoinedNodeUID   string `json:"joinedNodeUid"`
	JoinedNodeName  string `json:"joinedNodeName"`
	ResourceVersion string `json:"resourceVersion"`
	ClusterID       string `json:"clusterId"`
}

type NodeInstallPlan struct {
	SchemaVersion   string                 `json:"schemaVersion"`
	PlanID          string                 `json:"planId"`
	NodeID          string                 `json:"nodeId"`
	EnrollmentID    string                 `json:"enrollmentId"`
	WorkspaceID     string                 `json:"workspaceId"`
	IdempotencyKey  string                 `json:"idempotencyKey"`
	ApprovedBy      string                 `json:"approvedBy"`
	ApprovedAt      string                 `json:"approvedAt"`
	Hostname        string                 `json:"hostname"`
	Mode            NodeEnrollmentMode     `json:"mode"`
	InstallProfile  string                 `json:"installProfile"`
	Cluster         NodeInstallCluster     `json:"cluster"`
	Target          NodeInstallTarget      `json:"target"`
	RegistryTrust   []NodeRegistryTrust    `json:"registryTrust"`
	Components      []NodeInstallComponent `json:"components"`
	NodeService     NodeInstallService     `json:"nodeService"`
	Labels          map[string]string      `json:"labels"`
	Taints          []NodeTaint            `json:"taints"`
	ResourceBounds  NodeResourceBounds     `json:"resourceBounds"`
	Mutations       []NodeInstallMutation  `json:"mutations"`
	ValidationTests []string               `json:"validationTests"`
	Rollback        NodeInstallRollback    `json:"rollback"`
	IssuedAt        string                 `json:"issuedAt"`
	ExpiresAt       string                 `json:"expiresAt"`
	SigningKeyID    string                 `json:"signingKeyId"`
	Digest          string                 `json:"digest"`
	Signature       string                 `json:"signature"`
}

type NodeInstallCluster struct {
	ID                     string   `json:"id"`
	WorkerOnly             bool     `json:"workerOnly"`
	APIServer              string   `json:"apiServer"`
	KubernetesVersion      string   `json:"kubernetesVersion"`
	JoinCredentialEndpoint string   `json:"joinCredentialEndpoint"`
	BootstrapTaint         string   `json:"bootstrapTaint"`
	ExpectedCAFingerprint  string   `json:"expectedCaFingerprint"`
	RegistryEndpoints      []string `json:"registryEndpoints"`
}

type NodeInstallTarget struct {
	Platform                 NodePlatform     `json:"platform"`
	Architecture             NodeArchitecture `json:"architecture"`
	MachineFingerprint       string           `json:"machineFingerprint"`
	NodePublicKeyFingerprint string           `json:"nodePublicKeyFingerprint"`
	MinCPU                   int64            `json:"minCpu"`
	MinMemoryBytes           int64            `json:"minMemoryBytes"`
	MinDiskBytes             int64            `json:"minDiskBytes"`
}

type NodeRegistryTrust struct {
	Hostname       string `json:"hostname"`
	CABundleSHA256 string `json:"caBundleSha256"`
}

type NodeInstallComponent struct {
	Name             string `json:"name"`
	ArtifactType     string `json:"artifactType"`
	Version          string `json:"version"`
	Publisher        string `json:"publisher"`
	SourceHost       string `json:"sourceHost"`
	Source           string `json:"source"`
	RepositoryOrigin string `json:"repositoryOrigin,omitempty"`
	RegistryHost     string `json:"registryHost,omitempty"`
	OCIReference     string `json:"ociReference,omitempty"`
	SHA256           string `json:"sha256"`
	Ownership        string `json:"ownership"`
}

type NodeInstallService struct {
	Manager          string `json:"manager"`
	UnitName         string `json:"unitName"`
	BinaryPath       string `json:"binaryPath"`
	RunAsUser        string `json:"runAsUser"`
	RunAsGroup       string `json:"runAsGroup"`
	DefinitionSHA256 string `json:"definitionSha256"`
}

type NodeTaint struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Effect string `json:"effect"`
}

type NodeResourceBounds struct {
	ReservedCPUMillis   int64 `json:"reservedCpuMillis"`
	ReservedMemoryBytes int64 `json:"reservedMemoryBytes"`
	MaxPods             int64 `json:"maxPods"`
	MaxConcurrentAgents int64 `json:"maxConcurrentAgents"`
}

type NodeInstallMutation struct {
	Ordinal       int64          `json:"ordinal"`
	Kind          string         `json:"kind"`
	Action        string         `json:"action"`
	Target        string         `json:"target"`
	Desired       map[string]any `json:"desired"`
	DesiredDigest string         `json:"desiredDigest"`
	Mode          int64          `json:"mode"`
	UID           int64          `json:"uid"`
	GID           int64          `json:"gid"`
	Rollback      string         `json:"rollback"`
}

type NodeInstallRollback struct {
	PreserveUserData     bool   `json:"preserveUserData"`
	PreserveControlPlane bool   `json:"preserveControlPlane"`
	AmbiguousOwnership   string `json:"ambiguousOwnership"`
	BackupRootClass      string `json:"backupRootClass"`
	BackupRoot           string `json:"backupRoot"`
}

type NodeInstallReceipt struct {
	SchemaVersion          string                `json:"schemaVersion"`
	ReceiptID              string                `json:"receiptId"`
	PlanID                 string                `json:"planId"`
	PlanDigest             string                `json:"planDigest"`
	NodeID                 string                `json:"nodeId"`
	Generation             int64                 `json:"generation"`
	NodeIdentityGeneration int64                 `json:"nodeIdentityGeneration"`
	SignerKind             string                `json:"signerKind"`
	SignerFingerprint      string                `json:"signerFingerprint"`
	State                  string                `json:"state"`
	CurrentStage           string                `json:"currentStage"`
	Owner                  NodeReceiptOwner      `json:"owner"`
	Binary                 NodeReceiptBinary     `json:"binary"`
	Service                NodeReceiptService    `json:"service"`
	Mutations              []NodeReceiptMutation `json:"mutations"`
	Residues               []NodeReceiptResidue  `json:"residues"`
	CreatedAt              string                `json:"createdAt"`
	UpdatedAt              string                `json:"updatedAt"`
	SigningKeyID           string                `json:"signingKeyId"`
	Digest                 string                `json:"digest"`
	Signature              string                `json:"signature"`
}

type NodeReceiptOwner struct {
	UID                  int64  `json:"uid"`
	PID                  int64  `json:"pid"`
	ProcessStartIdentity string `json:"processStartIdentity"`
	Nonce                string `json:"nonce"`
}

type NodeReceiptBinary struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
}

type NodeReceiptService struct {
	Manager          string `json:"manager"`
	Name             string `json:"name"`
	DefinitionDigest string `json:"definitionDigest"`
	PriorEnabled     bool   `json:"priorEnabled"`
	PriorActive      bool   `json:"priorActive"`
}

type NodeReceiptMutation struct {
	Ordinal          int64                `json:"ordinal"`
	Kind             string               `json:"kind"`
	Target           string               `json:"target"`
	PriorState       string               `json:"priorState"`
	RollbackMaterial NodeRollbackMaterial `json:"rollbackMaterial"`
	DesiredDigest    string               `json:"desiredDigest"`
	Status           string               `json:"status"`
}

type NodeReceiptResidue struct {
	Target      string `json:"target"`
	ReasonCode  string `json:"reasonCode"`
	SafeMessage string `json:"safeMessage"`
}

type NodeRollbackMaterial struct {
	Kind    string `json:"kind"`
	Locator string `json:"locator,omitempty"`
	Digest  string `json:"digest,omitempty"`
	Mode    *int64 `json:"mode,omitempty"`
	UID     *int64 `json:"uid,omitempty"`
	GID     *int64 `json:"gid,omitempty"`
}

type NodeOperationReceipt struct {
	SchemaVersion       string               `json:"schemaVersion"`
	ReceiptID           string               `json:"receiptId"`
	OperationID         string               `json:"operationId"`
	NodeID              string               `json:"nodeId"`
	WorkspaceID         string               `json:"workspaceId"`
	OperationType       NodeOperationType    `json:"operationType"`
	ExpectedNodeVersion int64                `json:"expectedNodeVersion"`
	StartedAt           string               `json:"startedAt"`
	CompletedAt         string               `json:"completedAt"`
	Outcome             string               `json:"outcome"`
	KubernetesBefore    *KubernetesBinding   `json:"kubernetesBefore"`
	KubernetesAfter     *KubernetesBinding   `json:"kubernetesAfter"`
	Actions             []NodeReceiptAction  `json:"actions"`
	Residues            []NodeReceiptResidue `json:"residues"`
	SignerKind          string               `json:"signerKind"`
	IdentityGeneration  *int64               `json:"identityGeneration"`
	SignerFingerprint   string               `json:"signerFingerprint"`
	SigningKeyID        string               `json:"signingKeyId"`
	Digest              string               `json:"digest"`
	Signature           string               `json:"signature"`
}

type NodeReceiptAction struct {
	Ordinal int64  `json:"ordinal"`
	Kind    string `json:"kind"`
	Target  string `json:"target"`
	Outcome string `json:"outcome"`
}

type NodeSigningKeyring map[string]ed25519.PublicKey

type NodeTrustedInstallProfile struct {
	ID                       string
	AllowedClusterOrigins    []string
	AllowedDownloadOrigins   []string
	AllowedRegistryOrigins   []string
	AllowedMutationRoots     []string
	VerifyNoSymlinkTraversal func(string) error
}

type NodeTrustedSigner struct {
	Kind        string
	Status      string
	KeyID       string
	Generation  int64
	Fingerprint string
	PublicKey   ed25519.PublicKey
}

type NodeInstallPlanTrust struct {
	Now                time.Time
	Keyring            NodeSigningKeyring
	WorkspaceID        string
	EnrollmentID       string
	NodeID             string
	Hostname           string
	MachineFingerprint string
	NodePublicKey      ed25519.PublicKey
	Platform           NodePlatform
	Architecture       NodeArchitecture
	IdempotencyKey     string
	Profile            NodeTrustedInstallProfile
}

type NodeInstallReceiptTrust struct {
	PlanID                   string
	PlanDigest               string
	NodeID                   string
	Signer                   NodeTrustedSigner
	BackupRoot               string
	VerifyNoSymlinkTraversal func(string) error
}

type NodeOperationReceiptTrust struct {
	OperationID         string
	NodeID              string
	WorkspaceID         string
	OperationType       NodeOperationType
	ExpectedNodeVersion int64
	NodeIdentitySigner  *NodeTrustedSigner
	ControlPlaneSigner  *NodeTrustedSigner
}

type NodeJoinCredentialContext struct {
	WorkspaceID    string
	EnrollmentID   string
	PlanID         string
	NodeID         string
	IssuanceID     string
	IdempotencyKey string
	RequestDigest  string
}

type NodeEventStream struct {
	Body        io.ReadCloser
	ContentType string
}

var (
	nodeUUIDPattern              = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	nodeHashPattern              = regexp.MustCompile(`^[0-9a-f]{64}$`)
	nodeDigestPattern            = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	nodeBase64URLPattern         = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	nodeIdempotencyPattern       = regexp.MustCompile(`^[A-Za-z0-9._:-]+$`)
	nodeLabelPattern             = regexp.MustCompile(`^blazn\.dev/[a-z0-9][a-z0-9._-]{0,62}$`)
	nodeVersionPattern           = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(?:[-+][A-Za-z0-9.-]+)?$`)
	nodeKubernetesVersionPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)
	nodeReasonCodePattern        = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)
	nodeHostnamePattern          = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$`)
	nodeReceiptLocatorPattern    = regexp.MustCompile(`^receipt-backup://[A-Za-z0-9_-]{1,128}$`)
	nodePackageTargetPattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]{0,127}$`)
	nodeSystemdTargetPattern     = regexp.MustCompile(`^/etc/systemd/system/[A-Za-z0-9_.@-]+\.service$`)
	nodeSystemdUnitPattern       = regexp.MustCompile(`^[A-Za-z0-9_.@-]+\.service$`)
	nodeLaunchdTargetPattern     = regexp.MustCompile(`^/Library/LaunchDaemons/[A-Za-z0-9_.-]+\.plist$`)
	nodeImageTargetPattern       = regexp.MustCompile(`^[A-Za-z0-9._:/-]+@sha256:[0-9a-f]{64}$`)
	nodeFirewallTargetPattern    = regexp.MustCompile(`^blazn:[a-z0-9_-]{1,64}$`)
	nodeLinuxBackupRootPattern   = regexp.MustCompile(`^/var/lib/blazn/install-backups/[A-Za-z0-9_-]{1,128}$`)
	nodeMacOSBackupRootPattern   = regexp.MustCompile(`^/Library/Application Support/Blazn/install-backups/[A-Za-z0-9_-]{1,128}$`)
)

func ValidateCreateNodeOperationRequest(request CreateNodeOperationRequest) error {
	if request.ExpectedVersion < 1 || len(request.Parameters) == 0 || len(request.Parameters) > 4096 {
		return fmt.Errorf("node operation request is invalid")
	}
	switch request.Type {
	case "pause", "resume", "rotate_identity", "repair":
		var parameters struct{}
		if err := decodeClosedNodeObject(request.Parameters, &parameters); err != nil {
			return fmt.Errorf("%s parameters: %w", request.Type, err)
		}
	case "label":
		var parameters NodeLabelParameters
		if err := decodeClosedNodeObject(request.Parameters, &parameters); err != nil || !nodeLabelPattern.MatchString(parameters.Key) || len(parameters.Value) > 128 {
			return fmt.Errorf("label parameters are invalid")
		}
	case "cordon", "uncordon":
		var parameters KubernetesMutationParameters
		if err := decodeClosedNodeObject(request.Parameters, &parameters); err != nil || !validKubernetesMutation(parameters) {
			return fmt.Errorf("%s parameters are invalid", request.Type)
		}
	case "drain":
		var parameters NodeDrainParameters
		if err := decodeClosedNodeObject(request.Parameters, &parameters); err != nil || !validKubernetesMutation(parameters.KubernetesMutationParameters) || !nodeUUIDPattern.MatchString(parameters.WorkspaceID) || parameters.DeadlineSeconds < 60 || parameters.DeadlineSeconds > 3600 {
			return fmt.Errorf("drain parameters are invalid")
		}
	case "remove":
		var parameters NodeRemoveParameters
		if err := decodeClosedNodeObject(request.Parameters, &parameters); err != nil || !validKubernetesMutation(parameters.KubernetesMutationParameters) || !parameters.Confirm || !parameters.PreserveHostData {
			return fmt.Errorf("remove parameters are invalid")
		}
	case "update":
		var parameters NodeUpdateParameters
		if err := decodeClosedNodeObject(request.Parameters, &parameters); err != nil || len(parameters.TargetVersion) > 128 || !nodeVersionPattern.MatchString(parameters.TargetVersion) {
			return fmt.Errorf("update parameters are invalid")
		}
	default:
		return fmt.Errorf("node operation type is invalid")
	}
	return nil
}

func validKubernetesMutation(parameters KubernetesMutationParameters) bool {
	return len(parameters.ClusterID) >= 1 && len(parameters.ClusterID) <= 128 && len(parameters.ExpectedNodeUID) >= 1 && len(parameters.ExpectedNodeUID) <= 128 && len(parameters.ExpectedResourceVersion) >= 1 && len(parameters.ExpectedResourceVersion) <= 128
}

func decodeClosedNodeObject(encoded []byte, output any) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &object); err != nil || object == nil {
		return fmt.Errorf("parameters must be a JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("parameters must contain exactly one JSON object")
	}
	return nil
}

func ValidateNodeCapability(capability NodeCapability) error {
	if capability.Version < 1 || !validPlatform(capability.Host.Platform) || !validArchitecture(capability.Host.Architecture) || capability.Host.CPUMillis < 1 || capability.Host.MemoryBytes < 1 || capability.Host.DiskBytes < 1 || capability.Host.Accelerators == nil || capability.Host.Health.ReasonCodes == nil || capability.Worker.Platform != NodePlatformLinux || !validArchitecture(capability.Worker.Architecture) || capability.Worker.AllocatableCPUMillis < 1 || capability.Worker.AllocatableMemoryBytes < 1 || capability.Worker.AllocatableDiskBytes < 1 || capability.Worker.Labels == nil || capability.Worker.Health.ReasonCodes == nil || capability.SandboxBackends == nil || capability.RuntimeClasses == nil || capability.LocalModels == nil {
		return fmt.Errorf("node capability resources are invalid")
	}
	if len(capability.Host.Accelerators) > 16 || len(capability.Worker.Labels) > 64 || capability.Worker.Limits.MaxConcurrentSandboxes < 0 || capability.Worker.Limits.MaxConcurrentSandboxes > 1024 || capability.Worker.Limits.MaxConcurrentAgents < 0 || capability.Worker.Limits.MaxConcurrentAgents > 1024 || !validKubernetesBinding(capability.Worker.KubernetesBinding) {
		return fmt.Errorf("node capability limits or worker binding are invalid")
	}
	for index, accelerator := range capability.Host.Accelerators {
		if len(accelerator.Kind) < 1 || len(accelerator.Kind) > 128 || accelerator.Count < 1 {
			return fmt.Errorf("accelerators[%d] is invalid", index)
		}
	}
	for key, value := range capability.Worker.Labels {
		if !nodeLabelPattern.MatchString(key) || len(value) > 128 {
			return fmt.Errorf("node capability label %q is invalid", key)
		}
	}
	if err := validateNodeHealth(capability.Host.Health); err != nil {
		return fmt.Errorf("host health: %w", err)
	}
	if err := validateNodeHealth(capability.Worker.Health); err != nil {
		return fmt.Errorf("worker health: %w", err)
	}
	for name, values := range map[string][]string{"sandboxBackends": capability.SandboxBackends, "runtimeClasses": capability.RuntimeClasses} {
		if duplicateNodeStrings(values) {
			return fmt.Errorf("%s contains duplicates", name)
		}
		for _, value := range values {
			if len(value) > 128 {
				return fmt.Errorf("%s value is too long", name)
			}
		}
	}
	if len(capability.LocalModels) > 32 {
		return fmt.Errorf("local model limit exceeded")
	}
	routeIDs := make(map[string]struct{}, len(capability.LocalModels))
	for index, model := range capability.LocalModels {
		if !nodeUUIDPattern.MatchString(model.RouteID) || len(model.DisplayName) < 1 || len(model.DisplayName) > 128 || len(model.Model) < 1 || len(model.Model) > 160 || !oneOf(model.Protocol, "openai-chat", "openai-responses") || !oneOf(model.EndpointClass, "loopback", "authenticated_node_tunnel") || len(model.Capabilities) < 1 || duplicateNodeStrings(model.Capabilities) || model.DataBoundary != "local" || model.MaxConcurrency < 1 || model.MaxContextTokens < 1 || model.MaxOutputTokens < 1 {
			return fmt.Errorf("localModels[%d] is invalid", index)
		}
		if _, exists := routeIDs[model.RouteID]; exists {
			return fmt.Errorf("localModels[%d] repeats routeId", index)
		}
		routeIDs[model.RouteID] = struct{}{}
		for _, value := range model.Capabilities {
			if !oneOf(value, "text", "tools", "structured_output", "streaming") {
				return fmt.Errorf("localModels[%d] capability is invalid", index)
			}
		}
	}
	return nil
}

func validateNodeHealth(health NodeCapabilityHealth) error {
	if !oneOf(health.Status, "healthy", "degraded", "unavailable") || health.ReasonCodes == nil || len(health.ReasonCodes) > 16 || duplicateNodeStrings(health.ReasonCodes) {
		return fmt.Errorf("node capability health is invalid")
	}
	for _, reason := range health.ReasonCodes {
		if !nodeReasonCodePattern.MatchString(reason) {
			return fmt.Errorf("node capability reason code is invalid")
		}
	}
	return nil
}

func validKubernetesBinding(binding KubernetesBinding) bool {
	return len(binding.ClusterID) >= 1 && len(binding.ClusterID) <= 128 && len(binding.NodeName) >= 1 && len(binding.NodeName) <= 253 && len(binding.NodeUID) >= 1 && len(binding.NodeUID) <= 128 && len(binding.ResourceVersion) >= 1 && len(binding.ResourceVersion) <= 128
}

func duplicateNodeStrings(values []string) bool {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			return true
		}
		seen[value] = struct{}{}
	}
	return false
}

func ValidateNodeInstallPlan(plan NodeInstallPlan) error {
	if plan.SchemaVersion != NodeSchemaVersion {
		return fmt.Errorf("schemaVersion must be %q", NodeSchemaVersion)
	}
	for name, value := range map[string]string{"planId": plan.PlanID, "nodeId": plan.NodeID, "enrollmentId": plan.EnrollmentID, "workspaceId": plan.WorkspaceID, "approvedBy": plan.ApprovedBy} {
		if !nodeUUIDPattern.MatchString(value) {
			return fmt.Errorf("%s must be a UUID", name)
		}
	}
	if !validNodeIdempotencyKey(plan.IdempotencyKey) || len(plan.Hostname) < 1 || len(plan.Hostname) > 253 || !nodeHostnamePattern.MatchString(plan.Hostname) || (plan.Mode != NodeModeFresh && plan.Mode != NodeModeAdopt) || !oneOf(plan.InstallProfile, "ubuntu-26.04-amd64-worker/v1", "existing-linux-worker-adopt/v1", "macos-lima-worker-adopt/v1") {
		return fmt.Errorf("plan approval or target identity is invalid")
	}
	if len(plan.Cluster.ID) < 1 || len(plan.Cluster.ID) > 128 || !plan.Cluster.WorkerOnly || !validHTTPSURL(plan.Cluster.APIServer) || !nodeKubernetesVersionPattern.MatchString(plan.Cluster.KubernetesVersion) || plan.Cluster.JoinCredentialEndpoint != "/v1/node-service/join-credentials" || plan.Cluster.BootstrapTaint != "blazn.dev/bootstrap=pending:NoSchedule" || !nodeDigestPattern.MatchString(plan.Cluster.ExpectedCAFingerprint) || plan.Cluster.RegistryEndpoints == nil || len(plan.Cluster.RegistryEndpoints) > 16 || duplicateNodeStrings(plan.Cluster.RegistryEndpoints) {
		return fmt.Errorf("cluster is invalid")
	}
	for _, endpoint := range plan.Cluster.RegistryEndpoints {
		if !validHTTPSURL(endpoint) {
			return fmt.Errorf("cluster registry endpoint is invalid")
		}
	}
	if !validPlatform(plan.Target.Platform) || !validArchitecture(plan.Target.Architecture) || !nodeHashPattern.MatchString(plan.Target.MachineFingerprint) || !nodeDigestPattern.MatchString(plan.Target.NodePublicKeyFingerprint) || plan.Target.MinCPU < 1 || plan.Target.MinMemoryBytes < 1073741824 || plan.Target.MinDiskBytes < 10737418240 {
		return fmt.Errorf("target is invalid")
	}
	if plan.RegistryTrust == nil || len(plan.RegistryTrust) > 16 || plan.Components == nil || plan.Labels == nil || plan.Taints == nil || plan.Mutations == nil || plan.ValidationTests == nil || len(plan.Components) > 64 || len(plan.Mutations) > 256 || len(plan.ValidationTests) < 1 || len(plan.ValidationTests) > 32 || duplicateNodeStrings(plan.ValidationTests) || (plan.Mode == NodeModeFresh && (len(plan.Components) == 0 || len(plan.Mutations) == 0)) {
		return fmt.Errorf("plan collection limit exceeded")
	}
	for index, trust := range plan.RegistryTrust {
		if len(trust.Hostname) < 1 || len(trust.Hostname) > 253 || !nodeHostnamePattern.MatchString(trust.Hostname) || !nodeHashPattern.MatchString(trust.CABundleSHA256) {
			return fmt.Errorf("registryTrust[%d] is invalid", index)
		}
	}
	for index, component := range plan.Components {
		parsed, err := url.Parse(component.Source)
		if len(component.Name) < 1 || len(component.Name) > 128 || !oneOf(component.ArtifactType, "package", "binary", "image", "certificate", "configuration") || len(component.Version) < 1 || len(component.Version) > 128 || len(component.Publisher) < 1 || len(component.Publisher) > 256 || !nodeHostnamePattern.MatchString(component.SourceHost) || err != nil || !validHTTPSURL(component.Source) || parsed.Hostname() != component.SourceHost || !nodeHashPattern.MatchString(component.SHA256) || (component.Ownership != "install" && component.Ownership != "adopt_exact") {
			return fmt.Errorf("components[%d] is invalid", index)
		}
		switch component.ArtifactType {
		case "package":
			origin, originErr := nodeURLOrigin(component.RepositoryOrigin)
			if originErr != nil || origin != component.RepositoryOrigin || component.RegistryHost != "" || component.OCIReference != "" {
				return fmt.Errorf("components[%d] package repository binding is invalid", index)
			}
		case "image":
			registryOrigin, registryHost, registryErr := nodeOCIRegistryOrigin(component.OCIReference)
			if !nodeHostnamePattern.MatchString(component.RegistryHost) || !nodeImageTargetPattern.MatchString(component.OCIReference) || component.RepositoryOrigin != "" || registryErr != nil || registryOrigin == "" || registryHost != component.RegistryHost || !strings.HasSuffix(component.OCIReference, "@sha256:"+component.SHA256) {
				return fmt.Errorf("components[%d] image registry binding is invalid", index)
			}
		default:
			if component.RepositoryOrigin != "" || component.RegistryHost != "" || component.OCIReference != "" {
				return fmt.Errorf("components[%d] has unrelated repository metadata", index)
			}
		}
	}
	componentNames := make(map[string]NodeInstallComponent, len(plan.Components))
	for _, component := range plan.Components {
		if _, exists := componentNames[component.Name]; exists {
			return fmt.Errorf("component name %q is duplicated", component.Name)
		}
		componentNames[component.Name] = component
	}
	if !oneOf(plan.NodeService.Manager, "systemd", "launchd") || len(plan.NodeService.UnitName) < 1 || len(plan.NodeService.UnitName) > 256 || !strings.HasPrefix(plan.NodeService.BinaryPath, "/") || len(plan.NodeService.RunAsUser) < 1 || len(plan.NodeService.RunAsUser) > 128 || len(plan.NodeService.RunAsGroup) < 1 || len(plan.NodeService.RunAsGroup) > 128 || !nodeHashPattern.MatchString(plan.NodeService.DefinitionSHA256) {
		return fmt.Errorf("node service is invalid")
	}
	if len(plan.Labels) > 64 || len(plan.Taints) > 16 {
		return fmt.Errorf("plan labels or taints exceed limits")
	}
	for key, value := range plan.Labels {
		if !nodeLabelPattern.MatchString(key) || len(value) > 128 {
			return fmt.Errorf("plan label %q is invalid", key)
		}
	}
	for index, taint := range plan.Taints {
		if !nodeLabelPattern.MatchString(taint.Key) || len(taint.Value) > 128 || !oneOf(taint.Effect, "NoSchedule", "PreferNoSchedule", "NoExecute") {
			return fmt.Errorf("taints[%d] is invalid", index)
		}
	}
	if plan.ResourceBounds.ReservedCPUMillis < 0 || plan.ResourceBounds.ReservedMemoryBytes < 0 || plan.ResourceBounds.MaxPods < 1 || plan.ResourceBounds.MaxPods > 4096 || plan.ResourceBounds.MaxConcurrentAgents < 1 || plan.ResourceBounds.MaxConcurrentAgents > 1024 {
		return fmt.Errorf("resource bounds are invalid")
	}
	if err := validatePlanMutations(plan.Mutations); err != nil {
		return err
	}
	for index, mutation := range plan.Mutations {
		if componentName := nodeMutationSourceComponent(mutation); componentName != "" {
			component, exists := componentNames[componentName]
			if !exists {
				return fmt.Errorf("mutations[%d] references unknown component %q", index, componentName)
			}
			if err := validateNodeMutationComponentBinding(plan, mutation, component); err != nil {
				return fmt.Errorf("mutations[%d]: %w", index, err)
			}
		}
	}
	if err := validateNodeProfileSemantics(plan); err != nil {
		return err
	}
	for _, validation := range plan.ValidationTests {
		if !oneOf(validation, "binary_digest", "service_active", "node_identity", "cluster_ca", "worker_only", "node_uid_binding", "bootstrap_taint", "capability_heartbeat", "agent_eligibility") {
			return fmt.Errorf("validation test is invalid")
		}
	}
	if !plan.Rollback.PreserveUserData || !plan.Rollback.PreserveControlPlane || plan.Rollback.AmbiguousOwnership != "recovery_required" || !validNodeBackupRoot(plan.Rollback.BackupRootClass, plan.Rollback.BackupRoot) {
		return fmt.Errorf("rollback safety policy is invalid")
	}
	approved, err := time.Parse(time.RFC3339, plan.ApprovedAt)
	if err != nil {
		return fmt.Errorf("approvedAt is invalid")
	}
	issued, err := time.Parse(time.RFC3339, plan.IssuedAt)
	if err != nil || approved.After(issued) {
		return fmt.Errorf("approvedAt must not be after issuedAt")
	}
	if err := validateTimeWindow(plan.IssuedAt, plan.ExpiresAt); err != nil {
		return fmt.Errorf("plan time window: %w", err)
	}
	if len(plan.SigningKeyID) < 1 || len(plan.SigningKeyID) > 128 || !nodeDigestPattern.MatchString(plan.Digest) || len(plan.Signature) != 86 || !nodeBase64URLPattern.MatchString(plan.Signature) {
		return fmt.Errorf("plan signature metadata is invalid")
	}
	return nil
}

func validNodeBackupRoot(class, root string) bool {
	switch class {
	case "linux_var_lib":
		return nodeLinuxBackupRootPattern.MatchString(root)
	case "macos_library_application_support":
		return nodeMacOSBackupRootPattern.MatchString(root)
	default:
		return false
	}
}

func ValidateNodeInstallReceipt(receipt NodeInstallReceipt) error {
	if receipt.SchemaVersion != NodeSchemaVersion || !nodeUUIDPattern.MatchString(receipt.ReceiptID) || !nodeUUIDPattern.MatchString(receipt.PlanID) || !nodeUUIDPattern.MatchString(receipt.NodeID) || !nodeDigestPattern.MatchString(receipt.PlanDigest) || receipt.Generation < 1 || receipt.NodeIdentityGeneration < 1 || receipt.SignerKind != "node_identity" || !nodeDigestPattern.MatchString(receipt.SignerFingerprint) {
		return fmt.Errorf("receipt identity is invalid")
	}
	if !oneOf(receipt.State, "preparing", "installing", "joining", "verifying", "active", "rolling_back", "recovery_required", "removed") {
		return fmt.Errorf("receipt state is invalid")
	}
	if !oneOf(receipt.CurrentStage, "authenticate", "preflight", "plan", "acquire", "install", "configure", "pre_register", "join", "service", "verify", "activate", "complete") {
		return fmt.Errorf("receipt current stage is invalid")
	}
	if receipt.Owner.UID < 0 || receipt.Owner.PID < 1 || receipt.Owner.ProcessStartIdentity == "" || len(receipt.Owner.Nonce) < 32 || len(receipt.Owner.Nonce) > 128 || !nodeBase64URLPattern.MatchString(receipt.Owner.Nonce) {
		return fmt.Errorf("receipt owner is invalid")
	}
	if !strings.HasPrefix(receipt.Binary.Path, "/") || !nodeDigestPattern.MatchString(receipt.Binary.Digest) || !oneOf(receipt.Service.Manager, "systemd", "launchd") || len(receipt.Service.Name) < 1 || len(receipt.Service.Name) > 256 || !nodeDigestPattern.MatchString(receipt.Service.DefinitionDigest) {
		return fmt.Errorf("receipt binary or service is invalid")
	}
	if receipt.Mutations == nil || receipt.Residues == nil || len(receipt.Mutations) > 256 || len(receipt.Residues) > 256 {
		return fmt.Errorf("receipt mutation limit exceeded")
	}
	seen := make(map[int64]struct{}, len(receipt.Mutations))
	for index, mutation := range receipt.Mutations {
		if mutation.Ordinal < 1 || !oneOf(mutation.Kind, nodeMutationKinds()...) || len(mutation.Target) < 1 || len(mutation.Target) > 1024 || !oneOf(mutation.PriorState, "absent", "owned", "preexisting_exact") || !nodeDigestPattern.MatchString(mutation.DesiredDigest) || !oneOf(mutation.Status, "pending", "applied", "restored", "removed", "residue") || !validRollbackMaterial(mutation.PriorState, mutation.RollbackMaterial) {
			return fmt.Errorf("mutations[%d] is invalid", index)
		}
		if _, exists := seen[mutation.Ordinal]; exists {
			return fmt.Errorf("mutations[%d] repeats ordinal %d", index, mutation.Ordinal)
		}
		seen[mutation.Ordinal] = struct{}{}
	}
	for index, residue := range receipt.Residues {
		if len(residue.Target) < 1 || len(residue.Target) > 1024 || !nodeReasonCodePattern.MatchString(residue.ReasonCode) || len(residue.SafeMessage) < 1 || len(residue.SafeMessage) > 512 {
			return fmt.Errorf("residues[%d] is invalid", index)
		}
	}
	switch receipt.State {
	case "active":
		if receipt.CurrentStage != "complete" || len(receipt.Residues) != 0 || !allNodeMutationStatuses(receipt.Mutations, "applied") {
			return fmt.Errorf("active receipt state is incoherent")
		}
	case "removed":
		if receipt.CurrentStage != "complete" || len(receipt.Residues) != 0 || !allNodeMutationStatuses(receipt.Mutations, "restored", "removed") {
			return fmt.Errorf("removed receipt state is incoherent")
		}
	case "recovery_required":
		if len(receipt.Residues) == 0 {
			return fmt.Errorf("recovery-required receipt must contain residues")
		}
	}
	created, err := time.Parse(time.RFC3339, receipt.CreatedAt)
	if err != nil {
		return fmt.Errorf("createdAt is invalid")
	}
	updated, err := time.Parse(time.RFC3339, receipt.UpdatedAt)
	if err != nil || updated.Before(created) {
		return fmt.Errorf("updatedAt is invalid")
	}
	if len(receipt.SigningKeyID) < 1 || len(receipt.SigningKeyID) > 128 || !nodeDigestPattern.MatchString(receipt.Digest) || len(receipt.Signature) != 86 || !nodeBase64URLPattern.MatchString(receipt.Signature) {
		return fmt.Errorf("receipt signature metadata is invalid")
	}
	return nil
}

func allNodeMutationStatuses(mutations []NodeReceiptMutation, allowed ...string) bool {
	for _, mutation := range mutations {
		if !oneOf(mutation.Status, allowed...) {
			return false
		}
	}
	return true
}

func validRollbackMaterial(priorState string, material NodeRollbackMaterial) bool {
	if priorState == "absent" {
		return material.Kind == "absent" && material.Locator == "" && material.Digest == "" && material.Mode == nil && material.UID == nil && material.GID == nil
	}
	return oneOf(material.Kind, "file_backup", "package_snapshot", "unit_snapshot", "firewall_snapshot", "metadata_snapshot") && nodeReceiptLocatorPattern.MatchString(material.Locator) && nodeDigestPattern.MatchString(material.Digest) && material.Mode != nil && *material.Mode >= 0 && *material.Mode <= 4095 && material.UID != nil && *material.UID >= 0 && material.GID != nil && *material.GID >= 0
}

func ValidateNodeOperationReceipt(receipt NodeOperationReceipt) error {
	if receipt.SchemaVersion != NodeSchemaVersion || !nodeUUIDPattern.MatchString(receipt.ReceiptID) || !nodeUUIDPattern.MatchString(receipt.OperationID) || !nodeUUIDPattern.MatchString(receipt.NodeID) || !nodeUUIDPattern.MatchString(receipt.WorkspaceID) || !validNodeOperationType(receipt.OperationType) || receipt.ExpectedNodeVersion < 1 || !oneOf(receipt.Outcome, "succeeded", "failed", "cancelled", "partial", "recovery_required") || receipt.Actions == nil || receipt.Residues == nil || len(receipt.Actions) > 4096 || len(receipt.Residues) > 4096 || !oneOf(receipt.SignerKind, "node_identity", "control_plane") || !nodeDigestPattern.MatchString(receipt.SignerFingerprint) {
		return fmt.Errorf("operation receipt identity or outcome is invalid")
	}
	started, err := time.Parse(time.RFC3339, receipt.StartedAt)
	if err != nil {
		return fmt.Errorf("operation receipt startedAt is invalid")
	}
	completed, err := time.Parse(time.RFC3339, receipt.CompletedAt)
	if err != nil || completed.Before(started) {
		return fmt.Errorf("operation receipt completedAt is invalid")
	}
	for name, binding := range map[string]*KubernetesBinding{"kubernetesBefore": receipt.KubernetesBefore, "kubernetesAfter": receipt.KubernetesAfter} {
		if binding != nil && !validKubernetesBinding(*binding) {
			return fmt.Errorf("%s is invalid", name)
		}
	}
	seen := make(map[int64]struct{}, len(receipt.Actions))
	for index, action := range receipt.Actions {
		if action.Ordinal < 1 || !oneOf(action.Kind, "api", "kubernetes", "service", "identity", "filesystem") || len(action.Target) < 1 || len(action.Target) > 1024 || !oneOf(action.Outcome, "applied", "skipped", "restored", "failed", "residue") {
			return fmt.Errorf("actions[%d] is invalid", index)
		}
		if _, exists := seen[action.Ordinal]; exists {
			return fmt.Errorf("actions[%d] repeats ordinal", index)
		}
		seen[action.Ordinal] = struct{}{}
	}
	for index, residue := range receipt.Residues {
		if len(residue.Target) < 1 || len(residue.Target) > 1024 || !nodeReasonCodePattern.MatchString(residue.ReasonCode) || len(residue.SafeMessage) < 1 || len(residue.SafeMessage) > 512 {
			return fmt.Errorf("residues[%d] is invalid", index)
		}
	}
	if receipt.SignerKind == "node_identity" {
		if receipt.IdentityGeneration == nil || *receipt.IdentityGeneration < 1 {
			return fmt.Errorf("node-identity receipt generation is invalid")
		}
	} else {
		if receipt.IdentityGeneration != nil || !oneOf(receipt.Outcome, "failed", "cancelled", "recovery_required") {
			return fmt.Errorf("control-plane receipt identity or outcome is invalid")
		}
		for _, action := range receipt.Actions {
			if !oneOf(action.Outcome, "skipped", "failed", "residue") {
				return fmt.Errorf("control-plane receipt claims a host action")
			}
		}
	}
	if receipt.Outcome == "succeeded" {
		if len(receipt.Residues) != 0 {
			return fmt.Errorf("successful operation receipt contains residues")
		}
		for _, action := range receipt.Actions {
			if !oneOf(action.Outcome, "applied", "skipped", "restored") {
				return fmt.Errorf("successful operation receipt action is incoherent")
			}
		}
	}
	if oneOf(receipt.Outcome, "partial", "recovery_required") && len(receipt.Residues) == 0 {
		return fmt.Errorf("partial or recovery receipt must contain residues")
	}
	if len(receipt.SigningKeyID) < 1 || len(receipt.SigningKeyID) > 128 || !nodeDigestPattern.MatchString(receipt.Digest) || len(receipt.Signature) != 86 || !nodeBase64URLPattern.MatchString(receipt.Signature) {
		return fmt.Errorf("operation receipt signature metadata is invalid")
	}
	return nil
}

func DecodeNodeInstallPlan(reader io.Reader) (NodeInstallPlan, error) {
	var plan NodeInstallPlan
	raw, err := decodeStrictNodeObject(reader, &plan)
	if err != nil {
		return plan, err
	}
	if err := requireNodeFields(raw, "schemaVersion", "planId", "nodeId", "enrollmentId", "workspaceId", "idempotencyKey", "approvedBy", "approvedAt", "hostname", "mode", "installProfile", "cluster", "target", "registryTrust", "components", "nodeService", "labels", "taints", "resourceBounds", "mutations", "validationTests", "rollback", "issuedAt", "expiresAt", "signingKeyId", "digest", "signature"); err != nil {
		return plan, err
	}
	for key, fields := range map[string][]string{
		"cluster":        {"id", "workerOnly", "apiServer", "kubernetesVersion", "joinCredentialEndpoint", "bootstrapTaint", "expectedCaFingerprint", "registryEndpoints"},
		"target":         {"platform", "architecture", "machineFingerprint", "nodePublicKeyFingerprint", "minCpu", "minMemoryBytes", "minDiskBytes"},
		"nodeService":    {"manager", "unitName", "binaryPath", "runAsUser", "runAsGroup", "definitionSha256"},
		"resourceBounds": {"reservedCpuMillis", "reservedMemoryBytes", "maxPods", "maxConcurrentAgents"},
		"rollback":       {"preserveUserData", "preserveControlPlane", "ambiguousOwnership", "backupRootClass", "backupRoot"},
	} {
		if err := requireNodeNestedFields(raw, key, fields...); err != nil {
			return plan, err
		}
	}
	if err := requireNodeArrayFields(raw, "registryTrust", "hostname", "caBundleSha256"); err != nil {
		return plan, err
	}
	if err := requireNodeArrayFields(raw, "components", "name", "artifactType", "version", "publisher", "sourceHost", "source", "sha256", "ownership"); err != nil {
		return plan, err
	}
	if err := requireNodeArrayFields(raw, "taints", "key", "value", "effect"); err != nil {
		return plan, err
	}
	if err := requireNodeArrayFields(raw, "mutations", "ordinal", "kind", "action", "target", "desired", "desiredDigest", "mode", "uid", "gid", "rollback"); err != nil {
		return plan, err
	}
	return plan, ValidateNodeInstallPlan(plan)
}

func DecodeNodeInstallReceipt(reader io.Reader) (NodeInstallReceipt, error) {
	var receipt NodeInstallReceipt
	raw, err := decodeStrictNodeObject(reader, &receipt)
	if err != nil {
		return receipt, err
	}
	if err := requireNodeFields(raw, "schemaVersion", "receiptId", "planId", "planDigest", "nodeId", "generation", "nodeIdentityGeneration", "signerKind", "signerFingerprint", "state", "currentStage", "owner", "binary", "service", "mutations", "residues", "createdAt", "updatedAt", "signingKeyId", "digest", "signature"); err != nil {
		return receipt, err
	}
	for key, fields := range map[string][]string{
		"owner":   {"uid", "pid", "processStartIdentity", "nonce"},
		"binary":  {"path", "digest"},
		"service": {"manager", "name", "definitionDigest", "priorEnabled", "priorActive"},
	} {
		if err := requireNodeNestedFields(raw, key, fields...); err != nil {
			return receipt, err
		}
	}
	if err := requireNodeArrayFields(raw, "mutations", "ordinal", "kind", "target", "priorState", "rollbackMaterial", "desiredDigest", "status"); err != nil {
		return receipt, err
	}
	if err := requireNodeArrayFields(raw, "residues", "target", "reasonCode", "safeMessage"); err != nil {
		return receipt, err
	}
	return receipt, ValidateNodeInstallReceipt(receipt)
}

func DecodeNodeOperationReceipt(reader io.Reader) (NodeOperationReceipt, error) {
	var receipt NodeOperationReceipt
	raw, err := decodeStrictNodeObject(reader, &receipt)
	if err != nil {
		return receipt, err
	}
	if err := requireNodeFields(raw, "schemaVersion", "receiptId", "operationId", "nodeId", "workspaceId", "operationType", "expectedNodeVersion", "startedAt", "completedAt", "outcome", "kubernetesBefore", "kubernetesAfter", "actions", "residues", "signerKind", "identityGeneration", "signerFingerprint", "signingKeyId", "digest", "signature"); err != nil {
		return receipt, err
	}
	if err := requireNodeArrayFields(raw, "actions", "ordinal", "kind", "target", "outcome"); err != nil {
		return receipt, err
	}
	if err := requireNodeArrayFields(raw, "residues", "target", "reasonCode", "safeMessage"); err != nil {
		return receipt, err
	}
	return receipt, ValidateNodeOperationReceipt(receipt)
}

func decodeStrictNodeObject(reader io.Reader, output any) (map[string]json.RawMessage, error) {
	encoded, err := io.ReadAll(io.LimitReader(reader, nodeMaxJSONBytes+1))
	if err != nil {
		return nil, err
	}
	if len(encoded) > nodeMaxJSONBytes {
		return nil, fmt.Errorf("node JSON exceeds 8 MiB")
	}
	if err := decodeClosedNodeResponse(bytes.NewReader(encoded), output); err != nil {
		return nil, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &raw); err != nil || raw == nil {
		return nil, fmt.Errorf("node JSON must be an object")
	}
	return raw, nil
}

func decodeTypedNodeAPIResponse(reader io.Reader, output any) error {
	raw, err := decodeStrictNodeObject(reader, output)
	if err != nil {
		return err
	}
	switch value := output.(type) {
	case *NodeEnrollmentSecret:
		if err := requireNodeFields(raw, "id", "token", "tokenKeyId", "expiresAt", "replayed"); err != nil {
			return err
		}
		return ValidateNodeEnrollmentSecret(*value)
	case *Node:
		if err := requireNodeFields(raw, "id", "workspaceId", "name", "kind", "platform", "architecture", "lifecycleState", "trustState", "agentEligible", "version", "capabilityVersion", "identity", "createdAt", "updatedAt"); err != nil {
			return err
		}
		return ValidateNode(*value)
	case *NodeList:
		if err := requireNodeFields(raw, "items"); err != nil {
			return err
		}
		if err := requireNodeArrayFields(raw, "items", "id", "workspaceId", "name", "kind", "platform", "architecture", "lifecycleState", "trustState", "agentEligible", "version", "capabilityVersion", "identity", "createdAt", "updatedAt"); err != nil {
			return err
		}
		if value.Items == nil {
			return fmt.Errorf("node list items must be an array")
		}
		for index, node := range value.Items {
			if err := ValidateNode(node); err != nil {
				return fmt.Errorf("items[%d]: %w", index, err)
			}
		}
		return nil
	case *NodeOperation:
		if err := requireNodeFields(raw, "id", "nodeId", "type", "status", "expectedNodeVersion", "result", "error", "receipt", "createdAt"); err != nil {
			return err
		}
		if receiptJSON, exists := raw["receipt"]; exists && string(receiptJSON) != "null" {
			receipt, err := DecodeNodeOperationReceipt(bytes.NewReader(receiptJSON))
			if err != nil {
				return fmt.Errorf("receipt: %w", err)
			}
			value.Receipt = &receipt
		}
		return ValidateNodeOperation(*value)
	case *JoinCredential:
		if err := requireNodeFields(raw, "issuanceId", "credential", "expiresAt", "clusterId", "workerOnly", "replayed"); err != nil {
			return err
		}
		return ValidateJoinCredential(*value)
	default:
		return nil
	}
}

func requireNodeFields(raw map[string]json.RawMessage, fields ...string) error {
	for _, field := range fields {
		if _, exists := raw[field]; !exists {
			return fmt.Errorf("required field %s is missing", field)
		}
	}
	return nil
}

func requireNodeNestedFields(raw map[string]json.RawMessage, key string, fields ...string) error {
	var nested map[string]json.RawMessage
	if err := json.Unmarshal(raw[key], &nested); err != nil || nested == nil {
		return fmt.Errorf("%s must be an object", key)
	}
	if err := requireNodeFields(nested, fields...); err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	return nil
}

func requireNodeArrayFields(raw map[string]json.RawMessage, key string, fields ...string) error {
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(raw[key], &items); err != nil || items == nil {
		return fmt.Errorf("%s must be an array", key)
	}
	for index, item := range items {
		if err := requireNodeFields(item, fields...); err != nil {
			return fmt.Errorf("%s[%d]: %w", key, index, err)
		}
	}
	return nil
}

func DeriveNodeEnrollmentToken(key []byte, workspaceID, enrollmentID, principalID, idempotencyKey string) (token string, tokenHash string, err error) {
	if len(key) != 32 || !nodeUUIDPattern.MatchString(workspaceID) || !nodeUUIDPattern.MatchString(enrollmentID) || !nodeUUIDPattern.MatchString(principalID) || !validNodeIdempotencyKey(idempotencyKey) {
		return "", "", fmt.Errorf("enrollment token derivation input is invalid")
	}
	message := "blazn-node-enrollment-v1\n" + workspaceID + "\n" + enrollmentID + "\n" + principalID + "\n" + idempotencyKey
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(message))
	token = base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	digest := sha256.Sum256([]byte(token))
	return token, hex.EncodeToString(digest[:]), nil
}

func NodeJoinCredentialAAD(context NodeJoinCredentialContext) ([]byte, error) {
	for name, value := range map[string]string{"workspaceId": context.WorkspaceID, "enrollmentId": context.EnrollmentID, "planId": context.PlanID, "nodeId": context.NodeID, "issuanceId": context.IssuanceID} {
		if !nodeUUIDPattern.MatchString(value) {
			return nil, fmt.Errorf("join credential %s is invalid", name)
		}
	}
	if !validNodeIdempotencyKey(context.IdempotencyKey) || !nodeHashPattern.MatchString(context.RequestDigest) {
		return nil, fmt.Errorf("join credential retry binding is invalid")
	}
	return []byte("blazn-node-join-credential-v1\n" + context.WorkspaceID + "\n" + context.EnrollmentID + "\n" + context.PlanID + "\n" + context.NodeID + "\n" + context.IssuanceID + "\n" + context.IdempotencyKey + "\n" + context.RequestDigest), nil
}

func SealNodeJoinCredential(key []byte, credential string, context NodeJoinCredentialContext) ([]byte, error) {
	return sealNodeJoinCredential(key, rand.Reader, credential, context)
}

func sealNodeJoinCredential(key []byte, randomness io.Reader, credential string, context NodeJoinCredentialContext) ([]byte, error) {
	if len(key) != 32 || randomness == nil || len(credential) < 43 || len(credential) > 4096 {
		return nil, fmt.Errorf("join credential encryption input is invalid")
	}
	aad, err := NodeJoinCredentialAAD(context)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(randomness, nonce); err != nil {
		return nil, fmt.Errorf("generate join credential nonce: %w", err)
	}
	sealed := gcm.Seal(nil, nonce, []byte(credential), aad)
	return append(nonce, sealed...), nil
}

func OpenNodeJoinCredential(key, encoded []byte, context NodeJoinCredentialContext) (string, error) {
	if len(key) != 32 {
		return "", fmt.Errorf("join credential decryption key is invalid")
	}
	aad, err := NodeJoinCredentialAAD(context)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(encoded) < gcm.NonceSize()+gcm.Overhead()+1 {
		return "", fmt.Errorf("encrypted join credential is truncated")
	}
	plaintext, err := gcm.Open(nil, encoded[:gcm.NonceSize()], encoded[gcm.NonceSize():], aad)
	if err != nil {
		return "", fmt.Errorf("decrypt join credential: %w", err)
	}
	if len(plaintext) < 43 || len(plaintext) > 4096 {
		return "", fmt.Errorf("decrypted join credential length is invalid")
	}
	return string(plaintext), nil
}

func NodeInstallPlanDigest(plan NodeInstallPlan) (string, error) {
	return nodeCanonicalDigest(plan, "digest", "signature")
}

func NodeInstallReceiptDigest(receipt NodeInstallReceipt) (string, error) {
	return nodeCanonicalDigest(receipt, "digest", "signature")
}

func NodeOperationReceiptDigest(receipt NodeOperationReceipt) (string, error) {
	return nodeCanonicalDigest(receipt, "digest", "signature")
}

func NodeCapabilityDigest(capability NodeCapability) (string, error) {
	if err := ValidateNodeCapability(capability); err != nil {
		return "", err
	}
	canonical, err := nodeCanonicalJSON(capability)
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	_, _ = digest.Write([]byte("blazn-node-capability-v1\n"))
	_, _ = digest.Write(canonical)
	return "sha256:" + hex.EncodeToString(digest.Sum(nil)), nil
}

func NodePublicKeyFingerprint(publicKey ed25519.PublicKey) (string, error) {
	if len(publicKey) != ed25519.PublicKeySize {
		return "", fmt.Errorf("node Ed25519 public key must contain 32 bytes")
	}
	digest := sha256.Sum256(publicKey)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func VerifyNodeInstallPlan(plan NodeInstallPlan, trust NodeInstallPlanTrust) error {
	if err := ValidateNodeInstallPlan(plan); err != nil {
		return err
	}
	if trust.Now.IsZero() {
		return fmt.Errorf("trusted current time is required")
	}
	digest, err := NodeInstallPlanDigest(plan)
	if err != nil {
		return err
	}
	if !nodeSecureEqual(plan.Digest, digest) {
		return fmt.Errorf("install plan digest mismatch")
	}
	if err := verifyNodeSignature(trust.Keyring, plan.SigningKeyID, plan.Signature, "blazn-node-install-plan-v1\n", digest); err != nil {
		return fmt.Errorf("install plan signature: %w", err)
	}
	issued, _ := time.Parse(time.RFC3339, plan.IssuedAt)
	expires, _ := time.Parse(time.RFC3339, plan.ExpiresAt)
	if trust.Now.Before(issued) || !trust.Now.Before(expires) {
		return fmt.Errorf("install plan is not active at trusted current time")
	}
	publicKeyFingerprint, err := NodePublicKeyFingerprint(trust.NodePublicKey)
	if err != nil {
		return err
	}
	bindings := [][3]string{
		{"workspaceId", plan.WorkspaceID, trust.WorkspaceID},
		{"enrollmentId", plan.EnrollmentID, trust.EnrollmentID},
		{"nodeId", plan.NodeID, trust.NodeID},
		{"hostname", plan.Hostname, trust.Hostname},
		{"machineFingerprint", plan.Target.MachineFingerprint, trust.MachineFingerprint},
		{"nodePublicKeyFingerprint", plan.Target.NodePublicKeyFingerprint, publicKeyFingerprint},
		{"platform", string(plan.Target.Platform), string(trust.Platform)},
		{"architecture", string(plan.Target.Architecture), string(trust.Architecture)},
		{"idempotencyKey", plan.IdempotencyKey, trust.IdempotencyKey},
	}
	for _, binding := range bindings {
		if binding[2] == "" || !nodeSecureEqual(binding[1], binding[2]) {
			return fmt.Errorf("install plan %s does not match trusted input", binding[0])
		}
	}
	return ValidateNodeInstallProfile(plan, trust.Profile)
}

func ValidateNodeInstallProfile(plan NodeInstallPlan, profile NodeTrustedInstallProfile) error {
	if profile.ID == "" || plan.InstallProfile != profile.ID || profile.VerifyNoSymlinkTraversal == nil || !validNodeProfileTarget(profile.ID, plan.Mode, plan.Target.Platform, plan.Target.Architecture) {
		return fmt.Errorf("trusted install profile does not match plan target")
	}
	if err := validateNodeTrustedOrigins(profile.AllowedClusterOrigins); err != nil {
		return fmt.Errorf("cluster origins: %w", err)
	}
	if err := validateNodeTrustedOrigins(profile.AllowedDownloadOrigins); err != nil {
		return fmt.Errorf("download origins: %w", err)
	}
	if err := validateNodeTrustedOrigins(profile.AllowedRegistryOrigins); err != nil {
		return fmt.Errorf("registry origins: %w", err)
	}
	if !nodeOriginAllowed(plan.Cluster.APIServer, profile.AllowedClusterOrigins) {
		return fmt.Errorf("cluster API origin is not trusted")
	}
	for _, endpoint := range plan.Cluster.RegistryEndpoints {
		if !nodeOriginAllowed(endpoint, profile.AllowedRegistryOrigins) {
			return fmt.Errorf("registry origin is not trusted")
		}
	}
	for _, trust := range plan.RegistryTrust {
		if !nodeHostAllowed(trust.Hostname, profile.AllowedRegistryOrigins) {
			return fmt.Errorf("registry trust hostname is not trusted")
		}
	}
	for _, component := range plan.Components {
		if err := ValidateNodeComponentRedirect(profile, component, component.Source); err != nil {
			return err
		}
		if component.ArtifactType == "package" && !nodeOriginAllowed(component.RepositoryOrigin, profile.AllowedDownloadOrigins) {
			return fmt.Errorf("package component %q repository origin is not trusted", component.Name)
		}
		if component.ArtifactType == "image" {
			origin, _, err := nodeOCIRegistryOrigin(component.OCIReference)
			if err != nil || !nodeOriginAllowed(origin, profile.AllowedRegistryOrigins) {
				return fmt.Errorf("image component %q registry origin is not trusted", component.Name)
			}
		}
	}
	if err := validateNodeTrustedRoots(profile.AllowedMutationRoots); err != nil {
		return err
	}
	paths := []string{plan.NodeService.BinaryPath, plan.Rollback.BackupRoot}
	for _, mutation := range plan.Mutations {
		if oneOf(mutation.Kind, "file", "certificate", "directory", "systemd_unit", "launchd_unit") {
			paths = append(paths, mutation.Target)
		}
	}
	for _, target := range paths {
		if !nodePathUnderAny(target, profile.AllowedMutationRoots) {
			return fmt.Errorf("install target %q is outside trusted roots", target)
		}
		if err := profile.VerifyNoSymlinkTraversal(target); err != nil {
			return fmt.Errorf("install target %q traverses an untrusted link: %w", target, err)
		}
	}
	return nil
}

func ValidateNodeComponentRedirect(profile NodeTrustedInstallProfile, component NodeInstallComponent, candidateURL string) error {
	parsed, err := url.Parse(candidateURL)
	if err != nil || !validHTTPSURL(candidateURL) || parsed.Hostname() != component.SourceHost || !nodeOriginAllowed(candidateURL, profile.AllowedDownloadOrigins) {
		return fmt.Errorf("component %q source or redirect is not trusted", component.Name)
	}
	return nil
}

func validNodeProfileTarget(profileID string, mode NodeEnrollmentMode, platform NodePlatform, architecture NodeArchitecture) bool {
	switch profileID {
	case "ubuntu-26.04-amd64-worker/v1":
		return mode == NodeModeFresh && platform == NodePlatformLinux && architecture == NodeArchAMD64
	case "existing-linux-worker-adopt/v1":
		return mode == NodeModeAdopt && platform == NodePlatformLinux && validArchitecture(architecture)
	case "macos-lima-worker-adopt/v1":
		return mode == NodeModeAdopt && platform == NodePlatformMacOS && architecture == NodeArchARM64
	default:
		return false
	}
}

func validateNodeTrustedOrigins(origins []string) error {
	if origins == nil || duplicateNodeStrings(origins) {
		return fmt.Errorf("trusted origins must be present and unique")
	}
	for _, origin := range origins {
		canonical, err := nodeURLOrigin(origin)
		if err != nil || canonical != origin {
			return fmt.Errorf("trusted origin %q is not canonical", origin)
		}
	}
	return nil
}

func validateNodeTrustedRoots(roots []string) error {
	if len(roots) == 0 || duplicateNodeStrings(roots) {
		return fmt.Errorf("trusted mutation roots must be present and unique")
	}
	for _, root := range roots {
		if root == "/" || !strings.HasPrefix(root, "/") || path.Clean(root) != root || nodeHasParentTraversal(root) {
			return fmt.Errorf("trusted mutation root %q is unsafe", root)
		}
	}
	return nil
}

func nodeURLOrigin(value string) (string, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return "", fmt.Errorf("URL must use a credential-free HTTPS origin")
	}
	return "https://" + strings.ToLower(parsed.Host), nil
}

func nodeOriginAllowed(value string, allowed []string) bool {
	origin, err := nodeURLOrigin(value)
	if err != nil {
		return false
	}
	for _, candidate := range allowed {
		if origin == candidate {
			return true
		}
	}
	return false
}

func nodeHostAllowed(hostname string, allowed []string) bool {
	for _, origin := range allowed {
		parsed, err := url.Parse(origin)
		if err == nil && parsed.Hostname() == hostname {
			return true
		}
	}
	return false
}

func nodeOCIRegistryOrigin(reference string) (origin string, hostname string, err error) {
	slash := strings.IndexByte(reference, '/')
	if slash <= 0 {
		return "", "", fmt.Errorf("OCI reference has no registry authority")
	}
	origin, err = nodeURLOrigin("https://" + reference[:slash])
	if err != nil {
		return "", "", err
	}
	parsed, _ := url.Parse(origin)
	return origin, parsed.Hostname(), nil
}

func VerifyNodeInstallReceipt(receipt NodeInstallReceipt, trust NodeInstallReceiptTrust) error {
	if err := ValidateNodeInstallReceipt(receipt); err != nil {
		return err
	}
	digest, err := NodeInstallReceiptDigest(receipt)
	if err != nil {
		return err
	}
	if !nodeSecureEqual(receipt.Digest, digest) {
		return fmt.Errorf("install receipt digest mismatch")
	}
	if trust.Signer.Kind != "node_identity" || trust.Signer.Status != "active" || trust.Signer.Generation < 1 || receipt.NodeIdentityGeneration != trust.Signer.Generation || receipt.SignerKind != "node_identity" {
		return fmt.Errorf("install receipt identity generation does not match trusted active signer")
	}
	if err := verifyTrustedNodeSigner(trust.Signer, receipt.SigningKeyID, receipt.SignerFingerprint, receipt.Signature, "blazn-node-install-receipt-v1\n", digest); err != nil {
		return fmt.Errorf("install receipt signature: %w", err)
	}
	for name, values := range map[string][2]string{"planId": {receipt.PlanID, trust.PlanID}, "planDigest": {receipt.PlanDigest, trust.PlanDigest}, "nodeId": {receipt.NodeID, trust.NodeID}} {
		if values[1] == "" || !nodeSecureEqual(values[0], values[1]) {
			return fmt.Errorf("install receipt %s does not match trusted input", name)
		}
	}
	if (!nodeLinuxBackupRootPattern.MatchString(trust.BackupRoot) && !nodeMacOSBackupRootPattern.MatchString(trust.BackupRoot)) || trust.VerifyNoSymlinkTraversal == nil {
		return fmt.Errorf("trusted receipt backup root is invalid")
	}
	for _, mutation := range receipt.Mutations {
		if mutation.RollbackMaterial.Kind == "absent" {
			continue
		}
		resolved, err := ResolveNodeRollbackLocator(trust.BackupRoot, mutation.RollbackMaterial.Locator)
		if err != nil {
			return err
		}
		if err := trust.VerifyNoSymlinkTraversal(resolved); err != nil {
			return fmt.Errorf("rollback material traverses an untrusted link: %w", err)
		}
	}
	return nil
}

func ResolveNodeRollbackLocator(backupRoot, locator string) (string, error) {
	if (!nodeLinuxBackupRootPattern.MatchString(backupRoot) && !nodeMacOSBackupRootPattern.MatchString(backupRoot)) || !nodeReceiptLocatorPattern.MatchString(locator) {
		return "", fmt.Errorf("rollback root or locator is invalid")
	}
	id := strings.TrimPrefix(locator, "receipt-backup://")
	resolved := path.Join(backupRoot, id)
	if !nodePathUnderChildAny(resolved, []string{backupRoot}) {
		return "", fmt.Errorf("rollback locator escapes backup root")
	}
	return resolved, nil
}

func VerifyNodeOperationReceipt(receipt NodeOperationReceipt, trust NodeOperationReceiptTrust) error {
	if err := ValidateNodeOperationReceipt(receipt); err != nil {
		return err
	}
	digest, err := NodeOperationReceiptDigest(receipt)
	if err != nil {
		return err
	}
	if !nodeSecureEqual(receipt.Digest, digest) {
		return fmt.Errorf("operation receipt digest mismatch")
	}
	var signer *NodeTrustedSigner
	if receipt.SignerKind == "node_identity" {
		signer = trust.NodeIdentitySigner
		if signer == nil || signer.Kind != "node_identity" || signer.Status != "active" || receipt.IdentityGeneration == nil || signer.Generation < 1 || *receipt.IdentityGeneration != signer.Generation {
			return fmt.Errorf("operation receipt node identity generation does not match trusted signer")
		}
	} else {
		signer = trust.ControlPlaneSigner
		if signer == nil || signer.Kind != "control_plane" || signer.Generation != 0 || receipt.IdentityGeneration != nil {
			return fmt.Errorf("operation receipt control-plane signer is invalid")
		}
	}
	if err := verifyTrustedNodeSigner(*signer, receipt.SigningKeyID, receipt.SignerFingerprint, receipt.Signature, "blazn-node-operation-receipt-v1\n", digest); err != nil {
		return fmt.Errorf("operation receipt signature: %w", err)
	}
	bindings := [][3]string{{"operationId", receipt.OperationID, trust.OperationID}, {"nodeId", receipt.NodeID, trust.NodeID}, {"workspaceId", receipt.WorkspaceID, trust.WorkspaceID}, {"operationType", string(receipt.OperationType), string(trust.OperationType)}}
	for _, binding := range bindings {
		if binding[2] == "" || !nodeSecureEqual(binding[1], binding[2]) {
			return fmt.Errorf("operation receipt %s does not match trusted input", binding[0])
		}
	}
	if trust.ExpectedNodeVersion < 1 || receipt.ExpectedNodeVersion != trust.ExpectedNodeVersion {
		return fmt.Errorf("operation receipt expectedNodeVersion does not match trusted input")
	}
	return nil
}

func verifyTrustedNodeSigner(signer NodeTrustedSigner, receiptKeyID, receiptFingerprint, signature, prefix, digest string) error {
	if signer.KeyID == "" || !nodeSecureEqual(receiptKeyID, signer.KeyID) || !nodeDigestPattern.MatchString(signer.Fingerprint) || len(signer.PublicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("trusted signer identity is invalid")
	}
	computed, err := NodePublicKeyFingerprint(signer.PublicKey)
	if err != nil || !nodeSecureEqual(computed, signer.Fingerprint) || !nodeSecureEqual(receiptFingerprint, signer.Fingerprint) {
		return fmt.Errorf("trusted signer fingerprint mismatch")
	}
	return verifyNodeSignature(NodeSigningKeyring{signer.KeyID: signer.PublicKey}, receiptKeyID, signature, prefix, digest)
}

func nodeCanonicalDigest(value any, omitted ...string) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &object); err != nil || object == nil {
		return "", fmt.Errorf("signed node document must be an object")
	}
	for _, field := range omitted {
		delete(object, field)
	}
	canonical, err := nodeCanonicalJSON(object)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func nodeCanonicalJSON(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode canonical node JSON: %w", err)
	}
	canonical, err := jcs.Transform(encoded)
	if err != nil {
		return nil, fmt.Errorf("canonicalize node JSON: %w", err)
	}
	return canonical, nil
}

func verifyNodeSignature(keyring NodeSigningKeyring, keyID, encodedSignature, prefix, digest string) error {
	key, exists := keyring[keyID]
	if !exists || len(key) != ed25519.PublicKeySize {
		return fmt.Errorf("signing key ID is not pinned")
	}
	signature, err := base64.RawURLEncoding.DecodeString(encodedSignature)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return fmt.Errorf("signature encoding is invalid")
	}
	if !ed25519.Verify(key, []byte(prefix+digest), signature) {
		return fmt.Errorf("signature verification failed")
	}
	return nil
}

func nodeSecureEqual(left, right string) bool {
	return len(left) == len(right) && subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func validatePlanMutations(mutations []NodeInstallMutation) error {
	seen := make(map[int64]struct{}, len(mutations))
	for index, mutation := range mutations {
		if mutation.Ordinal < 1 || !oneOf(mutation.Kind, nodeMutationKinds()...) || len(mutation.Target) < 1 || len(mutation.Target) > 1024 || nodeHasParentTraversal(mutation.Target) || mutation.Desired == nil || len(mutation.Desired) > 32 || !nodeDigestPattern.MatchString(mutation.DesiredDigest) || mutation.Mode < 0 || mutation.Mode > 4095 || mutation.UID < 0 || mutation.GID < 0 || !oneOf(mutation.Rollback, "restore_prior", "remove_if_owned", "leave_and_report") {
			return fmt.Errorf("mutations[%d] is invalid", index)
		}
		if err := validateNodeMutationSemantics(mutation); err != nil {
			return fmt.Errorf("mutations[%d]: %w", index, err)
		}
		if _, exists := seen[mutation.Ordinal]; exists {
			return fmt.Errorf("mutations[%d] repeats ordinal %d", index, mutation.Ordinal)
		}
		seen[mutation.Ordinal] = struct{}{}
	}
	return nil
}

func validateNodeMutationSemantics(mutation NodeInstallMutation) error {
	switch mutation.Kind {
	case "package":
		var desired struct {
			Manager       string `json:"manager"`
			Version       string `json:"version"`
			ComponentName string `json:"componentName"`
		}
		if !oneOf(mutation.Action, "install", "adopt_exact") || !nodePackageTargetPattern.MatchString(mutation.Target) || decodeNodeDesired(mutation.Desired, &desired) != nil || !oneOf(desired.Manager, "apt", "snap", "brew") || len(desired.Version) < 1 || len(desired.Version) > 128 || len(desired.ComponentName) < 1 || len(desired.ComponentName) > 128 {
			return fmt.Errorf("package action or payload is invalid")
		}
	case "file", "certificate":
		var desired struct {
			SourceComponent string `json:"sourceComponent"`
			ContentSHA256   string `json:"contentSha256"`
		}
		if !oneOf(mutation.Action, "write", "adopt_exact") || !nodePathUnderChildAny(mutation.Target, []string{"/usr/local/bin", "/etc/blazn", "/var/lib/blazn"}) || decodeNodeDesired(mutation.Desired, &desired) != nil || len(desired.SourceComponent) < 1 || len(desired.SourceComponent) > 128 || !nodeHashPattern.MatchString(desired.ContentSHA256) {
			return fmt.Errorf("file action or payload is invalid")
		}
	case "directory":
		var desired struct{}
		if !oneOf(mutation.Action, "create", "adopt_exact") || !nodePathUnderAny(mutation.Target, []string{"/etc/blazn", "/var/lib/blazn", "/opt/blazn"}) || decodeNodeDesired(mutation.Desired, &desired) != nil {
			return fmt.Errorf("directory action or payload is invalid")
		}
	case "systemd_unit":
		var desired struct {
			UnitName        string `json:"unitName"`
			SourceComponent string `json:"sourceComponent"`
		}
		if !oneOf(mutation.Action, "write", "enable", "adopt_exact") || !nodeSystemdTargetPattern.MatchString(mutation.Target) || decodeNodeDesired(mutation.Desired, &desired) != nil || !nodeSystemdUnitPattern.MatchString(desired.UnitName) || path.Base(mutation.Target) != desired.UnitName || len(desired.SourceComponent) < 1 || len(desired.SourceComponent) > 128 {
			return fmt.Errorf("systemd action or payload is invalid")
		}
	case "launchd_unit":
		var desired struct {
			Label           string `json:"label"`
			SourceComponent string `json:"sourceComponent"`
		}
		if !oneOf(mutation.Action, "write", "enable", "adopt_exact") || !nodeLaunchdTargetPattern.MatchString(mutation.Target) || decodeNodeDesired(mutation.Desired, &desired) != nil || len(desired.Label) < 1 || len(desired.Label) > 256 || len(desired.SourceComponent) < 1 || len(desired.SourceComponent) > 128 {
			return fmt.Errorf("launchd action or payload is invalid")
		}
	case "image":
		var desired struct {
			Platform      string `json:"platform"`
			ComponentName string `json:"componentName"`
		}
		if !oneOf(mutation.Action, "pull", "adopt_exact") || !nodeImageTargetPattern.MatchString(mutation.Target) || decodeNodeDesired(mutation.Desired, &desired) != nil || !oneOf(desired.Platform, "linux/amd64", "linux/arm64") || len(desired.ComponentName) < 1 || len(desired.ComponentName) > 128 {
			return fmt.Errorf("image action or payload is invalid")
		}
	case "label":
		var desired struct {
			Value string `json:"value"`
		}
		if mutation.Action != "apply" || !nodeLabelPattern.MatchString(mutation.Target) || decodeNodeDesired(mutation.Desired, &desired) != nil || len(desired.Value) > 128 {
			return fmt.Errorf("label action or payload is invalid")
		}
	case "taint":
		var desired struct {
			Value  string `json:"value"`
			Effect string `json:"effect"`
		}
		if mutation.Action != "apply" || !nodeLabelPattern.MatchString(mutation.Target) || decodeNodeDesired(mutation.Desired, &desired) != nil || len(desired.Value) > 128 || !oneOf(desired.Effect, "NoSchedule", "PreferNoSchedule", "NoExecute") {
			return fmt.Errorf("taint action or payload is invalid")
		}
	case "firewall":
		var desired struct {
			Protocol  string `json:"protocol"`
			Port      int    `json:"port"`
			Direction string `json:"direction"`
		}
		if mutation.Action != "apply" || !nodeFirewallTargetPattern.MatchString(mutation.Target) || decodeNodeDesired(mutation.Desired, &desired) != nil || !oneOf(desired.Protocol, "tcp", "udp") || desired.Port < 1 || desired.Port > 65535 || !oneOf(desired.Direction, "ingress", "egress") {
			return fmt.Errorf("firewall action or payload is invalid")
		}
	default:
		return fmt.Errorf("mutation kind is invalid")
	}
	return nil
}

func decodeNodeDesired(input map[string]any, output any) error {
	encoded, err := json.Marshal(input)
	if err != nil {
		return err
	}
	return decodeClosedNodeObject(encoded, output)
}

func nodeMutationSourceComponent(mutation NodeInstallMutation) string {
	value, _ := mutation.Desired["sourceComponent"].(string)
	if value == "" {
		value, _ = mutation.Desired["componentName"].(string)
	}
	return value
}

func validateNodeMutationComponentBinding(plan NodeInstallPlan, mutation NodeInstallMutation, component NodeInstallComponent) error {
	switch mutation.Kind {
	case "package":
		version, _ := mutation.Desired["version"].(string)
		if component.ArtifactType != "package" || mutation.Target != component.Name || version != component.Version || component.RepositoryOrigin == "" {
			return fmt.Errorf("package mutation does not match its signed component")
		}
	case "image":
		if component.ArtifactType != "image" || mutation.Target != component.OCIReference || component.RegistryHost == "" || !strings.HasSuffix(mutation.Target, "@sha256:"+component.SHA256) {
			return fmt.Errorf("image mutation does not match its signed component")
		}
	default:
		_ = plan
	}
	return nil
}

func validateNodeProfileSemantics(plan NodeInstallPlan) error {
	wantImagePlatform := "linux/amd64"
	if plan.Target.Architecture == NodeArchARM64 {
		wantImagePlatform = "linux/arm64"
	}
	switch plan.InstallProfile {
	case "ubuntu-26.04-amd64-worker/v1", "existing-linux-worker-adopt/v1":
		if plan.NodeService.Manager != "systemd" || plan.NodeService.RunAsUser != "blazn-node" || plan.NodeService.RunAsGroup != "blazn-node" || plan.Rollback.BackupRootClass != "linux_var_lib" {
			return fmt.Errorf("Linux profile service identity is invalid")
		}
		for _, mutation := range plan.Mutations {
			if mutation.Kind == "launchd_unit" {
				return fmt.Errorf("Linux profile contains a launchd mutation")
			}
			if mutation.Kind == "package" {
				manager, _ := mutation.Desired["manager"].(string)
				if !oneOf(manager, "apt", "snap") {
					return fmt.Errorf("Linux profile package manager is invalid")
				}
			}
			if mutation.Kind == "image" && mutation.Desired["platform"] != wantImagePlatform {
				return fmt.Errorf("Linux profile image architecture is invalid")
			}
		}
	case "macos-lima-worker-adopt/v1":
		if plan.NodeService.Manager != "launchd" || plan.NodeService.RunAsUser != "root" || plan.NodeService.RunAsGroup != "wheel" || plan.Rollback.BackupRootClass != "macos_library_application_support" {
			return fmt.Errorf("macOS profile service identity is invalid")
		}
		for _, mutation := range plan.Mutations {
			if mutation.Kind == "systemd_unit" {
				return fmt.Errorf("macOS profile contains a systemd mutation")
			}
			if mutation.Kind == "package" && mutation.Desired["manager"] != "brew" {
				return fmt.Errorf("macOS profile package manager is invalid")
			}
			if mutation.Kind == "image" && mutation.Desired["platform"] != "linux/arm64" {
				return fmt.Errorf("macOS profile image architecture is invalid")
			}
		}
	default:
		return fmt.Errorf("install profile is invalid")
	}
	return nil
}

func nodeHasParentTraversal(value string) bool {
	for _, segment := range strings.Split(value, "/") {
		if segment == ".." {
			return true
		}
	}
	return value == "/"
}

func nodePathUnderAny(target string, roots []string) bool {
	if !strings.HasPrefix(target, "/") || path.Clean(target) != target || nodeHasParentTraversal(target) {
		return false
	}
	for _, root := range roots {
		cleanRoot := path.Clean(root)
		if cleanRoot != "/" && (target == cleanRoot || strings.HasPrefix(target, cleanRoot+"/")) {
			return true
		}
	}
	return false
}

func nodePathUnderChildAny(target string, roots []string) bool {
	if !strings.HasPrefix(target, "/") || path.Clean(target) != target || nodeHasParentTraversal(target) {
		return false
	}
	for _, root := range roots {
		cleanRoot := path.Clean(root)
		if cleanRoot != "/" && strings.HasPrefix(target, cleanRoot+"/") {
			return true
		}
	}
	return false
}

func nodeMutationKinds() []string {
	return []string{"package", "file", "directory", "systemd_unit", "launchd_unit", "certificate", "image", "label", "taint", "firewall"}
}

func validPlatform(value NodePlatform) bool {
	return value == NodePlatformLinux || value == NodePlatformMacOS
}

func validArchitecture(value NodeArchitecture) bool {
	return value == NodeArchAMD64 || value == NodeArchARM64
}

func validNodeOperationType(value NodeOperationType) bool {
	return oneOf(string(value), "pause", "resume", "label", "cordon", "uncordon", "rotate_identity", "repair", "update", "drain", "remove")
}

func ValidateNodeEnrollmentSecret(secret NodeEnrollmentSecret) error {
	if !nodeUUIDPattern.MatchString(secret.ID) || len(secret.Token) < 43 || len(secret.Token) > 128 || secret.TokenKeyID != NodeEnrollmentTokenKeyID {
		return fmt.Errorf("node enrollment response is invalid")
	}
	if _, err := time.Parse(time.RFC3339, secret.ExpiresAt); err != nil {
		return fmt.Errorf("node enrollment expiresAt is invalid")
	}
	return nil
}

func ValidateNode(node Node) error {
	if !nodeUUIDPattern.MatchString(node.ID) || !nodeUUIDPattern.MatchString(node.WorkspaceID) || node.Name == "" || !oneOf(node.Kind, "personal", "shared", "managed") || !validPlatform(node.Platform) || !validArchitecture(node.Architecture) || !oneOf(string(node.LifecycleState), "pending", "installing", "verifying", "active", "paused", "draining", "offline", "quarantined", "removed") || !oneOf(string(node.TrustState), "unverified", "verifying", "verified", "rotating", "revoked") || node.Version < 1 || (node.CapabilityVersion != nil && *node.CapabilityVersion < 1) {
		return fmt.Errorf("node response is invalid")
	}
	if node.KubernetesBinding != nil && !validKubernetesBinding(*node.KubernetesBinding) {
		return fmt.Errorf("node Kubernetes binding is invalid")
	}
	if node.AgentEligible && (node.LifecycleState != "active" || node.TrustState != "verified" || node.KubernetesBinding == nil || node.Identity == nil || node.Identity.Status != "active" || node.CapabilityVersion == nil) {
		return fmt.Errorf("agent-eligible node state is invalid")
	}
	if node.Identity != nil {
		if node.Identity.Generation < 1 || !nodeDigestPattern.MatchString(node.Identity.PublicKeyFingerprint) || !oneOf(node.Identity.Status, "active", "rotating", "revoked", "expired") {
			return fmt.Errorf("node identity is invalid")
		}
		if err := validateTimeWindow(node.Identity.IssuedAt, node.Identity.ExpiresAt); err != nil {
			return fmt.Errorf("node identity time window is invalid")
		}
	}
	if _, err := time.Parse(time.RFC3339, node.CreatedAt); err != nil {
		return fmt.Errorf("node createdAt is invalid")
	}
	if _, err := time.Parse(time.RFC3339, node.UpdatedAt); err != nil {
		return fmt.Errorf("node updatedAt is invalid")
	}
	return nil
}

func ValidateNodeOperation(operation NodeOperation) error {
	if !nodeUUIDPattern.MatchString(operation.ID) || !nodeUUIDPattern.MatchString(operation.NodeID) || !validNodeOperationType(operation.Type) || !oneOf(string(operation.Status), "pending", "running", "succeeded", "failed", "cancelled", "partial", "recovery_required") || operation.ExpectedNodeVersion < 1 {
		return fmt.Errorf("node operation response is invalid")
	}
	terminal := oneOf(string(operation.Status), "succeeded", "failed", "cancelled", "partial", "recovery_required")
	if terminal != (operation.Receipt != nil) {
		return fmt.Errorf("node operation terminal receipt state is invalid")
	}
	if operation.Receipt != nil {
		if err := ValidateNodeOperationReceipt(*operation.Receipt); err != nil {
			return err
		}
		if operation.Receipt.OperationID != operation.ID || operation.Receipt.NodeID != operation.NodeID || operation.Receipt.OperationType != operation.Type || operation.Receipt.ExpectedNodeVersion != operation.ExpectedNodeVersion || operation.Receipt.Outcome != string(operation.Status) {
			return fmt.Errorf("node operation receipt does not bind its response")
		}
	}
	if _, err := time.Parse(time.RFC3339, operation.CreatedAt); err != nil {
		return fmt.Errorf("node operation createdAt is invalid")
	}
	return nil
}

func ValidateJoinCredential(credential JoinCredential) error {
	if !nodeUUIDPattern.MatchString(credential.IssuanceID) || len(credential.Credential) < 43 || len(credential.Credential) > 4096 || len(credential.ClusterID) < 1 || len(credential.ClusterID) > 128 || !credential.WorkerOnly {
		return fmt.Errorf("join credential response is invalid")
	}
	if _, err := time.Parse(time.RFC3339, credential.ExpiresAt); err != nil {
		return fmt.Errorf("join credential expiresAt is invalid")
	}
	return nil
}

func validHTTPSURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func validateTimeWindow(startValue, endValue string) error {
	start, err := time.Parse(time.RFC3339, startValue)
	if err != nil {
		return fmt.Errorf("issuedAt is invalid")
	}
	end, err := time.Parse(time.RFC3339, endValue)
	if err != nil || !end.After(start) {
		return fmt.Errorf("expiresAt must be after issuedAt")
	}
	return nil
}

func (c *Client) CreateNodeEnrollment(ctx context.Context, accessToken, workspaceID, idempotencyKey string, request CreateNodeEnrollmentRequest) (NodeEnrollmentSecret, error) {
	var output NodeEnrollmentSecret
	if accessToken == "" || !validNodeIdempotencyKey(idempotencyKey) || len(request.Name) < 1 || len(request.Name) > 128 || (request.Mode != NodeModeFresh && request.Mode != NodeModeAdopt) || !validPlatform(request.Platform) || (request.Architecture != "" && !validArchitecture(request.Architecture)) {
		return output, fmt.Errorf("node enrollment request is invalid")
	}
	err := c.nodeDo(ctx, http.MethodPost, "/v1/workspaces/"+url.PathEscape(workspaceID)+"/node-enrollments", accessToken, "", idempotencyKey, request, &output, http.StatusCreated)
	return output, err
}

func (c *Client) ExchangeNodeEnrollment(ctx context.Context, enrollmentID string, request ExchangeNodeEnrollmentRequest) (NodeInstallPlan, error) {
	var output NodeInstallPlan
	if len(request.Token) < 43 || len(request.Token) > 128 || !nodeHashPattern.MatchString(request.MachineFingerprint) || len(request.NodePublicKey) != 43 || !nodeBase64URLPattern.MatchString(request.NodePublicKey) || !validPlatform(request.Platform) || !validArchitecture(request.Architecture) {
		return output, fmt.Errorf("node enrollment exchange request is invalid")
	}
	err := c.nodeDo(ctx, http.MethodPost, "/v1/node-enrollments/"+url.PathEscape(enrollmentID)+"/exchange", "", "", "", request, &output, http.StatusOK)
	return output, err
}

func (c *Client) ListNodes(ctx context.Context, accessToken, workspaceID string) (NodeList, error) {
	var output NodeList
	if accessToken == "" {
		return output, fmt.Errorf("access token is required")
	}
	err := c.nodeDo(ctx, http.MethodGet, "/v1/workspaces/"+url.PathEscape(workspaceID)+"/nodes", accessToken, "", "", nil, &output, http.StatusOK)
	return output, err
}

func (c *Client) GetNode(ctx context.Context, accessToken, nodeID string) (Node, error) {
	var output Node
	if accessToken == "" {
		return output, fmt.Errorf("access token is required")
	}
	err := c.nodeDo(ctx, http.MethodGet, "/v1/nodes/"+url.PathEscape(nodeID), accessToken, "", "", nil, &output, http.StatusOK)
	return output, err
}

func (c *Client) CreateNodeOperation(ctx context.Context, accessToken, nodeID, idempotencyKey string, request CreateNodeOperationRequest) (NodeOperation, error) {
	var output NodeOperation
	if accessToken == "" || !validNodeIdempotencyKey(idempotencyKey) {
		return output, fmt.Errorf("access token and valid idempotency key are required")
	}
	if err := ValidateCreateNodeOperationRequest(request); err != nil {
		return output, err
	}
	err := c.nodeDo(ctx, http.MethodPost, "/v1/nodes/"+url.PathEscape(nodeID)+"/operations", accessToken, "", idempotencyKey, request, &output, http.StatusAccepted)
	return output, err
}

func (c *Client) SubmitNodeHeartbeat(ctx context.Context, nodeProof string, heartbeat NodeHeartbeat) error {
	if nodeProof == "" || !nodeUUIDPattern.MatchString(heartbeat.NodeID) || heartbeat.IdentityGeneration < 1 || len(heartbeat.BootID) < 1 || len(heartbeat.BootID) > 128 || heartbeat.Sequence < 0 || !nodeDigestPattern.MatchString(heartbeat.CapabilityDigest) {
		return fmt.Errorf("node heartbeat is invalid")
	}
	if _, err := time.Parse(time.RFC3339, heartbeat.SentAt); err != nil {
		return fmt.Errorf("node heartbeat sentAt is invalid")
	}
	if err := ValidateNodeCapability(heartbeat.Capability); err != nil {
		return fmt.Errorf("node heartbeat capability: %w", err)
	}
	digest, err := NodeCapabilityDigest(heartbeat.Capability)
	if err != nil || !nodeSecureEqual(heartbeat.CapabilityDigest, digest) {
		return fmt.Errorf("node heartbeat capability digest mismatch")
	}
	return c.nodeDo(ctx, http.MethodPost, "/v1/node-service/heartbeats", "", nodeProof, "", heartbeat, nil, http.StatusNoContent)
}

func (c *Client) IssueNodeJoinCredential(ctx context.Context, nodeProof, idempotencyKey string, request JoinCredentialRequest) (JoinCredential, error) {
	var output JoinCredential
	if nodeProof == "" || !validNodeIdempotencyKey(idempotencyKey) || !nodeUUIDPattern.MatchString(request.EnrollmentID) || !nodeUUIDPattern.MatchString(request.PlanID) || !nodeUUIDPattern.MatchString(request.NodeID) || !nodeDigestPattern.MatchString(request.PlanDigest) || !nodeHashPattern.MatchString(request.MachineFingerprint) || !nodeDigestPattern.MatchString(request.NodePublicKeyFingerprint) {
		return output, fmt.Errorf("join credential request is invalid")
	}
	err := c.nodeDo(ctx, http.MethodPost, "/v1/node-service/join-credentials", "", nodeProof, idempotencyKey, request, &output, http.StatusOK)
	return output, err
}

func (c *Client) ConsumeNodeJoinCredential(ctx context.Context, nodeProof, issuanceID, idempotencyKey string, request ConsumeJoinCredentialRequest) (Node, error) {
	var output Node
	if nodeProof == "" || !nodeUUIDPattern.MatchString(issuanceID) || !validNodeIdempotencyKey(idempotencyKey) || !nodeUUIDPattern.MatchString(request.NodeID) || !nodeUUIDPattern.MatchString(request.EnrollmentID) || !nodeUUIDPattern.MatchString(request.PlanID) || len(request.JoinedNodeUID) < 1 || len(request.JoinedNodeUID) > 128 || len(request.JoinedNodeName) < 1 || len(request.JoinedNodeName) > 253 || len(request.ResourceVersion) < 1 || len(request.ResourceVersion) > 128 || len(request.ClusterID) < 1 || len(request.ClusterID) > 128 {
		return output, fmt.Errorf("consume join credential request is invalid")
	}
	err := c.nodeDo(ctx, http.MethodPost, "/v1/node-service/join-credentials/"+url.PathEscape(issuanceID)+"/consume", "", nodeProof, idempotencyKey, request, &output, http.StatusOK)
	return output, err
}

func (c *Client) StreamNodeEvents(ctx context.Context, accessToken, nodeID, lastEventID string) (*NodeEventStream, error) {
	if accessToken == "" {
		return nil, fmt.Errorf("access token is required")
	}
	if len(lastEventID) > 128 {
		return nil, fmt.Errorf("Last-Event-ID exceeds 128 characters")
	}
	endpoint := c.nodeEndpoint("/v1/nodes/" + url.PathEscape(nodeID) + "/events")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create node event request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	response, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call node event API: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		defer response.Body.Close()
		return nil, decodeNodeAPIError(response)
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "text/event-stream" {
		response.Body.Close()
		return nil, fmt.Errorf("node event API returned unsupported content type")
	}
	return &NodeEventStream{Body: response.Body, ContentType: mediaType}, nil
}

func (c *Client) nodeEndpoint(path string) string {
	endpoint := *c.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + path
	return endpoint.String()
}

func (c *Client) nodeDo(ctx context.Context, method, path, accessToken, nodeProof, idempotencyKey string, input, output any, success int) error {
	if accessToken != "" && nodeProof != "" {
		return fmt.Errorf("request cannot use both bearer and node proof authentication")
	}
	if idempotencyKey != "" && !validNodeIdempotencyKey(idempotencyKey) {
		return fmt.Errorf("idempotency key must contain between 8 and 128 characters")
	}
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return fmt.Errorf("encode node request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.nodeEndpoint(path), body)
	if err != nil {
		return fmt.Errorf("create node request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}
	if nodeProof != "" {
		req.Header.Set("X-Blazn-Node-Proof", nodeProof)
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	response, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("call node API: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != success {
		return decodeNodeAPIError(response)
	}
	if output == nil || success == http.StatusNoContent {
		_, err = io.Copy(io.Discard, io.LimitReader(response.Body, nodeMaxJSONBytes))
		return err
	}
	if plan, ok := output.(*NodeInstallPlan); ok {
		decoded, err := DecodeNodeInstallPlan(response.Body)
		if err != nil {
			return fmt.Errorf("decode node response: %w", err)
		}
		*plan = decoded
		return nil
	}
	if err := decodeTypedNodeAPIResponse(io.LimitReader(response.Body, nodeMaxJSONBytes), output); err != nil {
		return fmt.Errorf("decode node response: %w", err)
	}
	return nil
}

func validNodeIdempotencyKey(value string) bool {
	return len(value) >= 8 && len(value) <= 128 && nodeIdempotencyPattern.MatchString(value)
}

func decodeClosedNodeResponse(input io.Reader, output any) error {
	decoder := json.NewDecoder(input)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("response must contain exactly one JSON value")
	}
	return nil
}

func decodeNodeAPIError(response *http.Response) error {
	var apiError ErrorBody
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&apiError); err != nil && err != io.EOF {
		apiError.Message = http.StatusText(response.StatusCode)
	}
	return &APIError{StatusCode: response.StatusCode, Body: apiError}
}
