package plugin

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
)

const (
	RuntimeContextEnvironment = "BLAZN_PLUGIN_CONTEXT"
	RuntimeContextSchema      = 1
	maxRuntimeContextSize     = 16 * 1024
)

type RuntimeContext struct {
	SchemaVersion   int    `json:"schemaVersion"`
	ProtocolVersion int    `json:"protocolVersion"`
	InvocationID    string `json:"invocationId"`
	CoreVersion     string `json:"coreVersion"`
	OutputFormat    string `json:"outputFormat"`
	Status          string `json:"status"`
	ReasonCode      string `json:"reasonCode,omitempty"`
	APIOrigin       string `json:"apiOrigin,omitempty"`
	UserID          string `json:"userId,omitempty"`
	WorkspaceID     string `json:"workspaceId,omitempty"`
	ProjectID       string `json:"projectId,omitempty"`
}

var runtimeIdentifier = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
var invocationIdentifier = regexp.MustCompile(`^[0-9a-f]{32}$`)

func NewRuntimeContext(coreVersion, outputFormat string) (RuntimeContext, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return RuntimeContext{}, fmt.Errorf("create plugin invocation ID: %w", err)
	}
	return RuntimeContext{
		SchemaVersion:   RuntimeContextSchema,
		ProtocolVersion: ProtocolVersion,
		InvocationID:    hex.EncodeToString(random),
		CoreVersion:     coreVersion,
		OutputFormat:    outputFormat,
		Status:          "unavailable",
		ReasonCode:      "workspace_context_unavailable",
	}, nil
}

func (c RuntimeContext) Validate() error {
	if c.SchemaVersion != RuntimeContextSchema || c.ProtocolVersion != ProtocolVersion {
		return errors.New("plugin runtime context version is unsupported")
	}
	if !invocationIdentifier.MatchString(c.InvocationID) || c.CoreVersion == "" || len(c.CoreVersion) > 128 {
		return errors.New("plugin runtime context identity is invalid")
	}
	switch c.OutputFormat {
	case "human", "json", "jsonl", "csv":
	default:
		return errors.New("plugin runtime context output format is invalid")
	}
	switch c.Status {
	case "selected":
		if c.ReasonCode != "" || c.APIOrigin == "" || c.UserID == "" || c.WorkspaceID == "" {
			return errors.New("selected plugin runtime context is incomplete")
		}
	case "unselected", "unavailable":
		if !runtimeIdentifier.MatchString(c.ReasonCode) || c.UserID != "" || c.WorkspaceID != "" || c.ProjectID != "" {
			return errors.New("unselected plugin runtime context is invalid")
		}
	default:
		return errors.New("plugin runtime context status is invalid")
	}
	if c.APIOrigin != "" {
		if len(c.APIOrigin) > 2048 {
			return errors.New("plugin runtime context API origin is too long")
		}
		parsed, err := url.Parse(c.APIOrigin)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
			return errors.New("plugin runtime context API origin is invalid")
		}
		if parsed.Scheme != "https" && !(parsed.Scheme == "http" && (parsed.Hostname() == "localhost" || parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "::1")) {
			return errors.New("plugin runtime context API origin is unsafe")
		}
	}
	for _, identifier := range []string{c.UserID, c.WorkspaceID, c.ProjectID} {
		if len(identifier) > 255 {
			return errors.New("plugin runtime context resource identity is too long")
		}
	}
	encoded, err := json.Marshal(c)
	if err != nil || len(encoded) > maxRuntimeContextSize {
		return errors.New("plugin runtime context exceeds its size limit")
	}
	return nil
}

func EncodeRuntimeContext(context RuntimeContext) (string, error) {
	if err := context.Validate(); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(context)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func DecodeRuntimeContext(value string) (RuntimeContext, error) {
	if len(value) == 0 || len(value) > maxRuntimeContextSize {
		return RuntimeContext{}, errors.New("plugin runtime context has an invalid size")
	}
	decoder := json.NewDecoder(bytes.NewBufferString(value))
	decoder.DisallowUnknownFields()
	var context RuntimeContext
	if err := decoder.Decode(&context); err != nil {
		return RuntimeContext{}, fmt.Errorf("decode plugin runtime context: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return RuntimeContext{}, errors.New("plugin runtime context contains trailing data")
	}
	if err := context.Validate(); err != nil {
		return RuntimeContext{}, err
	}
	return context, nil
}

func runtimeEnvironment(base []string, context RuntimeContext) ([]string, error) {
	encoded, err := EncodeRuntimeContext(context)
	if err != nil {
		return nil, err
	}
	prefix := RuntimeContextEnvironment + "="
	result := make([]string, 0, len(base)+1)
	for _, value := range base {
		name, _, _ := strings.Cut(value, "=")
		if allowedPluginEnvironment(name) {
			result = append(result, value)
		}
	}
	return append(result, prefix+encoded), nil
}

func allowedPluginEnvironment(name string) bool {
	upper := strings.ToUpper(name)
	switch upper {
	case "PATH", "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "TMP", "TEMP", "TZ",
		"LANG", "LC_ALL", "LC_CTYPE", "TERM", "COLORTERM", "NO_COLOR",
		"SYSTEMROOT", "WINDIR", "COMSPEC", "PATHEXT", "SSL_CERT_FILE", "SSL_CERT_DIR":
		return true
	default:
		return strings.HasPrefix(upper, "LC_")
	}
}
