package proxycontract

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const id1 = "123e4567-e89b-42d3-a456-426614174000"
const id2 = "123e4567-e89b-42d3-a456-426614174001"
const digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func validPolicy() Policy {
	return Policy{ID: id1, Version: 1, WorkspaceID: id2, Protocols: []Protocol{ProtocolOpenAIChat}, Aliases: map[string]Alias{"local": {RouteIDs: []string{id1}, DataClass: DataCompany, AllowedDestinationBoundaries: []DataBoundary{BoundaryLocal}}}, Routes: []Route{{ID: id1, DestinationClass: DestinationLocalNode, Endpoint: Endpoint{Scheme: "http", Hostname: "localhost", Port: 8080, BasePath: "/v1", HostnameAllowlist: []string{"localhost"}, ResolvedAddressPolicy: AddressLoopbackOnly}, SourceProtocol: ProtocolOpenAIChat, DestinationProtocol: ProtocolOpenAIChat, Model: "qwen", Capabilities: []Capability{CapabilityText}, AcceptedDataClasses: []DataClass{DataCompany}, DataBoundary: BoundaryLocal, HealthTimeoutMS: 1000, CredentialRef: "local/qwen", CostClass: CostLocal}}, RequestLimits: RequestLimits{MaxContextTokens: 1000, MaxOutputTokens: 100, TimeoutMS: 1000, MaxCostClass: CostLocal}, Fallback: Fallback{MaxAttempts: 1}, ContentCapture: false}
}

func TestPolicyValidationEnforcesRoutesBoundaryAndCapture(t *testing.T) {
	policy := validPolicy()
	if err := policy.Validate(); err != nil {
		t.Fatal(err)
	}
	policy.ContentCapture = true
	if err := policy.Validate(); err == nil {
		t.Fatal("content capture accepted")
	}
	policy = validPolicy()
	policy.Aliases["local"] = Alias{RouteIDs: []string{id2}, DataClass: DataCompany, AllowedDestinationBoundaries: []DataBoundary{BoundaryLocal}}
	if err := policy.Validate(); err == nil {
		t.Fatal("unknown route accepted")
	}
	policy = validPolicy()
	policy.Fallback.MaxAttempts = 3
	if err := policy.Validate(); err == nil {
		t.Fatal("three attempts accepted")
	}
}

func TestJournalAndReceiptConditionalValidation(t *testing.T) {
	prior := "old"
	journal := ActivationJournal{SchemaVersion: "proxy/v1alpha1", ActivationID: id1, Nonce: strings.Repeat("a", 32), Generation: 1, State: JournalPrepared, OwnerUID: 1, Platform: PlatformLinux, Mode: ModeScopedRun, SessionIdentity: "uid:1", Policy: PolicyIdentity{ID: id2, Version: 1, Digest: digest}, Binary: BinaryIdentity{Path: "/usr/bin/blazn", Digest: digest}, Listener: ListenerIdentity{PID: 10, ProcessStartIdentity: "start", ExecutableIdentity: "exe", Address: "127.0.0.1:8080", ListenerKeyFingerprint: digest}, Environment: []EnvironmentMutation{{Name: EnvAnthropicAuthToken, PriorPresent: true, PriorValue: &prior, DesiredValueDigest: digest, ActivationMarker: "activation-marker", RollbackAction: RollbackRestore}}, CA: json.RawMessage("null"), RollbackActions: []RollbackStep{{Ordinal: 1, Operation: RollbackRestoreEnvironment, Target: "ANTHROPIC_AUTH_TOKEN"}}, CreatedAt: time.Now().UTC().Format(time.RFC3339), UpdatedAt: time.Now().UTC().Format(time.RFC3339), Checksum: digest}
	if err := journal.Validate(); err != nil {
		t.Fatal(err)
	}
	journal.Environment[0].PriorPresent = false
	if err := journal.Validate(); err == nil {
		t.Fatal("invalid journal conditional accepted")
	}
	receipt := ActivationReceipt{SchemaVersion: "proxy/v1alpha1", ActivationID: id1, Generation: 1, JournalDigest: digest, PolicyDigest: digest, Platform: PlatformLinux, Mode: ModeScopedRun, SessionIdentity: "uid:1", Listener: ReceiptListener{PID: 10, ProcessStartIdentity: "start", ExecutableIdentity: "exe", Address: "127.0.0.1:8080"}, PublicationMechanism: "process_environment", Environment: []ReceiptEnvironment{{Name: EnvAnthropicAuthToken, DesiredValueDigest: digest}}, ActivatedAt: time.Now().UTC().Format(time.RFC3339), State: "active", Checksum: digest}
	if err := receipt.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestStrictEventDecoderRejectsContentFields(t *testing.T) {
	event := `{"eventId":"` + id1 + `","cursor":"1","timestamp":"2026-01-01T00:00:00Z","type":"request_started","activationId":"` + id2 + `","logicalRequestId":"` + id1 + `","attempt":1,"protocol":"openai-chat","modelAlias":"local","policy":{"id":"` + id1 + `","version":1,"digest":"` + digest + `"},"routeId":"` + id2 + `","destinationClass":"local_node","outcome":"success","reasonCode":"none","latencyMs":1,"prompt":"secret"}`
	if _, err := DecodeEvent(strings.NewReader(event)); err == nil {
		t.Fatal("content-bearing event accepted")
	}
}

func TestNormalizedContracts(t *testing.T) {
	request := NormalizedRequest{LogicalRequestID: id1, Protocol: ProtocolOpenAIChat, ModelAlias: "local", DataClass: DataCompany, Blocks: []RequestBlock{{Role: "user", Type: "text", Text: "hello"}}, Tools: []Tool{}, Limits: NormalizedLimits{MaxOutputTokens: 10, DeadlineAt: "2026-01-01T00:00:00Z"}, CapabilitiesRequired: []Capability{CapabilityText}}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	response := NormalizedResponse{LogicalRequestID: id1, ModelAlias: "local", RouteID: id2, Blocks: []ResponseBlock{{Type: "text", Text: "ok"}}, FinishReason: "stop", Usage: Usage{}}
	if err := response.Validate(); err != nil {
		t.Fatal(err)
	}
	stream := NormalizedStreamEvent{LogicalRequestID: id1, Type: "response_start"}
	if err := stream.Validate(); err != nil {
		t.Fatal(err)
	}
	normalizedErr := NormalizedError{Code: "rate_limited", RetryClass: "before_first_byte_only", SafeMessage: "retry"}
	if err := normalizedErr.Validate(); err != nil {
		t.Fatal(err)
	}
}
