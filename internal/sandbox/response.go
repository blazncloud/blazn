package sandbox

import (
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/blazncloud/blazn/internal/client"
)

var (
	uuidPattern      = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	eventTypePattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,95}$`)
	grantEndpoint    = regexp.MustCompile(`^https://blazn\.benpelo\.com/v1/sandbox-access-grants/`)
)

func validUUID(value string) bool { return uuidPattern.MatchString(value) }

func validateGrant(created client.SandboxAccessGrantCreated, sandboxID string, kind client.SandboxGrantKind, now time.Time) error {
	grant := created.Grant
	wantScope := map[client.SandboxGrantKind]string{
		client.SandboxGrantExec: "sandbox.exec", client.SandboxGrantUpload: "sandbox.upload", client.SandboxGrantDownload: "sandbox.download",
	}[kind]
	if !validUUID(grant.ID) || grant.SandboxID != sandboxID || !validUUID(grant.SandboxID) || !validUUID(grant.WorkspaceID) {
		return errors.New("sandbox access grant identity is invalid")
	}
	if grant.Kind != kind || grant.Scope != wantScope || grant.State != client.SandboxGrantActive {
		return errors.New("sandbox access grant binding is invalid")
	}
	createdAt, err := time.Parse(time.RFC3339Nano, grant.CreatedAt)
	if err != nil {
		return errors.New("sandbox access grant createdAt is invalid")
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, grant.ExpiresAt)
	if err != nil || !expiresAt.After(createdAt) || expiresAt.Sub(createdAt) > 60*time.Second || !expiresAt.After(now) {
		return errors.New("sandbox access grant expiry is invalid")
	}
	if len(created.AccessToken) < 43 || len(created.AccessToken) > 256 || strings.ContainsAny(created.AccessToken, "\r\n\t ") {
		return errors.New("sandbox access grant token is invalid")
	}
	if !grantEndpoint.MatchString(created.Endpoint) {
		return errors.New("sandbox access grant endpoint is invalid")
	}
	return nil
}

func validateExecResult(result client.SandboxExecResult) error {
	if result.RemoteExitCode < 0 || result.RemoteExitCode > 255 {
		return errors.New("sandbox exec result exit code is invalid")
	}
	for name, value := range map[string]string{"stdoutBase64": result.StdoutBase64, "stderrBase64": result.StderrBase64} {
		if len(value) > 11184812 {
			return fmt.Errorf("sandbox exec result %s is too large", name)
		}
		if _, err := base64.StdEncoding.Strict().DecodeString(value); err != nil {
			return fmt.Errorf("sandbox exec result %s is invalid", name)
		}
	}
	return nil
}

func validateEvent(event client.SandboxEvent, sandboxID string) error {
	if !validUUID(event.EventID) || event.SandboxID != sandboxID || !validUUID(event.SandboxID) {
		return errors.New("sandbox event identity is invalid")
	}
	if event.OperationID != nil && !validUUID(*event.OperationID) {
		return errors.New("sandbox event operationId is invalid")
	}
	if event.Sequence < 0 || !eventTypePattern.MatchString(event.Type) {
		return errors.New("sandbox event sequence or type is invalid")
	}
	if event.Payload == nil || len(event.Payload) > 32 {
		return errors.New("sandbox event payload is invalid")
	}
	if _, err := time.Parse(time.RFC3339Nano, event.CreatedAt); err != nil {
		return errors.New("sandbox event createdAt is invalid")
	}
	return nil
}
