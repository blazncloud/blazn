package sandboxagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	maxToolOutput   = 24 << 10
	maxCapture      = 4 << 20
	maxReadBytes    = 256 << 10
	maxWriteBytes   = 1 << 20
	maxCommandBytes = 32 << 10
)

type toolResult struct {
	Output    string
	OK        bool
	ExitCode  int
	Truncated bool
}

func toolDefinitions() []map[string]any {
	function := func(name, description string, properties map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "function", "function": map[string]any{"name": name, "description": description,
			"parameters": map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}}}
	}
	text := func(description string) map[string]any {
		return map[string]any{"type": "string", "description": description}
	}
	return []map[string]any{
		function("run_command", "Run a shell command in the sandbox and return its combined output and exit code.",
			map[string]any{"command": text("The command line, interpreted by /bin/sh."),
				"timeoutSeconds": map[string]any{"type": "integer", "description": "Optional timeout; the default applies when omitted."}}, "command"),
		function("read_file", "Read a text file.", map[string]any{"path": text("Absolute path, or a path relative to the working directory.")}, "path"),
		function("write_file", "Create or replace a text file, creating parent directories as needed.",
			map[string]any{"path": text("Absolute path, or a path relative to the working directory."), "content": text("The complete new file content.")}, "path", "content"),
	}
}

func toolSummary(call toolCall) string {
	var arguments map[string]any
	if json.Unmarshal([]byte(call.Function.Arguments), &arguments) != nil {
		return ""
	}
	for _, key := range []string{"command", "path"} {
		if value, ok := arguments[key].(string); ok {
			return preview(strings.ReplaceAll(value, "\n", " "), 200)
		}
	}
	return ""
}

func runTool(ctx context.Context, config Config, call toolCall) toolResult {
	fail := func(format string, values ...any) toolResult {
		return toolResult{Output: "error: " + fmt.Sprintf(format, values...), ExitCode: -1}
	}
	switch call.Function.Name {
	case "run_command":
		var arguments struct {
			Command        string `json:"command"`
			TimeoutSeconds int    `json:"timeoutSeconds"`
		}
		if json.Unmarshal([]byte(call.Function.Arguments), &arguments) != nil || arguments.Command == "" || len(arguments.Command) > maxCommandBytes {
			return fail("run_command needs a non-empty command of at most %d bytes", maxCommandBytes)
		}
		timeout := arguments.TimeoutSeconds
		if timeout <= 0 || timeout > config.CommandTimeoutSeconds {
			timeout = config.CommandTimeoutSeconds
		}
		return runCommand(ctx, config.WorkingDirectory, arguments.Command, time.Duration(timeout)*time.Second)
	case "read_file":
		var arguments struct {
			Path string `json:"path"`
		}
		if json.Unmarshal([]byte(call.Function.Arguments), &arguments) != nil || arguments.Path == "" {
			return fail("read_file needs a path")
		}
		path := resolvePath(config.WorkingDirectory, arguments.Path)
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			return fail("%s is not a readable regular file", arguments.Path)
		}
		file, err := os.Open(path)
		if err != nil {
			return fail("%s could not be opened", arguments.Path)
		}
		defer file.Close()
		buffer := make([]byte, maxReadBytes+1)
		read, _ := file.Read(buffer)
		truncated := read > maxReadBytes
		if truncated {
			read = maxReadBytes
		}
		output := string(buffer[:read])
		if truncated {
			output += fmt.Sprintf("\n[truncated: file is %d bytes]", info.Size())
		}
		return toolResult{Output: output, OK: true, Truncated: truncated}
	case "write_file":
		var arguments struct {
			Path    string  `json:"path"`
			Content *string `json:"content"`
		}
		if json.Unmarshal([]byte(call.Function.Arguments), &arguments) != nil || arguments.Path == "" || arguments.Content == nil || len(*arguments.Content) > maxWriteBytes {
			return fail("write_file needs a path and content of at most %d bytes", maxWriteBytes)
		}
		path := resolvePath(config.WorkingDirectory, arguments.Path)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fail("the parent directory of %s could not be created", arguments.Path)
		}
		if err := os.WriteFile(path, []byte(*arguments.Content), 0o644); err != nil {
			return fail("%s could not be written", arguments.Path)
		}
		return toolResult{Output: fmt.Sprintf("wrote %d bytes to %s", len(*arguments.Content), path), OK: true}
	default:
		return fail("unknown tool %q", preview(call.Function.Name, 64))
	}
}

func resolvePath(workingDirectory, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(workingDirectory, path)
}

type boundedBuffer struct {
	mu      sync.Mutex
	data    bytes.Buffer
	dropped bool
}

func (b *boundedBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if remaining := maxCapture - b.data.Len(); remaining < len(value) {
		b.dropped = true
		if remaining > 0 {
			b.data.Write(value[:remaining])
		}
	} else {
		b.data.Write(value)
	}
	return len(value), nil
}

func runCommand(ctx context.Context, directory, commandLine string, timeout time.Duration) toolResult {
	command := exec.Command("/bin/sh", "-c", commandLine)
	command.Dir = directory
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	output := &boundedBuffer{}
	command.Stdout, command.Stderr = output, output
	if err := command.Start(); err != nil {
		return toolResult{Output: "error: the command could not be started", ExitCode: -1}
	}
	finished := make(chan error, 1)
	go func() { finished <- command.Wait() }()
	note := ""
	var waitErr error
	select {
	case waitErr = <-finished:
	case <-time.After(timeout):
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		waitErr = <-finished
		note = fmt.Sprintf("\n[timed out after %s and was killed]", timeout)
	case <-ctx.Done():
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		waitErr = <-finished
		note = "\n[cancelled]"
	}
	exitCode := 0
	var exitError *exec.ExitError
	if errors.As(waitErr, &exitError) {
		exitCode = exitError.ExitCode()
	} else if waitErr != nil {
		exitCode = -1
	}
	text, truncated := clip(output.data.String(), maxToolOutput)
	truncated = truncated || output.dropped
	return toolResult{Output: fmt.Sprintf("%s%s\n[exit code %d]", text, note, exitCode), OK: exitCode == 0 && note == "", ExitCode: exitCode, Truncated: truncated}
}

// clip keeps the head and tail of long output, where the useful parts usually are.
func clip(value string, limit int) (string, bool) {
	value = strings.ToValidUTF8(value, "�")
	if len(value) <= limit {
		return value, false
	}
	head, tail := limit*2/3, limit/3
	return strings.ToValidUTF8(value[:head], "") + fmt.Sprintf("\n[... %d bytes omitted ...]\n", len(value)-head-tail) + strings.ToValidUTF8(value[len(value)-tail:], ""), true
}

// A Sandbox source checkout is a plain tree with no .git directory, so the
// harness keeps its own baseline: a private git directory beside its state
// that snapshots the checkout before the first turn. The patch is the diff
// against that snapshot. The checkout itself is never given a .git directory.
func runGit(ctx context.Context, timeout time.Duration, gitDirectory, workTree string, arguments ...string) ([]byte, error) {
	commandContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	base := []string{"--git-dir=" + gitDirectory, "--work-tree=" + workTree, "-c", "safe.directory=*", "-c", "core.hooksPath=/dev/null",
		"-c", "commit.gpgsign=false", "-c", "user.name=blazn-agent", "-c", "user.email=agent@blazn.invalid", "-c", "gc.auto=0"}
	command := exec.CommandContext(commandContext, "git", append(base, arguments...)...)
	command.Dir = workTree
	command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0")
	output := &boundedBuffer{}
	command.Stdout = output
	err := command.Run()
	if output.dropped {
		return nil, errors.New("git output is too large")
	}
	return output.data.Bytes(), err
}

// ensureBaseline snapshots the checkout once. It builds the snapshot in a
// temporary directory and renames it, so a crash never leaves a baseline that
// already contains the Agent's changes.
func ensureBaseline(ctx context.Context, gitDirectory, workTree string) error {
	if _, err := os.Stat(gitDirectory); err == nil {
		return nil
	}
	building := gitDirectory + ".building"
	if err := os.RemoveAll(building); err != nil {
		return err
	}
	for _, arguments := range [][]string{{"init", "-q"}, {"add", "--all"}, {"commit", "-q", "--allow-empty", "--no-verify", "-m", "baseline"}} {
		if _, err := runGit(ctx, 10*time.Minute, building, workTree, arguments...); err != nil {
			_ = os.RemoveAll(building)
			return fmt.Errorf("baseline %s: %w", arguments[0], err)
		}
	}
	return os.Rename(building, gitDirectory)
}

// repositoryPatch returns the diff of the checkout against the baseline, including new files.
func repositoryPatch(ctx context.Context, gitDirectory, workTree string) ([]byte, error) {
	if _, err := os.Stat(gitDirectory); err != nil {
		return nil, errors.New("no baseline was recorded")
	}
	// The index is private to the harness, so staging everything is safe and makes new files part of the diff.
	if _, err := runGit(ctx, 2*time.Minute, gitDirectory, workTree, "add", "--all"); err != nil {
		return nil, err
	}
	return runGit(ctx, 2*time.Minute, gitDirectory, workTree, "diff", "--cached", "--no-color", "--no-ext-diff", "--binary", "HEAD")
}
