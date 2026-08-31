package harnessworker

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path"
	"regexp"
	"strings"
	"time"
)

var (
	namePattern = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,95}$`)
)

type ProtocolError struct{ Code string }

func (e *ProtocolError) Error() string { return e.Code }

func DecodeAssignmentLine(ctx context.Context, input io.Reader, now time.Time) (Assignment, error) {
	if input == nil {
		return Assignment{}, protocolError("request_invalid")
	}
	reader := bufio.NewReader(io.LimitReader(input, MaxProtocolLineBytes+2))
	line, err := reader.ReadBytes('\n')
	if err != nil || len(line) < 2 || len(line) > MaxProtocolLineBytes+1 || line[len(line)-1] != '\n' || line[len(line)-2] == '\r' {
		return Assignment{}, protocolError("request_invalid")
	}
	if err := ctx.Err(); err != nil {
		return Assignment{}, protocolError("request_cancelled")
	}
	extra, err := reader.ReadByte()
	if err == nil || !errors.Is(err, io.EOF) || extra != 0 {
		return Assignment{}, protocolError("protocol_injection")
	}
	assignment, err := DecodeAssignment(bytes.NewReader(line[:len(line)-1]), now)
	if err != nil {
		return Assignment{}, protocolError("request_invalid")
	}
	return assignment, nil
}

func EncodeResponse(output io.Writer, response any) error {
	if output == nil {
		return errors.New("worker output is required")
	}
	encoded, err := json.Marshal(response)
	if err != nil || len(encoded) > MaxProtocolLineBytes {
		return errors.New("worker response is invalid")
	}
	encoded = append(encoded, '\n')
	for len(encoded) > 0 {
		written, writeErr := output.Write(encoded)
		if writeErr != nil {
			return writeErr
		}
		if written <= 0 {
			return io.ErrShortWrite
		}
		encoded = encoded[written:]
	}
	return nil
}

func ValidateExecution(execution Execution, artifacts []ArtifactSpec) error {
	if artifacts == nil || len(artifacts) > MaxArtifacts || !validCommand(execution) {
		return protocolError("request_invalid")
	}
	seenNames, seenPaths := map[string]bool{}, map[string]bool{}
	var total int64
	for _, artifact := range artifacts {
		if !validArtifact(artifact) || seenNames[artifact.Name] || seenPaths[artifact.Path] || total > MaxTotalArtifactBytes-artifact.MaxBytes {
			return protocolError("request_invalid")
		}
		seenNames[artifact.Name], seenPaths[artifact.Path] = true, true
		total += artifact.MaxBytes
	}
	return nil
}

func ScopeDigest(scope WorkloadScope) string {
	encoded, _ := json.Marshal(scope)
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func validCommand(command Execution) bool {
	if len(command.Argv) == 0 || len(command.Argv) > MaxArguments || command.TimeoutSeconds < 1 || command.TimeoutSeconds > MaxRunSeconds ||
		command.CancelGraceSeconds < 1 || command.CancelGraceSeconds > MaxCancellationSeconds || !validWorkspaceDirectory(command.WorkingDirectory) {
		return false
	}
	total := 0
	for _, argument := range command.Argv {
		if argument == "" || len(argument) > MaxArgumentBytes || strings.ContainsRune(argument, 0) {
			return false
		}
		total += len(argument)
		if total > MaxArgumentTotalBytes {
			return false
		}
	}
	return strings.HasPrefix(command.Argv[0], "/") && path.Clean(command.Argv[0]) == command.Argv[0]
}

func validWorkspaceDirectory(value string) bool {
	return value == "/workspace" || strings.HasPrefix(value, "/workspace/") && path.Clean(value) == value && !strings.ContainsAny(value, "\\\x00\r\n\t")
}

func validArtifact(artifact ArtifactSpec) bool {
	if !namePattern.MatchString(artifact.Name) || artifact.MaxBytes < 1 || artifact.MaxBytes > MaxArtifactBytes ||
		artifact.Path == "/workspace/artifacts" || !strings.HasPrefix(artifact.Path, "/workspace/artifacts/") || path.Clean(artifact.Path) != artifact.Path ||
		strings.ContainsAny(artifact.Path, "\\\x00\r\n\t") {
		return false
	}
	switch artifact.Role {
	case "patch":
		return artifact.Name == "patch" && artifact.Kind == "agent.patch" && artifact.MediaType == "text/x-diff"
	case "summary":
		return artifact.Name == "summary" && artifact.Kind == "agent.summary" && artifact.MediaType == "text/markdown"
	case "output":
		return artifact.Kind == "agent.output" && (artifact.MediaType == "text/markdown" || artifact.MediaType == "application/octet-stream")
	default:
		return false
	}
}

func protocolError(code string) error { return &ProtocolError{Code: code} }

func ErrorCode(err error) string {
	var value *ProtocolError
	if errors.As(err, &value) {
		return value.Code
	}
	return "worker_internal"
}
