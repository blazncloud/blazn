package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/blazncloud/blazn/internal/client"
	projectpkg "github.com/blazncloud/blazn/internal/project"
	workspacepkg "github.com/blazncloud/blazn/internal/workspace"
)

const cliProjectTestWorkspaceID = "00000000-0000-4000-8000-000000000001"
const cliProjectTestProjectID = "00000000-0000-4000-8000-000000000002"

type fakeProjectCommands struct {
	createdName, requestID, status, value string
	changes                               projectpkg.Update
	version                               int
	selection                             workspacepkg.Selection
	err                                   error
}

func (f *fakeProjectCommands) Create(_ context.Context, name, _slug, _kind, _description, requestID string) (client.ProjectEnvelope, error) {
	f.createdName, f.requestID = name, requestID
	return client.ProjectEnvelope{Project: projectFixture()}, f.err
}
func (f *fakeProjectCommands) List(_ context.Context, status string) (client.ProjectList, error) {
	f.status = status
	return client.ProjectList{Items: []client.Project{projectFixture()}}, f.err
}
func (f *fakeProjectCommands) Get(_ context.Context, value string) (client.ProjectEnvelope, error) {
	f.value = value
	return client.ProjectEnvelope{Project: projectFixture()}, f.err
}
func (f *fakeProjectCommands) Use(_ context.Context, value string) (client.ProjectEnvelope, error) {
	f.value = value
	return client.ProjectEnvelope{Project: projectFixture()}, f.err
}
func (f *fakeProjectCommands) Update(_ context.Context, value, requestID string, version int, changes projectpkg.Update) (client.ProjectEnvelope, error) {
	f.value, f.requestID, f.version, f.changes = value, requestID, version, changes
	project := projectFixture()
	project.Version = version + 1
	return client.ProjectEnvelope{Project: project}, f.err
}
func (f *fakeProjectCommands) CurrentSelection(context.Context) (workspacepkg.Selection, error) {
	return f.selection, f.err
}

func projectApp(fake *fakeProjectCommands) (*App, *bytes.Buffer, *bytes.Buffer) {
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	app := New(stdout, stderr, testBuild)
	app.project = func() (projectCommands, error) { return fake, nil }
	app.stdinTTY = func() bool { return false }
	return app, stdout, stderr
}

func TestProjectCreateRequiresRequestIDAndRendersJSON(t *testing.T) {
	fake := &fakeProjectCommands{}
	app, stdout, stderr := projectApp(fake)
	if code := app.Run([]string{"project", "create", "Launch Video", "--kind", "content", "--request-id", "request-create-1", "--output=json"}); code != ExitSuccess || stderr.Len() != 0 || fake.createdName != "Launch Video" || fake.requestID != "request-create-1" || !strings.Contains(stdout.String(), `"slug":"launch-video"`) {
		t.Fatalf("code output=%q stderr=%q fake=%#v", stdout.String(), stderr.String(), fake)
	}
	app, _, stderr = projectApp(&fakeProjectCommands{})
	if code := app.Run([]string{"project", "create", "Launch"}); code != ExitUsage || !strings.Contains(stderr.String(), "--request-id") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
}

func TestProjectListUseGetAndEdit(t *testing.T) {
	fake := &fakeProjectCommands{}
	app, stdout, _ := projectApp(fake)
	if code := app.Run([]string{"project", "list", "--status", "all"}); code != ExitSuccess || fake.status != "all" || !strings.Contains(stdout.String(), "launch-video") {
		t.Fatalf("list code=%d output=%q fake=%#v", code, stdout.String(), fake)
	}
	app, _, _ = projectApp(fake)
	if code := app.Run([]string{"project", "use", "launch-video"}); code != ExitSuccess || fake.value != "launch-video" {
		t.Fatalf("use code=%d fake=%#v", code, fake)
	}
	app, _, _ = projectApp(fake)
	if code := app.Run([]string{"project", "get"}); code != ExitSuccess || fake.value != "" {
		t.Fatalf("get code=%d fake=%#v", code, fake)
	}
	app, _, _ = projectApp(fake)
	if code := app.Run([]string{"project", "edit", "launch-video", "--description", "", "--status", "archived", "--expected-version", "1", "--request-id", "request-update-1"}); code != ExitSuccess || fake.version != 1 || fake.changes.Description == nil || *fake.changes.Description != "" || fake.changes.Status == nil || *fake.changes.Status != client.ProjectStatusArchived {
		t.Fatalf("edit code=%d fake=%#v", code, fake)
	}
}

func TestProjectGetAndUseRejectFlagShapedProjectValues(t *testing.T) {
	for _, args := range [][]string{{"project", "get", "--status"}, {"project", "use", "--status"}} {
		fake := &fakeProjectCommands{}
		app, _, stderr := projectApp(fake)
		if code := app.Run(args); code != ExitUsage || fake.value != "" || stderr.Len() == 0 {
			t.Fatalf("args=%v code=%d fake=%#v stderr=%q", args, code, fake, stderr.String())
		}
	}
}

func TestPluginContextIncludesSelectedProject(t *testing.T) {
	fake := &fakeWorkspaceCommands{selection: workspacepkg.Selection{SchemaVersion: 1, APIOrigin: "https://example.test", UserID: "user-1", WorkspaceID: cliProjectTestWorkspaceID, ProjectID: cliProjectTestProjectID}}
	app, _, _ := workspaceApp(fake)
	runtimeContext, err := app.resolvePluginContext(context.Background(), OutputJSON)
	if err != nil || runtimeContext.ProjectID != cliProjectTestProjectID {
		t.Fatalf("context=%#v err=%v", runtimeContext, err)
	}
}

func projectFixture() client.Project {
	return client.Project{ID: cliProjectTestProjectID, WorkspaceID: cliProjectTestWorkspaceID, Slug: "launch-video", Kind: "content", Name: "Launch Video", Status: client.ProjectStatusActive, Version: 1, CreatedBy: "user-1", CreatedAt: "now", UpdatedAt: "now"}
}
