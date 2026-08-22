package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/KingJammin/blazn/internal/proxycontract"
)

const requestID = "11111111-1111-4111-8111-111111111111"

func testPolicy() proxycontract.Policy {
	return proxycontract.Policy{Aliases: map[string]proxycontract.Alias{"company-assistant": {DataClass: proxycontract.DataCompany}}, RequestLimits: proxycontract.RequestLimits{MaxOutputTokens: 4096, TimeoutMS: 30000}}
}

type harnessFixture struct {
	Provenance struct{ Claim, CaptureStatus, Client, TargetVersion string } `json:"provenance"`
	Activation struct {
		Environment            map[string]string `json:"environment"`
		EndpointPrecedence     []string          `json:"endpointPrecedence"`
		ExpectedRequestHeaders map[string]string `json:"expectedRequestHeaders"`
	} `json:"activation"`
	Request           json.RawMessage `json:"request"`
	NonstreamResponse json.RawMessage `json:"nonstreamResponse"`
	SSETranscript     []struct {
		Event string          `json:"event"`
		Data  json.RawMessage `json:"data"`
	} `json:"sseTranscript"`
}

func loadHarnessFixture(t *testing.T) harnessFixture {
	t.Helper()
	encoded, err := os.ReadFile("testdata/claude-code-2.1.212-harness-shape.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture harnessFixture
	if json.Unmarshal(encoded, &fixture) != nil {
		t.Fatal("invalid fixture")
	}
	return fixture
}

func TestClaudeCode212ReproducibleHarnessShape(t *testing.T) {
	fixture := loadHarnessFixture(t)
	if fixture.Provenance.Claim != "reproducible_harness_shape_only" || fixture.Provenance.CaptureStatus != "not_captured" || fixture.Provenance.TargetVersion != "2.1.212" {
		t.Fatalf("fixture overclaims provenance: %#v", fixture.Provenance)
	}
	if fixture.Activation.Environment["ANTHROPIC_API_KEY"] != "<listener-token>" || fixture.Activation.Environment["ANTHROPIC_AUTH_TOKEN"] != "<listener-token>" || len(fixture.Activation.EndpointPrecedence) != 3 {
		t.Fatal("fixture omitted endpoint credential precedence")
	}
	header := http.Header{"Authorization": {"Bearer listener"}, "Anthropic-Version": {fixture.Activation.ExpectedRequestHeaders["anthropic-version"]}}
	if err := AuthenticateAndStrip(header, "listener"); err != nil {
		t.Fatal(err)
	}
	if header.Get("Anthropic-Version") != "2023-06-01" {
		t.Fatal("protocol version header was not retained")
	}
	body := bytes.NewReader(fixture.Request)
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	got, err := Normalize(body, testPolicy(), now, func() string { return requestID })
	if err != nil {
		t.Fatal(err)
	}
	if got.Protocol != proxycontract.ProtocolAnthropicMessages || got.ModelAlias != "company-assistant" || got.DataClass != proxycontract.DataCompany || !got.Stream {
		t.Fatalf("bad envelope: %#v", got)
	}
	if got.Limits.MaxOutputTokens != 2048 || got.Limits.DeadlineAt != "2026-08-22T12:00:30Z" || *got.Limits.Temperature != .2 || *got.Limits.TopP != .9 || !reflect.DeepEqual(got.Limits.Stop, []string{"<END>"}) {
		t.Fatalf("bad limits: %#v", got.Limits)
	}
	if len(got.Blocks) != 6 {
		t.Fatalf("got %d blocks: %#v", len(got.Blocks), got.Blocks)
	}
	wantTypes := []proxycontract.NormalizedBlockType{"text", "text", "text", "tool_call", "tool_result", "text"}
	wantRoles := []proxycontract.NormalizedRole{"system", "user", "assistant", "assistant", "tool", "user"}
	for index := range got.Blocks {
		if got.Blocks[index].Type != wantTypes[index] || got.Blocks[index].Role != wantRoles[index] {
			t.Fatalf("block %d: %#v", index, got.Blocks[index])
		}
	}
	if string(got.Blocks[3].Arguments) != `{"path":"README.md"}` || string(got.Blocks[4].Result) != `"# Blazn"` {
		t.Fatalf("tool translation lost data: %#v", got.Blocks)
	}
	if len(got.Tools) != 1 || got.Tools[0].Name != "read_file" || got.ToolChoice != proxycontract.ToolChoiceAuto {
		t.Fatalf("bad tools: %#v", got.Tools)
	}
	if !reflect.DeepEqual(got.CapabilitiesRequired, []proxycontract.Capability{"text", "tools", "streaming"}) {
		t.Fatalf("bad capabilities: %#v", got.CapabilitiesRequired)
	}
}

func TestHarnessShapeResponseAndSSEPreserveStopSequence(t *testing.T) {
	fixture := loadHarnessFixture(t)
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	request, err := Normalize(bytes.NewReader(fixture.Request), testPolicy(), now, func() string { return requestID })
	if err != nil {
		t.Fatal(err)
	}
	routeID := "22222222-2222-4222-8222-222222222222"
	response, metadata, err := DecodeDestinationDetailed(bytes.NewReader(fixture.NonstreamResponse), request, routeID)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.StopSequence == nil || *metadata.StopSequence != "<END>" {
		t.Fatalf("lost stop sequence: %#v", metadata)
	}
	if _, err := DecodeDestination(bytes.NewReader(fixture.NonstreamResponse), request, routeID); err == nil {
		t.Fatal("metadata-free path must fail closed")
	}
	var encoded bytes.Buffer
	if err := WriteSourceResponseDetailed(&encoded, response, metadata); err != nil {
		t.Fatal(err)
	}
	var source map[string]any
	if json.Unmarshal(encoded.Bytes(), &source) != nil || source["stop_reason"] != "stop_sequence" || source["stop_sequence"] != "<END>" {
		t.Fatalf("stop metadata lost: %s", encoded.String())
	}
	var transcript strings.Builder
	for _, item := range fixture.SSETranscript {
		fmt.Fprintf(&transcript, "event: %s\ndata: %s\n\n", item.Event, item.Data)
	}
	decoder := NewStreamDecoderForRequest(strings.NewReader(transcript.String()), request)
	for {
		_, done, err := decoder.Next()
		if err != nil {
			t.Fatal(err)
		}
		if done {
			break
		}
	}
	if decoder.StopSequence() == nil || *decoder.StopSequence() != "<END>" {
		t.Fatal("stream stop sequence lost")
	}
}

func TestStrictUnsupportedMatrix(t *testing.T) {
	base := `{"model":"company-assistant","max_tokens":1,"messages":[{"role":"user","content":[%s]}]}`
	tests := map[string]string{
		"image":         `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}}`,
		"thinking":      `{"type":"thinking","thinking":"secret","signature":"x"}`,
		"cache control": `{"type":"text","text":"x","cache_control":{"type":"ephemeral"}}`,
		"computer use":  `{"type":"tool_use","id":"x","name":"computer_20250124","input":{}}`,
	}
	for name, part := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Normalize(strings.NewReader(fmt.Sprintf(base, part)), testPolicy(), time.Now(), func() string { return requestID })
			if err == nil {
				t.Fatal("expected rejection")
			}
			var api *Error
			if !errors.As(err, &api) || api.Status != 400 {
				t.Fatalf("unexpected error: %v", err)
			}
			if api.Code != "unsupported_capability" {
				t.Fatalf("feature was not explicitly rejected: %#v", api)
			}
		})
	}
	for name, body := range map[string]string{
		"top level unknown": `{"model":"company-assistant","max_tokens":1,"messages":[{"role":"user","content":"x"}],"metadata":{"user_id":"x"}}`,
		"named tool choice": `{"model":"company-assistant","max_tokens":1,"messages":[{"role":"user","content":"x"}],"tools":[{"name":"a","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"a"}}`,
		"parallel disable":  `{"model":"company-assistant","max_tokens":1,"messages":[{"role":"user","content":"x"}],"tool_choice":{"type":"auto","disable_parallel_tool_use":true}}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Normalize(strings.NewReader(body), testPolicy(), time.Now(), func() string { return requestID })
			if err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestForbiddenNamesRemainValidInsideUserData(t *testing.T) {
	body := `{"model":"company-assistant","max_tokens":50,"messages":[{"role":"user","content":"go"},{"role":"assistant","content":[{"type":"tool_use","id":"call","name":"inspect","input":{"source":"s","image":"i","thinking":"t","cache_control":"c"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call","content":"{\"source\":\"s\",\"image\":\"i\",\"thinking\":\"t\",\"cache_control\":\"c\"}"}]}],"tools":[{"name":"inspect","input_schema":{"type":"object","properties":{"source":{"type":"string"},"image":{"type":"string"},"thinking":{"type":"string"},"cache_control":{"type":"string"}}}}]}`
	request, err := Normalize(strings.NewReader(body), testPolicy(), time.Now(), func() string { return requestID })
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Blocks) != 3 || string(request.Blocks[1].Arguments) != `{"source":"s","image":"i","thinking":"t","cache_control":"c"}` {
		t.Fatalf("user data changed: %#v", request.Blocks)
	}
}

func TestNonStringToolResultAndLateSystemFailClosed(t *testing.T) {
	body := `{"model":"company-assistant","max_tokens":10,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call","name":"inspect","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call","content":{"source":"value"}}]}]}`
	if _, err := Normalize(strings.NewReader(body), testPolicy(), time.Now(), func() string { return requestID }); err == nil {
		t.Fatal("non-string result must be rejected")
	}
	text := "x"
	request := proxycontract.NormalizedRequest{Blocks: []proxycontract.RequestBlock{{Role: "user", Type: "text", Text: &text}, {Role: "developer", Type: "text", Text: &text}}, Limits: proxycontract.NormalizedLimits{MaxOutputTokens: 1}}
	_, _, err := EncodeDestination(request, proxycontract.Route{DestinationProtocol: proxycontract.ProtocolAnthropicMessages})
	if err == nil {
		t.Fatal("late developer block must be rejected")
	}
}

func TestAuthenticateAndStrip(t *testing.T) {
	for name, header := range map[string]http.Header{
		"api key": {"X-Api-Key": {"secret"}, "Proxy-Authorization": {"x"}},
		"bearer":  {"Authorization": {"Bearer secret"}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := AuthenticateAndStrip(header, "secret"); err != nil {
				t.Fatal(err)
			}
			assertCredentialsStripped(t, header)
		})
	}
	for name, header := range map[string]http.Header{
		"wrong":     {"X-Api-Key": {"wrong"}},
		"ambiguous": {"X-Api-Key": {"secret"}, "Authorization": {"Bearer secret"}},
		"beta":      {"X-Api-Key": {"secret"}, "Anthropic-Beta": {"prompt-caching-2024-07-31"}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := AuthenticateAndStrip(header, "secret"); err == nil {
				t.Fatal("expected failure")
			}
			assertCredentialsStripped(t, header)
		})
	}
}

func assertCredentialsStripped(t *testing.T, header http.Header) {
	t.Helper()
	for _, name := range []string{"Authorization", "Proxy-Authorization", "X-Api-Key"} {
		if header.Get(name) != "" {
			t.Fatalf("%s survived", name)
		}
	}
}

func TestCrossProtocolEncodeDecodeIsLossless(t *testing.T) {
	text := "hello"
	callID, name := "call_1", "lookup"
	input := proxycontract.NormalizedRequest{LogicalRequestID: requestID, Protocol: proxycontract.ProtocolOpenAIResponses, ModelAlias: "company-assistant", DataClass: "company", Blocks: []proxycontract.RequestBlock{{Role: "developer", Type: "text", Text: &text}, {Role: "assistant", Type: "tool_call", CallID: &callID, ToolName: &name, Arguments: json.RawMessage(`{"q":"x"}`)}, {Role: "tool", Type: "tool_result", CallID: &callID, Result: json.RawMessage(`{"answer":1}`)}}, Tools: []proxycontract.Tool{{Name: name, InputSchema: map[string]any{"type": "object"}}}, ToolChoice: "required", Limits: proxycontract.NormalizedLimits{MaxOutputTokens: 50, DeadlineAt: "2026-08-22T12:00:30Z", Stop: []string{"done"}}, CapabilitiesRequired: []proxycontract.Capability{"text", "tools"}}
	route := proxycontract.Route{DestinationProtocol: proxycontract.ProtocolAnthropicMessages, Model: "claude-test"}
	body, path, err := EncodeDestination(input, route)
	if err != nil {
		t.Fatal(err)
	}
	if path != "messages" {
		t.Fatal(path)
	}
	var encoded map[string]any
	if json.Unmarshal(body, &encoded) != nil {
		t.Fatal("invalid JSON")
	}
	if encoded["model"] != "claude-test" {
		t.Fatalf("%s", body)
	}
	upstreamBody := `{"id":"msg_cloud","type":"message","role":"assistant","model":"claude-test","content":[{"type":"text","text":"ok"},{"type":"tool_use","id":"call_2","name":"lookup","input":{"q":"y"}}],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":9,"output_tokens":4}}`
	response, err := DecodeDestination(strings.NewReader(upstreamBody), input, "22222222-2222-4222-8222-222222222222")
	if err != nil {
		t.Fatal(err)
	}
	if response.FinishReason != "tool_call" || response.Usage.InputTokens != 9 || len(response.Blocks) != 2 || string(response.Blocks[1].Arguments) != `{"q":"y"}` {
		t.Fatalf("lost response data: %#v", response)
	}
	var source bytes.Buffer
	if err := WriteSourceResponse(&source, response); err != nil {
		t.Fatal(err)
	}
	var roundTrip map[string]any
	if json.Unmarshal(source.Bytes(), &roundTrip) != nil {
		t.Fatal("invalid source response")
	}
	if roundTrip["stop_reason"] != "tool_use" {
		t.Fatalf("%s", source.Bytes())
	}
}

func TestLocalCloudCompatibilityMatrix(t *testing.T) {
	base := proxycontract.NormalizedRequest{Protocol: proxycontract.ProtocolAnthropicMessages}
	for _, test := range []struct {
		name        string
		stream      bool
		destination proxycontract.Protocol
		wantError   bool
	}{
		{"local qwen nonstream", false, proxycontract.ProtocolOpenAIChat, false},
		{"cloud responses nonstream", false, proxycontract.ProtocolOpenAIResponses, false},
		{"native anthropic stream", true, proxycontract.ProtocolAnthropicMessages, false},
		{"local qwen stream", true, proxycontract.ProtocolOpenAIChat, true},
		{"cloud responses stream", true, proxycontract.ProtocolOpenAIResponses, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := base
			request.Stream = test.stream
			err := CheckCompatibility(request, test.destination)
			if (err != nil) != test.wantError {
				t.Fatalf("error=%v wantError=%v", err, test.wantError)
			}
		})
	}
}

func TestStopSequenceCrossProtocolFailsClosed(t *testing.T) {
	request := proxycontract.NormalizedRequest{Protocol: proxycontract.ProtocolAnthropicMessages, Limits: proxycontract.NormalizedLimits{Stop: []string{"END"}}}
	for _, destination := range []proxycontract.Protocol{proxycontract.ProtocolOpenAIChat, proxycontract.ProtocolOpenAIResponses} {
		if err := CheckCompatibility(request, destination); err == nil {
			t.Fatalf("%s accepted lossy stop sequence", destination)
		}
	}
}

func TestDestinationResponseStrictness(t *testing.T) {
	input := proxycontract.NormalizedRequest{LogicalRequestID: requestID, ModelAlias: "company-assistant"}
	for name, body := range map[string]string{
		"missing usage":          `{"id":"msg","type":"message","role":"assistant","model":"m","content":[],"stop_reason":"end_turn","stop_sequence":null}`,
		"unknown response field": `{"id":"msg","type":"message","role":"assistant","model":"m","content":[],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1},"cache_id":"x"}`,
		"mixed content union":    `{"id":"msg","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"x","id":"bad"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeDestination(strings.NewReader(body), input, "22222222-2222-4222-8222-222222222222"); err == nil {
				t.Fatal("expected strict rejection")
			}
		})
	}
}

func TestDestinationStreamMatrix(t *testing.T) {
	sse := strings.Join([]string{
		`event: message_start\ndata: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":7,"output_tokens":0}}}\n`,
		`event: content_block_start\ndata: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}\n`,
		`event: content_block_delta\ndata: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}\n`,
		`event: content_block_stop\ndata: {"type":"content_block_stop","index":0}\n`,
		`event: content_block_start\ndata: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"lookup","input":{}}}\n`,
		`event: content_block_delta\ndata: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"q\":\"x\"}"}}\n`,
		`event: content_block_stop\ndata: {"type":"content_block_stop","index":1}\n`,
		`event: message_delta\ndata: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":3}}\n`,
		`event: message_stop\ndata: {"type":"message_stop"}\n`,
	}, "\n")
	sse = strings.ReplaceAll(sse, `\n`, "\n")
	sse += "\n"
	decoder := NewStreamDecoder(strings.NewReader(sse), requestID)
	types := []proxycontract.StreamEventType{}
	for {
		event, done, err := decoder.Next()
		if err != nil {
			t.Fatal(err)
		}
		if done {
			break
		}
		types = append(types, event.Type)
	}
	want := []proxycontract.StreamEventType{"response_start", "text_delta", "tool_call_start", "tool_arguments_delta", "usage", "response_end"}
	if !reflect.DeepEqual(types, want) {
		t.Fatalf("got %v want %v", types, want)
	}
}

func TestSourceStreamOrderAndCancellation(t *testing.T) {
	request := proxycontract.NormalizedRequest{LogicalRequestID: requestID, ModelAlias: "company-assistant"}
	var out bytes.Buffer
	s := NewStreamEncoder(&out, request)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	text := "hi"
	callID, name, args := "toolu_1", "lookup", `{"q":"x"}`
	usage := proxycontract.Usage{InputTokens: 7, OutputTokens: 3}
	finish := proxycontract.FinishReason("tool_call")
	for index, event := range []proxycontract.NormalizedStreamEvent{{LogicalRequestID: requestID, Type: "response_start"}, {LogicalRequestID: requestID, Type: "text_delta", Text: &text}, {LogicalRequestID: requestID, Type: "tool_call_start", CallID: &callID, ToolName: &name}, {LogicalRequestID: requestID, Type: "tool_arguments_delta", CallID: &callID, ArgumentsDelta: &args}, {LogicalRequestID: requestID, Type: "usage", Usage: &usage}, {LogicalRequestID: requestID, Type: "response_end", FinishReason: &finish}} {
		event.Sequence = index
		if err := s.Consume(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Finish(); err != nil {
		t.Fatal(err)
	}
	encoded := out.String()
	order := []string{"event: message_start", "event: content_block_start", "event: content_block_delta", "event: content_block_stop", "event: content_block_start", "event: content_block_delta", "event: content_block_stop", "event: message_delta", "event: message_stop"}
	cursor := 0
	for _, needle := range order {
		next := strings.Index(encoded[cursor:], needle)
		if next < 0 {
			t.Fatalf("missing/out-of-order %q in %s", needle, encoded)
		}
		cursor += next + len(needle)
	}
	var cancelled bytes.Buffer
	c := NewStreamEncoder(&cancelled, request)
	_ = c.Start()
	if err := c.Cancel(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cancelled.String(), `event: error`) || !strings.Contains(cancelled.String(), `request was cancelled`) {
		t.Fatalf("bad cancellation: %s", cancelled.String())
	}
}

func TestStreamRejectsOrderAndSupportsContextCancellation(t *testing.T) {
	bad := `event: message_stop\ndata: {"type":"message_stop"}\n\n`
	_, _, err := NewStreamDecoder(strings.NewReader(bad), requestID).Next()
	if err == nil {
		t.Fatal("expected order failure")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err = NewStreamDecoder(strings.NewReader(""), requestID).NextContext(ctx)
	var api *Error
	if !errors.As(err, &api) || api.Code != "cancelled" {
		t.Fatalf("unexpected cancel: %v", err)
	}
}

func TestBlockedStreamReadIsCancelled(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	decoder := NewStreamDecoder(reader, requestID)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, _, err := decoder.NextContext(ctx)
	var api *Error
	if !errors.As(err, &api) || api.Code != "cancelled" {
		t.Fatalf("unexpected cancellation: %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("blocked read was not promptly unblocked")
	}
}

func TestDestinationStreamStrictStateAndBounds(t *testing.T) {
	start := `event: message_start\ndata: {"type":"message_start","message":{"id":"msg","type":"message","role":"assistant","content":[],"model":"m","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":0}}}\n\n`
	for name, suffix := range map[string]string{
		"post end":      `event: message_delta\ndata: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}\n\nevent: message_stop\ndata: {"type":"message_stop"}\n\nevent: ping\ndata: {"type":"ping"}\n\n`,
		"bad tool json": `event: content_block_start\ndata: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call","name":"tool","input":{}}}\n\nevent: content_block_delta\ndata: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{"}}\n\nevent: content_block_stop\ndata: {"type":"content_block_stop","index":0}\n\n`,
	} {
		t.Run(name, func(t *testing.T) {
			stream := strings.ReplaceAll(start+suffix, `\n`, "\n")
			decoder := NewStreamDecoder(strings.NewReader(stream), requestID)
			for {
				_, done, err := decoder.Next()
				if err != nil {
					return
				}
				if done {
					t.Fatal("invalid stream accepted")
				}
			}
		})
	}
	stream := strings.ReplaceAll(start, `\n`, "\n")
	decoder := NewStreamDecoder(strings.NewReader(stream), requestID)
	decoder.eventsRead = MaxStreamEvents
	if _, _, err := decoder.Next(); err == nil {
		t.Fatal("event limit was not enforced")
	}
}

func TestSourceStreamStateMachineRejectsDuplicateAndPostEnd(t *testing.T) {
	request := proxycontract.NormalizedRequest{LogicalRequestID: requestID, ModelAlias: "model"}
	encoder := NewStreamEncoder(io.Discard, request)
	_ = encoder.Start()
	input := 0
	usage := proxycontract.Usage{}
	finish := proxycontract.FinishReason("stop")
	start := proxycontract.NormalizedStreamEvent{LogicalRequestID: requestID, Sequence: input, Type: "response_start"}
	if err := encoder.Consume(start); err != nil {
		t.Fatal(err)
	}
	duplicate := start
	duplicate.Sequence = 1
	if err := encoder.Consume(duplicate); err == nil {
		t.Fatal("duplicate start accepted")
	}
	encoder = NewStreamEncoder(io.Discard, request)
	_ = encoder.Start()
	events := []proxycontract.NormalizedStreamEvent{{LogicalRequestID: requestID, Sequence: 0, Type: "response_start"}, {LogicalRequestID: requestID, Sequence: 1, Type: "usage", Usage: &usage}, {LogicalRequestID: requestID, Sequence: 2, Type: "response_end", FinishReason: &finish}}
	for _, event := range events {
		if err := encoder.Consume(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := encoder.Finish(); err != nil {
		t.Fatal(err)
	}
	late := "x"
	if err := encoder.Consume(proxycontract.NormalizedStreamEvent{LogicalRequestID: requestID, Sequence: 3, Type: "text_delta", Text: &late}); err == nil {
		t.Fatal("post-end event accepted")
	}
}

func TestSourceStreamRejectsInvalidTransitions(t *testing.T) {
	request := proxycontract.NormalizedRequest{LogicalRequestID: requestID, ModelAlias: "model"}
	text, callID, name, badArgs := "x", "call", "tool", "{"
	usage := proxycontract.Usage{}
	finish := proxycontract.FinishReason("stop")
	tests := map[string][]proxycontract.NormalizedStreamEvent{
		"nonzero start":         {{LogicalRequestID: requestID, Sequence: 1, Type: "response_start"}},
		"delta before start":    {{LogicalRequestID: requestID, Sequence: 0, Type: "text_delta", Text: &text}},
		"tool args before call": {{LogicalRequestID: requestID, Sequence: 0, Type: "response_start"}, {LogicalRequestID: requestID, Sequence: 1, Type: "tool_arguments_delta", CallID: &callID, ArgumentsDelta: &badArgs}},
		"end before usage":      {{LogicalRequestID: requestID, Sequence: 0, Type: "response_start"}, {LogicalRequestID: requestID, Sequence: 1, Type: "response_end", FinishReason: &finish}},
		"invalid tool json":     {{LogicalRequestID: requestID, Sequence: 0, Type: "response_start"}, {LogicalRequestID: requestID, Sequence: 1, Type: "tool_call_start", CallID: &callID, ToolName: &name}, {LogicalRequestID: requestID, Sequence: 2, Type: "tool_arguments_delta", CallID: &callID, ArgumentsDelta: &badArgs}, {LogicalRequestID: requestID, Sequence: 3, Type: "usage", Usage: &usage}},
		"sequence gap":          {{LogicalRequestID: requestID, Sequence: 0, Type: "response_start"}, {LogicalRequestID: requestID, Sequence: 2, Type: "usage", Usage: &usage}},
	}
	for testName, events := range tests {
		t.Run(testName, func(t *testing.T) {
			encoder := NewStreamEncoder(io.Discard, request)
			_ = encoder.Start()
			for index, event := range events {
				err := encoder.Consume(event)
				if index == len(events)-1 {
					if err == nil {
						t.Fatal("invalid transition accepted")
					}
					return
				}
				if err != nil {
					t.Fatalf("setup event %d: %v", index, err)
				}
			}
		})
	}
}

func TestAdapterInterface(t *testing.T) { var _ Adapter = MessagesAdapter{} }
