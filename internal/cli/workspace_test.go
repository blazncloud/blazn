package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/KingJammin/blazn/internal/client"
	workspacepkg "github.com/KingJammin/blazn/internal/workspace"
)

type fakeWorkspaceCommands struct {
	joinedToken, workspaceValue, requestID string
	joinResult                             workspacepkg.JoinResult
	joinErr                                error
}

func (f *fakeWorkspaceCommands) Create(context.Context, string, string, string) (client.WorkspaceEnvelope, error) {
	return client.WorkspaceEnvelope{Workspace: client.Workspace{ID: "w1", Name: "Acme"}}, nil
}
func (f *fakeWorkspaceCommands) List(context.Context) (client.WorkspaceList, error) {
	return client.WorkspaceList{Items: []client.Workspace{}}, nil
}
func (f *fakeWorkspaceCommands) Get(_ context.Context, v string) (client.WorkspaceEnvelope, error) {
	f.workspaceValue = v
	return client.WorkspaceEnvelope{Workspace: client.Workspace{ID: "w1", Name: "Acme"}}, nil
}
func (f *fakeWorkspaceCommands) Edit(context.Context, string, string, int, string) (client.WorkspaceEnvelope, error) {
	return client.WorkspaceEnvelope{}, nil
}
func (f *fakeWorkspaceCommands) Use(context.Context, string) (client.WorkspaceEnvelope, error) {
	return client.WorkspaceEnvelope{}, nil
}
func (f *fakeWorkspaceCommands) Invite(context.Context, string, client.Role, int, string) (client.InvitationCreated, error) {
	return client.InvitationCreated{}, nil
}
func (f *fakeWorkspaceCommands) Invitations(context.Context, string) (client.InvitationList, error) {
	return client.InvitationList{}, nil
}
func (f *fakeWorkspaceCommands) RevokeInvitation(context.Context, string, string, int, string) (client.MutationResult, error) {
	return client.MutationResult{}, nil
}
func (f *fakeWorkspaceCommands) Join(_ context.Context, token, key string) (workspacepkg.JoinResult, error) {
	f.joinedToken = token
	f.requestID = key
	if f.joinErr != nil {
		return f.joinResult, f.joinErr
	}
	return workspacepkg.JoinResult{Workspace: client.Workspace{ID: "w1", Name: "Acme"}, Accepted: true, Selected: true, IdempotencyKey: key}, nil
}
func (f *fakeWorkspaceCommands) Members(context.Context, string) (client.MembershipList, error) {
	return client.MembershipList{}, nil
}
func (f *fakeWorkspaceCommands) SetRole(context.Context, string, string, client.Role, int, string) (client.Membership, error) {
	return client.Membership{}, nil
}
func (f *fakeWorkspaceCommands) RemoveMember(context.Context, string, string, int, string) (client.MutationResult, error) {
	return client.MutationResult{}, nil
}
func (f *fakeWorkspaceCommands) Leave(context.Context, string, string) (client.MutationResult, error) {
	return client.MutationResult{}, nil
}
func (f *fakeWorkspaceCommands) Events(context.Context, string, string) (*client.WorkspaceEventStream, error) {
	return &client.WorkspaceEventStream{Body: io.NopCloser(strings.NewReader("event: ready\ndata: {}\n\n"))}, nil
}

func workspaceApp(fake *fakeWorkspaceCommands) (*App, *bytes.Buffer, *bytes.Buffer) {
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	app := New(out, errOut, testBuild)
	app.workspace = func() (workspaceCommands, error) { return fake, nil }
	app.stdinTTY = func() bool { return false }
	return app, out, errOut
}

func TestWorkspaceJoinAcceptsTokenOnlyFromStdin(t *testing.T) {
	fake := &fakeWorkspaceCommands{}
	app, out, stderr := workspaceApp(fake)
	token := strings.Repeat("a", 43)
	app.stdin = strings.NewReader(token + "\n")
	code := app.Run([]string{"workspace", "join", "--invite-stdin", "--request-id", "request-123"})
	if code != ExitSuccess || fake.joinedToken != token || fake.requestID != "request-123" || stderr.Len() != 0 {
		t.Fatalf("code=%d token=%q out=%q stderr=%q", code, fake.joinedToken, out.String(), stderr.String())
	}
}

func TestWorkspaceJoinRejectsArgvTokenBeforeCommand(t *testing.T) {
	fake := &fakeWorkspaceCommands{}
	app, _, stderr := workspaceApp(fake)
	secret := strings.Repeat("s", 43)
	code := app.Run([]string{"workspace", "join", secret})
	if code != ExitUsage || fake.joinedToken != "" || strings.Contains(stderr.String(), secret) {
		t.Fatalf("code=%d joined=%q stderr=%q", code, fake.joinedToken, stderr.String())
	}
}

func TestWorkspaceOverrideAndJSONList(t *testing.T) {
	fake := &fakeWorkspaceCommands{}
	app, out, stderr := workspaceApp(fake)
	if code := app.Run([]string{"workspace", "get", "--workspace", "team-slug", "--output=json"}); code != ExitSuccess || fake.workspaceValue != "team-slug" || stderr.Len() != 0 || !strings.Contains(out.String(), `"id":"w1"`) {
		t.Fatalf("code=%d value=%q out=%q stderr=%q", code, fake.workspaceValue, out.String(), stderr.String())
	}
}

func TestWorkspaceWatchStreamsSSE(t *testing.T) {
	fake := &fakeWorkspaceCommands{}
	app, out, _ := workspaceApp(fake)
	if code := app.Run([]string{"workspace", "watch"}); code != ExitSuccess || !strings.Contains(out.String(), "event: ready") {
		t.Fatalf("code=%d out=%q", code, out.String())
	}
}

func TestWorkspaceMutationsRequireExplicitRequestID(t *testing.T) {
	fake := &fakeWorkspaceCommands{}
	app, _, stderr := workspaceApp(fake)
	if code := app.Run([]string{"workspace", "invite", "--role", "member"}); code != ExitUsage || !strings.Contains(stderr.String(), "--request-id") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	app.stdin = strings.NewReader(strings.Repeat("a", 43))
	if code := app.Run([]string{"workspace", "join", "--invite-stdin"}); code != ExitUsage || fake.joinedToken != "" {
		t.Fatalf("code=%d token=%q", code, fake.joinedToken)
	}
}

func TestWorkspaceJoinPartialResultUsesExitNine(t *testing.T) {
	result := workspacepkg.JoinResult{Workspace: client.Workspace{ID: "w1", Name: "Acme"}, Accepted: true, Selected: false, IdempotencyKey: "same-request-id", SelectionError: "disk full"}
	fake := &fakeWorkspaceCommands{joinResult: result, joinErr: &workspacepkg.PartialJoinError{Cause: errors.New("disk full")}}
	app, out, stderr := workspaceApp(fake)
	app.stdin = strings.NewReader(strings.Repeat("a", 43))
	code := app.Run([]string{"workspace", "join", "--invite-stdin", "--request-id", "same-request-id", "--output=json"})
	if code != ExitPartial || stderr.Len() != 0 || !strings.Contains(out.String(), `"accepted":true`) || !strings.Contains(out.String(), `"selected":false`) || !strings.Contains(out.String(), `"idempotencyKey":"same-request-id"`) {
		t.Fatalf("code=%d out=%q stderr=%q", code, out.String(), stderr.String())
	}
}
