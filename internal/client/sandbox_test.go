package client

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func sandboxFixture(t *testing.T, name string) []byte {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	encoded, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "packages", "contracts", "testdata", "sandbox", name))
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestCanonicalSandboxTemplateDigestUsesOnlyJCSSpec(t *testing.T) {
	digestA, canonicalA, err := CanonicalSandboxTemplateDigest(sandboxFixture(t, "canonical-vector-a.json"))
	if err != nil {
		t.Fatal(err)
	}
	digestB, canonicalB, err := CanonicalSandboxTemplateDigest(sandboxFixture(t, "canonical-vector-b.json"))
	if err != nil {
		t.Fatal(err)
	}
	if digestA != digestB || string(canonicalA) != string(canonicalB) {
		t.Fatalf("equivalent specs differ: %s %s", digestA, digestB)
	}
	const want = "sha256:00109b83fb101f65f31aac1b6d8375fbb4d0ad1ab8a57571e35ec97285409f09"
	if digestA != want {
		t.Fatalf("canonical digest = %s, want %s; bytes=%s", digestA, want, canonicalA)
	}
}

func TestSandboxGrantTokenUsesOnlyAuthorizationHeader(t *testing.T) {
	const token = "top-secret-one-time-grant"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.String(), token) {
			t.Error("grant token leaked into URL")
		}
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), token) {
			t.Error("grant token leaked into body")
		}
		if got := r.Header.Get("Authorization"); got != "Blazn-Grant "+token {
			t.Errorf("authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"remoteExitCode":0,"stdoutBase64":"","stderrBase64":"","truncated":false}`)
	}))
	defer server.Close()
	client, err := New(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.ExecuteSandboxGrant(context.Background(), "11111111-1111-4111-8111-111111111111", token, SandboxExecRequest{Command: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.RemoteExitCode != 0 || result.Truncated {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestCreateSandboxRequiresNonSensitiveAcknowledgementBeforeNetwork(t *testing.T) {
	client, err := New("https://example.invalid", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.CreateSandbox(context.Background(), "access", "11111111-1111-4111-8111-111111111111", "request-1", CreateSandboxRequest{})
	if err == nil || !strings.Contains(err.Error(), SandboxIsolationNotice) {
		t.Fatalf("error = %v", err)
	}
}

func TestSandboxTransferPathsAreConfined(t *testing.T) {
	for _, value := range []string{"/workspace/src/repo/file.txt", "/workspace/artifacts/result.json", "/workspace/tmp/chunk"} {
		if !validSandboxTransferPath(value) {
			t.Errorf("valid path rejected: %s", value)
		}
	}
	for _, value := range []string{"/etc/passwd", "/workspace/src/../secret", "/workspace/src/repo/../../secret", "/workspace/src/", `/workspace/src/repo\secret`} {
		if validSandboxTransferPath(value) {
			t.Errorf("unsafe path accepted: %s", value)
		}
	}
}

func TestSandboxErrorStatusMap(t *testing.T) {
	for code, want := range map[string]int{"sandbox_not_found": 404, "access_grant_consumed": 410, "sandbox_backend_unavailable": 503} {
		got, ok := SandboxErrorHTTPStatus(code)
		if !ok || got != want {
			t.Fatalf("%s = %d,%v want %d", code, got, ok, want)
		}
	}
}
