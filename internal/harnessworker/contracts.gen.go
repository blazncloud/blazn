// Code generated from packages/contracts/harness-worker.schema.json and packages/contracts/proxy/workload-scope.schema.json; DO NOT EDIT.
// harness-worker.schema.json SHA256: 06c5fb15126d18e51164fc3585918506bbcafdec3c88e2707ccbbc000d70b3b6
// workload-scope.schema.json SHA256: 93729e99abbde7514803d30209690e37f1aef3c3d2ad220ee948edbc585e8f7a

package harnessworker

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"
)

const (
	HarnessWorkerSchemaVersion = "blazn.dev/harness-worker/v1alpha1"
	WorkloadScopeMaxLifetime   = 24 * time.Hour
	ProtocolVersion            = HarnessWorkerSchemaVersion
	RequestTypeExecute         = "execute"
)

type Protocol string

const (
	ProtocolOpenAIResponses   Protocol = "openai-responses"
	ProtocolOpenAIChat        Protocol = "openai-chat"
	ProtocolAnthropicMessages Protocol = "anthropic-messages"
)

// WorkloadScope is safe to persist and log. The raw listener token is delivered
// out-of-band and must hash to ListenerTokenFingerprint before use.
type WorkloadScope struct {
	RunID                    string   `json:"runId"`
	WorkspaceID              string   `json:"workspaceId"`
	ProjectID                string   `json:"projectId"`
	OperationID              string   `json:"operationId"`
	SandboxID                string   `json:"sandboxId"`
	AgentVersionID           string   `json:"agentVersionId"`
	AgentVersionDigest       string   `json:"agentVersionDigest"`
	HarnessProfileID         string   `json:"harnessProfileId"`
	HarnessProfileDigest     string   `json:"harnessProfileDigest"`
	HarnessVersionID         string   `json:"harnessVersionId"`
	HarnessVersionDigest     string   `json:"harnessVersionDigest"`
	RouteID                  string   `json:"routeId"`
	RouteVersion             int64    `json:"routeVersion"`
	Protocol                 Protocol `json:"protocol"`
	ExpiresAt                string   `json:"expiresAt"`
	ListenerCredentialRef    string   `json:"listenerCredentialRef"`
	ListenerTokenFingerprint string   `json:"listenerTokenFingerprint"`
}

type Assignment struct {
	SchemaVersion string        `json:"schemaVersion"`
	Type          string        `json:"type"`
	Scope         WorkloadScope `json:"scope"`
}

var (
	contractUUIDPattern        = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	contractDigestPattern      = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	contractListenerRefPattern = regexp.MustCompile(`^listener-token://[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	digestPattern              = contractDigestPattern
)

func ValidateWorkloadScopeAt(scope WorkloadScope, now time.Time) error {
	for name, value := range map[string]string{
		"runId": scope.RunID, "workspaceId": scope.WorkspaceID, "projectId": scope.ProjectID,
		"operationId": scope.OperationID, "sandboxId": scope.SandboxID, "agentVersionId": scope.AgentVersionID,
		"harnessProfileId": scope.HarnessProfileID, "harnessVersionId": scope.HarnessVersionID, "routeId": scope.RouteID,
	} {
		if !contractUUIDPattern.MatchString(value) {
			return fmt.Errorf("%s must be a lowercase canonical UUID", name)
		}
	}
	for name, value := range map[string]string{
		"agentVersionDigest": scope.AgentVersionDigest, "harnessProfileDigest": scope.HarnessProfileDigest,
		"harnessVersionDigest": scope.HarnessVersionDigest, "listenerTokenFingerprint": scope.ListenerTokenFingerprint,
	} {
		if !contractDigestPattern.MatchString(value) {
			return fmt.Errorf("%s must be a SHA-256 digest", name)
		}
	}
	if scope.RouteVersion < 1 || scope.RouteVersion > 9007199254740991 {
		return errors.New("routeVersion is outside the supported range")
	}
	switch scope.Protocol {
	case ProtocolOpenAIResponses, ProtocolOpenAIChat, ProtocolAnthropicMessages:
	default:
		return errors.New("protocol is unsupported")
	}
	expires, err := time.Parse(time.RFC3339, scope.ExpiresAt)
	if err != nil {
		return errors.New("expiresAt must be RFC3339")
	}
	if !expires.After(now) {
		return errors.New("workload scope is expired")
	}
	if expires.After(now.Add(WorkloadScopeMaxLifetime)) {
		return errors.New("workload scope lifetime exceeds 24 hours")
	}
	if !contractListenerRefPattern.MatchString(scope.ListenerCredentialRef) {
		return errors.New("listenerCredentialRef is invalid")
	}
	return nil
}

func (scope WorkloadScope) ValidateAt(now time.Time) error {
	return ValidateWorkloadScopeAt(scope, now)
}

func (assignment Assignment) ValidateAt(now time.Time) error {
	if assignment.SchemaVersion != HarnessWorkerSchemaVersion || assignment.Type != RequestTypeExecute {
		return errors.New("invalid harness worker assignment identity")
	}
	return ValidateWorkloadScopeAt(assignment.Scope, now)
}

func DecodeAssignment(reader io.Reader, now time.Time) (Assignment, error) {
	var assignment Assignment
	decoder := json.NewDecoder(io.LimitReader(reader, MaxProtocolLineBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&assignment); err != nil {
		return Assignment{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Assignment{}, errors.New("trailing JSON value")
		}
		return Assignment{}, err
	}
	if err := assignment.ValidateAt(now); err != nil {
		return Assignment{}, err
	}
	return assignment, nil
}

func DecodeWorkloadScope(reader io.Reader, now time.Time) (WorkloadScope, error) {
	var scope WorkloadScope
	decoder := json.NewDecoder(io.LimitReader(reader, MaxProtocolLineBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&scope); err != nil {
		return WorkloadScope{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return WorkloadScope{}, errors.New("trailing JSON value")
		}
		return WorkloadScope{}, err
	}
	if err := scope.ValidateAt(now); err != nil {
		return WorkloadScope{}, err
	}
	return scope, nil
}

func ListenerTokenFingerprint(token []byte) (string, error) {
	if len(token) == 0 {
		return "", errors.New("listener token is required")
	}
	if len(token) > MaxListenerTokenBytes {
		return "", errors.New("listener token exceeds the supported size")
	}
	digest := sha256.Sum256(token)
	return fmt.Sprintf("sha256:%x", digest), nil
}
