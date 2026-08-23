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

func TestCreateReceiptBindsExactAdmissionWorkloadIdentity(t *testing.T) {
	receipt, err := NewReceipt("request-admission-1", OperationCreate, SandboxRecord{
		Name: "sandbox-a", Namespace: Namespace, UID: "sandbox-uid", ResourceVersion: "101",
		WorkspaceID: "workspace-a", OwnerID: "owner-a", QueueName: QueueName,
		TrustLevel: TrustApprovedPOC, State: StateReady, ArtifactContractDigest: "sha256:" + strings.Repeat("a", 64),
	}, nil, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateTerminalCreateReceipt(receipt); err == nil {
		t.Fatal("name-only create receipt passed the terminal admission boundary")
	}
	identity := WorkloadIdentity{APIVersion: AdmissionAPIVersion, Namespace: Namespace, Name: "sandbox-a-workload", UID: "workload-uid-1", ResourceVersion: "202", ClusterQueue: "poc-cluster",
		Owner: SandboxOwnerReference{APIVersion: APIVersion, Kind: Kind, Name: receipt.Name, UID: receipt.UID, Controller: true}, WorkspaceID: receipt.WorkspaceID, SandboxID: receipt.Name,
		Admitted: true, Condition: AdmissionCondition{Type: "Admitted", Status: "True"}}
	bound, err := AttachAdmissionIdentity(receipt, identity)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateTerminalCreateReceipt(bound); err != nil {
		t.Fatal(err)
	}
	if bound.Admission == nil || bound.Admission.UID != identity.UID || bound.Digest == receipt.Digest {
		t.Fatalf("admission identity was not bound into the receipt digest: %#v", bound)
	}
	if bound.Admission.Digest != "sha256:83b222826fa967a4fe778886ddf601a64861ad0599ea3ee8ce6eca91faa0a802" {
		t.Fatalf("admission identity digest=%q", bound.Admission.Digest)
	}

	tampered := bound
	tamperedIdentity := identity
	tamperedIdentity.UID = "replacement-uid"
	tampered.Admission = &tamperedIdentity
	if err := ValidateTerminalCreateReceipt(tampered); err == nil {
		t.Fatal("tampered Workload UID passed receipt validation")
	}
	if _, err := AttachAdmissionIdentity(receipt, WorkloadIdentity{APIVersion: AdmissionAPIVersion, Namespace: Namespace, Name: identity.Name, UID: "", ResourceVersion: identity.ResourceVersion, ClusterQueue: identity.ClusterQueue}); err == nil {
		t.Fatal("name-only Workload identity was accepted")
	}
	for _, invalidName := range []string{"bad..name", "bad.-segment", strings.Repeat("a", 64)} {
		invalid := identity
		invalid.Name = invalidName
		if _, err := AttachAdmissionIdentity(receipt, invalid); err == nil {
			t.Fatalf("invalid Workload name %q was accepted", invalidName)
		}
	}
	for name, mutate := range map[string]func(*WorkloadIdentity){
		"workspace substitution":     func(value *WorkloadIdentity) { value.WorkspaceID = "workspace-b" },
		"sandbox label substitution": func(value *WorkloadIdentity) { value.SandboxID = "sandbox-b" },
		"owner name substitution":    func(value *WorkloadIdentity) { value.Owner.Name = "sandbox-b" },
		"owner UID substitution":     func(value *WorkloadIdentity) { value.Owner.UID = "sandbox-uid-b" },
		"owner kind substitution":    func(value *WorkloadIdentity) { value.Owner.Kind = "Pod" },
		"non-controller owner":       func(value *WorkloadIdentity) { value.Owner.Controller = false },
		"unadmitted status":          func(value *WorkloadIdentity) { value.Admitted = false },
		"unadmitted condition":       func(value *WorkloadIdentity) { value.Condition.Status = "False" },
	} {
		substituted := identity
		mutate(&substituted)
		if _, err := AttachAdmissionIdentity(receipt, substituted); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}

type fakeAPI struct {
	t                           *testing.T
	server                      *httptest.Server
	mu                          sync.Mutex
	object                      kubeSandbox
	lastMethod                  string
	lastQuery                   url.Values
	stripQueue                  bool
	runtimeMissing              bool
	patchRetainsFinalizer       bool
	patchChangesUID             bool
	patchChangesArtifacts       bool
	postCount                   int
	ambiguousPost               bool
	conflictAfterPreflight      bool
	forbiddenAfterPreflight     bool
	mutatePostIntent            bool
	mutatePostSpec              bool
	duplicatePod                bool
	duplicatePodDropsLabel      bool
	duplicateWorkload           bool
	duplicateWorkloadDropsLabel bool
	substitutePodOwner          bool
	substituteWorkloadOwner     bool
	driftAdmissionAPI           bool
	wrongWorkloadQueue          bool
	sandboxAbsent               bool
	podsAbsent                  bool
	workloadsAbsent             bool
}

const fakeHTTPClientTimeout = time.Second

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
	podCollection := "/api/v1/namespaces/" + Namespace + "/pods"
	workloadCollection := "/apis/" + AdmissionAPIVersion + "/namespaces/" + Namespace + "/workloads"
	switch {
	case request.Method == http.MethodGet && request.URL.Path == "/apis/node.k8s.io/v1/runtimeclasses/gvisor":
		if f.runtimeMissing {
			writeJSON(response, http.StatusNotFound, map[string]any{"reason": "NotFound", "code": 404})
			return
		}
		writeJSON(response, http.StatusOK, map[string]any{"apiVersion": "node.k8s.io/v1", "kind": "RuntimeClass", "metadata": map[string]string{"name": "gvisor"}, "handler": "runsc"})
	case request.Method == http.MethodPost && request.URL.Path == collection:
		f.postCount++
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
		if f.mutatePostIntent {
			object.Metadata.Annotations[CreateIntentAnnotation] = "sha256:" + strings.Repeat("f", 64)
		}
		if f.mutatePostSpec {
			object.Spec.PodTemplate.Spec.Containers[0].Image = "registry.example.test/other@sha256:" + strings.Repeat("b", 64)
		}
		f.object = object
		if f.conflictAfterPreflight {
			writeJSON(response, http.StatusConflict, map[string]any{"reason": "AlreadyExists", "code": 409})
			return
		}
		if f.forbiddenAfterPreflight {
			writeJSON(response, http.StatusForbidden, map[string]any{"reason": "Forbidden", "code": 403})
			return
		}
		if f.ambiguousPost {
			writeJSON(response, http.StatusInternalServerError, map[string]any{"reason": "InternalError", "code": 500})
			return
		}
		writeJSON(response, http.StatusCreated, object)
	case request.Method == http.MethodGet && request.URL.Path == podCollection:
		list := observedPodList{APIVersion: podAPIVersion, Kind: "PodList"}
		if !f.podsAbsent && f.object.Metadata.UID != "" {
			pod := f.observedPod()
			list.Items = append(list.Items, pod)
			if f.duplicatePod {
				copy := pod
				copy.Metadata.Name, copy.Metadata.UID = "sandbox-a-pod-2", "pod-uid-2"
				if f.duplicatePodDropsLabel {
					copy.Metadata.Labels = cloneMap(copy.Metadata.Labels)
					delete(copy.Metadata.Labels, ManagedLabel)
				}
				list.Items = append(list.Items, copy)
			}
		}
		writeJSON(response, http.StatusOK, list)
	case request.Method == http.MethodGet && request.URL.Path == workloadCollection:
		list := observedWorkloadList{APIVersion: AdmissionAPIVersion, Kind: "WorkloadList"}
		if f.driftAdmissionAPI {
			list.APIVersion = "kueue.x-k8s.io/v1"
		}
		if !f.workloadsAbsent && f.object.Metadata.UID != "" {
			workload := f.observedWorkload()
			list.Items = append(list.Items, workload)
			if f.duplicateWorkload {
				copy := workload
				copy.Metadata.Name, copy.Metadata.UID = "sandbox-a-workload-2", "workload-uid-2"
				if f.duplicateWorkloadDropsLabel {
					copy.Metadata.Labels = cloneMap(copy.Metadata.Labels)
					delete(copy.Metadata.Labels, ManagedLabel)
				}
				list.Items = append(list.Items, copy)
			}
		}
		writeJSON(response, http.StatusOK, list)
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
		if f.sandboxAbsent || f.object.Metadata.UID == "" {
			writeJSON(response, http.StatusNotFound, map[string]any{"reason": "NotFound", "code": 404})
			return
		}
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

func (f *fakeAPI) observedPod() observedPod {
	ownerUID := f.object.Metadata.UID
	if f.substitutePodOwner {
		ownerUID = "replacement-sandbox-uid"
	}
	return observedPod{
		APIVersion: podAPIVersion, Kind: podKind,
		Metadata: observedMetadata{Name: "sandbox-a-pod", Namespace: Namespace, UID: "pod-uid-1", ResourceVersion: "11", Labels: cloneMap(f.object.Spec.PodTemplate.Metadata.Labels),
			OwnerReferences: []kubeOwnerReference{{APIVersion: APIVersion, Kind: Kind, Name: f.object.Metadata.Name, UID: ownerUID, Controller: true}}},
		Spec: f.object.Spec.PodTemplate.Spec,
	}
}

func (f *fakeAPI) observedWorkload() observedWorkload {
	podUID := "pod-uid-1"
	if f.substituteWorkloadOwner {
		podUID = "replacement-pod-uid"
	}
	value := observedWorkload{
		APIVersion: AdmissionAPIVersion, Kind: workloadKind,
		Metadata: observedMetadata{Name: "sandbox-a-workload", Namespace: Namespace, UID: "workload-uid-1", ResourceVersion: "21", Labels: cloneMap(f.object.Spec.PodTemplate.Metadata.Labels),
			OwnerReferences: []kubeOwnerReference{{APIVersion: podAPIVersion, Kind: podKind, Name: "sandbox-a-pod", UID: podUID, Controller: true}}},
	}
	value.Status.Admission = &struct {
		ClusterQueue string `json:"clusterQueue"`
	}{ClusterQueue: "poc-cluster"}
	value.Status.Conditions = []kubeCondition{{Type: "Admitted", Status: "True"}}
	value.Spec.QueueName = QueueName
	if f.wrongWorkloadQueue {
		value.Spec.QueueName = "other-queue"
	}
	return value
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

func TestCreateIntentDigestBindsEveryMaterialRequestField(t *testing.T) {
	base := testCreate()
	base.ExpiresAt = time.Date(2026, 8, 23, 12, 0, 0, 123, time.UTC)
	want, err := createIntentDigest(base)
	if err != nil {
		t.Fatal(err)
	}
	mutations := []struct {
		name   string
		mutate func(*CreateRequest)
	}{
		{"request id", func(v *CreateRequest) { v.RequestID = "request-create-2" }},
		{"name", func(v *CreateRequest) { v.Name = "sandbox-b" }},
		{"workspace", func(v *CreateRequest) { v.WorkspaceID = "workspace-b" }},
		{"owner", func(v *CreateRequest) { v.OwnerID = "owner-b" }},
		{"image", func(v *CreateRequest) { v.Image = "registry.example.test/other@sha256:" + strings.Repeat("b", 64) }},
		{"command", func(v *CreateRequest) { v.Command[0] = "bash" }},
		{"architecture", func(v *CreateRequest) { v.Architecture = "arm64" }},
		{"runtime", func(v *CreateRequest) { v.RuntimeClassName = "kata" }},
		{"trust", func(v *CreateRequest) { v.TrustLevel = TrustApprovedPOC }},
		{"sensitivity", func(v *CreateRequest) { v.NonSensitive = !v.NonSensitive }},
		{"cpu request", func(v *CreateRequest) { v.CPURequest = "101m" }},
		{"memory request", func(v *CreateRequest) { v.MemoryRequest = "65Mi" }},
		{"ephemeral request", func(v *CreateRequest) { v.EphemeralStorageRequest = "2Gi" }},
		{"cpu limit", func(v *CreateRequest) { v.CPULimit = "201m" }},
		{"memory limit", func(v *CreateRequest) { v.MemoryLimit = "129Mi" }},
		{"ephemeral limit", func(v *CreateRequest) { v.EphemeralStorageLimit = "7Gi" }},
		{"expiry", func(v *CreateRequest) { v.ExpiresAt = v.ExpiresAt.Add(time.Nanosecond) }},
		{"artifacts", func(v *CreateRequest) { v.Artifacts[0].Required = false }},
	}
	for _, testCase := range mutations {
		t.Run(testCase.name, func(t *testing.T) {
			changed := base
			changed.Command = append([]string(nil), base.Command...)
			changed.Artifacts = append([]ArtifactExport(nil), base.Artifacts...)
			testCase.mutate(&changed)
			got, err := createIntentDigest(changed)
			if err != nil {
				t.Fatal(err)
			}
			if got == want {
				t.Fatalf("material %s change did not change digest", testCase.name)
			}
		})
	}
	ordered := base
	ordered.Artifacts = append(ordered.Artifacts, ArtifactExport{Name: "archive", Path: "/workspace/artifacts/archive.zip", MediaType: "application/zip"})
	reversed := ordered
	reversed.Artifacts = []ArtifactExport{ordered.Artifacts[1], ordered.Artifacts[0]}
	left, _ := createIntentDigest(ordered)
	right, _ := createIntentDigest(reversed)
	if left != right {
		t.Fatal("artifact set order changed canonical create intent")
	}
}

func TestCreateRequiresBothEphemeralStorageBounds(t *testing.T) {
	for _, mutate := range []func(*CreateRequest){
		func(request *CreateRequest) { request.EphemeralStorageRequest = "" },
		func(request *CreateRequest) { request.EphemeralStorageLimit = "" },
	} {
		request := testCreate()
		mutate(&request)
		assertCode(t, ValidateCreate(request, trustedRuntimes()), ErrInvalidRequest)
	}
}

func TestEnsureCreatedRecoversAmbiguousPOSTByExactIntent(t *testing.T) {
	fake := newFakeAPI(t)
	fake.ambiguousPost = true
	request := testCreate()
	adapter := testAdapter(t, fake, &fakeExporter{})
	record, receipt, err := adapter.EnsureCreated(context.Background(), request, "")
	if err != nil {
		t.Fatal(err)
	}
	if record.UID != "uid-1" || record.CreateIntentDigest == "" || receipt.UID != record.UID || fake.postCount != 1 {
		t.Fatalf("record=%#v receipt=%#v posts=%d", record, receipt, fake.postCount)
	}
	if _, _, err := adapter.EnsureCreated(context.Background(), request, ""); err == nil {
		t.Fatal("name-only retry adopted an existing Sandbox")
	}
	if _, _, err := adapter.EnsureCreated(context.Background(), request, record.UID); err != nil {
		t.Fatalf("exact UID/spec/intent adoption failed: %v", err)
	}
	if fake.postCount != 1 {
		t.Fatalf("idempotent adoption issued %d POSTs", fake.postCount)
	}
}

func TestEnsureCreatedRejectsAmbiguousPOSTSubstitution(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		configure func(*fakeAPI)
	}{
		{"intent", func(f *fakeAPI) { f.mutatePostIntent = true }},
		{"spec", func(f *fakeAPI) { f.mutatePostSpec = true }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fake := newFakeAPI(t)
			fake.ambiguousPost = true
			testCase.configure(fake)
			adapter := testAdapter(t, fake, &fakeExporter{})
			_, _, err := adapter.EnsureCreated(context.Background(), testCreate(), "")
			assertCode(t, err, ErrConflict)
			if fake.object.Metadata.DeletionTimestamp != "" {
				t.Fatal("ambiguous substituted object was deleted without ownership proof")
			}
		})
	}
}

func TestEnsureCreatedNeverAdoptsAuthoritativePOSTRejection(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		configure func(*fakeAPI)
		code      ErrorCode
	}{
		{"concurrent exact-intent conflict", func(f *fakeAPI) { f.conflictAfterPreflight = true }, ErrConflict},
		{"forbidden", func(f *fakeAPI) { f.forbiddenAfterPreflight = true }, ErrBackend},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fake := newFakeAPI(t)
			testCase.configure(fake)
			adapter := testAdapter(t, fake, &fakeExporter{})
			_, _, err := adapter.EnsureCreated(context.Background(), testCreate(), "")
			assertCode(t, err, testCase.code)
			if fake.postCount != 1 {
				t.Fatalf("authoritative rejection issued %d POSTs", fake.postCount)
			}
		})
	}
}

func TestObserveAdmissionRequiresExactWorkloadPodSandboxChain(t *testing.T) {
	fake := newFakeAPI(t)
	request := testCreate()
	adapter := testAdapter(t, fake, &fakeExporter{})
	record, _, err := adapter.Create(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	observation, err := adapter.ObserveAdmission(context.Background(), request, record, nil)
	if err != nil {
		t.Fatal(err)
	}
	if observation.Pod.UID != "pod-uid-1" || observation.Workload.UID != "workload-uid-1" || observation.Workload.Owner.UID != record.UID || observation.Workload.Digest == "" {
		t.Fatalf("observation=%#v", observation)
	}
	if _, err := adapter.ObserveAdmission(context.Background(), request, record, &observation); err != nil {
		t.Fatalf("stable re-observation failed: %v", err)
	}
	drifted := observation
	drifted.Pod.ResourceVersion = "12"
	if _, err := adapter.ObserveAdmission(context.Background(), request, record, &drifted); err == nil {
		t.Fatal("resourceVersion drift was accepted")
	}
}

func TestObserveAdmissionRejectsDuplicatesSubstitutionsAndAPIDrift(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*fakeAPI)
	}{
		{"duplicate Pod", func(f *fakeAPI) { f.duplicatePod = true }},
		{"duplicate Pod with dropped label", func(f *fakeAPI) { f.duplicatePod, f.duplicatePodDropsLabel = true, true }},
		{"duplicate Workload", func(f *fakeAPI) { f.duplicateWorkload = true }},
		{"duplicate Workload with dropped label", func(f *fakeAPI) { f.duplicateWorkload, f.duplicateWorkloadDropsLabel = true, true }},
		{"Sandbox owner substitution", func(f *fakeAPI) { f.substitutePodOwner = true }},
		{"Pod owner substitution", func(f *fakeAPI) { f.substituteWorkloadOwner = true }},
		{"Workload API drift", func(f *fakeAPI) { f.driftAdmissionAPI = true }},
		{"Workload queue substitution", func(f *fakeAPI) { f.wrongWorkloadQueue = true }},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			fake := newFakeAPI(t)
			request := testCreate()
			adapter := testAdapter(t, fake, &fakeExporter{})
			record, _, err := adapter.Create(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			testCase.configure(fake)
			_, err = adapter.ObserveAdmission(context.Background(), request, record, nil)
			assertCode(t, err, ErrConflict)
		})
	}
}

func TestObserveAbsenceFindsExactOwnedOrphansWithoutLabels(t *testing.T) {
	fake := newFakeAPI(t)
	request := testCreate()
	adapter := testAdapter(t, fake, &fakeExporter{})
	record, _, err := adapter.Create(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	observation, err := adapter.ObserveAdmission(context.Background(), request, record, nil)
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.sandboxAbsent = true
	for key := range fake.object.Spec.PodTemplate.Metadata.Labels {
		delete(fake.object.Spec.PodTemplate.Metadata.Labels, key)
	}
	fake.mu.Unlock()
	assertCode(t, adapter.ObserveAbsence(context.Background(), observation), ErrCleanupIncomplete)
	fake.mu.Lock()
	fake.podsAbsent = true
	fake.mu.Unlock()
	assertCode(t, adapter.ObserveAbsence(context.Background(), observation), ErrCleanupIncomplete)
	fake.mu.Lock()
	fake.workloadsAbsent = true
	fake.mu.Unlock()
	if err := adapter.ObserveAbsence(context.Background(), observation); err != nil {
		t.Fatal(err)
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

func TestFakeAPIClientBoundsStalledHandlerAndCleanup(t *testing.T) {
	handlerExited := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		defer close(handlerExited)
		<-request.Context().Done()
	}))
	t.Cleanup(server.Close)
	fake := &fakeAPI{t: t, server: server}
	adapter := testAdapter(t, fake, &fakeExporter{})
	if adapter.client.Timeout != fakeHTTPClientTimeout {
		t.Fatalf("fake HTTP client timeout=%s, want %s", adapter.client.Timeout, fakeHTTPClientTimeout)
	}

	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 3*fakeHTTPClientTimeout)
	defer cancel()
	_, _, err := adapter.Create(ctx, testCreate())
	assertCode(t, err, ErrRuntimeUntrusted)
	if elapsed := time.Since(started); elapsed > 2*fakeHTTPClientTimeout {
		t.Fatalf("stalled fake request returned after %s, client bound=%s", elapsed, fakeHTTPClientTimeout)
	}
	select {
	case <-handlerExited:
	case <-time.After(fakeHTTPClientTimeout):
		t.Fatal("stalled fake handler did not exit after client timeout")
	}

	closed := make(chan struct{})
	go func() {
		server.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(fakeHTTPClientTimeout):
		t.Fatal("fake server cleanup hung after the bounded request")
	}
}

func testAdapter(t *testing.T, fake *fakeAPI, exporter ArtifactExporter) *Adapter {
	t.Helper()
	client := *fake.server.Client()
	client.Timeout = fakeHTTPClientTimeout
	if client.Timeout <= 0 {
		t.Fatal("fake Kubernetes HTTP client must have a positive timeout")
	}
	adapter, err := New(Config{
		BaseURL: fake.server.URL, BearerToken: "proof", HTTPClient: &client, RuntimeClasses: trustedRuntimes(), Exporter: exporter,
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
		CPURequest: "100m", MemoryRequest: "64Mi", EphemeralStorageRequest: "1Gi", CPULimit: "200m", MemoryLimit: "128Mi", EphemeralStorageLimit: "6Gi", ExpiresAt: time.Now().Add(time.Hour),
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
	resources := pod.Spec.Containers[0].Resources
	if resources["requests"]["ephemeral-storage"] != "1Gi" || resources["limits"]["ephemeral-storage"] != "6Gi" || !digestPattern.MatchString(object.Metadata.Annotations[CreateIntentAnnotation]) {
		t.Fatalf("resource or create-intent boundary missing: resources=%#v annotations=%#v", resources, object.Metadata.Annotations)
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
