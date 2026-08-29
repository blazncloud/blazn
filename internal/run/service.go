package run

import (
	"context"
	"net/http"
	"time"

	"github.com/blazncloud/blazn/internal/client"
	projectpkg "github.com/blazncloud/blazn/internal/project"
	workspacepkg "github.com/blazncloud/blazn/internal/workspace"
)

type API interface {
	ListRunMessages(context.Context, string, string, string, string, string) (client.RunMessageList, error)
	SendRunMessage(context.Context, string, string, string, string, string, client.SendRunMessageRequest) (client.RunMessageEnvelope, error)
	ClaimRunMessage(context.Context, string, string, string, string, string, client.ClaimRunMessageRequest) (client.RunMessageClaimEnvelope, error)
	DeliverRunMessage(context.Context, string, string, string, string, string, string, client.DeliverRunMessageRequest) (client.RunMessageEnvelope, error)
	CreateRun(context.Context, string, string, string, string, client.CreateRunRequest) (client.RunEnvelope, error)
	ListRuns(context.Context, string, string, string, string, string) (client.RunList, error)
	GetRun(context.Context, string, string, string, string) (client.RunEnvelope, error)
	CancelRun(context.Context, string, string, string, string, string, client.CancelRunRequest) (client.RunEnvelope, error)
	ListRunEvents(context.Context, string, string, string, string, string) (client.RunEventList, error)
	ListRunProgress(context.Context, string, string, string, string) (client.RunProgressList, error)
	ListRunArtifacts(context.Context, string, string, string, string, string) (client.ArtifactList, error)
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

func (s *Service) ClaimMessage(ctx context.Context, runID, requestID string, leaseSeconds int) (client.RunMessageClaimEnvelope, error) {
	selection, session, err := s.selection(ctx)
	if err != nil {
		return client.RunMessageClaimEnvelope{}, err
	}
	return withSession(ctx, s.sessions, session, func(current workspacepkg.Session) (client.RunMessageClaimEnvelope, error) {
		return s.api.ClaimRunMessage(ctx, current.AccessToken, selection.WorkspaceID, selection.ProjectID, runID, requestID, client.ClaimRunMessageRequest{LeaseSeconds: leaseSeconds})
	})
}

func (s *Service) DeliverMessage(ctx context.Context, runID, messageID, claimID, requestID string) (client.RunMessageEnvelope, error) {
	selection, session, err := s.selection(ctx)
	if err != nil {
		return client.RunMessageEnvelope{}, err
	}
	return withSession(ctx, s.sessions, session, func(current workspacepkg.Session) (client.RunMessageEnvelope, error) {
		return s.api.DeliverRunMessage(ctx, current.AccessToken, selection.WorkspaceID, selection.ProjectID, runID, messageID, requestID, client.DeliverRunMessageRequest{ClaimID: claimID})
	})
}

func (s *Service) Create(ctx context.Context, requestID string, request client.CreateRunRequest) (client.RunEnvelope, error) {
	selection, session, err := s.selection(ctx)
	if err != nil {
		return client.RunEnvelope{}, err
	}
	return withSession(ctx, s.sessions, session, func(current workspacepkg.Session) (client.RunEnvelope, error) {
		return s.api.CreateRun(ctx, current.AccessToken, selection.WorkspaceID, selection.ProjectID, requestID, request)
	})
}

func (s *Service) List(ctx context.Context, status, cursor string) (client.RunList, error) {
	selection, session, err := s.selection(ctx)
	if err != nil {
		return client.RunList{}, err
	}
	return withSession(ctx, s.sessions, session, func(current workspacepkg.Session) (client.RunList, error) {
		return s.api.ListRuns(ctx, current.AccessToken, selection.WorkspaceID, selection.ProjectID, status, cursor)
	})
}

func (s *Service) Get(ctx context.Context, runID string) (client.RunEnvelope, error) {
	selection, session, err := s.selection(ctx)
	if err != nil {
		return client.RunEnvelope{}, err
	}
	return withSession(ctx, s.sessions, session, func(current workspacepkg.Session) (client.RunEnvelope, error) {
		return s.api.GetRun(ctx, current.AccessToken, selection.WorkspaceID, selection.ProjectID, runID)
	})
}

func (s *Service) Cancel(ctx context.Context, runID, requestID string, expectedVersion int) (client.RunEnvelope, error) {
	selection, session, err := s.selection(ctx)
	if err != nil {
		return client.RunEnvelope{}, err
	}
	return withSession(ctx, s.sessions, session, func(current workspacepkg.Session) (client.RunEnvelope, error) {
		return s.api.CancelRun(ctx, current.AccessToken, selection.WorkspaceID, selection.ProjectID, runID, requestID, client.CancelRunRequest{ExpectedVersion: expectedVersion})
	})
}

func (s *Service) Events(ctx context.Context, runID, cursor string) (client.RunEventList, error) {
	selection, session, err := s.selection(ctx)
	if err != nil {
		return client.RunEventList{}, err
	}
	return withSession(ctx, s.sessions, session, func(current workspacepkg.Session) (client.RunEventList, error) {
		return s.api.ListRunEvents(ctx, current.AccessToken, selection.WorkspaceID, selection.ProjectID, runID, cursor)
	})
}

func (s *Service) Progress(ctx context.Context, runID string) (client.RunProgressList, error) {
	selection, session, err := s.selection(ctx)
	if err != nil {
		return client.RunProgressList{}, err
	}
	return withSession(ctx, s.sessions, session, func(current workspacepkg.Session) (client.RunProgressList, error) {
		return s.api.ListRunProgress(ctx, current.AccessToken, selection.WorkspaceID, selection.ProjectID, runID)
	})
}

func (s *Service) Artifacts(ctx context.Context, runID, cursor string) (client.ArtifactList, error) {
	selection, session, err := s.selection(ctx)
	if err != nil {
		return client.ArtifactList{}, err
	}
	return withSession(ctx, s.sessions, session, func(current workspacepkg.Session) (client.ArtifactList, error) {
		return s.api.ListRunArtifacts(ctx, current.AccessToken, selection.WorkspaceID, selection.ProjectID, runID, cursor)
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
