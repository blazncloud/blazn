package sandboxcontrol

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const testImage = "registry.example.test/blazn/sandbox@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type fakeExporter struct {
	mu       sync.Mutex
	calls    int
	fail     bool
	receipts []ArtifactReceipt
}

func (e *fakeExporter) Export(_ context.Context, sandbox SandboxRecord, specs []ArtifactExport) ([]ArtifactReceipt, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	if e.fail {
		return nil, errors.New("storage unavailable")
	}
	return append([]ArtifactReceipt(nil), e.receipts...), nil
}

type fakeAPI struct {
	t                     *testing.T
	server                *httptest.Server
	mu                    sync.Mutex
	object                kubeSandbox
	lastMethod            string
	lastQuery             url.Values
	stripQueue            bool
	runtimeMissing        bool
	patchRetainsFinalizer bool
	patchChangesUID       bool
	patchChangesArtifacts bool
}

func newFakeAPI(t *testing.T) *fakeAPI {
	fake := &fakeAPI{t: t}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.serveHTTP))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakeAPI) serveHTTP(response http.ResponseWriter, request *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastMethod, f.lastQuery = request.Method, request.URL.Query()
	if request.Header.Get("Authorization") != "Bearer proof" || request.Header.Get("User-Agent") != "blazn-sandbox-control-adapter/v1" {
		f.t.Errorf("unsafe auth or user agent: auth=%q agent=%q", request.Header.Get("Authorization"), request.Header.Get("User-Agent"))
	}
	collection := "/apis/agents.x-k8s.io/v1beta1/namespaces/" + Namespace + "/sandboxes"
	switch {
	case request.Method == http.MethodGet && request.URL.Path == "/apis/node.k8s.io/v1/runtimeclasses/gvisor":
		if f.runtimeMissing {
			writeJSON(response, http.StatusNotFound, map[string]any{"reason": "NotFound", "code": 404})
			return
		}
		writeJSON(response, http.StatusOK, map[string]any{"apiVersion": "node.k8s.io/v1", "kind": "RuntimeClass", "metadata": map[string]string{"name": "gvisor"}, "handler": "runsc"})
	case request.Method == http.MethodPost && request.URL.Path == collection:
		if request.Header.Get("Content-Type") != "application/json" {
			f.t.Errorf("create content type=%q", request.Header.Get("Content-Type"))
		}
		var object kubeSandbox
		decodeBody(f.t, request.Body, &object)
		assertRendered(f.t, object)
		object.Metadata.UID, object.Metadata.ResourceVersion, object.Metadata.Generation = "uid-1", "1", 1
		object.Status = kubeStatus{ObservedGeneration: 1, Conditions: []kubeCondition{{Type: "Ready", Status: "False", Reason: "QueueAdmissionPending"}}}
		if f.stripQueue {
			delete(object.Spec.PodTemplate.Metadata.Labels, QueueLabel)
		}
		f.object = object
		writeJSON(response, http.StatusCreated, object)
	case request.Method == http.MethodGet && request.URL.Path == collection && request.URL.Query().Get("watch") == "true":
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(response).Encode(kubeWatchEvent{Type: "ADDED", Object: f.object})
		_ = json.NewEncoder(response).Encode(map[string]any{"type": "BOOKMARK", "object": map[string]any{"metadata": map[string]string{"resourceVersion": "2"}}})
	case request.Method == http.MethodGet && request.URL.Path == collection:
		list := kubeList{Items: []kubeSandbox{f.object}}
		list.Metadata.ResourceVersion, list.Metadata.Continue = "2", "next"
		writeJSON(response, http.StatusOK, list)
	case request.Method == http.MethodGet && request.URL.Path == collection+"/sandbox-a/status":
		writeJSON(response, http.StatusOK, f.object)
	case request.Method == http.MethodGet && request.URL.Path == collection+"/sandbox-a":
		writeJSON(response, http.StatusOK, f.object)
	case request.Method == http.MethodDelete && request.URL.Path == collection+"/sandbox-a":
		var options map[string]any
		decodeBody(f.t, request.Body, &options)
		preconditions := options["preconditions"].(map[string]any)
		if preconditions["uid"] != "uid-1" || preconditions["resourceVersion"] != f.object.Metadata.ResourceVersion || options["propagationPolicy"] != "Foreground" {
			f.t.Errorf("delete preconditions=%v", options)
		}
		f.object.Metadata.DeletionTimestamp = "2026-08-22T12:00:00Z"
		response.WriteHeader(http.StatusOK)
		_, _ = response.Write([]byte(`{"kind":"Status","status":"Success"}`))
	case request.Method == http.MethodPatch && request.URL.Path == collection+"/sandbox-a":
		if request.Header.Get("Content-Type") != "application/merge-patch+json" {
			f.t.Errorf("patch content type=%q", request.Header.Get("Content-Type"))
		}
		var patch map[string]any
		decodeBody(f.t, request.Body, &patch)
		metadata := patch["metadata"].(map[string]any)
		if metadata["resourceVersion"] != f.object.Metadata.ResourceVersion {
			f.t.Errorf("patch resourceVersion=%v", metadata["resourceVersion"])
		}
		encoded, _ := json.Marshal(metadata["finalizers"])
		var finalizers []string
		_ = json.Unmarshal(encoded, &finalizers)
		if contains(finalizers, CleanupFinalizer) {
			f.t.Error("cleanup finalizer was retained")
		}
		f.object.Metadata.Finalizers = finalizers
		f.object.Metadata.ResourceVersion = "3"
		if f.patchRetainsFinalizer {
			f.object.Metadata.Finalizers = append(f.object.Metadata.Finalizers, CleanupFinalizer)
		}
		if f.patchChangesUID {
			f.object.Metadata.UID = "uid-other"
		}
		if f.patchChangesArtifacts {
			_, digest, _ := CanonicalArtifactContract(nil)
			f.object.Metadata.Annotations["sandboxes.blazn.dev/artifact-exports"] = "[]"
			f.object.Metadata.Annotations["sandboxes.blazn.dev/artifact-contract-digest"] = digest
		}
		writeJSON(response, http.StatusOK, f.object)
	default:
		writeJSON(response, http.StatusNotFound, map[string]any{"reason": "NotFound", "code": 404})
	}
}

func TestCreateInjectsQueueIdentityRuntimeAndSecurity(t *testing.T) {
	fake := newFakeAPI(t)
	exporter := &fakeExporter{}
	adapter := testAdapter(t, fake, exporter)
	record, receipt, err := adapter.Create(context.Background(), testCreate())
	if err != nil {
		t.Fatal(err)
	}
	if record.QueueName != QueueName || record.WorkspaceID != "workspace-a" || record.OwnerID != "owner-a" || record.State != StateQueued {
		t.Fatalf("record=%#v", record)
	}
	if err := ValidateReceipt(receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Operation != OperationCreate || receipt.RuntimeClass != "gvisor" || receipt.QueueName != QueueName {
		t.Fatalf("receipt=%#v", receipt)
	}
}

func TestNoUnmanagedFallbackWhenQueueIsStripped(t *testing.T) {
	fake := newFakeAPI(t)
	fake.stripQueue = true
	adapter := testAdapter(t, fake, &fakeExporter{})
	_, _, err := adapter.Create(context.Background(), testCreate())
	assertCode(t, err, ErrQueueRequired)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.object.Metadata.DeletionTimestamp == "" || contains(fake.object.Metadata.Finalizers, CleanupFinalizer) || fake.lastMethod != http.MethodPatch {
		t.Fatalf("rejected unmanaged Sandbox was not cleaned: deleting=%q finalizers=%v method=%s", fake.object.Metadata.DeletionTimestamp, fake.object.Metadata.Finalizers, fake.lastMethod)
	}
}

func TestCreateFailsClosedWhenLiveRuntimeDisappears(t *testing.T) {
	fake := newFakeAPI(t)
	fake.runtimeMissing = true
	adapter := testAdapter(t, fake, &fakeExporter{})
	_, _, err := adapter.Create(context.Background(), testCreate())
	assertCode(t, err, ErrRuntimeUntrusted)
	if fake.lastMethod != http.MethodGet {
		t.Fatalf("unexpected fallback method=%s", fake.lastMethod)
	}
}

func TestRuntimeGateFailsClosed(t *testing.T) {
	now := time.Now().UTC()
	base := testCreate()
	base.ExpiresAt = now.Add(time.Hour)
	tests := []struct {
		name    string
		mutate  func(*CreateRequest)
		runtime map[string]RuntimeCapability
		want    ErrorCode
	}{
		{"untrusted without runtime", func(request *CreateRequest) { request.RuntimeClassName = "" }, trustedRuntimes(), ErrRuntimeUntrusted},
		{"unqualified runtime", func(request *CreateRequest) {}, map[string]RuntimeCapability{"gvisor": {Name: "gvisor", Handler: "runsc", Hardened: true}}, ErrRuntimeUntrusted},
		{"wrong architecture", func(request *CreateRequest) { request.Architecture = "arm64" }, trustedRuntimes(), ErrRuntimeUntrusted},
		{"sensitive orchestration only", func(request *CreateRequest) {
			request.TrustLevel, request.RuntimeClassName, request.NonSensitive = TrustApprovedPOC, "", false
		}, trustedRuntimes(), ErrRuntimeUntrusted},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := base
			test.mutate(&request)
			assertCode(t, ValidateCreate(request, test.runtime), test.want)
		})
	}
	request := base
	request.TrustLevel, request.RuntimeClassName, request.NonSensitive = TrustApprovedPOC, "", true
	if err := ValidateCreate(request, trustedRuntimes()); err != nil {
		t.Fatal(err)
	}
}

func TestListStatusWatchEnforceOwnerBoundary(t *testing.T) {
	fake := newFakeAPI(t)
	adapter := testAdapter(t, fake, &fakeExporter{})
	_, _, err := adapter.Create(context.Background(), testCreate())
	if err != nil {
		t.Fatal(err)
	}
	list, err := adapter.List(context.Background(), "workspace-a", "owner-a", "", 10)
	if err != nil || len(list.Sandboxes) != 1 || list.Continue != "next" || list.ResourceVersion != "2" {
		t.Fatalf("list=%#v err=%v", list, err)
	}
	wantSelector := "blazn.dev/managed=true,blazn.dev/workspace=workspace-a,blazn.dev/owner=owner-a"
	if fake.lastQuery.Get("labelSelector") != wantSelector {
		t.Fatalf("selector=%q", fake.lastQuery.Get("labelSelector"))
	}
	status, err := adapter.Status(context.Background(), "workspace-a", "owner-a", "sandbox-a")
	if err != nil || status.State != StateQueued || status.Ready {
		t.Fatalf("status=%#v err=%v", status, err)
	}
	events, watchErrors, err := adapter.Watch(context.Background(), "workspace-a", "owner-a", "1")
	if err != nil {
		t.Fatal(err)
	}
	event := <-events
	if event.Type != "ADDED" || event.Sandbox.Name != "sandbox-a" {
		t.Fatalf("event=%#v", event)
	}
	if err := <-watchErrors; err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.object.Metadata.Labels[OwnerLabel] = "other-owner"
	fake.mu.Unlock()
	_, err = adapter.Get(context.Background(), "workspace-a", "owner-a", "sandbox-a")
	assertCode(t, err, ErrIdentityBoundary)
}

func TestDeleteFinalizeExportsBeforeRemovingFinalizer(t *testing.T) {
	fake := newFakeAPI(t)
	exportedAt := "2026-08-22T12:00:01Z"
	exporter := &fakeExporter{receipts: []ArtifactReceipt{{SchemaVersion: ArtifactSchema, Name: "result", ObjectKey: "workspaces/workspace-a/sandboxes/sandbox-a/artifacts/result", SHA256: "sha256:" + strings.Repeat("b", 64), Size: 12, ExportedAt: exportedAt}}}
	adapter := testAdapter(t, fake, exporter)
	record, _, err := adapter.Create(context.Background(), testCreate())
	if err != nil {
		t.Fatal(err)
	}
	deleteReceipt, err := adapter.Delete(context.Background(), "request-delete-1", "workspace-a", "owner-a", "sandbox-a", record.UID, record.ResourceVersion, record.ArtifactContractDigest)
	if err != nil || deleteReceipt.Operation != OperationDelete || deleteReceipt.State != StateStopping {
		t.Fatalf("delete receipt=%#v err=%v", deleteReceipt, err)
	}
	fake.mu.Lock()
	deleting := fake.object
	fake.mu.Unlock()
	finalReceipt, err := adapter.Finalize(context.Background(), "request-finalize-1", "workspace-a", "owner-a", "sandbox-a", deleting.Metadata.UID, deleting.Metadata.ResourceVersion, record.Artifacts, record.ArtifactContractDigest)
	if err != nil {
		t.Fatal(err)
	}
	if finalReceipt.Operation != OperationFinalize || finalReceipt.State != StateDeleted || len(finalReceipt.Artifacts) != 1 || exporter.calls != 1 {
		t.Fatalf("final receipt=%#v calls=%d", finalReceipt, exporter.calls)
	}
	if err := ValidateReceipt(finalReceipt); err != nil {
		t.Fatal(err)
	}
}

func TestFinalizeRetainsFinalizerOnArtifactFailure(t *testing.T) {
	fake := newFakeAPI(t)
	exporter := &fakeExporter{fail: true}
	adapter := testAdapter(t, fake, exporter)
	record, _, err := adapter.Create(context.Background(), testCreate())
	if err != nil {
		t.Fatal(err)
	}
	_, err = adapter.Delete(context.Background(), "request-delete-1", "workspace-a", "owner-a", "sandbox-a", record.UID, record.ResourceVersion, record.ArtifactContractDigest)
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	deleting := fake.object
	fake.mu.Unlock()
	_, err = adapter.Finalize(context.Background(), "request-finalize-1", "workspace-a", "owner-a", "sandbox-a", deleting.Metadata.UID, deleting.Metadata.ResourceVersion, record.Artifacts, record.ArtifactContractDigest)
	assertCode(t, err, ErrArtifactExport)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if !contains(fake.object.Metadata.Finalizers, CleanupFinalizer) || fake.lastMethod == http.MethodPatch {
		t.Fatalf("finalizer=%v method=%s", fake.object.Metadata.Finalizers, fake.lastMethod)
	}
}

func TestFinalizeRejectsAdmissionFinalizerOrUIDRewrite(t *testing.T) {
	for _, testCase := range []struct {
		name            string
		retainFinalizer bool
		changeUID       bool
		changeArtifacts bool
	}{{name: "finalizer re-added", retainFinalizer: true}, {name: "uid changed", changeUID: true}, {name: "artifact contract changed", changeArtifacts: true}} {
		t.Run(testCase.name, func(t *testing.T) {
			fake := newFakeAPI(t)
			fake.patchRetainsFinalizer, fake.patchChangesUID, fake.patchChangesArtifacts = testCase.retainFinalizer, testCase.changeUID, testCase.changeArtifacts
			exporter := &fakeExporter{receipts: []ArtifactReceipt{{SchemaVersion: ArtifactSchema, Name: "result", ObjectKey: "workspaces/workspace-a/sandboxes/sandbox-a/artifacts/result", SHA256: "sha256:" + strings.Repeat("b", 64), Size: 1, ExportedAt: "2026-08-22T12:00:01Z"}}}
			adapter := testAdapter(t, fake, exporter)
			record, _, err := adapter.Create(context.Background(), testCreate())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := adapter.Delete(context.Background(), "request-delete-1", "workspace-a", "owner-a", "sandbox-a", record.UID, record.ResourceVersion, record.ArtifactContractDigest); err != nil {
				t.Fatal(err)
			}
			fake.mu.Lock()
			deleting := fake.object
			fake.mu.Unlock()
			_, err = adapter.Finalize(context.Background(), "request-finalize-1", "workspace-a", "owner-a", "sandbox-a", deleting.Metadata.UID, deleting.Metadata.ResourceVersion, record.Artifacts, record.ArtifactContractDigest)
			assertCode(t, err, ErrCleanupIncomplete)
		})
	}
}

func TestMutatedArtifactAnnotationNeverReachesExporter(t *testing.T) {
	fake := newFakeAPI(t)
	exporter := &fakeExporter{}
	adapter := testAdapter(t, fake, exporter)
	record, _, err := adapter.Create(context.Background(), testCreate())
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.object.Metadata.DeletionTimestamp = "2026-08-22T12:00:00Z"
	fake.object.Metadata.Annotations["sandboxes.blazn.dev/artifact-exports"] = `[{"name":"result","path":"/etc/passwd","mediaType":"text/plain","required":false}]`
	deleting := fake.object
	fake.mu.Unlock()
	_, err = adapter.Finalize(context.Background(), "request-finalize-1", "workspace-a", "owner-a", "sandbox-a", deleting.Metadata.UID, deleting.Metadata.ResourceVersion, record.Artifacts, record.ArtifactContractDigest)
	assertCode(t, err, ErrBackend)
	if exporter.calls != 0 {
		t.Fatalf("mutated artifact contract reached exporter calls=%d", exporter.calls)
	}
}

func TestArtifactSuppressionCannotSatisfyTrustedFinalizePrecondition(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]string)
		code   ErrorCode
	}{
		{"annotation removed", func(annotations map[string]string) {
			delete(annotations, "sandboxes.blazn.dev/artifact-exports")
			delete(annotations, "sandboxes.blazn.dev/artifact-contract-digest")
		}, ErrBackend},
		{"null list", func(annotations map[string]string) {
			_, digest, _ := CanonicalArtifactContract(nil)
			annotations["sandboxes.blazn.dev/artifact-exports"] = "null"
			annotations["sandboxes.blazn.dev/artifact-contract-digest"] = digest
		}, ErrBackend},
		{"empty list", func(annotations map[string]string) {
			_, digest, _ := CanonicalArtifactContract(nil)
			annotations["sandboxes.blazn.dev/artifact-exports"] = "[]"
			annotations["sandboxes.blazn.dev/artifact-contract-digest"] = digest
		}, ErrConflict},
		{"required flipped", func(annotations map[string]string) {
			artifacts := []ArtifactExport{{Name: "result", Path: "/workspace/artifacts/result.txt", MediaType: "text/plain", Required: false}}
			canonical, digest, _ := CanonicalArtifactContract(artifacts)
			encoded, _ := json.Marshal(canonical)
			annotations["sandboxes.blazn.dev/artifact-exports"] = string(encoded)
			annotations["sandboxes.blazn.dev/artifact-contract-digest"] = digest
		}, ErrConflict},
		{"duplicate added", func(annotations map[string]string) {
			annotations["sandboxes.blazn.dev/artifact-exports"] = `[{"name":"result","path":"/workspace/artifacts/result.txt","mediaType":"text/plain","required":true},{"name":"result","path":"/workspace/artifacts/result.txt","mediaType":"text/plain","required":true}]`
		}, ErrBackend},
		{"digest changed", func(annotations map[string]string) {
			annotations["sandboxes.blazn.dev/artifact-contract-digest"] = "sha256:" + strings.Repeat("d", 64)
		}, ErrBackend},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			fake := newFakeAPI(t)
			exporter := &fakeExporter{}
			adapter := testAdapter(t, fake, exporter)
			record, _, err := adapter.Create(context.Background(), testCreate())
			if err != nil {
				t.Fatal(err)
			}
			fake.mu.Lock()
			fake.object.Metadata.DeletionTimestamp = "2026-08-22T12:00:00Z"
			testCase.mutate(fake.object.Metadata.Annotations)
			deleting := fake.object
			fake.mu.Unlock()
			_, err = adapter.Finalize(context.Background(), "request-finalize-1", "workspace-a", "owner-a", "sandbox-a", deleting.Metadata.UID, deleting.Metadata.ResourceVersion, record.Artifacts, record.ArtifactContractDigest)
			assertCode(t, err, testCase.code)
			if exporter.calls != 0 {
				t.Fatalf("suppressed contract reached exporter calls=%d", exporter.calls)
			}
		})
	}
}

func TestArtifactReorderingUsesCanonicalTrustedSet(t *testing.T) {
	fake := newFakeAPI(t)
	request := testCreate()
	request.Artifacts = append(request.Artifacts, ArtifactExport{Name: "logs", Path: "/workspace/artifacts/logs.txt", MediaType: "text/plain"})
	exporter := &fakeExporter{receipts: []ArtifactReceipt{{SchemaVersion: ArtifactSchema, Name: "result", ObjectKey: "workspaces/workspace-a/sandboxes/sandbox-a/artifacts/result", SHA256: "sha256:" + strings.Repeat("b", 64), Size: 1, ExportedAt: "2026-08-22T12:00:01Z"}}}
	adapter := testAdapter(t, fake, exporter)
	record, _, err := adapter.Create(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.object.Metadata.DeletionTimestamp = "2026-08-22T12:00:00Z"
	reversed := []ArtifactExport{record.Artifacts[1], record.Artifacts[0]}
	encoded, _ := json.Marshal(reversed)
	fake.object.Metadata.Annotations["sandboxes.blazn.dev/artifact-exports"] = string(encoded)
	deleting := fake.object
	fake.mu.Unlock()
	if _, err := adapter.Finalize(context.Background(), "request-finalize-1", "workspace-a", "owner-a", "sandbox-a", deleting.Metadata.UID, deleting.Metadata.ResourceVersion, record.Artifacts, record.ArtifactContractDigest); err != nil {
		t.Fatal(err)
	}
}

func TestFrozenErrorStatusesAndOrchestrationNotice(t *testing.T) {
	for code, status := range map[ErrorCode]int{
		ErrInvalidRequest: 400, ErrIdentityBoundary: 404, ErrQueueRequired: 502, ErrRuntimeUntrusted: 403,
		ErrConflict: 409, ErrNotFound: 404, ErrBackend: 502, ErrArtifactExport: 502, ErrCleanupIncomplete: 409, ErrResourceVersionStale: 409,
	} {
		err := adapterError(code, 599, "safe", nil).(*AdapterError)
		if err.Status != status {
			t.Fatalf("code=%s status=%d want=%d", code, err.Status, status)
		}
	}
	response := &http.Response{StatusCode: http.StatusUnprocessableEntity, Body: io.NopCloser(strings.NewReader(`{"reason":"Invalid"}`))}
	var invalid *AdapterError
	if err := decodeStatus(response); !errors.As(err, &invalid) || invalid.Code != ErrInvalidRequest || invalid.Status != 400 {
		t.Fatalf("422 mapping=%v", err)
	}
	fake := newFakeAPI(t)
	adapter := testAdapter(t, fake, &fakeExporter{})
	_, _, err := adapter.Create(context.Background(), testCreate())
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.object.Metadata.Annotations["sandboxes.blazn.dev/trust-level"] = string(TrustApprovedPOC)
	fake.mu.Unlock()
	status, err := adapter.Status(context.Background(), "workspace-a", "owner-a", "sandbox-a")
	if err != nil || status.IsolationNotice != OrchestrationNotice {
		t.Fatalf("status=%#v err=%v", status, err)
	}
}

func TestReceiptTamperAndArtifactContract(t *testing.T) {
	fake := newFakeAPI(t)
	adapter := testAdapter(t, fake, &fakeExporter{})
	_, receipt, err := adapter.Create(context.Background(), testCreate())
	if err != nil {
		t.Fatal(err)
	}
	receipt.OwnerID = "other-owner"
	if err := ValidateReceipt(receipt); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("tampered receipt error=%v", err)
	}
	request := testCreate()
	request.Artifacts = []ArtifactExport{{Name: "result", Path: "/tmp/result", MediaType: "text/plain", Required: true}}
	assertCode(t, ValidateCreate(request, trustedRuntimes()), ErrInvalidRequest)
	sandbox := SandboxRecord{Name: "sandbox-a", WorkspaceID: "workspace-a", Artifacts: []ArtifactExport{{Name: "result", Required: true}}}
	badArtifact := ArtifactReceipt{SchemaVersion: ArtifactSchema, Name: "result", ObjectKey: "workspaces/other/sandboxes/sandbox-a/result", SHA256: "sha256:" + strings.Repeat("c", 64), ExportedAt: "2026-08-22T12:00:00Z"}
	assertCode(t, validateArtifactCompletion(sandbox, []ArtifactReceipt{badArtifact}), ErrArtifactExport)
	badArtifact.Name, badArtifact.ObjectKey = "unrequested", "workspaces/workspace-a/sandboxes/sandbox-a/artifacts/unrequested"
	assertCode(t, validateArtifactCompletion(sandbox, []ArtifactReceipt{badArtifact}), ErrArtifactExport)
}

func testAdapter(t *testing.T, fake *fakeAPI, exporter ArtifactExporter) *Adapter {
	t.Helper()
	adapter, err := New(Config{
		BaseURL: fake.server.URL, BearerToken: "proof", HTTPClient: fake.server.Client(), RuntimeClasses: trustedRuntimes(), Exporter: exporter,
		Now: func() time.Time { return time.Date(2026, 8, 22, 12, 0, 2, 0, time.UTC) }, WatchIdleTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func testCreate() CreateRequest {
	return CreateRequest{
		RequestID: "request-create-1", Name: "sandbox-a", WorkspaceID: "workspace-a", OwnerID: "owner-a", Image: testImage,
		Command: []string{"sh", "-c", "sleep 3600"}, Architecture: "amd64", RuntimeClassName: "gvisor", TrustLevel: TrustUntrusted,
		CPURequest: "100m", MemoryRequest: "64Mi", CPULimit: "200m", MemoryLimit: "128Mi", ExpiresAt: time.Now().Add(time.Hour),
		Artifacts: []ArtifactExport{{Name: "result", Path: "/workspace/artifacts/result.txt", MediaType: "text/plain", Required: true}},
	}
}

func trustedRuntimes() map[string]RuntimeCapability {
	return map[string]RuntimeCapability{"gvisor": {Name: "gvisor", Handler: "runsc", Architectures: []string{"amd64"}, Hardened: true, Qualified: true}}
}

func assertRendered(t *testing.T, object kubeSandbox) {
	t.Helper()
	if object.APIVersion != APIVersion || object.Kind != Kind || object.Metadata.Namespace != Namespace || object.Metadata.Labels[ManagedLabel] != "true" || !reflect.DeepEqual(object.Metadata.Finalizers, []string{CleanupFinalizer}) {
		t.Fatalf("metadata=%#v", object.Metadata)
	}
	pod := object.Spec.PodTemplate
	if pod.Metadata.Labels[QueueLabel] != QueueName || pod.Spec.ServiceAccountName != ServiceAccountName || pod.Spec.AutomountServiceAccountToken || pod.Spec.RuntimeClassName != "gvisor" || pod.Spec.NodeSelector["blazn.dev/sandbox-eligible"] != "true" {
		t.Fatalf("pod=%#v", pod)
	}
	security := pod.Spec.Containers[0].SecurityContext
	if security["allowPrivilegeEscalation"] != false || security["readOnlyRootFilesystem"] != true || pod.Spec.SecurityContext["runAsUser"] != float64(65532) && pod.Spec.SecurityContext["runAsUser"] != int64(65532) || pod.Spec.Containers[0].Image != testImage {
		t.Fatalf("container=%#v", pod.Spec.Containers[0])
	}
}

func assertCode(t *testing.T, err error, code ErrorCode) {
	t.Helper()
	var adapterErr *AdapterError
	if !errors.As(err, &adapterErr) || adapterErr.Code != code {
		t.Fatalf("error=%v code=%s", err, code)
	}
}

func decodeBody(t *testing.T, body io.Reader, output any) {
	t.Helper()
	decoder := json.NewDecoder(body)
	if err := decoder.Decode(output); err != nil {
		t.Fatal(err)
	}
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}
