package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KingJammin/blazn/internal/proxycontract"
)

const activationID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"

type credentialMap map[string]string

func (c credentialMap) DestinationCredential(_ context.Context, ref string) (string, error) {
	value, ok := c[ref]
	if !ok {
		return "", errors.New("missing")
	}
	return value, nil
}

type staticDNS map[string][]netip.Addr

func (d staticDNS) LookupNetIP(_ context.Context, _ string, host string) ([]netip.Addr, error) {
	value, ok := d[host]
	if !ok {
		return nil, errors.New("not found")
	}
	return value, nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func fixturePolicy(t *testing.T) proxycontract.Policy {
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

type upstreamCall struct{ RouteID, Authorization, ListenerToken, Path, Body string }

func testHandler(t *testing.T, transport func(proxycontract.Route, *http.Request) (*http.Response, error), sink EventSink) (*Handler, *proxycontract.Policy) {
	t.Helper()
	policy := fixturePolicy(t)
	config := Config{Policy: policy, ActivationID: activationID, ListenerToken: "listener-secret", Credentials: credentialMap{"node-route://qwen38": "local-destination", "workspace-vault://poc/model-providers/openai": "cloud-destination"}, Resolver: EndpointResolver{DNS: staticDNS{"127.0.0.1": {netip.MustParseAddr("127.0.0.1")}, "api.openai.com": {netip.MustParseAddr("93.184.216.34")}}}, ClientFactory: func(route proxycontract.Route, _ ResolvedEndpoint) *http.Client {
		return &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) { return transport(route, request) }), CheckRedirect: redirectPolicy(route)}
	}, Events: sink, Now: func() time.Time { return time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC) }}
	handler, err := NewHandler(config)
	if err != nil {
		t.Fatal(err)
	}
	return handler, &policy
}

func response(status int, contentType, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(body))}
}
func request(handler http.Handler, path, body string) *httptest.ResponseRecorder {
	incoming := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	incoming.Header.Set("Authorization", "Bearer listener-secret")
	incoming.Header.Set("Content-Type", "application/json")
	record := httptest.NewRecorder()
	handler.ServeHTTP(record, incoming)
	return record
}

func TestChatFallsBackToResponsesAndNeverForwardsListenerCredential(t *testing.T) {
	var mu sync.Mutex
	calls := []upstreamCall{}
	handler, _ := testHandler(t, func(route proxycontract.Route, incoming *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(incoming.Body)
		mu.Lock()
		calls = append(calls, upstreamCall{route.ID, incoming.Header.Get("Authorization"), incoming.Header.Get("x-api-key"), incoming.URL.Path, string(body)})
		mu.Unlock()
		if route.Model == "qwen3.8" {
			return response(503, "application/json", `{"error":"down"}`), nil
		}
		return response(200, "application/json", `{"id":"resp_1","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"from cloud"}]}],"usage":{"input_tokens":7,"output_tokens":3}}`), nil
	}, nil)
	record := request(handler, "/v1/chat/completions", `{"model":"company-assistant","messages":[{"role":"user","content":"secret prompt"}],"max_completion_tokens":32}`)
	if record.Code != 200 {
		t.Fatalf("status %d: %s", record.Code, record.Body.String())
	}
	if !strings.Contains(record.Body.String(), "from cloud") || strings.Contains(record.Body.String(), "gpt-5.4") {
		t.Fatalf("unexpected response %s", record.Body.String())
	}
	if len(calls) != 2 {
		t.Fatalf("calls=%d", len(calls))
	}
	if calls[0].Authorization != "Bearer local-destination" || calls[1].Authorization != "Bearer cloud-destination" {
		t.Fatalf("destination credentials not isolated: %#v", calls)
	}
	for _, call := range calls {
		if strings.Contains(call.Authorization, "listener-secret") || call.ListenerToken != "" {
			t.Fatalf("listener secret forwarded: %#v", call)
		}
	}
}

func TestResponsesTranslatesToLocalChatAndReturnsResponsesShape(t *testing.T) {
	var upstreamBody string
	handler, _ := testHandler(t, func(_ proxycontract.Route, incoming *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(incoming.Body)
		upstreamBody = string(body)
		return response(200, "application/json", `{"id":"chat_1","choices":[{"message":{"content":"local answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`), nil
	}, nil)
	record := request(handler, "/v1/responses", `{"model":"company-assistant","instructions":"be concise","input":"hello","max_output_tokens":24}`)
	if record.Code != 200 || !strings.Contains(record.Body.String(), `"object":"response"`) || !strings.Contains(record.Body.String(), "local answer") {
		t.Fatalf("response %d %s", record.Code, record.Body.String())
	}
	if !strings.Contains(upstreamBody, `"model":"qwen3.8"`) || !strings.Contains(upstreamBody, "be concise") {
		t.Fatalf("translation: %s", upstreamBody)
	}
}

func TestNoFallbackAfterSuccessfulHeadersOrResponseBytes(t *testing.T) {
	var calls atomic.Int32
	handler, _ := testHandler(t, func(route proxycontract.Route, _ *http.Request) (*http.Response, error) {
		calls.Add(1)
		if route.Model != "qwen3.8" {
			t.Fatal("fallback happened after successful headers")
		}
		return response(200, "application/json", `not-json`), nil
	}, nil)
	record := request(handler, "/v1/chat/completions", `{"model":"company-assistant","messages":[{"role":"user","content":"hello"}]}`)
	if record.Code != 502 || calls.Load() != 1 {
		t.Fatalf("status=%d calls=%d body=%s", record.Code, calls.Load(), record.Body.String())
	}
}

func TestChatStreamingNormalizesSSEAndUsage(t *testing.T) {
	handler, _ := testHandler(t, func(_ proxycontract.Route, _ *http.Request) (*http.Response, error) {
		body := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n"
		return response(200, "text/event-stream", body), nil
	}, nil)
	record := request(handler, "/v1/chat/completions", `{"model":"company-assistant","messages":[{"role":"user","content":"hello"}],"stream":true}`)
	if record.Code != 200 || !strings.Contains(record.Body.String(), `"content":"hi"`) || !strings.Contains(record.Body.String(), `"prompt_tokens":4`) {
		t.Fatalf("stream %d: %s", record.Code, record.Body.String())
	}
}

func TestChatStreamingPreservesToolCalls(t *testing.T) {
	handler, _ := testHandler(t, func(_ proxycontract.Route, _ *http.Request) (*http.Response, error) {
		body := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\"}}]},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"x\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
		return response(200, "text/event-stream", body), nil
	}, nil)
	record := request(handler, "/v1/chat/completions", `{"model":"company-assistant","messages":[{"role":"user","content":"use tool"}],"stream":true,"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`)
	if record.Code != 200 || !strings.Contains(record.Body.String(), `"id":"call_1"`) || !strings.Contains(record.Body.String(), `"name":"lookup"`) || !strings.Contains(record.Body.String(), `"finish_reason":"tool_calls"`) {
		t.Fatalf("tool stream %d: %s", record.Code, record.Body.String())
	}
}

func TestListenerAuthenticationAndModels(t *testing.T) {
	handler, _ := testHandler(t, func(proxycontract.Route, *http.Request) (*http.Response, error) {
		t.Fatal("upstream should not run")
		return nil, nil
	}, nil)
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if unauthorized.Code != 401 {
		t.Fatalf("status=%d", unauthorized.Code)
	}
	ambiguousRequest := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	ambiguousRequest.Header.Set("Authorization", "Bearer listener-secret")
	ambiguousRequest.Header.Set("x-api-key", "listener-secret")
	ambiguous := httptest.NewRecorder()
	handler.ServeHTTP(ambiguous, ambiguousRequest)
	if ambiguous.Code != 401 {
		t.Fatalf("ambiguous status=%d", ambiguous.Code)
	}
	authorizedRequest := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	authorizedRequest.Header.Set("x-api-key", "listener-secret")
	authorized := httptest.NewRecorder()
	handler.ServeHTTP(authorized, authorizedRequest)
	if authorized.Code != 200 || !strings.Contains(authorized.Body.String(), "company-assistant") {
		t.Fatalf("models %d %s", authorized.Code, authorized.Body.String())
	}
	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != 200 || strings.Contains(health.Body.String(), "policy") {
		t.Fatalf("health leaked state: %s", health.Body.String())
	}
}

func TestPolicyAndRequestEnforcement(t *testing.T) {
	handler, _ := testHandler(t, func(proxycontract.Route, *http.Request) (*http.Response, error) {
		t.Fatal("upstream should not run")
		return nil, nil
	}, nil)
	cases := []string{`{"model":"company-assistant-restricted","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`, `{"model":"company-assistant","messages":[{"role":"user","content":"x"}],"max_completion_tokens":999999}`, `{"model":"missing","messages":[{"role":"user","content":"x"}]}`, `{"model":"company-assistant","messages":[{"role":"user","content":"x"}],"unknown":true}`}
	for _, body := range cases {
		record := request(handler, "/v1/chat/completions", body)
		if record.Code < 400 {
			t.Fatalf("accepted %s: %s", body, record.Body.String())
		}
	}
}

func TestEventsAreOperationalAndRedacted(t *testing.T) {
	var events []proxycontract.Event
	handler, _ := testHandler(t, func(_ proxycontract.Route, _ *http.Request) (*http.Response, error) {
		return response(200, "application/json", `{"choices":[{"message":{"content":"answer secret"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`), nil
	}, EventSinkFunc(func(event proxycontract.Event) { events = append(events, event) }))
	record := request(handler, "/v1/chat/completions", `{"model":"company-assistant","messages":[{"role":"user","content":"prompt secret"}]}`)
	if record.Code != 200 {
		t.Fatal(record.Body.String())
	}
	encoded, _ := json.Marshal(events)
	for _, forbidden := range []string{"prompt secret", "answer secret", "listener-secret", "local-destination", "cloud-destination"} {
		if bytes.Contains(encoded, []byte(forbidden)) {
			t.Fatalf("events leaked %q: %s", forbidden, encoded)
		}
	}
	if len(events) < 2 {
		t.Fatalf("events=%d", len(events))
	}
	for _, event := range events {
		if err := event.Validate(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEndpointResolverRejectsSSRFAndPinsAllowedAddresses(t *testing.T) {
	policy := fixturePolicy(t)
	external := policy.Routes[1]
	for _, address := range []string{"127.0.0.1", "10.0.0.1", "100.64.0.1", "169.254.169.254", "192.0.2.1", "198.18.0.1", "203.0.113.1", "::1", "fe80::1"} {
		resolver := EndpointResolver{DNS: staticDNS{"api.openai.com": {netip.MustParseAddr(address)}}}
		if _, err := resolver.Resolve(context.Background(), external); err == nil {
			t.Fatalf("accepted external %s", address)
		}
	}
	resolver := EndpointResolver{DNS: staticDNS{"api.openai.com": {netip.MustParseAddr("93.184.216.34")}}}
	resolved, err := resolver.Resolve(context.Background(), external)
	if err != nil || len(resolved.Addresses) != 1 {
		t.Fatalf("resolve: %#v %v", resolved, err)
	}
	local := policy.Routes[0]
	resolver = EndpointResolver{DNS: staticDNS{"127.0.0.1": {netip.MustParseAddr("192.168.1.2")}}}
	if _, err = resolver.Resolve(context.Background(), local); err == nil {
		t.Fatal("accepted non-loopback local destination")
	}
}

func TestLoadPolicyRequiresOwnerOnlyRegularFile(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "packages", "contracts", "proxy", "fixtures", "poc-policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "policy.json")
	if err = os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	policy, digest, err := LoadPolicy(path)
	if err != nil || policy.ID == "" || !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("load: %v %q", err, digest)
	}
	if err = os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err = LoadPolicy(path); err == nil {
		t.Fatal("accepted group/world-readable policy")
	}
}

func TestCancellationPropagatesBeforeFirstByte(t *testing.T) {
	started := make(chan struct{})
	var calls atomic.Int32
	handler, _ := testHandler(t, func(_ proxycontract.Route, incoming *http.Request) (*http.Response, error) {
		if calls.Add(1) > 1 {
			t.Fatal("cancellation triggered fallback")
		}
		close(started)
		<-incoming.Context().Done()
		return nil, incoming.Context().Err()
	}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	incoming := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"company-assistant-restricted","messages":[{"role":"user","content":"hello"}]}`)).WithContext(ctx)
	incoming.Header.Set("Authorization", "Bearer listener-secret")
	incoming.Header.Set("Content-Type", "application/json")
	done := make(chan struct{})
	go func() { handler.ServeHTTP(httptest.NewRecorder(), incoming); close(done) }()
	<-started
	cancel()
	select {
	case <-done:
		if calls.Load() != 1 {
			t.Fatalf("calls=%d", calls.Load())
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not propagate")
	}
}

func TestRedirectPolicyRejectsHostOrSchemeChange(t *testing.T) {
	policy := fixturePolicy(t)
	route := policy.Routes[1]
	check := redirectPolicy(route)
	prior := &http.Request{URL: mustURL("https://api.openai.com:443/v1/responses")}
	for _, raw := range []string{"https://evil.example:443/v1", "http://api.openai.com:443/v1", "https://api.openai.com:444/v1"} {
		if err := check(&http.Request{URL: mustURL(raw)}, []*http.Request{prior}); err == nil {
			t.Fatalf("accepted redirect %s", raw)
		}
	}
}
func mustURL(raw string) *url.URL {
	value, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return value
}
