package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/blazncloud/blazn/internal/client"
	sandboxpkg "github.com/blazncloud/blazn/internal/sandbox"
)

const testSandboxID = "11111111-1111-4111-8111-111111111111"

type fakeSandboxRuntime struct {
	workspace, requestID string
	create               client.CreateSandboxRequest
	exec                 sandboxpkg.ExecResult
	execErr              error
	watchEvents          []client.SandboxEvent
	watchTerminal        sandboxpkg.WatchTerminal
}

func (f *fakeSandboxRuntime) Create(_ context.Context, w, key string, r client.CreateSandboxRequest) (client.SandboxMutation, error) {
	f.workspace, f.requestID, f.create = w, key, r
	return client.SandboxMutation{Sandbox: client.Sandbox{ID: testSandboxID}}, nil
}
func (f *fakeSandboxRuntime) List(context.Context, string, string) (client.SandboxList, error) {
	return client.SandboxList{Items: []client.Sandbox{}}, nil
}
func (f *fakeSandboxRuntime) Get(context.Context, string) (client.Sandbox, error) {
	return client.Sandbox{ID: testSandboxID}, nil
}
func (f *fakeSandboxRuntime) Watch(_ context.Context, _ string, _ string, emit func(client.SandboxEvent) error) (sandboxpkg.WatchTerminal, error) {
	for _, event := range f.watchEvents {
		if err := emit(event); err != nil {
			return "", err
		}
	}
	return f.watchTerminal, nil
}
func (f *fakeSandboxRuntime) Exec(context.Context, string, []string) (sandboxpkg.ExecResult, error) {
	return f.exec, f.execErr
}
func (f *fakeSandboxRuntime) Upload(context.Context, string, string, string) (sandboxpkg.TransferResult, error) {
	return sandboxpkg.TransferResult{}, nil
}
func (f *fakeSandboxRuntime) Download(context.Context, string, string, string) (sandboxpkg.TransferResult, error) {
	return sandboxpkg.TransferResult{}, nil
}
func (f *fakeSandboxRuntime) Stop(context.Context, string, string) (client.SandboxMutation, error) {
	return client.SandboxMutation{}, nil
}
func (f *fakeSandboxRuntime) Delete(context.Context, string, string) (client.SandboxMutation, error) {
	return client.SandboxMutation{}, nil
}

type fakeTemplateRuntime struct {
	workspace, key string
	manifest       []byte
}

func (f *fakeTemplateRuntime) Publish(_ context.Context, w, key string, manifest []byte) (sandboxpkg.TemplatePublish, error) {
	f.workspace, f.key, f.manifest = w, key, append([]byte(nil), manifest...)
	return sandboxpkg.TemplatePublish{Template: client.SandboxTemplate{ID: "template"}}, nil
}

func commandApp() (*App, *bytes.Buffer, *bytes.Buffer) {
	out, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	return New(out, stderr, testBuild), out, stderr
}

func TestSandboxCreateParsesFrozenRequest(t *testing.T) {
	runtime := &fakeSandboxRuntime{}
	app, out, stderr := commandApp()
	commit := strings.Repeat("a", 40)
	code := app.RunSandboxCommand(context.Background(), OutputJSON, []string{"create", "--template", "coding-small@v1", "--arch", "arm64", "--mode", "claim", "--expires", "30m", "--approved-non-sensitive", "--workspace", "workspace-1", "--request-id", "request-123", "--source", "source=" + commit}, runtime)
	if code != ExitSuccess || stderr.Len() != 0 || runtime.workspace != "workspace-1" || runtime.create.Template.Version != "v1" || runtime.create.ExpiresInSeconds != 1800 || !runtime.create.ApprovedNonSensitive || len(runtime.create.Sources) != 1 || !strings.Contains(out.String(), testSandboxID) {
		t.Fatalf("code=%d request=%+v out=%q stderr=%q", code, runtime.create, out, stderr)
	}
}

func TestSandboxCreateRequiresExplicitAcknowledgement(t *testing.T) {
	runtime := &fakeSandboxRuntime{}
	app, _, stderr := commandApp()
	code := app.RunSandboxCommand(context.Background(), OutputHuman, []string{"create", "--template", "coding", "--arch", "amd64", "--mode", "direct", "--expires", "60", "--workspace", "w", "--request-id", "request-123"}, runtime)
	if code != ExitUsage || !strings.Contains(stderr.String(), "approved-non-sensitive") || runtime.workspace != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
}

func TestSandboxRejectsSecretArgvWithoutEcho(t *testing.T) {
	secret := "do-not-print-this"
	app, out, stderr := commandApp()
	code := app.RunSandboxCommand(context.Background(), OutputJSON, []string{"get", testSandboxID, "--access-token", secret}, &fakeSandboxRuntime{})
	combined := out.String() + stderr.String()
	if code != ExitUsage || strings.Contains(combined, secret) || strings.Contains(combined, "accessToken") || !strings.Contains(combined, `"requestId":"local"`) {
		t.Fatalf("code=%d output=%q", code, combined)
	}
}

func TestSandboxExecTruncationPrecedesRemoteFailure(t *testing.T) {
	result := sandboxpkg.ExecResult{SandboxID: testSandboxID, GrantID: "22222222-2222-4222-8222-222222222222", RemoteExitCode: 23, Truncated: true}
	runtime := &fakeSandboxRuntime{exec: result, execErr: &sandboxpkg.PartialError{Cause: errors.New("truncated")}}
	app, out, stderr := commandApp()
	code := app.RunSandboxCommand(context.Background(), OutputJSON, []string{"exec", testSandboxID, "--", "false"}, runtime)
	if code != ExitPartial || stderr.Len() != 0 || !strings.Contains(out.String(), `"remoteExitCode":23`) || !strings.Contains(out.String(), `"truncated":true`) {
		t.Fatalf("code=%d out=%q stderr=%q", code, out, stderr)
	}
}

func TestSandboxExecUnavailablePrecedesPartial(t *testing.T) {
	runtime := &fakeSandboxRuntime{execErr: &sandboxpkg.PartialError{Cause: &sandboxpkg.UnavailableError{Cause: errors.New("offline")}}}
	app, out, _ := commandApp()
	code := app.RunSandboxCommand(context.Background(), OutputJSON, []string{"exec", testSandboxID, "--", "true"}, runtime)
	if code != ExitUnavailable || !strings.Contains(out.String(), `"exitCode":7`) {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func TestSandboxWatchWritesNDJSONAndTerminalExit(t *testing.T) {
	runtime := &fakeSandboxRuntime{watchEvents: []client.SandboxEvent{{EventID: "22222222-2222-4222-8222-222222222222", SandboxID: testSandboxID, Sequence: 1, Type: "sandbox.failed", Payload: map[string]any{"state": "failed"}}}, watchTerminal: sandboxpkg.WatchFailed}
	app, out, stderr := commandApp()
	code := app.RunSandboxCommand(context.Background(), OutputHuman, []string{"watch", testSandboxID}, runtime)
	if code != ExitFailure || stderr.Len() != 0 || strings.Count(strings.TrimSpace(out.String()), "\n") != 0 || !strings.Contains(out.String(), `"eventId"`) {
		t.Fatalf("code=%d out=%q stderr=%q", code, out, stderr)
	}
}

func TestTemplateValidateAndPublishHooks(t *testing.T) {
	path := "../../packages/contracts/testdata/sandbox/template-good.json"
	app, out, stderr := commandApp()
	code := app.RunTemplateCommand(context.Background(), OutputJSON, []string{"validate", "-f", path}, nil)
	if code != ExitSuccess || stderr.Len() != 0 || !strings.Contains(out.String(), `"valid":true`) || !strings.Contains(out.String(), `"manifestDigest":"sha256:`) {
		t.Fatalf("code=%d out=%q stderr=%q", code, out, stderr)
	}
	runtime := &fakeTemplateRuntime{}
	app, out, stderr = commandApp()
	code = app.RunTemplateCommand(context.Background(), OutputJSON, []string{"publish", "-f", path, "--workspace", "workspace-1", "--request-id", "request-123"}, runtime)
	if code != ExitSuccess || stderr.Len() != 0 || runtime.workspace != "workspace-1" || runtime.key != "request-123" || len(runtime.manifest) == 0 || !strings.Contains(out.String(), `"id":"template"`) {
		t.Fatalf("code=%d runtime=%+v out=%q stderr=%q", code, runtime, out, stderr)
	}
}
