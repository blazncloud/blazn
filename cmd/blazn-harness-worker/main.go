package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/blazncloud/blazn/internal/harnessadapter/hermes"
	"github.com/blazncloud/blazn/internal/harnessworker"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args, os.Stdin, os.Stdout, os.Getenv("BLAZN_PROXY_URL"), time.Now))
}

func run(ctx context.Context, arguments []string, input io.Reader, output io.Writer, proxyURL string, now func() time.Time) int {
	execution, err := parseExecution(arguments)
	if err != nil {
		_ = harnessworker.EncodeResponse(output, harnessworker.ErrorResponse{SchemaVersion: harnessworker.HarnessWorkerSchemaVersion, Type: harnessworker.ResponseTypeError, ErrorCode: "launch_config_invalid"})
		return 2
	}
	adapter, err := hermes.New(hermes.Config{ProxyURL: proxyURL, Output: output, ArtifactRoot: harnessworker.DefaultArtifactRoot, Now: now})
	if err != nil {
		_ = harnessworker.EncodeResponse(output, harnessworker.ErrorResponse{SchemaVersion: harnessworker.HarnessWorkerSchemaVersion, Type: harnessworker.ResponseTypeError, ErrorCode: "launch_config_invalid"})
		return 2
	}
	artifacts := fixedArtifacts()
	runtime, err := harnessworker.NewRuntime(harnessworker.RunConfig{
		ScopeValidator:    harnessworker.FileScopeValidator{Path: harnessworker.DefaultScopePath},
		TokenSource:       harnessworker.ProtectedListenerTokenFile{Path: harnessworker.DefaultListenerToken},
		Adapter:           adapter,
		ProcessRunner:     harnessworker.ExecProcessRunner{},
		Collector:         harnessworker.FileArtifactCollector{Root: harnessworker.DefaultArtifactRoot},
		Execution:         execution,
		Artifacts:         artifacts,
		AllowedExecutable: hermes.ReviewedExecutable,
		Now:               now,
	})
	if err != nil {
		_ = harnessworker.EncodeResponse(output, harnessworker.ErrorResponse{SchemaVersion: harnessworker.HarnessWorkerSchemaVersion, Type: harnessworker.ResponseTypeError, ErrorCode: "worker_config_invalid"})
		return 2
	}
	assignment, err := harnessworker.DecodeAssignmentLine(ctx, input, now())
	if err != nil {
		_ = harnessworker.EncodeResponse(output, harnessworker.ErrorResponse{SchemaVersion: harnessworker.HarnessWorkerSchemaVersion, Type: harnessworker.ResponseTypeError, ErrorCode: harnessworker.ErrorCode(err)})
		return 2
	}
	result := runtime.Run(ctx, assignment)
	if err := harnessworker.EncodeResponse(output, result); err != nil {
		fmt.Fprintln(os.Stderr, "harness worker response failed")
		return 1
	}
	if result.Status == "succeeded" {
		return 0
	}
	return 1
}

func parseExecution(arguments []string) (harnessworker.Execution, error) {
	expected := []string{"--", hermes.ReviewedExecutable, hermes.ReviewedArgv[0], hermes.ReviewedArgv[1]}
	if len(arguments) != len(expected)+1 {
		return harnessworker.Execution{}, fmt.Errorf("launch configuration is invalid")
	}
	for index, value := range expected {
		if arguments[index+1] != value {
			return harnessworker.Execution{}, fmt.Errorf("launch configuration is invalid")
		}
	}
	return harnessworker.Execution{Argv: append([]string(nil), arguments[2:]...), WorkingDirectory: "/workspace", TimeoutSeconds: harnessworker.DefaultRunSeconds, CancelGraceSeconds: harnessworker.DefaultCancelSeconds}, nil
}

func fixedArtifacts() []harnessworker.ArtifactSpec {
	return []harnessworker.ArtifactSpec{
		{Name: "patch", Path: "/workspace/artifacts/patch.diff", Role: "patch", Kind: "agent.patch", MediaType: "text/x-diff", Required: true, MaxBytes: harnessworker.MaxArtifactBytes},
		{Name: "summary", Path: "/workspace/artifacts/summary.md", Role: "summary", Kind: "agent.summary", MediaType: "text/markdown", Required: true, MaxBytes: harnessworker.MaxArtifactBytes},
	}
}
