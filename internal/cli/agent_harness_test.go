package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blazncloud/blazn/internal/client"
)

func TestAgentValidateIsOffline(t *testing.T) {
	p := filepath.Join(t.TempDir(), "agent.json")
	fixture, err := os.ReadFile("../../packages/contracts/testdata/harness/agent-good.json")
	if err != nil {
		t.Fatal(err)
	}
	var bundle map[string]any
	if err := json.Unmarshal(fixture, &bundle); err != nil {
		t.Fatal(err)
	}
	version, _ := json.Marshal(bundle["version"])
	_ = os.WriteFile(p, version, 0600)
	out, errout := new(bytes.Buffer), new(bytes.Buffer)
	app := New(out, errout, BuildInfo{})
	app.agentHarness = func() (agentHarnessCommands, error) { t.Fatal("offline validation initialized API"); return nil, nil }
	if code := app.Run([]string{"agent", "validate", "--file", p}); code != ExitSuccess || !strings.Contains(out.String(), "valid") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errout)
	}
}

func TestAgentHarnessAPIErrorExitMapping(t *testing.T) {
	for _, tc := range []struct {
		code     string
		wantExit int
		wantCode string
	}{{"access_expired", ExitUnavailable, "access_expired"}, {"session_revoked", ExitUnavailable, "session_revoked"}, {"device_revoked", ExitUnavailable, "device_revoked"}, {"unauthorized", ExitUnavailable, "unauthorized"}, {"permission_denied", ExitFailure, "permission_denied"}, {"", ExitFailure, "api_error"}} {
		t.Run(tc.wantCode, func(t *testing.T) {
			out, errout := new(bytes.Buffer), new(bytes.Buffer)
			app := New(out, errout, BuildInfo{})
			apiErr := &client.APIError{StatusCode: 401, Body: client.ErrorBody{Code: tc.code, Message: "denied"}}
			if got := app.writeAgentHarnessError(OutputJSON, apiErr); got != tc.wantExit {
				t.Fatalf("exit=%d want=%d", got, tc.wantExit)
			}
			if errout.Len() != 0 || !strings.Contains(out.String(), `"code":"`+tc.wantCode+`"`) || !strings.Contains(out.String(), fmt.Sprintf(`"exitCode":%d`, tc.wantExit)) {
				t.Fatalf("out=%q err=%q", out, errout)
			}
		})
	}
}
func TestAgentAndHarnessHelpAreWired(t *testing.T) {
	for _, topic := range []string{"agent", "harness"} {
		code, out, err := runApp(t, "help", topic)
		if code != ExitSuccess || err != "" || !strings.Contains(out, "Usage:") {
			t.Fatalf("%s code=%d out=%q err=%q", topic, code, out, err)
		}
	}
}

func TestAgentValidatePreservesFileAndTrailingJSONErrors(t *testing.T) {
	out, errout := new(bytes.Buffer), new(bytes.Buffer)
	app := New(out, errout, BuildInfo{})
	missing := filepath.Join(t.TempDir(), "missing.json")
	if code := app.Run([]string{"agent", "validate", "--file", missing}); code != ExitUsage || !strings.Contains(errout.String(), "missing.json") {
		t.Fatalf("missing file code=%d err=%q", code, errout)
	}
	errout.Reset()
	bad := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(bad, []byte(`{} garbage`), 0o600)
	if code := app.Run([]string{"agent", "validate", "--file", bad}); code != ExitUsage || !strings.Contains(errout.String(), "trailing JSON") {
		t.Fatalf("trailing JSON code=%d err=%q", code, errout)
	}
}
