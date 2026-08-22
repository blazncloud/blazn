package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

const runTestWorkspaceID = "00000000-0000-4000-8000-000000000001"
const runTestProjectID = "00000000-0000-4000-8000-000000000002"
const runTestRunID = "00000000-0000-4000-8000-000000000003"
const runTestArtifactID = "00000000-0000-4000-8000-000000000004"

func TestRunClientRoutesHeadersQueriesAndNullableEvidence(t *testing.T) {
	requestNumber := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestNumber++
		if r.Header.Get("Authorization") != "Bearer access-token" {
			t.Fatalf("authorization=%q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		switch requestNumber {
		case 1:
			if r.Method != http.MethodPost || r.URL.Path != "/v1/workspaces/"+runTestWorkspaceID+"/projects/"+runTestProjectID+"/runs" || r.Header.Get("Idempotency-Key") != "run-create-1" {
				t.Fatalf("create %s %s", r.Method, r.URL.String())
			}
			var request CreateRunRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.ProofClass != ProofClassSynthetic {
				t.Fatalf("request=%#v err=%v", request, err)
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(runEnvelopeJSON("queued", 1)))
		case 2:
			if r.Method != http.MethodGet || r.URL.Query().Get("status") != "all" || r.URL.Query().Get("cursor") != runTestRunID {
				t.Fatalf("list %s", r.URL.String())
			}
			_, _ = w.Write([]byte(`{"items":[],"nextCursor":null}`))
		case 3:
			if r.Method != http.MethodPost || r.URL.Path != "/v1/workspaces/"+runTestWorkspaceID+"/projects/"+runTestProjectID+"/runs/"+runTestRunID+"/cancel" {
				t.Fatalf("cancel %s %s", r.Method, r.URL.String())
			}
			_, _ = w.Write([]byte(runEnvelopeJSON("cancelled", 2)))
		case 4:
			if r.URL.Path != "/v1/workspaces/"+runTestWorkspaceID+"/projects/"+runTestProjectID+"/artifacts" || r.URL.Query().Get("status") != "ready" {
				t.Fatalf("artifacts %s", r.URL.String())
			}
			_, _ = w.Write([]byte(`{"items":[],"nextCursor":null}`))
		case 5:
			if r.URL.Path != "/v1/workspaces/"+runTestWorkspaceID+"/projects/"+runTestProjectID+"/artifacts/"+runTestArtifactID {
				t.Fatalf("artifact %s", r.URL.String())
			}
			_, _ = w.Write([]byte(`{"artifact":{"id":"` + runTestArtifactID + `","workspaceId":"` + runTestWorkspaceID + `","projectId":"` + runTestProjectID + `","kind":"content.video","mediaType":"video","name":"preview.mp4","status":"ready","version":1,"digest":"sha256:` + repeat("b", 64) + `","sizeBytes":12,"createdBy":"00000000-0000-4000-8000-000000000005","createdAt":"2026-08-22T00:00:00Z","updatedAt":"2026-08-22T00:00:00Z","downloadAvailable":true}}`))
		default:
			t.Fatalf("unexpected request %d", requestNumber)
		}
	}))
	defer server.Close()
	api, err := New(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	created, err := api.CreateRun(ctx, "access-token", runTestWorkspaceID, runTestProjectID, "run-create-1", CreateRunRequest{Kind: "content.render", ProofClass: ProofClassSynthetic, PlanDigest: "sha256:" + repeat("a", 64), InputArtifactIDs: []string{}, OutputNames: []string{"preview.mp4"}})
	if err != nil || created.Run.Placement != nil || created.Run.Receipt != nil {
		t.Fatalf("created=%#v err=%v", created, err)
	}
	if _, err = api.ListRuns(ctx, "access-token", runTestWorkspaceID, runTestProjectID, "all", runTestRunID); err != nil {
		t.Fatal(err)
	}
	if _, err = api.CancelRun(ctx, "access-token", runTestWorkspaceID, runTestProjectID, runTestRunID, "run-cancel-1", CancelRunRequest{ExpectedVersion: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err = api.ListArtifacts(ctx, "access-token", runTestWorkspaceID, runTestProjectID, "ready", ""); err != nil {
		t.Fatal(err)
	}
	artifact, err := api.GetArtifact(ctx, "access-token", runTestWorkspaceID, runTestProjectID, runTestArtifactID)
	if err != nil || artifact.Artifact.SizeBytes == nil || *artifact.Artifact.SizeBytes != 12 {
		t.Fatalf("artifact=%#v err=%v", artifact, err)
	}
}

func TestRunClientRejectsInvalidInputsBeforeNetwork(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer server.Close()
	api, _ := New(server.URL, server.Client())
	ctx := context.Background()
	valid := CreateRunRequest{Kind: "content.render", ProofClass: ProofClassSynthetic, PlanDigest: "sha256:" + repeat("a", 64), InputArtifactIDs: []string{}, OutputNames: []string{}}
	if _, err := api.CreateRun(ctx, "token", "bad", runTestProjectID, "run-create-1", valid); err == nil {
		t.Fatal("invalid Workspace passed")
	}
	valid.OutputNames = []string{"same", "same"}
	if _, err := api.CreateRun(ctx, "token", runTestWorkspaceID, runTestProjectID, "run-create-1", valid); err == nil {
		t.Fatal("duplicate outputs passed")
	}
	if _, err := api.CancelRun(ctx, "token", runTestWorkspaceID, runTestProjectID, runTestRunID, "run-cancel-1", CancelRunRequest{}); err == nil {
		t.Fatal("zero version passed")
	}
	if _, err := api.GetArtifact(ctx, "token", runTestWorkspaceID, runTestProjectID, "bad"); err == nil {
		t.Fatal("invalid Artifact ID passed")
	}
	if requests != 0 {
		t.Fatalf("requests=%d", requests)
	}
}
func runEnvelopeJSON(status string, version int) string {
	completed := ""
	receipt := "null"
	if status == "cancelled" {
		completed = `,"completedAt":"2026-08-22T00:01:00Z"`
		receipt = `{"schemaVersion":"blazn.run/receipt/v1alpha1","proofClass":"synthetic","outcome":"cancelled","planDigest":"sha256:` + repeat("a", 64) + `","artifactIds":[],"summary":{"steps":0,"warnings":[]}}`
	}
	return `{"run":{"id":"` + runTestRunID + `","workspaceId":"` + runTestWorkspaceID + `","projectId":"` + runTestProjectID + `","kind":"content.render","proofClass":"synthetic","status":"` + status + `","version":` + string(rune('0'+version)) + `,"planDigest":"sha256:` + repeat("a", 64) + `","inputArtifactIds":[],"outputNames":["preview.mp4"],"requestedBy":"00000000-0000-4000-8000-000000000005","placement":null,"receipt":` + receipt + `,"createdAt":"2026-08-22T00:00:00Z"` + completed + `}}`
}
func repeat(value string, count int) string {
	result := ""
	for range count {
		result += value
	}
	return result
}
