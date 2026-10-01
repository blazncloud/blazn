package sandboxagent

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	requestDir    = "requests"
	maxWaitOutput = 6 << 20
)

// Start launches `EXECUTABLE serve --dir DIRECTORY` in its own session and
// returns its PID. A live agent is left alone, so Start is idempotent.
func Start(directory, executable string) (int, error) {
	if _, err := LoadConfig(directory); err != nil {
		return 0, err
	}
	if pid, alive := servePID(directory); alive {
		return pid, nil
	}
	if err := Init(directory); err != nil {
		return 0, err
	}
	log, err := os.OpenFile(filepath.Join(directory, logFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, err
	}
	defer log.Close()
	command := exec.Command(executable, "serve", "--dir", directory)
	command.Dir = directory
	command.Stdout, command.Stderr = log, log
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return 0, err
	}
	pid := command.Process.Pid
	if err := writeAtomic(filepath.Join(directory, pidFile), []byte(strconv.Itoa(pid)+"\n")); err != nil {
		return 0, err
	}
	return pid, command.Process.Release()
}

// Init creates the state directory layout so the controller can upload the
// configuration and inbox files.
func Init(directory string) error {
	if !withinWorkspace(directory) {
		return errors.New("agent state directory must be inside the workspace")
	}
	for _, name := range []string{"", inboxDir, outputDir, requestDir} {
		if err := os.MkdirAll(filepath.Join(directory, name), 0o700); err != nil {
			return err
		}
	}
	return nil
}

func servePID(directory string) (int, bool) {
	data, err := readBounded(filepath.Join(directory, pidFile), 32)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid < 2 {
		return 0, false
	}
	return pid, syscall.Kill(pid, 0) == nil
}

// Wait writes every outbox event after the given sequence as JSON Lines, then
// one `wait.status` line. With nothing new it blocks for up to timeout.
// Model request bodies are inlined from their request files.
func Wait(directory string, after int64, timeout time.Duration, output io.Writer) error {
	deadline := time.Now().Add(timeout)
	path := filepath.Join(directory, outboxFile)
	var offset int64
	last := after
	written, more := 0, false
	writer := bufio.NewWriterSize(output, 1<<16)
	for {
		file, err := os.Open(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil {
			if _, err := file.Seek(offset, io.SeekStart); err != nil {
				file.Close()
				return err
			}
			reader := bufio.NewReaderSize(file, 1<<20)
			for {
				line, err := readLine(reader)
				if err != nil {
					break
				}
				var event Event
				if json.Unmarshal(line, &event) != nil || event.Sequence <= last {
					offset += int64(len(line)) + 1
					continue
				}
				encoded := inlineRequest(directory, event, line)
				if written > 0 && written+len(encoded) > maxWaitOutput {
					more = true
					break
				}
				offset += int64(len(line)) + 1
				if _, err := writer.Write(append(encoded, '\n')); err != nil {
					file.Close()
					return err
				}
				written += len(encoded) + 1
				last = event.Sequence
			}
			file.Close()
		}
		if written > 0 || !time.Now().Before(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	_, alive := servePID(directory)
	status, _ := json.Marshal(map[string]any{"type": "wait.status", "alive": alive, "lastSeq": last, "more": more})
	if _, err := writer.Write(append(status, '\n')); err != nil {
		return err
	}
	return writer.Flush()
}

func inlineRequest(directory string, event Event, line []byte) []byte {
	if event.Type != EventModelRequest {
		return line
	}
	var data struct {
		RequestID string `json:"requestId"`
		File      string `json:"file"`
	}
	if json.Unmarshal(event.Data, &data) != nil || data.File == "" || data.File != filepath.Base(data.File) {
		return line
	}
	body, err := readBounded(filepath.Join(directory, requestDir, data.File), maxOutboxLine)
	if err != nil || !json.Valid(body) {
		return line // already answered and removed
	}
	inlined, err := json.Marshal(map[string]any{"requestId": data.RequestID, "request": json.RawMessage(body)})
	if err != nil {
		return line
	}
	event.Data = inlined
	encoded, err := json.Marshal(event)
	if err != nil {
		return line
	}
	return encoded
}

// writeRequest stores a model request body for Wait to inline.
func writeRequest(directory, requestID string, request any) (string, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	if len(body) > maxOutboxLine-4096 {
		return "", errors.New("model request is too large")
	}
	if err := os.MkdirAll(filepath.Join(directory, requestDir), 0o700); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s.json", requestID)
	return name, writeAtomic(filepath.Join(directory, requestDir, name), body)
}

func removeRequest(directory, name string) { _ = os.Remove(filepath.Join(directory, requestDir, name)) }
