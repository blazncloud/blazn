package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestTemplateValidateDoesNotCreateAuthenticatedRuntime(t *testing.T) {
	manifestPath := "../../packages/contracts/testdata/sandbox/template-good.json"
	var stdout, stderr bytes.Buffer
	app := New(&stdout, &stderr, testBuild)
	app.sandbox = func() (sandboxCommands, error) {
		t.Fatal("offline validation created sandbox runtime")
		return nil, nil
	}
	if code := app.Run([]string{"template", "validate", "-f", manifestPath}); code != ExitSuccess {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestSandboxAndTemplatePublishUseRuntimeFactory(t *testing.T) {
	for _, args := range [][]string{{"sandbox", "list", "--workspace", "workspace-1"}, {"template", "publish", "-f", "missing", "--workspace", "workspace-1", "--request-id", "request-1"}} {
		var stdout, stderr bytes.Buffer
		app := New(&stdout, &stderr, testBuild)
		calls := 0
		app.sandbox = func() (sandboxCommands, error) { calls++; return nil, errors.New("no runtime") }
		if code := app.Run(args); code != ExitUnavailable || calls != 1 {
			t.Fatalf("args=%v code=%d calls=%d stdout=%q stderr=%q", args, code, calls, stdout.String(), stderr.String())
		}
	}
}

func TestSandboxTopicHelpDoesNotCreateRuntime(t *testing.T) {
	for _, topic := range []string{"template", "sandbox"} {
		var stdout, stderr bytes.Buffer
		app := New(&stdout, &stderr, testBuild)
		app.sandbox = func() (sandboxCommands, error) { t.Fatal("help created sandbox runtime"); return nil, nil }
		if code := app.Run([]string{"help", topic}); code != ExitSuccess || !strings.Contains(stdout.String(), "Usage:") || stderr.Len() != 0 {
			t.Fatalf("topic=%s code=%d stdout=%q stderr=%q", topic, code, stdout.String(), stderr.String())
		}
	}
}
