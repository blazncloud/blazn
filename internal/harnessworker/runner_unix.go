//go:build linux || darwin

package harnessworker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

func (ExecProcessRunner) Run(ctx context.Context, spec ProcessSpec) (ProcessResult, error) {
	if err := validateProcessSpec(spec); err != nil {
		return ProcessResult{}, errors.New("process launch configuration is invalid")
	}
	execution := spec.Execution
	command := exec.Command(execution.Argv[0], execution.Argv[1:]...)
	command.Dir = execution.WorkingDirectory
	command.Env = append([]string{"HOME=/workspace", "LANG=C.UTF-8", "TMPDIR=/workspace/tmp"}, spec.Environment...)
	command.ExtraFiles = append([]*os.File(nil), spec.ExtraFiles...)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stderr := &boundedDiscard{limit: maxCapturedProcessBytes}
	command.Stdin, command.Stdout, command.Stderr = spec.Stdin, spec.Stdout, stderr
	if err := command.Start(); err != nil {
		return ProcessResult{}, err
	}
	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()
	result := ProcessResult{}
	select {
	case err := <-wait:
		setExitResult(&result, command, err)
		// A completed adapter may not leave detached work in its process group.
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		result.ProcessGroupGone = waitProcessGroupGone(command.Process.Pid, time.Second)
	case <-ctx.Done():
		terminateProcessTree(command, execution, wait, &result, errors.Is(ctx.Err(), context.DeadlineExceeded))
	case <-spec.Abort:
		terminateProcessTree(command, execution, wait, &result, false)
	}
	result.OutputTruncated = stderr.truncated
	if !result.ProcessGroupGone {
		return result, protocolError("process_cleanup_unproven")
	}
	return result, nil
}

func terminateProcessTree(command *exec.Cmd, execution Execution, wait <-chan error, result *ProcessResult, timedOut bool) {
	result.Canceled = true
	result.TimedOut = timedOut
	_ = syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
	grace := time.NewTimer(time.Duration(execution.CancelGraceSeconds) * time.Second)
	select {
	case err := <-wait:
		if !grace.Stop() {
			<-grace.C
		}
		setExitResult(result, command, err)
	case <-grace.C:
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		setExitResult(result, command, <-wait)
	}
	// Kill any descendant which survived the leader's graceful exit.
	_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	result.ProcessGroupGone = waitProcessGroupGone(command.Process.Pid, time.Second)
	result.TreeKilled = result.ProcessGroupGone
}

func waitProcessGroupGone(processGroupID int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Kill(-processGroupID, 0)
		if errors.Is(err, syscall.ESRCH) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func setExitResult(result *ProcessResult, command *exec.Cmd, waitErr error) {
	if command.ProcessState == nil {
		return
	}
	result.Exited = true
	result.ExitCode = command.ProcessState.ExitCode()
	status, ok := command.ProcessState.Sys().(syscall.WaitStatus)
	if ok && status.Signaled() {
		result.Signal = int(status.Signal())
	}
}
