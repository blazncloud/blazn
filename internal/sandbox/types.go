package sandbox

import (
	"errors"
	"fmt"
	"net"

	"github.com/blazncloud/blazn/internal/client"
)

const IsolationNotice = client.SandboxIsolationNotice

type TemplateValidation struct {
	Valid          bool     `json:"valid"`
	ManifestDigest *string  `json:"manifestDigest"`
	Errors         []string `json:"errors"`
	Warnings       []string `json:"warnings"`
}

type TemplatePublish struct {
	Template client.SandboxTemplate        `json:"template"`
	Version  client.SandboxTemplateVersion `json:"version"`
}

type ExecResult struct {
	SandboxID      string `json:"sandboxId"`
	GrantID        string `json:"grantId"`
	RemoteExitCode int    `json:"remoteExitCode"`
	StdoutBase64   string `json:"stdoutBase64"`
	StderrBase64   string `json:"stderrBase64"`
	Truncated      bool   `json:"truncated"`
}

type TransferResult struct {
	SandboxID   string `json:"sandboxId"`
	GrantID     string `json:"grantId"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
}

// PartialError means a one-time grant may have been consumed, so retrying the
// operation cannot establish complete evidence from the first attempt.
type PartialError struct{ Cause error }

func (e *PartialError) Error() string {
	return "sandbox operation has partial evidence: " + e.Cause.Error()
}
func (e *PartialError) Unwrap() error { return e.Cause }

func IsPartial(err error) bool {
	var partial *PartialError
	return errors.As(err, &partial)
}

// UnavailableError marks failures where the API or event transport could not
// provide an application response.
type UnavailableError struct{ Cause error }

func (e *UnavailableError) Error() string { return "sandbox service unavailable: " + e.Cause.Error() }
func (e *UnavailableError) Unwrap() error { return e.Cause }

func IsUnavailable(err error) bool {
	var unavailable *UnavailableError
	if errors.As(err, &unavailable) {
		return true
	}
	var apiErr *client.APIError
	if errors.As(err, &apiErr) && (apiErr.StatusCode == 429 || apiErr.StatusCode >= 500) {
		return true
	}
	var networkError net.Error
	return errors.As(err, &networkError)
}

type RemoteExitError struct{ Code int }

func (e *RemoteExitError) Error() string {
	return fmt.Sprintf("remote command exited with status %d", e.Code)
}

type WatchTerminal string

const (
	WatchReady   WatchTerminal = "ready"
	WatchFailed  WatchTerminal = "failed"
	WatchDeleted WatchTerminal = "deleted"
)
