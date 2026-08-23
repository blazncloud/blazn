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
func (f *fakeProxyCommands) Off(context.Context) (activation.Result, error) {
	f.method = "off"
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
	if code := app.Run([]string{"--output=jsonl", "proxy", "tail", "--cursor", "1", "--follow"}); code != 0 || fake.method != "tail" || fake.cursor != "1" || !fake.follow || !strings.Contains(stdout.String(), `"type":"request_started"`) {
		t.Fatalf("tail code=%d out=%s", code, stdout)
	}
	if code := app.Run([]string{"proxy", "reset", "--yes", "--remove-ca"}); code != 0 || !fake.yes || !fake.removeCA {
		t.Fatalf("reset code=%d", code)
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
