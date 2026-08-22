package cli

import (
	"context"
	"errors"
	"testing"

	workspacepkg "github.com/blazncloud/blazn/internal/workspace"
)

func TestResolvePluginContextUsesSelectedWorkspace(t *testing.T) {
	app, _, _ := workspaceApp(&fakeWorkspaceCommands{selection: workspacepkg.Selection{SchemaVersion: 1, APIOrigin: "https://example.test", UserID: "user-2", WorkspaceID: "workspace-2"}})
	runtimeContext, err := app.resolvePluginContext(context.Background(), OutputJSON)
	if err != nil || runtimeContext.Status != "selected" || runtimeContext.APIOrigin != "https://example.test" || runtimeContext.UserID != "user-2" || runtimeContext.WorkspaceID != "workspace-2" || runtimeContext.OutputFormat != "json" {
		t.Fatalf("context=%#v err=%v", runtimeContext, err)
	}
}

func TestResolvePluginContextDistinguishesUnselectedFromUnavailable(t *testing.T) {
	tests := []struct {
		err        error
		status     string
		reasonCode string
	}{
		{err: workspacepkg.ErrNoContext, status: "unselected", reasonCode: "workspace_not_selected"},
		{err: errors.New("authentication unavailable"), status: "unavailable", reasonCode: "workspace_context_unavailable"},
	}
	for _, test := range tests {
		fake := &fakeWorkspaceCommands{selection: workspacepkg.Selection{SchemaVersion: 1, APIOrigin: "https://example.test"}, selectionErr: test.err}
		app, _, _ := workspaceApp(fake)
		runtimeContext, err := app.resolvePluginContext(context.Background(), OutputHuman)
		if err != nil || runtimeContext.Status != test.status || runtimeContext.ReasonCode != test.reasonCode || runtimeContext.APIOrigin != "https://example.test" {
			t.Fatalf("context=%#v err=%v", runtimeContext, err)
		}
	}
}
