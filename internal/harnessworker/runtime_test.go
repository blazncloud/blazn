package harnessworker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func runtimeAssignment(now time.Time) Assignment {
	digest := "sha256:" + strings.Repeat("a", 64)
	return Assignment{SchemaVersion: HarnessWorkerSchemaVersion, Type: RequestTypeExecute, Scope: WorkloadScope{
		RunID: "10000000-0000-4000-8000-000000000001", WorkspaceID: "10000000-0000-4000-8000-000000000002",
		ProjectID: "10000000-0000-4000-8000-000000000003", OperationID: "10000000-0000-4000-8000-000000000004",
		SandboxID: "10000000-0000-4000-8000-000000000005", AgentVersionID: "10000000-0000-4000-8000-000000000006",
		AgentVersionDigest: digest, HarnessProfileID: "10000000-0000-4000-8000-000000000007", HarnessProfileDigest: digest,
		HarnessVersionID: "10000000-0000-4000-8000-000000000008", HarnessVersionDigest: digest, HarnessExecutableDigest: digest,
		RouteID: "10000000-0000-4000-8000-000000000009", RouteVersion: 1, Protocol: ProtocolOpenAIResponses,
		ExpiresAt: now.Add(time.Hour).UTC().Format(time.RFC3339), ListenerCredentialRef: "listener-token://10000000-0000-4000-8000-000000000010",
		ListenerTokenFingerprint: digest,
	}}
}

func TestDecodeAssignmentLineIsSingleClosedSecretFreeRecord(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	assignment := runtimeAssignment(now)
	body, _ := json.Marshal(assignment)
	decoded, err := DecodeAssignmentLine(context.Background(), bytes.NewReader(append(body, '\n')), now)
	if err != nil || decoded != assignment {
		t.Fatalf("decode assignment: %#v %v", decoded, err)
	}
	for name, input := range map[string][]byte{
		"missing newline": body,
		"second record":   append(append(append([]byte{}, body...), '\n'), []byte("{}\n")...),
		"raw token":       []byte(strings.Replace(string(body), `"type":"execute"`, `"type":"execute","listenerToken":"secret"`, 1) + "\n"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeAssignmentLine(context.Background(), bytes.NewReader(input), now); err == nil {
				t.Fatal("unsafe protocol input passed")
			}
		})
	}
}

func TestDecodeAssignmentLineCancellationClosesIncompleteInput(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := DecodeAssignmentLine(ctx, reader, time.Now())
		done <- err
	}()
	if _, err := writer.Write([]byte(`{"schemaVersion":"incomplete"}`)); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if ErrorCode(err) != "request_cancelled" {
			t.Fatalf("error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("incomplete assignment read ignored cancellation")
	}
}

type blockingWriteCloser struct{ closed chan struct{} }

func (writer *blockingWriteCloser) Write([]byte) (int, error) {
	<-writer.closed
	return 0, io.ErrClosedPipe
}
func (writer *blockingWriteCloser) Close() error {
	select {
	case <-writer.closed:
	default:
		close(writer.closed)
	}
	return nil
}

func TestEncodeResponseContextClosesBlockedOutputOnCancellation(t *testing.T) {
	output := &blockingWriteCloser{closed: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := EncodeResponseContext(ctx, output, ErrorResponse{SchemaVersion: HarnessWorkerSchemaVersion, Type: ResponseTypeError, ErrorCode: "test"})
	if ErrorCode(err) != "response_cancelled" || time.Since(started) > time.Second {
		t.Fatalf("error=%v elapsed=%v", err, time.Since(started))
	}
}

func TestProtectedListenerTokenRequiresExactBytesAndNoSymlink(t *testing.T) {
	directory := t.TempDir()
	tokenPath := filepath.Join(directory, "token")
	token := []byte("exact-ephemeral-listener-token")
	if err := os.WriteFile(tokenPath, token, 0o400); err != nil {
		t.Fatal(err)
	}
	fingerprint, _ := ListenerTokenFingerprint(token)
	file, err := (ProtectedListenerTokenFile{Path: tokenPath}).OpenListenerToken(context.Background(), fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	read, _ := io.ReadAll(file)
	_ = file.Close()
	if !bytes.Equal(read, token) {
		t.Fatal("token source changed exact bytes")
	}
	if _, err := (ProtectedListenerTokenFile{Path: tokenPath}).OpenListenerToken(context.Background(), "sha256:"+strings.Repeat("b", 64)); err == nil {
		t.Fatal("wrong fingerprint passed")
	}
	link := filepath.Join(directory, "token-link")
	if err := os.Symlink(tokenPath, link); err != nil {
		t.Fatal(err)
	}
	if _, err := (ProtectedListenerTokenFile{Path: link}).OpenListenerToken(context.Background(), fingerprint); err == nil {
		t.Fatal("symlink token passed")
	}
}

func TestFileArtifactCollectorBoundsAndClassifiesOutputs(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "patch.diff"), []byte("diff --git a/a b/a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	collector := FileArtifactCollector{Root: root}
	specs := defaultArtifactSpecs()
	artifacts, warnings, err := collector.Collect(context.Background(), specs, false)
	if err != nil || len(artifacts) != 1 || artifacts[0].Role != "patch" || len(warnings) != 1 {
		t.Fatalf("partial collection=%#v warnings=%#v err=%v", artifacts, warnings, err)
	}
	if _, _, err := collector.Collect(context.Background(), specs, true); err == nil {
		t.Fatal("successful run accepted missing required summary")
	}
	if err := os.WriteFile(filepath.Join(root, "summary.md"), bytes.Repeat([]byte("x"), MaxArtifactBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := collector.Collect(context.Background(), specs, true); err == nil {
		t.Fatal("oversized artifact passed")
	}
}

type fakeScopeValidator struct {
	calls     int
	failAfter int
}

func (v *fakeScopeValidator) ValidateWorkloadScope(context.Context, WorkloadScope) error {
	v.calls++
	if v.failAfter > 0 && v.calls >= v.failAfter {
		return protocolError("workload_scope_mismatch")
	}
	return nil
}

type fakeTokenSource struct {
	file  *os.File
	calls int
}

type fakeExecutableVerifier struct {
	calls int
	err   error
}

func (verifier *fakeExecutableVerifier) VerifyExecutable(context.Context, string, string) error {
	verifier.calls++
	return verifier.err
}

func (s *fakeTokenSource) OpenListenerToken(context.Context, string) (*os.File, error) {
	s.calls++
	return s.file, nil
}

type fakeAdapter struct {
	execution Execution
	token     *os.File
	child     *os.File
	finalized int
	finalErr  error
	invalid   bool
	reviewed  []ArtifactResult
}

func (a *fakeAdapter) Prepare(_ context.Context, _ Assignment, token *os.File) (ProcessSpec, error) {
	a.token = token
	if a.invalid {
		return ProcessSpec{Execution: a.execution, Environment: []string{"BLAZN_PROXY_URL=http://127.0.0.1:8080", "BLAZN_LISTENER_TOKEN_FD=3"}, Stdin: strings.NewReader(""), Stdout: io.Discard, ExtraFiles: []*os.File{token}}, nil
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return ProcessSpec{}, err
	}
	if _, err := writer.Write([]byte("one-shot-token")); err != nil {
		_ = reader.Close()
		_ = writer.Close()
		return ProcessSpec{}, err
	}
	_ = writer.Close()
	a.child = reader
	return ProcessSpec{Execution: a.execution, Environment: []string{"BLAZN_PROXY_URL=http://127.0.0.1:8080", "BLAZN_LISTENER_TOKEN_FD=3"}, Stdin: strings.NewReader(""), Stdout: io.Discard, ExtraFiles: []*os.File{reader}}, nil
}
func (a *fakeAdapter) Finalize(context.Context, ProcessResult) error {
	a.finalized++
	return a.finalErr
}
func (a *fakeAdapter) ReviewedArtifacts() []ArtifactResult {
	return append([]ArtifactResult(nil), a.reviewed...)
}

type fakeRunner struct {
	calls  int
	result ProcessResult
	file   *os.File
}

func (r *fakeRunner) Run(_ context.Context, spec ProcessSpec) (ProcessResult, error) {
	r.calls++
	r.file = spec.ExtraFiles[0]
	return r.result, nil
}

type fakeCollector struct {
	calls     int
	artifacts []ArtifactResult
}

func (c *fakeCollector) Collect(context.Context, []ArtifactSpec, bool) ([]ArtifactResult, []string, error) {
	c.calls++
	return append([]ArtifactResult(nil), c.artifacts...), []string{}, nil
}

func TestRuntimeFencesScopeRunsSealedAdapterAndRevalidates(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	token, err := os.Open(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	execution := Execution{Argv: []string{"/opt/blazn/hermes", "run", "--jsonl"}, WorkingDirectory: "/workspace", TimeoutSeconds: 60, CancelGraceSeconds: 2}
	validator, source := &fakeScopeValidator{}, &fakeTokenSource{file: token}
	adapter, runner, collector := &fakeAdapter{execution: execution}, &fakeRunner{result: ProcessResult{Exited: true, ExitCode: 0, CleanupComplete: true}}, &fakeCollector{}
	verifier := &fakeExecutableVerifier{}
	runtime, err := NewRuntime(RunConfig{ScopeValidator: validator, ExecutableVerifier: verifier, TokenSource: source, Adapter: adapter, ProcessRunner: runner, Collector: collector, Execution: execution, Artifacts: defaultArtifactSpecs(), AllowedExecutable: "/opt/blazn/hermes", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	result := runtime.Run(context.Background(), runtimeAssignment(now))
	if result.Status != "succeeded" || result.ProcessTreeTerminated || validator.calls != 2 || verifier.calls != 1 || source.calls != 1 || runner.calls != 1 || adapter.finalized != 1 || collector.calls != 1 || adapter.token != token {
		t.Fatalf("result=%#v validation=%d token=%d runner=%d finalize=%d collect=%d", result, validator.calls, source.calls, runner.calls, adapter.finalized, collector.calls)
	}
	if _, err := token.Stat(); err == nil {
		t.Fatal("verified listener FD remained open")
	}
	if _, err := adapter.child.Stat(); err == nil {
		t.Fatal("one-shot child FD remained open in parent")
	}
	if runner.file != adapter.child || runner.file == token {
		t.Fatal("runner did not receive the adapter-derived one-shot descriptor")
	}
}

func TestRuntimeFinalizesRejectedPreparedSpecAndClosesDescriptors(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	tokenPath := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(tokenPath, []byte("token"), 0o600)
	token, _ := os.Open(tokenPath)
	execution := Execution{Argv: []string{"/opt/blazn/hermes", "run", "--jsonl"}, WorkingDirectory: "/workspace", TimeoutSeconds: 60, CancelGraceSeconds: 2}
	adapter, runner := &fakeAdapter{execution: execution, invalid: true}, &fakeRunner{}
	runtime, err := NewRuntime(RunConfig{ScopeValidator: &fakeScopeValidator{}, ExecutableVerifier: &fakeExecutableVerifier{}, TokenSource: &fakeTokenSource{file: token}, Adapter: adapter, ProcessRunner: runner, Collector: &fakeCollector{}, Execution: execution, Artifacts: defaultArtifactSpecs(), AllowedExecutable: "/opt/blazn/hermes", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	result := runtime.Run(context.Background(), runtimeAssignment(now))
	if result.ErrorCode != "adapter_prepare_failed" || adapter.finalized != 1 || runner.calls != 0 {
		t.Fatalf("result=%#v finalized=%d runner=%d", result, adapter.finalized, runner.calls)
	}
	if _, err := token.Stat(); err == nil {
		t.Fatal("rejected regular source descriptor remained open")
	}
}

func TestRuntimeVerifiesFrozenExecutableBeforeOpeningCredential(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	execution := Execution{Argv: []string{"/opt/blazn/hermes", "run", "--jsonl"}, WorkingDirectory: "/workspace", TimeoutSeconds: 60, CancelGraceSeconds: 20}
	verifier := &fakeExecutableVerifier{err: protocolError("harness_executable_untrusted")}
	source := &fakeTokenSource{}
	runtime, err := NewRuntime(RunConfig{ScopeValidator: &fakeScopeValidator{}, ExecutableVerifier: verifier, TokenSource: source, Adapter: &fakeAdapter{execution: execution}, ProcessRunner: &fakeRunner{}, Collector: &fakeCollector{}, Execution: execution, Artifacts: defaultArtifactSpecs(), AllowedExecutable: "/opt/blazn/hermes", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	result := runtime.Run(context.Background(), runtimeAssignment(now))
	if result.ErrorCode != "harness_executable_untrusted" || verifier.calls != 1 || source.calls != 0 {
		t.Fatalf("result=%#v verifier=%d token=%d", result, verifier.calls, source.calls)
	}
}

type cleanupUnprovenRunner struct{}

func (cleanupUnprovenRunner) Run(context.Context, ProcessSpec) (ProcessResult, error) {
	return ProcessResult{Exited: true, ExitCode: 0}, protocolError("process_cleanup_unproven")
}

func TestRuntimeFailsClosedWhenNormalProcessCleanupIsUnproven(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	tokenPath := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(tokenPath, []byte("token"), 0o600)
	token, _ := os.Open(tokenPath)
	execution := Execution{Argv: []string{"/opt/blazn/hermes", "run", "--jsonl"}, WorkingDirectory: "/workspace", TimeoutSeconds: 60, CancelGraceSeconds: 2}
	collector := &fakeCollector{artifacts: []ArtifactResult{{Name: "patch", Role: "patch", Kind: "agent.patch", MediaType: "text/x-diff", Size: 9, ContentDigest: "sha256:" + strings.Repeat("a", 64)}}}
	runtime, err := NewRuntime(RunConfig{ScopeValidator: &fakeScopeValidator{}, ExecutableVerifier: &fakeExecutableVerifier{}, TokenSource: &fakeTokenSource{file: token}, Adapter: &fakeAdapter{execution: execution}, ProcessRunner: cleanupUnprovenRunner{}, Collector: collector, Execution: execution, Artifacts: defaultArtifactSpecs(), AllowedExecutable: "/opt/blazn/hermes", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	result := runtime.Run(context.Background(), runtimeAssignment(now))
	if result.Status != "recovery_required" || result.ErrorCode != "process_cleanup_unproven" || result.ProcessTreeTerminated || collector.calls != 0 || len(result.Artifacts) != 0 {
		t.Fatalf("cleanup result=%#v", result)
	}
}

func TestRuntimePostflightScopeFailureIsRecoveryRequired(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	tokenPath := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(tokenPath, []byte("token"), 0o600)
	token, _ := os.Open(tokenPath)
	execution := Execution{Argv: []string{"/opt/blazn/hermes", "run", "--jsonl"}, WorkingDirectory: "/workspace", TimeoutSeconds: 60, CancelGraceSeconds: 2}
	validator, collector := &fakeScopeValidator{failAfter: 2}, &fakeCollector{}
	runtime, err := NewRuntime(RunConfig{ScopeValidator: validator, ExecutableVerifier: &fakeExecutableVerifier{}, TokenSource: &fakeTokenSource{file: token}, Adapter: &fakeAdapter{execution: execution}, ProcessRunner: &fakeRunner{result: ProcessResult{Exited: true, ExitCode: 0, CleanupComplete: true}}, Collector: collector, Execution: execution, Artifacts: defaultArtifactSpecs(), AllowedExecutable: "/opt/blazn/hermes", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	result := runtime.Run(context.Background(), runtimeAssignment(now))
	if result.Status != "recovery_required" || result.ErrorCode != "postflight_scope_failed" || collector.calls != 0 {
		t.Fatalf("postflight result=%#v collector=%d", result, collector.calls)
	}
}

func TestRuntimeClassifiesAdapterOutputFailureSeparately(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	tokenPath := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(tokenPath, []byte("token"), 0o600)
	token, _ := os.Open(tokenPath)
	execution := Execution{Argv: []string{"/opt/blazn/hermes", "run", "--jsonl"}, WorkingDirectory: "/workspace", TimeoutSeconds: 60, CancelGraceSeconds: 2}
	runtime, err := NewRuntime(RunConfig{ScopeValidator: &fakeScopeValidator{}, ExecutableVerifier: &fakeExecutableVerifier{}, TokenSource: &fakeTokenSource{file: token}, Adapter: &fakeAdapter{execution: execution, finalErr: errors.New("untrusted output")}, ProcessRunner: &fakeRunner{result: ProcessResult{Exited: true, ExitCode: 0, TreeKilled: true, CleanupComplete: true}}, Collector: &fakeCollector{}, Execution: execution, Artifacts: defaultArtifactSpecs(), AllowedExecutable: "/opt/blazn/hermes", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	result := runtime.Run(context.Background(), runtimeAssignment(now))
	if result.Status != "recovery_required" || result.ErrorCode != "adapter_output_invalid" {
		t.Fatalf("adapter output result=%#v", result)
	}
}

func TestRuntimeRequiresCollectedArtifactsToMatchReviewedEvidence(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	tokenPath := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(tokenPath, []byte("token"), 0o600)
	token, _ := os.Open(tokenPath)
	execution := Execution{Argv: []string{"/opt/blazn/hermes", "run", "--jsonl"}, WorkingDirectory: "/workspace", TimeoutSeconds: 60, CancelGraceSeconds: 2}
	collected := ArtifactResult{Name: "patch", Role: "patch", Kind: "agent.patch", MediaType: "text/x-diff", Size: 9, ContentDigest: "sha256:" + strings.Repeat("a", 64)}
	adapter := &fakeAdapter{execution: execution, reviewed: []ArtifactResult{{Name: "patch", Role: "patch", Kind: "agent.patch", MediaType: "text/x-diff", Size: 10, ContentDigest: collected.ContentDigest}}}
	runtime, err := NewRuntime(RunConfig{ScopeValidator: &fakeScopeValidator{}, ExecutableVerifier: &fakeExecutableVerifier{}, TokenSource: &fakeTokenSource{file: token}, Adapter: adapter, ProcessRunner: &fakeRunner{result: ProcessResult{Exited: true, ExitCode: 0, CleanupComplete: true}}, Collector: &fakeCollector{artifacts: []ArtifactResult{collected}}, Execution: execution, Artifacts: defaultArtifactSpecs(), AllowedExecutable: "/opt/blazn/hermes", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	result := runtime.Run(context.Background(), runtimeAssignment(now))
	if result.Status != "recovery_required" || result.ErrorCode != "artifact_evidence_mismatch" || len(result.Artifacts) != 0 {
		t.Fatalf("artifact mismatch result=%#v", result)
	}
}

func TestRuntimeSuppressesUnreviewedArtifactsOnProcessFailure(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	tokenPath := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(tokenPath, []byte("token"), 0o600)
	token, _ := os.Open(tokenPath)
	execution := Execution{Argv: []string{"/opt/blazn/hermes", "run", "--jsonl"}, WorkingDirectory: "/workspace", TimeoutSeconds: 60, CancelGraceSeconds: 20}
	collected := ArtifactResult{Name: "patch", Role: "patch", Kind: "agent.patch", MediaType: "text/x-diff", Size: 9, ContentDigest: "sha256:" + strings.Repeat("a", 64)}
	adapter := &fakeAdapter{execution: execution, reviewed: []ArtifactResult{}}
	runtime, err := NewRuntime(RunConfig{ScopeValidator: &fakeScopeValidator{}, ExecutableVerifier: &fakeExecutableVerifier{}, TokenSource: &fakeTokenSource{file: token}, Adapter: adapter, ProcessRunner: &fakeRunner{result: ProcessResult{Exited: true, ExitCode: 1, CleanupComplete: true}}, Collector: &fakeCollector{artifacts: []ArtifactResult{collected}}, Execution: execution, Artifacts: defaultArtifactSpecs(), AllowedExecutable: "/opt/blazn/hermes", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	result := runtime.Run(context.Background(), runtimeAssignment(now))
	if result.Status != "recovery_required" || result.ErrorCode != "artifact_evidence_mismatch" || len(result.Artifacts) != 0 {
		t.Fatalf("failure artifact mismatch result=%#v", result)
	}
}

func defaultArtifactSpecs() []ArtifactSpec {
	return []ArtifactSpec{
		{Name: "patch", Path: "/workspace/artifacts/patch.diff", Role: "patch", Kind: "agent.patch", MediaType: "text/x-diff", Required: true, MaxBytes: MaxArtifactBytes},
		{Name: "summary", Path: "/workspace/artifacts/summary.md", Role: "summary", Kind: "agent.summary", MediaType: "text/markdown", Required: true, MaxBytes: MaxArtifactBytes},
	}
}
