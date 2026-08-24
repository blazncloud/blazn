package sandboxcontroller

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/blazncloud/blazn/internal/sandboxcontrol"
	"github.com/blazncloud/blazn/internal/sandboxio"
)

type queryCall struct {
	query string
	args  []any
}

type fakeExecutor struct {
	calls []queryCall
	rows  []sqlRow
}

func (e *fakeExecutor) QueryRow(_ context.Context, query string, args ...any) sqlRow {
	e.calls = append(e.calls, queryCall{query: query, args: append([]any(nil), args...)})
	row := e.rows[0]
	e.rows = e.rows[1:]
	return row
}
func (*fakeExecutor) Close() error { return nil }

type resultRow struct {
	values []any
	err    error
	delay  time.Duration
}

func (r resultRow) Scan(destinations ...any) error {
	if r.delay > 0 {
		time.Sleep(r.delay)
	}
	if r.err != nil {
		return r.err
	}
	if len(destinations) != len(r.values) {
		return errors.New("unexpected scan width")
	}
	for index, destination := range destinations {
		reflect.ValueOf(destination).Elem().Set(reflect.ValueOf(r.values[index]))
	}
	return nil
}

func TestPgStoreUsesOnlyFencedProcedures(t *testing.T) {
	executor := &fakeExecutor{rows: []sqlRow{
		resultRow{values: []any{true}},
		resultRow{values: []any{true}},
		resultRow{values: []any{sql.NullString{String: "80000000-0000-4000-8000-000000000001", Valid: true}, sql.NullTime{Time: time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC), Valid: true}}},
		resultRow{values: []any{true}},
		resultRow{values: []any{string(RetryScheduled)}},
		resultRow{values: []any{true}},
		resultRow{values: []any{3}},
	}}
	store := &PgStore{executor: executor}
	observation := storeObservationFixture()
	if bound, err := store.BindBackend(context.Background(), "operation", "worker", "lease", observation); err != nil || !bound {
		t.Fatalf("bind: bound=%v err=%v", bound, err)
	}
	source := sandboxio.Source{Name: "repo", URL: "https://example.test/owner/repo.git", Destination: "/workspace/src/repo", Commit: strings.Repeat("a", 40)}
	receipt, err := sandboxio.NewSourceMaterializationReceipt(
		sandboxio.SourceManifest{SchemaVersion: sandboxio.SourceManifestVersion, Sources: []sandboxio.Source{source}},
		[]sandboxio.SourceMaterialization{{Name: source.Name, URL: source.URL, Destination: source.Destination, Commit: source.Commit,
			Tree: strings.Repeat("b", 40), ContentDigest: "sha256:" + strings.Repeat("c", 64)}})
	if err != nil {
		t.Fatal(err)
	}
	if recorded, err := store.RecordSources(context.Background(), "operation", "worker", "lease", observation, receipt); err != nil || !recorded {
		t.Fatalf("record sources: recorded=%v err=%v", recorded, err)
	}
	artifactObservation := observation
	artifactObservation.Sandbox.Name = "30000000-0000-4000-8000-000000000001"
	artifactObservation.Workload.WorkspaceID = "40000000-0000-4000-8000-000000000001"
	artifact := PersistedArtifact{Name: "result", Path: "/workspace/artifacts/result", MediaType: "text/plain",
		Digest: "sha256:" + strings.Repeat("d", 64), Size: 6}
	artifact.ObjectKey, _ = ArtifactObjectKey(artifactObservation.Workload.WorkspaceID, artifactObservation.Sandbox.Name, artifact.Name)
	if persisted, recorded, err := store.RecordArtifact(context.Background(), "operation", "worker", "lease", artifactObservation, artifact); err != nil || !recorded || persisted.ID != "80000000-0000-4000-8000-000000000001" || persisted.ExportedAt != "2026-08-24T12:00:00Z" {
		t.Fatalf("record artifact: persisted=%#v recorded=%v err=%v", persisted, recorded, err)
	}
	if completed, err := store.CompleteArtifactExport(context.Background(), "operation", "worker", "lease", artifactObservation, []string{}); err != nil || !completed {
		t.Fatalf("complete artifact export: completed=%v err=%v", completed, err)
	}
	if outcome, err := store.Retry(context.Background(), "operation", "worker", "lease", 10,
		SafeError{Code: "backend_failure", Message: "safe", RequestID: "request-123"}); err != nil || outcome != RetryScheduled {
		t.Fatalf("retry: outcome=%q err=%v", outcome, err)
	}
	workloadDigest, observationDigest := observation.Workload.Digest, observation.Digest
	if completed, err := store.Complete(context.Background(), "operation", "worker", "lease",
		Completion{Status: "succeeded", ExpectedWorkloadDigest: &workloadDigest,
			ExpectedObservationDigest: &observationDigest, ArtifactIDs: []string{}, WarningCodes: []string{}}); err != nil || !completed {
		t.Fatalf("complete: completed=%v err=%v", completed, err)
	}
	if count, err := store.EnqueueExpired(context.Background(), 4); err != nil || count != 3 {
		t.Fatalf("expiry: count=%d err=%v", count, err)
	}
	queries := []string{bindSQL, recordSourcesSQL, recordArtifactSQL, completeArtifactExportSQL, retrySQL, completeSQL, expirySQL}
	for index, query := range queries {
		if executor.calls[index].query != query {
			t.Fatalf("query %d was %q, want %q", index, executor.calls[index].query, query)
		}
	}
	if got := executor.calls[0].args[33]; got != observation.Workload.Digest[7:] {
		t.Fatalf("bind Workload digest was not normalized: %v", got)
	}
	if got := executor.calls[0].args[34]; got != observation.Digest[7:] {
		t.Fatalf("bind observation digest was not normalized: %v", got)
	}
	if got := executor.calls[1].args[5]; got != observation.Digest[7:] {
		t.Fatalf("source observation digest was not normalized: %v", got)
	}
}

func TestPgStoreHealthRequiresExactControllerRole(t *testing.T) {
	executor := &fakeExecutor{rows: []sqlRow{resultRow{values: []any{false}}, resultRow{values: []any{true}}}}
	store := &PgStore{executor: executor}
	if err := store.Health(context.Background()); err == nil {
		t.Fatal("administrative database role was accepted")
	}
	if err := store.Health(context.Background()); err != nil {
		t.Fatalf("controller database role rejected: %v", err)
	}
	if executor.calls[0].query != "SELECT current_user = 'blazn_sandbox_controller'" {
		t.Fatalf("health authority query changed: %q", executor.calls[0].query)
	}
}

func TestPgStoreClaimDecodesImmutableWorkItem(t *testing.T) {
	payload, err := json.Marshal(map[string]any{
		"operation_id": "operation-1", "workspace_id": "workspace-1", "sandbox_id": "sandbox-1",
		"requested_by": "owner-1", "operation_type": "create", "expected_sandbox_version": 2,
		"lease_token": "lease-1", "lease_expires_at": "2026-08-22T12:00:00Z", "attempt": 1,
		"allocation_mode": "direct", "desired_state": "ready", "architecture": "amd64",
		"template_version_id": "template-1", "template_digest": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"variant_name": "amd64", "image_index_digest": "registry.example.test/blazn/sandbox@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"image_child_digest": "registry.example.test/blazn/sandbox@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		"placement_profile":  "poc-linux-amd64-v1", "command": []string{"true"},
		"request_cpu": "100m", "request_memory": "128Mi", "request_ephemeral_storage": "1Gi",
		"limit_cpu": "1", "limit_memory": "1Gi", "limit_ephemeral_storage": "2Gi",
		"queue_name": sandboxcontrol.QueueName, "admission_id": nil, "backend_uid": nil,
		"backend_resource_version": nil, "expires_at": "2026-08-23T12:00:00Z",
		"source_names": []string{"repo"}, "source_urls": []string{"https://example.invalid/repo"},
		"source_destinations": []string{"/workspace/repo"}, "source_writable": []bool{false},
		"source_commits": []string{"0123456789abcdef0123456789abcdef01234567"},
		"artifact_names": []string{"result"}, "artifact_paths": []string{"/workspace/artifacts/result"},
		"artifact_media_types": []string{"application/json"}, "artifact_required": []bool{true},
		"admission_digest": nil, "workload_api_version": nil, "workload_namespace": nil,
		"workload_name": nil, "workload_uid": nil, "workload_resource_version": nil,
		"admitted_cluster_queue": nil, "owner_api_version": nil, "owner_kind": nil,
		"owner_name": nil, "owner_uid": nil, "owner_controller": nil, "workspace_label": nil,
		"sandbox_label": nil, "admitted": nil, "condition_type": nil, "condition_status": nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	databaseNow := time.Date(2026, 8, 22, 11, 59, 30, 0, time.UTC)
	executor := &fakeExecutor{rows: []sqlRow{resultRow{delay: 20 * time.Millisecond,
		values: []any{string(payload), databaseNow}}}}
	store := &PgStore{executor: executor}
	item, err := store.Claim(context.Background(), "worker-1", 30)
	if err != nil {
		t.Fatal(err)
	}
	if item == nil || item.TemplateDigest != "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" ||
		len(item.Sources) != 1 || item.Sources[0].Commit != "0123456789abcdef0123456789abcdef01234567" ||
		len(item.Artifacts) != 1 || !item.Artifacts[0].Required || item.LeaseRemaining <= 29*time.Second || item.LeaseRemaining >= 29*time.Second+980*time.Millisecond {
		t.Fatalf("claim decoded incorrectly: %#v", item)
	}
	if executor.calls[0].query != claimSQL {
		t.Fatalf("claim bypassed claim_v4: %q", executor.calls[0].query)
	}
}

func TestPgStoreClaimRejectsInconsistentParallelArrays(t *testing.T) {
	payload := `{"template_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","source_names":["one"],"source_urls":[]}`
	executor := &fakeExecutor{rows: []sqlRow{resultRow{values: []any{payload, time.Now()}}}}
	store := &PgStore{executor: executor}
	if _, err := store.Claim(context.Background(), "worker-1", 30); err == nil {
		t.Fatal("inconsistent source arrays were accepted")
	}
}

func TestPgStoreClaimReturnsNoWork(t *testing.T) {
	executor := &fakeExecutor{rows: []sqlRow{resultRow{err: sql.ErrNoRows}}}
	store := &PgStore{executor: executor}
	item, err := store.Claim(context.Background(), "worker-1", 30)
	if err != nil || item != nil {
		t.Fatalf("no-work claim: item=%#v err=%v", item, err)
	}
}

func TestPgStoreRecordArtifactTreatsFencedNoRowAsRefusal(t *testing.T) {
	executor := &fakeExecutor{rows: []sqlRow{resultRow{err: sql.ErrNoRows}}}
	store := &PgStore{executor: executor}
	observation := storeObservationFixture()
	observation.Sandbox.Name = "30000000-0000-4000-8000-000000000001"
	observation.Workload.WorkspaceID = "40000000-0000-4000-8000-000000000001"
	artifact := PersistedArtifact{Name: "result", Path: "/workspace/artifacts/result", MediaType: "text/plain",
		Digest: "sha256:" + strings.Repeat("d", 64), Size: 6}
	artifact.ObjectKey, _ = ArtifactObjectKey(observation.Workload.WorkspaceID, observation.Sandbox.Name, artifact.Name)
	persisted, recorded, err := store.RecordArtifact(context.Background(), "operation", "worker", "stale-lease", observation, artifact)
	if err != nil || recorded || persisted != (PersistedArtifact{}) {
		t.Fatalf("fenced artifact record: persisted=%#v recorded=%v err=%v", persisted, recorded, err)
	}
}

func TestPgStoreClaimRequiresCanonicalAllOrNoneObservation(t *testing.T) {
	row := observationRowFixture()
	observation, digest, err := decodeObservation(row)
	if err != nil || observation == nil || digest == nil || *digest != observation.Workload.Digest {
		t.Fatalf("full observation decode: observation=%#v digest=%v err=%v", observation, digest, err)
	}

	tampered := row
	tampered.ObservationDigest = pointer(strings.Repeat("0", 64))
	if _, _, err := decodeObservation(tampered); err == nil {
		t.Fatal("tampered observation digest was accepted")
	}

	partial := row
	partial.PodUID = nil
	if _, _, err := decodeObservation(partial); err == nil {
		t.Fatal("partially populated Pod identity was accepted")
	}

	legacy := row
	legacy.PodAPIVersion, legacy.PodKind, legacy.PodNamespace = nil, nil, nil
	legacy.PodName, legacy.PodUID, legacy.PodResourceVersion, legacy.ObservationDigest = nil, nil, nil, nil
	observation, digest, err = decodeObservation(legacy)
	if err != nil || observation != nil || digest == nil || *digest != rowDigest(row) {
		t.Fatalf("legacy Workload-only decode: observation=%#v digest=%v err=%v", observation, digest, err)
	}
}

func storeObservationFixture() sandboxcontrol.AdmissionObservation {
	record := sandboxcontrol.SandboxRecord{UID: "backend-uid", ResourceVersion: "backend-rv"}
	identity := sandboxcontrol.WorkloadIdentity{APIVersion: sandboxcontrol.AdmissionAPIVersion,
		Namespace: sandboxcontrol.Namespace, Name: "workload.sandbox-1", UID: "workload-uid",
		ResourceVersion: "workload-rv", ClusterQueue: "queue", Owner: sandboxcontrol.SandboxOwnerReference{
			APIVersion: sandboxcontrol.APIVersion, Kind: sandboxcontrol.Kind, Name: "sandbox-1",
			UID: record.UID, Controller: true}, WorkspaceID: "workspace-1", SandboxID: "sandbox-1",
		Admitted: true, Condition: sandboxcontrol.AdmissionCondition{Type: "Admitted", Status: "True"}}
	receipt, err := sandboxcontrol.NewReceipt("store-observation", sandboxcontrol.OperationCreate,
		sandboxcontrol.SandboxRecord{Name: "sandbox-1", Namespace: sandboxcontrol.Namespace, UID: record.UID,
			ResourceVersion: record.ResourceVersion, WorkspaceID: "workspace-1", OwnerID: "owner-1",
			QueueName: sandboxcontrol.QueueName, State: sandboxcontrol.StateReady,
			ArtifactContractDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, nil, time.Unix(1, 0))
	if err != nil {
		panic(err)
	}
	receipt, err = sandboxcontrol.AttachAdmissionIdentity(receipt, identity)
	if err != nil {
		panic(err)
	}
	observation := sandboxcontrol.AdmissionObservation{
		Sandbox: sandboxcontrol.ObjectIdentity{APIVersion: sandboxcontrol.APIVersion, Kind: sandboxcontrol.Kind,
			Namespace: sandboxcontrol.Namespace, Name: "sandbox-1", UID: record.UID, ResourceVersion: record.ResourceVersion},
		Pod: sandboxcontrol.ObjectIdentity{APIVersion: "v1", Kind: "Pod", Namespace: sandboxcontrol.Namespace,
			Name: "pod.sandbox-1", UID: "pod-uid", ResourceVersion: "pod-rv"},
		Workload: *receipt.Admission,
	}
	observation.Digest = sandboxcontrol.AdmissionObservationDigest(observation)
	return observation
}

func observationRowFixture() workItemRow {
	observation := storeObservationFixture()
	rawWorkload, _ := rawDigest(observation.Workload.Digest)
	rawObservation, _ := rawDigest(observation.Digest)
	return workItemRow{OperationID: "operation-1", WorkspaceID: observation.Workload.WorkspaceID,
		SandboxID: observation.Workload.SandboxID, RequestedBy: "owner-1",
		AdmissionID: pointer(observation.Workload.UID), BackendUID: pointer(observation.Sandbox.UID),
		BackendResourceVersion: pointer(observation.Sandbox.ResourceVersion), AdmissionDigest: &rawWorkload,
		WorkloadAPIVersion: pointer(observation.Workload.APIVersion), WorkloadNamespace: pointer(observation.Workload.Namespace),
		WorkloadName: pointer(observation.Workload.Name), WorkloadUID: pointer(observation.Workload.UID),
		WorkloadResourceVersion: pointer(observation.Workload.ResourceVersion),
		AdmittedClusterQueue:    pointer(observation.Workload.ClusterQueue),
		OwnerAPIVersion:         pointer(observation.Workload.Owner.APIVersion), OwnerKind: pointer(observation.Workload.Owner.Kind),
		OwnerName: pointer(observation.Workload.Owner.Name), OwnerUID: pointer(observation.Workload.Owner.UID),
		OwnerController: pointer(observation.Workload.Owner.Controller), WorkspaceLabel: pointer(observation.Workload.WorkspaceID),
		SandboxLabel: pointer(observation.Workload.SandboxID), Admitted: pointer(observation.Workload.Admitted),
		ConditionType: pointer(observation.Workload.Condition.Type), ConditionStatus: pointer(observation.Workload.Condition.Status),
		PodAPIVersion: pointer(observation.Pod.APIVersion), PodKind: pointer(observation.Pod.Kind),
		PodNamespace: pointer(observation.Pod.Namespace), PodName: pointer(observation.Pod.Name),
		PodUID: pointer(observation.Pod.UID), PodResourceVersion: pointer(observation.Pod.ResourceVersion),
		ObservationDigest: &rawObservation}
}

func rowDigest(row workItemRow) string { return "sha256:" + *row.AdmissionDigest }

func TestPgStoreRenewPreservesNullAsFence(t *testing.T) {
	databaseNow := time.Now().UTC().Truncate(time.Microsecond)
	executor := &fakeExecutor{rows: []sqlRow{resultRow{values: []any{sql.NullTime{}, databaseNow}}}}
	store := &PgStore{executor: executor}
	window, ok, err := store.Renew(context.Background(), "operation", "worker", "lease", 30)
	if err != nil || ok || window != (LeaseWindow{}) {
		t.Fatalf("renew fence: window=%+v ok=%v err=%v", window, ok, err)
	}

	expiresAt := databaseNow.Add(30 * time.Second)
	executor.rows = []sqlRow{resultRow{values: []any{sql.NullTime{Time: expiresAt, Valid: true}, databaseNow}}}
	window, ok, err = store.Renew(context.Background(), "operation", "worker", "lease", 30)
	if err != nil || !ok || !window.ExpiresAt.Equal(expiresAt) || !window.DatabaseNow.Equal(databaseNow) || window.Remaining <= 29*time.Second || window.Remaining > 30*time.Second {
		t.Fatalf("renew success: window=%+v ok=%v err=%v", window, ok, err)
	}
}

func TestDatabaseLeaseRemainingIgnoresControllerWallClockSkew(t *testing.T) {
	for _, skew := range []time.Duration{-24 * time.Hour, 24 * time.Hour} {
		databaseNow := time.Unix(1_700_000_000, 0).Add(skew)
		if got := databaseLeaseRemaining(databaseNow.Add(30*time.Second), databaseNow, 2*time.Second); got != 28*time.Second {
			t.Fatalf("skew=%s remaining=%s", skew, got)
		}
	}
}

func TestPgStoreRenewSubtractsResponseDelay(t *testing.T) {
	databaseNow := time.Unix(1_700_000_000, 0)
	expiresAt := databaseNow.Add(500 * time.Millisecond)
	executor := &fakeExecutor{rows: []sqlRow{resultRow{delay: 50 * time.Millisecond,
		values: []any{sql.NullTime{Time: expiresAt, Valid: true}, databaseNow}}}}
	window, ok, err := (&PgStore{executor: executor}).Renew(context.Background(), "operation", "worker", "lease", 30)
	if err != nil || !ok || window.Remaining >= 470*time.Millisecond || window.Remaining <= 250*time.Millisecond {
		t.Fatalf("delayed renew: window=%+v ok=%v err=%v", window, ok, err)
	}
}
