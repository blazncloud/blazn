package plugin

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestExecProcessRunnerPassesOnlyValidatedRuntimeContext(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("BLAZN_PLUGIN_CONTEXT_HELPER", "1")
	t.Setenv(RuntimeContextEnvironment, `{"spoofed":true}`)
	runtimeContext := validRuntimeContext(t)
	var stdout bytes.Buffer
	code, err := (execProcessRunner{}).Run(context.Background(), executable, []string{"-test.run=TestRuntimeContextProcessHelper"}, runtimeContext, Stdio{Stdout: &stdout, Stderr: &stdout})
	if err != nil || code != 0 || strings.TrimSpace(stdout.String()) != runtimeContext.WorkspaceID {
		t.Fatalf("code=%d output=%q err=%v", code, stdout.String(), err)
	}
}

func TestRuntimeContextProcessHelper(t *testing.T) {
	if os.Getenv("BLAZN_PLUGIN_CONTEXT_HELPER") != "1" {
		return
	}
	runtimeContext, err := DecodeRuntimeContext(os.Getenv(RuntimeContextEnvironment))
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	_, _ = fmt.Fprintln(os.Stdout, runtimeContext.WorkspaceID)
	os.Exit(0)
}
