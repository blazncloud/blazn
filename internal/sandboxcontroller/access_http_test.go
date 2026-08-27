package sandboxcontroller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeAccessStore struct {
	binding        AccessGrantBinding
	consumed       bool
	id, hash, kind string
}

func (s *fakeAccessStore) ConsumeAccessGrant(_ context.Context, id, hash, kind string) (AccessGrantBinding, bool, error) {
	s.id, s.hash, s.kind = id, hash, kind
	return s.binding, s.consumed, nil
}

type fakeAccessExecutor struct {
	result    AccessCommandResult
	container string
	command   []string
	input     []byte
}

func (e *fakeAccessExecutor) Execute(_ context.Context, _ AccessGrantBinding, container string, command []string, input io.Reader) (AccessCommandResult, error) {
	e.container, e.command = container, append([]string(nil), command...)
	if input != nil {
		e.input, _ = io.ReadAll(input)
	}
	return e.result, nil
}

func TestAccessHandlerConsumesGrantAndPreservesExecOutput(t *testing.T) {
	token := strings.Repeat("g", 43)
	store := &fakeAccessStore{binding: AccessGrantBinding{SandboxID: "s"}, consumed: true}
	executor := &fakeAccessExecutor{result: AccessCommandResult{ExitCode: 23, Stdout: []byte("out"), Stderr: []byte("err")}}
	handler, _ := NewAccessHandler(store, executor)
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/sandbox-access-grants/11111111-1111-4111-8111-111111111111/exec", strings.NewReader(`{"command":["node","--version"]}`))
	request.Header.Set("Authorization", "Blazn-Grant "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || store.kind != "exec" || executor.container != "main" {
		t.Fatalf("status=%d store=%#v executor=%#v", response.Code, store, executor)
	}
	digest := sha256.Sum256([]byte(token))
	if store.hash != hex.EncodeToString(digest[:]) {
		t.Fatal("grant token hash mismatch")
	}
	var body struct {
		RemoteExitCode int    `json:"remoteExitCode"`
		StdoutBase64   string `json:"stdoutBase64"`
		StderrBase64   string `json:"stderrBase64"`
	}
	if json.Unmarshal(response.Body.Bytes(), &body) != nil || body.RemoteExitCode != 23 || body.StdoutBase64 != base64.StdEncoding.EncodeToString([]byte("out")) || body.StderrBase64 != base64.StdEncoding.EncodeToString([]byte("err")) {
		t.Fatalf("body=%s", response.Body)
	}
}

func TestAccessHandlerValidatesBeforeConsumingAndBindsUploadHelper(t *testing.T) {
	store := &fakeAccessStore{binding: AccessGrantBinding{SandboxID: "s"}, consumed: true}
	executor := &fakeAccessExecutor{}
	handler, _ := NewAccessHandler(store, executor)
	bad := httptest.NewRequest(http.MethodPost, "/internal/v1/sandbox-access-grants/11111111-1111-4111-8111-111111111111/exec", strings.NewReader(`{"command":[]}`))
	bad.Header.Set("Authorization", "Blazn-Grant "+strings.Repeat("g", 43))
	reply := httptest.NewRecorder()
	handler.ServeHTTP(reply, bad)
	if reply.Code != http.StatusBadRequest || store.kind != "" {
		t.Fatalf("status=%d consumed=%q", reply.Code, store.kind)
	}
	body := []byte("fixture")
	digest := sha256.Sum256(body)
	upload := httptest.NewRequest(http.MethodPut, "/internal/v1/sandbox-access-grants/11111111-1111-4111-8111-111111111111/file", bytes.NewReader(body))
	upload.Header.Set("Authorization", "Blazn-Grant "+strings.Repeat("h", 43))
	upload.Header.Set("X-Blazn-Sandbox-Path", "/workspace/src/blazn/fixture.txt")
	upload.Header.Set("X-Content-Size", "7")
	upload.Header.Set("X-Content-SHA256", "sha256:"+hex.EncodeToString(digest[:]))
	reply = httptest.NewRecorder()
	handler.ServeHTTP(reply, upload)
	if reply.Code != http.StatusOK || store.kind != "upload" || executor.container != "sandbox-access-io" || !bytes.Equal(executor.input, body) {
		t.Fatalf("status=%d store=%#v executor=%#v", reply.Code, store, executor)
	}
}
