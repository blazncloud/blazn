package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentValidateIsOffline(t *testing.T) {
	p := filepath.Join(t.TempDir(), "agent.json")
	_ = os.WriteFile(p, []byte(`{"id":"i","agentId":"a","workspaceId":"w","version":1,"digest":"d","createdBy":"u","createdAt":"now"}`), 0600)
	out, errout := new(bytes.Buffer), new(bytes.Buffer)
	app := New(out, errout, BuildInfo{})
	app.agentHarness = func() (agentHarnessCommands, error) { t.Fatal("offline validation initialized API"); return nil, nil }
	if code := app.Run([]string{"agent", "validate", "--file", p}); code != ExitSuccess || !strings.Contains(out.String(), "valid") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errout)
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
