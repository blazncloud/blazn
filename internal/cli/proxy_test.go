package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/blazncloud/blazn/internal/proxy/activation"
	"github.com/blazncloud/blazn/internal/proxycontract"
)

type fakeProxyCommands struct {
	method, policy, mode, cursor string
	argv                         []string
	follow, yes, removeCA        bool
	result                       activation.Result
	err                          error
}

func (f *fakeProxyCommands) On(_ context.Context, policy, mode string) (activation.Result, error) {
	f.method, f.policy, f.mode = "on", policy, mode
	return f.result, f.err
}
func (f *fakeProxyCommands) Off(_ context.Context, removeCA bool) (activation.Result, error) {
	f.method, f.removeCA = "off", removeCA
	return f.result, f.err
}
func (f *fakeProxyCommands) Status(context.Context) (activation.Result, error) {
	f.method = "status"
	return f.result, f.err
}
func (f *fakeProxyCommands) Doctor(_ context.Context, policy string) (activation.Result, error) {
	f.method, f.policy = "doctor", policy
	return f.result, f.err
}
func (f *fakeProxyCommands) Routes(_ context.Context, policy string) ([]activation.Route, error) {
	f.method, f.policy = "routes", policy
	return []activation.Route{{ID: "local", DestinationClass: "local_node", DestinationProtocol: "openai-chat", Endpoint: "http://127.0.0.1:11434/v1"}}, f.err
}
func (f *fakeProxyCommands) Tail(_ context.Context, cursor string, follow bool) ([]activation.Event, error) {
	f.method, f.cursor, f.follow = "tail", cursor, follow
	return []activation.Event{{EventID: "90000000-0000-4000-8000-000000000001", Cursor: "2", Timestamp: "2026-08-22T00:00:00Z", Type: proxycontract.EventRequestStarted, ActivationID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", LogicalRequestID: "90000000-0000-4000-8000-000000000002", Attempt: 1, Protocol: proxycontract.ProtocolOpenAIChat, ModelAlias: "company-assistant", Policy: proxycontract.PolicyIdentity{ID: "11111111-1111-4111-8111-111111111111", Version: 1, Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, RouteID: "33333333-3333-4333-8333-333333333333", DestinationClass: proxycontract.DestinationLocalNode, Outcome: proxycontract.OutcomeSuccess, ReasonCode: proxycontract.EventReasonNone, LatencyMS: 0}}, f.err
}
func (f *fakeProxyCommands) Run(_ context.Context, policy string, argv []string) (activation.Result, error) {
	f.method, f.policy, f.argv = "run", policy, append([]string(nil), argv...)
	return f.result, f.err
}
func (f *fakeProxyCommands) Reset(_ context.Context, yes, removeCA bool) (activation.Result, error) {
	f.method, f.yes, f.removeCA = "reset", yes, removeCA
	return f.result, f.err
}

func proxyApp(fake *fakeProxyCommands) (*App, *bytes.Buffer, *bytes.Buffer) {
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	app := New(stdout, stderr, BuildInfo{})
	app.proxy = func() (proxyCommands, error) { return fake, nil }
	return app, stdout, stderr
}

func TestProxyCLIParsesFrozenCommandsAndExactArgv(t *testing.T) {
	fake := &fakeProxyCommands{result: activation.Result{Command: "proxy", ContractVersion: activation.ContractVersion, Status: "active", State: "active"}}
	app, stdout, stderr := proxyApp(fake)
	if code := app.Run([]string{"proxy", "on", "--policy", "policy.json", "--mode", "session", "--output=json"}); code != 0 || fake.method != "on" || fake.policy != "policy.json" || fake.mode != "session" || stderr.Len() != 0 || !strings.Contains(stdout.String(), `"contractVersion":"proxy/v1alpha1"`) {
		t.Fatalf("on code=%d fake=%+v out=%s err=%s", code, fake, stdout, stderr)
	}
	stdout.Reset()
	if code := app.Run([]string{"proxy", "run", "--policy=policy.json", "--", "tool", "a b", "$HOME", ";rm"}); code != 0 || fake.method != "run" || strings.Join(fake.argv, "|") != "tool|a b|$HOME|;rm" {
		t.Fatalf("run code=%d argv=%q", code, fake.argv)
	}
	stdout.Reset()
	child := 23
	fake.result = activation.Result{Command: "proxy run", ContractVersion: activation.ContractVersion, Status: "completed", State: "inactive", ChildExitCode: &child, ExitCode: 23}
	if code := app.Run([]string{"--output=json", "proxy", "run", "--policy=p", "--", "tool", "--output=json"}); code != 23 || !strings.Contains(stdout.String(), `"childExitCode":23`) || strings.Join(fake.argv, "|") != "tool|--output=json" {
		t.Fatalf("json child exit code=%d argv=%q out=%s", code, fake.argv, stdout)
	}
	stdout.Reset()
	if code := app.Run([]string{"--output=jsonl", "proxy", "tail", "--cursor", "1", "--follow"}); code != 0 || fake.method != "tail" || fake.cursor != "1" || !fake.follow || !strings.Contains(stdout.String(), `"type":"request_started"`) {
		t.Fatalf("tail code=%d out=%s", code, stdout)
	}
	fake.result = activation.Result{Command: "proxy reset", ContractVersion: activation.ContractVersion, Status: "inactive", State: "inactive"}
	if code := app.Run([]string{"proxy", "reset", "--yes", "--remove-ca"}); code != 0 || !fake.yes || !fake.removeCA {
		t.Fatalf("reset code=%d", code)
	}
	stdout.Reset()
	fake.removeCA = false
	if code := app.Run([]string{"proxy", "off", "--remove-ca"}); code != 0 || !fake.removeCA {
		t.Fatalf("off remove-ca code=%d forwarded=%v", code, fake.removeCA)
	}
}

func TestProxyCLIRejectsAmbiguousFlagsAndMapsStableErrors(t *testing.T) {
	fake := &fakeProxyCommands{result: activation.Result{ExitCode: 6}, err: activation.ErrDifferentPolicy}
	app, stdout, stderr := proxyApp(fake)
	if code := app.Run([]string{"proxy", "on", "--policy", "one", "--policy", "two"}); code != ExitUsage || fake.method != "" {
		t.Fatalf("duplicate code=%d method=%s", code, fake.method)
	}
	stderr.Reset()
	if code := app.Run([]string{"proxy", "run", "--policy", "p", "tool"}); code != ExitUsage {
		t.Fatalf("missing separator code=%d", code)
	}
	stderr.Reset()
	if code := app.Run([]string{"proxy", "on", "--policy", "p", "--output=json"}); code != 6 || !strings.Contains(stdout.String(), "PROXY_ALREADY_ACTIVE_DIFFERENT_POLICY") {
		t.Fatalf("conflict code=%d out=%s err=%s", code, stdout, stderr)
	}
	stdout.Reset()
	fake.err = activation.ErrCARemovalUnsupported
	fake.result = activation.Result{ExitCode: 7}
	if code := app.Run([]string{"proxy", "off", "--remove-ca", "--output=json"}); code != 7 || !strings.Contains(stdout.String(), "PROXY_CA_REMOVAL_UNSUPPORTED") {
		t.Fatalf("ca code=%d out=%s", code, stdout)
	}
	stdout.Reset()
	fake.err = activation.ErrDifferentScope
	fake.result = activation.Result{ExitCode: 6}
	if code := app.Run([]string{"proxy", "on", "--policy", "p", "--output=json"}); code != 6 || !strings.Contains(stdout.String(), "PROXY_ALREADY_ACTIVE_DIFFERENT_SCOPE") {
		t.Fatalf("scope code=%d out=%s", code, stdout)
	}
	stdout.Reset()
	fake.err = errors.Join(activation.ErrLifecycleConflict, errors.New("reservation contained sk-proj-secret"))
	fake.result = activation.Result{ExitCode: 6}
	if code := app.Run([]string{"proxy", "status", "--output=json"}); code != 6 || !strings.Contains(stdout.String(), "PROXY_LIFECYCLE_CONFLICT") || strings.Contains(stdout.String(), "sk-proj-secret") {
		t.Fatalf("lifecycle conflict code=%d out=%s", code, stdout)
	}
	stdout.Reset()
	fake.err = errors.Join(activation.ErrLifecycleDeadline, errors.New("deadline adapter secret"))
	fake.result = activation.Result{ExitCode: 8}
	if code := app.Run([]string{"proxy", "off", "--output=json"}); code != 8 || !strings.Contains(stdout.String(), "PROXY_LIFECYCLE_DEADLINE") || strings.Contains(stdout.String(), "adapter secret") {
		t.Fatalf("lifecycle deadline code=%d out=%s", code, stdout)
	}
}

func TestProxyCLIUnavailableAndHelpNeedNoPlatformMutation(t *testing.T) {
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	app := New(stdout, stderr, BuildInfo{})
	app.proxy = func() (proxyCommands, error) { return nil, errors.New("unavailable") }
	if code := app.Run([]string{"help", "proxy"}); code != 0 || !strings.Contains(stdout.String(), "proxy on|off") || stderr.Len() != 0 {
		t.Fatalf("help code=%d out=%s err=%s", code, stdout, stderr)
	}
	stdout.Reset()
	if code := app.Run([]string{"proxy", "status", "--output=json"}); code != ExitUnavailable || !strings.Contains(stdout.String(), "PROXY_PLATFORM_UNAVAILABLE") {
		t.Fatalf("status code=%d out=%s", code, stdout)
	}
}

func TestProxyCLIRedactsSecretBearingBoundaryErrors(t *testing.T) {
	const secret = "sk-proj-adversarial-listener-secret"
	for _, testCase := range []struct {
		name string
		args []string
		err  error
	}{{name: "policy", args: []string{"proxy", "on", "--policy", "policy.json"}, err: errors.New("policy loader exposed " + secret)}, {name: "runner", args: []string{"proxy", "run", "--policy", "policy.json", "--", "tool"}, err: errors.New("runner environment exposed OPENAI_API_KEY=" + secret)}, {name: "adapter recovery", args: []string{"proxy", "off"}, err: errors.Join(activation.ErrRecovery, errors.New("publication adapter exposed " + secret))}} {
		for _, format := range []string{"human", "json"} {
			t.Run(testCase.name+"/"+format, func(t *testing.T) {
				fake := &fakeProxyCommands{result: activation.Result{ExitCode: 1}, err: testCase.err}
				app, stdout, stderr := proxyApp(fake)
				args := append([]string{"--output=" + format}, testCase.args...)
				if code := app.Run(args); code == 0 {
					t.Fatal("secret-bearing error unexpectedly succeeded")
				}
				combined := stdout.String() + stderr.String()
				if strings.Contains(combined, secret) || strings.Contains(combined, "OPENAI_API_KEY=") {
					t.Fatalf("secret-bearing error crossed CLI boundary: %s", combined)
				}
				if !strings.Contains(combined, "proxy") {
					t.Fatalf("stable public proxy error missing: %s", combined)
				}
			})
		}
	}
}
