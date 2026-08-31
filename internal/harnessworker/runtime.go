package harnessworker

import (
	"context"
	"errors"
	"reflect"
	"time"
)

type Runtime struct{ config RunConfig }

func NewRuntime(config RunConfig) (*Runtime, error) {
	if config.ScopeValidator == nil || config.TokenSource == nil || config.Adapter == nil || config.ProcessRunner == nil || config.Collector == nil ||
		ValidateExecution(config.Execution, config.Artifacts) != nil || config.AllowedExecutable == "" || config.Execution.Argv[0] != config.AllowedExecutable {
		return nil, errors.New("harness worker configuration is invalid")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Runtime{config: config}, nil
}

func (r *Runtime) Run(ctx context.Context, assignment Assignment) Result {
	started := r.config.Now()
	result := Result{SchemaVersion: HarnessWorkerSchemaVersion, Type: ResponseTypeResult, ScopeDigest: ScopeDigest(assignment.Scope), Status: "failed", Artifacts: []ArtifactResult{}, Warnings: []string{}}
	if err := assignment.ValidateAt(started); err != nil {
		result.ErrorCode = "assignment_invalid"
		return finish(result, started, r.config.Now())
	}
	if err := r.config.ScopeValidator.ValidateWorkloadScope(ctx, assignment.Scope); err != nil {
		result.ErrorCode = ErrorCode(err)
		return finish(result, started, r.config.Now())
	}
	token, err := r.config.TokenSource.OpenListenerToken(ctx, assignment.Scope.ListenerTokenFingerprint)
	if err != nil {
		result.ErrorCode = ErrorCode(err)
		return finish(result, started, r.config.Now())
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(r.config.Execution.TimeoutSeconds)*time.Second)
	spec, prepareErr := r.config.Adapter.Prepare(runCtx, assignment, token)
	var process ProcessResult
	var runErr, finalizeErr error
	if prepareErr != nil || !reflect.DeepEqual(spec.Execution, r.config.Execution) || len(spec.ExtraFiles) != 1 || spec.ExtraFiles[0] != token || validateProcessSpec(spec) != nil {
		prepareErr = errors.New("adapter process spec is invalid")
	} else {
		process, runErr = r.config.ProcessRunner.Run(runCtx, spec)
		finalizeCtx, finalizeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		finalizeErr = r.config.Adapter.Finalize(finalizeCtx, process)
		finalizeCancel()
	}
	cancel()
	closeErr := token.Close()
	result.OutputTruncated = process.OutputTruncated
	result.ProcessTreeTerminated = process.TreeKilled
	if process.Exited {
		exitCode := process.ExitCode
		result.ExitCode = &exitCode
	}
	if prepareErr != nil {
		result.ErrorCode = "adapter_prepare_failed"
	} else if finalizeErr != nil {
		result.Status, result.ErrorCode = "recovery_required", "adapter_output_invalid"
	} else if runErr != nil {
		if ErrorCode(runErr) == "process_cleanup_unproven" {
			result.Status, result.ErrorCode = "recovery_required", "process_cleanup_unproven"
		} else {
			result.ErrorCode = "process_start_failed"
		}
	} else if process.TimedOut {
		result.Status, result.ErrorCode = "timed_out", "process_timed_out"
	} else if process.Canceled {
		result.Status, result.ErrorCode = "cancelled", "process_cancelled"
	} else if !process.Exited || process.ExitCode != 0 {
		result.ErrorCode = "process_failed"
	} else {
		result.Status = "succeeded"
	}
	validationCtx, validationCancel := context.WithTimeout(context.Background(), 5*time.Second)
	postflightErr := r.config.ScopeValidator.ValidateWorkloadScope(validationCtx, assignment.Scope)
	validationCancel()
	if closeErr != nil || postflightErr != nil {
		result.Status, result.ErrorCode, result.Artifacts = "recovery_required", "postflight_scope_failed", []ArtifactResult{}
		return finish(result, started, r.config.Now())
	}
	artifactCtx, artifactCancel := context.WithTimeout(context.Background(), 30*time.Second)
	artifacts, warnings, artifactErr := r.config.Collector.Collect(artifactCtx, r.config.Artifacts, result.Status == "succeeded")
	artifactCancel()
	if artifactErr != nil {
		if result.Status == "succeeded" {
			result.Status, result.ErrorCode = "failed", "artifact_collection_failed"
		} else {
			result.Warnings = append(result.Warnings, "artifact_collection_failed")
		}
	} else {
		result.Artifacts, result.Warnings = artifacts, append(result.Warnings, warnings...)
	}
	return finish(result, started, r.config.Now())
}

func finish(result Result, started, ended time.Time) Result {
	if result.Artifacts == nil {
		result.Artifacts = []ArtifactResult{}
	}
	if result.Warnings == nil {
		result.Warnings = []string{}
	}
	result.DurationMS = ended.Sub(started).Milliseconds()
	if result.DurationMS < 0 {
		result.DurationMS = 0
	}
	return result
}
