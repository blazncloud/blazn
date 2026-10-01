package sandboxagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type chatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function toolFunction `json:"function"`
}

type toolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type modelRequest struct {
	Model      string           `json:"model"`
	Messages   []chatMessage    `json:"messages"`
	Tools      []map[string]any `json:"tools"`
	ToolChoice string           `json:"tool_choice"`
	MaxTokens  int              `json:"max_tokens,omitempty"`
}

type modelResponse struct {
	Choices []struct {
		Message struct {
			Content   *string    `json:"content"`
			ToolCalls []toolCall `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
}

type persistedState struct {
	Done          []string `json:"done"`
	Steps         int      `json:"steps"`
	Turns         int      `json:"turns"`
	LastAssistant string   `json:"lastAssistant"`
	Finalized     bool     `json:"finalized"`
	// CurrentMessage is set while a turn is in flight, so a restarted agent
	// can report that the turn was lost instead of leaving it open forever.
	CurrentMessage string `json:"currentMessage,omitempty"`
}

// Agent is the in-Sandbox turn loop.
type Agent struct {
	directory     string
	config        Config
	outbox        *Outbox
	history       []chatMessage
	done          map[string]bool
	state         persistedState
	now           func() time.Time
	poll          time.Duration
	requestNumber int
}

// Options adjusts timing for tests.
type Options struct {
	Now  func() time.Time
	Poll time.Duration
}

// New loads configuration and any persisted conversation from directory.
func New(directory string, options Options) (*Agent, error) {
	config, err := LoadConfig(directory)
	if err != nil {
		return nil, err
	}
	for _, name := range []string{inboxDir, outputDir} {
		if err := os.MkdirAll(filepath.Join(directory, name), 0o700); err != nil {
			return nil, err
		}
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Poll <= 0 {
		options.Poll = 150 * time.Millisecond
	}
	outbox, err := OpenOutbox(directory, options.Now)
	if err != nil {
		return nil, err
	}
	agent := &Agent{directory: directory, config: config, outbox: outbox, done: map[string]bool{}, now: options.Now, poll: options.Poll}
	if data, err := readBounded(filepath.Join(directory, stateFile), maxInboxBytes); err == nil {
		if json.Unmarshal(data, &agent.state) != nil {
			return nil, errors.New("agent state is corrupt")
		}
		for _, name := range agent.state.Done {
			agent.done[name] = true
		}
	}
	if data, err := readBounded(filepath.Join(directory, historyFile), 64<<20); err == nil {
		if json.Unmarshal(data, &agent.history) != nil {
			return nil, errors.New("agent history is corrupt")
		}
	}
	if len(agent.history) == 0 {
		agent.history = []chatMessage{{Role: "system", Content: systemPrompt(config)}}
	}
	return agent, nil
}

func systemPrompt(config Config) string {
	var b strings.Builder
	b.WriteString(config.Instructions)
	b.WriteString("\n\n## Environment\n")
	b.WriteString("You are running inside an isolated Blazn sandbox with no network access. ")
	fmt.Fprintf(&b, "Your working directory is %s.", config.WorkingDirectory)
	if config.RepositoryDirectory != "" {
		fmt.Fprintf(&b, " The project repository is checked out at %s.", config.RepositoryDirectory)
	}
	b.WriteString(" Use the run_command, read_file and write_file tools to inspect and change files. ")
	b.WriteString("When the work for the current message is done, reply with a concise plain-text answer and no tool call.")
	return b.String()
}

// Serve processes inbox items until a shutdown or finalize item, or until ctx ends.
func (a *Agent) Serve(ctx context.Context) error {
	defer a.outbox.Close()
	if _, err := a.outbox.Emit(EventStarted, map[string]any{"version": Version, "pid": os.Getpid(), "runId": a.config.RunID}); err != nil {
		return err
	}
	if lost := a.state.CurrentMessage; lost != "" {
		a.state.CurrentMessage = ""
		if err := a.persist(); err != nil {
			return err
		}
		if _, err := a.outbox.Emit(EventTurnCompleted, map[string]any{"messageId": lost, "status": "failed", "errorCode": "agent_restarted", "steps": 0}); err != nil {
			return err
		}
	}
	for {
		items, err := pendingInbox(a.directory, a.done)
		if err != nil {
			return err
		}
		for _, item := range items {
			stop, err := a.handle(ctx, item)
			if err != nil {
				return err
			}
			if stop {
				_, err := a.outbox.Emit(EventStopped, map[string]any{"reason": item.Type})
				return err
			}
			break // re-read the inbox: a turn may have consumed later items
		}
		if len(items) == 0 {
			select {
			case <-ctx.Done():
				_, _ = a.outbox.Emit(EventStopped, map[string]any{"reason": "signal"})
				return nil
			case <-time.After(a.poll):
			}
		}
	}
}

func (a *Agent) handle(ctx context.Context, item InboxItem) (bool, error) {
	switch item.Type {
	case InboxMessage:
		if item.ID == "" || item.Content == "" {
			return false, a.finish(item, EventError, map[string]any{"code": "inbox_invalid", "item": item.name})
		}
		if err := a.markDone(item.name); err != nil {
			return false, err
		}
		return a.runTurn(ctx, item)
	case InboxFinalize:
		if err := a.finalize(ctx); err != nil {
			return false, err
		}
		return true, a.markDone(item.name)
	case InboxShutdown:
		return true, a.markDone(item.name)
	case InboxCancel, InboxModelResponse:
		return false, a.markDone(item.name) // nothing is in flight; a stale item is dropped
	default:
		return false, a.finish(item, EventError, map[string]any{"code": "inbox_invalid", "item": item.name})
	}
}

func (a *Agent) finish(item InboxItem, eventType string, data any) error {
	if _, err := a.outbox.Emit(eventType, data); err != nil {
		return err
	}
	return a.markDone(item.name)
}

func (a *Agent) markDone(name string) error {
	a.done[name] = true
	a.state.Done = append(a.state.Done, name)
	if err := a.persist(); err != nil {
		return err
	}
	// Removal keeps the directory small; a file that cannot be removed stays
	// recorded as done.
	if os.Remove(filepath.Join(a.directory, inboxDir, name)) == nil {
		delete(a.done, name)
		kept := a.state.Done[:0]
		for _, value := range a.state.Done {
			if value != name {
				kept = append(kept, value)
			}
		}
		a.state.Done = kept
		return a.persist()
	}
	return nil
}

func (a *Agent) persist() error {
	state, err := json.Marshal(a.state)
	if err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(a.directory, stateFile), state); err != nil {
		return err
	}
	history, err := json.Marshal(a.history)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(a.directory, historyFile), history)
}

// runTurn answers one user message. It returns stop=true when a shutdown
// arrived while the turn was waiting for the model.
func (a *Agent) runTurn(ctx context.Context, message InboxItem) (bool, error) {
	a.state.Turns++
	a.state.CurrentMessage = message.ID
	a.history = append(a.history, chatMessage{Role: "user", Content: message.Content})
	if err := a.persist(); err != nil {
		return false, err
	}
	if _, err := a.outbox.Emit(EventMessageAccepted, map[string]any{"messageId": message.ID}); err != nil {
		return false, err
	}
	complete := func(status, code string, steps int) error {
		data := map[string]any{"messageId": message.ID, "status": status, "steps": steps}
		if code != "" {
			data["errorCode"] = code
		}
		a.state.CurrentMessage = ""
		if err := a.persist(); err != nil {
			return err
		}
		_, err := a.outbox.Emit(EventTurnCompleted, data)
		return err
	}
	for step := 1; step <= a.config.MaxStepsPerTurn; step++ {
		a.state.Steps++
		a.compact()
		response, outcome, err := a.callModel(ctx)
		if err != nil {
			return false, err
		}
		switch outcome {
		case "shutdown":
			return true, complete("cancelled", "shutdown", step)
		case "cancelled":
			return false, complete("cancelled", "cancelled", step)
		case "ok":
		default:
			return false, complete("failed", outcome, step)
		}
		if len(response.Choices) == 0 {
			return false, complete("failed", "model_response_invalid", step)
		}
		reply := response.Choices[0].Message
		content := ""
		if reply.Content != nil {
			content = *reply.Content
		}
		if len(reply.ToolCalls) == 0 {
			a.history = append(a.history, chatMessage{Role: "assistant", Content: content})
			a.state.LastAssistant = content
			if _, err := a.outbox.Emit(EventAssistantMessage, map[string]any{"messageId": message.ID, "content": content}); err != nil {
				return false, err
			}
			return false, complete("completed", "", step)
		}
		a.history = append(a.history, chatMessage{Role: "assistant", Content: content, ToolCalls: reply.ToolCalls})
		for _, call := range reply.ToolCalls {
			if _, err := a.outbox.Emit(EventToolStarted, map[string]any{"callId": call.ID, "name": call.Function.Name, "summary": toolSummary(call)}); err != nil {
				return false, err
			}
			result := runTool(ctx, a.config, call)
			a.history = append(a.history, chatMessage{Role: "tool", ToolCallID: call.ID, Content: result.Output})
			if _, err := a.outbox.Emit(EventToolFinished, map[string]any{"callId": call.ID, "name": call.Function.Name, "ok": result.OK,
				"exitCode": result.ExitCode, "outputPreview": preview(result.Output, 2000), "truncated": result.Truncated}); err != nil {
				return false, err
			}
		}
		if err := a.persist(); err != nil {
			return false, err
		}
	}
	return false, complete("failed", "step_limit_reached", a.config.MaxStepsPerTurn)
}

// callModel emits one model request and waits for its inbox response.
func (a *Agent) callModel(ctx context.Context) (modelResponse, string, error) {
	a.requestNumber++
	requestID := fmt.Sprintf("m%06d-%d", a.state.Steps, a.requestNumber)
	request := modelRequest{Model: a.config.Model, Messages: a.history, Tools: toolDefinitions(), ToolChoice: "auto", MaxTokens: a.config.MaxOutputTokens}
	requestFile, err := writeRequest(a.directory, requestID, request)
	if err != nil {
		return modelResponse{}, "model_request_too_large", nil
	}
	defer removeRequest(a.directory, requestFile)
	if _, err := a.outbox.Emit(EventModelRequest, map[string]any{"requestId": requestID, "file": requestFile}); err != nil {
		return modelResponse{}, "", err
	}
	deadline := a.now().Add(time.Duration(a.config.ModelTimeoutSeconds) * time.Second)
	for {
		items, err := pendingInbox(a.directory, a.done)
		if err != nil {
			return modelResponse{}, "", err
		}
		for _, item := range items {
			switch item.Type {
			case InboxShutdown:
				return modelResponse{}, "shutdown", nil // left pending for Serve
			case InboxCancel:
				return modelResponse{}, "cancelled", a.markDone(item.name)
			case InboxModelResponse:
				if err := a.markDone(item.name); err != nil {
					return modelResponse{}, "", err
				}
				if item.RequestID != requestID {
					continue
				}
				if item.Error != nil {
					code := item.Error.Code
					if !codePattern(code) {
						code = "model_error"
					}
					return modelResponse{}, code, nil
				}
				var response modelResponse
				if json.Unmarshal(item.Response, &response) != nil {
					return modelResponse{}, "model_response_invalid", nil
				}
				return response, "ok", nil
			}
			// Messages and finalize wait until this turn ends.
		}
		if !a.now().Before(deadline) {
			return modelResponse{}, "model_timeout", nil
		}
		select {
		case <-ctx.Done():
			return modelResponse{}, "shutdown", nil
		case <-time.After(a.poll):
		}
	}
}

func codePattern(value string) bool {
	if len(value) == 0 || len(value) > 63 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') {
			return false
		}
	}
	return true
}

// compact bounds the request size by eliding the oldest tool output first.
func (a *Agent) compact() {
	const budget = 600_000
	total := 0
	for _, message := range a.history {
		total += len(message.Content)
	}
	for index := 1; index < len(a.history)-8 && total > budget; index++ {
		if a.history[index].Role == "tool" && len(a.history[index].Content) > 200 {
			total -= len(a.history[index].Content)
			a.history[index].Content = "[earlier tool output removed to fit the context window]"
			total += len(a.history[index].Content)
		}
	}
}

// finalize writes the patch and summary artifacts and reports their digests.
func (a *Agent) finalize(ctx context.Context) error {
	output := filepath.Join(a.directory, outputDir)
	patch := []byte{}
	warnings := []string{}
	if a.config.RepositoryDirectory != "" {
		var err error
		if patch, err = repositoryPatch(ctx, a.config.RepositoryDirectory); err != nil {
			patch = []byte{}
			warnings = append(warnings, "patch could not be produced")
		} else if len(patch) > maxInboxBytes {
			patch = []byte{}
			warnings = append(warnings, "patch exceeded the artifact size limit and was omitted")
		}
	}
	summary := a.state.LastAssistant
	if summary == "" {
		summary = "The agent produced no reply."
	}
	summary = fmt.Sprintf("# Run summary\n\n%s\n\n- Turns: %d\n- Model steps: %d\n- Patch bytes: %d\n", summary, a.state.Turns, a.state.Steps, len(patch))
	artifacts := []map[string]any{}
	for _, file := range []struct {
		name, base string
		data       []byte
	}{{"patch", "patch.diff", patch}, {"summary", "summary.md", []byte(summary)}} {
		path := filepath.Join(output, file.base)
		if err := writeAtomic(path, file.data); err != nil {
			return err
		}
		digest := sha256.Sum256(file.data)
		artifacts = append(artifacts, map[string]any{"name": file.name, "path": path, "size": len(file.data), "sha256": "sha256:" + hex.EncodeToString(digest[:])})
	}
	a.state.Finalized = true
	if err := a.persist(); err != nil {
		return err
	}
	_, err := a.outbox.Emit(EventFinalized, map[string]any{"artifacts": artifacts, "steps": a.state.Steps, "turns": a.state.Turns, "warnings": warnings})
	return err
}

func preview(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
