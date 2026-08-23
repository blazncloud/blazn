package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

const projectTestWorkspaceID = "00000000-0000-4000-8000-000000000001"
const projectTestProjectID = "00000000-0000-4000-8000-000000000002"

func TestProjectClientRoutesHeadersQueriesAndOptionalUpdates(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Bearer access-token" || r.Header.Get("Accept") != "application/json" {
			t.Fatalf("headers=%v", r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		switch requests {
		case 1:
			if r.Method != http.MethodPost || r.URL.Path != "/v1/workspaces/"+projectTestWorkspaceID+"/projects" || r.Header.Get("Idempotency-Key") != "project-create-1" {
				t.Fatalf("create %s %s headers=%v", r.Method, r.URL.String(), r.Header)
			}
			var request CreateProjectRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Kind != "content" || request.Name != "Launch Video" {
				t.Fatalf("create request=%#v err=%v", request, err)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(projectEnvelopeJSON(1, "active", "")))
		case 2:
			if r.Method != http.MethodGet || r.URL.Path != "/v1/workspaces/"+projectTestWorkspaceID+"/projects" || r.URL.Query().Get("status") != "all" || r.URL.Query().Get("cursor") != projectTestProjectID {
				t.Fatalf("list %s %s", r.Method, r.URL.String())
			}
			_, _ = w.Write([]byte(`{"items":[],"nextCursor":null}`))
		case 3:
			if r.Method != http.MethodPatch || r.URL.Path != "/v1/workspaces/"+projectTestWorkspaceID+"/projects/"+projectTestProjectID || r.Header.Get("Idempotency-Key") != "project-update-1" {
				t.Fatalf("update %s %s headers=%v", r.Method, r.URL.String(), r.Header)
			}
			var request map[string]any
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request["description"] != "" || request["expectedVersion"] != float64(1) {
				t.Fatalf("update request=%#v err=%v", request, err)
			}
			_, _ = w.Write([]byte(projectEnvelopeJSON(2, "archived", "")))
		default:
			t.Fatalf("unexpected request %d", requests)
		}
	}))
	defer server.Close()
	api, err := New(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	created, err := api.CreateProject(context.Background(), "access-token", projectTestWorkspaceID, "project-create-1", CreateProjectRequest{Name: "Launch Video", Kind: "content"})
	if err != nil || created.Project.ID != projectTestProjectID {
		t.Fatalf("created=%#v err=%v", created, err)
	}
	if _, err := api.ListProjects(context.Background(), "access-token", projectTestWorkspaceID, "all", projectTestProjectID); err != nil {
		t.Fatal(err)
	}
	description := ""
	status := ProjectStatusArchived
	updated, err := api.UpdateProject(context.Background(), "access-token", projectTestWorkspaceID, projectTestProjectID, "project-update-1", UpdateProjectRequest{ExpectedVersion: 1, Description: &description, Status: &status})
	if err != nil || updated.Project.Version != 2 || updated.Project.Status != ProjectStatusArchived {
		t.Fatalf("updated=%#v err=%v", updated, err)
	}
}

func TestProjectClientRejectsInvalidInputsBeforeNetwork(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer server.Close()
	api, _ := New(server.URL, server.Client())
	ctx := context.Background()
	if _, err := api.CreateProject(ctx, "token", "not-a-uuid", "project-create-1", CreateProjectRequest{Name: "Project"}); err == nil {
		t.Fatal("invalid Workspace ID passed")
	}
	if _, err := api.CreateProject(ctx, "token", projectTestWorkspaceID, "project-create-1", CreateProjectRequest{Name: "   "}); err == nil {
		t.Fatal("whitespace-only Project name passed")
	}
	if _, err := api.GetProject(ctx, "token", projectTestWorkspaceID, "not-a-uuid"); err == nil {
		t.Fatal("invalid Project ID passed")
	}
	if _, err := api.ListProjects(ctx, "token", projectTestWorkspaceID, "deleted", ""); err == nil {
		t.Fatal("invalid status passed")
	}
	if _, err := api.UpdateProject(ctx, "token", projectTestWorkspaceID, projectTestProjectID, "project-update-1", UpdateProjectRequest{ExpectedVersion: 1}); err == nil {
		t.Fatal("no-op update passed")
	}
	if _,err:=api.PutProjectProfile(ctx,"token",projectTestWorkspaceID,projectTestProjectID,"content","profile-put-1",PutProjectProfileRequest{});err==nil{t.Fatal("invalid Project profile passed")}
	if requests != 0 {
		t.Fatalf("network requests=%d", requests)
	}
}

func TestProjectProfileClientRoutesAndVersionZeroCreate(t *testing.T){requests:=0;server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){requests++;if r.Header.Get("Authorization")!="Bearer access-token"{t.Fatalf("headers=%v",r.Header)};w.Header().Set("Content-Type","application/json");switch requests{case 1:if r.Method!=http.MethodGet||r.URL.Path!="/v1/workspaces/"+projectTestWorkspaceID+"/projects/"+projectTestProjectID+"/profiles/content"{t.Fatalf("get %s %s",r.Method,r.URL.Path)};_,_=w.Write([]byte(projectProfileEnvelopeJSON(1)));case 2:if r.Method!=http.MethodPut||r.Header.Get("Idempotency-Key")!="profile-put-1"{t.Fatalf("put %s headers=%v",r.Method,r.Header)};var request PutProjectProfileRequest;if json.NewDecoder(r.Body).Decode(&request)!=nil||request.ExpectedVersion!=0||request.Status!=ProjectProfileStatusActive{t.Fatalf("request=%#v",request)};_,_=w.Write([]byte(projectProfileEnvelopeJSON(1)));default:t.Fatalf("requests=%d",requests)}}));defer server.Close();api,_:=New(server.URL,server.Client());profile,err:=api.GetProjectProfile(context.Background(),"access-token",projectTestWorkspaceID,projectTestProjectID,"content");if err!=nil||profile.Profile.Kind!="content"{t.Fatalf("profile=%#v err=%v",profile,err)};request:=PutProjectProfileRequest{SchemaVersion:"blazn.content/project/v1alpha1",DraftID:"00000000-0000-4000-8000-000000000004",ArtifactID:"00000000-0000-4000-8000-000000000005",Digest:"sha256:"+strings.Repeat("a",64),Status:ProjectProfileStatusActive,ExpectedVersion:0};if _,err:=api.PutProjectProfile(context.Background(),"access-token",projectTestWorkspaceID,projectTestProjectID,"content","profile-put-1",request);err!=nil{t.Fatal(err)}}

func projectProfileEnvelopeJSON(version int)string{return`{"profile":{"workspaceId":"`+projectTestWorkspaceID+`","projectId":"`+projectTestProjectID+`","kind":"content","schemaVersion":"blazn.content/project/v1alpha1","version":`+strconv.Itoa(version)+`,"draftId":"00000000-0000-4000-8000-000000000004","artifactId":"00000000-0000-4000-8000-000000000005","digest":"sha256:`+strings.Repeat("a",64)+`","status":"active","createdBy":"00000000-0000-4000-8000-000000000003","updatedBy":"00000000-0000-4000-8000-000000000003","createdAt":"2026-08-23T00:00:00Z","updatedAt":"2026-08-23T00:00:00Z"}}`}

func projectEnvelopeJSON(version int, status, description string) string {
	return `{"project":{"id":"` + projectTestProjectID + `","workspaceId":"` + projectTestWorkspaceID + `","slug":"launch-video","kind":"content","name":"Launch Video","description":"` + strings.ReplaceAll(description, `"`, `\"`) + `","status":"` + status + `","version":` + strconv.Itoa(version) + `,"createdBy":"00000000-0000-4000-8000-000000000003","createdAt":"2026-08-22T00:00:00Z","updatedAt":"2026-08-22T00:00:00Z"}}`
}
