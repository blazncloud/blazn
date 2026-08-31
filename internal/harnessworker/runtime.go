package harnessworker

import (
	"context"
	"errors"
	"reflect"
	"time"
)

type Runtime struct{ config RunConfig }

func NewRuntime(config RunConfig) (*Runtime, error) {
	if config.ScopeValidator == nil || config.ExecutableVerifier == nil || config.TokenSource == nil || config.Adapter == nil || config.ProcessRunner == nil || config.Collector == nil ||
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
	if err := r.config.ExecutableVerifier.VerifyExecutable(ctx, r.config.AllowedExecutable, assignment.Scope.HarnessExecutableDigest); err != nil {
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
	prepared := prepareErr == nil
	sourceCloseErr := token.Close()
	for _, file := range spec.ExtraFiles {
		if file != nil {
			defer file.Close()
		}
	}
	var process ProcessResult
	var runErr, finalizeErr error
	runnerInvoked := false
	if prepareErr != nil || sourceCloseErr != nil || !reflect.DeepEqual(spec.Execution, r.config.Execution) || len(spec.ExtraFiles) != 1 || !validOneShotDescriptor(spec.ExtraFiles[0]) || validateProcessSpec(spec) != nil {
		prepareErr = errors.New("adapter process spec is invalid")
	} else {
		runnerInvoked = true
		process, runErr = r.config.ProcessRunner.Run(runCtx, spec)
	}
	if prepared {
		finalizeCtx, finalizeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		finalizeErr = r.config.Adapter.Finalize(finalizeCtx, process)
		finalizeCancel()
	}
	cancel()
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
	} else if !process.CleanupComplete {
		result.Status, result.ErrorCode = "recovery_required", "process_cleanup_unproven"
	} else if process.TimedOut {
		result.Status, result.ErrorCode = "timed_out", "process_timed_out"
	} else if process.Canceled {
		result.Status, result.ErrorCode = "cancelled", "process_cancelled"
	} else if !process.Exited || process.ExitCode != 0 {
		result.ErrorCode = "process_failed"
	} else {
		result.Status = "succeeded"
	}
	cleanupUnproven := runnerInvoked && (ErrorCode(runErr) == "process_cleanup_unproven" || runErr == nil && !process.CleanupComplete)
	if cleanupUnproven {
		result.Artifacts = []ArtifactResult{}
		return finish(result, started, r.config.Now())
	}
	validationCtx, validationCancel := context.WithTimeout(context.Background(), 5*time.Second)
	postflightErr := r.config.ScopeValidator.ValidateWorkloadScope(validationCtx, assignment.Scope)
	validationCancel()
	if sourceCloseErr != nil || postflightErr != nil {
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
		if !sameArtifacts(r.config.Adapter.ReviewedArtifacts(), artifacts) {
			result.Status, result.ErrorCode, result.Artifacts = "recovery_required", "artifact_evidence_mismatch", []ArtifactResult{}
		}
	}
	return finish(result, started, r.config.Now())
}

func sameArtifacts(reviewed, collected []ArtifactResult) bool {
	if len(reviewed) != len(collected) {
		return false
	}
	byName := make(map[string]ArtifactResult, len(reviewed))
	for _, artifact := range reviewed {
		if _, duplicate := byName[artifact.Name]; duplicate {
			return false
		}
		byName[artifact.Name] = artifact
	}
	for _, artifact := range collected {
		if expected, ok := byName[artifact.Name]; !ok || expected != artifact {
			return false
		}
		delete(byName, artifact.Name)
	}
	return len(byName) == 0
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
