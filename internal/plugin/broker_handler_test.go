package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/blazncloud/blazn/internal/client"
	workspacepkg "github.com/blazncloud/blazn/internal/workspace"
)

const (
	brokerTestWorkspaceID = "11111111-1111-4111-8111-111111111111"
	brokerTestProjectID   = "22222222-2222-4222-8222-222222222222"
	brokerTestRunID       = "33333333-3333-4333-8333-333333333333"
	brokerTestArtifactID  = "44444444-4444-4444-8444-444444444444"
)

type fakeBrokerSessions struct {
	origin       string
	sessions     []workspacepkg.Session
	forceRefresh []bool
}

func (s *fakeBrokerSessions) Origin() string { return s.origin }
func (s *fakeBrokerSessions) Session(_ context.Context, force bool) (workspacepkg.Session, error) {
	s.forceRefresh = append(s.forceRefresh, force)
	if len(s.sessions) == 0 {
		return workspacepkg.Session{}, errors.New("no session")
	}
	value := s.sessions[0]
	if len(s.sessions) > 1 {
		s.sessions = s.sessions[1:]
	}
	return value, nil
}

type fakeBrokerAPI struct {
	tokens        []string
	workspaceID   string
	projectID     string
	idempotency   string
	createRequest client.CreateRunRequest
	project       client.ProjectEnvelope
	run           client.RunEnvelope
	runs          client.RunList
	artifact      client.ArtifactEnvelope
	artifacts     client.ArtifactList
	err           error
	failFirst     bool
}

func (a *fakeBrokerAPI) record(token, workspaceID, projectID string) error {
	a.tokens = append(a.tokens, token)
	a.workspaceID, a.projectID = workspaceID, projectID
	if a.failFirst && len(a.tokens) == 1 {
		return &client.APIError{StatusCode: http.StatusUnauthorized, Body: client.ErrorBody{Code: "access_expired", Message: "expired"}}
	}
	return a.err
}
func (a *fakeBrokerAPI) GetProject(_ context.Context, token, workspaceID, projectID string) (client.ProjectEnvelope, error) {
	return a.project, a.record(token, workspaceID, projectID)
}
func (a *fakeBrokerAPI) CreateRun(_ context.Context, token, workspaceID, projectID, key string, request client.CreateRunRequest) (client.RunEnvelope, error) {
	a.idempotency, a.createRequest = key, request
	return a.run, a.record(token, workspaceID, projectID)
}
func (a *fakeBrokerAPI) ListRuns(_ context.Context, token, workspaceID, projectID, _, _ string) (client.RunList, error) {
	return a.runs, a.record(token, workspaceID, projectID)
}
func (a *fakeBrokerAPI) GetRun(_ context.Context, token, workspaceID, projectID, _ string) (client.RunEnvelope, error) {
	return a.run, a.record(token, workspaceID, projectID)
}
func (a *fakeBrokerAPI) CancelRun(_ context.Context, token, workspaceID, projectID, _, key string, _ client.CancelRunRequest) (client.RunEnvelope, error) {
	a.idempotency = key
	return a.run, a.record(token, workspaceID, projectID)
}
func (a *fakeBrokerAPI) ListArtifacts(_ context.Context, token, workspaceID, projectID, _, _ string) (client.ArtifactList, error) {
	return a.artifacts, a.record(token, workspaceID, projectID)
}
func (a *fakeBrokerAPI) GetArtifact(_ context.Context, token, workspaceID, projectID, _ string) (client.ArtifactEnvelope, error) {
	return a.artifact, a.record(token, workspaceID, projectID)
}

func brokerTestContext(t *testing.T) RuntimeContext {
	t.Helper()
	value := validRuntimeContext(t)
	value.WorkspaceID = brokerTestWorkspaceID
	value.ProjectID = brokerTestProjectID
	return value
}

func brokerTestHandler(t *testing.T, api *fakeBrokerAPI, sessions *fakeBrokerSessions) (*authenticatedBrokerHandler, RuntimeContext) {
	t.Helper()
	runtimeContext := brokerTestContext(t)
	if sessions.origin == "" {
		sessions.origin = runtimeContext.APIOrigin
	}
	if len(sessions.sessions) == 0 {
		sessions.sessions = []workspacepkg.Session{{AccessToken: "root-memory-token", UserID: runtimeContext.UserID}}
	}
	authority := &brokerAuthority{api: api, sessions: sessions}
	return &authenticatedBrokerHandler{runtimeContext: runtimeContext, initialize: func(RuntimeContext) (*brokerAuthority, error) { return authority, nil }}, runtimeContext
}

func brokerTestRequest(method, params string) brokerRequest {
	return brokerRequest{SchemaVersion: 1, RequestID: strings.Repeat("a", 32), Method: method, Params: json.RawMessage(params)}
}

func TestAuthenticatedBrokerDescriptionAdvertisesOnlyImplementedCapabilities(t *testing.T) {
	handler, runtimeContext := brokerTestHandler(t, &fakeBrokerAPI{}, &fakeBrokerSessions{})
	schema, value, failure := handler.Handle(context.Background(), "content", runtimeContext, brokerTestRequest("broker.describe", `{}`))
	description, ok := value.(brokerDescription)
	if failure != nil || schema != resultBrokerDescription || !ok || strings.Join(description.AvailableCapabilities, ",") != "artifact.read,broker.describe,project.read,run.cancel,run.create,run.read" {
		t.Fatalf("schema=%q value=%#v failure=%#v", schema, value, failure)
	}
	runtimeContext.ProjectID = ""
	_, value, failure = handler.Handle(context.Background(), "content", runtimeContext, brokerTestRequest("broker.describe", `{}`))
	description = value.(brokerDescription)
	if failure != nil || strings.Join(description.AvailableCapabilities, ",") != "broker.describe" {
		t.Fatalf("description=%#v failure=%#v", description, failure)
	}
}

func TestAuthenticatedBrokerBindsProjectAndKeepsTokenOutOfPayload(t *testing.T) {
	api := &fakeBrokerAPI{project: client.ProjectEnvelope{Project: client.Project{ID: brokerTestProjectID, WorkspaceID: brokerTestWorkspaceID, Name: "Content"}}}
	handler, runtimeContext := brokerTestHandler(t, api, &fakeBrokerSessions{})
	schema, value, failure := handler.Handle(context.Background(), "content", runtimeContext, brokerTestRequest("project.get", `{}`))
	encoded, _ := json.Marshal(value)
	if failure != nil || schema != resultProjectEnvelope || api.workspaceID != brokerTestWorkspaceID || api.projectID != brokerTestProjectID || len(api.tokens) != 1 || api.tokens[0] != "root-memory-token" || strings.Contains(string(encoded), "root-memory-token") {
		t.Fatalf("schema=%q value=%s api=%#v failure=%#v", schema, encoded, api, failure)
	}
}

func TestAuthenticatedBrokerScopesRunCreateAndRefreshesExpiredAccess(t *testing.T) {
	digest := "sha256:" + strings.Repeat("b", 64)
	api := &fakeBrokerAPI{failFirst: true, run: client.RunEnvelope{Run: client.Run{ID: brokerTestRunID, WorkspaceID: brokerTestWorkspaceID, ProjectID: brokerTestProjectID, ProofClass: client.ProofClassSynthetic, PlanDigest: digest}}}
	sessions := &fakeBrokerSessions{sessions: []workspacepkg.Session{{AccessToken: "expired-token", UserID: "user-1"}, {AccessToken: "fresh-token", UserID: "user-1"}}}
	handler, runtimeContext := brokerTestHandler(t, api, sessions)
	params := `{"kind":"content.render","proofClass":"synthetic","planDigest":"` + digest + `","inputArtifactIds":[],"outputNames":["preview.mp4"],"idempotencyKey":"plugin-run-1"}`
	schema, _, failure := handler.Handle(context.Background(), "content", runtimeContext, brokerTestRequest("run.create", params))
	if failure != nil || schema != resultRunEnvelope || !strings.HasPrefix(api.idempotency, "broker-v1-") || api.idempotency == "plugin-run-1" || len(api.idempotency) > 128 || api.createRequest.ProofClass != client.ProofClassSynthetic || strings.Join(api.tokens, ",") != "expired-token,fresh-token" || len(sessions.forceRefresh) != 2 || sessions.forceRefresh[0] || !sessions.forceRefresh[1] {
		t.Fatalf("schema=%q api=%#v sessions=%#v failure=%#v", schema, api, sessions, failure)
	}
}

func TestAuthenticatedBrokerRejectsInjectionProofUpgradeAndCrossScopeResults(t *testing.T) {
	api := &fakeBrokerAPI{run: client.RunEnvelope{Run: client.Run{ID: brokerTestRunID, WorkspaceID: brokerTestWorkspaceID, ProjectID: "55555555-5555-4555-8555-555555555555"}}}
	handler, runtimeContext := brokerTestHandler(t, api, &fakeBrokerSessions{})
	digest := "sha256:" + strings.Repeat("c", 64)
	for _, params := range []string{
		`{"kind":"content.render","proofClass":"provider","planDigest":"` + digest + `","inputArtifactIds":[],"outputNames":[],"idempotencyKey":"plugin-run-1"}`,
		`{"kind":"content.render","proofClass":"synthetic","planDigest":"` + digest + `","inputArtifactIds":[],"outputNames":[],"idempotencyKey":"plugin-run-1","workspaceId":"` + brokerTestWorkspaceID + `"}`,
	} {
		_, _, failure := handler.Handle(context.Background(), "content", runtimeContext, brokerTestRequest("run.create", params))
		if failure == nil || failure.Code != "invalid_request" {
			t.Fatalf("params=%s failure=%#v", params, failure)
		}
	}
	_, _, failure := handler.Handle(context.Background(), "content", runtimeContext, brokerTestRequest("run.get", `{"runId":"`+brokerTestRunID+`"}`))
	if failure == nil || failure.Code != "broker_response_invalid" {
		t.Fatalf("cross-scope failure=%#v", failure)
	}
}

func TestAuthenticatedBrokerConcealsInaccessibleResourceDetails(t *testing.T) {
	api := &fakeBrokerAPI{err: &client.APIError{StatusCode: http.StatusNotFound, Body: client.ErrorBody{Code: "project_not_found", Message: "secret project 123 does not exist"}}}
	handler, runtimeContext := brokerTestHandler(t, api, &fakeBrokerSessions{})
	_, _, failure := handler.Handle(context.Background(), "content", runtimeContext, brokerTestRequest("project.get", `{}`))
	if failure == nil || failure.Code != "resource_unavailable" || strings.Contains(failure.Message, "secret") || failure.Retryable {
		t.Fatalf("failure=%#v", failure)
	}
}

func TestAuthenticatedBrokerRejectsPrincipalChange(t *testing.T) {
	api := &fakeBrokerAPI{project: client.ProjectEnvelope{Project: client.Project{ID: brokerTestProjectID, WorkspaceID: brokerTestWorkspaceID}}}
	sessions := &fakeBrokerSessions{sessions: []workspacepkg.Session{{AccessToken: "other-token", UserID: "other-user"}}}
	handler, runtimeContext := brokerTestHandler(t, api, sessions)
	_, _, failure := handler.Handle(context.Background(), "content", runtimeContext, brokerTestRequest("project.get", `{}`))
	if failure == nil || failure.Code != "broker_backend_unavailable" || len(api.tokens) != 0 {
		t.Fatalf("api=%#v failure=%#v", api, failure)
	}
}

func TestAuthenticatedBrokerRoutesEveryFrozenMetadataMethod(t *testing.T) {
	digest := "sha256:" + strings.Repeat("d", 64)
	run := client.Run{ID: brokerTestRunID, WorkspaceID: brokerTestWorkspaceID, ProjectID: brokerTestProjectID, ProofClass: client.ProofClassSynthetic, PlanDigest: digest}
	artifact := client.Artifact{ID: brokerTestArtifactID, WorkspaceID: brokerTestWorkspaceID, ProjectID: brokerTestProjectID}
	api := &fakeBrokerAPI{run: client.RunEnvelope{Run: run}, runs: client.RunList{Items: []client.Run{run}}, artifact: client.ArtifactEnvelope{Artifact: artifact}, artifacts: client.ArtifactList{Items: []client.Artifact{artifact}}}
	handler, runtimeContext := brokerTestHandler(t, api, &fakeBrokerSessions{})
	for _, testCase := range []struct{ method, params, schema string }{
		{"run.list", `{"status":"all"}`, resultRunList},
		{"run.get", `{"runId":"` + brokerTestRunID + `"}`, resultRunEnvelope},
		{"run.cancel", `{"runId":"` + brokerTestRunID + `","expectedVersion":1,"idempotencyKey":"cancel-run-1"}`, resultRunEnvelope},
		{"artifact.list", `{"status":"ready"}`, resultArtifactList},
		{"artifact.get", `{"artifactId":"` + brokerTestArtifactID + `"}`, resultArtifactEnvelope},
	} {
		schema, _, failure := handler.Handle(context.Background(), "content", runtimeContext, brokerTestRequest(testCase.method, testCase.params))
		if failure != nil || schema != testCase.schema {
			t.Fatalf("method=%s schema=%q failure=%#v", testCase.method, schema, failure)
		}
	}
}

func TestAuthenticatedBrokerRejectsSchemaInvalidParamsBeforeAuthorityUse(t *testing.T) {
	api := &fakeBrokerAPI{}
	handler, runtimeContext := brokerTestHandler(t, api, &fakeBrokerSessions{})
	digest := "sha256:" + strings.Repeat("e", 64)
	for _, testCase := range []struct{ method, params string }{
		{"run.create", `{"kind":"content.render","proofClass":"synthetic","planDigest":"` + digest + `","outputNames":[],"idempotencyKey":"create-run-1"}`},
		{"run.list", `{"status":"unknown"}`},
		{"run.get", `{"runId":"not-a-uuid"}`},
		{"run.cancel", `{"runId":"` + brokerTestRunID + `","expectedVersion":0,"idempotencyKey":"cancel-run-1"}`},
		{"artifact.list", `{"status":"queued"}`},
		{"artifact.get", `{"artifactId":"not-a-uuid"}`},
	} {
		_, _, failure := handler.Handle(context.Background(), "content", runtimeContext, brokerTestRequest(testCase.method, testCase.params))
		if failure == nil || failure.Code != "invalid_request" {
			t.Fatalf("method=%s failure=%#v", testCase.method, failure)
		}
	}
	if len(api.tokens) != 0 {
		t.Fatalf("invalid requests reached API with tokens: %#v", api.tokens)
	}
}
