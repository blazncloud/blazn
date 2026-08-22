package project

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/blazncloud/blazn/internal/client"
	workspacepkg "github.com/blazncloud/blazn/internal/workspace"
)

type API interface {
	CreateProject(context.Context, string, string, string, client.CreateProjectRequest) (client.ProjectEnvelope, error)
	ListProjects(context.Context, string, string, string, string) (client.ProjectList, error)
	GetProject(context.Context, string, string, string) (client.ProjectEnvelope, error)
	UpdateProject(context.Context, string, string, string, string, client.UpdateProjectRequest) (client.ProjectEnvelope, error)
}

type Service struct {
	api      API
	sessions workspacepkg.SessionProvider
	contexts workspacepkg.ContextStore
	now      func() time.Time
}

var ErrNoProject = errors.New("no Project is selected")

type Update struct {
	Name        *string
	Description *string
	Status      *client.ProjectStatus
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
	return &Service{api: api, sessions: sessions, contexts: contexts, now: time.Now}
}

func (s *Service) Create(ctx context.Context, name, slug, kind, description, requestID string) (client.ProjectEnvelope, error) {
	selection, session, err := s.selection(ctx)
	if err != nil {
		return client.ProjectEnvelope{}, err
	}
	return withSession(ctx, s.sessions, session, func(current workspacepkg.Session) (client.ProjectEnvelope, error) {
		return s.api.CreateProject(ctx, current.AccessToken, selection.WorkspaceID, requestID, client.CreateProjectRequest{Name: name, Slug: slug, Kind: kind, Description: description})
	})
}

func (s *Service) List(ctx context.Context, status, cursor string) (client.ProjectList, error) {
	selection, session, err := s.selection(ctx)
	if err != nil {
		return client.ProjectList{}, err
	}
	return withSession(ctx, s.sessions, session, func(current workspacepkg.Session) (client.ProjectList, error) {
		return s.api.ListProjects(ctx, current.AccessToken, selection.WorkspaceID, status, cursor)
	})
}

func (s *Service) Get(ctx context.Context, value string) (client.ProjectEnvelope, error) {
	return s.resolve(ctx, value)
}

func (s *Service) Use(ctx context.Context, value string) (client.ProjectEnvelope, error) {
	project, err := s.resolve(ctx, value)
	if err != nil {
		return client.ProjectEnvelope{}, err
	}
	if selection.WorkspaceID != project.Project.WorkspaceID {
		return client.ProjectEnvelope{}, errors.New("Workspace selection changed while selecting the Project; retry")
	}
	selection, session, err := s.selection(ctx)
	if err != nil {
		return client.ProjectEnvelope{}, err
	}
	if selection.WorkspaceID != project.Project.WorkspaceID {
		return client.ProjectEnvelope{}, errors.New("Workspace selection changed while updating the Project; retry")
	}
	selection.ProjectID = project.Project.ID
	selection.SelectedAt = s.now().UTC()
	selection.UserID = session.UserID
	if err := s.contexts.Save(selection); err != nil {
		return client.ProjectEnvelope{}, err
	}
	return project, nil
}

func (s *Service) Update(ctx context.Context, value, requestID string, expectedVersion int, changes Update) (client.ProjectEnvelope, error) {
	project, err := s.resolve(ctx, value)
	if err != nil {
		return client.ProjectEnvelope{}, err
	}
	selection, session, err := s.selection(ctx)
	if err != nil {
		return client.ProjectEnvelope{}, err
	}
	request := client.UpdateProjectRequest{ExpectedVersion: expectedVersion, Name: changes.Name, Description: changes.Description, Status: changes.Status}
	return withSession(ctx, s.sessions, session, func(current workspacepkg.Session) (client.ProjectEnvelope, error) {
		return s.api.UpdateProject(ctx, current.AccessToken, selection.WorkspaceID, project.Project.ID, requestID, request)
	})
}

func (s *Service) CurrentSelection(ctx context.Context) (workspacepkg.Selection, error) {
	selection, _, err := s.selection(ctx)
	return selection, err
}

func (s *Service) resolve(ctx context.Context, value string) (client.ProjectEnvelope, error) {
	selection, session, err := s.selection(ctx)
	if err != nil {
		return client.ProjectEnvelope{}, err
	}
	if value == "" {
		if selection.ProjectID == "" {
			return client.ProjectEnvelope{}, ErrNoProject
		}
		value = selection.ProjectID
	}
	if uuidPattern.MatchString(value) {
		return withSession(ctx, s.sessions, session, func(current workspacepkg.Session) (client.ProjectEnvelope, error) {
			return s.api.GetProject(ctx, current.AccessToken, selection.WorkspaceID, value)
		})
	}
	cursor := ""
	for page := 0; ; page++ {
		if page >= 1000 {
			return client.ProjectEnvelope{}, errors.New("Project slug resolution exceeded the pagination limit")
		}
		projects, err := withSession(ctx, s.sessions, session, func(current workspacepkg.Session) (client.ProjectList, error) {
			return s.api.ListProjects(ctx, current.AccessToken, selection.WorkspaceID, "all", cursor)
		})
		if err != nil {
			return client.ProjectEnvelope{}, err
		}
		for _, project := range projects.Items {
			if project.Slug == value {
				return client.ProjectEnvelope{Project: project}, nil
			}
		}
		if projects.NextCursor == nil || *projects.NextCursor == "" {
			break
		}
		if *projects.NextCursor == cursor {
			return client.ProjectEnvelope{}, errors.New("Project pagination returned a repeated cursor")
		}
		cursor = *projects.NextCursor
	}
	return client.ProjectEnvelope{}, &client.APIError{StatusCode: http.StatusNotFound, Body: client.ErrorBody{Code: "project_not_found", Message: "project was not found"}}
}

func (s *Service) selection(ctx context.Context) (workspacepkg.Selection, workspacepkg.Session, error) {
	session, err := s.sessions.Session(ctx, false)
	selection := workspacepkg.Selection{SchemaVersion: 1, APIOrigin: s.sessions.Origin()}
	if err != nil {
		return selection, workspacepkg.Session{}, err
	}
	selection, err = s.contexts.Load(s.sessions.Origin(), session.UserID)
	return selection, session, err
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

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-5][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)

func ParseStatus(value string) (string, error) {
	if value == "" || value == "active" || value == "archived" || value == "all" {
		return value, nil
	}
	return "", fmt.Errorf("invalid Project status %q", value)
}
