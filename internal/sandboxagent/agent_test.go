package sandboxagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testDirectories points the workspace root at a temporary directory.
func testDirectories(t *testing.T) (state, work string) {
	t.Helper()
	base := t.TempDir()
	previous := workspaceRoot
	workspaceRoot = base
	t.Cleanup(func() { workspaceRoot = previous })
	state, work = filepath.Join(base, "state"), filepath.Join(base, "repo")
	for _, directory := range []string{state, work, filepath.Join(state, inboxDir)} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return state, work
}

func writeConfig(t *testing.T, state, work string, mutate func(*Config)) {
	t.Helper()
	config := Config{SchemaVersion: SchemaVersion, RunID: "run-1", Instructions: "You are a careful coding agent.", WorkingDirectory: work,
		RepositoryDirectory: work, Model: "test-model", MaxStepsPerTurn: 6, CommandTimeoutSeconds: 10, ModelTimeoutSeconds: 10}
	if mutate != nil {
		mutate(&config)
	}
	data, _ := json.Marshal(config)
	if err := os.WriteFile(filepath.Join(state, configFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func put(t *testing.T, state string, number int, id string, item map[string]any) {
	t.Helper()
	data, _ := json.Marshal(item)
	if err := writeAtomic(filepath.Join(state, inboxDir, fmt.Sprintf("%06d-%s.json", number, id)), data); err != nil {
		t.Fatal(err)
	}
}

type waited struct {
	Sequence int64           `json:"seq"`
	Type     string          `json:"type"`
	Data     json.RawMessage `json:"data"`
	LastSeq  int64           `json:"lastSeq"`
}

func next(t *testing.T, state string, after *int64) []waited {
	t.Helper()
	var output bytes.Buffer
	if err := Wait(state, *after, 5*time.Second, &output); err != nil {
		t.Fatal(err)
	}
	var events []waited
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		var event waited
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("wait line is not JSON: %q", line)
		}
		if event.Type == "wait.status" {
			*after = event.LastSeq
			continue
		}
		events = append(events, event)
	}
	return events
}

// until collects events until one of the wanted type arrives.
func until(t *testing.T, state string, after *int64, wanted string) (waited, []waited) {
	t.Helper()
	var seen []waited
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		for _, event := range next(t, state, after) {
			seen = append(seen, event)
			if event.Type == wanted {
				return event, seen
			}
		}
	}
	t.Fatalf("no %s event arrived; saw %d events", wanted, len(seen))
	return waited{}, nil
}

func TestAgentRunsAToolTurnThroughTheInboxAndFinalizesArtifacts(t *testing.T) {
	state, work := testDirectories(t)
	writeConfig(t, state, work, nil)
	for _, arguments := range [][]string{{"init", "-q"}, {"-c", "user.email=a@b.invalid", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "base"}} {
		command := exec.Command("git", append([]string{"-C", work}, arguments...)...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Skipf("git is unavailable: %v %s", err, output)
		}
	}
	agent, err := New(state, Options{Poll: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- agent.Serve(ctx) }()

	var after int64
	put(t, state, 1, "msg-1", map[string]any{"type": InboxMessage, "id": "msg-1", "kind": "prompt", "content": "Create hello.txt"})
	requestEvent, _ := until(t, state, &after, EventModelRequest)
	var first struct {
		RequestID string `json:"requestId"`
		Request   struct {
			Model    string        `json:"model"`
			Messages []chatMessage `json:"messages"`
			Tools    []any         `json:"tools"`
		} `json:"request"`
	}
	if err := json.Unmarshal(requestEvent.Data, &first); err != nil || first.Request.Model != "test-model" || len(first.Request.Tools) != 3 ||
		len(first.Request.Messages) != 2 || first.Request.Messages[0].Role != "system" || first.Request.Messages[1].Content != "Create hello.txt" {
		t.Fatalf("first model request = %s err=%v", requestEvent.Data, err)
	}
	put(t, state, 2, "resp-1", map[string]any{"type": InboxModelResponse, "requestId": first.RequestID, "response": map[string]any{"choices": []any{map[string]any{"message": map[string]any{
		"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "call-1", "type": "function", "function": map[string]any{
			"name": "run_command", "arguments": `{"command":"printf hello > hello.txt && cat hello.txt"}`}}}}}}}})
	second, seen := until(t, state, &after, EventModelRequest)
	sawTool := false
	for _, event := range seen {
		if event.Type == EventToolFinished && strings.Contains(string(event.Data), `"exitCode":0`) && strings.Contains(string(event.Data), "hello") {
			sawTool = true
		}
	}
	if !sawTool {
		t.Fatalf("tool result was not reported: %+v", seen)
	}
	var followUp struct {
		RequestID string `json:"requestId"`
		Request   struct {
			Messages []chatMessage `json:"messages"`
		} `json:"request"`
	}
	if err := json.Unmarshal(second.Data, &followUp); err != nil || len(followUp.Request.Messages) != 4 || followUp.Request.Messages[3].Role != "tool" ||
		followUp.Request.Messages[3].ToolCallID != "call-1" || !strings.Contains(followUp.Request.Messages[3].Content, "[exit code 0]") {
		t.Fatalf("second model request = %s err=%v", second.Data, err)
	}
	put(t, state, 3, "resp-2", map[string]any{"type": InboxModelResponse, "requestId": followUp.RequestID,
		"response": map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "Created hello.txt."}}}}})
	completed, seen := until(t, state, &after, EventTurnCompleted)
	if !strings.Contains(string(completed.Data), `"status":"completed"`) || !strings.Contains(string(completed.Data), `"messageId":"msg-1"`) {
		t.Fatalf("turn completion = %s", completed.Data)
	}
	replied := false
	for _, event := range seen {
		if event.Type == EventAssistantMessage && strings.Contains(string(event.Data), "Created hello.txt.") {
			replied = true
		}
	}
	if !replied {
		t.Fatalf("assistant reply was not reported: %+v", seen)
	}

	put(t, state, 4, "finalize", map[string]any{"type": InboxFinalize})
	finalized, _ := until(t, state, &after, EventFinalized)
	var result struct {
		Artifacts []struct {
			Name string `json:"name"`
			Path string `json:"path"`
			Size int    `json:"size"`
		} `json:"artifacts"`
	}
	if err := json.Unmarshal(finalized.Data, &result); err != nil || len(result.Artifacts) != 2 || result.Artifacts[0].Name != "patch" || result.Artifacts[1].Name != "summary" {
		t.Fatalf("finalized = %s", finalized.Data)
	}
	patch, _ := os.ReadFile(result.Artifacts[0].Path)
	summary, _ := os.ReadFile(result.Artifacts[1].Path)
	if !strings.Contains(string(patch), "hello.txt") || !strings.Contains(string(patch), "+hello") || !strings.Contains(string(summary), "Created hello.txt.") {
		t.Fatalf("patch=%q summary=%q", patch, summary)
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
}

func TestAgentReportsModelErrorsCancellationAndALostTurn(t *testing.T) {
	state, work := testDirectories(t)
	writeConfig(t, state, work, func(c *Config) { c.RepositoryDirectory = "" })
	agent, err := New(state, Options{Poll: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- agent.Serve(ctx) }()
	var after int64
	requestID := func(event waited) string {
		var data struct {
			RequestID string `json:"requestId"`
		}
		_ = json.Unmarshal(event.Data, &data)
		return data.RequestID
	}

	put(t, state, 1, "m1", map[string]any{"type": InboxMessage, "id": "m1", "content": "first"})
	request, _ := until(t, state, &after, EventModelRequest)
	put(t, state, 2, "r1", map[string]any{"type": InboxModelResponse, "requestId": requestID(request), "error": map[string]any{"code": "model_unavailable", "message": "down"}})
	completed, _ := until(t, state, &after, EventTurnCompleted)
	if !strings.Contains(string(completed.Data), `"errorCode":"model_unavailable"`) || !strings.Contains(string(completed.Data), `"status":"failed"`) {
		t.Fatalf("model error completion = %s", completed.Data)
	}

	put(t, state, 3, "m2", map[string]any{"type": InboxMessage, "id": "m2", "content": "second"})
	until(t, state, &after, EventModelRequest)
	put(t, state, 4, "cancel", map[string]any{"type": InboxCancel})
	completed, _ = until(t, state, &after, EventTurnCompleted)
	if !strings.Contains(string(completed.Data), `"status":"cancelled"`) {
		t.Fatalf("cancel completion = %s", completed.Data)
	}

	put(t, state, 5, "m3", map[string]any{"type": InboxMessage, "id": "m3", "content": "third"})
	until(t, state, &after, EventModelRequest)
	cancel() // the process dies mid-turn
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	// A turn interrupted by a crash leaves CurrentMessage set; simulate that.
	raw, _ := os.ReadFile(filepath.Join(state, stateFile))
	var persisted persistedState
	_ = json.Unmarshal(raw, &persisted)
	persisted.CurrentMessage = "m3"
	raw, _ = json.Marshal(persisted)
	_ = os.WriteFile(filepath.Join(state, stateFile), raw, 0o600)
	restarted, err := New(state, Options{Poll: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go func() { _ = restarted.Serve(ctx2) }()
	for {
		completed, _ = until(t, state, &after, EventTurnCompleted)
		if strings.Contains(string(completed.Data), `"messageId":"m3"`) && strings.Contains(string(completed.Data), `"errorCode":"agent_restarted"`) {
			break
		}
	}
}

func TestConfigRejectsDirectoriesOutsideTheWorkspaceAndUnknownFields(t *testing.T) {
	directory := t.TempDir()
	write := func(value string) {
		if err := os.WriteFile(filepath.Join(directory, configFile), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	valid := `{"schemaVersion":"` + SchemaVersion + `","runId":"r","instructions":"i","workingDirectory":"/workspace/src/x","model":"m"}`
	write(valid)
	if config, err := LoadConfig(directory); err != nil || config.MaxStepsPerTurn != 40 || config.CommandTimeoutSeconds != 120 {
		t.Fatalf("valid config rejected: %+v %v", config, err)
	}
	for _, invalid := range []string{
		strings.Replace(valid, "/workspace/src/x", "/etc", 1),
		strings.Replace(valid, "/workspace/src/x", "/workspace/../etc", 1),
		strings.Replace(valid, `"model":"m"`, `"model":"m","apiKey":"x"`, 1),
		strings.Replace(valid, SchemaVersion, "other", 1),
	} {
		write(invalid)
		if _, err := LoadConfig(directory); err == nil {
			t.Fatalf("invalid config accepted: %s", invalid)
		}
	}
}

func TestClipKeepsHeadAndTail(t *testing.T) {
	value := strings.Repeat("a", 30000) + "TAIL"
	clipped, truncated := clip(value, 3000)
	if !truncated || len(clipped) > 3100 || !strings.HasSuffix(clipped, "TAIL") || !strings.Contains(clipped, "bytes omitted") {
		t.Fatalf("clip len=%d truncated=%v", len(clipped), truncated)
	}
}
