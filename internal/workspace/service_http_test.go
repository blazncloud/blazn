package workspace

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KingJammin/blazn/internal/client"
)

type fakeSessions struct{ forced int }

func (s *fakeSessions) Origin() string { return "https://example.test" }
func (s *fakeSessions) Session(_ context.Context, force bool) (Session, error) {
	if force {
		s.forced++
		return Session{AccessToken: "new-access", UserID: "user-1"}, nil
	}
	return Session{AccessToken: "old-access", UserID: "user-1"}, nil
}

type memoryContexts struct{ selection Selection }

func (m *memoryContexts) Load(origin, user string) (Selection, error) {
	if m.selection.WorkspaceID == "" {
		return Selection{}, ErrNoContext
	}
	return m.selection, nil
}
func (m *memoryContexts) Save(value Selection) error { m.selection = value; return nil }

func TestJoinUsesBodyOnlyRetriesAccessAndSelectsWorkspace(t *testing.T) {
	const workspaceID = "123e4567-e89b-42d3-a456-426614174000"
	token := strings.Repeat("t", 43)
	var acceptURI, acceptBody, idempotency string
	var accepts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/workspace-invitations/accept":
			accepts++
			acceptURI = r.RequestURI
			idempotency = r.Header.Get("Idempotency-Key")
			var body client.AcceptInvitationRequest
			_ = json.NewDecoder(r.Body).Decode(&body)
			acceptBody = body.InviteToken
			if r.Header.Get("Authorization") == "Bearer old-access" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"code":"access_expired","message":"expired","requestId":"r1"}`))
				return
			}
			_, _ = w.Write([]byte(`{"workspace":{"id":"` + workspaceID + `","slug":"team","name":"Team","status":"active","version":1,"currentUserRole":"member","createdAt":"now","updatedAt":"now"}}`))
		case strings.HasPrefix(r.URL.Path, "/v1/workspaces/"):
			_, _ = w.Write([]byte(`{"workspace":{"id":"` + workspaceID + `","slug":"team","name":"Team","status":"active","version":1,"currentUserRole":"member","createdAt":"now","updatedAt":"now"}}`))
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	api, _ := client.New(server.URL, server.Client())
	sessions := &fakeSessions{}
	contexts := &memoryContexts{}
	service := NewService(api, sessions, contexts)
	service.newKey = func() (string, error) { return "request-123", nil }
	result, err := service.Join(context.Background(), token, "")
	if err != nil || result.Workspace.ID != workspaceID || sessions.forced != 1 || accepts != 2 {
		t.Fatalf("result=%#v forced=%d accepts=%d err=%v", result, sessions.forced, accepts, err)
	}
	if acceptURI != "/v1/workspace-invitations/accept" || acceptBody != token || strings.Contains(acceptURI, token) || idempotency != "request-123" {
		t.Fatalf("uri=%q body=%q key=%q", acceptURI, acceptBody, idempotency)
	}
	if contexts.selection.APIOrigin != sessions.Origin() || contexts.selection.UserID != "user-1" || contexts.selection.WorkspaceID != workspaceID {
		t.Fatalf("selection=%#v", contexts.selection)
	}
}
