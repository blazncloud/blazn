package cli

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/blazncloud/blazn/internal/client"
	sandboxpkg "github.com/blazncloud/blazn/internal/sandbox"
)

const (
	developmentSessionSchema   = "blazn-development-session/v1"
	developmentTemplateName    = "coding-agent"
	developmentTemplateVersion = "go-1.26.2-node-22.19.0-poc-dev-5"
	developmentSourceName      = "source"
	developmentSourcePath      = "/workspace/src/blazn"
)

var developmentSessionName = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,62}[a-z0-9])?$`)

type developmentSessionReceipt struct {
	Schema        string `json:"schema"`
	Phase         string `json:"phase"`
	SandboxID     string `json:"sandboxId,omitempty"`
	WorkspaceID   string `json:"workspaceId"`
	SourceCommit  string `json:"sourceCommit"`
	Architecture  string `json:"architecture"`
	Template      string `json:"template"`
	Expires       string `json:"expires"`
	RequestPrefix string `json:"requestPrefix"`
	CreatedAt     string `json:"createdAt"`
}

type developmentSessionResult struct {
	Session string                    `json:"session"`
	Receipt developmentSessionReceipt `json:"receipt"`
	Sandbox *client.Sandbox           `json:"sandbox,omitempty"`
	Patch   string                    `json:"patch,omitempty"`
}

func (a *App) runDevelopmentSession(format OutputFormat, args []string) int {
	if len(args) == 0 || helpRequested(args) {
		return a.writeHelp(format, "dev-session")
	}
	action := args[0]
	session, rest, err := extractDevelopmentSession(args[1:])
	if err != nil {
		return a.developmentSessionError(format, ExitUsage, err)
	}
	path, unlock, err := lockDevelopmentSession(session)
	if err != nil {
		return a.developmentSessionError(format, ExitFailure, err)
	}
	defer unlock()
	ctx := context.Background()

	if action == "start" {
		return a.startDevelopmentSession(ctx, format, session, path, rest)
	}
	receipt, err := readDevelopmentSession(path)
	if err != nil {
		return a.developmentSessionError(format, ExitFailure, err)
	}
	if action == "status" && receipt.Phase == "deleted" {
		if len(rest) != 0 {
			return a.developmentSessionError(format, ExitUsage, errors.New("session status takes no arguments"))
		}
		return a.writeDevelopmentSession(format, developmentSessionResult{Session: session, Receipt: receipt})
	}
	if receipt.SandboxID == "" || !validSandboxID(receipt.SandboxID) {
		return a.developmentSessionError(format, ExitFailure, errors.New("development session creation is incomplete"))
	}
	runtime, err := a.sandbox()
	if err != nil {
		return a.developmentSessionError(format, ExitUnavailable, err)
	}
	if action != "status" && receipt.Phase == "deleted" {
		return a.developmentSessionError(format, ExitFailure, errors.New("development session is already deleted"))
	}
	switch action {
	case "status":
		if len(rest) != 0 {
			return a.developmentSessionError(format, ExitUsage, errors.New("session status takes no arguments"))
		}
		value, err := runtime.Get(ctx, receipt.SandboxID)
		if err != nil {
			return writeSandboxRuntimeError(format, a.stdout, a.stderr, err)
		}
		if value.ID != receipt.SandboxID {
			return a.developmentSessionError(format, ExitFailure, errors.New("sandbox status identity mismatch"))
		}
		return a.writeDevelopmentSession(format, developmentSessionResult{Session: session, Receipt: receipt, Sandbox: &value})
	case "exec":
		if receipt.Phase != "ready" {
			return a.developmentSessionError(format, ExitFailure, fmt.Errorf("development session is not ready (phase %s)", receipt.Phase))
		}
		return a.runDevelopmentSessionExec(ctx, format, receipt.SandboxID, rest, runtime)
	case "upload", "download":
		if receipt.Phase != "ready" {
			return a.developmentSessionError(format, ExitFailure, fmt.Errorf("development session is not ready (phase %s)", receipt.Phase))
		}
		return runSandboxTransfer(ctx, format, action, append([]string{receipt.SandboxID}, rest...), runtime, a.stdout, a.stderr)
	case "patch":
		if receipt.Phase != "ready" || len(rest) != 1 {
			return a.developmentSessionError(format, ExitUsage, errors.New("session patch requires OUTPUT_PATH for a ready session"))
		}
		patch, err := captureDevelopmentPatch(ctx, runtime, receipt.SandboxID, rest[0])
		if err != nil {
			return a.developmentSessionError(format, ExitFailure, err)
		}
		return a.writeDevelopmentSession(format, developmentSessionResult{Session: session, Receipt: receipt, Patch: patch})
	case "finish":
		return a.finishDevelopmentSession(ctx, format, session, path, receipt, rest, runtime)
	default:
		return a.developmentSessionError(format, ExitUsage, fmt.Errorf("unknown dev session command %q", action))
	}
}

func extractDevelopmentSession(args []string) (string, []string, error) {
	session := "default"
	rest := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		if args[index] == "--session" {
			if index+1 >= len(args) {
				return "", nil, errors.New("--session requires a name")
			}
			session = args[index+1]
			index++
			continue
		}
		rest = append(rest, args[index])
	}
	if !developmentSessionName.MatchString(session) {
		return "", nil, errors.New("--session must contain 1 to 64 lowercase letters, digits, dots, dashes, or underscores")
	}
	return session, rest, nil
}

func developmentSessionPath(name string) (string, error) {
	root := os.Getenv("XDG_STATE_HOME")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(root, "blazn", "development-sessions", name+".json"), nil
}

func lockDevelopmentSession(name string) (string, func(), error) {
	path, err := developmentSessionPath(name)
	if err != nil {
		return "", nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", nil, err
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return "", nil, err
	}
	lock := path + ".lock"
	if err := os.Mkdir(lock, 0o700); err != nil {
		return "", nil, fmt.Errorf("development session is busy: %s", name)
	}
	return path, func() { _ = os.Remove(lock) }, nil
}

func readDevelopmentSession(path string) (developmentSessionReceipt, error) {
	var value developmentSessionReceipt
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return value, errors.New("development session receipt is missing or unsafe")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return value, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil || value.Schema != developmentSessionSchema {
		return value, errors.New("development session receipt is invalid")
	}
	if !uuidPatternCLI(value.WorkspaceID) || !exactCommit(value.SourceCommit) || (value.Architecture != "amd64" && value.Architecture != "arm64") || value.Template != developmentTemplateName+"@"+developmentTemplateVersion || !validRequestID(value.RequestPrefix) {
		return value, errors.New("development session receipt is invalid")
	}
	if _, err := parseExpiry(value.Expires); err != nil {
		return value, errors.New("development session receipt is invalid")
	}
	switch value.Phase {
	case "creating":
		if value.SandboxID != "" {
			return value, errors.New("development session receipt is invalid")
		}
	case "starting", "ready", "patch_saved", "discard_approved", "stopping", "deleting", "deleted":
		if !validSandboxID(value.SandboxID) {
			return value, errors.New("development session receipt is invalid")
		}
	default:
		return value, errors.New("development session receipt is invalid")
	}
	return value, nil
}

func writeDevelopmentSession(path string, value developmentSessionReceipt) error {
	if info, err := os.Lstat(path); err == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
		return errors.New("refusing unsafe development session receipt")
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temp, err := os.CreateTemp(filepath.Dir(path), ".session-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func (a *App) startDevelopmentSession(ctx context.Context, format OutputFormat, session, path string, args []string) int {
	allowed := map[string]flagKind{"workspace": flagValue, "source": flagValue, "arch": flagValue, "expires": flagValue}
	values, positional, err := parseSandboxFlags(args, allowed, false)
	if err != nil || len(positional) != 0 {
		if err == nil {
			err = errors.New("session start accepts only options")
		}
		return a.developmentSessionError(format, ExitUsage, err)
	}
	workspace := firstFlag(values, "workspace")
	if workspace == "" {
		commands, selectionErr := a.workspace()
		if selectionErr != nil {
			return a.developmentSessionError(format, ExitUnavailable, selectionErr)
		}
		selection, selectionErr := commands.CurrentSelection(ctx)
		if selectionErr != nil || selection.WorkspaceID == "" {
			return a.developmentSessionError(format, ExitUsage, errors.New("select a Workspace or pass --workspace"))
		}
		workspace = selection.WorkspaceID
	}
	if !uuidPatternCLI(workspace) {
		return a.developmentSessionError(format, ExitUsage, errors.New("--workspace must be a UUID"))
	}
	arch := firstFlag(values, "arch")
	if arch == "" {
		arch = "amd64"
	}
	if arch != "amd64" && arch != "arm64" {
		return a.developmentSessionError(format, ExitUsage, errors.New("--arch must be amd64 or arm64"))
	}
	expires := firstFlag(values, "expires")
	if expires == "" {
		expires = "2h"
	}
	seconds, err := parseExpiry(expires)
	if err != nil {
		return a.developmentSessionError(format, ExitUsage, err)
	}
	runtime, err := a.sandbox()
	if err != nil {
		return a.developmentSessionError(format, ExitUnavailable, err)
	}
	var receipt developmentSessionReceipt
	if _, err := os.Lstat(path); err == nil {
		receipt, err = readDevelopmentSession(path)
		if err != nil {
			return a.developmentSessionError(format, ExitFailure, err)
		}
		if receipt.Phase != "creating" && receipt.Phase != "starting" {
			return a.developmentSessionError(format, ExitFailure, fmt.Errorf("refusing to replace existing session in phase %s", receipt.Phase))
		}
		workspace, arch, expires = receipt.WorkspaceID, receipt.Architecture, receipt.Expires
		seconds, err = parseExpiry(expires)
		if err != nil {
			return a.developmentSessionError(format, ExitFailure, err)
		}
	} else if !os.IsNotExist(err) {
		return a.developmentSessionError(format, ExitFailure, err)
	} else {
		source, sourceErr := resolveDevelopmentSource(firstFlag(values, "source"))
		if sourceErr != nil {
			return a.developmentSessionError(format, ExitFailure, sourceErr)
		}
		receipt = developmentSessionReceipt{Schema: developmentSessionSchema, Phase: "creating", WorkspaceID: workspace, SourceCommit: source, Architecture: arch, Template: developmentTemplateName + "@" + developmentTemplateVersion, Expires: expires, RequestPrefix: fmt.Sprintf("development-session-%d-%d", time.Now().UTC().Unix(), os.Getpid()), CreatedAt: time.Now().UTC().Format(time.RFC3339)}
		if err := writeDevelopmentSession(path, receipt); err != nil {
			return a.developmentSessionError(format, ExitFailure, err)
		}
	}
	if receipt.Phase == "creating" {
		mutation, err := runtime.Create(ctx, workspace, receipt.RequestPrefix+"-create", client.CreateSandboxRequest{Template: client.SandboxTemplateReference{Name: developmentTemplateName, Version: developmentTemplateVersion}, Architecture: client.SandboxArchitecture(arch), AllocationMode: client.SandboxDirect, ExpiresInSeconds: seconds, Sources: []client.SandboxSource{{Repository: developmentSourceName, Commit: receipt.SourceCommit}}, ApprovedNonSensitive: true})
		if err != nil {
			return writeSandboxRuntimeError(format, a.stdout, a.stderr, err)
		}
		if !validSandboxID(mutation.Sandbox.ID) {
			return a.developmentSessionError(format, ExitFailure, errors.New("sandbox create returned an invalid identity"))
		}
		receipt.SandboxID, receipt.Phase = mutation.Sandbox.ID, "starting"
		if err := writeDevelopmentSession(path, receipt); err != nil {
			return a.developmentSessionError(format, ExitFailure, err)
		}
	}
	attempts := developmentSessionInt("BLAZN_SESSION_READY_ATTEMPTS", 120)
	var sandbox client.Sandbox
	for attempt := 0; attempt < attempts; attempt++ {
		sandbox, err = runtime.Get(ctx, receipt.SandboxID)
		if err == nil && sandbox.ID != receipt.SandboxID {
			return a.developmentSessionError(format, ExitFailure, errors.New("sandbox status identity mismatch"))
		}
		if err == nil && sandbox.State == client.SandboxReady {
			break
		}
		if err == nil && (sandbox.State == client.SandboxFailed || sandbox.State == client.SandboxDeleted) {
			return a.developmentSessionError(format, ExitFailure, fmt.Errorf("sandbox entered terminal state %s", sandbox.State))
		}
		if err != nil && !sandboxpkg.IsUnavailable(err) {
			return writeSandboxRuntimeError(format, a.stdout, a.stderr, err)
		}
		time.Sleep(time.Duration(developmentSessionInt("BLAZN_SESSION_POLL_DELAY_SECONDS", 5)) * time.Second)
	}
	if sandbox.State != client.SandboxReady {
		return a.developmentSessionError(format, ExitFailure, errors.New("sandbox did not reach ready state"))
	}
	baseline := []string{"sh", "-lc", "set -eu; cd " + developmentSourcePath + "; rm -rf .git; git init -q; git -c safe.directory=" + developmentSourcePath + " add -A; git -c safe.directory=" + developmentSourcePath + " -c user.name=Blazn -c user.email=development@blazn.invalid -c commit.gpgsign=false commit -qm 'Blazn materialized source baseline'; git -c safe.directory=" + developmentSourcePath + " update-ref refs/blazn/baseline HEAD"}
	if _, err := runtime.Exec(ctx, receipt.SandboxID, baseline); err != nil {
		return writeSandboxRuntimeError(format, a.stdout, a.stderr, err)
	}
	receipt.Phase = "ready"
	if err := writeDevelopmentSession(path, receipt); err != nil {
		return a.developmentSessionError(format, ExitFailure, err)
	}
	return a.writeDevelopmentSession(format, developmentSessionResult{Session: session, Receipt: receipt, Sandbox: &sandbox})
}

func resolveDevelopmentSource(explicit string) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	rootBytes, err := exec.Command("git", "-C", cwd, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", errors.New("current directory is not a Git repository")
	}
	root := strings.TrimSpace(string(rootBytes))
	originBytes, err := exec.Command("git", "-C", root, "config", "--get", "remote.origin.url").Output()
	if err != nil {
		return "", errors.New("Git repository has no origin remote")
	}
	switch strings.TrimSpace(string(originBytes)) {
	case "https://github.com/blazncloud/blazn", "https://github.com/blazncloud/blazn.git", "git@github.com:blazncloud/blazn.git":
	default:
		return "", errors.New("the current development template supports only github.com/blazncloud/blazn")
	}
	source := explicit
	if source == "" {
		status, err := exec.Command("git", "-C", root, "status", "--porcelain", "--untracked-files=normal").Output()
		if err != nil || len(status) != 0 {
			return "", errors.New("working tree is dirty; commit and push it before starting")
		}
		head, err := exec.Command("git", "-C", root, "rev-parse", "--verify", "HEAD").Output()
		if err != nil {
			return "", err
		}
		source = strings.TrimSpace(string(head))
	}
	if !exactCommit(source) {
		return "", errors.New("--source must be an exact lowercase 40- or 64-hex commit")
	}
	if os.Getenv("BLAZN_SOURCE_PREFLIGHT_FETCH") != "0" {
		if err := exec.Command("git", "-C", root, "fetch", "--quiet", "--no-tags", "origin").Run(); err != nil {
			return "", errors.New("could not refresh origin before source preflight")
		}
	}
	if err := exec.Command("git", "-C", root, "cat-file", "-e", source+"^{commit}").Run(); err != nil {
		return "", errors.New("source commit is not present locally")
	}
	contained, _ := exec.Command("git", "-C", root, "for-each-ref", "--format=%(refname)", "--contains", source, "refs/remotes/origin/").Output()
	if strings.TrimSpace(string(contained)) == "" {
		remote, err := exec.Command("git", "-C", root, "ls-remote", "--heads", "--tags", "origin").Output()
		if err != nil {
			return "", errors.New("could not verify source commit on origin")
		}
		found := false
		for _, line := range strings.Split(string(remote), "\n") {
			if strings.HasPrefix(line, source+"\t") {
				found = true
				break
			}
		}
		if !found {
			return "", errors.New("source commit is not pushed")
		}
	}
	return source, nil
}

func captureDevelopmentPatch(ctx context.Context, runtime SandboxCommandRuntime, sandboxID, output string) (string, error) {
	result, err := runtime.Exec(ctx, sandboxID, []string{"sh", "-lc", "set -eu; cd " + developmentSourcePath + "; git -c safe.directory=" + developmentSourcePath + " add -A; git -c safe.directory=" + developmentSourcePath + " diff --cached --binary refs/blazn/baseline -- > /workspace/artifacts/change.patch; if test -s /workspace/artifacts/change.patch; then printf PATCH_READY; else rm -f /workspace/artifacts/change.patch; printf NO_CHANGES; fi"})
	if err != nil {
		return "", err
	}
	decoded, err := base64.StdEncoding.DecodeString(result.StdoutBase64)
	if err != nil {
		return "", err
	}
	if string(decoded) == "NO_CHANGES" {
		return "", nil
	}
	if string(decoded) != "PATCH_READY" {
		return "", errors.New("unexpected patch generation response")
	}
	parent := filepath.Dir(output)
	if info, err := os.Stat(parent); err != nil || !info.IsDir() {
		return "", errors.New("patch output directory does not exist")
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		return "", errors.New("refusing to overwrite patch output")
	}
	if _, err := os.Lstat(output + ".sha256"); !os.IsNotExist(err) {
		return "", errors.New("refusing to overwrite patch checksum")
	}
	temp, err := os.CreateTemp(parent, ".blazn-patch-*")
	if err != nil {
		return "", err
	}
	tempName := temp.Name()
	temp.Close()
	os.Remove(tempName)
	defer os.Remove(tempName)
	transfer, err := runtime.Download(ctx, sandboxID, "/workspace/artifacts/change.patch", tempName)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(tempName)
	if err != nil || len(data) == 0 {
		return "", errors.New("downloaded patch is empty")
	}
	digest := sha256.Sum256(data)
	actual := "sha256:" + hex.EncodeToString(digest[:])
	if transfer.SHA256 != actual {
		return "", errors.New("downloaded patch checksum mismatch")
	}
	if err := os.Link(tempName, output); err != nil {
		return "", errors.New("refusing to overwrite patch output")
	}
	checksumTemp, err := os.CreateTemp(parent, ".blazn-checksum-*")
	if err != nil {
		os.Remove(output)
		return "", err
	}
	checksumName := checksumTemp.Name()
	defer os.Remove(checksumName)
	_, writeErr := fmt.Fprintf(checksumTemp, "%s  %s\n", strings.TrimPrefix(actual, "sha256:"), filepath.Base(output))
	closeErr := checksumTemp.Close()
	if writeErr != nil || closeErr != nil {
		os.Remove(output)
		return "", errors.New("could not write patch checksum")
	}
	if err := os.Link(checksumName, output+".sha256"); err != nil {
		os.Remove(output)
		return "", errors.New("refusing to overwrite patch checksum")
	}
	return output, nil
}

func (a *App) runDevelopmentSessionExec(ctx context.Context, format OutputFormat, sandboxID string, args []string, runtime SandboxCommandRuntime) int {
	if len(args) < 2 || args[0] != "--" {
		return a.developmentSessionError(format, ExitUsage, errors.New("session exec requires -- COMMAND..."))
	}
	command := args[1:]
	if len(command) > 32 {
		return a.developmentSessionError(format, ExitUsage, errors.New("exec command may contain at most 32 arguments"))
	}
	for _, argument := range command {
		if argument == "" || len(argument) > 1024 {
			return a.developmentSessionError(format, ExitUsage, errors.New("exec arguments must contain 1 to 1024 bytes"))
		}
	}
	result, err := runtime.Exec(ctx, sandboxID, command)
	if format == OutputJSON {
		if err == nil {
			return encodeSandboxValue(a.stdout, a.stderr, result)
		}
		if sandboxpkg.IsPartial(err) {
			if code := encodeSandboxValue(a.stdout, a.stderr, result); code != ExitSuccess {
				return code
			}
			return ExitPartial
		}
		var remote *sandboxpkg.RemoteExitError
		if errors.As(err, &remote) {
			if code := encodeSandboxValue(a.stdout, a.stderr, result); code != ExitSuccess {
				return code
			}
			return ExitFailure
		}
		return writeSandboxRuntimeError(format, a.stdout, a.stderr, err)
	}
	stdout, stdoutErr := base64.StdEncoding.DecodeString(result.StdoutBase64)
	stderr, stderrErr := base64.StdEncoding.DecodeString(result.StderrBase64)
	if stdoutErr != nil || stderrErr != nil {
		return a.developmentSessionError(format, ExitPartial, errors.New("remote command output is invalid"))
	}
	if len(stdout) != 0 {
		_, _ = a.stdout.Write(stdout)
	}
	if len(stderr) != 0 {
		_, _ = a.stderr.Write(stderr)
	}
	if err == nil {
		return ExitSuccess
	}
	if sandboxpkg.IsPartial(err) {
		fmt.Fprintln(a.stderr, "blazn: remote command evidence is incomplete")
		return ExitPartial
	}
	var remote *sandboxpkg.RemoteExitError
	if errors.As(err, &remote) {
		fmt.Fprintln(a.stderr, remote.Error())
		return ExitFailure
	}
	return writeSandboxRuntimeError(format, a.stdout, a.stderr, err)
}

func (a *App) finishDevelopmentSession(ctx context.Context, format OutputFormat, session, path string, receipt developmentSessionReceipt, args []string, runtime SandboxCommandRuntime) int {
	if receipt.Phase == "ready" {
		if len(args) == 2 && args[0] == "--patch" {
			patch, err := captureDevelopmentPatch(ctx, runtime, receipt.SandboxID, args[1])
			if err != nil {
				return a.developmentSessionError(format, ExitFailure, err)
			}
			_ = patch
			receipt.Phase = "patch_saved"
		} else if len(args) == 1 && args[0] == "--discard" {
			receipt.Phase = "discard_approved"
		} else {
			return a.developmentSessionError(format, ExitUsage, errors.New("session finish requires --patch OUTPUT_PATH or explicit --discard"))
		}
		if err := writeDevelopmentSession(path, receipt); err != nil {
			return a.developmentSessionError(format, ExitFailure, err)
		}
	} else if receipt.Phase == "starting" && len(args) == 1 && args[0] == "--discard" {
		receipt.Phase = "discard_approved"
		if err := writeDevelopmentSession(path, receipt); err != nil {
			return a.developmentSessionError(format, ExitFailure, err)
		}
	} else if len(args) != 0 {
		return a.developmentSessionError(format, ExitUsage, errors.New("resuming finish does not accept options"))
	}
	current, err := runtime.Get(ctx, receipt.SandboxID)
	if err != nil {
		return writeSandboxRuntimeError(format, a.stdout, a.stderr, err)
	}
	if current.ID != receipt.SandboxID {
		return a.developmentSessionError(format, ExitFailure, errors.New("sandbox status identity mismatch"))
	}
	if current.State == client.SandboxDeleted && current.DesiredState == client.SandboxDesiredDeleted {
		receipt.Phase = "deleted"
		_ = writeDevelopmentSession(path, receipt)
		return a.writeDevelopmentSession(format, developmentSessionResult{Session: session, Receipt: receipt, Sandbox: &current})
	}
	if receipt.Phase != "deleting" {
		receipt.Phase = "stopping"
		if err := writeDevelopmentSession(path, receipt); err != nil {
			return a.developmentSessionError(format, ExitFailure, err)
		}
		if _, err := runtime.Stop(ctx, receipt.SandboxID, receipt.RequestPrefix+"-stop"); err != nil {
			return writeSandboxRuntimeError(format, a.stdout, a.stderr, err)
		}
		for attempt := 0; attempt < developmentSessionInt("BLAZN_SESSION_STOP_ATTEMPTS", 120); attempt++ {
			current, err = runtime.Get(ctx, receipt.SandboxID)
			if err == nil && current.ID != receipt.SandboxID {
				return a.developmentSessionError(format, ExitFailure, errors.New("sandbox status identity mismatch"))
			}
			if err == nil && current.State == client.SandboxStopped {
				break
			}
			if err != nil && !sandboxpkg.IsUnavailable(err) {
				return writeSandboxRuntimeError(format, a.stdout, a.stderr, err)
			}
			time.Sleep(time.Duration(developmentSessionInt("BLAZN_SESSION_POLL_DELAY_SECONDS", 2)) * time.Second)
		}
		if current.State != client.SandboxStopped {
			return a.developmentSessionError(format, ExitFailure, errors.New("sandbox did not reach stopped state"))
		}
		receipt.Phase = "deleting"
		if err := writeDevelopmentSession(path, receipt); err != nil {
			return a.developmentSessionError(format, ExitFailure, err)
		}
	}
	if _, err := runtime.Delete(ctx, receipt.SandboxID, receipt.RequestPrefix+"-delete"); err != nil {
		return writeSandboxRuntimeError(format, a.stdout, a.stderr, err)
	}
	for attempt := 0; attempt < developmentSessionInt("BLAZN_DELETE_POLL_ATTEMPTS", 120); attempt++ {
		current, err = runtime.Get(ctx, receipt.SandboxID)
		if err == nil && current.ID != receipt.SandboxID {
			return a.developmentSessionError(format, ExitFailure, errors.New("sandbox status identity mismatch"))
		}
		if err == nil && current.State == client.SandboxDeleted && current.DesiredState == client.SandboxDesiredDeleted {
			receipt.Phase = "deleted"
			if err := writeDevelopmentSession(path, receipt); err != nil {
				return a.developmentSessionError(format, ExitFailure, err)
			}
			return a.writeDevelopmentSession(format, developmentSessionResult{Session: session, Receipt: receipt, Sandbox: &current})
		}
		if err != nil && !sandboxpkg.IsUnavailable(err) {
			return writeSandboxRuntimeError(format, a.stdout, a.stderr, err)
		}
		time.Sleep(time.Duration(developmentSessionInt("BLAZN_SESSION_POLL_DELAY_SECONDS", 2)) * time.Second)
	}
	return a.developmentSessionError(format, ExitFailure, errors.New("sandbox deletion was not proven"))
}

func developmentSessionInt(name string, fallback int) int {
	value, err := strconv.Atoi(os.Getenv(name))
	if err == nil && value >= 0 {
		return value
	}
	return fallback
}
func (a *App) developmentSessionError(format OutputFormat, exit int, err error) int {
	return a.writeError(format, exit, "development_session_failed", err.Error())
}
func (a *App) writeDevelopmentSession(format OutputFormat, result developmentSessionResult) int {
	if format == OutputJSON {
		return a.writeJSON(result)
	}
	fmt.Fprintf(a.stdout, "Development session %s [%s]", result.Session, result.Receipt.Phase)
	if result.Receipt.SandboxID != "" {
		fmt.Fprintf(a.stdout, " %s", result.Receipt.SandboxID)
	}
	if result.Patch != "" {
		fmt.Fprintf(a.stdout, " patch=%s", result.Patch)
	}
	fmt.Fprintln(a.stdout)
	return ExitSuccess
}
