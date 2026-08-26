package run

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/blazncloud/blazn/internal/client"
	projectpkg "github.com/blazncloud/blazn/internal/project"
	workspacepkg "github.com/blazncloud/blazn/internal/workspace"
)

type API interface {
	ListRunMessages(context.Context, string, string, string, string, string) (client.RunMessageList, error)
	SendRunMessage(context.Context, string, string, string, string, string, client.SendRunMessageRequest) (client.RunMessageEnvelope, error)
}

type Service struct {
	api      API
	sessions workspacepkg.SessionProvider
	contexts workspacepkg.ContextStore
}

func NewDefaultService() (*Service, error) {
	sessions, err := workspacepkg.NewDefaultSessionProvider()
	if err != nil {
		return nil, err
	}
	api, err := client.New(sessions.Origin(), &http.Client{Timeout: 30 * time.Second})
	if err != nil {
		return nil, err
	}
	contexts, err := workspacepkg.NewFileContextStore()
	if err != nil {
		return nil, err
	}
	return NewService(api, sessions, contexts), nil
}

func NewService(api API, sessions workspacepkg.SessionProvider, contexts workspacepkg.ContextStore) *Service {
	return &Service{api: api, sessions: sessions, contexts: contexts}
}

func (s *Service) ListMessages(ctx context.Context, runID, cursor string) (client.RunMessageList, error) {
	selection, session, err := s.selection(ctx)
	if err != nil {
		return client.RunMessageList{}, err
	}
	return withSession(ctx, s.sessions, session, func(current workspacepkg.Session) (client.RunMessageList, error) {
		return s.api.ListRunMessages(ctx, current.AccessToken, selection.WorkspaceID, selection.ProjectID, runID, cursor)
	})
}

func (s *Service) SendMessage(ctx context.Context, runID, requestID string, request client.SendRunMessageRequest) (client.RunMessageEnvelope, error) {
	selection, session, err := s.selection(ctx)
	if err != nil {
		return client.RunMessageEnvelope{}, err
	}
	return withSession(ctx, s.sessions, session, func(current workspacepkg.Session) (client.RunMessageEnvelope, error) {
		return s.api.SendRunMessage(ctx, current.AccessToken, selection.WorkspaceID, selection.ProjectID, runID, requestID, request)
	})
}

func (s *Service) selection(ctx context.Context) (workspacepkg.Selection, workspacepkg.Session, error) {
	session, err := s.sessions.Session(ctx, false)
	if err != nil {
		return workspacepkg.Selection{}, workspacepkg.Session{}, err
	}
	selection, err := s.contexts.Load(s.sessions.Origin(), session.UserID)
	if err != nil {
		return selection, session, err
	}
	if selection.ProjectID == "" {
		return selection, session, projectpkg.ErrNoProject
	}
	return selection, session, nil
}

func withSession[T any](ctx context.Context, sessions workspacepkg.SessionProvider, session workspacepkg.Session, action func(workspacepkg.Session) (T, error)) (T, error) {
	result, err := action(session)
	if client.IsCode(err, "access_expired") {
		refreshed, refreshErr := sessions.Session(ctx, true)
		if refreshErr != nil {
			var zero T
			return zero, refreshErr
		}
		return action(refreshed)
	}
	return result, err
}

var ErrNoContext = errors.New("Run commands require a selected Workspace and Project")
