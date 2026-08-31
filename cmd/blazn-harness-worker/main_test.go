package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/blazncloud/blazn/internal/harnessadapter/hermes"
	"github.com/blazncloud/blazn/internal/harnessworker"
)

func TestParseExecutionAllowsOnlyReviewedHermesArgv(t *testing.T) {
	valid := []string{"blazn-harness-worker", "--", hermes.ReviewedExecutable, "run", "--jsonl"}
	execution, err := parseExecution(valid)
	if err != nil || strings.Join(execution.Argv, "|") != hermes.ReviewedExecutable+"|run|--jsonl" || execution.TimeoutSeconds != hermes.ReviewedRunSeconds || execution.CancelGraceSeconds != hermes.ReviewedCancelSeconds {
		t.Fatalf("execution=%#v err=%v", execution, err)
	}
	for _, invalid := range [][]string{
		{"blazn-harness-worker", "--", "/bin/sh", "-c", "id"},
		append(append([]string{}, valid...), "extra"),
		{"blazn-harness-worker", hermes.ReviewedExecutable, "run", "--jsonl"},
	} {
		if _, err := parseExecution(invalid); err == nil {
			t.Fatalf("unreviewed argv passed: %#v", invalid)
		}
	}
}

func TestHermesWorkerMatchesFrozenProfile(t *testing.T) {
	type fixture struct {
		Version struct {
			Executable struct {
				Path                 string   `json:"path"`
				FixedArgv            []string `json:"fixedArgv"`
				EnvironmentAllowlist []string `json:"environmentAllowlist"`
				Termination          struct {
					GracefulSignal string `json:"gracefulSignal"`
					GraceSeconds   int    `json:"graceSeconds"`
				} `json:"termination"`
			} `json:"executable"`
			Compatibility struct {
				ProxyProtocols []string `json:"proxyProtocols"`
			} `json:"compatibility"`
		} `json:"version"`
		Profile struct {
			Policy struct {
				MaxRunSeconds int `json:"maxRunSeconds"`
			} `json:"policy"`
		} `json:"profile"`
	}
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve worker test source")
	}
	body, err := os.ReadFile(filepath.Join(filepath.Dir(source), "..", "..", "packages", "contracts", "testdata", "harness", "hermes-profile.json"))
	if err != nil {
		t.Fatal(err)
	}
	var frozen fixture
	if err := json.Unmarshal(body, &frozen); err != nil {
		t.Fatal(err)
	}
	if frozen.Version.Executable.Path != hermes.ReviewedExecutable || !reflect.DeepEqual(frozen.Version.Executable.FixedArgv, hermes.ReviewedArgv) ||
		!reflect.DeepEqual(frozen.Version.Executable.EnvironmentAllowlist, hermes.ReviewedEnvironmentAllowlist()) || frozen.Version.Executable.Termination.GracefulSignal != hermes.ReviewedGracefulSignal ||
		frozen.Version.Executable.Termination.GraceSeconds != hermes.ReviewedCancelSeconds || frozen.Profile.Policy.MaxRunSeconds != hermes.ReviewedRunSeconds {
		t.Fatalf("frozen Hermes executable contract diverged: %#v", frozen)
	}
	wantProtocols := []string{string(harnessworker.ProtocolOpenAIResponses), string(harnessworker.ProtocolOpenAIChat)}
	if !reflect.DeepEqual(frozen.Version.Compatibility.ProxyProtocols, wantProtocols) || !hermes.SupportsProtocol(harnessworker.ProtocolOpenAIResponses) || !hermes.SupportsProtocol(harnessworker.ProtocolOpenAIChat) || hermes.SupportsProtocol(harnessworker.ProtocolAnthropicMessages) {
		t.Fatalf("frozen Hermes protocol contract diverged: %q", frozen.Version.Compatibility.ProxyProtocols)
	}
}

func TestRunRejectsLaunchWithoutReflectingInputOrProxyMaterial(t *testing.T) {
	var output bytes.Buffer
	secret := "do-not-reflect-this-listener-secret"
	code := run(context.Background(), []string{"worker", "--", "/bin/sh", "-c", secret}, strings.NewReader(secret+"\n"), &output, "http://user:password@127.0.0.1:8080", time.Now)
	if code != 2 || strings.Contains(output.String(), secret) || strings.Contains(output.String(), "password") {
		t.Fatalf("code=%d output=%s", code, output.String())
	}
	var response harnessworker.ErrorResponse
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &response); err != nil || response.ErrorCode != "launch_config_invalid" {
		t.Fatalf("response=%#v err=%v", response, err)
	}
}

func TestFixedArtifactsAreBoundedCanonicalOutputs(t *testing.T) {
	artifacts := fixedArtifacts()
	if len(artifacts) != 2 || artifacts[0].Role != "patch" || artifacts[1].Role != "summary" || harnessworker.ValidateExecution(harnessworker.Execution{Argv: []string{hermes.ReviewedExecutable, "run", "--jsonl"}, WorkingDirectory: "/workspace", TimeoutSeconds: 60, CancelGraceSeconds: 5}, artifacts) != nil {
		t.Fatalf("artifacts=%#v", artifacts)
	}
}

type compositionScopeValidator struct{}

func (compositionScopeValidator) ValidateWorkloadScope(context.Context, harnessworker.WorkloadScope) error {
	return nil
}

type compositionExecutableVerifier struct{}

func (compositionExecutableVerifier) VerifyExecutable(context.Context, string, string) error {
	return nil
}

type compositionTokenSource struct{ file *os.File }

func (source compositionTokenSource) OpenListenerToken(context.Context, string) (*os.File, error) {
	return source.file, nil
}

type compositionRunner struct{}

func (compositionRunner) Run(_ context.Context, spec harnessworker.ProcessSpec) (harnessworker.ProcessResult, error) {
	records := []string{
		`{"schemaVersion":"blazn.dev/hermes-adapter-record/v1alpha1","sequence":1,"type":"harness.started","payload":{},"extensions":{}}`,
		`{"schemaVersion":"blazn.dev/hermes-adapter-record/v1alpha1","sequence":2,"type":"result.reported","payload":{"status":"succeeded"},"extensions":{}}`,
	}
	for _, record := range records {
		if _, err := io.WriteString(spec.Stdout, record+"\n"); err != nil {
			return harnessworker.ProcessResult{}, err
		}
	}
	return harnessworker.ProcessResult{Exited: true, ExitCode: 0, ProcessGroupGone: true, CleanupComplete: true}, nil
}

type compositionCollector struct{}

func (compositionCollector) Collect(context.Context, []harnessworker.ArtifactSpec, bool) ([]harnessworker.ArtifactResult, []string, error) {
	return []harnessworker.ArtifactResult{}, []string{}, nil
}

func TestHermesCompositionStreamsSafeEvidenceBeforeTerminalResult(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	token := []byte("composition-listener-secret-material")
	fingerprint, err := harnessworker.ListenerTokenFingerprint(token)
	if err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(t.TempDir(), "listener-token")
	if err := os.WriteFile(tokenPath, token, 0o400); err != nil {
		t.Fatal(err)
	}
	tokenFile, err := os.Open(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	defer tokenFile.Close()

	digest := "sha256:" + strings.Repeat("a", 64)
	assignment := harnessworker.Assignment{SchemaVersion: harnessworker.HarnessWorkerSchemaVersion, Type: harnessworker.RequestTypeExecute, Scope: harnessworker.WorkloadScope{
		RunID: "10000000-0000-4000-8000-000000000001", WorkspaceID: "10000000-0000-4000-8000-000000000002", ProjectID: "10000000-0000-4000-8000-000000000003",
		OperationID: "10000000-0000-4000-8000-000000000004", SandboxID: "10000000-0000-4000-8000-000000000005", AgentVersionID: "10000000-0000-4000-8000-000000000006",
		AgentVersionDigest: digest, HarnessProfileID: "10000000-0000-4000-8000-000000000007", HarnessProfileDigest: digest, HarnessVersionID: "10000000-0000-4000-8000-000000000008",
		HarnessVersionDigest: digest, HarnessExecutableDigest: digest, RouteID: "10000000-0000-4000-8000-000000000009", RouteVersion: 1, Protocol: harnessworker.ProtocolOpenAIResponses,
		ExpiresAt: now.Add(time.Hour).Format(time.RFC3339), ListenerCredentialRef: "listener-token://10000000-0000-4000-8000-000000000010", ListenerTokenFingerprint: fingerprint,
	}}

	var output bytes.Buffer
	adapter, err := hermes.New(hermes.Config{ProxyURL: "http://127.0.0.1:8080", Output: &output, ArtifactRoot: t.TempDir(), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	execution := harnessworker.Execution{Argv: []string{hermes.ReviewedExecutable, "run", "--jsonl"}, WorkingDirectory: "/workspace", TimeoutSeconds: hermes.ReviewedRunSeconds, CancelGraceSeconds: hermes.ReviewedCancelSeconds}
	runtime, err := harnessworker.NewRuntime(harnessworker.RunConfig{
		ScopeValidator: compositionScopeValidator{}, ExecutableVerifier: compositionExecutableVerifier{}, TokenSource: compositionTokenSource{file: tokenFile}, Adapter: adapter,
		ProcessRunner: compositionRunner{}, Collector: compositionCollector{}, Execution: execution, Artifacts: fixedArtifacts(),
		AllowedExecutable: hermes.ReviewedExecutable, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	result := runtime.Run(context.Background(), assignment)
	if err := harnessworker.EncodeResponse(&output, result); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"type":"harness.started"`) || !strings.Contains(lines[1], `"type":"result"`) {
		t.Fatalf("unexpected composed JSONL: %s", output.String())
	}
	if result.Status != "recovery_required" || result.ErrorCode != "adapter_output_invalid" || result.ProcessTreeTerminated || strings.Contains(output.String(), string(token)) || strings.Contains(output.String(), "listener-secret") {
		t.Fatalf("unsafe or incorrect composed result: result=%#v output=%s", result, output.String())
	}
}
