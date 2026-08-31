package harnessworker

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const maxCapturedProcessBytes = 1 << 20

type ProcessRunner interface {
	Run(context.Context, ProcessSpec) (ProcessResult, error)
}

type ExecProcessRunner struct{}

type boundedDiscard struct {
	written   int64
	limit     int64
	truncated bool
}

func (w *boundedDiscard) Write(value []byte) (int, error) {
	if remaining := w.limit - w.written; int64(len(value)) > remaining {
		w.written = w.limit
		w.truncated = true
	} else {
		w.written += int64(len(value))
	}
	return len(value), nil
}

var _ io.Writer = (*boundedDiscard)(nil)

func validateProcessSpec(spec ProcessSpec) error {
	if !validProcessExecution(spec.Execution) || spec.Stdin == nil || spec.Stdout == nil || len(spec.ExtraFiles) != 1 || !validOneShotDescriptor(spec.ExtraFiles[0]) {
		return errors.New("process spec is incomplete")
	}
	seen := map[string]bool{}
	for _, entry := range spec.Environment {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || seen[key] {
			return errors.New("process environment is invalid")
		}
		seen[key] = true
		switch key {
		case "BLAZN_LISTENER_TOKEN_FD":
			if value != "3" {
				return errors.New("listener token descriptor is invalid")
			}
		case "BLAZN_PROXY_URL":
			if !validLoopbackProxyURL(value) {
				return errors.New("proxy URL is invalid")
			}
		default:
			return errors.New("process environment key is not allowed")
		}
	}
	if !seen["BLAZN_LISTENER_TOKEN_FD"] || !seen["BLAZN_PROXY_URL"] {
		return errors.New("process environment is incomplete")
	}
	return nil
}

func validOneShotDescriptor(file *os.File) bool {
	if file == nil {
		return false
	}
	info, err := file.Stat()
	if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		return false
	}
	_, err = file.Seek(0, io.SeekCurrent)
	return err != nil
}

func validLoopbackProxyURL(value string) bool {
	if value == "" || len(value) > 256 {
		return false
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" || parsed.Port() == "" {
		return false
	}
	address := net.ParseIP(parsed.Hostname())
	return address != nil && address.IsLoopback()
}

func validProcessExecution(execution Execution) bool {
	if len(execution.Argv) == 0 || len(execution.Argv) > MaxArguments || !filepath.IsAbs(execution.Argv[0]) || filepath.Clean(execution.Argv[0]) != execution.Argv[0] ||
		!filepath.IsAbs(execution.WorkingDirectory) || filepath.Clean(execution.WorkingDirectory) != execution.WorkingDirectory || execution.TimeoutSeconds < 1 || execution.TimeoutSeconds > MaxRunSeconds ||
		execution.CancelGraceSeconds < 1 || execution.CancelGraceSeconds > MaxCancellationSeconds {
		return false
	}
	total := 0
	for _, argument := range execution.Argv {
		if argument == "" || len(argument) > MaxArgumentBytes || strings.ContainsRune(argument, 0) {
			return false
		}
		total += len(argument)
	}
	return total <= MaxArgumentTotalBytes
}
