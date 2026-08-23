package proxycontract

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

const id1 = "123e4567-e89b-42d3-a456-426614174000"
const id2 = "123e4567-e89b-42d3-a456-426614174001"
const digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func validPolicy() Policy {
	return Policy{ID: id1, Version: 1, WorkspaceID: id2, Protocols: []Protocol{ProtocolOpenAIChat}, Aliases: map[string]Alias{"local": {RouteIDs: []string{id1}, DataClass: DataCompany, AllowedDestinationBoundaries: []DataBoundary{BoundaryLocal}}}, Routes: []Route{{ID: id1, DestinationClass: DestinationLocalNode, Endpoint: Endpoint{Scheme: "http", Hostname: "localhost", Port: 8080, BasePath: "/v1", HostnameAllowlist: []string{"localhost"}, ResolvedAddressPolicy: AddressLoopbackOnly}, SourceProtocols: []Protocol{ProtocolOpenAIChat}, DestinationProtocol: ProtocolOpenAIChat, Model: "qwen", Capabilities: []Capability{CapabilityText}, AcceptedDataClasses: []DataClass{DataCompany}, DataBoundary: BoundaryLocal, HealthTimeoutMS: 1000, CredentialRef: "node-route://qwen", CostClass: CostLocal}}, RequestLimits: RequestLimits{MaxContextTokens: 1000, MaxOutputTokens: 100, TimeoutMS: 1000, MaxCostClass: CostLocal}, Fallback: Fallback{MaxAttempts: 1, RetryableReasons: []RetryableReason{}, AllowedBoundaryTransitions: []BoundaryTransition{}}, ContentCapture: false}
}

func TestCanonicalPOCPolicyDecodesWithExactRouteSemantics(t *testing.T) {
	encoded, err := os.ReadFile("../../packages/contracts/proxy/fixtures/poc-policy.json")
	if err != nil {
		t.Fatal(err)
	}
	policy, err := DecodePolicy(strings.NewReader(string(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	expected := canonicalPOCPolicy()
	if !reflect.DeepEqual(policy, expected) {
		t.Fatalf("canonical POC policy does not match every frozen field\ngot:  %#v\nwant: %#v", policy, expected)
	}
}

func canonicalPOCPolicy() Policy {
	protocols := []Protocol{ProtocolOpenAIResponses, ProtocolOpenAIChat, ProtocolAnthropicMessages}
	localID := "33333333-3333-4333-8333-333333333333"
	cloudID := "44444444-4444-4444-8444-444444444444"
	return Policy{
		ID: "11111111-1111-4111-8111-111111111111", Version: 1, WorkspaceID: "22222222-2222-4222-8222-222222222222", Protocols: protocols,
		Aliases: map[string]Alias{
			"company-assistant":            {RouteIDs: []string{localID, cloudID}, DataClass: DataCompany, AllowedDestinationBoundaries: []DataBoundary{BoundaryLocal, BoundaryExternal}},
			"company-assistant-public":     {RouteIDs: []string{localID, cloudID}, DataClass: DataPublic, AllowedDestinationBoundaries: []DataBoundary{BoundaryLocal, BoundaryExternal}},
			"company-assistant-restricted": {RouteIDs: []string{localID}, DataClass: DataRestricted, AllowedDestinationBoundaries: []DataBoundary{BoundaryLocal}},
			"company-assistant-local":      {RouteIDs: []string{localID}, DataClass: DataLocalOnly, AllowedDestinationBoundaries: []DataBoundary{BoundaryLocal}},
		},
		Routes: []Route{
			{ID: localID, DestinationClass: DestinationLocalNode, Endpoint: Endpoint{Scheme: "http", Hostname: "127.0.0.1", Port: 11434, BasePath: "/v1", HostnameAllowlist: []string{"127.0.0.1"}, ResolvedAddressPolicy: AddressLoopbackOnly}, SourceProtocols: protocols, DestinationProtocol: ProtocolOpenAIChat, Model: "qwen3.8", Capabilities: []Capability{CapabilityText, CapabilityTools, CapabilityStructuredOutput, CapabilityStreaming}, AcceptedDataClasses: []DataClass{DataPublic, DataCompany, DataRestricted, DataLocalOnly}, DataBoundary: BoundaryLocal, HealthTimeoutMS: 2000, CredentialRef: "node-route://qwen38", CostClass: CostLocal},
			{ID: cloudID, DestinationClass: DestinationProvider, Endpoint: Endpoint{Scheme: "https", Hostname: "api.openai.com", Port: 443, BasePath: "/v1", HostnameAllowlist: []string{"api.openai.com"}, ResolvedAddressPolicy: AddressPublicUnicast}, SourceProtocols: protocols, DestinationProtocol: ProtocolOpenAIResponses, Model: "gpt-5.4", Capabilities: []Capability{CapabilityText, CapabilityTools, CapabilityStructuredOutput, CapabilityStreaming}, AcceptedDataClasses: []DataClass{DataPublic, DataCompany}, DataBoundary: BoundaryExternal, HealthTimeoutMS: 5000, CredentialRef: "workspace-vault://poc/model-providers/openai", CostClass: CostMeteredHigh},
		},
		RequestLimits:  RequestLimits{MaxContextTokens: 131072, MaxOutputTokens: 8192, TimeoutMS: 120000, Streaming: true, MaxCostClass: CostMeteredHigh},
		Fallback:       Fallback{MaxAttempts: 2, RetryableReasons: []RetryableReason{ReasonConnectionFailure, ReasonTimeoutBeforeFirstByte, ReasonRateLimited, ReasonUpstream5xx, ReasonModelUnavailable, ReasonCompatibleContextOverflow}, AllowedBoundaryTransitions: []BoundaryTransition{"local_to_external"}},
		ContentCapture: false,
	}
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

func validActivationRecords() (ActivationJournal, ActivationReceipt) {
	prior := "old"
	journal := ActivationJournal{SchemaVersion: "proxy/v1alpha1", ActivationID: id1, Nonce: strings.Repeat("a", 32), Generation: 1, State: JournalActive, OwnerUID: 1, Platform: PlatformLinux, Mode: ModeScopedRun, SessionIdentity: "uid:1", Policy: PolicyIdentity{ID: id2, Version: 1, Digest: digest}, Binary: BinaryIdentity{Path: "/usr/bin/blazn", Digest: digest}, Listener: ListenerIdentity{PID: 10, ProcessStartIdentity: "start", ExecutableIdentity: "exe", Address: "127.0.0.1:8080", ListenerKeyFingerprint: digest}, Environment: journalEnvironment(&prior), CA: json.RawMessage("null"), RollbackActions: []RollbackStep{}, CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z", Checksum: digest}
	receipt := ActivationReceipt{SchemaVersion: "proxy/v1alpha1", ActivationID: id1, Nonce: journal.Nonce, Generation: 1, OwnerUID: 1, JournalDigest: digest, PolicyDigest: digest, Platform: PlatformLinux, Mode: ModeScopedRun, SessionIdentity: "uid:1", Binary: journal.Binary, Listener: ReceiptListener{PID: 10, ProcessStartIdentity: "start", ExecutableIdentity: "exe", Address: "127.0.0.1:8080", ListenerKeyFingerprint: digest}, PublicationMechanism: "process_environment", Environment: receiptEnvironment(), RollbackSummary: []RollbackStep{}, ActivatedAt: "2026-01-01T00:00:00Z", State: "active", Checksum: digest}
	return journal, receipt
}

func TestRequiredCollectionsAndZeroUsageRoundTrip(t *testing.T) {
	hello := "hello"
	policy := validPolicy()
	request := NormalizedRequest{LogicalRequestID: id1, Protocol: ProtocolOpenAIChat, ModelAlias: "local", DataClass: DataCompany, Blocks: []RequestBlock{{Role: "user", Type: "text", Text: &hello}}, Tools: []Tool{}, Limits: NormalizedLimits{MaxOutputTokens: 10, DeadlineAt: "2026-01-01T00:00:00Z"}, CapabilitiesRequired: []Capability{}}
	response := NormalizedResponse{LogicalRequestID: id1, ModelAlias: "local", RouteID: id2, Blocks: []ResponseBlock{}, FinishReason: "stop", Usage: Usage{}}
	journal, receipt := validActivationRecords()

	for name, roundTrip := range map[string]func() error{
		"policy": func() error {
			if err := policy.Validate(); err != nil {
				return err
			}
			encoded, err := json.Marshal(policy)
			if err != nil {
				return err
			}
			_, err = DecodePolicy(strings.NewReader(string(encoded)))
			return err
		},
		"request": func() error {
			if err := request.Validate(); err != nil {
				return err
			}
			encoded, err := json.Marshal(request)
			if err != nil {
				return err
			}
			_, err = DecodeNormalizedRequest(strings.NewReader(string(encoded)))
			return err
		},
		"response": func() error {
			if err := response.Validate(); err != nil {
				return err
			}
			encoded, err := json.Marshal(response)
			if err != nil {
				return err
			}
			if !strings.Contains(string(encoded), `"usage":{"inputTokens":0,"outputTokens":0}`) {
				return fmt.Errorf("required zero usage fields missing from %s", encoded)
			}
			_, err = DecodeNormalizedResponse(strings.NewReader(string(encoded)))
			return err
		},
		"journal": func() error {
			if err := journal.Validate(); err != nil {
				return err
			}
			encoded, err := json.Marshal(journal)
			if err != nil {
				return err
			}
			_, err = DecodeActivationJournal(strings.NewReader(string(encoded)))
			return err
		},
		"receipt": func() error {
			if err := receipt.Validate(); err != nil {
				return err
			}
			encoded, err := json.Marshal(receipt)
			if err != nil {
				return err
			}
			_, err = DecodeActivationReceipt(strings.NewReader(string(encoded)))
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := roundTrip(); err != nil {
				t.Fatal(err)
			}
		})
	}

	for name, invalid := range map[string]func() error{
		"retry reasons": func() error { value := validPolicy(); value.Fallback.RetryableReasons = nil; return value.Validate() },
		"boundary transitions": func() error {
			value := validPolicy()
			value.Fallback.AllowedBoundaryTransitions = nil
			return value.Validate()
		},
		"journal rollback": func() error {
			value, _ := validActivationRecords()
			value.RollbackActions = nil
			return value.Validate()
		},
		"receipt rollback": func() error {
			_, value := validActivationRecords()
			value.RollbackSummary = nil
			return value.Validate()
		},
		"request tools":        func() error { value := request; value.Tools = nil; return value.Validate() },
		"request capabilities": func() error { value := request; value.CapabilitiesRequired = nil; return value.Validate() },
		"response blocks":      func() error { value := response; value.Blocks = nil; return value.Validate() },
	} {
		t.Run("nil "+name, func(t *testing.T) {
			if err := invalid(); err == nil {
				t.Fatal("nil required collection accepted")
			}
		})
	}
}

func TestReceiptPlatformModePublicationMatrix(t *testing.T) {
	for name, configure := range map[string]func(*ActivationReceipt){
		"linux auto scoped": func(r *ActivationReceipt) {
			r.Platform = PlatformLinux
			r.Mode = ModeScopedRun
			r.PublicationMechanism = "process_environment"
		},
		"darwin scoped": func(r *ActivationReceipt) {
			r.Platform = PlatformDarwin
			r.Mode = ModeScopedRun
			r.PublicationMechanism = "process_environment"
		},
		"darwin session": func(r *ActivationReceipt) {
			r.Platform = PlatformDarwin
			r.Mode = ModeSession
			r.PublicationMechanism = "launchctl_user_environment"
		},
		"linux session": func(r *ActivationReceipt) {
			r.Platform = PlatformLinux
			r.Mode = ModeSession
			r.PublicationMechanism = "systemd_user_environment"
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, receipt := validActivationRecords()
			configure(&receipt)
			if err := receipt.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
	for name, configure := range map[string]func(*ActivationReceipt){
		"scoped launchctl": func(r *ActivationReceipt) {
			r.Platform = PlatformDarwin
			r.Mode = ModeScopedRun
			r.PublicationMechanism = "launchctl_user_environment"
		},
		"darwin session systemd": func(r *ActivationReceipt) {
			r.Platform = PlatformDarwin
			r.Mode = ModeSession
			r.PublicationMechanism = "systemd_user_environment"
		},
		"linux session process": func(r *ActivationReceipt) {
			r.Platform = PlatformLinux
			r.Mode = ModeSession
			r.PublicationMechanism = "process_environment"
		},
	} {
		t.Run("reject "+name, func(t *testing.T) {
			_, receipt := validActivationRecords()
			configure(&receipt)
			if err := receipt.Validate(); err == nil {
				t.Fatal("invalid platform/mode/publication tuple accepted")
			}
		})
	}
}

func TestAliasMustCoverEveryEnabledProtocol(t *testing.T) {
	policy := validPolicy()
	policy.Protocols = []Protocol{ProtocolOpenAIChat, ProtocolAnthropicMessages}
	if err := policy.Validate(); err == nil {
		t.Fatal("alias without an Anthropic-capable route accepted")
	}
	policy.Routes[0].SourceProtocols = append(policy.Routes[0].SourceProtocols, ProtocolAnthropicMessages)
	if err := policy.Validate(); err != nil {
		t.Fatal(err)
	}
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
	second.CredentialRef = "workspace-vault://poc/company"
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
	receiptChecksum, err := ContractChecksum(receipt)
	if err != nil {
		t.Fatal(err)
	}
	receipt.Checksum = receiptChecksum
	if err := ValidateJournalReceipt(journal, receipt); err != nil {
		t.Fatal(err)
	}
	if !receipt.AllowsVerifiedListenerStop() {
		t.Fatal("verified receipt did not authorize listener stop")
	}
	tamperedJournal := journal
	tamperedJournal.Listener.PID++
	if err := ValidateJournalReceipt(tamperedJournal, receipt); err == nil {
		t.Fatal("journal with stale checksum authorized recovery")
	}
	tamperedReceipt := receipt
	tamperedReceipt.Listener.PID++
	if tamperedReceipt.AllowsVerifiedListenerStop() {
		t.Fatal("receipt with stale checksum authorized listener stop")
	}
	fakeChecksum := receipt
	fakeChecksum.Checksum = digest
	if fakeChecksum.AllowsVerifiedListenerStop() {
		t.Fatal("receipt with syntactically valid fake checksum authorized listener stop")
	}
	receipt.Environment[0].ActivationMarker = "different-marker-xx"
	if err := ValidateJournalReceipt(journal, receipt); err == nil {
		t.Fatal("cross-record mismatch accepted")
	}
	if receipt.AllowsEnvironmentRestore() {
		t.Fatal("receipt incorrectly grants environment restore")
	}
}

func TestDecoderMatchesSchemaEnumsNullsAndUnions(t *testing.T) {
	validRequest := `{"logicalRequestId":"` + id1 + `","protocol":"openai-chat","modelAlias":"local","dataClass":"company","stream":false,"blocks":[{"role":"user","type":"text","text":"x"}],"tools":[],"toolChoice":"auto","limits":{"maxOutputTokens":1,"deadlineAt":"2026-01-01T00:00:00Z"},"capabilitiesRequired":[]}`
	for name, encoded := range map[string]string{
		"tool choice enum":     strings.Replace(validRequest, `"toolChoice":"auto"`, `"toolChoice":"sometimes"`, 1),
		"tool choice null":     strings.Replace(validRequest, `"toolChoice":"auto"`, `"toolChoice":null`, 1),
		"stream null":          strings.Replace(validRequest, `"stream":false`, `"stream":null`, 1),
		"forbidden null union": strings.Replace(validRequest, `"text":"x"`, `"text":"x","callId":null`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeNormalizedRequest(strings.NewReader(encoded)); err == nil {
				t.Fatal("schema-invalid request accepted")
			}
		})
	}
	validStream := `{"logicalRequestId":"` + id1 + `","sequence":0,"type":"tool_call_start","callId":"call","toolName":"tool"}`
	for name, encoded := range map[string]string{
		"empty call id":         strings.Replace(validStream, `"callId":"call"`, `"callId":""`, 1),
		"empty tool name":       strings.Replace(validStream, `"toolName":"tool"`, `"toolName":""`, 1),
		"required call id null": strings.Replace(validStream, `"callId":"call"`, `"callId":null`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeNormalizedStreamEvent(strings.NewReader(encoded)); err == nil {
				t.Fatal("schema-invalid stream event accepted")
			}
		})
	}
	event := `{"eventId":"` + id1 + `","cursor":"1","timestamp":"2026-01-01T00:00:00Z","type":"request_started","activationId":"` + id2 + `","logicalRequestId":"` + id1 + `","attempt":1,"protocol":"openai-chat","modelAlias":"local","policy":{"id":"` + id1 + `","version":1,"digest":"` + digest + `"},"routeId":"` + id2 + `","destinationClass":"local_node","outcome":"success","reasonCode":"invented","latencyMs":0}`
	if _, err := DecodeEvent(strings.NewReader(event)); err == nil {
		t.Fatal("unknown reasonCode accepted")
	}
}

func TestPOCDestinationProtocolsAndCredentialReferences(t *testing.T) {
	for name, mutate := range map[string]func(*Policy){
		"anthropic destination": func(policy *Policy) { policy.Routes[0].DestinationProtocol = ProtocolAnthropicMessages },
		"local vault ref":       func(policy *Policy) { policy.Routes[0].CredentialRef = "workspace-vault://poc/qwen" },
		"empty ref path":        func(policy *Policy) { policy.Routes[0].CredentialRef = "node-route://" },
		"query ref":             func(policy *Policy) { policy.Routes[0].CredentialRef = "node-route://qwen?token=x" },
		"traversal ref":         func(policy *Policy) { policy.Routes[0].CredentialRef = "node-route://poc/../qwen" },
	} {
		t.Run(name, func(t *testing.T) {
			policy := validPolicy()
			mutate(&policy)
			if err := policy.Validate(); err == nil {
				t.Fatal("invalid POC route accepted")
			}
		})
	}
}

func TestActivationSchemaConformanceBoundaries(t *testing.T) {
	journal, receipt := validActivationRecords()
	encoded, err := json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	withoutCA := strings.Replace(string(encoded), `,"ca":null`, "", 1)
	if _, err := DecodeActivationJournal(strings.NewReader(withoutCA)); err == nil {
		t.Fatal("journal without explicit ca:null accepted")
	}
	receipt.Listener.Address = "192.0.2.10:8080"
	if err := receipt.Validate(); err == nil {
		t.Fatal("non-loopback receipt listener accepted")
	}
}

func TestNormalizedRequestRoleAndToolSemantics(t *testing.T) {
	text := "x"
	base := NormalizedRequest{LogicalRequestID: id1, Protocol: ProtocolOpenAIChat, ModelAlias: "local", DataClass: DataCompany, Stream: false, Blocks: []RequestBlock{{Role: "user", Type: "text", Text: &text}}, Tools: []Tool{}, Limits: NormalizedLimits{MaxOutputTokens: 1, DeadlineAt: "2026-01-01T00:00:00Z"}, CapabilitiesRequired: []Capability{CapabilityText}}
	toolText := base
	toolText.Blocks = []RequestBlock{{Role: "tool", Type: "text", Text: &text}}
	if err := toolText.Validate(); err == nil {
		t.Fatal("tool-role text accepted")
	}
	requiredWithoutTools := base
	requiredWithoutTools.ToolChoice = ToolChoiceRequired
	if err := requiredWithoutTools.Validate(); err == nil {
		t.Fatal("required tool choice without tools accepted")
	}
	duplicateTools := base
	duplicateTools.Tools = []Tool{{Name: "lookup", InputSchema: map[string]any{}}, {Name: "lookup", InputSchema: map[string]any{}}}
	if err := duplicateTools.Validate(); err == nil {
		t.Fatal("duplicate tool names accepted")
	}
}

func TestNormalizedStreamUnionIsExclusive(t *testing.T) {
	text, callID, toolName, arguments := "x", "call", "tool", "{}"
	usage, finish := &Usage{}, FinishReason("stop")
	normalizedError := &NormalizedError{Code: "internal_error", RetryClass: "never", SafeMessage: "failed"}
	invalid := []NormalizedStreamEvent{
		{LogicalRequestID: id1, Type: "response_start", CallID: &callID},
		{LogicalRequestID: id1, Type: "text_delta", Text: &text, FinishReason: &finish},
		{LogicalRequestID: id1, Type: "tool_call_start", CallID: &callID, ToolName: &toolName, Usage: usage},
		{LogicalRequestID: id1, Type: "tool_arguments_delta", CallID: &callID, ArgumentsDelta: &arguments, FinishReason: &finish},
		{LogicalRequestID: id1, Type: "usage", Usage: usage, CallID: &callID},
		{LogicalRequestID: id1, Type: "response_end", FinishReason: &finish, Usage: usage},
		{LogicalRequestID: id1, Type: "error", Error: normalizedError, ToolName: &toolName},
	}
	for index, event := range invalid {
		if err := event.Validate(); err == nil {
			t.Fatalf("stream union case %d accepted", index)
		}
	}
	missingUsageFields := `{"logicalRequestId":"` + id1 + `","sequence":0,"type":"usage","usage":{}}`
	if _, err := DecodeNormalizedStreamEvent(strings.NewReader(missingUsageFields)); err == nil {
		t.Fatal("usage event without token fields accepted")
	}
}

func TestEventSemanticMatrix(t *testing.T) {
	base := Event{EventID: id1, Cursor: "1", Timestamp: "2026-01-01T00:00:00Z", Type: EventRequestStarted, ActivationID: id2, LogicalRequestID: id1, Attempt: 1, Protocol: ProtocolOpenAIChat, ModelAlias: "local", Policy: PolicyIdentity{ID: id1, Version: 1, Digest: digest}, RouteID: id2, DestinationClass: DestinationLocalNode, Outcome: OutcomeSuccess, ReasonCode: EventReasonNone}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	invalid := []Event{
		func() Event { value := base; value.Outcome = OutcomeFailed; return value }(),
		func() Event {
			value := base
			value.Type = EventRouteSelected
			value.Outcome = OutcomeFallback
			value.ReasonCode = EventReasonNone
			return value
		}(),
		func() Event {
			value := base
			value.Type = EventAttemptFinished
			value.Outcome = OutcomeFailed
			value.ReasonCode = EventReasonFallbackSelected
			return value
		}(),
		func() Event {
			value := base
			value.Type = EventRequestFinished
			value.Outcome = OutcomeSuccess
			value.ReasonCode = EventReasonRateLimited
			return value
		}(),
		func() Event {
			value := base
			value.Type = EventRequestCancelled
			value.Outcome = OutcomeCancelled
			value.ReasonCode = EventReasonCancelled
			value.Usage = &Usage{}
			return value
		}(),
	}
	for index, event := range invalid {
		if err := event.Validate(); err == nil {
			t.Fatalf("event semantic case %d accepted", index)
		}
	}
	missingUsageFields := `{"eventId":"` + id1 + `","cursor":"1","timestamp":"2026-01-01T00:00:00Z","type":"request_finished","activationId":"` + id2 + `","logicalRequestId":"` + id1 + `","attempt":1,"protocol":"openai-chat","modelAlias":"local","policy":{"id":"` + id1 + `","version":1,"digest":"` + digest + `"},"routeId":"` + id2 + `","destinationClass":"local_node","outcome":"success","reasonCode":"none","latencyMs":1,"usage":{}}`
	if _, err := DecodeEvent(strings.NewReader(missingUsageFields)); err == nil {
		t.Fatal("event usage without token fields accepted")
	}
}

func TestCanonicalDigestVector(t *testing.T) {
	got, err := ContractDigest(map[string]any{"z": 1, "a": "<>&"})
	if err != nil {
		t.Fatal(err)
	}
	const want = "sha256:4ad7cdde6a20d82804d6653117fcc8e99692008e7464fb732969740847efc601"
	if got != want {
		t.Fatalf("canonical digest=%s want=%s", got, want)
	}
}

func TestCanonicalJSONUsesRFC8785UTF16PropertyOrder(t *testing.T) {
	value := map[string]any{"\u20ac": "Euro Sign", "\r": "Carriage Return", "\ufb33": "Hebrew Letter Dalet With Dagesh", "1": "One", "😀": "Emoji: Grinning Face", "\u0080": "Control", "ö": "Latin Small Letter O With Diaeresis"}
	got, err := canonicalJSON(value)
	if err != nil {
		t.Fatal(err)
	}
	want := "{\"\\r\":\"Carriage Return\",\"1\":\"One\",\"\u0080\":\"Control\",\"ö\":\"Latin Small Letter O With Diaeresis\",\"€\":\"Euro Sign\",\"😀\":\"Emoji: Grinning Face\",\"דּ\":\"Hebrew Letter Dalet With Dagesh\"}"
	if string(got) != want {
		t.Fatalf("canonical JSON=%s want=%s", got, want)
	}
}
