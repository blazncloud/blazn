package sandboxcontroller

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/blazncloud/blazn/internal/sandboxcontrol"
)

const (
	claimSQL    = "SELECT row_to_json(claimed)::text FROM public.sandbox_controller_claim_v2($1,$2) claimed"
	renewSQL    = "SELECT public.sandbox_controller_renew($1,$2,$3,$4)"
	bindSQL     = "SELECT public.sandbox_controller_bind_backend_v2($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)"
	retrySQL    = "SELECT public.sandbox_controller_retry($1,$2,$3,$4,$5,$6,$7)"
	completeSQL = "SELECT public.sandbox_controller_complete_v2($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12::uuid[],$13::text[],$14,$15,$16)"
	expirySQL   = "SELECT count(*) FROM public.sandbox_controller_enqueue_expired($1)"
)

var sha256Pattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type sqlRow interface {
	Scan(...any) error
}

type pgExecutor interface {
	QueryRow(context.Context, string, ...any) sqlRow
	Close() error
}

type databaseExecutor struct{ database *sql.DB }

func (e databaseExecutor) QueryRow(ctx context.Context, query string, args ...any) sqlRow {
	return e.database.QueryRowContext(ctx, query, args...)
}

func (e databaseExecutor) Close() error { return e.database.Close() }

// PgStore is the least-privilege PostgreSQL implementation of Store. Its
// connection must authenticate as blazn_sandbox_controller; every mutation is
// performed through the fenced SECURITY DEFINER procedures.
type PgStore struct{ executor pgExecutor }

func NewPgStore(database *sql.DB) (*PgStore, error) {
	if database == nil {
		return nil, errors.New("sandbox controller database is required")
	}
	return &PgStore{executor: databaseExecutor{database: database}}, nil
}

func (s *PgStore) Health(ctx context.Context) error {
	var authorized bool
	if err := s.executor.QueryRow(ctx, "SELECT current_user = 'blazn_sandbox_controller'").Scan(&authorized); err != nil {
		return err
	}
	if !authorized {
		return errors.New("sandbox controller database role is unauthorized")
	}
	return nil
}

func (s *PgStore) Claim(ctx context.Context, workerID string, leaseSeconds int) (*WorkItem, error) {
	var payload string
	err := s.executor.QueryRow(ctx, claimSQL, workerID, leaseSeconds).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeWorkItem([]byte(payload))
}

func (s *PgStore) Renew(ctx context.Context, operationID, workerID, leaseToken string, leaseSeconds int) (time.Time, bool, error) {
	var renewed sql.NullTime
	if err := s.executor.QueryRow(ctx, renewSQL, operationID, workerID, leaseToken, leaseSeconds).Scan(&renewed); err != nil {
		return time.Time{}, false, err
	}
	return renewed.Time, renewed.Valid, nil
}

func (s *PgStore) BindBackend(ctx context.Context, operationID, workerID, leaseToken string, record sandboxcontrol.SandboxRecord, admission sandboxcontrol.WorkloadIdentity) (bool, error) {
	digest, err := rawDigest(admission.Digest)
	if err != nil {
		return false, err
	}
	var bound bool
	err = s.executor.QueryRow(ctx, bindSQL, operationID, workerID, leaseToken,
		record.UID, record.ResourceVersion, admission.APIVersion, admission.Namespace,
		admission.Name, admission.UID, admission.ResourceVersion, admission.ClusterQueue,
		admission.Owner.APIVersion, admission.Owner.Kind, admission.Owner.Name,
		admission.Owner.UID, admission.Owner.Controller, admission.WorkspaceID,
		admission.SandboxID, admission.Admitted, admission.Condition.Type,
		admission.Condition.Status, digest).Scan(&bound)
	return bound, err
}

func (s *PgStore) Retry(ctx context.Context, operationID, workerID, leaseToken string, delaySeconds int, safe SafeError) (RetryOutcome, error) {
	var outcome string
	if err := s.executor.QueryRow(ctx, retrySQL, operationID, workerID, leaseToken, delaySeconds,
		safe.Code, safe.Message, safe.RequestID).Scan(&outcome); err != nil {
		return "", err
	}
	result := RetryOutcome(outcome)
	if result != RetryScheduled && result != RecoveryRequired && result != Fenced {
		return "", fmt.Errorf("sandbox controller retry returned invalid outcome %q", outcome)
	}
	return result, nil
}

func (s *PgStore) Complete(ctx context.Context, operationID, workerID, leaseToken string, completion Completion) (bool, error) {
	var admissionDigest any
	if completion.ExpectedAdmissionDigest != nil {
		digest, err := rawDigest(*completion.ExpectedAdmissionDigest)
		if err != nil {
			return false, err
		}
		admissionDigest = digest
	}
	var errorCode, errorMessage, errorRequestID any
	if completion.Error != nil {
		errorCode, errorMessage, errorRequestID = completion.Error.Code, completion.Error.Message, completion.Error.RequestID
	}
	var completed bool
	err := s.executor.QueryRow(ctx, completeSQL, operationID, workerID, leaseToken,
		completion.Status, completion.ExpectedBackendUID, completion.ExpectedBackendResourceVersion,
		admissionDigest, completion.CleanupComplete, completion.ArtifactExportComplete,
		completion.GrantsRevoked, completion.BackendDestroyed, completion.ArtifactIDs,
		completion.WarningCodes, errorCode, errorMessage, errorRequestID).Scan(&completed)
	return completed, err
}

func (s *PgStore) EnqueueExpired(ctx context.Context, limit int) (int, error) {
	var count int
	err := s.executor.QueryRow(ctx, expirySQL, limit).Scan(&count)
	return count, err
}

func (s *PgStore) Close() error { return s.executor.Close() }

type workItemRow struct {
	OperationID             string    `json:"operation_id"`
	WorkspaceID             string    `json:"workspace_id"`
	SandboxID               string    `json:"sandbox_id"`
	RequestedBy             string    `json:"requested_by"`
	OperationType           string    `json:"operation_type"`
	ExpectedSandboxVersion  int64     `json:"expected_sandbox_version"`
	LeaseToken              string    `json:"lease_token"`
	LeaseExpiresAt          time.Time `json:"lease_expires_at"`
	Attempt                 int       `json:"attempt"`
	AllocationMode          string    `json:"allocation_mode"`
	DesiredState            string    `json:"desired_state"`
	Architecture            string    `json:"architecture"`
	TemplateVersionID       string    `json:"template_version_id"`
	TemplateDigest          string    `json:"template_digest"`
	VariantName             string    `json:"variant_name"`
	ImageIndexDigest        string    `json:"image_index_digest"`
	ImageDigest             string    `json:"image_child_digest"`
	PlacementProfile        string    `json:"placement_profile"`
	Command                 []string  `json:"command"`
	RequestCPU              string    `json:"request_cpu"`
	RequestMemory           string    `json:"request_memory"`
	RequestEphemeral        string    `json:"request_ephemeral_storage"`
	LimitCPU                string    `json:"limit_cpu"`
	LimitMemory             string    `json:"limit_memory"`
	LimitEphemeral          string    `json:"limit_ephemeral_storage"`
	QueueName               string    `json:"queue_name"`
	AdmissionID             *string   `json:"admission_id"`
	BackendUID              *string   `json:"backend_uid"`
	BackendResourceVersion  *string   `json:"backend_resource_version"`
	ExpiresAt               time.Time `json:"expires_at"`
	SourceNames             []string  `json:"source_names"`
	SourceURLs              []string  `json:"source_urls"`
	SourceDestinations      []string  `json:"source_destinations"`
	SourceWritable          []bool    `json:"source_writable"`
	SourceCommits           []string  `json:"source_commits"`
	ArtifactNames           []string  `json:"artifact_names"`
	ArtifactPaths           []string  `json:"artifact_paths"`
	ArtifactMediaTypes      []string  `json:"artifact_media_types"`
	ArtifactRequired        []bool    `json:"artifact_required"`
	AdmissionDigest         *string   `json:"admission_digest"`
	WorkloadAPIVersion      *string   `json:"workload_api_version"`
	WorkloadNamespace       *string   `json:"workload_namespace"`
	WorkloadName            *string   `json:"workload_name"`
	WorkloadUID             *string   `json:"workload_uid"`
	WorkloadResourceVersion *string   `json:"workload_resource_version"`
	AdmittedClusterQueue    *string   `json:"admitted_cluster_queue"`
	OwnerAPIVersion         *string   `json:"owner_api_version"`
	OwnerKind               *string   `json:"owner_kind"`
	OwnerName               *string   `json:"owner_name"`
	OwnerUID                *string   `json:"owner_uid"`
	OwnerController         *bool     `json:"owner_controller"`
	WorkspaceLabel          *string   `json:"workspace_label"`
	SandboxLabel            *string   `json:"sandbox_label"`
	Admitted                *bool     `json:"admitted"`
	ConditionType           *string   `json:"condition_type"`
	ConditionStatus         *string   `json:"condition_status"`
}

func decodeWorkItem(payload []byte) (*WorkItem, error) {
	var row workItemRow
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&row); err != nil {
		return nil, fmt.Errorf("decode sandbox controller claim: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("sandbox controller claim contains trailing data")
	}
	if len(row.SourceNames) != len(row.SourceURLs) || len(row.SourceNames) != len(row.SourceDestinations) ||
		len(row.SourceNames) != len(row.SourceWritable) || len(row.SourceNames) != len(row.SourceCommits) {
		return nil, errors.New("sandbox controller source columns are inconsistent")
	}
	if len(row.ArtifactNames) != len(row.ArtifactPaths) || len(row.ArtifactNames) != len(row.ArtifactMediaTypes) ||
		len(row.ArtifactNames) != len(row.ArtifactRequired) {
		return nil, errors.New("sandbox controller artifact columns are inconsistent")
	}
	templateDigest := "sha256:" + strings.TrimSpace(row.TemplateDigest)
	if !sha256Pattern.MatchString(templateDigest) {
		return nil, errors.New("sandbox controller template digest is invalid")
	}
	item := &WorkItem{
		OperationID: row.OperationID, WorkspaceID: row.WorkspaceID, SandboxID: row.SandboxID,
		RequestedBy: row.RequestedBy, OperationType: row.OperationType,
		ExpectedSandboxVersion: row.ExpectedSandboxVersion, LeaseToken: row.LeaseToken,
		LeaseExpiresAt: row.LeaseExpiresAt, Attempt: row.Attempt, AllocationMode: row.AllocationMode,
		DesiredState: row.DesiredState, Architecture: row.Architecture,
		TemplateVersionID: row.TemplateVersionID, TemplateDigest: templateDigest,
		VariantName: row.VariantName, ImageIndexDigest: row.ImageIndexDigest,
		ImageDigest: row.ImageDigest, PlacementProfile: row.PlacementProfile,
		Command: append([]string(nil), row.Command...),
		Resources: Resources{CPURequest: row.RequestCPU, MemoryRequest: row.RequestMemory,
			EphemeralRequest: row.RequestEphemeral, CPULimit: row.LimitCPU,
			MemoryLimit: row.LimitMemory, EphemeralLimit: row.LimitEphemeral},
		QueueName: row.QueueName, AdmissionID: row.AdmissionID, BackendUID: row.BackendUID,
		BackendResourceVersion: row.BackendResourceVersion, ExpiresAt: row.ExpiresAt,
	}
	for index, name := range row.SourceNames {
		item.Sources = append(item.Sources, Source{Name: name, URL: row.SourceURLs[index],
			Destination: row.SourceDestinations[index], Writable: row.SourceWritable[index],
			Commit: row.SourceCommits[index]})
	}
	for index, name := range row.ArtifactNames {
		item.Artifacts = append(item.Artifacts, Artifact{Name: name, Path: row.ArtifactPaths[index],
			MediaType: row.ArtifactMediaTypes[index], Required: row.ArtifactRequired[index]})
	}
	admission, err := decodeAdmission(row)
	if err != nil {
		return nil, err
	}
	item.Admission = admission
	return item, nil
}

func decodeAdmission(row workItemRow) (*sandboxcontrol.WorkloadIdentity, error) {
	if row.AdmissionDigest == nil {
		if row.WorkloadAPIVersion != nil || row.WorkloadNamespace != nil || row.WorkloadName != nil ||
			row.WorkloadUID != nil || row.WorkloadResourceVersion != nil || row.AdmittedClusterQueue != nil ||
			row.OwnerAPIVersion != nil || row.OwnerKind != nil || row.OwnerName != nil || row.OwnerUID != nil ||
			row.OwnerController != nil || row.WorkspaceLabel != nil || row.SandboxLabel != nil || row.Admitted != nil ||
			row.ConditionType != nil || row.ConditionStatus != nil {
			return nil, errors.New("sandbox controller admission identity is partially populated")
		}
		return nil, nil
	}
	requiredStrings := []*string{row.WorkloadAPIVersion, row.WorkloadNamespace, row.WorkloadName,
		row.WorkloadUID, row.WorkloadResourceVersion, row.AdmittedClusterQueue, row.OwnerAPIVersion,
		row.OwnerKind, row.OwnerName, row.OwnerUID, row.WorkspaceLabel, row.SandboxLabel,
		row.ConditionType, row.ConditionStatus}
	for _, value := range requiredStrings {
		if value == nil || *value == "" {
			return nil, errors.New("sandbox controller admission identity is incomplete")
		}
	}
	if row.OwnerController == nil || row.Admitted == nil {
		return nil, errors.New("sandbox controller admission identity is incomplete")
	}
	digest := "sha256:" + strings.TrimSpace(*row.AdmissionDigest)
	identity := &sandboxcontrol.WorkloadIdentity{
		APIVersion: *row.WorkloadAPIVersion, Namespace: *row.WorkloadNamespace,
		Name: *row.WorkloadName, UID: *row.WorkloadUID,
		ResourceVersion: *row.WorkloadResourceVersion, ClusterQueue: *row.AdmittedClusterQueue,
		Owner: sandboxcontrol.SandboxOwnerReference{APIVersion: *row.OwnerAPIVersion,
			Kind: *row.OwnerKind, Name: *row.OwnerName, UID: *row.OwnerUID,
			Controller: *row.OwnerController},
		WorkspaceID: *row.WorkspaceLabel, SandboxID: *row.SandboxLabel, Admitted: *row.Admitted,
		Condition: sandboxcontrol.AdmissionCondition{Type: *row.ConditionType, Status: *row.ConditionStatus},
		Digest:    digest,
	}
	if identity.APIVersion != "kueue.x-k8s.io/v1beta1" || identity.Namespace != sandboxcontrol.Namespace ||
		identity.Owner.APIVersion != "agents.x-k8s.io/v1beta1" || identity.Owner.Kind != "Sandbox" ||
		!identity.Owner.Controller || !identity.Admitted || identity.Condition.Type != "Admitted" ||
		identity.Condition.Status != "True" || identity.WorkspaceID != row.WorkspaceID ||
		identity.SandboxID != row.SandboxID || identity.Owner.Name != row.SandboxID ||
		row.BackendUID == nil || identity.Owner.UID != *row.BackendUID || row.AdmissionID == nil ||
		identity.UID != *row.AdmissionID || !sha256Pattern.MatchString(identity.Digest) {
		return nil, errors.New("sandbox controller admission identity is inconsistent")
	}
	claimedDigest := identity.Digest
	record := sandboxcontrol.SandboxRecord{Name: row.SandboxID, Namespace: sandboxcontrol.Namespace,
		UID: identity.Owner.UID, ResourceVersion: "admission-check", WorkspaceID: row.WorkspaceID,
		OwnerID: row.RequestedBy, QueueName: sandboxcontrol.QueueName, State: sandboxcontrol.StateReady,
		ArtifactContractDigest: "sha256:0000000000000000000000000000000000000000000000000000000000000000"}
	receipt, err := sandboxcontrol.NewReceipt("controller-admission-check", sandboxcontrol.OperationCreate, record, nil, time.Unix(1, 0))
	if err != nil {
		return nil, errors.New("sandbox controller admission identity cannot be verified")
	}
	identity.Digest = ""
	bound, err := sandboxcontrol.AttachAdmissionIdentity(receipt, *identity)
	if err != nil || bound.Admission == nil || bound.Admission.Digest != claimedDigest {
		return nil, errors.New("sandbox controller admission digest is inconsistent")
	}
	identity.Digest = claimedDigest
	return identity, nil
}

func rawDigest(value string) (string, error) {
	if !sha256Pattern.MatchString(value) {
		return "", errors.New("sandbox controller admission digest is invalid")
	}
	return strings.TrimPrefix(value, "sha256:"), nil
}
