package microk8sissuer

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
)

const (
	SchemaVersion   = "blazn.dev/microk8s-worker-issuer/v1"
	BootstrapTaint  = "blazn.dev/bootstrap=pending:NoSchedule"
	MaxMessageBytes = 16 << 10
)

var (
	uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}-[89aAbB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)
	namePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$`)
)

type Request struct {
	SchemaVersion    string `json:"schemaVersion"`
	Operation        string `json:"operation"`
	IssuanceID       string `json:"issuanceId,omitempty"`
	ClusterID        string `json:"clusterId,omitempty"`
	ExpectedNodeName string `json:"expectedNodeName,omitempty"`
	BootstrapTaint   string `json:"bootstrapTaint,omitempty"`
	TTLSeconds       int    `json:"ttlSeconds,omitempty"`
	WorkerOnly       bool   `json:"workerOnly,omitempty"`
	ProviderHandle   string `json:"providerHandle,omitempty"`
}

type IssueResponse struct {
	SchemaVersion  string    `json:"schemaVersion"`
	Operation      string    `json:"operation"`
	ProviderHandle string    `json:"providerHandle"`
	Credential     string    `json:"credential"`
	ClusterID      string    `json:"clusterId"`
	ClusterHealthy bool      `json:"clusterHealthy"`
	WorkerOnly     bool      `json:"workerOnly"`
	ExpiresAt      time.Time `json:"expiresAt"`
}

type RevokeResponse struct {
	SchemaVersion  string `json:"schemaVersion"`
	Operation      string `json:"operation"`
	ProviderHandle string `json:"providerHandle"`
	Revoked        bool   `json:"revoked"`
}

type ProtocolError struct{ Code, Message string }

func (e *ProtocolError) Error() string { return e.Message }

func DecodeRequest(data []byte) (Request, error) {
	if len(data) == 0 || len(data) > MaxMessageBytes {
		return Request{}, invalid("request size is invalid")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return Request{}, invalid("request is not valid JSON")
	}
	var req Request
	if err := json.Unmarshal(data, &req); err != nil {
		return Request{}, invalid("request is invalid")
	}
	if req.SchemaVersion != SchemaVersion {
		return Request{}, invalid("schemaVersion is invalid")
	}
	allowed := map[string]bool{"schemaVersion": true, "operation": true}
	switch req.Operation {
	case "issue":
		for _, key := range []string{"issuanceId", "clusterId", "expectedNodeName", "bootstrapTaint", "ttlSeconds", "workerOnly"} {
			allowed[key] = true
		}
		if !uuidPattern.MatchString(req.IssuanceID) || len(req.ClusterID) < 1 || len(req.ClusterID) > 128 ||
			!namePattern.MatchString(req.ExpectedNodeName) || req.BootstrapTaint != BootstrapTaint ||
			req.TTLSeconds < 1 || req.TTLSeconds > 300 || !req.WorkerOnly {
			return Request{}, invalid("issue binding is invalid")
		}
	case "revoke":
		allowed["providerHandle"] = true
		if !uuidPattern.MatchString(req.ProviderHandle) {
			return Request{}, invalid("providerHandle is invalid")
		}
	default:
		return Request{}, invalid("operation is invalid")
	}
	for key := range raw {
		if !allowed[key] {
			return Request{}, invalid(fmt.Sprintf("unexpected field %q", key))
		}
	}
	for key := range allowed {
		if _, ok := raw[key]; !ok {
			return Request{}, invalid("required field is missing")
		}
	}
	return req, nil
}

func invalid(message string) error { return &ProtocolError{Code: "invalid_request", Message: message} }
func asProtocol(err error) *ProtocolError {
	var result *ProtocolError
	if errors.As(err, &result) {
		return result
	}
	return &ProtocolError{Code: "internal_error", Message: "issuer operation failed"}
}
