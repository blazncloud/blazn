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
	"sort"
	"strings"
	"time"

	"github.com/KingJammin/blazn/internal/proxycontract"
)

type upstreamResult struct {
	response       *http.Response
	route          proxycontract.Route
	attempt        int
	scanner        *bufio.Scanner
	normalizer     *streamNormalizer
	buffered       []proxycontract.NormalizedStreamEvent
	attemptStarted time.Time
}

func (h *Handler) dispatch(ctx context.Context, routed routedRequest, routes []proxycontract.Route) (upstreamResult, error) {
	request := routed.normalized
	var last error
	var failed upstreamResult
	for attempt, route := range routes {
		failed = upstreamResult{route: route, attempt: attempt + 1}
		started := h.config.Now()
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
			h.emit(request, route, attempt+1, proxycontract.EventAttemptFinished, proxycontract.OutcomeFailed, proxycontract.EventReasonConnectionFailure, h.config.Now().Sub(started), nil)
			continue
		}
		credential, err := h.config.Credentials.DestinationCredential(ctx, route.CredentialRef)
		if err != nil {
			failure := &APIError{Code: "credential_unavailable", Message: "destination credential is unavailable", Status: 503, Cause: err}
			h.emit(request, route, attempt+1, proxycontract.EventAttemptFinished, proxycontract.OutcomeFailed, proxycontract.EventReasonAuthenticationFailed, h.config.Now().Sub(started), nil)
			return failed, failure
		}
		body, path, err := encodeDestinationRequest(routed, route)
		if err != nil {
			return failed, err
		}
		endpoint := *resolved.URL
		endpoint.Path = joinPath(endpoint.Path, path)
		httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
		if err != nil {
			return failed, err
		}
		httpRequest.Header.Set("Content-Type", "application/json")
		httpRequest.Header.Set("Accept", "application/json")
		if request.Stream {
			httpRequest.Header.Set("Accept", "text/event-stream")
		}
		stripCredentialHeaders(httpRequest.Header)
		if err := h.config.CredentialApply.Apply(httpRequest, route, credential); err != nil {
			failure := &APIError{Code: "credential_unavailable", Message: "destination credential is unavailable", Status: 503, Cause: err}
			h.emit(request, route, attempt+1, proxycontract.EventAttemptFinished, proxycontract.OutcomeFailed, proxycontract.EventReasonAuthenticationFailed, h.config.Now().Sub(started), nil)
			return failed, failure
		}
		transport := http.RoundTripper(resolved.Transport)
		if h.transportFactory != nil {
			transport = h.transportFactory(route, resolved)
		}
		client := &http.Client{Transport: transport, CheckRedirect: redirectPolicy(route)}
		response, err := client.Do(httpRequest)
		if err != nil {
			if ctx.Err() != nil {
				return failed, &APIError{Code: "cancelled", Message: "request was cancelled", Status: 499, Retryable: false, Cause: ctx.Err()}
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
		result := upstreamResult{response: response, route: route, attempt: attempt + 1, attemptStarted: started}
		if request.Stream {
			firstEventTimeout := time.Duration(route.HealthTimeoutMS) * time.Millisecond
			if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < firstEventTimeout {
				firstEventTimeout = time.Until(deadline)
			}
			timerFired := make(chan struct{})
			timer := time.AfterFunc(firstEventTimeout, func() { _ = response.Body.Close(); close(timerFired) })
			prepared, prepareErr := prepareStream(result, request)
			if !timer.Stop() {
				<-timerFired
				prepareErr = &APIError{Code: "timeout_before_first_byte", Message: "upstream did not produce an event before the route deadline", Status: 504, Retryable: true, Reason: proxycontract.ReasonTimeoutBeforeFirstByte}
			}
			if prepareErr != nil {
				_ = response.Body.Close()
				last = prepareErr
				h.emit(request, route, attempt+1, proxycontract.EventAttemptFinished, proxycontract.OutcomeFailed, reasonForError(prepareErr), h.config.Now().Sub(started), nil)
				continue
			}
			result = prepared
		}
		return result, nil
	}
	if last == nil {
		last = safeError("no_compliant_route", "no route could serve the request", 403, false)
	}
	return failed, last
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

func encodeDestinationRequest(routed routedRequest, route proxycontract.Route) ([]byte, string, error) {
	request := routed.normalized
	switch route.DestinationProtocol {
	case proxycontract.ProtocolOpenAIChat:
		messages := make([]map[string]any, 0, len(request.Blocks))
		for index := 0; index < len(request.Blocks); {
			block := request.Blocks[index]
			if block.Role == "assistant" && (block.Type == "text" || block.Type == "tool_call") {
				parts := []string{}
				calls := []any{}
				next := index
				for next < len(request.Blocks) {
					candidate := request.Blocks[next]
					if candidate.Role != "assistant" || (candidate.Type != "text" && candidate.Type != "tool_call") {
						break
					}
					if candidate.Type == "text" {
						parts = append(parts, *candidate.Text)
					} else {
						calls = append(calls, map[string]any{"id": *candidate.CallID, "type": "function", "function": map[string]any{"name": *candidate.ToolName, "arguments": string(candidate.Arguments)}})
					}
					next++
				}
				message := map[string]any{"role": "assistant", "content": nil}
				if len(parts) > 0 {
					message["content"] = strings.Join(parts, "")
				}
				if len(calls) > 0 {
					message["tool_calls"] = calls
				}
				messages = append(messages, message)
				index = next
				continue
			}
			switch block.Type {
			case "text":
				messages = append(messages, map[string]any{"role": block.Role, "content": *block.Text})
			case "tool_call":
				messages = append(messages, map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": *block.CallID, "type": "function", "function": map[string]any{"name": *block.ToolName, "arguments": string(block.Arguments)}}}})
			case "tool_result":
				messages = append(messages, map[string]any{"role": "tool", "tool_call_id": *block.CallID, "content": jsonContent(block.Result)})
			}
			index++
		}
		tools := make([]any, 0, len(request.Tools))
		for _, tool := range request.Tools {
			tools = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": tool.Name, "description": tool.Description, "parameters": tool.InputSchema}})
		}
		body := map[string]any{"model": route.Model, "messages": messages, "stream": request.Stream, "max_completion_tokens": request.Limits.MaxOutputTokens, "tools": tools, "tool_choice": request.ToolChoice}
		if metadata := routed.source.responses; metadata != nil {
			if metadata.parallelToolCalls != nil {
				body["parallel_tool_calls"] = *metadata.parallelToolCalls
			}
			if metadata.serviceTier != "" {
				body["service_tier"] = metadata.serviceTier
			}
			if metadata.promptCacheKey != "" {
				body["prompt_cache_key"] = metadata.promptCacheKey
			}
		}
		if request.ResponseSchema != nil {
			body["response_format"] = map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": schemaName(routed.source.responseSchemaName), "schema": request.ResponseSchema, "strict": routed.source.responseSchemaStrict}}
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
				input = append(input, map[string]any{"type": "function_call_output", "call_id": *block.CallID, "output": jsonContent(block.Result)})
			}
		}
		tools := make([]any, 0, len(request.Tools))
		for _, tool := range request.Tools {
			tools = append(tools, map[string]any{"type": "function", "name": tool.Name, "description": tool.Description, "parameters": tool.InputSchema})
		}
		body := map[string]any{"model": route.Model, "input": input, "stream": request.Stream, "max_output_tokens": request.Limits.MaxOutputTokens, "tools": tools, "tool_choice": request.ToolChoice}
		if request.Limits.Temperature != nil {
			body["temperature"] = *request.Limits.Temperature
		}
		if request.Limits.TopP != nil {
			body["top_p"] = *request.Limits.TopP
		}
		if metadata := routed.source.responses; metadata != nil {
			if len(metadata.originalInput) > 0 {
				var original any
				if json.Unmarshal(metadata.originalInput, &original) != nil {
					return nil, "", safeError("invalid_request", "Responses input could not be preserved", 400, false)
				}
				body["input"] = original
			}
			if metadata.instructions != "" {
				body["instructions"] = metadata.instructions
			}
			if metadata.parallelToolCalls != nil {
				body["parallel_tool_calls"] = *metadata.parallelToolCalls
			}
			if metadata.store != nil {
				body["store"] = *metadata.store
			}
			if len(metadata.include) > 0 {
				body["include"] = metadata.include
			}
			if len(metadata.reasoning) > 0 {
				body["reasoning"] = json.RawMessage(metadata.reasoning)
			}
			if len(metadata.streamOptions) > 0 {
				body["stream_options"] = json.RawMessage(metadata.streamOptions)
			}
			if metadata.serviceTier != "" {
				body["service_tier"] = metadata.serviceTier
			}
			if metadata.promptCacheKey != "" {
				body["prompt_cache_key"] = metadata.promptCacheKey
			}
			if len(metadata.clientMetadata) > 0 {
				body["client_metadata"] = json.RawMessage(metadata.clientMetadata)
			}
		}
		if request.ResponseSchema != nil {
			body["text"] = map[string]any{"format": map[string]any{"type": "json_schema", "name": schemaName(routed.source.responseSchemaName), "schema": request.ResponseSchema, "strict": routed.source.responseSchemaStrict}}
		}
		encoded, err := json.Marshal(body)
		return encoded, "responses", err
	default:
		return nil, "", unsupported("destination protocol is unsupported")
	}
}

func schemaName(value string) string {
	if value == "" {
		return "blazn_response"
	}
	return value
}
func jsonContent(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var compact bytes.Buffer
	if json.Compact(&compact, raw) == nil {
		return compact.String()
	}
	return string(raw)
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
	Usage *struct {
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
	Usage *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	IncompleteDetails json.RawMessage `json:"incomplete_details"`
}

type responseMetadata struct {
	status            string
	incompleteDetails json.RawMessage
}

func decodeUpstreamResponse(result upstreamResult, request proxycontract.NormalizedRequest) (proxycontract.NormalizedResponse, error) {
	return decodeUpstreamResponseDetailed(result, request, nil)
}
func decodeUpstreamResponseDetailed(result upstreamResult, request proxycontract.NormalizedRequest, metadata *responseMetadata) (proxycontract.NormalizedResponse, error) {
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
		if source.Usage == nil {
			return response, safeError("upstream_invalid_response", "upstream Chat response omitted usage", 502, false)
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
		if metadata != nil {
			metadata.status = source.Status
			metadata.incompleteDetails = append(json.RawMessage(nil), source.IncompleteDetails...)
		}
		if source.Usage == nil {
			return response, safeError("upstream_invalid_response", "upstream Responses payload omitted usage", 502, false)
		}
		sawToolCall := false
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
				sawToolCall = true
				args := normalizeJSONValue(item.Arguments)
				response.Blocks = append(response.Blocks, proxycontract.ResponseBlock{Type: "tool_call", CallID: stringPtr(item.CallID), ToolName: stringPtr(item.Name), Arguments: args})
			default:
				return response, unsupported("upstream Responses item is unsupported")
			}
		}
		if source.Status == "incomplete" {
			response.FinishReason = "length"
		} else if sawToolCall {
			response.FinishReason = "tool_call"
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
	return writeSourceResponseDetailed(writer, protocol, response, responseMetadata{})
}
func writeSourceResponseDetailed(writer http.ResponseWriter, protocol proxycontract.Protocol, response proxycontract.NormalizedResponse, metadata responseMetadata) error {
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
		status := metadata.status
		if status == "" {
			status = "completed"
		}
		body := map[string]any{"id": "resp_" + response.LogicalRequestID, "object": "response", "status": status, "model": response.ModelAlias, "output": output, "usage": map[string]any{"input_tokens": response.Usage.InputTokens, "output_tokens": response.Usage.OutputTokens, "total_tokens": response.Usage.InputTokens + response.Usage.OutputTokens}}
		if status == "incomplete" && len(metadata.incompleteDetails) > 0 {
			body["incomplete_details"] = json.RawMessage(metadata.incompleteDetails)
		}
		return json.NewEncoder(writer).Encode(body)
	default:
		return unsupported("source protocol is unsupported")
	}
}

func prepareStream(result upstreamResult, request proxycontract.NormalizedRequest) (upstreamResult, error) {
	scanner := bufio.NewScanner(result.response.Body)
	scanner.Buffer(make([]byte, 4096), maxRequestBytes)
	normalizer := &streamNormalizer{toolIDs: map[int]string{}, toolNames: map[int]string{}, toolStarted: map[int]bool{}, responseCallIDs: map[string]string{}, reasoningItems: map[string]reasoningOutput{}, toolOutputIndexes: map[string]int{}, textOutputIndex: -1}
	for {
		events, done, err := nextStreamEvents(scanner, normalizer, result.route.DestinationProtocol, request.LogicalRequestID)
		if err != nil {
			return result, streamAPIError(err)
		}
		if done {
			return result, &APIError{Code: "connection_failure", Message: "upstream ended before the first usable event", Status: 502, Retryable: true, Reason: proxycontract.ReasonConnectionFailure}
		}
		if len(events) == 0 {
			continue
		}
		if events[0].Type == "error" {
			return result, streamAPIError(events[0].Error)
		}
		result.scanner, result.normalizer, result.buffered = scanner, normalizer, events
		return result, nil
	}
}

func nextStreamEvents(scanner *bufio.Scanner, normalizer *streamNormalizer, protocol proxycontract.Protocol, requestID string) ([]proxycontract.NormalizedStreamEvent, bool, error) {
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			return nil, true, nil
		}
		events, err := normalizer.normalize(protocol, requestID, normalizer.sequence, []byte(payload))
		if err != nil {
			return nil, false, err
		}
		if len(events) > 0 {
			normalizer.sequence = events[len(events)-1].Sequence + 1
			return events, false, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, false, err
	}
	if normalizer.terminal {
		return nil, true, nil
	}
	return nil, false, io.ErrUnexpectedEOF
}

func streamAPIError(value any) error {
	if normalized, ok := value.(*proxycontract.NormalizedError); ok && normalized != nil {
		reason := proxycontract.RetryableReason(normalized.Code)
		if normalized.Code == "context_overflow" {
			reason = proxycontract.ReasonCompatibleContextOverflow
		}
		retry := normalized.RetryClass == "before_first_byte_only"
		return &APIError{Code: string(normalized.Code), Message: normalized.SafeMessage, Status: safeUpstreamStatus(normalized.UpstreamStatus), Retryable: retry, Reason: reason}
	}
	if err, ok := value.(error); ok {
		if errors.Is(err, context.DeadlineExceeded) {
			return &APIError{Code: "timeout_before_first_byte", Message: "upstream did not produce an event before the deadline", Status: 504, Retryable: true, Reason: proxycontract.ReasonTimeoutBeforeFirstByte, Cause: err}
		}
		if errors.Is(err, context.Canceled) {
			return &APIError{Code: "cancelled", Message: "request was cancelled", Status: 499, Retryable: false, Cause: err}
		}
		return &APIError{Code: "connection_failure", Message: "upstream stream failed", Status: 502, Retryable: true, Reason: proxycontract.ReasonConnectionFailure, Cause: err}
	}
	return safeError("upstream_5xx", "upstream stream failed", 502, false)
}

// streamResponse commits the route only after dispatch buffered one usable
// event. From this point onward fallback is forbidden even if the stream fails.
func streamResponse(ctx context.Context, writer http.ResponseWriter, result upstreamResult, request proxycontract.NormalizedRequest) (*proxycontract.Usage, error) {
	defer result.response.Body.Close()
	flusher, ok := writer.(http.Flusher)
	if !ok {
		return nil, errors.New("streaming writer does not support flushing")
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Accel-Buffering", "no")
	encoder := sourceStreamEncoder{writer: writer, protocol: request.Protocol, alias: request.ModelAlias, requestID: request.LogicalRequestID, toolIndexes: map[string]int{}, toolNames: map[string]string{}, toolArguments: map[string]string{}, textIndex: -1}
	encoder.applyIndexHints(result.normalizer)
	if err := encoder.start(); err != nil {
		return nil, err
	}
	flusher.Flush()
	consume := func(events []proxycontract.NormalizedStreamEvent) error {
		for _, event := range events {
			if event.Type == "error" {
				return streamAPIError(event.Error)
			}
			if err := encoder.consume(event); err != nil {
				return err
			}
			flusher.Flush()
		}
		return nil
	}
	if err := consume(result.buffered); err != nil {
		_ = encoder.fail(err)
		flusher.Flush()
		return encoder.usage, err
	}
	sawDone := false
	for {
		select {
		case <-ctx.Done():
			return encoder.usage, ctx.Err()
		default:
		}
		events, done, err := nextStreamEvents(result.scanner, result.normalizer, result.route.DestinationProtocol, request.LogicalRequestID)
		if err != nil {
			_ = encoder.fail(err)
			flusher.Flush()
			return encoder.usage, err
		}
		if done {
			sawDone = true
			break
		}
		encoder.applyIndexHints(result.normalizer)
		if err = consume(events); err != nil {
			_ = encoder.fail(err)
			flusher.Flush()
			return encoder.usage, err
		}
	}
	if !sawDone || !result.normalizer.terminal {
		failure := safeError("upstream_invalid_response", "upstream stream ended without a terminal event", 502, false)
		_ = encoder.fail(failure)
		flusher.Flush()
		return encoder.usage, failure
	}
	encoder.reasoningItems = result.normalizer.orderedReasoningItems()
	if err := encoder.finish(); err != nil {
		return encoder.usage, err
	}
	flusher.Flush()
	return encoder.usage, nil
}

type streamNormalizer struct {
	toolIDs           map[int]string
	toolNames         map[int]string
	toolStarted       map[int]bool
	responseCallIDs   map[string]string
	sequence          int
	started           bool
	terminal          bool
	sawFunctionCall   bool
	reasoningItems    map[string]reasoningOutput
	reasoningOrder    []string
	toolOutputIndexes map[string]int
	textOutputIndex   int
}

type reasoningOutput struct {
	raw   json.RawMessage
	index int
}

func (n *streamNormalizer) normalize(protocol proxycontract.Protocol, requestID string, sequence int, payload []byte) ([]proxycontract.NormalizedStreamEvent, error) {
	if protocol == proxycontract.ProtocolOpenAIChat {
		var chunk struct {
			Error *struct {
				Message string `json:"message"`
				Code    string `json:"code"`
			} `json:"error"`
			Choices []struct {
				Delta struct {
					Role      string  `json:"role"`
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
		if chunk.Error != nil {
			normalized := normalizeStreamFailure(chunk.Error.Code)
			return []proxycontract.NormalizedStreamEvent{{LogicalRequestID: requestID, Sequence: sequence, Type: "error", Error: &normalized}}, nil
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Role == "assistant" && !n.started {
				n.started = true
				out = append(out, proxycontract.NormalizedStreamEvent{LogicalRequestID: requestID, Sequence: sequence, Type: "response_start"})
				sequence++
			}
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
				n.terminal = true
			}
		}
		if chunk.Usage != nil {
			usage := proxycontract.Usage{InputTokens: chunk.Usage.PromptTokens, OutputTokens: chunk.Usage.CompletionTokens}
			out = append(out, proxycontract.NormalizedStreamEvent{LogicalRequestID: requestID, Sequence: sequence, Type: "usage", Usage: &usage})
		}
		return out, nil
	}
	var event struct {
		Type        string          `json:"type"`
		Delta       string          `json:"delta"`
		Text        string          `json:"text"`
		ItemID      string          `json:"item_id"`
		Item        json.RawMessage `json:"item"`
		OutputIndex *int            `json:"output_index"`
		Response    *struct {
			Usage *struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
			Status string `json:"status"`
			Error  *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		} `json:"response"`
		Error *struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		return nil, err
	}
	switch event.Type {
	case "response.created", "response.in_progress":
		if n.started {
			return nil, nil
		}
		n.started = true
		return []proxycontract.NormalizedStreamEvent{{LogicalRequestID: requestID, Sequence: sequence, Type: "response_start"}}, nil
	case "response.output_text.delta":
		if event.OutputIndex == nil {
			return nil, errors.New("Responses text delta omitted output_index")
		}
		n.textOutputIndex = *event.OutputIndex
		text := event.Delta
		return []proxycontract.NormalizedStreamEvent{{LogicalRequestID: requestID, Sequence: sequence, Type: "text_delta", Text: &text}}, nil
	case "response.output_item.added":
		if event.OutputIndex == nil {
			return nil, errors.New("Responses output item omitted output_index")
		}
		var item struct {
			Type   string `json:"type"`
			ID     string `json:"id"`
			CallID string `json:"call_id"`
			Name   string `json:"name"`
		}
		if len(event.Item) == 0 || json.Unmarshal(event.Item, &item) != nil {
			return nil, errors.New("invalid Responses output item")
		}
		if item.Type == "reasoning" {
			if item.ID == "" {
				return nil, errors.New("reasoning output item omitted id")
			}
			if _, ok := n.reasoningItems[item.ID]; !ok {
				n.reasoningOrder = append(n.reasoningOrder, item.ID)
			}
			index := len(n.reasoningOrder) - 1
			if event.OutputIndex != nil {
				index = *event.OutputIndex
			}
			n.reasoningItems[item.ID] = reasoningOutput{raw: append(json.RawMessage(nil), event.Item...), index: index}
			return nil, nil
		}
		if item.Type != "function_call" {
			return nil, nil
		}
		n.sawFunctionCall = true
		callID := item.CallID
		if callID == "" {
			callID = item.ID
		}
		name := item.Name
		if callID == "" || name == "" {
			return nil, errors.New("invalid Responses function-call start")
		}
		if event.OutputIndex != nil {
			n.toolOutputIndexes[callID] = *event.OutputIndex
		}
		if item.ID != "" {
			n.responseCallIDs[item.ID] = callID
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
	case "response.output_item.done":
		var item struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		}
		if len(event.Item) > 0 && json.Unmarshal(event.Item, &item) == nil && item.Type == "reasoning" && item.ID != "" {
			if _, ok := n.reasoningItems[item.ID]; !ok {
				n.reasoningOrder = append(n.reasoningOrder, item.ID)
			}
			index := len(n.reasoningOrder) - 1
			if event.OutputIndex != nil {
				index = *event.OutputIndex
			}
			n.reasoningItems[item.ID] = reasoningOutput{raw: append(json.RawMessage(nil), event.Item...), index: index}
		}
		return nil, nil
	case "response.completed":
		if event.Response == nil {
			return nil, errors.New("completed Responses event omitted response")
		}
		if event.Response.Usage == nil {
			return nil, errors.New("completed Responses event omitted usage")
		}
		n.terminal = true
		usage := proxycontract.Usage{InputTokens: event.Response.Usage.InputTokens, OutputTokens: event.Response.Usage.OutputTokens}
		finish := proxycontract.FinishReason("stop")
		if n.sawFunctionCall {
			finish = "tool_call"
		}
		return []proxycontract.NormalizedStreamEvent{{LogicalRequestID: requestID, Sequence: sequence, Type: "usage", Usage: &usage}, {LogicalRequestID: requestID, Sequence: sequence + 1, Type: "response_end", FinishReason: &finish}}, nil
	case "response.incomplete":
		if event.Response == nil {
			return nil, errors.New("incomplete Responses event omitted response")
		}
		if event.Response.Usage == nil {
			return nil, errors.New("incomplete Responses event omitted usage")
		}
		n.terminal = true
		usage := proxycontract.Usage{InputTokens: event.Response.Usage.InputTokens, OutputTokens: event.Response.Usage.OutputTokens}
		finish := proxycontract.FinishReason("length")
		return []proxycontract.NormalizedStreamEvent{{LogicalRequestID: requestID, Sequence: sequence, Type: "usage", Usage: &usage}, {LogicalRequestID: requestID, Sequence: sequence + 1, Type: "response_end", FinishReason: &finish}}, nil
	case "response.failed", "error":
		code := ""
		if event.Error != nil {
			code = event.Error.Code
		}
		if code == "" && event.Response != nil && event.Response.Error != nil {
			code = event.Response.Error.Code
		}
		normalized := normalizeStreamFailure(code)
		return []proxycontract.NormalizedStreamEvent{{LogicalRequestID: requestID, Sequence: sequence, Type: "error", Error: &normalized}}, nil
	default:
		return nil, nil
	}
}

func (n *streamNormalizer) orderedReasoningItems() []reasoningOutput {
	out := make([]reasoningOutput, 0, len(n.reasoningOrder))
	for _, id := range n.reasoningOrder {
		item := n.reasoningItems[id]
		item.raw = append(json.RawMessage(nil), item.raw...)
		out = append(out, item)
	}
	return out
}

func normalizeStreamFailure(code string) proxycontract.NormalizedError {
	lower := strings.ToLower(code)
	normalized := proxycontract.NormalizedError{Code: "upstream_5xx", RetryClass: "before_first_byte_only", SafeMessage: "upstream stream returned an error", UpstreamStatus: 502}
	switch {
	case strings.Contains(lower, "rate") && strings.Contains(lower, "limit"):
		normalized.Code = "rate_limited"
		normalized.UpstreamStatus = 429
	case strings.Contains(lower, "context") && strings.Contains(lower, "length"):
		normalized.Code = "context_overflow"
		normalized.UpstreamStatus = 413
	case strings.Contains(lower, "model") && (strings.Contains(lower, "not_found") || strings.Contains(lower, "unavailable")):
		normalized.Code = "model_unavailable"
		normalized.UpstreamStatus = 404
	case strings.Contains(lower, "invalid") || strings.Contains(lower, "authentication") || strings.Contains(lower, "permission"):
		normalized.Code = "invalid_request"
		normalized.RetryClass = "never"
		normalized.UpstreamStatus = 400
	}
	return normalized
}

type sourceStreamEncoder struct {
	writer          io.Writer
	protocol        proxycontract.Protocol
	alias           string
	toolIndexes     map[string]int
	nextToolIndex   int
	requestID       string
	toolNames       map[string]string
	toolArguments   map[string]string
	text            strings.Builder
	usage           *proxycontract.Usage
	finishReason    proxycontract.FinishReason
	sequence        int
	started         bool
	textStarted     bool
	textIndex       int
	nextOutputIndex int
	reasoningItems  []reasoningOutput
}

func (s *sourceStreamEncoder) applyIndexHints(normalizer *streamNormalizer) {
	if normalizer == nil {
		return
	}
	if normalizer.textOutputIndex >= 0 {
		s.textIndex = normalizer.textOutputIndex
		if s.nextOutputIndex <= s.textIndex {
			s.nextOutputIndex = s.textIndex + 1
		}
	}
	for id, index := range normalizer.toolOutputIndexes {
		s.toolIndexes[id] = index
		if s.nextOutputIndex <= index {
			s.nextOutputIndex = index + 1
		}
	}
}

func (s *sourceStreamEncoder) emit(payload any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(s.writer, "data: %s\n\n", encoded)
	return err
}
func (s *sourceStreamEncoder) start() error {
	if s.started {
		return nil
	}
	s.started = true
	if s.protocol == proxycontract.ProtocolOpenAIChat {
		return s.emit(map[string]any{"id": "chatcmpl_" + s.requestID, "object": "chat.completion.chunk", "model": s.alias, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}, "finish_reason": nil}}})
	}
	response := s.responseObject("in_progress")
	if err := s.emit(map[string]any{"type": "response.created", "sequence_number": s.sequence, "response": response}); err != nil {
		return err
	}
	s.sequence++
	err := s.emit(map[string]any{"type": "response.in_progress", "sequence_number": s.sequence, "response": response})
	s.sequence++
	return err
}
func (s *sourceStreamEncoder) consume(event proxycontract.NormalizedStreamEvent) error {
	var payload any
	if s.protocol == proxycontract.ProtocolOpenAIChat {
		choice := map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": nil}
		if event.Type == "text_delta" {
			choice["delta"] = map[string]any{"content": *event.Text}
		}
		if event.Type == "response_end" {
			s.finishReason = *event.FinishReason
			return nil
		}
		if event.Type == "tool_call_start" {
			index := s.nextToolIndex
			s.nextToolIndex++
			s.toolIndexes[*event.CallID] = index
			choice["delta"] = map[string]any{"tool_calls": []any{map[string]any{"index": index, "id": *event.CallID, "type": "function", "function": map[string]any{"name": *event.ToolName, "arguments": ""}}}}
			s.toolNames[*event.CallID] = *event.ToolName
		}
		if event.Type == "tool_arguments_delta" {
			index, ok := s.toolIndexes[*event.CallID]
			if !ok {
				return errors.New("tool argument delta preceded tool start")
			}
			choice["delta"] = map[string]any{"tool_calls": []any{map[string]any{"index": index, "function": map[string]any{"arguments": *event.ArgumentsDelta}}}}
			s.toolArguments[*event.CallID] += *event.ArgumentsDelta
		}
		if event.Type == "response_start" {
			return nil
		}
		if event.Type == "usage" {
			copy := *event.Usage
			s.usage = &copy
			return nil
		}
		if event.Type == "text_delta" {
			s.text.WriteString(*event.Text)
		}
		object := map[string]any{"id": "chatcmpl_" + event.LogicalRequestID, "object": "chat.completion.chunk", "model": s.alias, "choices": []any{choice}}
		payload = object
	} else {
		if event.Type == "response_start" {
			return nil
		}
		if event.Type == "usage" {
			copy := *event.Usage
			s.usage = &copy
			return nil
		}
		if event.Type == "response_end" {
			s.finishReason = *event.FinishReason
			return nil
		}
		if event.Type == "text_delta" && !s.textStarted {
			s.textStarted = true
			if s.textIndex < 0 {
				s.textIndex = s.nextOutputIndex
				s.nextOutputIndex++
			}
			messageID := "msg_" + s.requestID
			if err := s.emit(map[string]any{"type": "response.output_item.added", "sequence_number": s.sequence, "output_index": s.textIndex, "item": map[string]any{"id": messageID, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}}); err != nil {
				return err
			}
			s.sequence++
			if err := s.emit(map[string]any{"type": "response.content_part.added", "sequence_number": s.sequence, "item_id": messageID, "output_index": s.textIndex, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}}); err != nil {
				return err
			}
			s.sequence++
		}
		kind := map[proxycontract.StreamEventType]string{"text_delta": "response.output_text.delta", "tool_call_start": "response.output_item.added", "tool_arguments_delta": "response.function_call_arguments.delta"}[event.Type]
		payload = map[string]any{"type": kind, "sequence_number": s.sequence}
		s.sequence++
		if event.Text != nil {
			payload.(map[string]any)["item_id"] = "msg_" + s.requestID
			payload.(map[string]any)["output_index"] = s.textIndex
			payload.(map[string]any)["content_index"] = 0
			payload.(map[string]any)["delta"] = *event.Text
			s.text.WriteString(*event.Text)
		}
		if event.Usage != nil {
			payload.(map[string]any)["usage"] = event.Usage
		}
		if event.Type == "tool_call_start" {
			s.toolNames[*event.CallID] = *event.ToolName
			if _, ok := s.toolIndexes[*event.CallID]; !ok {
				s.toolIndexes[*event.CallID] = s.nextOutputIndex
				s.nextOutputIndex++
			}
			payload.(map[string]any)["output_index"] = s.toolIndexes[*event.CallID]
			payload.(map[string]any)["item"] = map[string]any{"id": "fc_" + *event.CallID, "type": "function_call", "call_id": *event.CallID, "name": *event.ToolName, "arguments": ""}
		}
		if event.Type == "tool_arguments_delta" {
			s.toolArguments[*event.CallID] += *event.ArgumentsDelta
			payload.(map[string]any)["item_id"] = "fc_" + *event.CallID
			payload.(map[string]any)["output_index"] = s.toolIndexes[*event.CallID]
			payload.(map[string]any)["delta"] = *event.ArgumentsDelta
		}
	}
	return s.emit(payload)
}

func (s *sourceStreamEncoder) finish() error {
	if s.finishReason == "" {
		return errors.New("stream omitted finish reason")
	}
	if s.usage == nil {
		return errors.New("stream omitted required usage")
	}
	if err := s.validateOutputIndexes(); err != nil {
		return err
	}
	if s.protocol == proxycontract.ProtocolOpenAIChat {
		finish := string(s.finishReason)
		if finish == "tool_call" {
			finish = "tool_calls"
		}
		if err := s.emit(map[string]any{"id": "chatcmpl_" + s.requestID, "object": "chat.completion.chunk", "model": s.alias, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}}); err != nil {
			return err
		}
		if s.usage != nil {
			if err := s.emit(map[string]any{"id": "chatcmpl_" + s.requestID, "object": "chat.completion.chunk", "model": s.alias, "choices": []any{}, "usage": map[string]any{"prompt_tokens": s.usage.InputTokens, "completion_tokens": s.usage.OutputTokens, "total_tokens": s.usage.InputTokens + s.usage.OutputTokens}}); err != nil {
				return err
			}
		}
	} else {
		status := "completed"
		if s.finishReason == "length" {
			status = "incomplete"
		}
		eventType := "response.completed"
		if status == "incomplete" {
			eventType = "response.incomplete"
		}
		actions := map[int]func() error{}
		for _, reasoning := range s.reasoningItems {
			reasoning := reasoning
			actions[reasoning.index] = func() error {
				var item map[string]any
				if json.Unmarshal(reasoning.raw, &item) != nil {
					return nil
				}
				if err := s.emit(map[string]any{"type": "response.output_item.added", "sequence_number": s.sequence, "output_index": reasoning.index, "item": item}); err != nil {
					return err
				}
				s.sequence++
				if err := s.emit(map[string]any{"type": "response.output_item.done", "sequence_number": s.sequence, "output_index": reasoning.index, "item": item}); err != nil {
					return err
				}
				s.sequence++
				return nil
			}
		}
		if s.textStarted {
			actions[s.textIndex] = func() error {
				messageID := "msg_" + s.requestID
				if err := s.emit(map[string]any{"type": "response.output_text.done", "sequence_number": s.sequence, "item_id": messageID, "output_index": s.textIndex, "content_index": 0, "text": s.text.String()}); err != nil {
					return err
				}
				s.sequence++
				part := map[string]any{"type": "output_text", "text": s.text.String(), "annotations": []any{}}
				if err := s.emit(map[string]any{"type": "response.content_part.done", "sequence_number": s.sequence, "item_id": messageID, "output_index": s.textIndex, "content_index": 0, "part": part}); err != nil {
					return err
				}
				s.sequence++
				if err := s.emit(map[string]any{"type": "response.output_item.done", "sequence_number": s.sequence, "output_index": s.textIndex, "item": map[string]any{"id": messageID, "type": "message", "role": "assistant", "status": "completed", "content": []any{part}}}); err != nil {
					return err
				}
				s.sequence++
				return nil
			}
		}
		for id, index := range s.toolIndexes {
			id, index := id, index
			actions[index] = func() error {
				if err := s.emit(map[string]any{"type": "response.function_call_arguments.done", "sequence_number": s.sequence, "item_id": "fc_" + id, "output_index": index, "arguments": s.toolArguments[id]}); err != nil {
					return err
				}
				s.sequence++
				item := map[string]any{"id": "fc_" + id, "type": "function_call", "call_id": id, "name": s.toolNames[id], "arguments": s.toolArguments[id]}
				if err := s.emit(map[string]any{"type": "response.output_item.done", "sequence_number": s.sequence, "output_index": index, "item": item}); err != nil {
					return err
				}
				s.sequence++
				return nil
			}
		}
		indexes := make([]int, 0, len(actions))
		for index := range actions {
			indexes = append(indexes, index)
		}
		sort.Ints(indexes)
		for _, index := range indexes {
			if err := actions[index](); err != nil {
				return err
			}
		}
		if err := s.emit(map[string]any{"type": eventType, "sequence_number": s.sequence, "response": s.responseObject(status)}); err != nil {
			return err
		}
	}
	_, err := fmt.Fprint(s.writer, "data: [DONE]\n\n")
	return err
}

func (s *sourceStreamEncoder) validateOutputIndexes() error {
	if s.protocol != proxycontract.ProtocolOpenAIResponses {
		return nil
	}
	seen := map[int]bool{}
	claim := func(index int) error {
		if index < 0 || seen[index] {
			return errors.New("Responses stream has missing or duplicate output indexes")
		}
		seen[index] = true
		return nil
	}
	for _, item := range s.reasoningItems {
		if err := claim(item.index); err != nil {
			return err
		}
	}
	if s.textStarted {
		if err := claim(s.textIndex); err != nil {
			return err
		}
	}
	for _, index := range s.toolIndexes {
		if err := claim(index); err != nil {
			return err
		}
	}
	return nil
}

func (s *sourceStreamEncoder) fail(err error) error {
	code, message := "upstream_invalid_response", "upstream stream failed"
	var api *APIError
	if errors.As(err, &api) {
		code, message = api.Code, api.Message
	}
	if s.protocol == proxycontract.ProtocolOpenAIChat {
		if emitErr := s.emit(map[string]any{"error": map[string]any{"code": code, "message": message, "type": "blazn_proxy_error"}}); emitErr != nil {
			return emitErr
		}
	} else {
		response := s.responseObject("failed")
		response["error"] = map[string]any{"code": code, "message": message}
		if emitErr := s.emit(map[string]any{"type": "response.failed", "sequence_number": s.sequence, "response": response}); emitErr != nil {
			return emitErr
		}
		s.sequence++
		if emitErr := s.emit(map[string]any{"type": "error", "sequence_number": s.sequence, "code": code, "message": message}); emitErr != nil {
			return emitErr
		}
	}
	_, writeErr := fmt.Fprint(s.writer, "data: [DONE]\n\n")
	return writeErr
}

func (s *sourceStreamEncoder) responseObject(status string) map[string]any {
	items := map[int]any{}
	for _, reasoning := range s.reasoningItems {
		var item any
		if json.Unmarshal(reasoning.raw, &item) == nil {
			items[reasoning.index] = item
		}
	}
	if s.text.Len() > 0 {
		content := []any{map[string]any{"type": "output_text", "text": s.text.String(), "annotations": []any{}}}
		items[s.textIndex] = map[string]any{"id": "msg_" + s.requestID, "type": "message", "role": "assistant", "status": "completed", "content": content}
	}
	ids := make([]string, 0, len(s.toolIndexes))
	for id := range s.toolIndexes {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return s.toolIndexes[ids[i]] < s.toolIndexes[ids[j]] })
	for _, id := range ids {
		items[s.toolIndexes[id]] = map[string]any{"id": "fc_" + id, "type": "function_call", "call_id": id, "name": s.toolNames[id], "arguments": s.toolArguments[id]}
	}
	indexes := make([]int, 0, len(items))
	for index := range items {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	output := make([]any, 0, len(indexes))
	for _, index := range indexes {
		output = append(output, items[index])
	}
	usage := map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}
	if s.usage != nil {
		usage = map[string]any{"input_tokens": s.usage.InputTokens, "output_tokens": s.usage.OutputTokens, "total_tokens": s.usage.InputTokens + s.usage.OutputTokens}
	}
	return map[string]any{"id": "resp_" + s.requestID, "object": "response", "status": status, "model": s.alias, "output": output, "usage": usage}
}
