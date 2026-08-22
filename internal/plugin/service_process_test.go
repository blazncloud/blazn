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
	t.Setenv(RuntimeContextEnvironment, `{"spoofed":true}`)
	t.Setenv("OPENAI_API_KEY", "must-not-pass")
	runtimeContext := validRuntimeContext(t)
	var stdout bytes.Buffer
	code, err := (execProcessRunner{}).Run(context.Background(), executable, []string{"-test.run=TestRuntimeContextProcessHelper"}, runtimeContext, Stdio{Stdout: &stdout, Stderr: &stdout})
	if err != nil || code != 0 || strings.TrimSpace(stdout.String()) != runtimeContext.WorkspaceID {
		t.Fatalf("code=%d output=%q err=%v", code, stdout.String(), err)
	}
}

func TestRuntimeContextProcessHelper(t *testing.T) {
	encoded := os.Getenv(RuntimeContextEnvironment)
	if encoded == "" || encoded == `{"spoofed":true}` {
		return
	}
	if os.Getenv("OPENAI_API_KEY") != "" {
		_, _ = fmt.Fprintln(os.Stderr, "secret environment was inherited")
		os.Exit(3)
	}
	runtimeContext, err := DecodeRuntimeContext(encoded)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	_, _ = fmt.Fprintln(os.Stdout, runtimeContext.WorkspaceID)
	os.Exit(0)
}
