//go:build darwin || linux

package scopedrun

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/blazncloud/blazn/internal/proxy/activation"
	"github.com/blazncloud/blazn/internal/proxy/credential"
	"github.com/blazncloud/blazn/internal/proxy/state"
	"github.com/blazncloud/blazn/internal/proxycontract"
)

const scopedTestDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type staticPolicyLoader struct{ policy proxycontract.Policy }

func (l staticPolicyLoader) Load(string) (proxycontract.Policy, string, error) {
	return l.policy, scopedTestDigest, nil
}

type backendFunc func(context.Context, string) ([]byte, error)

func (f backendFunc) Lookup(ctx context.Context, ref string) ([]byte, error) { return f(ctx, ref) }

type recordingService struct {
	path string
	argv []string
}

func (s *recordingService) Run(_ context.Context, path string, argv []string) (activation.Result, error) {
	s.path, s.argv = path, append([]string(nil), argv...)
	return activation.Result{Command: "proxy run", ContractVersion: activation.ContractVersion, Status: "completed", State: "inactive", ExitCode: 0}, nil
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

func TestRunPreResolvesCredentialsBeforeProductionStateOrListenerFactory(t *testing.T) {
	policy := scopedPolicy(t)
	var backendCalls atomic.Int32
	serviceCalls := 0
	failing := backendFunc(func(context.Context, string) ([]byte, error) {
		backendCalls.Add(1)
		return nil, errors.New("native store unavailable")
	})
	commands := &Commands{deps: dependencies{
		loadPolicy: staticPolicyLoader{policy: policy},
		resolve:    &credential.Resolver{NodeRoute: failing, WorkspaceVault: failing},
		newService: func(string, proxycontract.Policy, string, *credential.Snapshot) (serviceRunner, error) {
			serviceCalls++
			return nil, errors.New("must not be called")
		},
		now: func() time.Time { return time.Unix(1, 0) },
	}}
	result, err := commands.Run(context.Background(), "policy.json", []string{"tool"})
	if !errors.Is(err, activation.ErrCredentialUnavailable) || result.ExitCode != 3 || backendCalls.Load() == 0 || serviceCalls != 0 {
		t.Fatalf("result=%+v err=%v backendCalls=%d serviceCalls=%d", result, err, backendCalls.Load(), serviceCalls)
	}
}

func TestRunPassesFrozenPolicySnapshotAndExactArgv(t *testing.T) {
	policy := scopedPolicy(t)
	backend := backendFunc(func(_ context.Context, ref string) ([]byte, error) {
		return []byte("credential-for-" + ref), nil
	})
	recorder := &recordingService{}
	serviceCalls := 0
	commands := &Commands{deps: dependencies{
		loadPolicy: staticPolicyLoader{policy: policy},
		resolve:    &credential.Resolver{NodeRoute: backend, WorkspaceVault: backend},
		newService: func(path string, got proxycontract.Policy, digest string, snapshot *credential.Snapshot) (serviceRunner, error) {
			serviceCalls++
			if path != "policy.json" || !reflect.DeepEqual(got, policy) || digest != scopedTestDigest || snapshot == nil {
				t.Fatalf("factory path=%q digest=%q snapshot=%v", path, digest, snapshot)
			}
			return recorder, nil
		},
		now: time.Now,
	}}
	argv := []string{"tool", "a b", "$HOME", ";touch", "$(id)", "'quoted'"}
	result, err := commands.Run(context.Background(), "policy.json", argv)
	if err != nil || result.ExitCode != 0 || serviceCalls != 1 || !reflect.DeepEqual(recorder.argv, argv) || recorder.path != "policy.json" {
		t.Fatalf("result=%+v err=%v calls=%d path=%q argv=%q", result, err, serviceCalls, recorder.path, recorder.argv)
	}
	argv[0] = "mutated"
	if recorder.argv[0] != "tool" {
		t.Fatal("service retained caller argv storage")
	}
}

func TestOnlyScopedRunIsEnabled(t *testing.T) {
	commands := &Commands{deps: dependencies{now: time.Now}}
	if result, err := commands.On(context.Background(), "policy", "auto"); !errors.Is(err, activation.ErrSessionUnsupported) || result.ExitCode != 2 {
		t.Fatalf("on result=%+v err=%v", result, err)
	}
	for name, call := range map[string]func() error{
		"off":    func() error { _, err := commands.Off(context.Background(), false); return err },
		"status": func() error { _, err := commands.Status(context.Background()); return err },
		"doctor": func() error { _, err := commands.Doctor(context.Background(), "policy"); return err },
		"routes": func() error { _, err := commands.Routes(context.Background(), "policy"); return err },
		"tail":   func() error { _, err := commands.Tail(context.Background(), "", false); return err },
		"reset":  func() error { _, err := commands.Reset(context.Background(), true, false); return err },
	} {
		if err := call(); !errors.Is(err, activation.ErrUnavailable) {
			t.Fatalf("%s unexpectedly enabled: %v", name, err)
		}
	}
}

func TestProcessEnvironmentIsExactNoOpChildScope(t *testing.T) {
	base := []string{
		"PATH=/bin", "HOME=/account", "HTTP_PROXY=http://direct", "HTTPS_PROXY=https://direct",
		"OPENAI_BASE_URL=https://old", "OPENAI_API_KEY=provider-old", "ANTHROPIC_BASE_URL=https://old",
		"ANTHROPIC_API_KEY=provider-old", "ANTHROPIC_AUTH_TOKEN=provider-old",
		"BLAZN_ACCESS_TOKEN=management", "BLAZN_REFRESH_SECRET=management", "BLAZN_API_URL=https://management.example",
	}
	environment := processEnvironment{uid: os.Getuid(), environ: func() []string { return append([]string(nil), base...) }}
	child := environment.BaseEnvironment()
	joined := strings.Join(child, "\n")
	for _, forbidden := range append(append([]string(nil), state.EnvironmentNames[:]...), "BLAZN_ACCESS_TOKEN", "BLAZN_REFRESH_SECRET") {
		if strings.Contains(joined, forbidden+"=") {
			t.Fatalf("credential or replaced proxy variable reached child base: %s", joined)
		}
	}
	for _, preserved := range []string{"PATH=/bin", "HOME=/account", "HTTP_PROXY=http://direct", "HTTPS_PROXY=https://direct", "BLAZN_API_URL=https://management.example"} {
		if !strings.Contains(joined, preserved) {
			t.Fatalf("non-credential base variable %q was changed: %s", preserved, joined)
		}
	}
	before := append([]string(nil), base...)
	if err := environment.Publish(context.Background(), "process_environment", nil); !errors.Is(err, activation.ErrSessionUnsupported) {
		t.Fatalf("process environment unexpectedly published: %v", err)
	}
	for _, name := range state.EnvironmentNames {
		result, err := environment.CompareAndSet(context.Background(), state.CompareAndSetRequest{Name: name})
		if err != nil || result != state.CASAlreadyRestored {
			t.Fatalf("no-op restore %s=%s err=%v", name, result, err)
		}
	}
	if !reflect.DeepEqual(base, before) {
		t.Fatal("process environment adapter mutated its source")
	}
	session, err := environment.SessionIdentity(context.Background())
	if err != nil || !strings.HasPrefix(session, fmt.Sprintf("uid:%d/process:", os.Getuid())) {
		t.Fatalf("session=%q err=%v", session, err)
	}
	wrongUID := environment
	wrongUID.uid++
	if _, err := wrongUID.SessionIdentity(context.Background()); !errors.Is(err, activation.ErrUnavailable) {
		t.Fatal("environment accepted non-current UID authority")
	}
}

func TestCurrentProcessIdentityBindsBinaryPIDAndCurrentUID(t *testing.T) {
	binary, process, err := currentProcessIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(binary.Path) || !strings.HasPrefix(binary.Digest, "sha256:") || len(binary.Digest) != 71 || process.pid != os.Getpid() || process.start == "" || process.executable == "" {
		t.Fatalf("binary=%+v process=%+v", binary, process)
	}
	metadata := activation.ListenerMetadata{ActivationID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Nonce: strings.Repeat("n", 32), Mode: "scoped_run", SessionIdentity: fmt.Sprintf("uid:%d/process:%d", os.Getuid(), os.Getpid()), BinaryDigest: binary.Digest, Generation: 1, OwnerUID: int64(os.Getuid())}
	identity, proof, err := process.Proof(context.Background(), "127.0.0.1:8123", scopedTestDigest, metadata)
	if err != nil || identity.PID != os.Getpid() || proof.OwnerUID != os.Getuid() || proof.BinaryDigest != binary.Digest || proof.ProcessStartIdentity != process.start {
		t.Fatalf("identity=%+v proof=%+v err=%v", identity, proof, err)
	}
	metadata.OwnerUID++
	if _, _, err := process.Proof(context.Background(), "127.0.0.1:8123", scopedTestDigest, metadata); !errors.Is(err, activation.ErrUnavailable) {
		t.Fatal("listener identity accepted non-current UID")
	}
}

func TestExecRunnerUsesExactArgvStreamsEnvironmentAndPreservesExit(t *testing.T) {
	stdin := strings.NewReader("stdin-value")
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	runner := ExecRunner{Streams: processStreams{stdin: stdin, stdout: stdout, stderr: stderr}}
	argv := []string{os.Args[0], "-test.run=TestScopedRunHelper", "--", "a b", "$HOME", ";touch", "$(id)"}
	environment := []string{"GO_WANT_SCOPED_RUN_HELPER=argv", "ONLY_CHILD_ENV=present"}
	exit, err := runner.Run(context.Background(), argv, environment)
	if err != nil || exit != 23 {
		t.Fatalf("exit=%d err=%v stderr=%s", exit, err, stderr)
	}
	var report struct {
		Args  []string `json:"args"`
		Input string   `json:"input"`
		Only  string   `json:"only"`
		Home  string   `json:"home"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	want := []string{"a b", "$HOME", ";touch", "$(id)"}
	if !reflect.DeepEqual(report.Args, want) || report.Input != "stdin-value" || report.Only != "present" || report.Home != "" || stderr.String() != "helper-stderr" {
		t.Fatalf("report=%+v stderr=%q", report, stderr)
	}
}

func TestExecRunnerPreservesSignalAndBoundsCancellation(t *testing.T) {
	runner := ExecRunner{Streams: processStreams{stdin: strings.NewReader(""), stdout: io.Discard, stderr: io.Discard}}
	exit, err := runner.Run(context.Background(), []string{os.Args[0], "-test.run=TestScopedRunHelper"}, []string{"GO_WANT_SCOPED_RUN_HELPER=self-term"})
	if err != nil || exit != 128+int(syscall.SIGTERM) {
		t.Fatalf("signal exit=%d err=%v", exit, err)
	}

	reader, writer := io.Pipe()
	runner.Streams.stdout = writer
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan struct {
		exit int
		err  error
	}, 1)
	go func() {
		exit, err := runner.Run(ctx, []string{os.Args[0], "-test.run=TestScopedRunHelper"}, []string{"GO_WANT_SCOPED_RUN_HELPER=cancel"})
		result <- struct {
			exit int
			err  error
		}{exit: exit, err: err}
	}()
	line, readErr := bufio.NewReader(reader).ReadString('\n')
	if readErr != nil || line != "ready\n" {
		t.Fatalf("helper readiness=%q err=%v", line, readErr)
	}
	cancel()
	select {
	case got := <-result:
		_ = writer.Close()
		_ = reader.Close()
		if got.err != nil || got.exit != 42 {
			t.Fatalf("cancel exit=%d err=%v", got.exit, got.err)
		}
	case <-time.After(childCancelGrace + 2*time.Second):
		t.Fatal("cancellation exceeded the bounded grace period")
	}
}

func TestExecRunnerLeavesApplicationConfigBytesUnchanged(t *testing.T) {
	root := t.TempDir()
	paths := []string{filepath.Join(root, "codex.toml"), filepath.Join(root, "claude.json"), filepath.Join(root, "hermes.yaml")}
	before := map[string][sha256.Size]byte{}
	for index, path := range paths {
		body := []byte(fmt.Sprintf("config-%d\nHTTP_PROXY=direct\n", index))
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
		before[path] = sha256.Sum256(body)
	}
	runner := ExecRunner{Streams: processStreams{stdin: strings.NewReader(""), stdout: io.Discard, stderr: io.Discard}}
	if exit, err := runner.Run(context.Background(), []string{os.Args[0], "-test.run=TestScopedRunHelper"}, []string{"GO_WANT_SCOPED_RUN_HELPER=success"}); err != nil || exit != 0 {
		t.Fatalf("exit=%d err=%v", exit, err)
	}
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil || sha256.Sum256(body) != before[path] {
			t.Fatalf("config changed: %s err=%v", path, err)
		}
	}
}

func TestScopedRunHelper(t *testing.T) {
	mode := os.Getenv("GO_WANT_SCOPED_RUN_HELPER")
	if mode == "" {
		return
	}
	switch mode {
	case "argv":
		separator := -1
		for index, value := range os.Args {
			if value == "--" {
				separator = index
				break
			}
		}
		input, _ := io.ReadAll(os.Stdin)
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"args": os.Args[separator+1:], "input": string(input), "only": os.Getenv("ONLY_CHILD_ENV"), "home": os.Getenv("HOME")})
		_, _ = io.WriteString(os.Stderr, "helper-stderr")
		os.Exit(23)
	case "self-term":
		signal.Reset(syscall.SIGTERM)
		_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
		select {}
	case "cancel":
		caught := make(chan os.Signal, 1)
		signal.Notify(caught, syscall.SIGTERM)
		_, _ = io.WriteString(os.Stdout, "ready\n")
		<-caught
		os.Exit(42)
	case "success":
		os.Exit(0)
	default:
		os.Exit(97)
	}
}
