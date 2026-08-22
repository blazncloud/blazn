package workspace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/KingJammin/blazn/internal/client"
)

type API interface {
	CreateWorkspace(context.Context, string, string, client.CreateWorkspaceRequest) (client.WorkspaceEnvelope, error)
	ListWorkspaces(context.Context, string, string) (client.WorkspaceList, error)
	GetWorkspace(context.Context, string, string) (client.WorkspaceEnvelope, error)
	UpdateWorkspace(context.Context, string, string, string, client.UpdateWorkspaceRequest) (client.WorkspaceEnvelope, error)
	CreateWorkspaceInvitation(context.Context, string, string, string, client.CreateInvitationRequest) (client.InvitationCreated, error)
	ListWorkspaceInvitations(context.Context, string, string, string) (client.InvitationList, error)
	RevokeWorkspaceInvitation(context.Context, string, string, string, int, string) (client.MutationResult, error)
	AcceptWorkspaceInvitation(context.Context, string, string, client.AcceptInvitationRequest) (client.WorkspaceEnvelope, error)
	ListWorkspaceMembers(context.Context, string, string, string) (client.MembershipList, error)
	UpdateWorkspaceMember(context.Context, string, string, string, string, client.UpdateMembershipRequest) (client.Membership, error)
	RemoveWorkspaceMember(context.Context, string, string, string, int, string) (client.MutationResult, error)
	LeaveWorkspace(context.Context, string, string, int, string) (client.MutationResult, error)
	StreamWorkspaceEvents(context.Context, string, string, string) (*client.WorkspaceEventStream, error)
}

type Service struct {
	api       API
	streamAPI API
	sessions  SessionProvider
	contexts  ContextStore
	now       func() time.Time
	newKey    func() (string, error)
}

func NewDefaultService() (*Service, error) {
	sessions, err := NewDefaultSessionProvider()
	if err != nil {
		return nil, err
	}
	provider := sessions.(*authSessionProvider)
	streamAPI, err := client.New(provider.baseURL, &http.Client{})
	if err != nil {
		return nil, err
	}
	contexts, err := NewFileContextStore()
	if err != nil {
		return nil, err
	}
	service := NewService(provider.api, sessions, contexts)
	service.streamAPI = streamAPI
	return service, nil
}

func NewService(api API, sessions SessionProvider, contexts ContextStore) *Service {
	return &Service{api: api, streamAPI: api, sessions: sessions, contexts: contexts, now: time.Now, newKey: randomKey}
}

func randomKey() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func (s *Service) Create(ctx context.Context, name, slug, requestID string) (client.WorkspaceEnvelope, error) {
	key, err := s.idempotencyKey(requestID)
	if err != nil {
		return client.WorkspaceEnvelope{}, err
	}
	return withSession(ctx, s.sessions, func(session Session) (client.WorkspaceEnvelope, error) {
		return s.api.CreateWorkspace(ctx, session.AccessToken, key, client.CreateWorkspaceRequest{Name: name, Slug: slug})
	})
}

func (s *Service) List(ctx context.Context) (client.WorkspaceList, error) {
	return withSession(ctx, s.sessions, func(session Session) (client.WorkspaceList, error) {
		return s.api.ListWorkspaces(ctx, session.AccessToken, "")
	})
}

func (s *Service) Get(ctx context.Context, value string) (client.WorkspaceEnvelope, error) {
	return s.resolve(ctx, value)
}

func (s *Service) Edit(ctx context.Context, value, name string, expected int, requestID string) (client.WorkspaceEnvelope, error) {
	workspace, err := s.resolve(ctx, value)
	if err != nil {
		return client.WorkspaceEnvelope{}, err
	}
	key, err := s.idempotencyKey(requestID)
	if err != nil {
		return client.WorkspaceEnvelope{}, err
	}
	return withSession(ctx, s.sessions, func(session Session) (client.WorkspaceEnvelope, error) {
		return s.api.UpdateWorkspace(ctx, session.AccessToken, workspace.Workspace.ID, key, client.UpdateWorkspaceRequest{Name: name, ExpectedVersion: expected})
	})
}

func (s *Service) Use(ctx context.Context, value string) (client.WorkspaceEnvelope, error) {
	workspace, err := s.resolve(ctx, value)
	if err != nil {
		return workspace, err
	}
	session, err := s.sessions.Session(ctx, false)
	if err != nil {
		return workspace, err
	}
	err = s.contexts.Save(Selection{APIOrigin: s.sessions.Origin(), UserID: session.UserID, WorkspaceID: workspace.Workspace.ID, SelectedAt: s.now().UTC()})
	return workspace, err
}

func (s *Service) Invite(ctx context.Context, value string, role client.Role, expires int, requestID string) (client.InvitationCreated, error) {
	workspace, err := s.resolve(ctx, value)
	if err != nil {
		return client.InvitationCreated{}, err
	}
	key, err := s.idempotencyKey(requestID)
	if err != nil {
		return client.InvitationCreated{}, err
	}
	return withSession(ctx, s.sessions, func(session Session) (client.InvitationCreated, error) {
		return s.api.CreateWorkspaceInvitation(ctx, session.AccessToken, workspace.Workspace.ID, key, client.CreateInvitationRequest{Role: role, ExpiresIn: expires})
	})
}

func (s *Service) Invitations(ctx context.Context, value string) (client.InvitationList, error) {
	workspace, err := s.resolve(ctx, value)
	if err != nil {
		return client.InvitationList{}, err
	}
	return withSession(ctx, s.sessions, func(session Session) (client.InvitationList, error) {
		return s.api.ListWorkspaceInvitations(ctx, session.AccessToken, workspace.Workspace.ID, "")
	})
}

func (s *Service) RevokeInvitation(ctx context.Context, workspaceValue, invitationID string, version int, requestID string) (client.MutationResult, error) {
	workspace, err := s.resolve(ctx, workspaceValue)
	if err != nil {
		return client.MutationResult{}, err
	}
	key, err := s.idempotencyKey(requestID)
	if err != nil {
		return client.MutationResult{}, err
	}
	return withSession(ctx, s.sessions, func(session Session) (client.MutationResult, error) {
		return s.api.RevokeWorkspaceInvitation(ctx, session.AccessToken, workspace.Workspace.ID, invitationID, version, key)
	})
}

func (s *Service) Join(ctx context.Context, inviteToken, requestID string) (client.WorkspaceEnvelope, error) {
	key, err := s.idempotencyKey(requestID)
	if err != nil {
		return client.WorkspaceEnvelope{}, err
	}
	workspace, err := withSession(ctx, s.sessions, func(session Session) (client.WorkspaceEnvelope, error) {
		return s.api.AcceptWorkspaceInvitation(ctx, session.AccessToken, key, client.AcceptInvitationRequest{InviteToken: inviteToken})
	})
	if err != nil {
		return workspace, err
	}
	_, err = s.Use(ctx, workspace.Workspace.ID)
	return workspace, err
}

func (s *Service) Members(ctx context.Context, value string) (client.MembershipList, error) {
	workspace, err := s.resolve(ctx, value)
	if err != nil {
		return client.MembershipList{}, err
	}
	return withSession(ctx, s.sessions, func(session Session) (client.MembershipList, error) {
		return s.api.ListWorkspaceMembers(ctx, session.AccessToken, workspace.Workspace.ID, "")
	})
}

func (s *Service) SetRole(ctx context.Context, workspaceValue, userID string, role client.Role, version int, requestID string) (client.Membership, error) {
	workspace, err := s.resolve(ctx, workspaceValue)
	if err != nil {
		return client.Membership{}, err
	}
	key, err := s.idempotencyKey(requestID)
	if err != nil {
		return client.Membership{}, err
	}
	return withSession(ctx, s.sessions, func(session Session) (client.Membership, error) {
		return s.api.UpdateWorkspaceMember(ctx, session.AccessToken, workspace.Workspace.ID, userID, key, client.UpdateMembershipRequest{Role: role, ExpectedVersion: version})
	})
}

func (s *Service) RemoveMember(ctx context.Context, workspaceValue, userID string, version int, requestID string) (client.MutationResult, error) {
	workspace, err := s.resolve(ctx, workspaceValue)
	if err != nil {
		return client.MutationResult{}, err
	}
	key, err := s.idempotencyKey(requestID)
	if err != nil {
		return client.MutationResult{}, err
	}
	return withSession(ctx, s.sessions, func(session Session) (client.MutationResult, error) {
		return s.api.RemoveWorkspaceMember(ctx, session.AccessToken, workspace.Workspace.ID, userID, version, key)
	})
}

func (s *Service) Leave(ctx context.Context, workspaceValue, requestID string) (client.MutationResult, error) {
	workspace, err := s.resolve(ctx, workspaceValue)
	if err != nil {
		return client.MutationResult{}, err
	}
	members, err := s.Members(ctx, workspace.Workspace.ID)
	if err != nil {
		return client.MutationResult{}, err
	}
	session, err := s.sessions.Session(ctx, false)
	if err != nil {
		return client.MutationResult{}, err
	}
	version := 0
	for _, member := range members.Items {
		if member.User.ID == session.UserID && member.Status == "active" {
			version = member.Version
			break
		}
	}
	if version < 1 {
		return client.MutationResult{}, errors.New("current membership was not found")
	}
	key, err := s.idempotencyKey(requestID)
	if err != nil {
		return client.MutationResult{}, err
	}
	return withSession(ctx, s.sessions, func(session Session) (client.MutationResult, error) {
		return s.api.LeaveWorkspace(ctx, session.AccessToken, workspace.Workspace.ID, version, key)
	})
}

func (s *Service) Events(ctx context.Context, workspaceValue, cursor string) (*client.WorkspaceEventStream, error) {
	workspace, err := s.resolve(ctx, workspaceValue)
	if err != nil {
		return nil, err
	}
	session, err := s.sessions.Session(ctx, false)
	if err != nil {
		return nil, err
	}
	stream, err := s.streamAPI.StreamWorkspaceEvents(ctx, session.AccessToken, workspace.Workspace.ID, cursor)
	if client.IsCode(err, "access_expired") {
		session, refreshErr := s.sessions.Session(ctx, true)
		if refreshErr != nil {
			return nil, refreshErr
		}
		return s.streamAPI.StreamWorkspaceEvents(ctx, session.AccessToken, workspace.Workspace.ID, cursor)
	}
	return stream, err
}

func (s *Service) resolve(ctx context.Context, value string) (client.WorkspaceEnvelope, error) {
	if value == "" {
		session, err := s.sessions.Session(ctx, false)
		if err != nil {
			return client.WorkspaceEnvelope{}, err
		}
		selection, err := s.contexts.Load(s.sessions.Origin(), session.UserID)
		if err != nil {
			return client.WorkspaceEnvelope{}, err
		}
		value = selection.WorkspaceID
	}
	if uuidPattern.MatchString(value) {
		return withSession(ctx, s.sessions, func(session Session) (client.WorkspaceEnvelope, error) {
			return s.api.GetWorkspace(ctx, session.AccessToken, value)
		})
	}
	list, err := withSession(ctx, s.sessions, func(session Session) (client.WorkspaceList, error) {
		return s.api.ListWorkspaces(ctx, session.AccessToken, "")
	})
	if err != nil {
		return client.WorkspaceEnvelope{}, err
	}
	for _, workspace := range list.Items {
		if workspace.Slug == value || workspace.ID == value {
			return client.WorkspaceEnvelope{Workspace: workspace}, nil
		}
	}
	return client.WorkspaceEnvelope{}, &client.APIError{StatusCode: 404, Body: client.ErrorBody{Code: "workspace_not_found", Message: "workspace was not found"}}
}

func (s *Service) idempotencyKey(value string) (string, error) {
	if value != "" {
		if len(value) < 8 || len(value) > 128 {
			return "", errors.New("request ID must contain between 8 and 128 characters")
		}
		return value, nil
	}
	return s.newKey()
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-5][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)

func withSession[T any](ctx context.Context, sessions SessionProvider, action func(Session) (T, error)) (T, error) {
	session, err := sessions.Session(ctx, false)
	if err != nil {
		var zero T
		return zero, err
	}
	result, err := action(session)
	if client.IsCode(err, "access_expired") {
		session, refreshErr := sessions.Session(ctx, true)
		if refreshErr != nil {
			var zero T
			return zero, refreshErr
		}
		return action(session)
	}
	return result, err
}

func ParseRole(value string) (client.Role, error) {
	role := client.Role(value)
	switch role {
	case client.RoleAdministrator, client.RoleOperator, client.RoleMember, client.RoleViewer:
		return role, nil
	}
	return "", fmt.Errorf("invalid role %q", value)
}

func ParseExpiresIn(value string) (int, error) {
	duration, err := time.ParseDuration(value)
	if err != nil || duration%time.Second != 0 {
		return 0, errors.New("expires-in must be a whole-second duration")
	}
	seconds := int(duration / time.Second)
	if seconds < 300 || seconds > 604800 {
		return 0, errors.New("expires-in must be between 5m and 168h")
	}
	return seconds, nil
}
