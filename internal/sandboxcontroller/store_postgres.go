package sandboxcontroller

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/blazncloud/blazn/internal/sandboxcontrol"
	"github.com/blazncloud/blazn/internal/sandboxio"
)

const (
	claimSQL                  = "SELECT row_to_json(claimed)::text, clock_timestamp() FROM public.sandbox_controller_claim_v5($1,$2) claimed"
	renewSQL                  = "SELECT renewed, clock_timestamp() FROM (SELECT public.sandbox_controller_renew($1,$2,$3,$4) renewed) result"
	bindSQL                   = "SELECT public.sandbox_controller_bind_backend_v4($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32,$33,$34,$35)"
	recordSourcesSQL          = "SELECT public.sandbox_controller_record_source_materialization_v1($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb,$10::jsonb)"
	recordArtifactSQL         = "SELECT public.sandbox_controller_record_artifact_v1($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)"
	completeArtifactExportSQL = "SELECT public.sandbox_controller_complete_artifact_export_v1($1,$2,$3,$4,$5::text[])"
	retrySQL                  = "SELECT public.sandbox_controller_retry($1,$2,$3,$4,$5,$6,$7)"
	completeSQL               = "SELECT public.sandbox_controller_complete_v5($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13::uuid[],$14::text[],$15,$16,$17)"
	expirySQL                 = "SELECT count(*) FROM public.sandbox_controller_enqueue_expired($1)"
	consumeAccessGrantSQL     = "SELECT workspace_id,sandbox_id,requested_by,backend_uid,backend_resource_version FROM public.sandbox_controller_consume_access_grant_v1($1,$2,$3)"
)

var sha256Pattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var artifactWarningPattern = regexp.MustCompile(`^optional_artifact_missing_[a-z0-9](?:[a-z0-9_]{0,61}[a-z0-9])?$`)

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

type AccessGrantBinding struct {
	WorkspaceID, SandboxID, RequestedBy, BackendUID, BackendResourceVersion string
}

func (s *PgStore) ConsumeAccessGrant(ctx context.Context, grantID, tokenHash, kind string) (AccessGrantBinding, bool, error) {
	var binding AccessGrantBinding
	err := s.executor.QueryRow(ctx, consumeAccessGrantSQL, grantID, tokenHash, kind).Scan(
		&binding.WorkspaceID, &binding.SandboxID, &binding.RequestedBy, &binding.BackendUID, &binding.BackendResourceVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return AccessGrantBinding{}, false, nil
	}
	if err != nil {
		return AccessGrantBinding{}, false, err
	}
	return binding, true, nil
}

func NewPgStore(database *sql.DB) (*PgStore, error) {
	if database == nil {
		return nil, errors.New("sandbox controller database is required")
	}
	return &PgStore{executor: databaseExecutor{database: database}}, nil
}

func (s *PgStore) Health(ctx context.Context) error {
	var authorized bool
	// This error can surface in logs (Run wraps it and main prints it). A
	// pgconn connect failure names host/user/database but never the
	// password: pgx redacts credentials in every error it constructs.
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
	var databaseNow time.Time
	started := time.Now()
	err := s.executor.QueryRow(ctx, claimSQL, workerID, leaseSeconds).Scan(&payload, &databaseNow)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	item, err := decodeWorkItem([]byte(payload))
	if err != nil {
		return nil, err
	}
	window := databaseLeaseWindow(item.LeaseExpiresAt, databaseNow, started)
	item.LeaseRemaining, item.LeaseDeadline = window.Remaining, window.Deadline
	return item, nil
}

func (s *PgStore) Renew(ctx context.Context, operationID, workerID, leaseToken string, leaseSeconds int) (LeaseWindow, bool, error) {
	var renewed sql.NullTime
	var databaseNow time.Time
	started := time.Now()
	if err := s.executor.QueryRow(ctx, renewSQL, operationID, workerID, leaseToken, leaseSeconds).Scan(&renewed, &databaseNow); err != nil {
		return LeaseWindow{}, false, err
	}
	if !renewed.Valid {
		return LeaseWindow{}, false, nil
	}
	return databaseLeaseWindow(renewed.Time, databaseNow, started), true, nil
}

func databaseLeaseWindow(expiresAt, databaseNow, localStarted time.Time) LeaseWindow {
	localObserved := time.Now()
	remaining := databaseLeaseRemaining(expiresAt, databaseNow, localObserved.Sub(localStarted))
	return LeaseWindow{DatabaseNow: databaseNow, ExpiresAt: expiresAt, Remaining: remaining,
		Deadline: localObserved.Add(remaining)}
}

func databaseLeaseRemaining(expiresAt, databaseNow time.Time, queryElapsed time.Duration) time.Duration {
	return expiresAt.Sub(databaseNow) - queryElapsed
}

func (s *PgStore) BindBackend(ctx context.Context, operationID, workerID, leaseToken string, observation sandboxcontrol.AdmissionObservation) (bool, error) {
	workloadDigest, err := rawDigest(observation.Workload.Digest)
	if err != nil {
		return false, err
	}
	observationDigest, err := rawDigest(observation.Digest)
	if err != nil {
		return false, err
	}
	var bound bool
	err = s.executor.QueryRow(ctx, bindSQL, operationID, workerID, leaseToken,
		observation.Sandbox.UID, observation.Sandbox.ResourceVersion,
		observation.Sandbox.APIVersion, observation.Sandbox.Kind, observation.Sandbox.Namespace,
		observation.Sandbox.Name, observation.Sandbox.UID, observation.Sandbox.ResourceVersion,
		observation.Pod.APIVersion, observation.Pod.Kind, observation.Pod.Namespace,
		observation.Pod.Name, observation.Pod.UID, observation.Pod.ResourceVersion,
		observation.Workload.APIVersion, observation.Workload.Namespace,
		observation.Workload.Name, observation.Workload.UID, observation.Workload.ResourceVersion,
		observation.Workload.ClusterQueue, observation.Workload.Owner.APIVersion,
		observation.Workload.Owner.Kind, observation.Workload.Owner.Name,
		observation.Workload.Owner.UID, observation.Workload.Owner.Controller,
		observation.Workload.WorkspaceID, observation.Workload.SandboxID,
		observation.Workload.Admitted, observation.Workload.Condition.Type,
		observation.Workload.Condition.Status, workloadDigest, observationDigest).Scan(&bound)
	return bound, err
}

func (s *PgStore) RecordSources(ctx context.Context, operationID, workerID, leaseToken string, observation sandboxcontrol.AdmissionObservation, receipt sandboxio.SourceMaterializationReceipt) (bool, error) {
	manifest := sandboxio.SourceManifest{SchemaVersion: sandboxio.SourceManifestVersion, Sources: make([]sandboxio.Source, len(receipt.Sources))}
	for index, source := range receipt.Sources {
		manifest.Sources[index] = sandboxio.Source{Name: source.Name, URL: source.URL, Destination: source.Destination, Commit: source.Commit, Writable: source.Writable}
	}
	if err := sandboxio.ValidateSourceMaterializationReceipt(receipt, &manifest); err != nil {
		return false, err
	}
	manifestDigest, err := rawDigest(receipt.ManifestDigest)
	if err != nil {
		return false, err
	}
	receiptDigest, err := rawDigest(receipt.Digest)
	if err != nil {
		return false, err
	}
	observationDigest, err := rawDigest(observation.Digest)
	if err != nil {
		return false, err
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return false, err
	}
	encodedObservation, err := json.Marshal(observation)
	if err != nil {
		return false, err
	}
	var recorded bool
	err = s.executor.QueryRow(ctx, recordSourcesSQL, operationID, workerID, leaseToken,
		observation.Sandbox.UID, observation.Sandbox.ResourceVersion, observationDigest,
		manifestDigest, receiptDigest, string(encoded), string(encodedObservation)).Scan(&recorded)
	return recorded, err
}

func (s *PgStore) RecordArtifact(ctx context.Context, operationID, workerID, leaseToken string, observation sandboxcontrol.AdmissionObservation, artifact PersistedArtifact) (PersistedArtifact, bool, error) {
	if artifact.ID != "" || artifact.ExportedAt != "" || !artifactNamePattern.MatchString(artifact.Name) || !validWorkspaceArtifactPath(artifact.Path) ||
		!mediaTypePattern.MatchString(artifact.MediaType) || !sha256Pattern.MatchString(artifact.Digest) || artifact.Size < 0 || artifact.Size > maxArtifactBytes {
		return PersistedArtifact{}, false, errors.New("sandbox artifact persistence input is invalid")
	}
	wantedKey, err := ArtifactObjectKey(observation.Workload.WorkspaceID, observation.Sandbox.Name, artifact.Name)
	if err != nil || artifact.ObjectKey != wantedKey {
		return PersistedArtifact{}, false, errors.New("sandbox artifact object key is invalid")
	}
	workloadDigest, err := rawDigest(observation.Workload.Digest)
	if err != nil {
		return PersistedArtifact{}, false, err
	}
	observationDigest, err := rawDigest(observation.Digest)
	if err != nil {
		return PersistedArtifact{}, false, err
	}
	contentDigest, err := rawDigest(artifact.Digest)
	if err != nil {
		return PersistedArtifact{}, false, err
	}
	var id sql.NullString
	var exported sql.NullTime
	err = s.executor.QueryRow(ctx, recordArtifactSQL, operationID, workerID, leaseToken,
		observation.Sandbox.UID, observation.Sandbox.ResourceVersion, workloadDigest, observationDigest,
		artifact.Name, artifact.Path, artifact.MediaType, contentDigest, artifact.Size, artifact.ObjectKey).Scan(&id, &exported)
	if errors.Is(err, sql.ErrNoRows) {
		return PersistedArtifact{}, false, nil
	}
	if err != nil || !id.Valid || !exported.Valid {
		return PersistedArtifact{}, false, err
	}
	artifact.ID, artifact.ExportedAt = id.String, exported.Time.UTC().Format(time.RFC3339Nano)
	return artifact, true, nil
}

func (s *PgStore) CompleteArtifactExport(ctx context.Context, operationID, workerID, leaseToken string, observation sandboxcontrol.AdmissionObservation, warnings []string) (bool, error) {
	observationDigest, err := rawDigest(observation.Digest)
	if err != nil {
		return false, err
	}
	if len(warnings) > 32 {
		return false, errors.New("artifact export warnings are invalid")
	}
	if warnings == nil {
		warnings = []string{}
	}
	for index, warning := range warnings {
		if !artifactWarningPattern.MatchString(warning) || index > 0 && warnings[index-1] >= warning {
			return false, errors.New("artifact export warnings are invalid")
		}
	}
	var completed bool
	err = s.executor.QueryRow(ctx, completeArtifactExportSQL, operationID, workerID, leaseToken, observationDigest, warnings).Scan(&completed)
	return completed, err
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
	var workloadDigest any
	if completion.ExpectedWorkloadDigest != nil {
		digest, err := rawDigest(*completion.ExpectedWorkloadDigest)
		if err != nil {
			return false, err
		}
		workloadDigest = digest
	}
	var observationDigest any
	if completion.ExpectedObservationDigest != nil {
		digest, err := rawDigest(*completion.ExpectedObservationDigest)
		if err != nil {
			return false, err
		}
		observationDigest = digest
	}
	var errorCode, errorMessage, errorRequestID any
	if completion.Error != nil {
		errorCode, errorMessage, errorRequestID = completion.Error.Code, completion.Error.Message, completion.Error.RequestID
	}
	var completed bool
	err := s.executor.QueryRow(ctx, completeSQL, operationID, workerID, leaseToken,
		completion.Status, completion.ExpectedBackendUID, completion.ExpectedBackendResourceVersion,
		workloadDigest, observationDigest, completion.CleanupComplete, completion.ArtifactExportComplete,
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
	OperationID                string          `json:"operation_id"`
	WorkspaceID                string          `json:"workspace_id"`
	SandboxID                  string          `json:"sandbox_id"`
	RequestedBy                string          `json:"requested_by"`
	OperationType              string          `json:"operation_type"`
	ExpectedSandboxVersion     int64           `json:"expected_sandbox_version"`
	LeaseToken                 string          `json:"lease_token"`
	LeaseExpiresAt             time.Time       `json:"lease_expires_at"`
	Attempt                    int             `json:"attempt"`
	AllocationMode             string          `json:"allocation_mode"`
	DesiredState               string          `json:"desired_state"`
	Architecture               string          `json:"architecture"`
	TemplateVersionID          string          `json:"template_version_id"`
	TemplateDigest             string          `json:"template_digest"`
	VariantName                string          `json:"variant_name"`
	ImageIndexDigest           string          `json:"image_index_digest"`
	ImageDigest                string          `json:"image_child_digest"`
	PlacementProfile           string          `json:"placement_profile"`
	Command                    []string        `json:"command"`
	RequestCPU                 string          `json:"request_cpu"`
	RequestMemory              string          `json:"request_memory"`
	RequestEphemeral           string          `json:"request_ephemeral_storage"`
	LimitCPU                   string          `json:"limit_cpu"`
	LimitMemory                string          `json:"limit_memory"`
	LimitEphemeral             string          `json:"limit_ephemeral_storage"`
	QueueName                  string          `json:"queue_name"`
	AdmissionID                *string         `json:"admission_id"`
	BackendUID                 *string         `json:"backend_uid"`
	BackendResourceVersion     *string         `json:"backend_resource_version"`
	ExpiresAt                  time.Time       `json:"expires_at"`
	SourceNames                []string        `json:"source_names"`
	SourceURLs                 []string        `json:"source_urls"`
	SourceDestinations         []string        `json:"source_destinations"`
	SourceWritable             []bool          `json:"source_writable"`
	SourceCommits              []string        `json:"source_commits"`
	ArtifactNames              []string        `json:"artifact_names"`
	ArtifactPaths              []string        `json:"artifact_paths"`
	ArtifactMediaTypes         []string        `json:"artifact_media_types"`
	ArtifactRequired           []bool          `json:"artifact_required"`
	AdmissionDigest            *string         `json:"admission_digest"`
	WorkloadAPIVersion         *string         `json:"workload_api_version"`
	WorkloadNamespace          *string         `json:"workload_namespace"`
	WorkloadName               *string         `json:"workload_name"`
	WorkloadUID                *string         `json:"workload_uid"`
	WorkloadResourceVersion    *string         `json:"workload_resource_version"`
	AdmittedClusterQueue       *string         `json:"admitted_cluster_queue"`
	OwnerAPIVersion            *string         `json:"owner_api_version"`
	OwnerKind                  *string         `json:"owner_kind"`
	OwnerName                  *string         `json:"owner_name"`
	OwnerUID                   *string         `json:"owner_uid"`
	OwnerController            *bool           `json:"owner_controller"`
	WorkspaceLabel             *string         `json:"workspace_label"`
	SandboxLabel               *string         `json:"sandbox_label"`
	Admitted                   *bool           `json:"admitted"`
	ConditionType              *string         `json:"condition_type"`
	ConditionStatus            *string         `json:"condition_status"`
	PodAPIVersion              *string         `json:"pod_api_version"`
	PodKind                    *string         `json:"pod_kind"`
	PodNamespace               *string         `json:"pod_namespace"`
	PodName                    *string         `json:"pod_name"`
	PodUID                     *string         `json:"pod_uid"`
	PodResourceVersion         *string         `json:"pod_resource_version"`
	ObservationDigest          *string         `json:"observation_digest"`
	SourceReceipt              json.RawMessage `json:"source_materialization_receipt"`
	SourceBootstrapObservation json.RawMessage `json:"source_bootstrap_observation"`
	ExportedArtifactIDs        []string        `json:"exported_artifact_ids"`
	ExportedArtifactNames      []string        `json:"exported_artifact_names"`
	ExportedArtifactPaths      []string        `json:"exported_artifact_paths"`
	ExportedArtifactMediaTypes []string        `json:"exported_artifact_media_types"`
	ExportedArtifactDigests    []string        `json:"exported_artifact_digests"`
	ExportedArtifactSizes      []int64         `json:"exported_artifact_sizes"`
	ExportedArtifactKeys       []string        `json:"exported_artifact_keys"`
	ExportedArtifactTimes      []time.Time     `json:"exported_artifact_times"`
	ArtifactExportComplete     bool            `json:"artifact_export_complete"`
	ArtifactExportWarningCodes []string        `json:"artifact_export_warning_codes"`
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
	exportedCount := len(row.ExportedArtifactIDs)
	if len(row.ExportedArtifactNames) != exportedCount || len(row.ExportedArtifactPaths) != exportedCount ||
		len(row.ExportedArtifactMediaTypes) != exportedCount || len(row.ExportedArtifactDigests) != exportedCount ||
		len(row.ExportedArtifactSizes) != exportedCount || len(row.ExportedArtifactKeys) != exportedCount || len(row.ExportedArtifactTimes) != exportedCount {
		return nil, errors.New("sandbox controller exported artifact columns are inconsistent")
	}
	contracts := make(map[string]Artifact, len(item.Artifacts))
	for _, artifact := range item.Artifacts {
		contracts[artifact.Name] = artifact
	}
	for index, id := range row.ExportedArtifactIDs {
		name := row.ExportedArtifactNames[index]
		contract, ok := contracts[name]
		digest := "sha256:" + strings.TrimSpace(row.ExportedArtifactDigests[index])
		wantedKey, keyErr := ArtifactObjectKey(item.WorkspaceID, item.SandboxID, name)
		if !ok || !canonicalUUID(id) || index > 0 && row.ExportedArtifactNames[index-1] >= name ||
			row.ExportedArtifactPaths[index] != contract.Path || row.ExportedArtifactMediaTypes[index] != contract.MediaType ||
			!sha256Pattern.MatchString(digest) || row.ExportedArtifactSizes[index] < 0 || row.ExportedArtifactSizes[index] > maxArtifactBytes ||
			keyErr != nil || row.ExportedArtifactKeys[index] != wantedKey || row.ExportedArtifactTimes[index].IsZero() {
			return nil, errors.New("sandbox controller exported artifact identity is inconsistent")
		}
		item.PersistedArtifacts = append(item.PersistedArtifacts, PersistedArtifact{ID: id, Name: name, Path: contract.Path,
			MediaType: contract.MediaType, Digest: digest, Size: row.ExportedArtifactSizes[index], ObjectKey: wantedKey,
			ExportedAt: row.ExportedArtifactTimes[index].UTC().Format(time.RFC3339Nano)})
	}
	if !row.ArtifactExportComplete && len(row.ArtifactExportWarningCodes) != 0 {
		return nil, errors.New("incomplete artifact export carries warnings")
	}
	for index, warning := range row.ArtifactExportWarningCodes {
		if !artifactWarningPattern.MatchString(warning) || index > 0 && row.ArtifactExportWarningCodes[index-1] >= warning {
			return nil, errors.New("artifact export warnings are invalid")
		}
	}
	item.ArtifactExportComplete = row.ArtifactExportComplete
	item.ArtifactWarningCodes = append([]string(nil), row.ArtifactExportWarningCodes...)
	observation, workloadDigest, err := decodeObservation(row)
	if err != nil {
		return nil, err
	}
	item.AdmissionObservation = observation
	item.PersistedWorkloadDigest = workloadDigest
	if len(row.SourceReceipt) != 0 && string(row.SourceReceipt) != "null" {
		manifest := sourceManifest(item.Sources)
		receipt, err := sandboxio.DecodeSourceMaterializationReceipt(row.SourceReceipt, &manifest)
		if err != nil || len(item.Sources) == 0 {
			return nil, errors.New("sandbox controller source materialization receipt is inconsistent")
		}
		item.SourceMaterialization = &receipt
	}
	if len(row.SourceBootstrapObservation) != 0 && string(row.SourceBootstrapObservation) != "null" {
		var sourceObservation sandboxcontrol.AdmissionObservation
		decoder := json.NewDecoder(strings.NewReader(string(row.SourceBootstrapObservation)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&sourceObservation); err != nil || decoder.Decode(&struct{}{}) != io.EOF ||
			sandboxcontrol.ValidateAdmissionObservation(sourceObservation) != nil || item.SourceMaterialization == nil ||
			sourceObservation.Sandbox.Name != item.SandboxID || sourceObservation.Workload.WorkspaceID != item.WorkspaceID {
			return nil, errors.New("sandbox controller source bootstrap observation is inconsistent")
		}
		item.SourceBootstrapObservation = &sourceObservation
	}
	if item.SourceMaterialization != nil && observation == nil && item.SourceBootstrapObservation == nil {
		return nil, errors.New("sandbox controller source receipt lacks a bootstrap observation")
	}
	return item, nil
}

func sourceManifest(sources []Source) sandboxio.SourceManifest {
	manifest := sandboxio.SourceManifest{SchemaVersion: sandboxio.SourceManifestVersion, Sources: make([]sandboxio.Source, len(sources))}
	for index, source := range sources {
		manifest.Sources[index] = sandboxio.Source{Name: source.Name, URL: source.URL, Destination: source.Destination, Commit: source.Commit, Writable: source.Writable}
	}
	return manifest
}

func decodeObservation(row workItemRow) (*sandboxcontrol.AdmissionObservation, *string, error) {
	if row.AdmissionDigest == nil {
		if row.WorkloadAPIVersion != nil || row.WorkloadNamespace != nil || row.WorkloadName != nil ||
			row.WorkloadUID != nil || row.WorkloadResourceVersion != nil || row.AdmittedClusterQueue != nil ||
			row.OwnerAPIVersion != nil || row.OwnerKind != nil || row.OwnerName != nil || row.OwnerUID != nil ||
			row.OwnerController != nil || row.WorkspaceLabel != nil || row.SandboxLabel != nil || row.Admitted != nil ||
			row.ConditionType != nil || row.ConditionStatus != nil {
			return nil, nil, errors.New("sandbox controller admission identity is partially populated")
		}
		if hasObservationColumns(row) {
			return nil, nil, errors.New("sandbox controller admission observation is partially populated")
		}
		return nil, nil, nil
	}
	requiredStrings := []*string{row.WorkloadAPIVersion, row.WorkloadNamespace, row.WorkloadName,
		row.WorkloadUID, row.WorkloadResourceVersion, row.AdmittedClusterQueue, row.OwnerAPIVersion,
		row.OwnerKind, row.OwnerName, row.OwnerUID, row.WorkspaceLabel, row.SandboxLabel,
		row.ConditionType, row.ConditionStatus}
	for _, value := range requiredStrings {
		if value == nil || *value == "" {
			return nil, nil, errors.New("sandbox controller admission identity is incomplete")
		}
	}
	if row.OwnerController == nil || row.Admitted == nil {
		return nil, nil, errors.New("sandbox controller admission identity is incomplete")
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
		row.BackendUID == nil || row.BackendResourceVersion == nil || identity.Owner.UID != *row.BackendUID || row.AdmissionID == nil ||
		identity.UID != *row.AdmissionID || !sha256Pattern.MatchString(identity.Digest) {
		return nil, nil, errors.New("sandbox controller admission identity is inconsistent")
	}
	claimedDigest := identity.Digest
	record := sandboxcontrol.SandboxRecord{Name: row.SandboxID, Namespace: sandboxcontrol.Namespace,
		UID: identity.Owner.UID, ResourceVersion: "admission-check", WorkspaceID: row.WorkspaceID,
		OwnerID: row.RequestedBy, QueueName: sandboxcontrol.QueueName, State: sandboxcontrol.StateReady,
		ArtifactContractDigest: "sha256:0000000000000000000000000000000000000000000000000000000000000000"}
	receipt, err := sandboxcontrol.NewReceipt("controller-admission-check", sandboxcontrol.OperationCreate, record, nil, time.Unix(1, 0))
	if err != nil {
		return nil, nil, errors.New("sandbox controller admission identity cannot be verified")
	}
	identity.Digest = ""
	bound, err := sandboxcontrol.AttachAdmissionIdentity(receipt, *identity)
	if err != nil || bound.Admission == nil || bound.Admission.Digest != claimedDigest {
		return nil, nil, errors.New("sandbox controller admission digest is inconsistent")
	}
	identity.Digest = claimedDigest
	if !hasObservationColumns(row) {
		return nil, &claimedDigest, nil
	}
	for _, value := range []*string{row.PodAPIVersion, row.PodKind, row.PodNamespace, row.PodName,
		row.PodUID, row.PodResourceVersion, row.ObservationDigest} {
		if value == nil || *value == "" {
			return nil, nil, errors.New("sandbox controller admission observation is incomplete")
		}
	}
	observation := &sandboxcontrol.AdmissionObservation{
		Sandbox: sandboxcontrol.ObjectIdentity{APIVersion: sandboxcontrol.APIVersion,
			Kind: sandboxcontrol.Kind, Namespace: sandboxcontrol.Namespace, Name: row.SandboxID,
			UID: *row.BackendUID, ResourceVersion: *row.BackendResourceVersion},
		Pod: sandboxcontrol.ObjectIdentity{APIVersion: *row.PodAPIVersion, Kind: *row.PodKind,
			Namespace: *row.PodNamespace, Name: *row.PodName, UID: *row.PodUID,
			ResourceVersion: *row.PodResourceVersion},
		Workload: *identity, Digest: "sha256:" + strings.TrimSpace(*row.ObservationDigest),
	}
	if err := sandboxcontrol.ValidateAdmissionObservation(*observation); err != nil {
		return nil, nil, fmt.Errorf("sandbox controller admission observation is inconsistent: %w", err)
	}
	return observation, &claimedDigest, nil
}

func hasObservationColumns(row workItemRow) bool {
	return row.PodAPIVersion != nil || row.PodKind != nil || row.PodNamespace != nil ||
		row.PodName != nil || row.PodUID != nil || row.PodResourceVersion != nil || row.ObservationDigest != nil
}

func rawDigest(value string) (string, error) {
	if !sha256Pattern.MatchString(value) {
		return "", errors.New("sandbox controller admission digest is invalid")
	}
	return strings.TrimPrefix(value, "sha256:"), nil
}

func validWorkspaceArtifactPath(value string) bool {
	return len(value) > len("/workspace/artifacts/") && len(value) <= 512 && strings.HasPrefix(value, "/workspace/artifacts/") &&
		path.Clean(value) == value && !strings.Contains(value, "//") && !strings.Contains(value, `\`)
}
