package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type capturedWorkspaceRequest struct {
	method         string
	requestURI     string
	authorization  string
	idempotencyKey string
	lastEventID    string
	body           string
}

func TestWorkspaceCRUDRequestContractAndInvitationSecrecy(t *testing.T) {
	var requests []capturedWorkspaceRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests = append(requests, capturedWorkspaceRequest{
			method: r.Method, requestURI: r.RequestURI, authorization: r.Header.Get("Authorization"),
			idempotencyKey: r.Header.Get("Idempotency-Key"), lastEventID: r.Header.Get("Last-Event-ID"), body: string(body),
		})
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && (r.URL.Path == "/v1/workspaces" || strings.HasSuffix(r.URL.Path, "/invitations")) {
			w.WriteHeader(http.StatusCreated)
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/invitations") && r.Method == http.MethodPost:
			_, _ = w.Write([]byte(`{"invitation":{},"inviteToken":"returned-once"}`))
		case strings.HasSuffix(r.URL.Path, "/invitations") && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"items":[],"nextCursor":null}`))
		case strings.HasSuffix(r.URL.Path, "/members") && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"items":[],"nextCursor":null}`))
		case r.URL.Path == "/v1/workspaces" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"items":[],"nextCursor":null}`))
		case r.Method == http.MethodDelete:
			_, _ = w.Write([]byte(`{"status":"removed","workspaceId":"w","version":2}`))
		case strings.Contains(r.URL.Path, "/members/") && r.Method == http.MethodPatch:
			_, _ = w.Write([]byte(`{}`))
		default:
			_, _ = w.Write([]byte(`{"workspace":{}}`))
		}
	}))
	defer server.Close()
	api, err := New(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const token = "invite-secret-value-that-is-at-least-32-bytes"
	_, _ = api.CreateWorkspace(ctx, "access", "idem-create", CreateWorkspaceRequest{Name: "Acme", Slug: "acme"})
	_, _ = api.ListWorkspaces(ctx, "access", "cursor-one")
	_, _ = api.GetWorkspace(ctx, "access", "workspace-id")
	_, _ = api.UpdateWorkspace(ctx, "access", "workspace-id", "idem-update", UpdateWorkspaceRequest{Name: "New", ExpectedVersion: 1})
	_, _ = api.CreateWorkspaceInvitation(ctx, "access", "workspace-id", "idem-invite", CreateInvitationRequest{Role: RoleMember, ExpiresIn: 300})
	_, _ = api.ListWorkspaceInvitations(ctx, "access", "workspace-id", "cursor-two")
	_, _ = api.RevokeWorkspaceInvitation(ctx, "access", "workspace-id", "invitation-id", 3, "idem-revoke")
	_, _ = api.AcceptWorkspaceInvitation(ctx, "access", "idem-accept", AcceptInvitationRequest{InviteToken: token})
	_, _ = api.ListWorkspaceMembers(ctx, "access", "workspace-id", "cursor-three")
	_, _ = api.UpdateWorkspaceMember(ctx, "access", "workspace-id", "user-id", "idem-role", UpdateMembershipRequest{Role: RoleViewer, ExpectedVersion: 4})
	_, _ = api.RemoveWorkspaceMember(ctx, "access", "workspace-id", "user-id", 5, "idem-remove")
	_, _ = api.LeaveWorkspace(ctx, "access", "workspace-id", 6, "idem-leave")
	if len(requests) != 12 {
		t.Fatalf("requests=%d want=12", len(requests))
	}
	for index, request := range requests {
		if request.authorization != "Bearer access" {
			t.Fatalf("request %d authorization=%q", index, request.authorization)
		}
		if strings.Contains(request.requestURI, token) || request.idempotencyKey == token || request.lastEventID == token {
			t.Fatalf("invitation token escaped request body: %#v", request)
		}
	}
	accept := requests[7]
	if accept.method != http.MethodPost || accept.requestURI != "/v1/workspace-invitations/accept" {
		t.Fatalf("accept request=%#v", accept)
	}
	var acceptBody AcceptInvitationRequest
	if err := json.Unmarshal([]byte(accept.body), &acceptBody); err != nil || acceptBody.InviteToken != token {
		t.Fatalf("accept body=%q err=%v", accept.body, err)
	}
	if requests[1].requestURI != "/v1/workspaces?cursor=cursor-one" || !strings.Contains(requests[6].requestURI, "expectedVersion=3") {
		t.Fatalf("query contracts: list=%q revoke=%q", requests[1].requestURI, requests[6].requestURI)
	}
}

func TestWorkspaceEventStreamUsesHeaderNotQuery(t *testing.T) {
	var request capturedWorkspaceRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request = capturedWorkspaceRequest{method: r.Method, requestURI: r.RequestURI, authorization: r.Header.Get("Authorization"), lastEventID: r.Header.Get("Last-Event-ID")}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		_, _ = w.Write([]byte("event: ready\ndata: {}\n\n"))
	}))
	defer server.Close()
	api, _ := New(server.URL, server.Client())
	stream, err := api.StreamWorkspaceEvents(context.Background(), "access", "workspace-id", "event-17")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	if request.requestURI != "/v1/workspaces/workspace-id/events" || request.lastEventID != "event-17" || request.authorization != "Bearer access" {
		t.Fatalf("request=%#v", request)
	}
}

func TestWorkspaceClientRejectsUnsafeMutationInputsBeforeNetwork(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer server.Close()
	api, _ := New(server.URL, server.Client())
	if _, err := api.AcceptWorkspaceInvitation(context.Background(), "access", "valid-idem", AcceptInvitationRequest{InviteToken: "short"}); err == nil {
		t.Fatal("short invitation token was accepted")
	}
	if _, err := api.LeaveWorkspace(context.Background(), "access", "workspace", 0, "valid-idem"); err == nil {
		t.Fatal("invalid expected version was accepted")
	}
	if _, err := api.CreateWorkspace(context.Background(), "access", "short", CreateWorkspaceRequest{Name: "x"}); err == nil {
		t.Fatal("short idempotency key was accepted")
	}
	if calls != 0 {
		t.Fatalf("unsafe inputs made %d network calls", calls)
	}
}
