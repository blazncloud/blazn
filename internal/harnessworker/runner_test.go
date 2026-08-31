//go:build linux || darwin

package harnessworker

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestExecProcessRunnerUsesExactArgvWithoutShell(t *testing.T) {
	directory := t.TempDir()
	output := filepath.Join(directory, "argv")
	literal := "$(touch " + filepath.Join(directory, "injected") + ")"
	spec, closeToken := helperProcessSpec(t, directory, "write", output, literal)
	defer closeToken()
	result, err := (ExecProcessRunner{}).Run(context.Background(), spec)
	if runtime.GOOS == "linux" && err != nil || runtime.GOOS == "darwin" && ErrorCode(err) != "process_cleanup_unproven" || !result.Exited || result.ExitCode != 0 || !result.ProcessGroupGone || result.TreeKilled {
		t.Fatalf("process result=%#v err=%v", result, err)
	}
	body, err := os.ReadFile(output)
	if err != nil || string(body) != literal {
		t.Fatalf("exact argv body=%q err=%v", body, err)
	}
	if _, err := os.Stat(filepath.Join(directory, "injected")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("argument was interpreted by a shell")
	}
}

func TestExecProcessRunnerTimeoutTerminatesProcessGroup(t *testing.T) {
	directory := t.TempDir()
	pidPath := filepath.Join(directory, "child.pid")
	spec, closeToken := helperProcessSpec(t, directory, "spawn-wait", pidPath)
	defer closeToken()
	spec.Execution.CancelGraceSeconds = 1
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	started := time.Now()
	result, err := (ExecProcessRunner{}).Run(ctx, spec)
	if runtime.GOOS == "linux" && (err != nil || !result.TreeKilled || !result.CleanupComplete) || runtime.GOOS == "darwin" && (ErrorCode(err) != "process_cleanup_unproven" || result.TreeKilled || result.CleanupComplete) || !result.Canceled || !result.TimedOut || !result.ProcessGroupGone || time.Since(started) > 3*time.Second {
		t.Fatalf("cancel result=%#v elapsed=%v err=%v", result, time.Since(started), err)
	}
	body, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(string(body))
	deadline := time.Now().Add(2 * time.Second)
	for processExists(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if processExists(pid) {
		t.Fatal("descendant survived process-group cancellation")
	}
}

func TestExecProcessRunnerUsesHermesGracefulSIGINT(t *testing.T) {
	directory := t.TempDir()
	observed := filepath.Join(directory, "signal")
	ready := filepath.Join(directory, "ready")
	spec, closeToken := helperProcessSpec(t, directory, "await-sigint", observed, ready)
	defer closeToken()
	spec.Execution.CancelGraceSeconds = 1
	ctx, cancel := context.WithCancel(context.Background())
	type outcome struct {
		result ProcessResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := (ExecProcessRunner{}).Run(ctx, spec)
		done <- outcome{result: result, err: err}
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("helper did not install SIGINT handler")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	var completed outcome
	select {
	case completed = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runner did not finish after SIGINT")
	}
	result, err := completed.result, completed.err
	if runtime.GOOS == "linux" && err != nil || runtime.GOOS == "darwin" && ErrorCode(err) != "process_cleanup_unproven" || !result.Canceled {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	body, readErr := os.ReadFile(observed)
	if readErr != nil || string(body) != "interrupt" {
		t.Fatalf("graceful signal=%q err=%v", body, readErr)
	}
}

func TestProcessSpecRejectsEnvironmentAndDescriptorExpansion(t *testing.T) {
	spec, closeToken := helperProcessSpec(t, t.TempDir(), "write", filepath.Join(t.TempDir(), "out"), "ok")
	defer closeToken()
	spec.Environment = append(spec.Environment, "SECRET=forbidden")
	if validateProcessSpec(spec) == nil {
		t.Fatal("unapproved environment passed")
	}
	spec.Environment = []string{"BLAZN_PROXY_URL=http://example.com:8080", "BLAZN_LISTENER_TOKEN_FD=3"}
	if validateProcessSpec(spec) == nil {
		t.Fatal("non-loopback proxy origin passed")
	}
	spec.Environment = []string{"BLAZN_PROXY_URL=http://127.0.0.1:8080/path", "BLAZN_LISTENER_TOKEN_FD=3"}
	if validateProcessSpec(spec) == nil {
		t.Fatal("proxy URL with path passed")
	}
	spec.Environment = []string{"BLAZN_PROXY_URL=http://127.0.0.1:8080", "BLAZN_LISTENER_TOKEN_FD=3"}
	spec.Environment = spec.Environment[:2]
	spec.ExtraFiles = append(spec.ExtraFiles, spec.ExtraFiles[0])
	if validateProcessSpec(spec) == nil {
		t.Fatal("extra inherited descriptor passed")
	}
}

func TestReceiveWaitIsBoundedWhenWaitNeverReturns(t *testing.T) {
	started := time.Now()
	if _, ok := receiveWait(make(chan error), 20*time.Millisecond); ok || time.Since(started) > time.Second {
		t.Fatalf("unbounded wait elapsed=%v", time.Since(started))
	}
}

func helperProcessSpec(t *testing.T, directory string, arguments ...string) (ProcessSpec, func()) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	token, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("test-listener-token")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	argv := []string{executable, "-test.run=^TestHarnessWorkerProcessHelper$", "--", "harnessworker-helper"}
	argv = append(argv, arguments...)
	return ProcessSpec{
		Execution:   Execution{Argv: argv, WorkingDirectory: directory, TimeoutSeconds: 30, CancelGraceSeconds: 1},
		Environment: []string{"BLAZN_PROXY_URL=http://127.0.0.1:8080", "BLAZN_LISTENER_TOKEN_FD=3"},
		Stdin:       strings.NewReader(""), Stdout: io.Discard, ExtraFiles: []*os.File{token},
	}, func() { _ = token.Close() }
}

func TestHarnessWorkerProcessHelper(t *testing.T) {
	marker := -1
	for index, argument := range os.Args {
		if argument == "harnessworker-helper" {
			marker = index
			break
		}
	}
	if marker < 0 {
		return
	}
	arguments := os.Args[marker+1:]
	switch arguments[0] {
	case "write":
		if err := os.WriteFile(arguments[1], []byte(arguments[2]), 0o600); err != nil {
			os.Exit(10)
		}
	case "spawn-wait":
		child := exec.Command("/bin/sleep", "30")
		if err := child.Start(); err != nil {
			os.Exit(11)
		}
		if err := os.WriteFile(arguments[1], []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			os.Exit(12)
		}
		_ = child.Wait()
	case "spawn-setsid-wait":
		executable, err := os.Executable()
		if err != nil {
			os.Exit(14)
		}
		child := exec.Command(executable, "-test.run=^TestHarnessWorkerProcessHelper$", "--", "harnessworker-helper", "escape-wait", arguments[1])
		child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := child.Start(); err != nil {
			os.Exit(15)
		}
		_ = child.Wait()
	case "escape-wait":
		if err := os.WriteFile(arguments[1], []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			os.Exit(16)
		}
		time.Sleep(30 * time.Second)
	case "await-sigint":
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGINT)
		defer signal.Stop(signals)
		if err := os.WriteFile(arguments[2], []byte("ready"), 0o600); err != nil {
			os.Exit(18)
		}
		received := <-signals
		if err := os.WriteFile(arguments[1], []byte(received.String()), 0o600); err != nil {
			os.Exit(17)
		}
	default:
		os.Exit(13)
	}
	os.Exit(0)
}

func processExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
