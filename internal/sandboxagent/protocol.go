// Package sandboxagent is the Blazn reference agent harness. It runs inside a
// Sandbox with no network access and no credentials. Every model request is
// written to an append-only outbox and answered through an inbox directory, so
// the Agent Run controller is the only path to a model.
package sandboxagent

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// SchemaVersion identifies the file protocol between the controller and the agent.
	SchemaVersion = "blazn.dev/sandbox-agent/v1alpha1"
	// Version is reported by `blazn-agent version` and in the started event.
	Version = "0.1.0"

	DefaultDirectory = "/workspace/artifacts/blazn-agent"

	configFile  = "config.json"
	outboxFile  = "outbox.jsonl"
	historyFile = "history.json"
	stateFile   = "state.json"
	pidFile     = "serve.pid"
	logFile     = "serve.log"
	inboxDir    = "inbox"
	outputDir   = "out"
	baselineDir = "baseline.git"

	maxInboxBytes  = 8 << 20
	maxOutboxLine  = 7 << 20
	maxConfigBytes = 1 << 20
)

// Inbox item types written by the controller.
const (
	InboxMessage       = "message"
	InboxModelResponse = "model.response"
	InboxCancel        = "cancel"
	InboxFinalize      = "finalize"
	InboxShutdown      = "shutdown"
)

// Outbox event types written by the agent.
const (
	EventStarted          = "started"
	EventMessageAccepted  = "message.accepted"
	EventModelRequest     = "model.request"
	EventToolStarted      = "tool.started"
	EventToolFinished     = "tool.finished"
	EventAssistantMessage = "assistant.message"
	EventTurnCompleted    = "turn.completed"
	EventFinalized        = "finalized"
	EventStopped          = "stopped"
	EventError            = "error"
)

// Inbox files are processed in name order; the controller uses a fixed-width
// numeric prefix so that order is the order it sent them.
var inboxNamePattern = regexp.MustCompile(`^[0-9]{6,20}-[A-Za-z0-9][A-Za-z0-9._-]{0,80}\.json$`)

// Config is uploaded by the controller before the agent starts. It never
// contains a credential.
type Config struct {
	SchemaVersion         string `json:"schemaVersion"`
	RunID                 string `json:"runId"`
	Instructions          string `json:"instructions"`
	Purpose               string `json:"purpose,omitempty"`
	WorkingDirectory      string `json:"workingDirectory"`
	RepositoryDirectory   string `json:"repositoryDirectory,omitempty"`
	Model                 string `json:"model"`
	MaxStepsPerTurn       int    `json:"maxStepsPerTurn"`
	CommandTimeoutSeconds int    `json:"commandTimeoutSeconds"`
	ModelTimeoutSeconds   int    `json:"modelTimeoutSeconds"`
	MaxOutputTokens       int    `json:"maxOutputTokens,omitempty"`
}

func (c *Config) normalize() error {
	if c.SchemaVersion != SchemaVersion {
		return errors.New("agent configuration schema is unsupported")
	}
	if c.RunID == "" || len(c.RunID) > 64 || c.Model == "" || len(c.Model) > 128 {
		return errors.New("agent configuration identity is invalid")
	}
	if len(c.Instructions) == 0 || len(c.Instructions) > 64<<10 {
		return errors.New("agent instructions are invalid")
	}
	if !withinWorkspace(c.WorkingDirectory) || c.RepositoryDirectory != "" && !withinWorkspace(c.RepositoryDirectory) {
		return errors.New("agent directories must be inside the workspace")
	}
	if c.MaxStepsPerTurn == 0 {
		c.MaxStepsPerTurn = 40
	}
	if c.CommandTimeoutSeconds == 0 {
		c.CommandTimeoutSeconds = 120
	}
	if c.ModelTimeoutSeconds == 0 {
		c.ModelTimeoutSeconds = 300
	}
	if c.MaxStepsPerTurn < 1 || c.MaxStepsPerTurn > 200 || c.CommandTimeoutSeconds < 1 || c.CommandTimeoutSeconds > 1800 ||
		c.ModelTimeoutSeconds < 5 || c.ModelTimeoutSeconds > 1800 || c.MaxOutputTokens < 0 || c.MaxOutputTokens > 1<<20 {
		return errors.New("agent limits are invalid")
	}
	return nil
}

// workspaceRoot is the only tree the agent may be configured to work in.
// Tests replace it with a temporary directory.
var workspaceRoot = "/workspace"

func withinWorkspace(value string) bool {
	clean := filepath.Clean(value)
	return clean == value && (value == workspaceRoot || strings.HasPrefix(value, workspaceRoot+"/")) && !strings.ContainsRune(value, 0)
}

// LoadConfig reads and validates DIRECTORY/config.json.
func LoadConfig(directory string) (Config, error) {
	var config Config
	data, err := readBounded(filepath.Join(directory, configFile), maxConfigBytes)
	if err != nil {
		return config, fmt.Errorf("read agent configuration: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return config, errors.New("agent configuration is not valid JSON")
	}
	return config, config.normalize()
}

// InboxItem is one controller-authored instruction.
type InboxItem struct {
	Type      string          `json:"type"`
	ID        string          `json:"id,omitempty"`
	Kind      string          `json:"kind,omitempty"`
	Content   string          `json:"content,omitempty"`
	RequestID string          `json:"requestId,omitempty"`
	Response  json.RawMessage `json:"response,omitempty"`
	Error     *ItemError      `json:"error,omitempty"`
	name      string
}

type ItemError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Event is one outbox line. Sequence numbers start at 1 and never repeat.
type Event struct {
	Sequence int64           `json:"seq"`
	Time     string          `json:"time"`
	Type     string          `json:"type"`
	Data     json.RawMessage `json:"data"`
}

// Outbox appends events durably. It is safe for concurrent use in one process.
type Outbox struct {
	mu   sync.Mutex
	file *os.File
	next int64
	now  func() time.Time
}

func OpenOutbox(directory string, now func() time.Time) (*Outbox, error) {
	path := filepath.Join(directory, outboxFile)
	last, err := lastSequence(path)
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	return &Outbox{file: file, next: last + 1, now: now}, nil
}

func (o *Outbox) Close() error { return o.file.Close() }

// Emit appends one event and returns its sequence number.
func (o *Outbox) Emit(eventType string, data any) (int64, error) {
	encoded, err := json.Marshal(data)
	if err != nil {
		return 0, err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	line, err := json.Marshal(Event{Sequence: o.next, Time: o.now().UTC().Format(time.RFC3339Nano), Type: eventType, Data: encoded})
	if err != nil {
		return 0, err
	}
	if len(line) > maxOutboxLine {
		return 0, errors.New("agent event is too large")
	}
	if _, err := o.file.Write(append(line, '\n')); err != nil {
		return 0, err
	}
	if err := o.file.Sync(); err != nil {
		return 0, err
	}
	sequence := o.next
	o.next++
	return sequence, nil
}

func lastSequence(path string) (int64, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer file.Close()
	var last int64
	reader := bufio.NewReaderSize(file, 1<<20)
	for {
		line, err := readLine(reader)
		if len(line) > 0 {
			var header struct {
				Sequence int64 `json:"seq"`
			}
			if json.Unmarshal(line, &header) == nil && header.Sequence > last {
				last = header.Sequence
			}
		}
		if err != nil {
			break
		}
	}
	return last, nil
}

func readLine(reader *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		line = append(line, chunk...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			return nil, err // a partial trailing line is still being written
		}
		return line[:len(line)-1], nil
	}
}

// pendingInbox returns unprocessed inbox files in name order.
func pendingInbox(directory string, done map[string]bool) ([]InboxItem, error) {
	entries, err := os.ReadDir(filepath.Join(directory, inboxDir))
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Type().IsRegular() && inboxNamePattern.MatchString(entry.Name()) && !done[entry.Name()] {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	items := make([]InboxItem, 0, len(names))
	for _, name := range names {
		data, err := readBounded(filepath.Join(directory, inboxDir, name), maxInboxBytes)
		if err != nil {
			return nil, err
		}
		var item InboxItem
		if err := json.Unmarshal(data, &item); err != nil || item.Type == "" {
			item = InboxItem{Type: "invalid"}
		}
		item.name = name
		items = append(items, item)
	}
	return items, nil
}

func readBounded(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("file is not a bounded regular file")
	}
	return os.ReadFile(path)
}

func writeAtomic(path string, data []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	if _, err = temporary.Write(data); err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		_ = os.Remove(name)
	}
	return err
}
