package harnessworker

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func validAssignment(t *testing.T) Assignment {
	t.Helper()
	fingerprint, err := ListenerTokenFingerprint([]byte("ephemeral-listener-value"))
	if err != nil {
		t.Fatal(err)
	}
	return Assignment{SchemaVersion: HarnessWorkerSchemaVersion, Type: RequestTypeExecute, Scope: WorkloadScope{
		RunID: "123e4567-e89b-42d3-a456-426614174000", WorkspaceID: "123e4567-e89b-42d3-a456-426614174001", ProjectID: "123e4567-e89b-42d3-a456-426614174002",
		OperationID: "123e4567-e89b-42d3-a456-426614174003", SandboxID: "123e4567-e89b-42d3-a456-426614174004", AgentVersionID: "123e4567-e89b-42d3-a456-426614174005",
		AgentVersionDigest: "sha256:" + strings.Repeat("a", 64), HarnessProfileID: "123e4567-e89b-42d3-a456-426614174006", HarnessProfileDigest: "sha256:" + strings.Repeat("b", 64),
		HarnessVersionID: "123e4567-e89b-42d3-a456-426614174007", HarnessVersionDigest: "sha256:" + strings.Repeat("c", 64), HarnessExecutableDigest: "sha256:" + strings.Repeat("d", 64), RouteID: "123e4567-e89b-42d3-a456-426614174008",
		RouteVersion: 7, Protocol: ProtocolOpenAIChat, ExpiresAt: "2026-09-01T00:00:00Z", ListenerCredentialRef: "listener-token://123e4567-e89b-42d3-a456-426614174009", ListenerTokenFingerprint: fingerprint,
	}}
}

func TestAssignmentRoundTripBindsScopeWithoutRawToken(t *testing.T) {
	assignment := validAssignment(t)
	encoded, err := json.Marshal(assignment)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "ephemeral-listener-value") || strings.Contains(string(encoded), `"listenerToken"`) {
		t.Fatal("serialized assignment exposed listener token material")
	}
	decoded, err := DecodeAssignment(strings.NewReader(string(encoded)), time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Scope.WorkspaceID != assignment.Scope.WorkspaceID || decoded.Scope.ProjectID != assignment.Scope.ProjectID || decoded.Scope.RunID != assignment.Scope.RunID || decoded.Scope.HarnessExecutableDigest != assignment.Scope.HarnessExecutableDigest || decoded.Scope.RouteID != assignment.Scope.RouteID || decoded.Scope.RouteVersion != assignment.Scope.RouteVersion || decoded.Scope.Protocol != assignment.Scope.Protocol {
		t.Fatal("assignment scope binding changed during round trip")
	}
}

func TestDecodeRejectsRawTokenAndTrailingJSON(t *testing.T) {
	encoded, err := json.Marshal(validAssignment(t))
	if err != nil {
		t.Fatal(err)
	}
	withToken := strings.Replace(string(encoded), `"listenerTokenFingerprint":`, `"listenerToken":"forbidden","listenerTokenFingerprint":`, 1)
	now := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	if _, err := DecodeAssignment(strings.NewReader(withToken), now); err == nil {
		t.Fatal("raw listener token field was accepted")
	}
	if _, err := DecodeAssignment(strings.NewReader(string(encoded)+` {}`), now); err == nil {
		t.Fatal("trailing JSON was accepted")
	}
}

func TestScopeValidationRejectsIdentityRouteAndLifetimeDrift(t *testing.T) {
	now := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	assignment := validAssignment(t)
	assignment.Scope.ExpiresAt = "2026-08-30T00:00:00Z"
	if err := assignment.ValidateAt(now); err == nil {
		t.Fatal("expired scope passed validation")
	}
	assignment = validAssignment(t)
	assignment.Scope.ExpiresAt = "2026-09-01T00:00:01Z"
	if err := assignment.ValidateAt(now); err == nil {
		t.Fatal("scope lifetime over 24 hours passed validation")
	}
	assignment = validAssignment(t)
	assignment.Scope.WorkspaceID = strings.ToUpper(assignment.Scope.WorkspaceID)
	if err := assignment.ValidateAt(now); err == nil {
		t.Fatal("uppercase UUID passed validation")
	}
	assignment = validAssignment(t)
	assignment.Scope.Protocol = "unknown"
	if err := assignment.ValidateAt(now); err == nil {
		t.Fatal("unsupported route protocol passed validation")
	}
	assignment = validAssignment(t)
	assignment.Scope.HarnessExecutableDigest = "sha256:not-a-digest"
	if err := assignment.ValidateAt(now); err == nil {
		t.Fatal("malformed harness executable digest passed validation")
	}
}

func TestListenerTokenFingerprintUsesExactBoundedBytes(t *testing.T) {
	first, err := ListenerTokenFingerprint([]byte("token"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := ListenerTokenFingerprint([]byte("token\n"))
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("listener token hashing normalized exact bytes")
	}
	if _, err := ListenerTokenFingerprint(nil); err == nil {
		t.Fatal("empty listener token passed validation")
	}
	if _, err := ListenerTokenFingerprint(make([]byte, contractMaxListenerTokenBytes+1)); err == nil {
		t.Fatal("oversized listener token passed validation")
	}
}
