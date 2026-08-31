package hermes

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blazncloud/blazn/internal/harnessworker"
	"github.com/blazncloud/blazn/internal/proxy/router"
	"github.com/blazncloud/blazn/internal/proxycontract"
)

const (
	testToken   = "hermes-listener-secret-material"
	testRouteID = "33333333-3333-4333-8333-333333333333"
)

type credentials map[string]string

func (c credentials) DestinationCredential(_ context.Context, reference string) (string, error) {
	value, ok := c[reference]
	if !ok {
		return "", errors.New("missing credential")
	}
	return value, nil
}

type fakeHermes struct {
	t                 *testing.T
	artifactRoot      string
	wantScope         harnessworker.WorkloadScope
	observedToken     string
	observedInput     string
	observedExecution harnessworker.Execution
}

func (f *fakeHermes) Run(ctx context.Context, spec harnessworker.ProcessSpec) (harnessworker.ProcessResult, error) {
	f.t.Helper()
	f.observedExecution = spec.Execution
	if len(spec.Environment) != 2 || spec.Environment[0] != "BLAZN_LISTENER_TOKEN_FD=3" || !strings.HasPrefix(spec.Environment[1], "BLAZN_PROXY_URL=http://127.0.0.1:") {
		f.t.Fatalf("Hermes environment was not closed: %q", spec.Environment)
	}
	proxyURL := strings.TrimPrefix(spec.Environment[1], "BLAZN_PROXY_URL=")
	encoded, err := io.ReadAll(spec.Stdin)
	if err != nil {
		f.t.Fatal(err)
	}
	f.observedInput = string(encoded)
	if strings.Contains(f.observedInput, testToken) {
		f.t.Fatal("listener token appeared in Hermes JSONL input")
	}
	if len(spec.ExtraFiles) != 1 || spec.ExtraFiles[0] == nil {
		f.t.Fatal("verified token FD was not inherited exactly once")
	}
	token, err := io.ReadAll(spec.ExtraFiles[0])
	if err != nil {
		f.t.Fatal(err)
	}
	f.observedToken = string(token)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, proxyURL+"/v1/responses", strings.NewReader(`{"model":"company-assistant","input":"bounded task","max_output_tokens":16}`))
	if err != nil {
		f.t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+f.observedToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return harnessworker.ProcessResult{}, err
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("scoped answer")) {
		f.t.Fatalf("proxy response=%d %s", response.StatusCode, body)
	}
	patch := []byte("diff --git a/a b/a\n")
	if err := os.WriteFile(filepath.Join(f.artifactRoot, "patch.diff"), patch, 0o600); err != nil {
		f.t.Fatal(err)
	}
	summary := []byte("Completed the bounded change.\n")
	if err := os.WriteFile(filepath.Join(f.artifactRoot, "summary.md"), summary, 0o600); err != nil {
		f.t.Fatal(err)
	}
	patchDigest, summaryDigest := sha256.Sum256(patch), sha256.Sum256(summary)
	records := []map[string]any{
		recordValue(1, "harness.started", map[string]any{}, map[string]map[string]any{"hermes.session": {"sourceSessionId": "opaque-session"}}),
		recordValue(2, "model.requested", routePayload(f.wantScope, map[string]any{"requestId": "74000000-0000-4000-8000-000000000001"}), map[string]map[string]any{}),
		recordValue(3, "model.usage", routePayload(f.wantScope, map[string]any{"requestId": "74000000-0000-4000-8000-000000000001", "inputTokens": 4, "outputTokens": 2}), map[string]map[string]any{}),
		recordValue(4, "patch.created", map[string]any{"name": "patch", "role": "patch", "kind": "agent.patch", "mediaType": "text/x-diff", "path": "/workspace/artifacts/patch.diff", "contentDigest": "sha256:" + hex.EncodeToString(patchDigest[:])}, map[string]map[string]any{}),
		recordValue(5, "artifact.created", map[string]any{"name": "summary", "role": "summary", "kind": "agent.summary", "mediaType": "text/markdown", "path": "/workspace/artifacts/summary.md", "contentDigest": "sha256:" + hex.EncodeToString(summaryDigest[:])}, map[string]map[string]any{}),
		recordValue(6, "result.reported", map[string]any{"status": "succeeded"}, map[string]map[string]any{}),
	}
	for _, item := range records {
		encoded, _ := json.Marshal(item)
		if _, err := spec.Stdout.Write(append(encoded, '\n')); err != nil {
			return harnessworker.ProcessResult{Canceled: true, TreeKilled: true}, err
		}
	}
	return harnessworker.ProcessResult{Exited: true, ExitCode: 0}, nil
}

type shortWriter struct {
	max int
	buf bytes.Buffer
}

func (w *shortWriter) Write(value []byte) (int, error) {
	if len(value) > w.max {
		value = value[:w.max]
	}
	return w.buf.Write(value)
}

func TestHermesNormalizedOutputCompletesShortWrites(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	output := &shortWriter{max: 3}
	adapter, err := New(Config{ProxyURL: "http://127.0.0.1:19090", Output: output, ArtifactRoot: t.TempDir(), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	scope := validScope(t, now)
	spec, err := adapter.Prepare(context.Background(), harnessworker.Assignment{SchemaVersion: harnessworker.HarnessWorkerSchemaVersion, Type: harnessworker.RequestTypeExecute, Scope: scope}, tokenFile(t, testToken))
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(recordValue(1, "result.reported", map[string]any{"status": "failed"}, map[string]map[string]any{}))
	if _, err := spec.Stdout.Write(append(want, '\n')); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Finalize(context.Background(), harnessworker.ProcessResult{Exited: true, ExitCode: 1}); err != nil {
		t.Fatal(err)
	}
	var gotRecord map[string]any
	if !bytes.HasSuffix(output.buf.Bytes(), []byte("\n")) || bytes.Count(output.buf.Bytes(), []byte("\n")) != 1 || json.Unmarshal(bytes.TrimSpace(output.buf.Bytes()), &gotRecord) != nil || gotRecord["type"] != "result.reported" {
		t.Fatalf("short-write output is not one complete JSONL record: %q", output.buf.Bytes())
	}
}

type blockedWriter struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *blockedWriter) Write(value []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return len(value), nil
}

func TestHermesBlockedNormalizedOutputUnblocksOnCancellation(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	ctx, cancel := context.WithCancel(context.Background())
	output := &blockedWriter{entered: make(chan struct{}), release: make(chan struct{})}
	adapter, err := New(Config{ProxyURL: "http://127.0.0.1:19090", Output: output, ArtifactRoot: t.TempDir(), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	scope := validScope(t, now)
	spec, err := adapter.Prepare(ctx, harnessworker.Assignment{SchemaVersion: harnessworker.HarnessWorkerSchemaVersion, Type: harnessworker.RequestTypeExecute, Scope: scope}, tokenFile(t, testToken))
	if err != nil {
		t.Fatal(err)
	}
	writeDone := make(chan error, 1)
	go func() {
		for sequence := 1; sequence <= 20; sequence++ {
			line, _ := json.Marshal(recordValue(sequence, "message.assistant", map[string]any{"content": "bounded"}, map[string]map[string]any{}))
			if _, err := spec.Stdout.Write(append(line, '\n')); err != nil {
				writeDone <- err
				return
			}
		}
		writeDone <- nil
	}()
	<-output.entered
	cancel()
	select {
	case err := <-writeDone:
		if err == nil || !strings.Contains(err.Error(), "cancelled") {
			t.Fatalf("blocked output error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked normalized output held the Hermes stdout writer after cancellation")
	}
	close(output.release)
	finalizeCtx, finalizeCancel := context.WithTimeout(context.Background(), time.Second)
	defer finalizeCancel()
	if err := adapter.Finalize(finalizeCtx, harnessworker.ProcessResult{Canceled: true, TreeKilled: true}); err == nil {
		t.Fatal("cancelled blocked output finalized successfully")
	}
}

func TestHermesUnresolvedOutputRetainsExclusiveOwnershipAfterFinalize(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	ctx, cancel := context.WithCancel(context.Background())
	output := &blockedWriter{entered: make(chan struct{}), release: make(chan struct{})}
	adapter, err := New(Config{ProxyURL: "http://127.0.0.1:19090", Output: output, ArtifactRoot: t.TempDir(), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	scope := validScope(t, now)
	spec, err := adapter.Prepare(ctx, harnessworker.Assignment{SchemaVersion: harnessworker.HarnessWorkerSchemaVersion, Type: harnessworker.RequestTypeExecute, Scope: scope}, tokenFile(t, testToken))
	if err != nil {
		t.Fatal(err)
	}
	line, _ := json.Marshal(recordValue(1, "result.reported", map[string]any{"status": "failed"}, map[string]map[string]any{}))
	if _, err := spec.Stdout.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
	<-output.entered
	cancel()
	finalizeCtx, finalizeCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer finalizeCancel()
	if err := adapter.Finalize(finalizeCtx, harnessworker.ProcessResult{Canceled: true, TreeKilled: true}); err == nil || !strings.Contains(err.Error(), "flush") {
		t.Fatalf("unresolved delivery finalize error=%v", err)
	}
	if adapter.OutputReusable() {
		t.Fatal("blocked delivery released output ownership")
	}
	// The command observes false and suppresses its terminal response. Resuming
	// the writer can therefore complete only the already-owned adapter record.
	close(output.release)
	deadline := time.Now().Add(time.Second)
	for !adapter.OutputReusable() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !adapter.OutputReusable() {
		t.Fatal("resumed delivery did not finish")
	}
}

func TestHermesRunsThroughScopedProxyAndPreservesSafeEvidence(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	scope := validScope(t, now)
	policy := scopedPolicy(t)
	policy.Version = int(scope.RouteVersion)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/chat/completions" || request.Header.Get("Authorization") != "Bearer destination-secret" {
			t.Fatalf("unexpected upstream request path=%s auth=%q", request.URL.Path, request.Header.Get("Authorization"))
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"choices":[{"message":{"content":"scoped answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`)
	}))
	defer upstream.Close()
	port, _ := strconv.Atoi(strings.TrimPrefix(upstream.URL, "http://127.0.0.1:"))
	policy.Routes[0].Endpoint.Port = port
	policy.Aliases["company-assistant"] = proxycontract.Alias{RouteIDs: []string{testRouteID}, DataClass: proxycontract.DataCompany, AllowedDestinationBoundaries: []proxycontract.DataBoundary{proxycontract.BoundaryLocal}}
	events := []proxycontract.Event{}
	handler, err := router.NewHandler(router.Config{Policy: policy, ActivationID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", ListenerToken: testToken, Credentials: credentials{"node-route://qwen38": "destination-secret"}, Resolver: router.EndpointResolver{}, Events: router.EventSinkFunc(func(event proxycontract.Event) { events = append(events, event) }), Now: func() time.Time { return now }, WorkloadScope: &scope})
	if err != nil {
		t.Fatal(err)
	}
	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()
	artifactRoot := t.TempDir()
	var normalized bytes.Buffer
	adapter, err := New(Config{ProxyURL: proxyServer.URL, Output: &normalized, ArtifactRoot: artifactRoot, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	tokenFile := tokenFile(t, testToken)
	assignment := harnessworker.Assignment{SchemaVersion: harnessworker.HarnessWorkerSchemaVersion, Type: harnessworker.RequestTypeExecute, Scope: scope}
	spec, err := adapter.Prepare(context.Background(), assignment, tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeHermes{t: t, artifactRoot: artifactRoot, wantScope: scope}
	process, err := fake.Run(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.Finalize(context.Background(), process); err != nil {
		t.Fatal(err)
	}
	if fake.observedToken != testToken || !strings.Contains(fake.observedInput, `"routeId":"`+testRouteID+`"`) || !strings.Contains(fake.observedInput, `"routeVersion":7`) || !strings.Contains(fake.observedInput, `"protocol":"openai-responses"`) {
		t.Fatalf("scope/token handoff was not exact: input=%s token=%q", fake.observedInput, fake.observedToken)
	}
	wantArgv := []string{ReviewedExecutable, "run", "--jsonl"}
	if strings.Join(fake.observedExecution.Argv, "\x00") != strings.Join(wantArgv, "\x00") {
		t.Fatalf("argv=%q", fake.observedExecution.Argv)
	}
	evidence := normalized.String()
	if strings.Contains(evidence, testToken) || strings.Contains(evidence, "destination-secret") || strings.Contains(evidence, "Authorization") {
		t.Fatalf("normalized evidence leaked credentials: %s", evidence)
	}
	artifacts := adapter.Artifacts()
	if len(artifacts) != 2 || artifacts[0].Role != "patch" || artifacts[1].Role != "summary" || artifacts[0].Size == 0 || artifacts[1].Size == 0 {
		t.Fatalf("artifacts=%#v", artifacts)
	}
	for _, event := range events {
		if event.RouteID != testRouteID || event.Protocol != proxycontract.ProtocolOpenAIResponses {
			t.Fatalf("proxy event escaped scope: %#v", event)
		}
	}
	if len(events) == 0 {
		t.Fatal("scoped proxy emitted no authoritative events")
	}
}

func TestHermesFailsClosedOnRouteSecretAndCancellationContradictions(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	for name, test := range map[string]struct {
		line    map[string]any
		process harnessworker.ProcessResult
		want    string
	}{
		"route mismatch": {recordValue(1, "model.requested", map[string]any{"requestId": "x", "routeId": "44444444-4444-4444-8444-444444444444", "routeVersion": 7, "protocol": "openai-responses"}, map[string]map[string]any{}), harnessworker.ProcessResult{Canceled: true, TreeKilled: true}, "route binding"},
		"secret value":   {recordValue(1, "message.assistant", map[string]any{"content": "Bearer very-secret-value"}, map[string]map[string]any{}), harnessworker.ProcessResult{Canceled: true, TreeKilled: true}, "credential"},
	} {
		t.Run(name, func(t *testing.T) {
			adapter, spec := preparedAdapter(t, now)
			encoded, _ := json.Marshal(test.line)
			if _, err := spec.Stdout.Write(append(encoded, '\n')); err == nil {
				t.Fatal("unsafe record was accepted")
			}
			if err := adapter.Finalize(context.Background(), test.process); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("finalize error=%v want %q", err, test.want)
			}
			select {
			case err := <-spec.Abort:
				if err == nil {
					t.Fatal("abort did not carry parser error")
				}
			default:
				t.Fatal("parser failure did not abort the process")
			}
		})
	}
	adapter, spec, root := preparedAdapterAt(t, now)
	emitRequiredArtifacts(t, spec, root, 1)
	line, _ := json.Marshal(recordValue(3, "result.reported", map[string]any{"status": "succeeded"}, map[string]map[string]any{}))
	if _, err := spec.Stdout.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Finalize(context.Background(), harnessworker.ProcessResult{Canceled: true, TreeKilled: true}); err == nil || !strings.Contains(err.Error(), "success after cancellation") {
		t.Fatalf("cancellation contradiction error=%v", err)
	}
}

func TestScopedProxyRejectsWrongProtocolAndUnavailableRoute(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	scope := validScope(t, now)
	policy := scopedPolicy(t)
	policy.Version = int(scope.RouteVersion)
	handler, err := router.NewHandler(router.Config{Policy: policy, ActivationID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", ListenerToken: testToken, Credentials: credentials{"node-route://qwen38": "destination-secret", "workspace-vault://poc/model-providers/openai": "cloud"}, Resolver: router.EndpointResolver{}, Now: func() time.Time { return now }, WorkloadScope: &scope})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"company-assistant","messages":[{"role":"user","content":"x"}]}`))
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("Content-Type", "application/json")
	record := httptest.NewRecorder()
	handler.ServeHTTP(record, request)
	if record.Code != http.StatusForbidden {
		t.Fatalf("wrong protocol status=%d body=%s", record.Code, record.Body.String())
	}
	scope.RouteID = "99999999-9999-4999-8999-999999999999"
	handler, err = router.NewHandler(router.Config{Policy: policy, ActivationID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", ListenerToken: testToken, Credentials: credentials{"node-route://qwen38": "destination-secret", "workspace-vault://poc/model-providers/openai": "cloud"}, Resolver: router.EndpointResolver{}, Now: func() time.Time { return now }, WorkloadScope: &scope})
	if err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"company-assistant","input":"x"}`))
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("Content-Type", "application/json")
	record = httptest.NewRecorder()
	handler.ServeHTTP(record, request)
	if record.Code != http.StatusForbidden || !strings.Contains(record.Body.String(), "workload route is unavailable") {
		t.Fatalf("wrong route status=%d body=%s", record.Code, record.Body.String())
	}
	scope = validScope(t, now)
	policy.Version = int(scope.RouteVersion) + 1
	if _, err := router.NewHandler(router.Config{Policy: policy, ActivationID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", ListenerToken: testToken, Credentials: credentials{}, Resolver: router.EndpointResolver{}, Now: func() time.Time { return now }, WorkloadScope: &scope}); err == nil || !strings.Contains(err.Error(), "route version") {
		t.Fatalf("route-version mismatch error=%v", err)
	}
}

func TestHermesRefusesSymlinkArtifact(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.diff")
	contents := []byte("diff --git a/a b/a\n")
	if err := os.WriteFile(outside, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "patch.diff")); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	adapter, err := New(Config{ProxyURL: "http://127.0.0.1:19090", Output: &output, ArtifactRoot: root, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	scope := validScope(t, now)
	spec, err := adapter.Prepare(context.Background(), harnessworker.Assignment{SchemaVersion: harnessworker.HarnessWorkerSchemaVersion, Type: harnessworker.RequestTypeExecute, Scope: scope}, tokenFile(t, testToken))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	line, _ := json.Marshal(recordValue(1, "patch.created", map[string]any{"name": "patch", "role": "patch", "kind": "agent.patch", "mediaType": "text/x-diff", "path": "/workspace/artifacts/patch.diff", "contentDigest": "sha256:" + hex.EncodeToString(digest[:])}, map[string]map[string]any{}))
	if _, err := spec.Stdout.Write(append(line, '\n')); err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("symlink artifact error=%v", err)
	}
	select {
	case <-spec.Abort:
	default:
		t.Fatal("unsafe artifact did not abort Hermes")
	}
}

func TestHermesRefusesArtifactReplacedAfterReview(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	reviewed := []byte("diff --git a/a b/a\n")
	replaced := []byte("different bytes\n")
	name := filepath.Join(root, "patch.diff")
	if err := os.WriteFile(name, reviewed, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(reviewed)
	if err := os.Rename(filepath.Join(root, "patch.diff"), filepath.Join(root, "reviewed-away.diff")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, replaced, 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	adapter, err := New(Config{ProxyURL: "http://127.0.0.1:19090", Output: &output, ArtifactRoot: root, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	scope := validScope(t, now)
	spec, err := adapter.Prepare(context.Background(), harnessworker.Assignment{SchemaVersion: harnessworker.HarnessWorkerSchemaVersion, Type: harnessworker.RequestTypeExecute, Scope: scope}, tokenFile(t, testToken))
	if err != nil {
		t.Fatal(err)
	}
	line, _ := json.Marshal(recordValue(1, "patch.created", map[string]any{"name": "patch", "role": "patch", "kind": "agent.patch", "mediaType": "text/x-diff", "path": "/workspace/artifacts/patch.diff", "contentDigest": "sha256:" + hex.EncodeToString(digest[:])}, map[string]map[string]any{}))
	if _, err := spec.Stdout.Write(append(line, '\n')); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("replaced artifact error=%v", err)
	}
	select {
	case <-spec.Abort:
	default:
		t.Fatal("replaced artifact did not abort Hermes")
	}
}

func TestHermesSuccessfulResultRequiresExactArtifactEvents(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	t.Run("missing", func(t *testing.T) {
		adapter, spec := preparedAdapter(t, now)
		line, _ := json.Marshal(recordValue(1, "result.reported", map[string]any{"status": "succeeded"}, map[string]map[string]any{}))
		if _, err := spec.Stdout.Write(append(line, '\n')); err == nil || !strings.Contains(err.Error(), "missing required") {
			t.Fatalf("missing artifacts error=%v", err)
		}
		if err := adapter.Finalize(context.Background(), harnessworker.ProcessResult{Canceled: true, TreeKilled: true}); err == nil {
			t.Fatal("missing artifact events finalized successfully")
		}
	})
	t.Run("duplicate", func(t *testing.T) {
		_, spec, root := preparedAdapterAt(t, now)
		patch := []byte("diff --git a/a b/a\n")
		if err := os.WriteFile(filepath.Join(root, "patch.diff"), patch, 0o600); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(patch)
		payload := map[string]any{"name": "patch", "role": "patch", "kind": "agent.patch", "mediaType": "text/x-diff", "path": "/workspace/artifacts/patch.diff", "contentDigest": "sha256:" + hex.EncodeToString(digest[:])}
		for sequence := 1; sequence <= 2; sequence++ {
			line, _ := json.Marshal(recordValue(sequence, "patch.created", payload, map[string]map[string]any{}))
			_, err := spec.Stdout.Write(append(line, '\n'))
			if sequence == 2 && (err == nil || !strings.Contains(err.Error(), "duplicate")) {
				t.Fatalf("duplicate artifact error=%v", err)
			}
		}
	})
	t.Run("event metadata mismatch", func(t *testing.T) {
		_, spec, root := preparedAdapterAt(t, now)
		summary := []byte("summary\n")
		if err := os.WriteFile(filepath.Join(root, "summary.md"), summary, 0o600); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(summary)
		line, _ := json.Marshal(recordValue(1, "patch.created", map[string]any{"name": "summary", "role": "summary", "kind": "agent.summary", "mediaType": "text/markdown", "path": "/workspace/artifacts/summary.md", "contentDigest": "sha256:" + hex.EncodeToString(digest[:])}, map[string]map[string]any{}))
		if _, err := spec.Stdout.Write(append(line, '\n')); err == nil || !strings.Contains(err.Error(), "output contract") {
			t.Fatalf("mismatched artifact error=%v", err)
		}
	})
}

func TestHermesFinalizeRevalidatesArtifactBytesAndTokenAbsence(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	for name, test := range map[string]struct {
		replacement []byte
		want        string
	}{
		"replacement":    {[]byte("different final summary\n"), "changed"},
		"listener token": {[]byte(testToken), "credential"},
	} {
		t.Run(name, func(t *testing.T) {
			adapter, spec, root := preparedAdapterAt(t, now)
			emitRequiredArtifacts(t, spec, root, 1)
			line, _ := json.Marshal(recordValue(3, "result.reported", map[string]any{"status": "succeeded"}, map[string]map[string]any{}))
			if _, err := spec.Stdout.Write(append(line, '\n')); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "summary.md"), test.replacement, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := adapter.Finalize(context.Background(), harnessworker.ProcessResult{Exited: true, ExitCode: 0}); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("post-event %s error=%v", name, err)
			}
		})
	}
}

func TestHermesFailedRunScansUnrecordedFixedArtifactsForToken(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	adapter, spec, root := preparedAdapterAt(t, now)
	if err := os.WriteFile(filepath.Join(root, "patch.diff"), []byte("failure context "+testToken), 0o600); err != nil {
		t.Fatal(err)
	}
	line, _ := json.Marshal(recordValue(1, "result.reported", map[string]any{"status": "failed"}, map[string]map[string]any{}))
	if _, err := spec.Stdout.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Finalize(context.Background(), harnessworker.ProcessResult{Exited: true, ExitCode: 1}); err == nil || !strings.Contains(err.Error(), "credential") {
		t.Fatalf("failed-run listener artifact error=%v", err)
	}
	if reviewed := adapter.ReviewedArtifacts(); len(reviewed) != 0 {
		t.Fatalf("unsafe failed artifacts were reviewed: %#v", reviewed)
	}
}

func preparedAdapter(t *testing.T, now time.Time) (*Adapter, harnessworker.ProcessSpec) {
	adapter, spec, _ := preparedAdapterAt(t, now)
	return adapter, spec
}

func preparedAdapterAt(t *testing.T, now time.Time) (*Adapter, harnessworker.ProcessSpec, string) {
	t.Helper()
	var output bytes.Buffer
	root := t.TempDir()
	adapter, err := New(Config{ProxyURL: "http://127.0.0.1:19090", Output: &output, ArtifactRoot: root, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	scope := validScope(t, now)
	spec, err := adapter.Prepare(context.Background(), harnessworker.Assignment{SchemaVersion: harnessworker.HarnessWorkerSchemaVersion, Type: harnessworker.RequestTypeExecute, Scope: scope}, tokenFile(t, testToken))
	if err != nil {
		t.Fatal(err)
	}
	return adapter, spec, root
}

func emitRequiredArtifacts(t *testing.T, spec harnessworker.ProcessSpec, root string, firstSequence int) {
	t.Helper()
	patch, summary := []byte("diff --git a/a b/a\n"), []byte("summary\n")
	if err := os.WriteFile(filepath.Join(root, "patch.diff"), patch, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "summary.md"), summary, 0o600); err != nil {
		t.Fatal(err)
	}
	patchDigest, summaryDigest := sha256.Sum256(patch), sha256.Sum256(summary)
	records := []map[string]any{
		recordValue(firstSequence, "patch.created", map[string]any{"name": "patch", "role": "patch", "kind": "agent.patch", "mediaType": "text/x-diff", "path": "/workspace/artifacts/patch.diff", "contentDigest": "sha256:" + hex.EncodeToString(patchDigest[:])}, map[string]map[string]any{}),
		recordValue(firstSequence+1, "artifact.created", map[string]any{"name": "summary", "role": "summary", "kind": "agent.summary", "mediaType": "text/markdown", "path": "/workspace/artifacts/summary.md", "contentDigest": "sha256:" + hex.EncodeToString(summaryDigest[:])}, map[string]map[string]any{}),
	}
	for _, item := range records {
		encoded, _ := json.Marshal(item)
		if _, err := spec.Stdout.Write(append(encoded, '\n')); err != nil {
			t.Fatal(err)
		}
	}
}

func validScope(t *testing.T, now time.Time) harnessworker.WorkloadScope {
	t.Helper()
	fingerprint, err := harnessworker.ListenerTokenFingerprint([]byte(testToken))
	if err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	return harnessworker.WorkloadScope{RunID: "30000000-0000-4000-8000-000000000001", WorkspaceID: "40000000-0000-4000-8000-000000000001", ProjectID: "50000000-0000-4000-8000-000000000001", OperationID: "60000000-0000-4000-8000-000000000001", SandboxID: "61000000-0000-4000-8000-000000000001", AgentVersionID: "62000000-0000-4000-8000-000000000001", AgentVersionDigest: digest, HarnessProfileID: "63000000-0000-4000-8000-000000000001", HarnessProfileDigest: digest, HarnessVersionID: "64000000-0000-4000-8000-000000000001", HarnessVersionDigest: digest, HarnessExecutableDigest: digest, RouteID: testRouteID, RouteVersion: 7, Protocol: harnessworker.ProtocolOpenAIResponses, ExpiresAt: now.Add(time.Hour).Format(time.RFC3339), ListenerCredentialRef: "listener-token://65000000-0000-4000-8000-000000000001", ListenerTokenFingerprint: fingerprint}
}

func tokenFile(t *testing.T, value string) *os.File {
	t.Helper()
	name := filepath.Join(t.TempDir(), "listener-token")
	if err := os.WriteFile(name, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}

func recordValue(sequence int, kind string, payload map[string]any, extensions map[string]map[string]any) map[string]any {
	return map[string]any{"schemaVersion": RecordSchema, "sequence": sequence, "type": kind, "payload": payload, "extensions": extensions}
}

func routePayload(scope harnessworker.WorkloadScope, fields map[string]any) map[string]any {
	fields["routeId"], fields["routeVersion"], fields["protocol"] = scope.RouteID, scope.RouteVersion, scope.Protocol
	return fields
}

func scopedPolicy(t *testing.T) proxycontract.Policy {
	t.Helper()
	file, err := os.Open(filepath.Join("..", "..", "..", "packages", "contracts", "proxy", "fixtures", "poc-policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	policy, err := proxycontract.DecodePolicy(file)
	if err != nil {
		t.Fatal(err)
	}
	return policy
}
