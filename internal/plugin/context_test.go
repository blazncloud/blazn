package plugin

import (
	"encoding/json"
	"strings"
	"testing"
)

func validRuntimeContext(t *testing.T) RuntimeContext {
	t.Helper()
	context, err := NewRuntimeContext("v0.1.0-poc.4", "json")
	if err != nil {
		t.Fatal(err)
	}
	context.Status = "selected"
	context.ReasonCode = ""
	context.APIOrigin = "https://blazn.example.test"
	context.UserID = "user-1"
	context.WorkspaceID = "workspace-1"
	return context
}

func TestRuntimeContextRoundTripAndSpoofReplacement(t *testing.T) {
	context := validRuntimeContext(t)
	environment, err := runtimeEnvironment([]string{
		"PATH=/bin",
		"LC_MESSAGES=C",
		"OPENAI_API_KEY=must-not-pass",
		"AWS_SESSION_TOKEN=must-not-pass",
		"GH_TOKEN=must-not-pass",
		strings.ToLower(RuntimeContextEnvironment) + `={"spoofed":true}`,
	}, context)
	if err != nil {
		t.Fatal(err)
	}
	if len(environment) != 3 || environment[0] != "PATH=/bin" || environment[1] != "LC_MESSAGES=C" || !strings.HasPrefix(environment[2], RuntimeContextEnvironment+"=") {
		t.Fatalf("environment = %#v", environment)
	}
	actual, err := DecodeRuntimeContext(strings.TrimPrefix(environment[2], RuntimeContextEnvironment+"="))
	if err != nil {
		t.Fatal(err)
	}
	if actual.WorkspaceID != context.WorkspaceID || actual.InvocationID != context.InvocationID {
		t.Fatalf("runtime context = %#v", actual)
	}
}

func TestDecodeRuntimeContextRejectsUnknownAndTrailingData(t *testing.T) {
	context := validRuntimeContext(t)
	encoded, err := json.Marshal(context)
	if err != nil {
		t.Fatal(err)
	}
	unknown := strings.TrimSuffix(string(encoded), "}") + `,"secret":"must-not-pass"}`
	for _, value := range []string{unknown, string(encoded) + `{}`} {
		if _, err := DecodeRuntimeContext(value); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
}

func TestRuntimeContextAllowsUnselectedAndUnavailableStates(t *testing.T) {
	for _, status := range []string{"unselected", "unavailable"} {
		context, err := NewRuntimeContext("dev", "human")
		if err != nil {
			t.Fatal(err)
		}
		context.Status = status
		context.ReasonCode = "workspace_not_selected"
		context.APIOrigin = "http://localhost:8789"
		if err := context.Validate(); err != nil {
			t.Fatalf("status %s: %v", status, err)
		}
	}
}

func TestRuntimeContextRejectsUnsafeOrIncoherentValues(t *testing.T) {
	base := validRuntimeContext(t)
	tests := []func(*RuntimeContext){
		func(value *RuntimeContext) { value.SchemaVersion = 2 },
		func(value *RuntimeContext) { value.InvocationID = "bad" },
		func(value *RuntimeContext) { value.CoreVersion = "v1\ninjected" },
		func(value *RuntimeContext) { value.OutputFormat = "yaml" },
		func(value *RuntimeContext) { value.Status = "unknown" },
		func(value *RuntimeContext) { value.Status, value.ReasonCode = "unselected", "workspace_required" },
		func(value *RuntimeContext) { value.APIOrigin = "http://metadata.internal" },
	}
	for index, mutate := range tests {
		value := base
		mutate(&value)
		if err := value.Validate(); err == nil {
			t.Fatalf("case %d passed: %#v", index, value)
		}
	}
}

func TestRuntimeContextRejectsOversizedResourceIdentity(t *testing.T) {
	context := validRuntimeContext(t)
	context.WorkspaceID = strings.Repeat("w", 256)
	if err := context.Validate(); err == nil {
		t.Fatal("oversized Workspace identity passed")
	}
}

func TestRuntimeContextRejectsControlCharactersInResourceIdentity(t *testing.T) {
	context := validRuntimeContext(t)
	context.WorkspaceID = "workspace-1\ninjected"
	if err := context.Validate(); err == nil {
		t.Fatal("Workspace identity with a newline passed")
	}
}
