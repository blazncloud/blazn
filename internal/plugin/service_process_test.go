package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
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
	code, err := (execProcessRunner{}).Run(context.Background(), executable, []string{"-test.run=TestRuntimeContextProcessHelper"}, "social", runtimeContext, Stdio{Stdout: &stdout, Stderr: &stdout})
	if err != nil || code != 0 || strings.TrimSpace(stdout.String()) != runtimeContext.WorkspaceID {
		t.Fatalf("code=%d output=%q err=%v", code, stdout.String(), err)
	}
}

func TestExecProcessRunnerProvidesScopedContentBroker(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("socketpair transport")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(BrokerFDEnvironment, "99")
	runtimeContext := validRuntimeContext(t)
	var stdout bytes.Buffer
	code, err := (execProcessRunner{}).Run(context.Background(), executable, []string{"-test.run=TestBrokerProcessHelper"}, "content", runtimeContext, Stdio{Stdout: &stdout, Stderr: &stdout})
	if err != nil || code != 0 || !strings.Contains(stdout.String(), `"transport":"inherited-socket"`) {
		t.Fatalf("code=%d output=%q err=%v", code, stdout.String(), err)
	}
}

func TestBrokerProcessHelper(t *testing.T) {
	if os.Getenv(BrokerFDEnvironment) != "3" {
		return
	}
	connection := os.NewFile(3, "broker")
	if connection == nil {
		os.Exit(4)
	}
	request := []byte(`{"schemaVersion":1,"requestId":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","method":"broker.describe","params":{}}`)
	if writeBrokerFrame(connection, brokerFrame{Type: brokerFrameRequest, StreamID: 2, Payload: request}) != nil {
		os.Exit(5)
	}
	frame, err := readBrokerFrame(connection)
	if err != nil {
		os.Exit(6)
	}
	var response brokerResponse
	if json.Unmarshal(frame.Payload, &response) != nil || !response.OK {
		os.Exit(7)
	}
	_, _ = fmt.Fprintln(os.Stdout, response.Payload)
	os.Exit(0)
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
