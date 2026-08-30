package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blazncloud/blazn/internal/client"
	sandboxpkg "github.com/blazncloud/blazn/internal/sandbox"
)

const developmentSessionTestSandbox = "66578829-ee27-49b1-bfc0-65813042ceaf"
const developmentSessionTestWorkspace = "3340c6d2-3684-4580-8385-146f1f11220c"

type fakeDevelopmentSessionSandbox struct {
	state            client.SandboxState
	desired          client.SandboxDesiredState
	execCommands     [][]string
	stopID, deleteID string
}

func (f *fakeDevelopmentSessionSandbox) Create(_ context.Context, workspace, request string, input client.CreateSandboxRequest) (client.SandboxMutation, error) {
	if workspace != developmentSessionTestWorkspace || !strings.HasSuffix(request, "-create") || input.Template.Name != developmentTemplateName || input.Template.Version != developmentTemplateVersion || len(input.Sources) != 1 {
		return client.SandboxMutation{}, os.ErrInvalid
	}
	f.state, f.desired = client.SandboxReady, client.SandboxDesiredReady
	return client.SandboxMutation{Sandbox: client.Sandbox{ID: developmentSessionTestSandbox}}, nil
}
func (f *fakeDevelopmentSessionSandbox) List(context.Context, string, string) (client.SandboxList, error) {
	return client.SandboxList{}, nil
}
func (f *fakeDevelopmentSessionSandbox) Get(context.Context, string) (client.Sandbox, error) {
	return client.Sandbox{ID: developmentSessionTestSandbox, State: f.state, DesiredState: f.desired}, nil
}
func (f *fakeDevelopmentSessionSandbox) Watch(context.Context, string, string, func(client.SandboxEvent) error) (sandboxpkg.WatchTerminal, error) {
	return sandboxpkg.WatchReady, nil
}
func (f *fakeDevelopmentSessionSandbox) Exec(_ context.Context, _ string, command []string) (sandboxpkg.ExecResult, error) {
	f.execCommands = append(f.execCommands, append([]string(nil), command...))
	stdout := ""
	joined := strings.Join(command, " ")
	if strings.Contains(joined, "diff --cached") {
		stdout = "PATCH_READY"
	} else if len(command) > 0 && command[0] == "printf" {
		stdout = "ok"
	}
	return sandboxpkg.ExecResult{SandboxID: developmentSessionTestSandbox, StdoutBase64: base64.StdEncoding.EncodeToString([]byte(stdout))}, nil
}
func (f *fakeDevelopmentSessionSandbox) Upload(context.Context, string, string, string) (sandboxpkg.TransferResult, error) {
	return sandboxpkg.TransferResult{}, nil
}
func (f *fakeDevelopmentSessionSandbox) Download(_ context.Context, id, source, destination string) (sandboxpkg.TransferResult, error) {
	data := []byte("diff --git a/README.md b/README.md\n")
	if err := os.WriteFile(destination, data, 0o600); err != nil {
		return sandboxpkg.TransferResult{}, err
	}
	digest := sha256.Sum256(data)
	return sandboxpkg.TransferResult{SandboxID: id, Source: source, Destination: destination, Size: int64(len(data)), SHA256: "sha256:" + hex.EncodeToString(digest[:])}, nil
}
func (f *fakeDevelopmentSessionSandbox) Stop(_ context.Context, id, _ string) (client.SandboxMutation, error) {
	f.stopID, f.state, f.desired = id, client.SandboxStopped, client.SandboxDesiredStopped
	return client.SandboxMutation{}, nil
}
func (f *fakeDevelopmentSessionSandbox) Delete(_ context.Context, id, _ string) (client.SandboxMutation, error) {
	f.deleteID, f.state, f.desired = id, client.SandboxDeleted, client.SandboxDesiredDeleted
	return client.SandboxMutation{}, nil
}
func (f *fakeDevelopmentSessionSandbox) Validate(context.Context, []byte) sandboxpkg.TemplateValidation {
	return sandboxpkg.TemplateValidation{}
}
func (f *fakeDevelopmentSessionSandbox) Publish(context.Context, string, string, []byte) (sandboxpkg.TemplatePublish, error) {
	return sandboxpkg.TemplatePublish{}, nil
}

func TestDevelopmentSessionNativeLifecycle(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("BLAZN_SOURCE_PREFLIGHT_FETCH", "0")
	t.Setenv("BLAZN_SESSION_POLL_DELAY_SECONDS", "0")
	commitBytes, err := exec.Command("git", "rev-parse", "origin/main").Output()
	if err != nil {
		t.Fatal(err)
	}
	commit := strings.TrimSpace(string(commitBytes))
	runtime := &fakeDevelopmentSessionSandbox{}
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	app := New(stdout, stderr, BuildInfo{})
	app.sandbox = func() (sandboxCommands, error) { return runtime, nil }

	start := []string{"dev", "session", "start", "--workspace", developmentSessionTestWorkspace, "--source", commit, "--expires", "30m"}
	if code := app.Run(start); code != ExitSuccess {
		t.Fatalf("start code=%d stderr=%q", code, stderr.String())
	}
	receiptPath, _ := developmentSessionPath("default")
	receipt, err := readDevelopmentSession(receiptPath)
	if err != nil || receipt.Phase != "ready" || receipt.SandboxID != developmentSessionTestSandbox {
		t.Fatalf("receipt=%#v err=%v", receipt, err)
	}
	if info, err := os.Stat(receiptPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("receipt mode=%v err=%v", info.Mode().Perm(), err)
	}
	if len(runtime.execCommands) != 1 || !strings.Contains(strings.Join(runtime.execCommands[0], " "), "refs/blazn/baseline") {
		t.Fatalf("baseline=%v", runtime.execCommands)
	}

	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"dev", "session", "exec", "--", "printf", "ok"}); code != ExitSuccess {
		t.Fatalf("exec code=%d stderr=%q", code, stderr.String())
	}
	if stdout.String() != "ok" {
		t.Fatalf("exec output=%q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"--output", "json", "dev", "session", "status"}); code != ExitSuccess {
		t.Fatalf("status code=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), developmentSessionTestSandbox) {
		t.Fatalf("status=%q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	patch := filepath.Join(t.TempDir(), "change.patch")
	if code := app.Run([]string{"dev", "session", "patch", patch}); code != ExitSuccess {
		t.Fatalf("patch code=%d stderr=%q", code, stderr.String())
	}
	if data, err := os.ReadFile(patch); err != nil || !bytes.HasPrefix(data, []byte("diff --git")) {
		t.Fatalf("patch=%q err=%v", data, err)
	}
	if _, err := os.Stat(patch + ".sha256"); err != nil {
		t.Fatal(err)
	}
	if code := app.Run([]string{"dev", "session", "patch", patch}); code != ExitFailure {
		t.Fatalf("patch overwrite code=%d", code)
	}

	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"dev", "session", "finish", "--discard"}); code != ExitSuccess {
		t.Fatalf("finish code=%d stderr=%q", code, stderr.String())
	}
	if runtime.stopID != developmentSessionTestSandbox || runtime.deleteID != developmentSessionTestSandbox {
		t.Fatalf("stop=%q delete=%q", runtime.stopID, runtime.deleteID)
	}
	receipt, err = readDevelopmentSession(receiptPath)
	if err != nil || receipt.Phase != "deleted" {
		t.Fatalf("finished receipt=%#v err=%v", receipt, err)
	}
}

func TestDevelopmentSessionRejectsUnsafeNameAndReceipt(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	app := New(stdout, stderr, BuildInfo{})
	if code := app.Run([]string{"dev", "session", "status", "--session", "../escape"}); code != ExitUsage {
		t.Fatalf("unsafe name code=%d", code)
	}
	path, _ := developmentSessionPath("default")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if code := app.Run([]string{"dev", "session", "status"}); code != ExitFailure {
		t.Fatalf("symlink receipt code=%d", code)
	}
}

func TestDevelopmentSessionOptionParsingStopsAtExecDelimiter(t *testing.T) {
	session, rest, err := extractDevelopmentSession([]string{"--session", "named", "--", "printf", "--session", "remote-value"})
	if err != nil {
		t.Fatal(err)
	}
	if session != "named" || strings.Join(rest, "|") != "--|printf|--session|remote-value" {
		t.Fatalf("session=%q rest=%q", session, rest)
	}
	session, rest, err = extractDevelopmentSession([]string{"--", "printf", "--session", "remote-value"})
	if err != nil || session != "default" || strings.Join(rest, "|") != "--|printf|--session|remote-value" {
		t.Fatalf("session=%q rest=%q err=%v", session, rest, err)
	}
}

func TestDevelopmentSessionLockRecoversOnlyProvenStaleOwner(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	path, err := developmentSessionPath("default")
	if err != nil {
		t.Fatal(err)
	}
	lock := path + ".lock"
	if err := os.MkdirAll(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := developmentSessionLockOwner{PID: 2147483647, Start: "stale", Token: "stale-token"}
	data, _ := json.Marshal(stale)
	if err := os.WriteFile(filepath.Join(lock, "owner.json"), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	gotPath, unlock, err := lockDevelopmentSession("default")
	if err != nil || gotPath != path {
		t.Fatalf("path=%q err=%v", gotPath, err)
	}
	owner, err := readDevelopmentSessionLockOwner(lock)
	if err != nil || owner.PID != os.Getpid() || owner.Token == stale.Token {
		t.Fatalf("owner=%#v err=%v", owner, err)
	}
	unlock()
	if _, err := os.Lstat(lock); !os.IsNotExist(err) {
		t.Fatalf("released lock remains: %v", err)
	}
}

func TestDevelopmentSessionLockPreservesLiveAndInvalidOwners(t *testing.T) {
	for name, ownerData := range map[string][]byte{
		"live": func() []byte {
			start, err := developmentProcessStart(os.Getpid())
			if err != nil {
				t.Fatal(err)
			}
			data, _ := json.Marshal(developmentSessionLockOwner{PID: os.Getpid(), Start: start, Token: "live-token"})
			return append(data, '\n')
		}(),
		"invalid": []byte("{}\n"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			path, _ := developmentSessionPath("default")
			lock := path + ".lock"
			if err := os.MkdirAll(lock, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(lock, "owner.json"), ownerData, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := lockDevelopmentSession("default"); err == nil {
				t.Fatal("existing owner unexpectedly replaced")
			}
			if _, err := os.Lstat(lock); err != nil {
				t.Fatalf("existing lock was not preserved: %v", err)
			}
		})
	}
}

func TestDevelopmentSessionTemplateDefaultsMatchPublishedManifest(t *testing.T) {
	data, err := os.ReadFile("../../examples/coding-agent/sandbox-template-dev.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			Version      string                               `json:"version"`
			Repositories []struct{ Name, Destination string } `json:"repositories"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Metadata.Name != developmentTemplateName || manifest.Spec.Version != developmentTemplateVersion || len(manifest.Spec.Repositories) != 1 || manifest.Spec.Repositories[0].Name != developmentSourceName || manifest.Spec.Repositories[0].Destination != developmentSourcePath {
		t.Fatalf("native defaults drifted from template: %#v", manifest)
	}
}
