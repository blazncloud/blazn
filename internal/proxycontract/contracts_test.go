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

func journalEnvironment(prior *string) []EnvironmentMutation {
	names := []EnvironmentName{EnvOpenAIBaseURL, EnvOpenAIAPIKey, EnvAnthropicBaseURL, EnvAnthropicAPIKey, EnvAnthropicAuthToken}
	out := make([]EnvironmentMutation, 0, len(names))
	for _, name := range names {
		out = append(out, EnvironmentMutation{Name: name, PriorPresent: prior != nil, PriorValue: prior, DesiredValueDigest: digest, ActivationMarker: "activation-marker", RollbackAction: RollbackRestore})
		if prior == nil {
			out[len(out)-1].RollbackAction = RollbackRemove
		}
	}
	return out
}

func receiptEnvironment() []ReceiptEnvironment {
	names := []EnvironmentName{EnvOpenAIBaseURL, EnvOpenAIAPIKey, EnvAnthropicBaseURL, EnvAnthropicAPIKey, EnvAnthropicAuthToken}
	out := make([]ReceiptEnvironment, 0, len(names))
	for _, name := range names {
		out = append(out, ReceiptEnvironment{Name: name, DesiredValueDigest: digest, ActivationMarker: "activation-marker"})
	}
	return out
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
	journal := ActivationJournal{SchemaVersion: "proxy/v1alpha1", ActivationID: id1, Nonce: strings.Repeat("a", 32), Generation: 1, State: JournalPrepared, OwnerUID: 1, Platform: PlatformLinux, Mode: ModeScopedRun, SessionIdentity: "uid:1", Policy: PolicyIdentity{ID: id2, Version: 1, Digest: digest}, Binary: BinaryIdentity{Path: "/usr/bin/blazn", Digest: digest}, Listener: ListenerIdentity{PID: 10, ProcessStartIdentity: "start", ExecutableIdentity: "exe", Address: "127.0.0.1:8080", ListenerKeyFingerprint: digest}, Environment: journalEnvironment(&prior), CA: json.RawMessage("null"), RollbackActions: []RollbackStep{{Ordinal: 1, Operation: RollbackRestoreEnvironment, Target: "ANTHROPIC_AUTH_TOKEN"}}, CreatedAt: time.Now().UTC().Format(time.RFC3339), UpdatedAt: time.Now().UTC().Format(time.RFC3339), Checksum: digest}
	if err := journal.Validate(); err != nil {
		t.Fatal(err)
	}
	journal.Environment[0].PriorPresent = false
	if err := journal.Validate(); err == nil {
		t.Fatal("invalid journal conditional accepted")
	}
	receipt := ActivationReceipt{SchemaVersion: "proxy/v1alpha1", ActivationID: id1, Nonce: strings.Repeat("a", 32), Generation: 1, OwnerUID: 1, JournalDigest: digest, PolicyDigest: digest, Platform: PlatformLinux, Mode: ModeScopedRun, SessionIdentity: "uid:1", Binary: BinaryIdentity{Path: "/usr/bin/blazn", Digest: digest}, Listener: ReceiptListener{PID: 10, ProcessStartIdentity: "start", ExecutableIdentity: "exe", Address: "127.0.0.1:8080", ListenerKeyFingerprint: digest}, PublicationMechanism: "process_environment", Environment: receiptEnvironment(), RollbackSummary: []RollbackStep{{Ordinal: 1, Operation: RollbackRestoreEnvironment, Target: "ANTHROPIC_AUTH_TOKEN"}}, ActivatedAt: time.Now().UTC().Format(time.RFC3339), State: "active", Checksum: digest}
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
	hello, ok := "hello", "ok"
	request := NormalizedRequest{LogicalRequestID: id1, Protocol: ProtocolOpenAIChat, ModelAlias: "local", DataClass: DataCompany, Blocks: []RequestBlock{{Role: "user", Type: "text", Text: &hello}}, Tools: []Tool{}, Limits: NormalizedLimits{MaxOutputTokens: 10, DeadlineAt: "2026-01-01T00:00:00Z"}, CapabilitiesRequired: []Capability{CapabilityText}}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	response := NormalizedResponse{LogicalRequestID: id1, ModelAlias: "local", RouteID: id2, Blocks: []ResponseBlock{{Type: "text", Text: &ok}}, FinishReason: "stop", Usage: Usage{}}
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

func TestPolicySemanticBoundaryCostAndFallbackMatrix(t *testing.T) {
	for name, mutate := range map[string]func(*Policy){
		"class boundary": func(p *Policy) { p.Routes[0].DataBoundary = BoundaryCompany },
		"local only escape": func(p *Policy) {
			a := p.Aliases["local"]
			a.DataClass = DataLocalOnly
			a.AllowedDestinationBoundaries = []DataBoundary{BoundaryLocal, BoundaryCompany}
			p.Aliases["local"] = a
			p.Routes[0].AcceptedDataClasses = []DataClass{DataLocalOnly}
		},
		"cost cap": func(p *Policy) { p.Routes[0].CostClass = CostMeteredLow },
	} {
		t.Run(name, func(t *testing.T) {
			p := validPolicy()
			mutate(&p)
			if err := p.Validate(); err == nil {
				t.Fatal("invalid policy accepted")
			}
		})
	}
	p := validPolicy()
	second := p.Routes[0]
	second.ID = id2
	second.DestinationClass = DestinationCompany
	second.DataBoundary = BoundaryCompany
	second.Endpoint.ResolvedAddressPolicy = AddressPublicUnicast
	p.Routes = append(p.Routes, second)
	a := p.Aliases["local"]
	a.RouteIDs = []string{id1, id2}
	a.AllowedDestinationBoundaries = []DataBoundary{BoundaryLocal, BoundaryCompany}
	p.Aliases["local"] = a
	if err := p.Validate(); err == nil {
		t.Fatal("undeclared fallback transition accepted")
	}
	p.Fallback.AllowedBoundaryTransitions = []BoundaryTransition{"local_to_company"}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestRequiredBooleanPresence(t *testing.T) {
	policy := validPolicy()
	encoded, _ := json.Marshal(policy)
	encoded = []byte(strings.Replace(string(encoded), `"contentCapture":false`, "", 1))
	encoded = []byte(strings.Replace(string(encoded), ",}", "}", 1))
	if _, err := DecodePolicy(strings.NewReader(string(encoded))); err == nil {
		t.Fatal("absent contentCapture accepted")
	}
	requestJSON := `{"logicalRequestId":"` + id1 + `","protocol":"openai-chat","modelAlias":"local","dataClass":"company","blocks":[{"role":"user","type":"text","text":"x"}],"tools":[],"limits":{"maxOutputTokens":1,"deadlineAt":"2026-01-01T00:00:00Z"},"capabilitiesRequired":[]}`
	if _, err := DecodeNormalizedRequest(strings.NewReader(requestJSON)); err == nil {
		t.Fatal("absent stream accepted")
	}
	journal := `{"environment":[{"name":"OPENAI_BASE_URL"}]}`
	if err := requiredArrayNested([]byte(journal), "environment", "priorPresent"); err == nil {
		t.Fatal("absent priorPresent accepted")
	}
}

func TestDiscriminatedUnionNegativeMatrix(t *testing.T) {
	text := "x"
	call := "c"
	tool := "t"
	requests := []RequestBlock{{Role: "user", Type: "text"}, {Role: "user", Type: "text", Text: &text, CallID: &call}, {Role: "user", Type: "tool_call", CallID: &call, ToolName: &tool, Arguments: json.RawMessage(`{}`)}, {Role: "tool", Type: "tool_result", CallID: &call}}
	for i, value := range requests {
		if err := value.Validate(); err == nil {
			t.Fatalf("invalid request union %d accepted", i)
		}
	}
	responses := []ResponseBlock{{Type: "text"}, {Type: "text", Text: &text, Arguments: json.RawMessage(`{}`)}, {Type: "tool_call", CallID: &call, ToolName: &tool}}
	for i, value := range responses {
		if err := value.Validate(); err == nil {
			t.Fatalf("invalid response union %d accepted", i)
		}
	}
	events := []NormalizedStreamEvent{{LogicalRequestID: id1, Type: "text_delta"}, {LogicalRequestID: id1, Type: "response_start", Text: &text}, {LogicalRequestID: id1, Type: "error"}}
	for i, value := range events {
		if err := value.Validate(); err == nil {
			t.Fatalf("invalid stream union %d accepted", i)
		}
	}
}

func TestJournalReceiptBindingAndChecksum(t *testing.T) {
	prior := "old"
	journal := ActivationJournal{SchemaVersion: "proxy/v1alpha1", ActivationID: id1, Nonce: strings.Repeat("a", 32), Generation: 1, State: JournalActive, OwnerUID: 1, Platform: PlatformLinux, Mode: ModeScopedRun, SessionIdentity: "uid:1", Policy: PolicyIdentity{ID: id2, Version: 1, Digest: digest}, Binary: BinaryIdentity{Path: "/usr/bin/blazn", Digest: digest}, Listener: ListenerIdentity{PID: 10, ProcessStartIdentity: "start", ExecutableIdentity: "exe", Address: "127.0.0.1:8080", ListenerKeyFingerprint: digest}, Environment: journalEnvironment(&prior), CA: json.RawMessage("null"), RollbackActions: []RollbackStep{{Ordinal: 1, Operation: RollbackRestoreEnvironment, Target: "all"}}, CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z"}
	checksum, err := ContractChecksum(journal)
	if err != nil {
		t.Fatal(err)
	}
	journal.Checksum = checksum
	if err := VerifyContractChecksum(journal, checksum); err != nil {
		t.Fatal(err)
	}
	journalDigest, _ := ContractDigest(journal)
	receipt := ActivationReceipt{SchemaVersion: "proxy/v1alpha1", ActivationID: id1, Nonce: journal.Nonce, Generation: 1, OwnerUID: 1, JournalDigest: journalDigest, PolicyDigest: digest, Platform: PlatformLinux, Mode: ModeScopedRun, SessionIdentity: "uid:1", Binary: journal.Binary, Listener: ReceiptListener{PID: 10, ProcessStartIdentity: "start", ExecutableIdentity: "exe", Address: "127.0.0.1:8080", ListenerKeyFingerprint: digest}, PublicationMechanism: "process_environment", Environment: receiptEnvironment(), RollbackSummary: journal.RollbackActions, ActivatedAt: "2026-01-01T00:00:00Z", State: "active", Checksum: digest}
	if err := ValidateJournalReceipt(journal, receipt); err != nil {
		t.Fatal(err)
	}
	receipt.Environment[0].ActivationMarker = "different-marker-xx"
	if err := ValidateJournalReceipt(journal, receipt); err == nil {
		t.Fatal("cross-record mismatch accepted")
	}
	if receipt.AllowsEnvironmentRestore() {
		t.Fatal("receipt incorrectly grants environment restore")
	}
}
