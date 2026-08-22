// Code generated from the Blazn sandbox contracts; DO NOT EDIT.
// Sandbox OpenAPI SHA256: 5ccadb92e20448475752101cdae00b06d54cd71c496824a8543980257f699f9c
// SandboxTemplate SHA256: e555682663c8c45c6813d65faf1937d5a860e670f2c816fdf31b7fbb96f932e1
// Sandbox CLI contract SHA256: e40063e5f7b1edc107282a637e3d67f1d477467c8e9243d1ae082c0a44c3da83

package client

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/gowebpki/jcs"
)

const (
	SandboxSchemaVersion            = "sandboxes/v1alpha1"
	SandboxTemplateAPIVersion       = "blazn.dev/v1alpha1"
	SandboxTemplateKind             = "SandboxTemplate"
	SandboxIsolationNotice          = "POC orchestration isolation only; approved non-sensitive workloads only"
	SandboxMaxFileBytes       int64 = 8 << 20
)

type SandboxError = ErrorBody
type SandboxManifest = json.RawMessage
type SandboxArchitecture string
type SandboxAllocationMode string
type SandboxState string
type SandboxDesiredState string
type SandboxOperationType string
type SandboxOperationStatus string
type SandboxGrantKind string
type SandboxGrantState string

const (
	SandboxAMD64                     SandboxArchitecture    = "amd64"
	SandboxARM64                     SandboxArchitecture    = "arm64"
	SandboxDirect                    SandboxAllocationMode  = "direct"
	SandboxClaim                     SandboxAllocationMode  = "claim"
	SandboxGrantExec                 SandboxGrantKind       = "exec"
	SandboxGrantUpload               SandboxGrantKind       = "upload"
	SandboxGrantDownload             SandboxGrantKind       = "download"
	SandboxRequested                 SandboxState           = "requested"
	SandboxQueued                    SandboxState           = "queued"
	SandboxProvisioning              SandboxState           = "provisioning"
	SandboxReady                     SandboxState           = "ready"
	SandboxRunning                   SandboxState           = "running"
	SandboxStopping                  SandboxState           = "stopping"
	SandboxStopped                   SandboxState           = "stopped"
	SandboxDeleting                  SandboxState           = "deleting"
	SandboxDeleted                   SandboxState           = "deleted"
	SandboxFailed                    SandboxState           = "failed"
	SandboxDesiredReady              SandboxDesiredState    = "ready"
	SandboxDesiredStopped            SandboxDesiredState    = "stopped"
	SandboxDesiredDeleted            SandboxDesiredState    = "deleted"
	SandboxOperationCreate           SandboxOperationType   = "create"
	SandboxOperationStop             SandboxOperationType   = "stop"
	SandboxOperationDelete           SandboxOperationType   = "delete"
	SandboxOperationPending          SandboxOperationStatus = "pending"
	SandboxOperationRunning          SandboxOperationStatus = "running"
	SandboxOperationSucceeded        SandboxOperationStatus = "succeeded"
	SandboxOperationFailed           SandboxOperationStatus = "failed"
	SandboxOperationRecoveryRequired SandboxOperationStatus = "recovery_required"
	SandboxGrantActive               SandboxGrantState      = "active"
	SandboxGrantConsumed             SandboxGrantState      = "consumed"
	SandboxGrantExpired              SandboxGrantState      = "expired"
	SandboxGrantRevoked              SandboxGrantState      = "revoked"
)

type SandboxTemplate struct {
	ID                 string          `json:"id"`
	WorkspaceID        string          `json:"workspaceId"`
	Name               string          `json:"name"`
	DraftVersion       int64           `json:"draftVersion"`
	DraftManifest      SandboxManifest `json:"draftManifest"`
	DraftDigest        string          `json:"draftDigest"`
	PublishedVersionID *string         `json:"publishedVersionId"`
	CreatedAt          string          `json:"createdAt"`
	UpdatedAt          string          `json:"updatedAt"`
}

type SandboxTemplateEnvelope struct {
	Template SandboxTemplate `json:"template"`
}
type SandboxTemplateList struct {
	Items      []SandboxTemplate `json:"items"`
	NextCursor *string           `json:"nextCursor"`
}
type ReplaceSandboxTemplateDraftRequest struct {
	ExpectedDraftVersion int64           `json:"expectedDraftVersion"`
	Manifest             SandboxManifest `json:"manifest"`
}
type PublishSandboxTemplateVersionRequest struct {
	ExpectedDraftVersion int64 `json:"expectedDraftVersion"`
}

type SandboxTemplateVersion struct {
	ID            string          `json:"id"`
	WorkspaceID   string          `json:"workspaceId"`
	TemplateID    string          `json:"templateId"`
	Name          string          `json:"name"`
	Version       string          `json:"version"`
	ContentDigest string          `json:"contentDigest"`
	Manifest      SandboxManifest `json:"manifest"`
	Status        string          `json:"status"`
	CreatedAt     string          `json:"createdAt"`
}

type SandboxTemplateVersionEnvelope struct {
	Template SandboxTemplate        `json:"template"`
	Version  SandboxTemplateVersion `json:"version"`
}
type SandboxTemplateVersionList struct {
	Items      []SandboxTemplateVersion `json:"items"`
	NextCursor *string                  `json:"nextCursor"`
}
type SandboxTemplateReference struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}
type SandboxSource struct {
	Repository string `json:"repository"`
	Commit     string `json:"commit"`
}

type CreateSandboxRequest struct {
	Template             SandboxTemplateReference `json:"template"`
	Architecture         SandboxArchitecture      `json:"architecture"`
	AllocationMode       SandboxAllocationMode    `json:"allocationMode"`
	ExpiresInSeconds     int64                    `json:"expiresInSeconds"`
	Sources              []SandboxSource          `json:"sources"`
	ApprovedNonSensitive bool                     `json:"approvedNonSensitive"`
}

type SandboxCondition struct {
	Type       string `json:"type"`
	Status     string `json:"status"`
	Reason     string `json:"reason"`
	Message    string `json:"message,omitempty"`
	ObservedAt string `json:"observedAt"`
}
type SandboxSourceBinding struct {
	Repository  string `json:"repository"`
	URL         string `json:"url"`
	Destination string `json:"destination"`
	Writable    bool   `json:"writable"`
	Commit      string `json:"commit"`
}
type SandboxArtifactContractEntry struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	MediaType string `json:"mediaType"`
	Required  bool   `json:"required"`
}
type SandboxArtifactContract struct {
	Digest string                         `json:"digest"`
	Items  []SandboxArtifactContractEntry `json:"items"`
}

type Sandbox struct {
	ID                string                  `json:"id"`
	WorkspaceID       string                  `json:"workspaceId"`
	RequestedBy       string                  `json:"requestedBy"`
	TemplateID        string                  `json:"templateId"`
	TemplateVersionID string                  `json:"templateVersionId"`
	TemplateName      string                  `json:"templateName"`
	TemplateVersion   string                  `json:"templateVersion"`
	TemplateDigest    string                  `json:"templateDigest"`
	VariantName       string                  `json:"variantName"`
	ImageIndexDigest  string                  `json:"imageIndexDigest"`
	ImageDigest       string                  `json:"imageDigest"`
	Architecture      SandboxArchitecture     `json:"architecture"`
	AllocationMode    SandboxAllocationMode   `json:"allocationMode"`
	SourceBindings    []SandboxSourceBinding  `json:"sourceBindings"`
	ArtifactContract  SandboxArtifactContract `json:"artifactContract"`
	State             SandboxState            `json:"state"`
	DesiredState      SandboxDesiredState     `json:"desiredState"`
	Version           int64                   `json:"version"`
	QueueName         string                  `json:"queueName"`
	AdmissionID       *string                 `json:"admissionId"`
	Isolation         string                  `json:"isolation"`
	ExpiresAt         string                  `json:"expiresAt"`
	Conditions        []SandboxCondition      `json:"conditions"`
	CreatedAt         string                  `json:"createdAt"`
	UpdatedAt         string                  `json:"updatedAt"`
	StoppedAt         *string                 `json:"stoppedAt,omitempty"`
	DeletedAt         *string                 `json:"deletedAt,omitempty"`
}

type SandboxList struct {
	Items      []Sandbox `json:"items"`
	NextCursor *string   `json:"nextCursor"`
}
type CreateSandboxOperationRequest struct {
	Type            SandboxOperationType `json:"type"`
	ExpectedVersion int64                `json:"expectedVersion"`
}
type SandboxCleanupResult struct {
	ArtifactIDs []string `json:"artifactIds"`
	Warnings    []string `json:"warnings"`
}
type SandboxBackendReceipt struct {
	Present         bool    `json:"present"`
	UID             *string `json:"uid"`
	ResourceVersion *string `json:"resourceVersion"`
}
type SandboxOperationReceipt struct {
	ID                     string                 `json:"id"`
	OperationID            string                 `json:"operationId"`
	OperationType          SandboxOperationType   `json:"operationType"`
	Status                 SandboxOperationStatus `json:"status"`
	CleanupComplete        bool                   `json:"cleanupComplete"`
	ArtifactExportComplete bool                   `json:"artifactExportComplete"`
	GrantsRevoked          bool                   `json:"grantsRevoked"`
	BackendDestroyed       bool                   `json:"backendDestroyed"`
	Backend                SandboxBackendReceipt  `json:"backend"`
	Result                 *SandboxCleanupResult  `json:"result"`
	Error                  *SandboxError          `json:"error"`
	CreatedAt              string                 `json:"createdAt"`
}
type SandboxOperation struct {
	ID                     string                   `json:"id"`
	SandboxID              string                   `json:"sandboxId"`
	Type                   SandboxOperationType     `json:"type"`
	Status                 SandboxOperationStatus   `json:"status"`
	ExpectedSandboxVersion int64                    `json:"expectedSandboxVersion"`
	Receipt                *SandboxOperationReceipt `json:"receipt"`
	CreatedAt              string                   `json:"createdAt"`
	CompletedAt            *string                  `json:"completedAt"`
}
type SandboxMutation struct {
	Sandbox   Sandbox          `json:"sandbox"`
	Operation SandboxOperation `json:"operation"`
}
type CreateSandboxAccessGrantRequest struct {
	Kind             SandboxGrantKind `json:"kind"`
	ExpiresInSeconds int64            `json:"expiresInSeconds"`
}
type SandboxAccessGrant struct {
	ID          string            `json:"id"`
	SandboxID   string            `json:"sandboxId"`
	WorkspaceID string            `json:"workspaceId"`
	Scope       string            `json:"scope"`
	Kind        SandboxGrantKind  `json:"kind"`
	State       SandboxGrantState `json:"state"`
	ExpiresAt   string            `json:"expiresAt"`
	CreatedAt   string            `json:"createdAt"`
}
type SandboxAccessGrantCreated struct {
	Grant       SandboxAccessGrant `json:"grant"`
	AccessToken string             `json:"accessToken"`
	Endpoint    string             `json:"endpoint"`
}
type SandboxExecRequest struct {
	Command []string `json:"command"`
}
type SandboxExecResult struct {
	RemoteExitCode int    `json:"remoteExitCode"`
	StdoutBase64   string `json:"stdoutBase64"`
	StderrBase64   string `json:"stderrBase64"`
	Truncated      bool   `json:"truncated"`
}
type SandboxFileTransferResult struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}
type SandboxEvent struct {
	EventID     string         `json:"eventId"`
	SandboxID   string         `json:"sandboxId"`
	OperationID *string        `json:"operationId"`
	Sequence    int64          `json:"sequence"`
	Type        string         `json:"type"`
	Payload     map[string]any `json:"payload"`
	CreatedAt   string         `json:"createdAt"`
}
type SandboxArtifactDownload struct {
	Endpoint  string `json:"endpoint"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	MediaType string `json:"mediaType"`
}
type SandboxArtifact struct {
	ID          string                  `json:"id"`
	WorkspaceID string                  `json:"workspaceId"`
	SandboxID   string                  `json:"sandboxId"`
	Name        string                  `json:"name"`
	Path        string                  `json:"path"`
	MediaType   string                  `json:"mediaType"`
	Size        int64                   `json:"size"`
	SHA256      string                  `json:"sha256"`
	ExportedAt  string                  `json:"exportedAt"`
	Download    SandboxArtifactDownload `json:"download"`
}
type SandboxArtifactList struct {
	Items      []SandboxArtifact `json:"items"`
	NextCursor *string           `json:"nextCursor"`
}

var sandboxErrorHTTPStatuses = map[string]int{
	"access_expired":                   401,
	"access_grant_consumed":            410,
	"access_grant_expired":             410,
	"access_grant_revoked":             410,
	"idempotency_conflict":             409,
	"internal_error":                   500,
	"invalid_json":                     400,
	"invalid_request":                  400,
	"membership_required":              403,
	"permission_denied":                403,
	"rate_limited":                     429,
	"request_too_large":                413,
	"sandbox_access_denied":            404,
	"sandbox_architecture_unavailable": 409,
	"sandbox_artifact_not_found":       404,
	"sandbox_backend_unavailable":      503,
	"sandbox_cleanup_incomplete":       409,
	"sandbox_not_found":                404,
	"sandbox_operation_not_found":      404,
	"sandbox_state_conflict":           409,
	"sandbox_template_unavailable":     409,
	"session_revoked":                  401,
	"template_invalid":                 400,
	"template_name_conflict":           409,
	"template_not_found":               404,
	"template_policy_denied":           403,
	"template_version_conflict":        409,
	"template_version_not_found":       404,
	"unauthorized":                     401,
	"version_conflict":                 409}

func SandboxErrorHTTPStatus(code string) (int, bool) {
	status, ok := sandboxErrorHTTPStatuses[code]
	return status, ok
}

// CanonicalSandboxTemplateDigest computes the frozen content identity over only
// the fully resolved spec, using RFC 8785/JCS and a SHA-256 digest.
func CanonicalSandboxTemplateDigest(manifest []byte) (string, []byte, error) {
	var root struct {
		Spec json.RawMessage `json:"spec"`
	}
	if err := json.Unmarshal(manifest, &root); err != nil {
		return "", nil, fmt.Errorf("decode sandbox template: %w", err)
	}
	if len(root.Spec) == 0 || string(root.Spec) == "null" {
		return "", nil, fmt.Errorf("sandbox template spec is required")
	}
	canonical, err := jcs.Transform(root.Spec)
	if err != nil {
		return "", nil, fmt.Errorf("canonicalize sandbox template spec: %w", err)
	}
	digest := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(digest[:]), canonical, nil
}

func CanonicalSandboxArtifactContractDigest(entries []SandboxArtifactContractEntry) (string, []byte, error) {
	ordered := append([]SandboxArtifactContractEntry(nil), entries...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Name < ordered[j].Name })
	encoded, err := json.Marshal(struct {
		Items []SandboxArtifactContractEntry `json:"items"`
	}{Items: ordered})
	if err != nil {
		return "", nil, err
	}
	canonical, err := jcs.Transform(encoded)
	if err != nil {
		return "", nil, fmt.Errorf("canonicalize sandbox artifact contract: %w", err)
	}
	digest := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(digest[:]), canonical, nil
}

func (c *Client) CreateSandboxTemplate(ctx context.Context, accessToken, workspaceID, idempotencyKey string, manifest SandboxManifest) (SandboxTemplateEnvelope, error) {
	var out SandboxTemplateEnvelope
	err := c.sandboxJSON(ctx, http.MethodPost, "/v1/workspaces/"+url.PathEscape(workspaceID)+"/sandbox-templates", accessToken, idempotencyKey, nil, manifest, &out, http.StatusCreated)
	return out, err
}
func (c *Client) ListSandboxTemplates(ctx context.Context, accessToken, workspaceID, cursor string) (SandboxTemplateList, error) {
	var out SandboxTemplateList
	q := url.Values{}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	err := c.sandboxJSON(ctx, http.MethodGet, "/v1/workspaces/"+url.PathEscape(workspaceID)+"/sandbox-templates", accessToken, "", q, nil, &out, http.StatusOK)
	return out, err
}
func (c *Client) GetSandboxTemplate(ctx context.Context, accessToken, templateID string) (SandboxTemplateEnvelope, error) {
	var out SandboxTemplateEnvelope
	err := c.sandboxJSON(ctx, http.MethodGet, "/v1/sandbox-templates/"+url.PathEscape(templateID), accessToken, "", nil, nil, &out, http.StatusOK)
	return out, err
}
func (c *Client) ReplaceSandboxTemplateDraft(ctx context.Context, accessToken, templateID, idempotencyKey string, request ReplaceSandboxTemplateDraftRequest) (SandboxTemplateEnvelope, error) {
	var out SandboxTemplateEnvelope
	err := c.sandboxJSON(ctx, http.MethodPut, "/v1/sandbox-templates/"+url.PathEscape(templateID)+"/draft", accessToken, idempotencyKey, nil, request, &out, http.StatusOK)
	return out, err
}
func (c *Client) PublishSandboxTemplateVersion(ctx context.Context, accessToken, templateID, idempotencyKey string, request PublishSandboxTemplateVersionRequest) (SandboxTemplateVersionEnvelope, error) {
	var out SandboxTemplateVersionEnvelope
	err := c.sandboxJSON(ctx, http.MethodPost, "/v1/sandbox-templates/"+url.PathEscape(templateID)+"/versions", accessToken, idempotencyKey, nil, request, &out, http.StatusCreated)
	return out, err
}
func (c *Client) ListSandboxTemplateVersions(ctx context.Context, accessToken, templateID, cursor string) (SandboxTemplateVersionList, error) {
	var out SandboxTemplateVersionList
	q := url.Values{}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	err := c.sandboxJSON(ctx, http.MethodGet, "/v1/sandbox-templates/"+url.PathEscape(templateID)+"/versions", accessToken, "", q, nil, &out, http.StatusOK)
	return out, err
}
func (c *Client) GetSandboxTemplateVersion(ctx context.Context, accessToken, versionID string) (SandboxTemplateVersionEnvelope, error) {
	var out SandboxTemplateVersionEnvelope
	err := c.sandboxJSON(ctx, http.MethodGet, "/v1/sandbox-template-versions/"+url.PathEscape(versionID), accessToken, "", nil, nil, &out, http.StatusOK)
	return out, err
}
func (c *Client) CreateSandbox(ctx context.Context, accessToken, workspaceID, idempotencyKey string, request CreateSandboxRequest) (SandboxMutation, error) {
	var out SandboxMutation
	if !request.ApprovedNonSensitive {
		return out, fmt.Errorf("approvedNonSensitive must be true: %s", SandboxIsolationNotice)
	}
	err := c.sandboxJSON(ctx, http.MethodPost, "/v1/workspaces/"+url.PathEscape(workspaceID)+"/sandboxes", accessToken, idempotencyKey, nil, request, &out, http.StatusAccepted)
	return out, err
}
func (c *Client) ListSandboxes(ctx context.Context, accessToken, workspaceID, cursor string) (SandboxList, error) {
	var out SandboxList
	q := url.Values{}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	err := c.sandboxJSON(ctx, http.MethodGet, "/v1/workspaces/"+url.PathEscape(workspaceID)+"/sandboxes", accessToken, "", q, nil, &out, http.StatusOK)
	return out, err
}
func (c *Client) GetSandbox(ctx context.Context, accessToken, sandboxID string) (Sandbox, error) {
	var out Sandbox
	err := c.sandboxJSON(ctx, http.MethodGet, "/v1/sandboxes/"+url.PathEscape(sandboxID), accessToken, "", nil, nil, &out, http.StatusOK)
	return out, err
}
func (c *Client) CreateSandboxOperation(ctx context.Context, accessToken, sandboxID, idempotencyKey string, request CreateSandboxOperationRequest) (SandboxMutation, error) {
	var out SandboxMutation
	err := c.sandboxJSON(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(sandboxID)+"/operations", accessToken, idempotencyKey, nil, request, &out, http.StatusAccepted)
	return out, err
}
func (c *Client) CreateSandboxAccessGrant(ctx context.Context, accessToken, sandboxID string, request CreateSandboxAccessGrantRequest) (SandboxAccessGrantCreated, error) {
	var out SandboxAccessGrantCreated
	err := c.sandboxJSON(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(sandboxID)+"/access-grants", accessToken, "", nil, request, &out, http.StatusCreated)
	return out, err
}
func (c *Client) GetSandboxOperation(ctx context.Context, accessToken, operationID string) (SandboxOperation, error) {
	var out SandboxOperation
	err := c.sandboxJSON(ctx, http.MethodGet, "/v1/sandbox-operations/"+url.PathEscape(operationID), accessToken, "", nil, nil, &out, http.StatusOK)
	return out, err
}
func (c *Client) ListSandboxArtifacts(ctx context.Context, accessToken, sandboxID, cursor string) (SandboxArtifactList, error) {
	var out SandboxArtifactList
	q := url.Values{}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	err := c.sandboxJSON(ctx, http.MethodGet, "/v1/sandboxes/"+url.PathEscape(sandboxID)+"/artifacts", accessToken, "", q, nil, &out, http.StatusOK)
	return out, err
}
func (c *Client) GetSandboxArtifact(ctx context.Context, accessToken, artifactID string) (SandboxArtifact, error) {
	var out SandboxArtifact
	err := c.sandboxJSON(ctx, http.MethodGet, "/v1/sandbox-artifacts/"+url.PathEscape(artifactID), accessToken, "", nil, nil, &out, http.StatusOK)
	return out, err
}

func (c *Client) StreamSandboxEvents(ctx context.Context, accessToken, sandboxID, lastEventID string) (*SandboxEventStream, error) {
	endpoint := c.sandboxEndpoint("/v1/sandboxes/"+url.PathEscape(sandboxID)+"/events", nil)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "text/event-stream")
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, decodeSandboxAPIError(resp)
	}
	if !strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		resp.Body.Close()
		return nil, fmt.Errorf("sandbox event API returned unsupported content type")
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	return &SandboxEventStream{body: resp.Body, scanner: scanner}, nil
}

type SandboxEventStream struct {
	body    io.ReadCloser
	scanner *bufio.Scanner
}

func (s *SandboxEventStream) Close() error { return s.body.Close() }
func (s *SandboxEventStream) Next() (SandboxEvent, error) {
	var event SandboxEvent
	var eventID string
	var data strings.Builder
	for s.scanner.Scan() {
		line := s.scanner.Text()
		if line == "" {
			if data.Len() == 0 {
				continue
			}
			if err := json.Unmarshal([]byte(data.String()), &event); err != nil {
				return event, fmt.Errorf("decode sandbox event: %w", err)
			}
			if eventID == "" || event.EventID != eventID {
				return event, fmt.Errorf("sandbox SSE id does not match typed eventId")
			}
			return event, nil
		}
		if strings.HasPrefix(line, "id:") {
			eventID = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
		}
		if strings.HasPrefix(line, "data:") {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := s.scanner.Err(); err != nil {
		return event, err
	}
	return event, io.EOF
}

func (c *Client) ExecuteSandboxGrant(ctx context.Context, grantID, grantToken string, request SandboxExecRequest) (SandboxExecResult, error) {
	var out SandboxExecResult
	err := c.sandboxGrantJSON(ctx, http.MethodPost, "/v1/sandbox-access-grants/"+url.PathEscape(grantID)+"/exec", grantToken, request, &out)
	return out, err
}

func (c *Client) UploadSandboxGrantFile(ctx context.Context, grantID, grantToken, sandboxPath, digest string, content io.Reader, size int64) (SandboxFileTransferResult, error) {
	var out SandboxFileTransferResult
	if size < 0 || size > SandboxMaxFileBytes {
		return out, fmt.Errorf("sandbox upload size must be between 0 and %d bytes", SandboxMaxFileBytes)
	}
	if !validSandboxTransferPath(sandboxPath) {
		return out, fmt.Errorf("sandbox transfer path is not confined")
	}
	if !validSandboxDigest(digest) {
		return out, fmt.Errorf("sandbox upload digest is invalid")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.sandboxEndpoint("/v1/sandbox-access-grants/"+url.PathEscape(grantID)+"/file", nil), io.LimitReader(content, size))
	if err != nil {
		return out, err
	}
	req.ContentLength = size
	req.Header.Set("Authorization", "Blazn-Grant "+grantToken)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Blazn-Sandbox-Path", sandboxPath)
	req.Header.Set("X-Content-Size", strconv.FormatInt(size, 10))
	req.Header.Set("X-Content-SHA256", digest)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return out, decodeSandboxAPIError(resp)
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out)
	return out, err
}

func (c *Client) DownloadSandboxGrantFile(ctx context.Context, grantID, grantToken, sandboxPath string) (io.ReadCloser, int64, string, error) {
	if !validSandboxTransferPath(sandboxPath) {
		return nil, 0, "", fmt.Errorf("sandbox transfer path is not confined")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.sandboxEndpoint("/v1/sandbox-access-grants/"+url.PathEscape(grantID)+"/file", nil), nil)
	if err != nil {
		return nil, 0, "", err
	}
	req.Header.Set("Authorization", "Blazn-Grant "+grantToken)
	req.Header.Set("X-Blazn-Sandbox-Path", sandboxPath)
	req.Header.Set("Accept", "application/octet-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, "", err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, 0, "", decodeSandboxAPIError(resp)
	}
	return verifiedSandboxDownload(resp, SandboxMaxFileBytes)
}
func (c *Client) DownloadSandboxArtifact(ctx context.Context, accessToken, artifactID string) (io.ReadCloser, int64, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.sandboxEndpoint("/v1/sandbox-artifacts/"+url.PathEscape(artifactID)+"/content", nil), nil)
	if err != nil {
		return nil, 0, "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/octet-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, "", err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, 0, "", decodeSandboxAPIError(resp)
	}
	return verifiedSandboxDownload(resp, -1)
}

type sandboxVerifiedReadCloser struct {
	body           io.ReadCloser
	digest         hash.Hash
	expectedSize   int64
	expectedDigest string
	maximum        int64
	read           int64
	complete       bool
}

func (r *sandboxVerifiedReadCloser) Read(p []byte) (int, error) {
	if r.complete {
		return 0, io.EOF
	}
	n, err := r.body.Read(p)
	if n > 0 {
		r.read += int64(n)
		_, _ = r.digest.Write(p[:n])
		if (r.maximum >= 0 && r.read > r.maximum) || r.read > r.expectedSize {
			return 0, fmt.Errorf("sandbox download exceeded declared size")
		}
	}
	if err == io.EOF {
		r.complete = true
		if r.read != r.expectedSize {
			return n, fmt.Errorf("sandbox download size %d does not match declared %d", r.read, r.expectedSize)
		}
		got := "sha256:" + hex.EncodeToString(r.digest.Sum(nil))
		if got != r.expectedDigest {
			return n, fmt.Errorf("sandbox download digest mismatch")
		}
	}
	return n, err
}
func (r *sandboxVerifiedReadCloser) Close() error { return r.body.Close() }
func verifiedSandboxDownload(resp *http.Response, maximum int64) (io.ReadCloser, int64, string, error) {
	mediaType := strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0])
	if !strings.EqualFold(mediaType, "application/octet-stream") {
		resp.Body.Close()
		return nil, 0, "", fmt.Errorf("sandbox download has invalid content type")
	}
	size, err := strconv.ParseInt(resp.Header.Get("X-Content-Size"), 10, 64)
	if err != nil || size < 0 || (maximum >= 0 && size > maximum) {
		resp.Body.Close()
		return nil, 0, "", fmt.Errorf("sandbox download has invalid X-Content-Size")
	}
	digest := resp.Header.Get("X-Content-SHA256")
	if !validSandboxDigest(digest) {
		resp.Body.Close()
		return nil, 0, "", fmt.Errorf("sandbox download has invalid X-Content-SHA256")
	}
	return &sandboxVerifiedReadCloser{body: resp.Body, digest: sha256.New(), expectedSize: size, expectedDigest: digest, maximum: maximum}, size, digest, nil
}
func validSandboxTransferPath(value string) bool {
	if strings.Contains(value, "\\") || strings.Contains(value, "//") || strings.Contains(value, "/../") || strings.Contains(value, "/./") || strings.HasSuffix(value, "/..") || strings.HasSuffix(value, "/.") {
		return false
	}
	for _, prefix := range []string{"/workspace/src/", "/workspace/artifacts/", "/workspace/tmp/"} {
		if strings.HasPrefix(value, prefix) && len(value) > len(prefix) {
			return true
		}
	}
	return false
}
func validSandboxDigest(value string) bool {
	if len(value) != 71 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

func (c *Client) sandboxGrantJSON(ctx context.Context, method, path, grantToken string, input, output any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.sandboxEndpoint(path, nil), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Blazn-Grant "+grantToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return decodeSandboxAPIError(resp)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(output)
}

func (c *Client) sandboxJSON(ctx context.Context, method, path, accessToken, idempotencyKey string, query url.Values, input, output any, success int) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.sandboxEndpoint(path, query), body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != success {
		return decodeSandboxAPIError(resp)
	}
	if output == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(output)
}
func (c *Client) sandboxEndpoint(path string, query url.Values) string {
	base := *c.baseURL
	base.Path = strings.TrimSuffix(base.Path, "/") + path
	base.RawQuery = query.Encode()
	return base.String()
}
func decodeSandboxAPIError(resp *http.Response) error {
	apiErr := &APIError{StatusCode: resp.StatusCode}
	if retry := resp.Header.Get("Retry-After"); retry != "" {
		apiErr.RetryAfter, _ = strconv.Atoi(retry)
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&apiErr.Body)
	return apiErr
}
