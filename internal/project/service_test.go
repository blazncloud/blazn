package project

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/blazncloud/blazn/internal/client"
	workspacepkg "github.com/blazncloud/blazn/internal/workspace"
)

const testWorkspaceID = "00000000-0000-4000-8000-000000000001"
const testProjectID = "00000000-0000-4000-8000-000000000002"

type fakeSessions struct{ forced int }

func (s *fakeSessions) Origin() string { return "https://example.test" }
func (s *fakeSessions) Session(_ context.Context, force bool) (workspacepkg.Session, error) {
	if force {
		s.forced++
		return workspacepkg.Session{AccessToken: "new-token", UserID: "user-1"}, nil
	}
	return workspacepkg.Session{AccessToken: "old-token", UserID: "user-1"}, nil
}

type memoryContexts struct {
	selection workspacepkg.Selection
	saved     workspacepkg.Selection
}

func (m *memoryContexts) Load(origin, userID string) (workspacepkg.Selection, error) {
	if m.selection.WorkspaceID == "" {
		return workspacepkg.Selection{}, workspacepkg.ErrNoContext
	}
	return m.selection, nil
}
func (m *memoryContexts) Save(value workspacepkg.Selection) error { m.saved, m.selection = value, value; return nil }

type fakeAPI struct {
	projects        []client.Project
	workspaceID     string
	idempotencyKey  string
	status          string
	update          client.UpdateProjectRequest
	getAccessTokens []string
	pages           map[string]client.ProjectList
}

func (f *fakeAPI) CreateProject(_ context.Context, _ string, workspaceID, key string, request client.CreateProjectRequest) (client.ProjectEnvelope, error) {
	f.workspaceID, f.idempotencyKey = workspaceID, key
	project := fixtureProject()
	project.Name, project.Slug, project.Kind, project.Description = request.Name, request.Slug, request.Kind, request.Description
	return client.ProjectEnvelope{Project: project}, nil
}
func (f *fakeAPI) ListProjects(_ context.Context, _ string, workspaceID, status, cursor string) (client.ProjectList, error) {
	f.workspaceID, f.status = workspaceID, status
	if f.pages != nil {
		return f.pages[cursor], nil
	}
	return client.ProjectList{Items: append([]client.Project(nil), f.projects...)}, nil
}

func TestProjectUsePagesUntilSlugIsFound(t *testing.T) {
	next := "page-2"
	api := &fakeAPI{pages: map[string]client.ProjectList{"": {Items: []client.Project{}, NextCursor: &next}, "page-2": {Items: []client.Project{fixtureProject()}}}}
	contexts := &memoryContexts{selection: workspacepkg.Selection{SchemaVersion: 1, APIOrigin: "https://example.test", UserID: "user-1", WorkspaceID: testWorkspaceID, SelectedAt: time.Now().UTC()}}
	selected, err := NewService(api, &fakeSessions{}, contexts).Use(context.Background(), "launch-video")
	if err != nil || selected.Project.ID != testProjectID || contexts.saved.ProjectID != testProjectID {
		t.Fatalf("selected=%#v saved=%#v err=%v", selected, contexts.saved, err)
	}
}
func (f *fakeAPI) GetProject(_ context.Context, accessToken, workspaceID, projectID string) (client.ProjectEnvelope, error) {
	f.workspaceID = workspaceID
	f.getAccessTokens = append(f.getAccessTokens, accessToken)
	if len(f.getAccessTokens) == 1 && accessToken == "old-token" {
		return client.ProjectEnvelope{}, &client.APIError{StatusCode: 401, Body: client.ErrorBody{Code: "access_expired", Message: "expired"}}
	}
	project := fixtureProject()
	project.ID = projectID
	return client.ProjectEnvelope{Project: project}, nil
}
func (f *fakeAPI) UpdateProject(_ context.Context, _ string, workspaceID, _ string, key string, request client.UpdateProjectRequest) (client.ProjectEnvelope, error) {
	f.workspaceID, f.idempotencyKey, f.update = workspaceID, key, request
	project := fixtureProject()
	project.Version = request.ExpectedVersion + 1
	return client.ProjectEnvelope{Project: project}, nil
}

func TestProjectServiceUsesSelectedWorkspaceAndExplicitMutationIdentity(t *testing.T) {
	api := &fakeAPI{}
	contexts := &memoryContexts{selection: workspacepkg.Selection{SchemaVersion: 1, APIOrigin: "https://example.test", UserID: "user-1", WorkspaceID: testWorkspaceID, SelectedAt: time.Now().UTC()}}
	service := NewService(api, &fakeSessions{}, contexts)
	created, err := service.Create(context.Background(), "Launch", "launch", "content", "Brief", "request-create-1")
	if err != nil || api.workspaceID != testWorkspaceID || api.idempotencyKey != "request-create-1" || created.Project.Kind != "content" {
		t.Fatalf("created=%#v api=%#v err=%v", created, api, err)
	}
	description := "Updated"
	status := client.ProjectStatusArchived
	updated, err := service.Update(context.Background(), testProjectID, "request-update-1", 1, Update{Description: &description, Status: &status})
	if err != nil || updated.Project.Version != 2 || api.update.Description == nil || *api.update.Description != "Updated" || api.update.Status == nil || *api.update.Status != client.ProjectStatusArchived {
		t.Fatalf("updated=%#v request=%#v err=%v", updated, api.update, err)
	}
}

func TestProjectUseResolvesSlugAndPersistsScopedSelection(t *testing.T) {
	api := &fakeAPI{projects: []client.Project{fixtureProject()}}
	contexts := &memoryContexts{selection: workspacepkg.Selection{SchemaVersion: 1, APIOrigin: "https://example.test", UserID: "user-1", WorkspaceID: testWorkspaceID, SelectedAt: time.Now().UTC()}}
	service := NewService(api, &fakeSessions{}, contexts)
	service.now = func() time.Time { return time.Date(2026, 8, 22, 1, 2, 3, 0, time.UTC) }
	selected, err := service.Use(context.Background(), "launch-video")
	if err != nil || selected.Project.ID != testProjectID || api.status != "all" || contexts.saved.ProjectID != testProjectID || contexts.saved.WorkspaceID != testWorkspaceID || !contexts.saved.SelectedAt.Equal(service.now()) {
		t.Fatalf("selected=%#v saved=%#v api=%#v err=%v", selected, contexts.saved, api, err)
	}
}

func TestProjectGetRefreshesAccessAndRequiresSelections(t *testing.T) {
	api := &fakeAPI{}
	sessions := &fakeSessions{}
	contexts := &memoryContexts{selection: workspacepkg.Selection{SchemaVersion: 1, APIOrigin: "https://example.test", UserID: "user-1", WorkspaceID: testWorkspaceID, ProjectID: testProjectID, SelectedAt: time.Now().UTC()}}
	service := NewService(api, sessions, contexts)
	project, err := service.Get(context.Background(), "")
	if err != nil || project.Project.ID != testProjectID || sessions.forced != 1 || len(api.getAccessTokens) != 2 || api.getAccessTokens[1] != "new-token" {
		t.Fatalf("project=%#v tokens=%v forced=%d err=%v", project, api.getAccessTokens, sessions.forced, err)
	}
	contexts.selection.ProjectID = ""
	if _, err := service.Get(context.Background(), ""); !errors.Is(err, ErrNoProject) {
		t.Fatalf("missing Project err=%v", err)
	}
	contexts.selection = workspacepkg.Selection{}
	if _, err := service.List(context.Background(), "active"); !errors.Is(err, workspacepkg.ErrNoContext) {
		t.Fatalf("missing Workspace err=%v", err)
	}
}

func fixtureProject() client.Project {
	return client.Project{ID: testProjectID, WorkspaceID: testWorkspaceID, Slug: "launch-video", Kind: "content", Name: "Launch Video", Status: client.ProjectStatusActive, Version: 1, CreatedBy: "user-1", CreatedAt: "now", UpdatedAt: "now"}
}
