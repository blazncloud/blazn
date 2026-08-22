package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/blazncloud/blazn/internal/client"
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

type memoryContexts struct {
	selection Selection
	saveErr   error
}

func (m *memoryContexts) Load(origin, user string) (Selection, error) {
	if m.selection.WorkspaceID == "" {
		return Selection{}, ErrNoContext
	}
	return m.selection, nil
}
func (m *memoryContexts) Save(value Selection) error {
	if m.saveErr != nil {
		return m.saveErr
	}
	m.selection = value
	return nil
}

func TestCurrentSelectionBindsOriginAndAuthenticatedUser(t *testing.T) {
	contexts := &memoryContexts{selection: Selection{SchemaVersion: 1, APIOrigin: "https://example.test", UserID: "user-1", WorkspaceID: "workspace-1"}}
	selection, err := NewService(nil, &fakeSessions{}, contexts).CurrentSelection(context.Background())
	if err != nil || selection.APIOrigin != "https://example.test" || selection.UserID != "user-1" || selection.WorkspaceID != "workspace-1" {
		t.Fatalf("selection=%#v err=%v", selection, err)
	}
	contexts.selection = Selection{}
	selection, err = NewService(nil, &fakeSessions{}, contexts).CurrentSelection(context.Background())
	if !errors.Is(err, ErrNoContext) || selection.APIOrigin != "https://example.test" || selection.UserID != "user-1" || selection.WorkspaceID != "" {
		t.Fatalf("unselected=%#v err=%v", selection, err)
	}
}

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
	result, err := service.Join(context.Background(), token, "request-123")
	if err != nil || result.Workspace.ID != workspaceID || !result.Accepted || !result.Selected || sessions.forced != 1 || accepts != 2 {
		t.Fatalf("result=%#v forced=%d accepts=%d err=%v", result, sessions.forced, accepts, err)
	}
	if acceptURI != "/v1/workspace-invitations/accept" || acceptBody != token || strings.Contains(acceptURI, token) || idempotency != "request-123" {
		t.Fatalf("uri=%q body=%q key=%q", acceptURI, acceptBody, idempotency)
	}
	if contexts.selection.APIOrigin != sessions.Origin() || contexts.selection.UserID != "user-1" || contexts.selection.WorkspaceID != workspaceID {
		t.Fatalf("selection=%#v", contexts.selection)
	}
}

func TestJoinContextFailureIsExplicitPartialAndDoesNotReaccept(t *testing.T) {
	const workspaceID = "123e4567-e89b-42d3-a456-426614174000"
	token := strings.Repeat("t", 43)
	accepts := 0
	var key string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/workspace-invitations/accept" {
			t.Fatalf("unexpected %s", r.URL.Path)
		}
		accepts++
		key = r.Header.Get("Idempotency-Key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"workspace":{"id":"` + workspaceID + `","slug":"team","name":"Team","status":"active","version":1,"currentUserRole":"member","createdAt":"now","updatedAt":"now"}}`))
	}))
	defer server.Close()
	api, _ := client.New(server.URL, server.Client())
	sessions := &fakeSessions{}
	contexts := &memoryContexts{saveErr: errors.New("disk full")}
	service := NewService(api, sessions, contexts)
	result, err := service.Join(context.Background(), token, "same-request-id")
	var partial *PartialJoinError
	if !errors.As(err, &partial) || !result.Accepted || result.Selected || result.IdempotencyKey != "same-request-id" || result.SelectionError == "" || accepts != 1 || key != "same-request-id" {
		t.Fatalf("result=%#v accepts=%d key=%q err=%v", result, accepts, key, err)
	}
}

func TestInvitationCreateRetryUsesExplicitStableIdempotencyKey(t *testing.T) {
	const workspaceID = "123e4567-e89b-42d3-a456-426614174000"
	var keys []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"workspace":{"id":"` + workspaceID + `","slug":"team","name":"Team","status":"active","version":1,"currentUserRole":"owner","createdAt":"now","updatedAt":"now"}}`))
			return
		}
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"invitation":{"id":"123e4567-e89b-42d3-a456-426614174001","workspaceId":"` + workspaceID + `","role":"member","status":"pending","version":1,"createdAt":"now","expiresAt":"later"},"inviteToken":"deterministic-token"}`))
	}))
	defer server.Close()
	api, _ := client.New(server.URL, server.Client())
	service := NewService(api, &fakeSessions{}, &memoryContexts{selection: Selection{APIOrigin: "https://example.test", UserID: "user-1", WorkspaceID: workspaceID}})
	first, err := service.Invite(context.Background(), "", client.RoleMember, 300, "invite-request-123")
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Invite(context.Background(), "", client.RoleMember, 300, "invite-request-123")
	if err != nil {
		t.Fatal(err)
	}
	if first.InviteToken != "deterministic-token" || second.InviteToken != first.InviteToken || len(keys) != 2 || keys[0] != "invite-request-123" || keys[1] != keys[0] {
		t.Fatalf("first=%#v second=%#v keys=%v", first, second, keys)
	}
}
