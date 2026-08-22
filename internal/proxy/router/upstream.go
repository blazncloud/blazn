package router

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/KingJammin/blazn/internal/proxycontract"
)

type upstreamResult struct {
	response *http.Response
	route    proxycontract.Route
	attempt  int
}

func (h *Handler) dispatch(ctx context.Context, request proxycontract.NormalizedRequest, routes []proxycontract.Route) (upstreamResult, error) {
	var last error
	for attempt, route := range routes {
		if attempt > 0 && !h.fallbackAllowed(last) {
			break
		}
		selectionOutcome := proxycontract.OutcomeSuccess
		selectionReason := proxycontract.EventReasonNone
		if attempt > 0 {
			selectionOutcome = proxycontract.OutcomeFallback
			selectionReason = proxycontract.EventReasonFallbackSelected
		}
		h.emit(request, route, attempt+1, proxycontract.EventRouteSelected, selectionOutcome, selectionReason, 0, nil)
		resolved, err := h.config.Resolver.Resolve(ctx, route)
		if err != nil {
			last = &APIError{Code: "connection_failure", Message: "upstream route is unavailable", Status: 502, Retryable: true, Reason: proxycontract.ReasonConnectionFailure, Cause: err}
			h.emit(request, route, attempt+1, proxycontract.EventAttemptFinished, proxycontract.OutcomeFailed, proxycontract.EventReasonConnectionFailure, 0, nil)
			continue
		}
		credential, err := h.config.Credentials.DestinationCredential(ctx, route.CredentialRef)
		if err != nil {
			return upstreamResult{}, &APIError{Code: "credential_unavailable", Message: "destination credential is unavailable", Status: 503, Cause: err}
		}
		body, path, err := encodeDestinationRequest(request, route)
		if err != nil {
			return upstreamResult{}, err
		}
		endpoint := *resolved.URL
		endpoint.Path = joinPath(endpoint.Path, path)
		httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
		if err != nil {
			return upstreamResult{}, err
		}
		httpRequest.Header.Set("Content-Type", "application/json")
		httpRequest.Header.Set("Accept", "application/json")
		if request.Stream {
			httpRequest.Header.Set("Accept", "text/event-stream")
		}
		stripCredentialHeaders(httpRequest.Header)
		if err := h.config.CredentialApply.Apply(httpRequest, route, credential); err != nil {
			return upstreamResult{}, &APIError{Code: "credential_unavailable", Message: "destination credential is unavailable", Status: 503, Cause: err}
		}
		client := &http.Client{Transport: resolved.Transport, CheckRedirect: redirectPolicy(route), Timeout: time.Duration(h.config.Policy.RequestLimits.TimeoutMS) * time.Millisecond}
		if h.config.ClientFactory != nil {
			client = h.config.ClientFactory(route, resolved)
		}
		started := h.config.Now()
		response, err := client.Do(httpRequest)
		if err != nil {
			if ctx.Err() != nil {
				return upstreamResult{}, &APIError{Code: "cancelled", Message: "request was cancelled", Status: 499, Retryable: false, Cause: ctx.Err()}
			}
			reason := proxycontract.ReasonConnectionFailure
			code := "connection_failure"
			if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
				reason = proxycontract.ReasonTimeoutBeforeFirstByte
				code = "timeout_before_first_byte"
			}
			last = &APIError{Code: code, Message: "upstream did not respond before the deadline", Status: 504, Retryable: true, Reason: reason, Cause: err}
			h.emit(request, route, attempt+1, proxycontract.EventAttemptFinished, proxycontract.OutcomeFailed, eventReason(reason), h.config.Now().Sub(started), nil)
			continue
		}
		if response.StatusCode >= 400 {
			retryReason, retryable := classifyStatus(response.StatusCode)
			_ = response.Body.Close()
			code, reasonCode := string(retryReason), eventReason(retryReason)
			if retryReason == "" {
				code, reasonCode = "invalid_request", proxycontract.EventReasonPolicyDenied
			}
			if response.StatusCode == 401 || response.StatusCode == 403 {
				code, reasonCode = "credential_unavailable", proxycontract.EventReasonAuthenticationFailed
			}
			last = &APIError{Code: code, Message: "upstream rejected the request", Status: safeUpstreamStatus(response.StatusCode), Retryable: retryable, Reason: retryReason}
			h.emit(request, route, attempt+1, proxycontract.EventAttemptFinished, proxycontract.OutcomeFailed, reasonCode, h.config.Now().Sub(started), nil)
			continue
		}
		return upstreamResult{response: response, route: route, attempt: attempt + 1}, nil
	}
	if last == nil {
		last = safeError("no_compliant_route", "no route could serve the request", 403, false)
	}
	return upstreamResult{}, last
}

func (h *Handler) fallbackAllowed(err error) bool {
	var api *APIError
	if !errors.As(err, &api) || !api.Retryable {
		return false
	}
	for _, reason := range h.config.Policy.Fallback.RetryableReasons {
		if reason == api.Reason {
			return true
		}
	}
	return false
}

func classifyStatus(status int) (proxycontract.RetryableReason, bool) {
	switch {
	case status == 429:
		return proxycontract.ReasonRateLimited, true
	case status >= 500:
		return proxycontract.ReasonUpstream5xx, true
	case status == 404:
		return proxycontract.ReasonModelUnavailable, true
	case status == 413:
		return proxycontract.ReasonCompatibleContextOverflow, true
	default:
		return "", false
	}
}
func safeUpstreamStatus(status int) int {
	if status == 429 {
		return 429
	}
	if status >= 500 {
		return 502
	}
	if status == 401 || status == 403 {
		return 502
	}
	return 400
}
func isTimeout(err error) bool { var value net.Error; return errors.As(err, &value) && value.Timeout() }

func encodeDestinationRequest(request proxycontract.NormalizedRequest, route proxycontract.Route) ([]byte, string, error) {
	switch route.DestinationProtocol {
	case proxycontract.ProtocolOpenAIChat:
		messages := make([]map[string]any, 0, len(request.Blocks))
		for _, block := range request.Blocks {
			switch block.Type {
			case "text":
				messages = append(messages, map[string]any{"role": block.Role, "content": *block.Text})
			case "tool_call":
				messages = append(messages, map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": *block.CallID, "type": "function", "function": map[string]any{"name": *block.ToolName, "arguments": string(block.Arguments)}}}})
			case "tool_result":
				messages = append(messages, map[string]any{"role": "tool", "tool_call_id": *block.CallID, "content": string(block.Result)})
			}
		}
		tools := make([]any, 0, len(request.Tools))
		for _, tool := range request.Tools {
			tools = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": tool.Name, "description": tool.Description, "parameters": tool.InputSchema}})
		}
		body := map[string]any{"model": route.Model, "messages": messages, "stream": request.Stream, "max_completion_tokens": request.Limits.MaxOutputTokens, "tools": tools, "tool_choice": request.ToolChoice}
		if request.ResponseSchema != nil {
			body["response_format"] = map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "blazn_response", "schema": request.ResponseSchema, "strict": true}}
		}
		if request.Limits.Temperature != nil {
			body["temperature"] = *request.Limits.Temperature
		}
		if request.Limits.TopP != nil {
			body["top_p"] = *request.Limits.TopP
		}
		if len(request.Limits.Stop) > 0 {
			body["stop"] = request.Limits.Stop
		}
		if request.Stream {
			body["stream_options"] = map[string]any{"include_usage": true}
		}
		encoded, err := json.Marshal(body)
		return encoded, "chat/completions", err
	case proxycontract.ProtocolOpenAIResponses:
		input := make([]any, 0, len(request.Blocks))
		for _, block := range request.Blocks {
			switch block.Type {
			case "text":
				input = append(input, map[string]any{"type": "message", "role": block.Role, "content": *block.Text})
			case "tool_call":
				input = append(input, map[string]any{"type": "function_call", "call_id": *block.CallID, "name": *block.ToolName, "arguments": string(block.Arguments)})
			case "tool_result":
				input = append(input, map[string]any{"type": "function_call_output", "call_id": *block.CallID, "output": string(block.Result)})
			}
		}
		tools := make([]any, 0, len(request.Tools))
		for _, tool := range request.Tools {
			tools = append(tools, map[string]any{"type": "function", "name": tool.Name, "description": tool.Description, "parameters": tool.InputSchema})
		}
		body := map[string]any{"model": route.Model, "input": input, "stream": request.Stream, "max_output_tokens": request.Limits.MaxOutputTokens, "tools": tools, "tool_choice": request.ToolChoice}
		if request.ResponseSchema != nil {
			body["text"] = map[string]any{"format": map[string]any{"type": "json_schema", "name": "blazn_response", "schema": request.ResponseSchema, "strict": true}}
		}
		encoded, err := json.Marshal(body)
		return encoded, "responses", err
	default:
		return nil, "", unsupported("destination protocol is unsupported")
	}
}

type chatResponse struct {
	ID      string `json:"id"`
	Choices []struct {
		Message struct {
			Content   *string        `json:"content"`
			ToolCalls []chatToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}
type responsesResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Output []struct {
		Type      string          `json:"type"`
		CallID    string          `json:"call_id"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Content   []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func decodeUpstreamResponse(result upstreamResult, request proxycontract.NormalizedRequest) (proxycontract.NormalizedResponse, error) {
	defer result.response.Body.Close()
	limited := io.LimitReader(result.response.Body, maxRequestBytes+1)
	response := proxycontract.NormalizedResponse{LogicalRequestID: request.LogicalRequestID, ModelAlias: request.ModelAlias, RouteID: result.route.ID, Blocks: []proxycontract.ResponseBlock{}, FinishReason: "stop"}
	switch result.route.DestinationProtocol {
	case proxycontract.ProtocolOpenAIChat:
		var source chatResponse
		if err := decodeUpstreamJSON(limited, &source); err != nil {
			return response, safeError("upstream_invalid_response", "upstream returned an invalid Chat response", 502, false)
		}
		if len(source.Choices) != 1 {
			return response, safeError("upstream_invalid_response", "upstream returned an invalid choice count", 502, false)
		}
		choice := source.Choices[0]
		if choice.Message.Content != nil {
			response.Blocks = append(response.Blocks, proxycontract.ResponseBlock{Type: "text", Text: choice.Message.Content})
		}
		for _, call := range choice.Message.ToolCalls {
			args := normalizeJSONValue(call.Function.Arguments)
			response.Blocks = append(response.Blocks, proxycontract.ResponseBlock{Type: "tool_call", CallID: stringPtr(call.ID), ToolName: stringPtr(call.Function.Name), Arguments: args})
		}
		response.FinishReason = mapFinish(choice.FinishReason)
		response.Usage = proxycontract.Usage{InputTokens: source.Usage.PromptTokens, OutputTokens: source.Usage.CompletionTokens}
	case proxycontract.ProtocolOpenAIResponses:
		var source responsesResponse
		if err := decodeUpstreamJSON(limited, &source); err != nil {
			return response, safeError("upstream_invalid_response", "upstream returned an invalid Responses payload", 502, false)
		}
		if source.Status != "completed" && source.Status != "incomplete" {
			return response, safeError("upstream_invalid_response", "upstream Responses request did not complete", 502, false)
		}
		for _, item := range source.Output {
			switch item.Type {
			case "message":
				for _, content := range item.Content {
					if content.Type != "output_text" {
						return response, unsupported("upstream Responses output type is unsupported")
					}
					text := content.Text
					response.Blocks = append(response.Blocks, proxycontract.ResponseBlock{Type: "text", Text: &text})
				}
			case "function_call":
				args := normalizeJSONValue(item.Arguments)
				response.Blocks = append(response.Blocks, proxycontract.ResponseBlock{Type: "tool_call", CallID: stringPtr(item.CallID), ToolName: stringPtr(item.Name), Arguments: args})
			default:
				return response, unsupported("upstream Responses item is unsupported")
			}
		}
		if source.Status == "incomplete" {
			response.FinishReason = "length"
		}
		response.Usage = proxycontract.Usage{InputTokens: source.Usage.InputTokens, OutputTokens: source.Usage.OutputTokens}
	}
	if err := response.Validate(); err != nil {
		return response, safeError("upstream_invalid_response", "upstream response failed normalization", 502, false)
	}
	return response, nil
}

func decodeUpstreamJSON(reader io.Reader, target any) error {
	decoder := json.NewDecoder(reader)
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("upstream response has trailing JSON data")
	}
	return nil
}

func mapFinish(value string) proxycontract.FinishReason {
	switch value {
	case "length":
		return "length"
	case "tool_calls":
		return "tool_call"
	case "content_filter":
		return "content_filter"
	default:
		return "stop"
	}
}

func writeSourceResponse(writer http.ResponseWriter, protocol proxycontract.Protocol, response proxycontract.NormalizedResponse) error {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	switch protocol {
	case proxycontract.ProtocolOpenAIChat:
		message := map[string]any{"role": "assistant", "content": nil}
		var texts []string
		var calls []any
		for _, block := range response.Blocks {
			if block.Type == "text" {
				texts = append(texts, *block.Text)
			} else {
				calls = append(calls, map[string]any{"id": *block.CallID, "type": "function", "function": map[string]any{"name": *block.ToolName, "arguments": string(block.Arguments)}})
			}
		}
		if len(texts) > 0 {
			message["content"] = strings.Join(texts, "")
		}
		if len(calls) > 0 {
			message["tool_calls"] = calls
		}
		finish := string(response.FinishReason)
		if finish == "tool_call" {
			finish = "tool_calls"
		}
		return json.NewEncoder(writer).Encode(map[string]any{"id": "chatcmpl_" + response.LogicalRequestID, "object": "chat.completion", "model": response.ModelAlias, "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}, "usage": map[string]any{"prompt_tokens": response.Usage.InputTokens, "completion_tokens": response.Usage.OutputTokens, "total_tokens": response.Usage.InputTokens + response.Usage.OutputTokens}})
	case proxycontract.ProtocolOpenAIResponses:
		output := make([]any, 0, len(response.Blocks))
		for index, block := range response.Blocks {
			if block.Type == "text" {
				output = append(output, map[string]any{"id": fmt.Sprintf("msg_%d", index), "type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": *block.Text, "annotations": []any{}}}})
			} else {
				output = append(output, map[string]any{"id": fmt.Sprintf("fc_%d", index), "type": "function_call", "call_id": *block.CallID, "name": *block.ToolName, "arguments": string(block.Arguments)})
			}
		}
		return json.NewEncoder(writer).Encode(map[string]any{"id": "resp_" + response.LogicalRequestID, "object": "response", "status": "completed", "model": response.ModelAlias, "output": output, "usage": map[string]any{"input_tokens": response.Usage.InputTokens, "output_tokens": response.Usage.OutputTokens, "total_tokens": response.Usage.InputTokens + response.Usage.OutputTokens}})
	default:
		return unsupported("source protocol is unsupported")
	}
}

// streamResponse normalizes upstream SSE enough to guarantee cancellation,
// usage delivery and the no-fallback-after-first-byte invariant. Each event is
// re-emitted in the source protocol rather than blindly proxying provider data.
func streamResponse(ctx context.Context, writer http.ResponseWriter, result upstreamResult, request proxycontract.NormalizedRequest) error {
	defer result.response.Body.Close()
	flusher, ok := writer.(http.Flusher)
	if !ok {
		return errors.New("streaming writer does not support flushing")
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Accel-Buffering", "no")
	scanner := bufio.NewScanner(result.response.Body)
	scanner.Buffer(make([]byte, 4096), maxRequestBytes)
	sequence := 0
	normalizer := streamNormalizer{toolIDs: map[int]string{}, toolNames: map[int]string{}, toolStarted: map[int]bool{}, responseCallIDs: map[string]string{}}
	encoder := sourceStreamEncoder{writer: writer, protocol: request.Protocol, alias: request.ModelAlias, toolIndexes: map[string]int{}}
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		events, err := normalizer.normalize(result.route.DestinationProtocol, request.LogicalRequestID, sequence, []byte(payload))
		if err != nil {
			return err
		}
		for _, event := range events {
			sequence = event.Sequence + 1
			if err := encoder.write(event); err != nil {
				return err
			}
			flusher.Flush()
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return nil
}

type streamNormalizer struct {
	toolIDs         map[int]string
	toolNames       map[int]string
	toolStarted     map[int]bool
	responseCallIDs map[string]string
}

func (n *streamNormalizer) normalize(protocol proxycontract.Protocol, requestID string, sequence int, payload []byte) ([]proxycontract.NormalizedStreamEvent, error) {
	if protocol == proxycontract.ProtocolOpenAIChat {
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   *string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(payload, &chunk); err != nil {
			return nil, err
		}
		out := []proxycontract.NormalizedStreamEvent{}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != nil {
				out = append(out, proxycontract.NormalizedStreamEvent{LogicalRequestID: requestID, Sequence: sequence, Type: "text_delta", Text: choice.Delta.Content})
				sequence++
			}
			for _, call := range choice.Delta.ToolCalls {
				if call.ID != "" {
					n.toolIDs[call.Index] = call.ID
				}
				if call.Function.Name != "" {
					n.toolNames[call.Index] = call.Function.Name
				}
				callID, name := n.toolIDs[call.Index], n.toolNames[call.Index]
				if !n.toolStarted[call.Index] && callID != "" && name != "" {
					n.toolStarted[call.Index] = true
					out = append(out, proxycontract.NormalizedStreamEvent{LogicalRequestID: requestID, Sequence: sequence, Type: "tool_call_start", CallID: &callID, ToolName: &name})
					sequence++
				}
				if call.Function.Arguments != "" && callID != "" {
					delta := call.Function.Arguments
					out = append(out, proxycontract.NormalizedStreamEvent{LogicalRequestID: requestID, Sequence: sequence, Type: "tool_arguments_delta", CallID: &callID, ArgumentsDelta: &delta})
					sequence++
				}
			}
			if choice.FinishReason != nil {
				finish := mapFinish(*choice.FinishReason)
				out = append(out, proxycontract.NormalizedStreamEvent{LogicalRequestID: requestID, Sequence: sequence, Type: "response_end", FinishReason: &finish})
				sequence++
			}
		}
		if chunk.Usage != nil {
			usage := proxycontract.Usage{InputTokens: chunk.Usage.PromptTokens, OutputTokens: chunk.Usage.CompletionTokens}
			out = append(out, proxycontract.NormalizedStreamEvent{LogicalRequestID: requestID, Sequence: sequence, Type: "usage", Usage: &usage})
		}
		return out, nil
	}
	var event struct {
		Type   string `json:"type"`
		Delta  string `json:"delta"`
		Text   string `json:"text"`
		ItemID string `json:"item_id"`
		Item   *struct {
			Type   string `json:"type"`
			ID     string `json:"id"`
			CallID string `json:"call_id"`
			Name   string `json:"name"`
		} `json:"item"`
		Response *struct {
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
			Status string `json:"status"`
		} `json:"response"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		return nil, err
	}
	switch event.Type {
	case "response.output_text.delta":
		text := event.Delta
		return []proxycontract.NormalizedStreamEvent{{LogicalRequestID: requestID, Sequence: sequence, Type: "text_delta", Text: &text}}, nil
	case "response.output_item.added":
		if event.Item == nil || event.Item.Type != "function_call" {
			return nil, nil
		}
		callID := event.Item.CallID
		if callID == "" {
			callID = event.Item.ID
		}
		name := event.Item.Name
		if callID == "" || name == "" {
			return nil, errors.New("invalid Responses function-call start")
		}
		if event.Item.ID != "" {
			n.responseCallIDs[event.Item.ID] = callID
		}
		return []proxycontract.NormalizedStreamEvent{{LogicalRequestID: requestID, Sequence: sequence, Type: "tool_call_start", CallID: &callID, ToolName: &name}}, nil
	case "response.function_call_arguments.delta":
		callID := n.responseCallIDs[event.ItemID]
		if callID == "" {
			callID = event.ItemID
		}
		if callID == "" {
			return nil, errors.New("invalid Responses function-call delta")
		}
		delta := event.Delta
		return []proxycontract.NormalizedStreamEvent{{LogicalRequestID: requestID, Sequence: sequence, Type: "tool_arguments_delta", CallID: &callID, ArgumentsDelta: &delta}}, nil
	case "response.completed":
		usage := proxycontract.Usage{InputTokens: event.Response.Usage.InputTokens, OutputTokens: event.Response.Usage.OutputTokens}
		finish := proxycontract.FinishReason("stop")
		return []proxycontract.NormalizedStreamEvent{{LogicalRequestID: requestID, Sequence: sequence, Type: "usage", Usage: &usage}, {LogicalRequestID: requestID, Sequence: sequence + 1, Type: "response_end", FinishReason: &finish}}, nil
	default:
		return nil, nil
	}
}

type sourceStreamEncoder struct {
	writer        io.Writer
	protocol      proxycontract.Protocol
	alias         string
	toolIndexes   map[string]int
	nextToolIndex int
}

func (s *sourceStreamEncoder) write(event proxycontract.NormalizedStreamEvent) error {
	var payload any
	if s.protocol == proxycontract.ProtocolOpenAIChat {
		choice := map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": nil}
		if event.Type == "text_delta" {
			choice["delta"] = map[string]any{"content": *event.Text}
		}
		if event.Type == "response_end" {
			finish := string(*event.FinishReason)
			if finish == "tool_call" {
				finish = "tool_calls"
			}
			choice["finish_reason"] = finish
		}
		if event.Type == "tool_call_start" {
			index := s.nextToolIndex
			s.nextToolIndex++
			s.toolIndexes[*event.CallID] = index
			choice["delta"] = map[string]any{"tool_calls": []any{map[string]any{"index": index, "id": *event.CallID, "type": "function", "function": map[string]any{"name": *event.ToolName, "arguments": ""}}}}
		}
		if event.Type == "tool_arguments_delta" {
			index, ok := s.toolIndexes[*event.CallID]
			if !ok {
				return errors.New("tool argument delta preceded tool start")
			}
			choice["delta"] = map[string]any{"tool_calls": []any{map[string]any{"index": index, "function": map[string]any{"arguments": *event.ArgumentsDelta}}}}
		}
		object := map[string]any{"id": "chatcmpl_" + event.LogicalRequestID, "object": "chat.completion.chunk", "model": s.alias, "choices": []any{choice}}
		if event.Type == "usage" {
			object["choices"] = []any{}
			object["usage"] = map[string]any{"prompt_tokens": event.Usage.InputTokens, "completion_tokens": event.Usage.OutputTokens, "total_tokens": event.Usage.InputTokens + event.Usage.OutputTokens}
		}
		payload = object
	} else {
		kind := map[proxycontract.StreamEventType]string{"text_delta": "response.output_text.delta", "tool_call_start": "response.output_item.added", "tool_arguments_delta": "response.function_call_arguments.delta", "usage": "response.usage", "response_end": "response.completed"}[event.Type]
		payload = map[string]any{"type": kind, "sequence_number": event.Sequence}
		if event.Text != nil {
			payload.(map[string]any)["delta"] = *event.Text
		}
		if event.Usage != nil {
			payload.(map[string]any)["usage"] = event.Usage
		}
		if event.Type == "tool_call_start" {
			payload.(map[string]any)["item"] = map[string]any{"type": "function_call", "call_id": *event.CallID, "name": *event.ToolName}
		}
		if event.Type == "tool_arguments_delta" {
			payload.(map[string]any)["item_id"] = *event.CallID
			payload.(map[string]any)["delta"] = *event.ArgumentsDelta
		}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(s.writer, "data: %s\n\n", encoded)
	return err
}
