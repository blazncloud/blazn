package router

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/blazncloud/blazn/internal/proxycontract"
)

type chatRequest struct {
	Model               string          `json:"model"`
	Messages            []chatMessage   `json:"messages"`
	Stream              bool            `json:"stream,omitempty"`
	MaxTokens           int             `json:"max_tokens,omitempty"`
	MaxCompletionTokens int             `json:"max_completion_tokens,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	Stop                json.RawMessage `json:"stop,omitempty"`
	Tools               []chatTool      `json:"tools,omitempty"`
	ToolChoice          json.RawMessage `json:"tool_choice,omitempty"`
	ResponseFormat      json.RawMessage `json:"response_format,omitempty"`
	StreamOptions       json.RawMessage `json:"stream_options,omitempty"`
}

type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	ToolCalls  []chatToolCall  `json:"tool_calls,omitempty"`
}
type chatTool struct {
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}
type chatFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
}
type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

type responsesRequest struct {
	Model             string          `json:"model"`
	Instructions      string          `json:"instructions,omitempty"`
	Input             json.RawMessage `json:"input"`
	Stream            bool            `json:"stream,omitempty"`
	MaxOutputTokens   int             `json:"max_output_tokens,omitempty"`
	Temperature       *float64        `json:"temperature,omitempty"`
	TopP              *float64        `json:"top_p,omitempty"`
	Tools             []responseTool  `json:"tools,omitempty"`
	ToolChoice        json.RawMessage `json:"tool_choice,omitempty"`
	Text              json.RawMessage `json:"text,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
	Store             *bool           `json:"store,omitempty"`
	Include           []string        `json:"include,omitempty"`
	Reasoning         json.RawMessage `json:"reasoning,omitempty"`
	StreamOptions     json.RawMessage `json:"stream_options,omitempty"`
	ServiceTier       string          `json:"service_tier,omitempty"`
	PromptCacheKey    string          `json:"prompt_cache_key,omitempty"`
	ClientMetadata    json.RawMessage `json:"client_metadata,omitempty"`
}
type responseTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
}
type responseInput struct {
	Type             string          `json:"type,omitempty"`
	Role             string          `json:"role,omitempty"`
	Content          json.RawMessage `json:"content,omitempty"`
	CallID           string          `json:"call_id,omitempty"`
	Name             string          `json:"name,omitempty"`
	Arguments        json.RawMessage `json:"arguments,omitempty"`
	Output           json.RawMessage `json:"output,omitempty"`
	ID               string          `json:"id,omitempty"`
	Summary          json.RawMessage `json:"summary,omitempty"`
	EncryptedContent string          `json:"encrypted_content,omitempty"`
	Status           string          `json:"status,omitempty"`
}

type sourceMetadata struct {
	responses            *responsesMetadata
	responseSchemaName   string
	responseSchemaStrict bool
}

type responsesMetadata struct {
	instructions      string
	originalInput     json.RawMessage
	parallelToolCalls *bool
	store             *bool
	include           []string
	reasoning         json.RawMessage
	streamOptions     json.RawMessage
	serviceTier       string
	promptCacheKey    string
	clientMetadata    json.RawMessage
	reasoningItems    []json.RawMessage
}

type routedRequest struct {
	normalized proxycontract.NormalizedRequest
	source     sourceMetadata
}

func decodeStrictJSON[T any](body io.Reader, target *T) error {
	decoder := json.NewDecoder(io.LimitReader(body, maxRequestBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("request has trailing JSON data")
	}
	return nil
}

func normalizeChat(body io.Reader, policy proxycontract.Policy, now time.Time) (proxycontract.NormalizedRequest, error) {
	var source chatRequest
	if err := decodeStrictJSON(body, &source); err != nil {
		return proxycontract.NormalizedRequest{}, safeError("invalid_request", "invalid OpenAI Chat request", 400, false)
	}
	alias, ok := policy.Aliases[source.Model]
	if !ok {
		return proxycontract.NormalizedRequest{}, safeError("model_not_found", "model alias is not defined by policy", 404, false)
	}
	if len(source.Messages) == 0 {
		return proxycontract.NormalizedRequest{}, safeError("invalid_request", "messages must not be empty", 400, false)
	}
	if source.MaxTokens > 0 && source.MaxCompletionTokens > 0 {
		return proxycontract.NormalizedRequest{}, safeError("invalid_request", "max_tokens and max_completion_tokens are mutually exclusive", 400, false)
	}
	if err := validateStreamOptions(source.Stream, source.StreamOptions); err != nil {
		return proxycontract.NormalizedRequest{}, err
	}
	maxOutput := source.MaxCompletionTokens
	if maxOutput == 0 {
		maxOutput = source.MaxTokens
	}
	if maxOutput == 0 {
		maxOutput = policy.RequestLimits.MaxOutputTokens
	}
	blocks := make([]proxycontract.RequestBlock, 0, len(source.Messages))
	for _, message := range source.Messages {
		if len(message.ToolCalls) > 0 {
			if message.Role != "assistant" {
				return proxycontract.NormalizedRequest{}, unsupported("tool calls require an assistant message")
			}
			if len(message.Content) > 0 && string(message.Content) != "null" {
				texts, contentErr := rawTexts(message.Content, "text")
				if contentErr != nil {
					return proxycontract.NormalizedRequest{}, unsupported("multimodal assistant tool content is unsupported")
				}
				for _, text := range texts {
					textCopy := text
					blocks = append(blocks, proxycontract.RequestBlock{Role: "assistant", Type: "text", Text: &textCopy})
				}
			}
			for _, call := range message.ToolCalls {
				if call.Type != "function" {
					return proxycontract.NormalizedRequest{}, unsupported("only function tool calls are supported")
				}
				arguments := normalizeJSONValue(call.Function.Arguments)
				blocks = append(blocks, proxycontract.RequestBlock{Role: "assistant", Type: "tool_call", CallID: stringPtr(call.ID), ToolName: stringPtr(call.Function.Name), Arguments: arguments})
			}
			continue
		}
		if message.Role == "tool" {
			result, err := rawContentValue(message.Content)
			if err != nil {
				return proxycontract.NormalizedRequest{}, unsupported("tool result must be text or JSON")
			}
			blocks = append(blocks, proxycontract.RequestBlock{Role: "tool", Type: "tool_result", CallID: stringPtr(message.ToolCallID), Result: result})
			continue
		}
		texts, err := rawTexts(message.Content, "text")
		if err != nil {
			return proxycontract.NormalizedRequest{}, unsupported("multimodal message content is unsupported")
		}
		role := proxycontract.NormalizedRole(message.Role)
		for _, text := range texts {
			textCopy := text
			blocks = append(blocks, proxycontract.RequestBlock{Role: role, Type: "text", Text: &textCopy})
		}
	}
	tools := make([]proxycontract.Tool, 0, len(source.Tools))
	for _, tool := range source.Tools {
		if tool.Type != "function" {
			return proxycontract.NormalizedRequest{}, unsupported("only function tools are supported")
		}
		tools = append(tools, proxycontract.Tool{Name: tool.Function.Name, Description: tool.Function.Description, InputSchema: tool.Function.Parameters})
	}
	choice, err := parseToolChoice(source.ToolChoice)
	if err != nil {
		return proxycontract.NormalizedRequest{}, err
	}
	responseSchema, err := parseChatResponseFormat(source.ResponseFormat)
	if err != nil {
		return proxycontract.NormalizedRequest{}, err
	}
	stop, err := parseStop(source.Stop)
	if err != nil {
		return proxycontract.NormalizedRequest{}, err
	}
	capabilities := []proxycontract.Capability{proxycontract.CapabilityText}
	if len(tools) > 0 {
		capabilities = append(capabilities, proxycontract.CapabilityTools)
	}
	if responseSchema != nil {
		capabilities = append(capabilities, proxycontract.CapabilityStructuredOutput)
	}
	if source.Stream {
		capabilities = append(capabilities, proxycontract.CapabilityStreaming)
	}
	request := proxycontract.NormalizedRequest{LogicalRequestID: newUUID(), Protocol: proxycontract.ProtocolOpenAIChat, ModelAlias: source.Model, DataClass: alias.DataClass, Stream: source.Stream, Blocks: blocks, Tools: tools, ToolChoice: choice, ResponseSchema: responseSchema, Limits: proxycontract.NormalizedLimits{MaxOutputTokens: maxOutput, DeadlineAt: now.Add(time.Duration(policy.RequestLimits.TimeoutMS) * time.Millisecond).UTC().Format(time.RFC3339), Temperature: source.Temperature, TopP: source.TopP, Stop: stop}, CapabilitiesRequired: capabilities}
	if err := request.Validate(); err != nil {
		return proxycontract.NormalizedRequest{}, safeError("invalid_request", "request is outside the supported Chat subset", 400, false)
	}
	return request, nil
}

func normalizeResponses(body io.Reader, policy proxycontract.Policy, now time.Time) (proxycontract.NormalizedRequest, error) {
	var source responsesRequest
	if err := decodeStrictJSON(body, &source); err != nil {
		return proxycontract.NormalizedRequest{}, safeError("invalid_request", "invalid OpenAI Responses request", 400, false)
	}
	alias, ok := policy.Aliases[source.Model]
	if !ok {
		return proxycontract.NormalizedRequest{}, safeError("model_not_found", "model alias is not defined by policy", 404, false)
	}
	blocks := []proxycontract.RequestBlock{}
	if source.Instructions != "" {
		text := source.Instructions
		blocks = append(blocks, proxycontract.RequestBlock{Role: "developer", Type: "text", Text: &text})
	}
	if len(source.Input) == 0 {
		return proxycontract.NormalizedRequest{}, safeError("invalid_request", "input is required", 400, false)
	}
	if text, err := rawText(source.Input); err == nil {
		blocks = append(blocks, proxycontract.RequestBlock{Role: "user", Type: "text", Text: &text})
	} else {
		var inputs []responseInput
		if err := decodeRawStrict(source.Input, &inputs); err != nil {
			return proxycontract.NormalizedRequest{}, unsupported("Responses input type is unsupported")
		}
		for _, input := range inputs {
			if input.Status != "" && input.Status != "completed" {
				return proxycontract.NormalizedRequest{}, unsupported("only completed historical Responses items are supported")
			}
			switch input.Type {
			case "", "message":
				texts, err := rawTexts(input.Content, "input_text", "output_text")
				if err != nil {
					return proxycontract.NormalizedRequest{}, unsupported("multimodal Responses content is unsupported")
				}
				for _, text := range texts {
					textCopy := text
					blocks = append(blocks, proxycontract.RequestBlock{Role: proxycontract.NormalizedRole(input.Role), Type: "text", Text: &textCopy})
				}
			case "function_call":
				blocks = append(blocks, proxycontract.RequestBlock{Role: "assistant", Type: "tool_call", CallID: stringPtr(input.CallID), ToolName: stringPtr(input.Name), Arguments: normalizeJSONValue(input.Arguments)})
			case "function_call_output":
				blocks = append(blocks, proxycontract.RequestBlock{Role: "tool", Type: "tool_result", CallID: stringPtr(input.CallID), Result: normalizeJSONValue(input.Output)})
			case "reasoning":
				if input.ID == "" {
					return proxycontract.NormalizedRequest{}, unsupported("reasoning item requires an id")
				}
			default:
				return proxycontract.NormalizedRequest{}, unsupported("Responses input item type is unsupported")
			}
		}
	}
	tools := make([]proxycontract.Tool, 0, len(source.Tools))
	for _, tool := range source.Tools {
		if tool.Type != "function" {
			return proxycontract.NormalizedRequest{}, unsupported("only function tools are supported")
		}
		tools = append(tools, proxycontract.Tool{Name: tool.Name, Description: tool.Description, InputSchema: tool.Parameters})
	}
	choice, err := parseToolChoice(source.ToolChoice)
	if err != nil {
		return proxycontract.NormalizedRequest{}, err
	}
	responseSchema, err := parseResponsesText(source.Text)
	if err != nil {
		return proxycontract.NormalizedRequest{}, err
	}
	maxOutput := source.MaxOutputTokens
	if maxOutput == 0 {
		maxOutput = policy.RequestLimits.MaxOutputTokens
	}
	capabilities := []proxycontract.Capability{proxycontract.CapabilityText}
	if len(tools) > 0 {
		capabilities = append(capabilities, proxycontract.CapabilityTools)
	}
	if responseSchema != nil {
		capabilities = append(capabilities, proxycontract.CapabilityStructuredOutput)
	}
	if source.Stream {
		capabilities = append(capabilities, proxycontract.CapabilityStreaming)
	}
	request := proxycontract.NormalizedRequest{LogicalRequestID: newUUID(), Protocol: proxycontract.ProtocolOpenAIResponses, ModelAlias: source.Model, DataClass: alias.DataClass, Stream: source.Stream, Blocks: blocks, Tools: tools, ToolChoice: choice, ResponseSchema: responseSchema, Limits: proxycontract.NormalizedLimits{MaxOutputTokens: maxOutput, DeadlineAt: now.Add(time.Duration(policy.RequestLimits.TimeoutMS) * time.Millisecond).UTC().Format(time.RFC3339), Temperature: source.Temperature, TopP: source.TopP}, CapabilitiesRequired: capabilities}
	if err := request.Validate(); err != nil {
		return proxycontract.NormalizedRequest{}, safeError("invalid_request", "request is outside the supported Responses subset", 400, false)
	}
	return request, nil
}

func normalizeChatIncoming(body io.Reader, policy proxycontract.Policy, now time.Time) (routedRequest, error) {
	raw, err := io.ReadAll(io.LimitReader(body, maxRequestBytes+1))
	if err != nil {
		return routedRequest{}, err
	}
	if len(raw) > maxRequestBytes {
		return routedRequest{}, safeError("invalid_request", "request exceeds the body limit", 413, false)
	}
	normalized, err := normalizeChat(bytes.NewReader(raw), policy, now)
	if err != nil {
		return routedRequest{}, err
	}
	var source chatRequest
	if err = decodeStrictJSON(bytes.NewReader(raw), &source); err != nil {
		return routedRequest{}, safeError("invalid_request", "invalid OpenAI Chat request", 400, false)
	}
	name, strict, err := inspectChatSchema(source.ResponseFormat)
	if err != nil {
		return routedRequest{}, err
	}
	return routedRequest{normalized: normalized, source: sourceMetadata{responseSchemaName: name, responseSchemaStrict: strict}}, nil
}

func normalizeResponsesIncoming(body io.Reader, policy proxycontract.Policy, now time.Time) (routedRequest, error) {
	raw, err := io.ReadAll(io.LimitReader(body, maxRequestBytes+1))
	if err != nil {
		return routedRequest{}, err
	}
	if len(raw) > maxRequestBytes {
		return routedRequest{}, safeError("invalid_request", "request exceeds the body limit", 413, false)
	}
	normalized, err := normalizeResponses(bytes.NewReader(raw), policy, now)
	if err != nil {
		return routedRequest{}, err
	}
	var source responsesRequest
	if err = decodeStrictJSON(bytes.NewReader(raw), &source); err != nil {
		return routedRequest{}, safeError("invalid_request", "invalid OpenAI Responses request", 400, false)
	}
	metadata, err := inspectResponsesMetadata(source)
	if err != nil {
		return routedRequest{}, err
	}
	name, strict, err := inspectResponsesSchema(source.Text)
	if err != nil {
		return routedRequest{}, err
	}
	metadata.reasoningItems, err = extractReasoningItems(source.Input)
	if err != nil {
		return routedRequest{}, err
	}
	return routedRequest{normalized: normalized, source: sourceMetadata{responses: &metadata, responseSchemaName: name, responseSchemaStrict: strict}}, nil
}

func inspectResponsesMetadata(source responsesRequest) (responsesMetadata, error) {
	metadata := responsesMetadata{instructions: source.Instructions, originalInput: append(json.RawMessage(nil), source.Input...), parallelToolCalls: source.ParallelToolCalls, store: source.Store, include: append([]string(nil), source.Include...), reasoning: append(json.RawMessage(nil), source.Reasoning...), streamOptions: append(json.RawMessage(nil), source.StreamOptions...), serviceTier: source.ServiceTier, promptCacheKey: source.PromptCacheKey, clientMetadata: append(json.RawMessage(nil), source.ClientMetadata...)}
	if source.Store != nil && *source.Store {
		return metadata, unsupported("store=true is forbidden by the proxy data-retention policy")
	}
	for _, item := range source.Include {
		if item != "reasoning.encrypted_content" {
			return metadata, unsupported("Responses include value is unsupported")
		}
	}
	if len(source.Reasoning) > 0 && string(source.Reasoning) != "null" {
		var value struct {
			Effort  string `json:"effort,omitempty"`
			Summary string `json:"summary,omitempty"`
		}
		if err := decodeRawStrict(source.Reasoning, &value); err != nil {
			return metadata, unsupported("Responses reasoning options are unsupported")
		}
		if value.Effort != "" && value.Effort != "none" && value.Effort != "minimal" && value.Effort != "low" && value.Effort != "medium" && value.Effort != "high" && value.Effort != "xhigh" {
			return metadata, unsupported("Responses reasoning effort is unsupported")
		}
		if value.Summary != "" && value.Summary != "auto" && value.Summary != "concise" && value.Summary != "detailed" {
			return metadata, unsupported("Responses reasoning summary is unsupported")
		}
	}
	if len(source.StreamOptions) > 0 && string(source.StreamOptions) != "null" {
		var value struct {
			IncludeObfuscation bool `json:"include_obfuscation"`
		}
		if err := decodeRawStrict(source.StreamOptions, &value); err != nil {
			return metadata, unsupported("Responses stream_options are unsupported")
		}
	}
	if source.ServiceTier != "" && source.ServiceTier != "auto" && source.ServiceTier != "default" {
		return metadata, unsupported("Responses service_tier is unsupported")
	}
	if len(source.PromptCacheKey) > 256 {
		return metadata, safeError("invalid_request", "prompt_cache_key is too long", 400, false)
	}
	if len(source.ClientMetadata) > 0 && string(source.ClientMetadata) != "null" {
		var value map[string]string
		if err := decodeRawStrict(source.ClientMetadata, &value); err != nil || len(value) > 16 {
			return metadata, unsupported("client_metadata must be a small string map")
		}
		for key, item := range value {
			if len(key) > 64 || len(item) > 512 {
				return metadata, safeError("invalid_request", "client_metadata entry is too long", 400, false)
			}
		}
	}
	return metadata, nil
}

func extractReasoningItems(raw json.RawMessage) ([]json.RawMessage, error) {
	var inputs []json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &inputs) != nil {
		return nil, nil
	}
	out := []json.RawMessage{}
	for _, item := range inputs {
		var header struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(item, &header) != nil {
			continue
		}
		if header.Type == "reasoning" {
			var value responseInput
			if err := decodeRawStrict(item, &value); err != nil {
				return nil, unsupported("reasoning input item is unsupported")
			}
			out = append(out, append(json.RawMessage(nil), item...))
		}
	}
	return out, nil
}

func inspectChatSchema(raw json.RawMessage) (string, bool, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", false, nil
	}
	var value struct {
		Type       string `json:"type"`
		JSONSchema struct {
			Name   string         `json:"name"`
			Schema map[string]any `json:"schema"`
			Strict bool           `json:"strict,omitempty"`
		} `json:"json_schema"`
	}
	if err := decodeRawStrict(raw, &value); err != nil {
		return "", false, unsupported("response_format is unsupported")
	}
	if value.Type != "json_schema" {
		return "", false, nil
	}
	if value.JSONSchema.Name == "" {
		return "", false, safeError("invalid_request", "json_schema name is required", 400, false)
	}
	return value.JSONSchema.Name, value.JSONSchema.Strict, nil
}

func inspectResponsesSchema(raw json.RawMessage) (string, bool, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", false, nil
	}
	var value struct {
		Format struct {
			Type   string         `json:"type"`
			Name   string         `json:"name"`
			Schema map[string]any `json:"schema"`
			Strict bool           `json:"strict,omitempty"`
		} `json:"format"`
	}
	if err := decodeRawStrict(raw, &value); err != nil {
		return "", false, unsupported("Responses text format is unsupported")
	}
	if value.Format.Type != "json_schema" {
		return "", false, nil
	}
	if value.Format.Name == "" {
		return "", false, safeError("invalid_request", "json_schema name is required", 400, false)
	}
	return value.Format.Name, value.Format.Strict, nil
}

func unsupported(message string) error {
	return safeError("unsupported_capability", message, 400, false)
}
func stringPtr(value string) *string { return &value }
func decodeRawStrict[T any](raw json.RawMessage, target *T) error {
	return decodeStrictJSON(bytes.NewReader(raw), target)
}
func rawText(raw json.RawMessage) (string, error) {
	var text string
	if len(raw) == 0 {
		return "", nil
	}
	if err := json.Unmarshal(raw, &text); err != nil {
		return "", err
	}
	return text, nil
}

func rawTexts(raw json.RawMessage, allowedTypes ...string) ([]string, error) {
	if text, err := rawText(raw); err == nil {
		return []string{text}, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := decodeRawStrict(raw, &parts); err != nil || len(parts) == 0 {
		return nil, errors.New("content is not text")
	}
	allowed := map[string]bool{}
	for _, kind := range allowedTypes {
		allowed[kind] = true
	}
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		if !allowed[part.Type] {
			return nil, errors.New("content contains unsupported type")
		}
		texts = append(texts, part.Text)
	}
	return texts, nil
}
func rawContentValue(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, errors.New("missing")
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return normalizeJSONValue(raw), nil
}
func normalizeJSONValue(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("null")
	}
	var text string
	if json.Unmarshal(raw, &text) == nil && json.Valid([]byte(text)) {
		return json.RawMessage(text)
	}
	return append(json.RawMessage(nil), raw...)
}
func parseStop(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return []string{one}, nil
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		return many, nil
	}
	return nil, safeError("invalid_request", "stop must be a string or string array", 400, false)
}

func validateStreamOptions(stream bool, raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if !stream {
		return safeError("invalid_request", "stream_options requires streaming", 400, false)
	}
	var options struct {
		IncludeUsage bool `json:"include_usage"`
	}
	if err := decodeRawStrict(raw, &options); err != nil || !options.IncludeUsage {
		return unsupported("only stream_options.include_usage=true is supported")
	}
	return nil
}
func parseToolChoice(raw json.RawMessage) (proxycontract.ToolChoice, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return proxycontract.ToolChoiceAuto, nil
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return "", unsupported("named tool choice is unsupported")
	}
	switch value {
	case "none", "auto", "required":
		return proxycontract.ToolChoice(value), nil
	}
	return "", unsupported("tool choice is unsupported")
}
func parseChatResponseFormat(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var value struct {
		Type       string `json:"type"`
		JSONSchema struct {
			Name   string         `json:"name"`
			Schema map[string]any `json:"schema"`
			Strict bool           `json:"strict,omitempty"`
		} `json:"json_schema"`
	}
	if err := decodeRawStrict(raw, &value); err != nil {
		return nil, unsupported("response_format is unsupported")
	}
	if value.Type == "text" {
		return nil, nil
	}
	if value.Type != "json_schema" || value.JSONSchema.Schema == nil {
		return nil, unsupported("only json_schema response format is supported")
	}
	return value.JSONSchema.Schema, nil
}
func parseResponsesText(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var value struct {
		Format struct {
			Type   string         `json:"type"`
			Name   string         `json:"name"`
			Schema map[string]any `json:"schema"`
			Strict bool           `json:"strict,omitempty"`
		} `json:"format"`
	}
	if err := decodeRawStrict(raw, &value); err != nil {
		return nil, unsupported("Responses text format is unsupported")
	}
	if value.Format.Type == "text" || value.Format.Type == "" {
		return nil, nil
	}
	if value.Format.Type != "json_schema" || value.Format.Schema == nil {
		return nil, unsupported("only json_schema text format is supported")
	}
	return value.Format.Schema, nil
}
func estimateInputTokens(request proxycontract.NormalizedRequest) int {
	encoded, _ := json.Marshal(struct {
		Blocks         []proxycontract.RequestBlock `json:"blocks"`
		Tools          []proxycontract.Tool         `json:"tools"`
		ResponseSchema map[string]any               `json:"responseSchema,omitempty"`
	}{request.Blocks, request.Tools, request.ResponseSchema})
	// The POC uses a conservative tokenizer-independent ceiling: every UTF-8
	// byte may consume one token, plus fixed protocol framing per item.
	return len(encoded) + 16*(len(request.Blocks)+len(request.Tools)+1)
}
func ensureContextLimit(request routedRequest, policy proxycontract.Policy) error {
	metadataBytes := len(request.source.responseSchemaName)
	if metadata := request.source.responses; metadata != nil {
		metadataBytes += len(metadata.instructions) + len(metadata.originalInput) + len(metadata.reasoning) + len(metadata.streamOptions) + len(metadata.clientMetadata) + len(metadata.promptCacheKey)
		for _, value := range metadata.include {
			metadataBytes += len(value)
		}
		for _, item := range metadata.reasoningItems {
			metadataBytes += len(item)
		}
	}
	inputTokens := estimateInputTokens(request.normalized) + metadataBytes + 16
	if inputTokens+request.normalized.Limits.MaxOutputTokens > policy.RequestLimits.MaxContextTokens {
		return safeError("context_overflow", "request exceeds the policy context limit", 400, false)
	}
	return nil
}
func joinPath(base, suffix string) string {
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(suffix, "/")
}
