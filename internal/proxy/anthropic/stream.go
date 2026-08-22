package anthropic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/KingJammin/blazn/internal/proxycontract"
)

type streamEnvelope struct {
	Type         string          `json:"type"`
	Index        *int            `json:"index,omitempty"`
	Message      json.RawMessage `json:"message,omitempty"`
	ContentBlock json.RawMessage `json:"content_block,omitempty"`
	Delta        json.RawMessage `json:"delta,omitempty"`
	Usage        json.RawMessage `json:"usage,omitempty"`
	Error        json.RawMessage `json:"error,omitempty"`
}

type streamMessage struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"`
	Role         string         `json:"role"`
	Content      []contentBlock `json:"content"`
	Model        string         `json:"model"`
	StopReason   *string        `json:"stop_reason"`
	StopSequence *string        `json:"stop_sequence"`
	Usage        usage          `json:"usage"`
}

type streamDelta struct {
	Type         string  `json:"type,omitempty"`
	Text         string  `json:"text,omitempty"`
	PartialJSON  string  `json:"partial_json,omitempty"`
	StopReason   *string `json:"stop_reason,omitempty"`
	StopSequence *string `json:"stop_sequence,omitempty"`
}

type streamError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type StreamDecoder struct {
	scanner      *bufio.Scanner
	requestID    string
	sequence     int
	started      bool
	terminal     bool
	messageDelta bool
	blocks       map[int]contentBlock
	closed       map[int]bool
	usage        proxycontract.Usage
	pending      []proxycontract.NormalizedStreamEvent
}

func NewStreamDecoder(reader io.Reader, requestID string) *StreamDecoder {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), MaxRequestBytes)
	return &StreamDecoder{scanner: scanner, requestID: requestID, blocks: map[int]contentBlock{}, closed: map[int]bool{}}
}

// Next returns one normalized event. done is true only after a valid
// message_stop has been consumed. It rejects missing, duplicate, and reordered
// Anthropic events, so cross-protocol adapters never repair a lossy stream.
func (d *StreamDecoder) Next() (event proxycontract.NormalizedStreamEvent, done bool, err error) {
	return d.NextContext(context.Background())
}

func (d *StreamDecoder) NextContext(ctx context.Context) (event proxycontract.NormalizedStreamEvent, done bool, err error) {
	if len(d.pending) > 0 {
		event, d.pending = d.pending[0], d.pending[1:]
		return event, false, nil
	}
	if d.terminal {
		return event, true, nil
	}
	select {
	case <-ctx.Done():
		return event, false, &Error{Code: "cancelled", Message: "request was cancelled", Status: 499, Cause: ctx.Err()}
	default:
	}
	eventName, data, readErr := readSSE(d.scanner)
	if readErr != nil {
		if errors.Is(readErr, io.EOF) {
			return event, false, upstream("Anthropic stream ended without message_stop")
		}
		return event, false, &Error{Code: "connection_failure", Message: "upstream stream failed", Status: 502, Retryable: true, Cause: readErr}
	}
	var envelope streamEnvelope
	if err := decodeStrict(bytes.NewReader(data), &envelope); err != nil || envelope.Type == "" || eventName != envelope.Type {
		return event, false, upstream("upstream returned an invalid Anthropic stream event")
	}
	allowed := map[string][]string{
		"ping": {"type"}, "message_start": {"type", "message"},
		"content_block_start": {"type", "index", "content_block"},
		"content_block_delta": {"type", "index", "delta"},
		"content_block_stop":  {"type", "index"},
		"message_delta":       {"type", "delta", "usage"},
		"message_stop":        {"type"}, "error": {"type", "error"},
	}[envelope.Type]
	if allowed == nil || !hasExactKeys(data, allowed...) {
		return event, false, upstream("Anthropic stream event contains fields from another event type")
	}
	emit := func(value proxycontract.NormalizedStreamEvent) (proxycontract.NormalizedStreamEvent, bool, error) {
		d.sequence++
		value.LogicalRequestID, value.Sequence = d.requestID, d.sequence
		if err := value.Validate(); err != nil {
			return event, false, upstream("Anthropic stream event failed normalization")
		}
		return value, false, nil
	}
	switch envelope.Type {
	case "ping":
		return d.NextContext(ctx)
	case "message_start":
		if d.started || len(envelope.Message) == 0 {
			return event, false, upstream("duplicate or invalid message_start")
		}
		var message streamMessage
		if decodeStrict(bytes.NewReader(envelope.Message), &message) != nil || message.ID == "" || message.Type != "message" || message.Role != "assistant" || message.Model == "" || len(message.Content) != 0 || message.StopReason != nil || message.StopSequence != nil || message.Usage.InputTokens < 0 {
			return event, false, upstream("invalid message_start payload")
		}
		d.started = true
		d.usage.InputTokens = message.Usage.InputTokens
		initial := proxycontract.Usage{InputTokens: message.Usage.InputTokens, OutputTokens: 0}
		return emit(proxycontract.NormalizedStreamEvent{Type: "response_start", Usage: &initial})
	case "content_block_start":
		if !d.started || d.messageDelta || envelope.Index == nil || len(envelope.ContentBlock) == 0 {
			return event, false, upstream("content_block_start is out of order")
		}
		index := *envelope.Index
		if index < 0 {
			return event, false, upstream("content block index is invalid")
		}
		if _, exists := d.blocks[index]; exists {
			return event, false, upstream("content block index is duplicated")
		}
		var block contentBlock
		if decodeStrict(bytes.NewReader(envelope.ContentBlock), &block) != nil {
			return event, false, upstream("invalid content_block_start")
		}
		if err := validateContentBlock(block); err != nil {
			return event, false, upstream("invalid content block union")
		}
		d.blocks[index] = block
		switch block.Type {
		case "text":
			if block.Text != "" {
				return event, false, upstream("text block start must be empty")
			}
			return d.NextContext(ctx)
		case "tool_use":
			if block.ID == "" || block.Name == "" || !isJSONObject(block.Input) {
				return event, false, upstream("invalid tool_use block start")
			}
			return emit(proxycontract.NormalizedStreamEvent{Type: "tool_call_start", CallID: ptr(block.ID), ToolName: ptr(block.Name)})
		default:
			return event, false, upstream("unsupported Anthropic stream content")
		}
	case "content_block_delta":
		if !d.started || d.messageDelta || envelope.Index == nil || len(envelope.Delta) == 0 {
			return event, false, upstream("content_block_delta is out of order")
		}
		block, exists := d.blocks[*envelope.Index]
		if !exists || d.closed[*envelope.Index] {
			return event, false, upstream("content block delta has no open block")
		}
		var delta streamDelta
		if decodeStrict(bytes.NewReader(envelope.Delta), &delta) != nil {
			return event, false, upstream("invalid content block delta")
		}
		if delta.StopReason != nil || delta.StopSequence != nil {
			return event, false, upstream("content delta contains message fields")
		}
		if block.Type == "text" && delta.Type == "text_delta" && delta.PartialJSON == "" {
			return emit(proxycontract.NormalizedStreamEvent{Type: "text_delta", Text: ptr(delta.Text)})
		}
		if block.Type == "tool_use" && delta.Type == "input_json_delta" && delta.Text == "" {
			return emit(proxycontract.NormalizedStreamEvent{Type: "tool_arguments_delta", CallID: ptr(block.ID), ArgumentsDelta: ptr(delta.PartialJSON)})
		}
		return event, false, upstream("content block delta does not match its block")
	case "content_block_stop":
		if !d.started || d.messageDelta || envelope.Index == nil {
			return event, false, upstream("content_block_stop is out of order")
		}
		if _, exists := d.blocks[*envelope.Index]; !exists || d.closed[*envelope.Index] {
			return event, false, upstream("content block stop has no open block")
		}
		d.closed[*envelope.Index] = true
		return d.NextContext(ctx)
	case "message_delta":
		if !d.started || d.messageDelta || len(envelope.Delta) == 0 || len(envelope.Usage) == 0 {
			return event, false, upstream("message_delta is out of order")
		}
		for index := range d.blocks {
			if !d.closed[index] {
				return event, false, upstream("message_delta preceded content_block_stop")
			}
		}
		var delta streamDelta
		var update struct {
			OutputTokens int `json:"output_tokens"`
		}
		if decodeStrict(bytes.NewReader(envelope.Delta), &delta) != nil || decodeStrict(bytes.NewReader(envelope.Usage), &update) != nil || delta.StopReason == nil || update.OutputTokens < 0 {
			return event, false, upstream("invalid message_delta")
		}
		if delta.Type != "" || delta.Text != "" || delta.PartialJSON != "" {
			return event, false, upstream("message_delta contains content fields")
		}
		finish, finishErr := finishReason(*delta.StopReason)
		if finishErr != nil {
			return event, false, finishErr
		}
		d.messageDelta = true
		d.usage.OutputTokens = update.OutputTokens
		d.sequence++
		usageEvent := proxycontract.NormalizedStreamEvent{LogicalRequestID: d.requestID, Sequence: d.sequence, Type: "usage", Usage: &proxycontract.Usage{InputTokens: d.usage.InputTokens, OutputTokens: d.usage.OutputTokens}}
		d.sequence++
		finishEvent := proxycontract.NormalizedStreamEvent{LogicalRequestID: d.requestID, Sequence: d.sequence, Type: "response_end", FinishReason: &finish}
		if usageEvent.Validate() != nil || finishEvent.Validate() != nil {
			return event, false, upstream("message_delta failed normalization")
		}
		d.pending = append(d.pending, finishEvent)
		return usageEvent, false, nil
	case "message_stop":
		if !d.started || !d.messageDelta || len(d.pending) > 0 {
			return event, false, upstream("message_stop is out of order")
		}
		d.terminal = true
		return event, true, nil
	case "error":
		if len(envelope.Error) == 0 {
			return event, false, upstream("invalid Anthropic error event")
		}
		var source streamError
		if decodeStrict(bytes.NewReader(envelope.Error), &source) != nil || source.Message == "" {
			return event, false, upstream("invalid Anthropic error event")
		}
		code, status, retry := "upstream_5xx", 502, true
		switch source.Type {
		case "rate_limit_error":
			code, status = "rate_limited", 429
		case "overloaded_error":
			code, status = "upstream_5xx", 529
		case "authentication_error", "permission_error":
			code, status, retry = "credential_unavailable", 503, false
		case "not_found_error":
			code, status = "model_unavailable", 404
		case "invalid_request_error":
			code, status, retry = "invalid_request", 400, false
		case "api_error":
		default:
			return event, false, upstream("unknown Anthropic error type")
		}
		return event, false, &Error{Code: code, Message: "upstream Anthropic request failed", Status: status, Retryable: retry}
	default:
		return event, false, upstream("unknown Anthropic stream event")
	}
}

func hasExactKeys(raw []byte, keys ...string) bool {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || len(object) != len(keys) {
		return false
	}
	for _, key := range keys {
		if _, ok := object[key]; !ok {
			return false
		}
	}
	return true
}

func readSSE(scanner *bufio.Scanner) (string, []byte, error) {
	var event string
	var data []string
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if event == "" && len(data) == 0 {
				continue
			}
			if event == "" || len(data) == 0 {
				return "", nil, errors.New("incomplete SSE event")
			}
			return event, []byte(strings.Join(data, "\n")), nil
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "event:") {
			if event != "" {
				return "", nil, errors.New("duplicate event field")
			}
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			continue
		}
		if strings.HasPrefix(line, "id:") {
			continue
		}
		return "", nil, errors.New("unsupported SSE field")
	}
	if err := scanner.Err(); err != nil {
		return "", nil, err
	}
	return "", nil, io.EOF
}

type StreamEncoder struct {
	writer      io.Writer
	request     proxycontract.NormalizedRequest
	started     bool
	published   bool
	terminal    bool
	sequence    int
	nextIndex   int
	textIndex   int
	openText    bool
	toolIndexes map[string]int
	openTools   map[string]bool
	usage       *proxycontract.Usage
	finish      *proxycontract.FinishReason
}

func NewStreamEncoder(writer io.Writer, request proxycontract.NormalizedRequest) *StreamEncoder {
	return &StreamEncoder{writer: writer, request: request, textIndex: -1, toolIndexes: map[string]int{}, openTools: map[string]bool{}}
}

func (s *StreamEncoder) Start() error {
	if s.started {
		return errors.New("Anthropic source stream already started")
	}
	s.started = true
	return nil
}

func (s *StreamEncoder) Consume(event proxycontract.NormalizedStreamEvent) error {
	if !s.started || s.terminal {
		return errors.New("Anthropic source stream is not writable")
	}
	if event.LogicalRequestID != s.request.LogicalRequestID {
		return errors.New("stream event request ID mismatch")
	}
	switch event.Type {
	case "response_start":
		if s.published {
			return errors.New("duplicate normalized response_start")
		}
		inputTokens := 0
		if event.Usage != nil {
			inputTokens = event.Usage.InputTokens
		}
		s.published = true
		return s.emit("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_" + s.request.LogicalRequestID, "type": "message", "role": "assistant", "content": []any{}, "model": s.request.ModelAlias, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": inputTokens, "output_tokens": 0}}})
	case "text_delta":
		if !s.published {
			return errors.New("text delta preceded response_start")
		}
		if !s.openText {
			if err := s.closeTools(); err != nil {
				return err
			}
			s.textIndex = s.nextIndex
			s.nextIndex++
			s.openText = true
			if err := s.emit("content_block_start", map[string]any{"type": "content_block_start", "index": s.textIndex, "content_block": map[string]any{"type": "text", "text": ""}}); err != nil {
				return err
			}
		}
		return s.emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": s.textIndex, "delta": map[string]any{"type": "text_delta", "text": *event.Text}})
	case "tool_call_start":
		if !s.published {
			return errors.New("tool call preceded response_start")
		}
		if err := s.closeText(); err != nil {
			return err
		}
		if _, exists := s.toolIndexes[*event.CallID]; exists {
			return errors.New("duplicate tool call ID")
		}
		index := s.nextIndex
		s.nextIndex++
		s.toolIndexes[*event.CallID] = index
		s.openTools[*event.CallID] = true
		return s.emit("content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]any{"type": "tool_use", "id": *event.CallID, "name": *event.ToolName, "input": map[string]any{}}})
	case "tool_arguments_delta":
		if !s.published {
			return errors.New("tool arguments preceded response_start")
		}
		index, exists := s.toolIndexes[*event.CallID]
		if !exists || !s.openTools[*event.CallID] {
			return errors.New("tool arguments preceded tool start")
		}
		return s.emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "input_json_delta", "partial_json": *event.ArgumentsDelta}})
	case "usage":
		copy := *event.Usage
		s.usage = &copy
		return nil
	case "response_end":
		copy := *event.FinishReason
		s.finish = &copy
		return nil
	case "error":
		return s.Error(&Error{Code: string(event.Error.Code), Message: event.Error.SafeMessage, Status: event.Error.UpstreamStatus})
	default:
		return errors.New("unsupported normalized stream event")
	}
}

func (s *StreamEncoder) Finish() error {
	if !s.started || !s.published || s.terminal || s.usage == nil || s.finish == nil {
		return errors.New("Anthropic stream is missing terminal usage or finish reason")
	}
	if err := s.closeText(); err != nil {
		return err
	}
	if err := s.closeTools(); err != nil {
		return err
	}
	stop := map[proxycontract.FinishReason]string{"stop": "end_turn", "length": "max_tokens", "tool_call": "tool_use", "content_filter": "refusal"}[*s.finish]
	if stop == "" {
		return errors.New("finish reason cannot be represented by Anthropic")
	}
	if err := s.emit("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": s.usage.OutputTokens}}); err != nil {
		return err
	}
	if err := s.emit("message_stop", map[string]any{"type": "message_stop"}); err != nil {
		return err
	}
	s.terminal = true
	return nil
}

func (s *StreamEncoder) Error(err error) error {
	if s.terminal {
		return nil
	}
	api := &Error{Code: "upstream_5xx", Message: "upstream request failed", Status: 502}
	if !errors.As(err, &api) {
		api = &Error{Code: "upstream_5xx", Message: "upstream request failed", Status: 502}
	}
	typeName := "api_error"
	switch api.Code {
	case "rate_limited":
		typeName = "rate_limit_error"
	case "authentication_failed", "credential_unavailable":
		typeName = "authentication_error"
	case "invalid_request", "unsupported_capability":
		typeName = "invalid_request_error"
	case "model_not_found", "model_unavailable":
		typeName = "not_found_error"
	}
	s.terminal = true
	return s.emit("error", map[string]any{"type": "error", "error": map[string]any{"type": typeName, "message": api.Message}})
}

func (s *StreamEncoder) Cancel() error {
	return s.Error(&Error{Code: "cancelled", Message: "request was cancelled", Status: 499})
}

func (s *StreamEncoder) closeText() error {
	if !s.openText {
		return nil
	}
	s.openText = false
	return s.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": s.textIndex})
}
func (s *StreamEncoder) closeTools() error {
	for id, open := range s.openTools {
		if open {
			if err := s.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": s.toolIndexes[id]}); err != nil {
				return err
			}
			s.openTools[id] = false
		}
	}
	return nil
}
func (s *StreamEncoder) emit(name string, value any) error {
	s.sequence++
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(s.writer, "id: %s:%d\nevent: %s\ndata: %s\n\n", s.request.LogicalRequestID, s.sequence, name, encoded)
	return err
}
