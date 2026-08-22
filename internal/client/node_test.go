package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	testUUIDA = "11111111-1111-4111-8111-111111111111"
	testUUIDB = "22222222-2222-4222-8222-222222222222"
	testUUIDC = "33333333-3333-4333-8333-333333333333"
	testUUIDD = "44444444-4444-4444-8444-444444444444"
	testHash  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

func validNodeInstallPlan() NodeInstallPlan {
	return NodeInstallPlan{
		SchemaVersion: NodeSchemaVersion,
		PlanID:        testUUIDA, NodeID: testUUIDB, EnrollmentID: testUUIDC, WorkspaceID: testUUIDD,
		Mode:       NodeModeFresh,
		Cluster:    NodeInstallCluster{ID: "cluster-1", WorkerOnly: true, BootstrapTaint: "blazn.dev/bootstrap=pending:NoSchedule", ExpectedCAFingerprint: "sha256:" + testHash},
		Target:     NodeInstallTarget{Platform: NodePlatformLinux, Architecture: NodeArchAMD64, MachineFingerprint: testHash, MinCPU: 1, MinMemoryBytes: 1073741824, MinDiskBytes: 10737418240},
		Components: []NodeInstallComponent{{Name: "kubernetes", Version: "1.0", Source: "https://example.test/kubernetes", SHA256: testHash, Ownership: "install"}},
		Mutations:  []NodeInstallMutation{{Ordinal: 1, Kind: "file", Target: "/etc/blazn/node", DesiredDigest: "sha256:" + testHash, Rollback: "remove_if_owned"}},
		Rollback:   NodeInstallRollback{PreserveUserData: true, PreserveControlPlane: true, AmbiguousOwnership: "recovery_required"},
		IssuedAt:   "2026-08-21T00:00:00Z", ExpiresAt: "2026-08-21T00:10:00Z", SigningKeyID: "node-plan/v1", Digest: "sha256:" + testHash, Signature: strings.Repeat("A", 86),
	}
}

func TestValidateNodeInstallPlanSafetyAndMutationUniqueness(t *testing.T) {
	plan := validNodeInstallPlan()
	if err := ValidateNodeInstallPlan(plan); err != nil {
		t.Fatal(err)
	}
	plan.Rollback.PreserveUserData = false
	if err := ValidateNodeInstallPlan(plan); err == nil {
		t.Fatal("unsafe rollback plan passed validation")
	}
	plan = validNodeInstallPlan()
	plan.Mutations = append(plan.Mutations, plan.Mutations[0])
	if err := ValidateNodeInstallPlan(plan); err == nil || !strings.Contains(err.Error(), "repeats ordinal") {
		t.Fatalf("duplicate mutation error=%v", err)
	}
}

func TestValidateNodeOperationRejectsBroadOrUnsafeParameters(t *testing.T) {
	valid := CreateNodeOperationRequest{Type: "remove", ExpectedVersion: 3, Parameters: json.RawMessage(`{"clusterId":"cluster-1","expectedNodeUid":"uid-1","expectedResourceVersion":"9","confirm":true,"preserveHostData":true}`)}
	if err := ValidateCreateNodeOperationRequest(valid); err != nil {
		t.Fatal(err)
	}
	for _, parameters := range []string{
		`{"clusterId":"cluster-1","expectedNodeUid":"uid-1","expectedResourceVersion":"9","confirm":true,"preserveHostData":false}`,
		`{"clusterId":"cluster-1","expectedNodeUid":"uid-1","expectedResourceVersion":"9","confirm":true,"preserveHostData":true,"selector":"*"}`,
	} {
		request := valid
		request.Parameters = json.RawMessage(parameters)
		if err := ValidateCreateNodeOperationRequest(request); err == nil {
			t.Fatalf("unsafe remove parameters passed: %s", parameters)
		}
	}
}

func TestExchangeNodeEnrollmentUsesBodyAndValidatesPlan(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.RawQuery != "" || request.Header.Get("Authorization") != "" || !strings.HasSuffix(request.URL.Path, "/exchange") {
			t.Fatalf("unsafe exchange request path=%s query=%s auth=%q", request.URL.Path, request.URL.RawQuery, request.Header.Get("Authorization"))
		}
		var input ExchangeNodeEnrollmentRequest
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil || input.Token != strings.Repeat("t", 43) {
			t.Fatalf("exchange body=%#v err=%v", input, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(validNodeInstallPlan())
	}))
	defer server.Close()
	api, err := New(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	plan, err := api.ExchangeNodeEnrollment(context.Background(), testUUIDC, ExchangeNodeEnrollmentRequest{Token: strings.Repeat("t", 43), MachineFingerprint: testHash, NodePublicKey: strings.Repeat("A", 43), Platform: NodePlatformLinux, Architecture: NodeArchAMD64})
	if err != nil || plan.NodeID != testUUIDB {
		t.Fatalf("plan=%#v err=%v", plan, err)
	}
}

func TestHeartbeatUsesOnlyNodeProof(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-Blazn-Node-Proof") != "proof" || request.Header.Get("Authorization") != "" {
			t.Fatalf("proof=%q auth=%q", request.Header.Get("X-Blazn-Node-Proof"), request.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	api, _ := New(server.URL, server.Client())
	heartbeat := NodeHeartbeat{NodeID: testUUIDA, IdentityGeneration: 1, BootID: "boot", Sequence: 0, SentAt: "2026-08-21T00:00:00Z", CapabilityDigest: "sha256:" + testHash}
	if err := api.SubmitNodeHeartbeat(context.Background(), "proof", heartbeat); err != nil {
		t.Fatal(err)
	}
}

func TestNodeMutationsRequireBearerAndContractIdempotencyKey(t *testing.T) {
	api, _ := New("https://example.test", nil)
	request := CreateNodeOperationRequest{Type: "pause", ExpectedVersion: 1, Parameters: json.RawMessage(`{}`)}
	for _, testCase := range []struct{ token, key string }{{"", "valid-key"}, {"token", ""}, {"token", "invalid key"}} {
		if _, err := api.CreateNodeOperation(context.Background(), testCase.token, testUUIDA, testCase.key, request); err == nil {
			t.Fatalf("token=%q key=%q unexpectedly accepted", testCase.token, testCase.key)
		}
	}
}

func TestExchangeRejectsUnknownOrTrailingPlanFields(t *testing.T) {
	for _, suffix := range []string{`,"unexpected":true}`, `} {}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			encoded, _ := json.Marshal(validNodeInstallPlan())
			encoded = encoded[:len(encoded)-1]
			_, _ = w.Write(append(encoded, []byte(suffix)...))
		}))
		api, _ := New(server.URL, server.Client())
		_, err := api.ExchangeNodeEnrollment(context.Background(), testUUIDC, ExchangeNodeEnrollmentRequest{Token: strings.Repeat("t", 43), MachineFingerprint: testHash, NodePublicKey: strings.Repeat("A", 43), Platform: NodePlatformLinux, Architecture: NodeArchAMD64})
		server.Close()
		if err == nil {
			t.Fatalf("unsafe response suffix %q passed", suffix)
		}
	}
}
