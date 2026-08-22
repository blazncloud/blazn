package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	config := Config{Policy: policy, ActivationID: activationID, ListenerToken: "listener-secret", Credentials: credentialMap{"node-route://qwen38": "local-destination", "workspace-vault://poc/model-providers/openai": "cloud-destination"}, Resolver: EndpointResolver{DNS: staticDNS{"127.0.0.1": {netip.MustParseAddr("127.0.0.1")}, "api.openai.com": {netip.MustParseAddr("93.184.216.34")}}}, Events: sink, Now: func() time.Time { return time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC) }}
	handler, err := NewHandler(config)
	if err != nil {
		t.Fatal(err)
	}
	handler.transportFactory = func(route proxycontract.Route, _ ResolvedEndpoint) http.RoundTripper {
		return roundTripFunc(func(request *http.Request) (*http.Response, error) { return transport(route, request) })
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
	record := request(handler, "/v1/responses", `{"model":"company-assistant","instructions":"be concise","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}],"max_output_tokens":24}`)
	if record.Code != 200 || !strings.Contains(record.Body.String(), `"object":"response"`) || !strings.Contains(record.Body.String(), "local answer") {
		t.Fatalf("response %d %s", record.Code, record.Body.String())
	}
	if !strings.Contains(upstreamBody, `"model":"qwen3.8"`) || !strings.Contains(upstreamBody, "be concise") {
		t.Fatalf("translation: %s", upstreamBody)
	}
}

func TestCodex0147ResponsesFixturePreservesSerializedFields(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "codex-0.147-responses.json"))
	if err != nil {
		t.Fatal(err)
	}
	var calls []upstreamCall
	handler, _ := testHandler(t, func(route proxycontract.Route, incoming *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(incoming.Body)
		calls = append(calls, upstreamCall{RouteID: route.ID, Path: incoming.URL.Path, Body: string(body)})
		stream := "data: {\"type\":\"response.created\",\"response\":{\"status\":\"in_progress\",\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"rs_out\",\"type\":\"reasoning\",\"summary\":[],\"encrypted_content\":\"opaque-next-turn\"}}\n\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"rs_out\",\"type\":\"reasoning\",\"summary\":[],\"encrypted_content\":\"opaque-next-turn\"}}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"{\\\"summary\\\":\\\"ok\\\"}\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":40,\"output_tokens\":8}}}\n\ndata: [DONE]\n\n"
		return response(200, "text/event-stream", stream), nil
	}, nil)
	record := request(handler, "/v1/responses", string(raw))
	if record.Code != 200 {
		t.Fatalf("status=%d body=%s", record.Code, record.Body.String())
	}
	if len(calls) != 1 || calls[0].Path != "/v1/responses" {
		t.Fatalf("expected direct Responses route: %#v", calls)
	}
	for _, want := range []string{`"parallel_tool_calls":true`, `"store":false`, `"reasoning":{"effort":"medium","summary":"auto"}`, `"stream_options":{"include_obfuscation":false}`, `"service_tier":"auto"`, `"prompt_cache_key":"codex-session-01"`, `"client_metadata":{"harness":"codex","version":"0.147.0"}`, `"type":"reasoning"`, `"name":"repository_result"`, `"strict":true`, `"output":"README contents"`} {
		if !strings.Contains(calls[0].Body, want) {
			t.Fatalf("outgoing request omitted %s: %s", want, calls[0].Body)
		}
	}
	stream := record.Body.String()
	ordered := []string{`"type":"response.created"`, `"type":"response.in_progress"`, `"type":"response.output_item.added"`, `"type":"response.content_part.added"`, `"type":"response.output_text.delta"`, `"type":"response.output_text.done"`, `"type":"response.content_part.done"`, `"type":"response.output_item.done"`, `"type":"response.completed"`, `data: [DONE]`}
	last := -1
	for _, needle := range ordered {
		next := strings.Index(stream, needle)
		if next <= last {
			t.Fatalf("invalid Responses stream order at %s: %s", needle, stream)
		}
		last = next
	}
	if !strings.Contains(stream, `"input_tokens":40`) || !strings.Contains(stream, `"status":"completed"`) || !strings.Contains(stream, `"encrypted_content":"opaque-next-turn"`) {
		t.Fatalf("completed response missing usage: %s", stream)
	}
}

func TestCodexUnsupportedFieldsAreRejectedBeforeRouting(t *testing.T) {
	var calls atomic.Int32
	handler, _ := testHandler(t, func(proxycontract.Route, *http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("unexpected")
	}, nil)
	for _, body := range []string{`{"model":"company-assistant","input":"x","store":true}`, `{"model":"company-assistant","input":"x","include":["file_search_call.results"]}`, `{"model":"company-assistant","input":"x","reasoning":{"effort":"extreme"}}`, `{"model":"company-assistant","input":"x","service_tier":"priority"}`, `{"model":"company-assistant","input":"x","client_metadata":{"nested":"ok","extra":1}}`} {
		record := request(handler, "/v1/responses", body)
		if record.Code < 400 || !strings.Contains(record.Body.String(), "unsupported_capability") {
			t.Fatalf("accepted unsupported request %s: %d %s", body, record.Code, record.Body.String())
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("routed %d rejected requests", calls.Load())
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

func TestDestinationAuthenticationFailureDoesNotFallback(t *testing.T) {
	var calls atomic.Int32
	handler, _ := testHandler(t, func(_ proxycontract.Route, _ *http.Request) (*http.Response, error) {
		calls.Add(1)
		return response(401, "application/json", `{"error":"bad credential"}`), nil
	}, nil)
	record := request(handler, "/v1/chat/completions", `{"model":"company-assistant","messages":[{"role":"user","content":"hello"}]}`)
	if record.Code != 502 || calls.Load() != 1 || !strings.Contains(record.Body.String(), "credential_unavailable") {
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

func TestPreFirstEventErrorFallsBackButEOFPostCommitFailsClosed(t *testing.T) {
	var calls atomic.Int32
	handler, _ := testHandler(t, func(route proxycontract.Route, _ *http.Request) (*http.Response, error) {
		calls.Add(1)
		if route.Model == "qwen3.8" {
			return response(200, "text/event-stream", "data: {\"error\":{\"message\":\"unavailable\"}}\n\ndata: [DONE]\n\n"), nil
		}
		return response(200, "text/event-stream", "data: {\"type\":\"response.created\",\"response\":{\"status\":\"in_progress\",\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"fallback\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":2,\"output_tokens\":1}}}\n\ndata: [DONE]\n\n"), nil
	}, nil)
	record := request(handler, "/v1/chat/completions", `{"model":"company-assistant","messages":[{"role":"user","content":"x"}],"stream":true}`)
	if record.Code != 200 || calls.Load() != 2 || !strings.Contains(record.Body.String(), "fallback") || !strings.HasSuffix(record.Body.String(), "data: [DONE]\n\n") {
		t.Fatalf("fallback stream status=%d calls=%d body=%s", record.Code, calls.Load(), record.Body.String())
	}
	var events []proxycontract.Event
	handler, _ = testHandler(t, func(proxycontract.Route, *http.Request) (*http.Response, error) {
		return response(200, "text/event-stream", "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"partial\"},\"finish_reason\":null}]}\n\n"), nil
	}, EventSinkFunc(func(event proxycontract.Event) { events = append(events, event) }))
	record = request(handler, "/v1/chat/completions", `{"model":"company-assistant-restricted","messages":[{"role":"user","content":"x"}],"stream":true}`)
	if !strings.Contains(record.Body.String(), "upstream_invalid_response") || !strings.HasSuffix(record.Body.String(), "data: [DONE]\n\n") {
		t.Fatalf("EOF did not emit explicit failed terminal: %s", record.Body.String())
	}
	foundFailure := false
	for _, event := range events {
		if event.Type == proxycontract.EventRequestFinished && event.Outcome == proxycontract.OutcomeFailed {
			foundFailure = true
		}
	}
	if !foundFailure {
		t.Fatalf("missing failed analytics: %#v", events)
	}
}

func TestChatStreamingPreservesToolCalls(t *testing.T) {
	handler, _ := testHandler(t, func(_ proxycontract.Route, _ *http.Request) (*http.Response, error) {
		body := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\"}}]},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"x\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n"
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

func TestHealthRequiresResolvableCredentialedRoute(t *testing.T) {
	policy := fixturePolicy(t)
	handler, err := NewHandler(Config{Policy: policy, ActivationID: activationID, ListenerToken: "listener", Credentials: credentialMap{}, Resolver: EndpointResolver{DNS: staticDNS{}}, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	record := httptest.NewRecorder()
	handler.ServeHTTP(record, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if record.Code != 503 || !strings.Contains(record.Body.String(), "not_ready") {
		t.Fatalf("health=%d %s", record.Code, record.Body.String())
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

func TestRouteSelectionBoundaryAndCapabilityMatrix(t *testing.T) {
	policy := fixturePolicy(t)
	index, err := newRouteIndex(policy)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		alias string
		data  proxycontract.DataClass
		want  int
	}{{"company-assistant", proxycontract.DataCompany, 2}, {"company-assistant-public", proxycontract.DataPublic, 2}, {"company-assistant-restricted", proxycontract.DataRestricted, 1}, {"company-assistant-local", proxycontract.DataLocalOnly, 1}}
	for _, tc := range cases {
		request := proxycontract.NormalizedRequest{LogicalRequestID: newUUID(), Protocol: proxycontract.ProtocolOpenAIChat, ModelAlias: tc.alias, DataClass: tc.data, Stream: false, Blocks: []proxycontract.RequestBlock{{Role: "user", Type: "text", Text: stringPtr("x")}}, Tools: []proxycontract.Tool{}, Limits: proxycontract.NormalizedLimits{MaxOutputTokens: 1, DeadlineAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339)}, CapabilitiesRequired: []proxycontract.Capability{proxycontract.CapabilityText}}
		routes, selectErr := index.selectRoutesFor(routedRequest{normalized: request})
		if selectErr != nil || len(routes) != tc.want {
			t.Fatalf("%s routes=%d err=%v", tc.alias, len(routes), selectErr)
		}
	}
	request := proxycontract.NormalizedRequest{LogicalRequestID: newUUID(), Protocol: proxycontract.ProtocolOpenAIChat, ModelAlias: "company-assistant", DataClass: proxycontract.DataCompany, Blocks: []proxycontract.RequestBlock{{Role: "user", Type: "text", Text: stringPtr("x")}}, Tools: []proxycontract.Tool{}, Limits: proxycontract.NormalizedLimits{MaxOutputTokens: 1, DeadlineAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339)}, CapabilitiesRequired: []proxycontract.Capability{proxycontract.Capability("unsupported")}}
	if _, err = index.selectRoutesFor(routedRequest{normalized: request}); err == nil {
		t.Fatal("accepted unsupported route capability")
	}
}

func TestFallbackStatusMatrix(t *testing.T) {
	for _, tc := range []struct{ status, wantCalls int }{{429, 2}, {500, 2}, {503, 2}, {404, 2}, {413, 2}, {400, 1}, {401, 1}, {403, 1}} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			var calls atomic.Int32
			handler, _ := testHandler(t, func(route proxycontract.Route, _ *http.Request) (*http.Response, error) {
				calls.Add(1)
				if route.Model == "qwen3.8" {
					return response(tc.status, "application/json", `{"error":"fixture"}`), nil
				}
				return response(200, "application/json", `{"id":"resp","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`), nil
			}, nil)
			_ = request(handler, "/v1/chat/completions", `{"model":"company-assistant","messages":[{"role":"user","content":"x"}]}`)
			if int(calls.Load()) != tc.wantCalls {
				t.Fatalf("status %d calls=%d want=%d", tc.status, calls.Load(), tc.wantCalls)
			}
		})
	}
}

func TestContextLimitCountsToolsSchemasAndMetadata(t *testing.T) {
	policy := fixturePolicy(t)
	policy.RequestLimits.MaxContextTokens = 512
	huge := strings.Repeat("x", 600)
	request := proxycontract.NormalizedRequest{LogicalRequestID: newUUID(), Protocol: proxycontract.ProtocolOpenAIResponses, ModelAlias: "company-assistant", DataClass: proxycontract.DataCompany, Blocks: []proxycontract.RequestBlock{{Role: "user", Type: "text", Text: stringPtr("x")}}, Tools: []proxycontract.Tool{{Name: "tool", Description: huge, InputSchema: map[string]any{"type": "object", "description": huge}}}, ResponseSchema: map[string]any{"type": "object", "description": huge}, Limits: proxycontract.NormalizedLimits{MaxOutputTokens: 1, DeadlineAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339)}, CapabilitiesRequired: []proxycontract.Capability{proxycontract.CapabilityText, proxycontract.CapabilityTools, proxycontract.CapabilityStructuredOutput}}
	metadata := &responsesMetadata{clientMetadata: json.RawMessage(`{"cache":"` + huge + `"}`)}
	if err := ensureContextLimit(routedRequest{normalized: request, source: sourceMetadata{responses: metadata}}, policy); err == nil {
		t.Fatal("full tool/schema/metadata context was not counted")
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
	if err := check(&http.Request{URL: mustURL("https://api.openai.com:443/admin")}, []*http.Request{prior}); err == nil {
		t.Fatal("accepted redirect outside frozen basePath")
	}
}
func mustURL(raw string) *url.URL {
	value, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return value
}
