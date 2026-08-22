// Package anthropic implements the bounded Anthropic Messages protocol adapter
// used by the Blazn proxy. It exchanges only proxycontract normalized values;
// routing, credentials, fallback, and transport remain owned by the router.
package anthropic

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/blazncloud/blazn/internal/proxycontract"
)

const MaxRequestBytes = 8 << 20

// Error is safe to expose to a Messages client. Cause is never serialized.
type Error struct {
	Code      string
	Message   string
	Status    int
	Retryable bool
	Cause     error
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }
func (e *Error) Unwrap() error { return e.Cause }

func invalid(message string) error {
	return &Error{Code: "invalid_request", Message: message, Status: 400}
}
func unsupported(message string) error {
	return &Error{Code: "unsupported_capability", Message: message, Status: 400}
}
func upstream(message string) error {
	return &Error{Code: "upstream_invalid_response", Message: message, Status: 502}
}

// Adapter is the protocol boundary consumed by the router integration. It is
// deliberately expressed entirely in frozen proxycontract types.
type Adapter interface {
	Normalize(io.Reader, proxycontract.Policy, time.Time, func() string) (proxycontract.NormalizedRequest, error)
	EncodeDestination(proxycontract.NormalizedRequest, proxycontract.Route) ([]byte, string, error)
	DecodeDestination(io.Reader, proxycontract.NormalizedRequest, string) (DecodedResponse, error)
	NewDestinationStream(io.Reader, proxycontract.NormalizedRequest) DestinationStream
	WriteSourceResponse(io.Writer, DecodedResponse) error
	NewSourceStream(io.Writer, proxycontract.NormalizedRequest) SourceStream
}

type DestinationStream interface {
	NextContext(context.Context) (proxycontract.NormalizedStreamEvent, bool, error)
	Metadata() ResponseMetadata
}

type SourceStream interface {
	Start() error
	Consume(proxycontract.NormalizedStreamEvent) error
	SetMetadata(ResponseMetadata) error
	Finish() error
	Error(error) error
	Cancel() error
}

type MessagesAdapter struct{}

func (MessagesAdapter) Normalize(body io.Reader, policy proxycontract.Policy, now time.Time, id func() string) (proxycontract.NormalizedRequest, error) {
	return Normalize(body, policy, now, id)
}
func (MessagesAdapter) EncodeDestination(request proxycontract.NormalizedRequest, route proxycontract.Route) ([]byte, string, error) {
	return EncodeDestination(request, route)
}
func (MessagesAdapter) DecodeDestination(body io.Reader, request proxycontract.NormalizedRequest, routeID string) (DecodedResponse, error) {
	response, metadata, err := DecodeDestinationDetailed(body, request, routeID)
	return DecodedResponse{Normalized: response, Metadata: metadata}, err
}
func (MessagesAdapter) NewDestinationStream(body io.Reader, request proxycontract.NormalizedRequest) DestinationStream {
	return NewStreamDecoderForRequest(body, request)
}
func (MessagesAdapter) WriteSourceResponse(writer io.Writer, response DecodedResponse) error {
	return WriteSourceResponseDetailed(writer, response.Normalized, response.Metadata)
}
func (MessagesAdapter) NewSourceStream(writer io.Writer, request proxycontract.NormalizedRequest) SourceStream {
	return NewStreamEncoder(writer, request)
}

// AuthenticateAndStrip enforces the listener's single-source-credential rule,
// rejects unsupported Anthropic beta semantics, and always strips all source
// credentials before a caller can construct an upstream request.
func AuthenticateAndStrip(header http.Header, listenerToken string) error {
	defer stripSourceCredentials(header)
	for name := range header {
		lower := strings.ToLower(name)
		if lower == "anthropic-beta" || strings.Contains(lower, "prompt-cache") {
			return unsupported("Anthropic beta and prompt caching are unsupported")
		}
	}
	values := make([]string, 0, 2)
	for _, value := range header.Values("Authorization") {
		parts := strings.SplitN(value, " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			values = append(values, parts[1])
		}
	}
	values = append(values, header.Values("x-api-key")...)
	if len(values) != 1 || listenerToken == "" || subtle.ConstantTimeCompare([]byte(values[0]), []byte(listenerToken)) != 1 {
		return &Error{Code: "authentication_failed", Message: "listener authentication failed", Status: 401}
	}
	return nil
}

func stripSourceCredentials(header http.Header) {
	for name := range header {
		switch strings.ToLower(name) {
		case "authorization", "proxy-authorization", "x-api-key":
			header.Del(name)
		}
	}
}

type request struct {
	Model         string          `json:"model"`
	MaxTokens     int             `json:"max_tokens"`
	Messages      []message       `json:"messages"`
	System        json.RawMessage `json:"system,omitempty"`
	Tools         []tool          `json:"tools,omitempty"`
	ToolChoice    json.RawMessage `json:"tool_choice,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
}

type message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   *bool           `json:"is_error,omitempty"`
}

type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}

type toolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse *bool  `json:"disable_parallel_tool_use,omitempty"`
}

func Normalize(body io.Reader, policy proxycontract.Policy, now time.Time, newID func() string) (proxycontract.NormalizedRequest, error) {
	if newID == nil {
		return proxycontract.NormalizedRequest{}, errors.New("logical request ID generator is required")
	}
	raw, err := io.ReadAll(io.LimitReader(body, MaxRequestBytes+1))
	if err != nil {
		return proxycontract.NormalizedRequest{}, invalid("invalid Anthropic Messages request")
	}
	if len(raw) > MaxRequestBytes {
		return proxycontract.NormalizedRequest{}, &Error{Code: "invalid_request", Message: "request exceeds the body limit", Status: 413}
	}
	var topLevel map[string]json.RawMessage
	if json.Unmarshal(raw, &topLevel) == nil {
		if _, present := topLevel["thinking"]; present {
			return proxycontract.NormalizedRequest{}, unsupported("extended thinking is unsupported")
		}
	}
	var source request
	if err := decodeStrict(bytes.NewReader(raw), &source); err != nil {
		return proxycontract.NormalizedRequest{}, invalid("invalid Anthropic Messages request")
	}
	alias, ok := policy.Aliases[source.Model]
	if !ok {
		return proxycontract.NormalizedRequest{}, &Error{Code: "model_not_found", Message: "model alias is not defined by policy", Status: 404}
	}
	if source.MaxTokens <= 0 || source.MaxTokens > policy.RequestLimits.MaxOutputTokens {
		return proxycontract.NormalizedRequest{}, invalid("max_tokens is required and exceeds no policy limit")
	}
	if len(source.Messages) == 0 {
		return proxycontract.NormalizedRequest{}, invalid("messages must not be empty")
	}
	if source.Temperature != nil && (*source.Temperature < 0 || *source.Temperature > 1) {
		return proxycontract.NormalizedRequest{}, invalid("temperature must be between 0 and 1")
	}
	if source.TopP != nil && (*source.TopP < 0 || *source.TopP > 1) {
		return proxycontract.NormalizedRequest{}, invalid("top_p must be between 0 and 1")
	}
	for _, stop := range source.StopSequences {
		if stop == "" {
			return proxycontract.NormalizedRequest{}, invalid("stop_sequences cannot contain an empty value")
		}
	}

	blocks := []proxycontract.RequestBlock{}
	if len(source.System) > 0 && string(source.System) != "null" {
		texts, err := textContent(source.System)
		if err != nil {
			return proxycontract.NormalizedRequest{}, unsupported("system supports only text blocks")
		}
		for _, value := range texts {
			text := value
			blocks = append(blocks, proxycontract.RequestBlock{Role: "system", Type: "text", Text: &text})
		}
	}
	seenCalls := map[string]bool{}
	for _, sourceMessage := range source.Messages {
		if sourceMessage.Role != "user" && sourceMessage.Role != "assistant" {
			return proxycontract.NormalizedRequest{}, invalid("message role must be user or assistant")
		}
		parts, err := decodeContent(sourceMessage.Content)
		if err != nil || len(parts) == 0 {
			var api *Error
			if errors.As(err, &api) {
				return proxycontract.NormalizedRequest{}, api
			}
			return proxycontract.NormalizedRequest{}, invalid("message content must be text or supported content blocks")
		}
		for _, part := range parts {
			if err := validateContentBlock(part); err != nil {
				return proxycontract.NormalizedRequest{}, err
			}
			switch part.Type {
			case "text":
				text := part.Text
				blocks = append(blocks, proxycontract.RequestBlock{Role: proxycontract.NormalizedRole(sourceMessage.Role), Type: "text", Text: &text})
			case "tool_use":
				if strings.HasPrefix(strings.ToLower(part.Name), "computer_") || strings.Contains(strings.ToLower(part.Name), "computer-use") {
					return proxycontract.NormalizedRequest{}, unsupported("computer-use tools are unsupported")
				}
				if sourceMessage.Role != "assistant" || part.ID == "" || part.Name == "" || len(part.Input) == 0 || !isJSONObject(part.Input) {
					return proxycontract.NormalizedRequest{}, invalid("tool_use requires assistant role, id, name, and object input")
				}
				if seenCalls[part.ID] {
					return proxycontract.NormalizedRequest{}, invalid("tool_use id must be unique")
				}
				seenCalls[part.ID] = true
				blocks = append(blocks, proxycontract.RequestBlock{Role: "assistant", Type: "tool_call", CallID: ptr(part.ID), ToolName: ptr(part.Name), Arguments: compact(part.Input)})
			case "tool_result":
				if sourceMessage.Role != "user" || part.ToolUseID == "" || !seenCalls[part.ToolUseID] || part.IsError != nil && *part.IsError {
					return proxycontract.NormalizedRequest{}, unsupported("tool_result must reference a prior successful tool_use")
				}
				result, err := normalizeResult(part.Content)
				if err != nil {
					return proxycontract.NormalizedRequest{}, unsupported("tool_result content must be text, JSON, or text blocks")
				}
				blocks = append(blocks, proxycontract.RequestBlock{Role: "tool", Type: "tool_result", CallID: ptr(part.ToolUseID), Result: result})
			default:
				return proxycontract.NormalizedRequest{}, unsupported("multimodal, thinking, and computer-use content are unsupported")
			}
		}
	}

	tools := make([]proxycontract.Tool, 0, len(source.Tools))
	seenTools := map[string]bool{}
	for _, item := range source.Tools {
		if item.Name == "" || item.InputSchema == nil || seenTools[item.Name] {
			return proxycontract.NormalizedRequest{}, invalid("tools require unique names and input_schema")
		}
		if strings.HasPrefix(item.Name, "computer_") || strings.Contains(strings.ToLower(item.Name), "computer use") {
			return proxycontract.NormalizedRequest{}, unsupported("computer-use tools are unsupported")
		}
		seenTools[item.Name] = true
		tools = append(tools, proxycontract.Tool{Name: item.Name, Description: item.Description, InputSchema: item.InputSchema})
	}
	choice, err := normalizeToolChoice(source.ToolChoice, seenTools)
	if err != nil {
		return proxycontract.NormalizedRequest{}, err
	}
	capabilities := []proxycontract.Capability{proxycontract.CapabilityText}
	if len(tools) > 0 {
		capabilities = append(capabilities, proxycontract.CapabilityTools)
	}
	if source.Stream {
		capabilities = append(capabilities, proxycontract.CapabilityStreaming)
	}
	result := proxycontract.NormalizedRequest{
		LogicalRequestID: newID(), Protocol: proxycontract.ProtocolAnthropicMessages,
		ModelAlias: source.Model, DataClass: alias.DataClass, Stream: source.Stream,
		Blocks: blocks, Tools: tools, ToolChoice: choice,
		Limits:               proxycontract.NormalizedLimits{MaxOutputTokens: source.MaxTokens, DeadlineAt: now.Add(time.Duration(policy.RequestLimits.TimeoutMS) * time.Millisecond).UTC().Format(time.RFC3339), Temperature: source.Temperature, TopP: source.TopP, Stop: append([]string(nil), source.StopSequences...)},
		CapabilitiesRequired: capabilities,
	}
	if err := result.Validate(); err != nil {
		return proxycontract.NormalizedRequest{}, invalid("request is outside the supported Anthropic Messages subset")
	}
	return result, nil
}

func normalizeToolChoice(raw json.RawMessage, tools map[string]bool) (proxycontract.ToolChoice, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return proxycontract.ToolChoiceAuto, nil
	}
	var choice toolChoice
	if err := decodeStrict(bytes.NewReader(raw), &choice); err != nil {
		return "", unsupported("tool_choice is unsupported")
	}
	if choice.DisableParallelToolUse != nil && *choice.DisableParallelToolUse {
		return "", unsupported("disable_parallel_tool_use is not losslessly representable")
	}
	switch choice.Type {
	case "auto":
		return proxycontract.ToolChoiceAuto, nil
	case "none":
		return proxycontract.ToolChoiceNone, nil
	case "any":
		return proxycontract.ToolChoiceRequired, nil
	case "tool":
		if choice.Name == "" || !tools[choice.Name] {
			return "", invalid("tool_choice references an unknown tool")
		}
		return "", unsupported("forcing a named tool is not losslessly representable")
	default:
		return "", unsupported("tool_choice type is unsupported")
	}
}

func decodeContent(raw json.RawMessage) ([]contentBlock, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, errors.New("content cannot be null")
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []contentBlock{{Type: "text", Text: text}}, nil
	}
	var rawParts []json.RawMessage
	if err := json.Unmarshal(raw, &rawParts); err != nil {
		return nil, err
	}
	parts := make([]contentBlock, 0, len(rawParts))
	for _, rawPart := range rawParts {
		var header map[string]json.RawMessage
		if json.Unmarshal(rawPart, &header) != nil {
			return nil, errors.New("invalid content block")
		}
		var kind string
		_ = json.Unmarshal(header["type"], &kind)
		if _, present := header["cache_control"]; present {
			return nil, unsupported("prompt caching is unsupported")
		}
		switch kind {
		case "image", "document", "thinking", "redacted_thinking", "server_tool_use", "web_search_tool_result", "computer_tool_result":
			return nil, unsupported("multimodal, thinking, and computer-use content are unsupported")
		}
		var part contentBlock
		if err := decodeStrict(bytes.NewReader(rawPart), &part); err != nil {
			return nil, err
		}
		switch part.Type {
		case "text":
			if !hasExactKeys(rawPart, "type", "text") {
				return nil, errors.New("invalid text block")
			}
		case "tool_use":
			if !hasExactKeys(rawPart, "type", "id", "name", "input") {
				return nil, errors.New("invalid tool_use block")
			}
		case "tool_result":
			allowed := []string{"type", "tool_use_id", "content"}
			if _, ok := header["is_error"]; ok {
				allowed = append(allowed, "is_error")
			}
			if !hasExactKeys(rawPart, allowed...) {
				return nil, errors.New("invalid tool_result block")
			}
		}
		parts = append(parts, part)
	}
	return parts, nil
}

func validateContentBlock(part contentBlock) error {
	switch part.Type {
	case "text":
		if part.ID != "" || part.Name != "" || len(part.Input) != 0 || part.ToolUseID != "" || len(part.Content) != 0 || part.IsError != nil {
			return invalid("text content contains fields from another content type")
		}
	case "tool_use":
		if part.Text != "" || part.ToolUseID != "" || len(part.Content) != 0 || part.IsError != nil {
			return invalid("tool_use contains fields from another content type")
		}
	case "tool_result":
		if part.Text != "" || part.ID != "" || part.Name != "" || len(part.Input) != 0 {
			return invalid("tool_result contains fields from another content type")
		}
	}
	return nil
}

func textContent(raw json.RawMessage) ([]string, error) {
	parts, err := decodeContent(raw)
	if err != nil {
		return nil, err
	}
	if len(parts) == 0 {
		return nil, errors.New("content blocks cannot be empty")
	}
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		if part.Type != "text" || validateContentBlock(part) != nil {
			return nil, errors.New("non-text content")
		}
		values = append(values, part.Text)
	}
	return values, nil
}

func normalizeResult(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, errors.New("tool result content cannot be null")
	}
	if texts, err := textContent(raw); err == nil {
		encoded, _ := json.Marshal(strings.Join(texts, ""))
		return encoded, nil
	}
	return nil, errors.New("non-string tool result is not losslessly representable")
}

func decodeStrict(reader io.Reader, target any) error {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("JSON value has trailing data")
	}
	return nil
}

func compact(raw json.RawMessage) json.RawMessage {
	var buffer bytes.Buffer
	if json.Compact(&buffer, raw) != nil {
		return append(json.RawMessage(nil), raw...)
	}
	return buffer.Bytes()
}

func isJSONObject(raw json.RawMessage) bool {
	var value map[string]any
	return json.Unmarshal(raw, &value) == nil && value != nil
}

func ptr(value string) *string { return &value }

// EncodeDestination translates a normalized request to Anthropic Messages.
// It refuses values Anthropic cannot preserve rather than dropping them.
func EncodeDestination(input proxycontract.NormalizedRequest, route proxycontract.Route) ([]byte, string, error) {
	if route.DestinationProtocol != proxycontract.ProtocolAnthropicMessages {
		return nil, "", unsupported("destination protocol is not Anthropic Messages")
	}
	if input.ResponseSchema != nil {
		return nil, "", unsupported("structured output is not supported by Anthropic Messages")
	}
	system := []any{}
	messages := []any{}
	sawMessage := false
	for index := 0; index < len(input.Blocks); {
		block := input.Blocks[index]
		if block.Role == "system" || block.Role == "developer" {
			if sawMessage {
				return nil, "", unsupported("late system or developer content is not representable by Anthropic Messages")
			}
			if block.Type != "text" {
				return nil, "", unsupported("non-text system content is unsupported")
			}
			system = append(system, map[string]any{"type": "text", "text": *block.Text})
			index++
			continue
		}
		role := string(block.Role)
		sawMessage = true
		if role == "tool" {
			role = "user"
		}
		parts := []any{}
		for index < len(input.Blocks) {
			current := input.Blocks[index]
			currentRole := string(current.Role)
			if currentRole == "tool" {
				currentRole = "user"
			}
			if currentRole != role || current.Role == "system" || current.Role == "developer" {
				break
			}
			switch current.Type {
			case "text":
				parts = append(parts, map[string]any{"type": "text", "text": *current.Text})
			case "tool_call":
				parts = append(parts, map[string]any{"type": "tool_use", "id": *current.CallID, "name": *current.ToolName, "input": json.RawMessage(current.Arguments)})
			case "tool_result":
				content, err := resultContent(current.Result)
				if err != nil {
					return nil, "", err
				}
				parts = append(parts, map[string]any{"type": "tool_result", "tool_use_id": *current.CallID, "content": content})
			default:
				return nil, "", unsupported("normalized block cannot be represented by Anthropic Messages")
			}
			index++
		}
		messages = append(messages, map[string]any{"role": role, "content": parts})
	}
	tools := make([]any, 0, len(input.Tools))
	for _, item := range input.Tools {
		tools = append(tools, map[string]any{"name": item.Name, "description": item.Description, "input_schema": item.InputSchema})
	}
	body := map[string]any{"model": route.Model, "max_tokens": input.Limits.MaxOutputTokens, "messages": messages, "stream": input.Stream}
	if len(system) > 0 {
		body["system"] = system
	}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	switch input.ToolChoice {
	case proxycontract.ToolChoiceNone:
		body["tool_choice"] = map[string]any{"type": "none"}
	case proxycontract.ToolChoiceRequired:
		body["tool_choice"] = map[string]any{"type": "any"}
	case proxycontract.ToolChoiceAuto, "":
		if len(tools) > 0 {
			body["tool_choice"] = map[string]any{"type": "auto"}
		}
	default:
		return nil, "", unsupported("normalized tool choice cannot be represented")
	}
	if input.Limits.Temperature != nil {
		body["temperature"] = *input.Limits.Temperature
	}
	if input.Limits.TopP != nil {
		body["top_p"] = *input.Limits.TopP
	}
	if len(input.Limits.Stop) > 0 {
		body["stop_sequences"] = input.Limits.Stop
	}
	encoded, err := json.Marshal(body)
	return encoded, "messages", err
}

// CheckCompatibility is called before route commitment. The normalized
// envelope can represent Anthropic's bounded text/tool subset for every frozen
// protocol. It cannot preserve named-tool forcing, structured output, or
// Anthropic streaming input-usage timing through a non-Anthropic destination.
func CheckCompatibility(input proxycontract.NormalizedRequest, destination proxycontract.Protocol) error {
	if input.ResponseSchema != nil {
		return unsupported("structured output cannot be translated through Anthropic Messages")
	}
	if input.Protocol == proxycontract.ProtocolAnthropicMessages && input.Stream && destination != proxycontract.ProtocolAnthropicMessages {
		return unsupported("cross-protocol streaming cannot preserve Anthropic input usage timing")
	}
	if input.Protocol == proxycontract.ProtocolAnthropicMessages && len(input.Limits.Stop) > 0 && destination != proxycontract.ProtocolAnthropicMessages {
		return unsupported("cross-protocol routing cannot preserve the matched Anthropic stop_sequence")
	}
	switch destination {
	case proxycontract.ProtocolOpenAIChat, proxycontract.ProtocolOpenAIResponses, proxycontract.ProtocolAnthropicMessages:
		return nil
	default:
		return unsupported("destination protocol is unsupported")
	}
}

func resultContent(raw json.RawMessage) (any, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, unsupported("normalized null tool result is not representable by Anthropic Messages")
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil
	}
	return nil, unsupported("normalized non-string tool result is not losslessly representable by Anthropic Messages")
}

type response struct {
	ID         string            `json:"id"`
	Type       string            `json:"type"`
	Role       string            `json:"role"`
	Model      string            `json:"model"`
	Content    []json.RawMessage `json:"content"`
	StopReason string            `json:"stop_reason"`
	StopSeq    *string           `json:"stop_sequence"`
	Usage      usage             `json:"usage"`
}
type usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type ResponseMetadata struct {
	StopReason   string
	StopSequence *string
}

type DecodedResponse struct {
	Normalized proxycontract.NormalizedResponse
	Metadata   ResponseMetadata
}

func DecodeDestination(body io.Reader, input proxycontract.NormalizedRequest, routeID string) (proxycontract.NormalizedResponse, error) {
	result, metadata, err := DecodeDestinationDetailed(body, input, routeID)
	if err == nil && metadata.StopSequence != nil {
		return proxycontract.NormalizedResponse{}, unsupported("matched Anthropic stop_sequence requires adapter metadata")
	}
	return result, err
}

func DecodeDestinationDetailed(body io.Reader, input proxycontract.NormalizedRequest, routeID string) (proxycontract.NormalizedResponse, ResponseMetadata, error) {
	raw, err := io.ReadAll(io.LimitReader(body, MaxRequestBytes+1))
	if err != nil || len(raw) > MaxRequestBytes {
		return proxycontract.NormalizedResponse{}, ResponseMetadata{}, upstream("upstream returned an invalid Anthropic Messages response")
	}
	if !hasExactKeys(raw, "id", "type", "role", "model", "content", "stop_reason", "stop_sequence", "usage") {
		return proxycontract.NormalizedResponse{}, ResponseMetadata{}, upstream("upstream returned incomplete Anthropic message metadata")
	}
	var source response
	if err := decodeStrict(bytes.NewReader(raw), &source); err != nil || !hasExactKeys(source.UsageJSON(raw), "input_tokens", "output_tokens") {
		return proxycontract.NormalizedResponse{}, ResponseMetadata{}, upstream("upstream returned an invalid Anthropic Messages response")
	}
	if source.Type != "message" || source.Role != "assistant" || source.ID == "" || source.Model == "" {
		return proxycontract.NormalizedResponse{}, ResponseMetadata{}, upstream("upstream returned invalid Anthropic message metadata")
	}
	blocks := make([]proxycontract.ResponseBlock, 0, len(source.Content))
	sawTool := false
	for _, rawPart := range source.Content {
		var header struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(rawPart, &header) != nil {
			return proxycontract.NormalizedResponse{}, ResponseMetadata{}, upstream("upstream returned invalid Anthropic content")
		}
		if header.Type == "text" && !hasExactKeys(rawPart, "type", "text") || header.Type == "tool_use" && !hasExactKeys(rawPart, "type", "id", "name", "input") {
			return proxycontract.NormalizedResponse{}, ResponseMetadata{}, upstream("upstream returned invalid Anthropic content keys")
		}
		var part contentBlock
		if decodeStrict(bytes.NewReader(rawPart), &part) != nil {
			return proxycontract.NormalizedResponse{}, ResponseMetadata{}, upstream("upstream returned invalid Anthropic content")
		}
		if err := validateContentBlock(part); err != nil {
			return proxycontract.NormalizedResponse{}, ResponseMetadata{}, upstream("upstream returned invalid Anthropic content union")
		}
		switch part.Type {
		case "text":
			text := part.Text
			blocks = append(blocks, proxycontract.ResponseBlock{Type: "text", Text: &text})
		case "tool_use":
			if part.ID == "" || part.Name == "" || !isJSONObject(part.Input) {
				return proxycontract.NormalizedResponse{}, ResponseMetadata{}, upstream("upstream returned invalid tool_use content")
			}
			blocks = append(blocks, proxycontract.ResponseBlock{Type: "tool_call", CallID: ptr(part.ID), ToolName: ptr(part.Name), Arguments: compact(part.Input)})
			sawTool = true
		default:
			return proxycontract.NormalizedResponse{}, ResponseMetadata{}, upstream("upstream returned unsupported Anthropic content")
		}
	}
	if (source.StopReason == "tool_use") != sawTool {
		return proxycontract.NormalizedResponse{}, ResponseMetadata{}, upstream("tool_use stop_reason contradicts response content")
	}
	if err := validateStopSequence(source.StopReason, source.StopSeq, input.Limits.Stop); err != nil {
		return proxycontract.NormalizedResponse{}, ResponseMetadata{}, err
	}
	finish, err := finishReason(source.StopReason)
	if err != nil {
		return proxycontract.NormalizedResponse{}, ResponseMetadata{}, err
	}
	result := proxycontract.NormalizedResponse{LogicalRequestID: input.LogicalRequestID, ModelAlias: input.ModelAlias, RouteID: routeID, Blocks: blocks, FinishReason: finish, Usage: proxycontract.Usage{InputTokens: source.Usage.InputTokens, OutputTokens: source.Usage.OutputTokens}}
	if err := result.Validate(); err != nil {
		return proxycontract.NormalizedResponse{}, ResponseMetadata{}, upstream("upstream response failed normalization")
	}
	return result, ResponseMetadata{StopReason: source.StopReason, StopSequence: source.StopSeq}, nil
}

func (response) UsageJSON(raw json.RawMessage) json.RawMessage {
	var object map[string]json.RawMessage
	_ = json.Unmarshal(raw, &object)
	return object["usage"]
}

func finishReason(reason string) (proxycontract.FinishReason, error) {
	switch reason {
	case "end_turn", "stop_sequence":
		return "stop", nil
	case "max_tokens":
		return "length", nil
	case "tool_use":
		return "tool_call", nil
	case "refusal":
		return "content_filter", nil
	default:
		return "", upstream("upstream returned an unsupported stop_reason")
	}
}

func validateStopSequence(reason string, sequence *string, requested []string) error {
	if reason == "stop_sequence" {
		if sequence == nil || *sequence == "" {
			return upstream("stop_sequence reason omitted the matched sequence")
		}
		for _, candidate := range requested {
			if candidate == *sequence {
				return nil
			}
		}
		return upstream("upstream reported an unrequested stop_sequence")
	}
	if sequence != nil {
		return upstream("stop_sequence must be null for this stop_reason")
	}
	return nil
}

func WriteSourceResponse(writer io.Writer, input proxycontract.NormalizedResponse) error {
	return WriteSourceResponseDetailed(writer, input, ResponseMetadata{})
}

func WriteSourceResponseDetailed(writer io.Writer, input proxycontract.NormalizedResponse, metadata ResponseMetadata) error {
	if metadata.StopReason == "stop_sequence" && metadata.StopSequence == nil {
		return unsupported("matched stop_sequence metadata is incomplete")
	}
	if metadata.StopReason != "" && metadata.StopReason != "stop_sequence" {
		mapped, err := finishReason(metadata.StopReason)
		if err != nil || mapped != input.FinishReason {
			return unsupported("Anthropic stop metadata contradicts normalized finish reason")
		}
	}
	content := make([]any, 0, len(input.Blocks))
	for _, block := range input.Blocks {
		switch block.Type {
		case "text":
			content = append(content, map[string]any{"type": "text", "text": *block.Text})
		case "tool_call":
			content = append(content, map[string]any{"type": "tool_use", "id": *block.CallID, "name": *block.ToolName, "input": json.RawMessage(block.Arguments)})
		default:
			return unsupported("normalized response block cannot be represented by Anthropic Messages")
		}
	}
	stop := map[proxycontract.FinishReason]string{"stop": "end_turn", "length": "max_tokens", "tool_call": "tool_use", "content_filter": "refusal"}[input.FinishReason]
	stopSequence := any(nil)
	if metadata.StopSequence != nil {
		if input.FinishReason != "stop" || metadata.StopReason != "stop_sequence" {
			return unsupported("Anthropic stop metadata contradicts normalized finish reason")
		}
		stop = "stop_sequence"
		stopSequence = *metadata.StopSequence
	}
	if stop == "" {
		return unsupported("normalized finish reason cannot be represented by Anthropic Messages")
	}
	return json.NewEncoder(writer).Encode(map[string]any{"id": "msg_" + input.LogicalRequestID, "type": "message", "role": "assistant", "model": input.ModelAlias, "content": content, "stop_reason": stop, "stop_sequence": stopSequence, "usage": map[string]any{"input_tokens": input.Usage.InputTokens, "output_tokens": input.Usage.OutputTokens}})
}

func WriteError(writer io.Writer, err error) error {
	api := &Error{Code: "internal_error", Message: "proxy request failed", Status: 500}
	if !errors.As(err, &api) {
		api = &Error{Code: "internal_error", Message: "proxy request failed", Status: 500}
	}
	typeName := "api_error"
	switch api.Code {
	case "authentication_failed":
		typeName = "authentication_error"
	case "invalid_request", "unsupported_capability":
		typeName = "invalid_request_error"
	case "model_not_found":
		typeName = "not_found_error"
	case "rate_limited":
		typeName = "rate_limit_error"
	case "upstream_5xx", "upstream_invalid_response", "connection_failure", "timeout_before_first_byte":
		typeName = "api_error"
	}
	return json.NewEncoder(writer).Encode(map[string]any{"type": "error", "error": map[string]any{"type": typeName, "message": api.Message}})
}
