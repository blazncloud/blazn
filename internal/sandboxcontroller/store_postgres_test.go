package sandboxcontroller

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/blazncloud/blazn/internal/sandboxcontrol"
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
}

func (r resultRow) Scan(destinations ...any) error {
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
		resultRow{values: []any{string(RetryScheduled)}},
		resultRow{values: []any{true}},
		resultRow{values: []any{3}},
	}}
	store := &PgStore{executor: executor}
	record, identity := storeIdentityFixture()
	if bound, err := store.BindBackend(context.Background(), "operation", "worker", "lease", record, identity); err != nil || !bound {
		t.Fatalf("bind: bound=%v err=%v", bound, err)
	}
	if outcome, err := store.Retry(context.Background(), "operation", "worker", "lease", 10,
		SafeError{Code: "backend_failure", Message: "safe", RequestID: "request-123"}); err != nil || outcome != RetryScheduled {
		t.Fatalf("retry: outcome=%q err=%v", outcome, err)
	}
	digest := identity.Digest
	if completed, err := store.Complete(context.Background(), "operation", "worker", "lease",
		Completion{Status: "succeeded", ExpectedAdmissionDigest: &digest, ArtifactIDs: []string{}, WarningCodes: []string{}}); err != nil || !completed {
		t.Fatalf("complete: completed=%v err=%v", completed, err)
	}
	if count, err := store.EnqueueExpired(context.Background(), 4); err != nil || count != 3 {
		t.Fatalf("expiry: count=%d err=%v", count, err)
	}
	queries := []string{bindSQL, retrySQL, completeSQL, expirySQL}
	for index, query := range queries {
		if executor.calls[index].query != query {
			t.Fatalf("query %d was %q, want %q", index, executor.calls[index].query, query)
		}
	}
	if got := executor.calls[0].args[21]; got != identity.Digest[7:] {
		t.Fatalf("bind digest was not normalized: %v", got)
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
		"variant_name": "amd64", "image_index_digest": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"image_child_digest": "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
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
	executor := &fakeExecutor{rows: []sqlRow{resultRow{values: []any{string(payload)}}}}
	store := &PgStore{executor: executor}
	item, err := store.Claim(context.Background(), "worker-1", 30)
	if err != nil {
		t.Fatal(err)
	}
	if item == nil || item.TemplateDigest != "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" ||
		len(item.Sources) != 1 || item.Sources[0].Commit != "0123456789abcdef0123456789abcdef01234567" ||
		len(item.Artifacts) != 1 || !item.Artifacts[0].Required {
		t.Fatalf("claim decoded incorrectly: %#v", item)
	}
	if executor.calls[0].query != claimSQL {
		t.Fatalf("claim bypassed claim_v2: %q", executor.calls[0].query)
	}
}

func TestPgStoreClaimRejectsInconsistentParallelArrays(t *testing.T) {
	payload := `{"template_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","source_names":["one"],"source_urls":[]}`
	executor := &fakeExecutor{rows: []sqlRow{resultRow{values: []any{payload}}}}
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

func storeIdentityFixture() (sandboxcontrol.SandboxRecord, sandboxcontrol.WorkloadIdentity) {
	digest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	record := sandboxcontrol.SandboxRecord{UID: "backend-uid", ResourceVersion: "backend-rv"}
	identity := sandboxcontrol.WorkloadIdentity{APIVersion: sandboxcontrol.AdmissionAPIVersion,
		Namespace: sandboxcontrol.Namespace, Name: "workload.sandbox-1", UID: "workload-uid",
		ResourceVersion: "workload-rv", ClusterQueue: "queue", Owner: sandboxcontrol.SandboxOwnerReference{
			APIVersion: sandboxcontrol.APIVersion, Kind: sandboxcontrol.Kind, Name: "sandbox-1",
			UID: record.UID, Controller: true}, WorkspaceID: "workspace-1", SandboxID: "sandbox-1",
		Admitted: true, Condition: sandboxcontrol.AdmissionCondition{Type: "Admitted", Status: "True"}, Digest: digest}
	return record, identity
}

func TestPgStoreRenewPreservesNullAsFence(t *testing.T) {
	executor := &fakeExecutor{rows: []sqlRow{resultRow{values: []any{sql.NullTime{}}}}}
	store := &PgStore{executor: executor}
	when, ok, err := store.Renew(context.Background(), "operation", "worker", "lease", 30)
	if err != nil || ok || !when.IsZero() {
		t.Fatalf("renew fence: when=%v ok=%v err=%v", when, ok, err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	executor.rows = []sqlRow{resultRow{values: []any{sql.NullTime{Time: now, Valid: true}}}}
	when, ok, err = store.Renew(context.Background(), "operation", "worker", "lease", 30)
	if err != nil || !ok || !when.Equal(now) {
		t.Fatalf("renew success: when=%v ok=%v err=%v", when, ok, err)
	}
}
