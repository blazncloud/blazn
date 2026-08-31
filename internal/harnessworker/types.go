package harnessworker

import (
	"context"
	"io"
	"os"
	"time"
)

const (
	ResponseTypeResult     = "result"
	ResponseTypeError      = "error"
	DefaultScopePath       = "/run/blazn-harness/workload-scope.json"
	DefaultListenerToken   = "/run/blazn-harness/listener-token"
	DefaultArtifactRoot    = "/workspace/artifacts"
	MaxProtocolLineBytes   = 128 << 10
	MaxListenerTokenBytes  = 4 << 10
	MaxArtifactBytes       = 8 << 20
	MaxTotalArtifactBytes  = 32 << 20
	MaxArtifacts           = 32
	MaxArguments           = 64
	MaxArgumentBytes       = 4096
	MaxArgumentTotalBytes  = 32 << 10
	DefaultRunSeconds      = 60 * 60
	DefaultCancelSeconds   = 5
	MaxRunSeconds          = 24 * 60 * 60
	MaxCancellationSeconds = 30
)

type Execution struct {
	Argv               []string `json:"argv"`
	WorkingDirectory   string   `json:"workingDirectory"`
	TimeoutSeconds     int      `json:"timeoutSeconds"`
	CancelGraceSeconds int      `json:"cancelGraceSeconds"`
}

type ArtifactSpec struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Role      string `json:"role"`
	Kind      string `json:"kind"`
	MediaType string `json:"mediaType"`
	Required  bool   `json:"required"`
	MaxBytes  int64  `json:"maxBytes"`
}

type ArtifactResult struct {
	Name          string `json:"name"`
	Role          string `json:"role"`
	Kind          string `json:"kind"`
	MediaType     string `json:"mediaType"`
	Size          int64  `json:"size"`
	ContentDigest string `json:"contentDigest"`
}

type Result struct {
	SchemaVersion         string           `json:"schemaVersion"`
	Type                  string           `json:"type"`
	ScopeDigest           string           `json:"scopeDigest"`
	Status                string           `json:"status"`
	ErrorCode             string           `json:"errorCode,omitempty"`
	ExitCode              *int             `json:"exitCode"`
	ProcessTreeTerminated bool             `json:"processTreeTerminated"`
	OutputTruncated       bool             `json:"outputTruncated"`
	Artifacts             []ArtifactResult `json:"artifacts"`
	Warnings              []string         `json:"warnings"`
	DurationMS            int64            `json:"durationMs"`
}

type ErrorResponse struct {
	SchemaVersion string `json:"schemaVersion"`
	Type          string `json:"type"`
	ErrorCode     string `json:"errorCode"`
}

type ProcessResult struct {
	ExitCode, Signal                                         int
	Exited, Canceled, TimedOut, TreeKilled, ProcessGroupGone bool
	OutputTruncated                                          bool
}

type ProcessSpec struct {
	Execution   Execution
	Environment []string
	Stdin       io.Reader
	Stdout      io.Writer
	ExtraFiles  []*os.File
	Abort       <-chan error
}

type Adapter interface {
	Prepare(context.Context, Assignment, *os.File) (ProcessSpec, error)
	Finalize(context.Context, ProcessResult) error
}

type RunConfig struct {
	ScopeValidator    WorkloadScopeValidator
	TokenSource       ListenerTokenSource
	Adapter           Adapter
	ProcessRunner     ProcessRunner
	Collector         ArtifactCollector
	Execution         Execution
	Artifacts         []ArtifactSpec
	AllowedExecutable string
	Now               func() time.Time
}
