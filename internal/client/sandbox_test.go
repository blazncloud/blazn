package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

func TestCanonicalSandboxArtifactContractDigestIsOrderedAndPinned(t *testing.T) {
	entries := []SandboxArtifactContractEntry{{Name: "z-last", Path: "/workspace/artifacts/z", MediaType: "text/plain", Required: false}, {Name: "patch", Path: "/workspace/artifacts/change.patch", MediaType: "text/plain", Required: true}}
	oneDigest, oneBytes, err := CanonicalSandboxArtifactContractDigest(entries[1:])
	if err != nil {
		t.Fatal(err)
	}
	if oneDigest != "sha256:d139b2eb8bb329f61b85f95b4983c028fbbadcfd36fd73cdbb05d143a4ac0729" {
		t.Fatalf("artifact digest=%s bytes=%s", oneDigest, oneBytes)
	}
	a, bytesA, err := CanonicalSandboxArtifactContractDigest(entries)
	if err != nil {
		t.Fatal(err)
	}
	b, bytesB, err := CanonicalSandboxArtifactContractDigest([]SandboxArtifactContractEntry{entries[1], entries[0]})
	if err != nil {
		t.Fatal(err)
	}
	if a != b || string(bytesA) != string(bytesB) {
		t.Fatal("artifact contract digest depends on input order")
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
	for _, value := range []string{"/etc/passwd", "/workspace/src/./escape", "/workspace/src/../secret", "/workspace/src/repo/../../secret", "/workspace/src/", `/workspace/src/repo\secret`} {
		if validSandboxTransferPath(value) {
			t.Errorf("unsafe path accepted: %s", value)
		}
	}
}

func TestChunkedSandboxDownloadVerifiesDeclaredSizeAndDigest(t *testing.T) {
	content := []byte("chunked-content")
	sum := sha256.Sum256(content)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Content-Size", "15")
		w.Header().Set("X-Content-SHA256", "sha256:"+hex.EncodeToString(sum[:]))
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		_, _ = w.Write(content)
	}))
	defer server.Close()
	client, err := New(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	body, size, digest, err := client.DownloadSandboxGrantFile(context.Background(), "11111111-1111-4111-8111-111111111111", "grant", "/workspace/src/repo/file")
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) || size != int64(len(content)) || digest != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatalf("download mismatch: %q %d %s", got, size, digest)
	}
}

func TestSandboxDownloadRejectsDotSegmentHeadersBeforeNetwork(t *testing.T) {
	for _, value := range []string{"/workspace/src/./escape", "/workspace/src/../escape"} {
		if validSandboxTransferPath(value) {
			t.Errorf("dot segment accepted: %s", value)
		}
	}
}

func TestCreateAccessGrantNeverSendsIdempotencyKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Idempotency-Key"); got != "" {
			t.Errorf("unexpected Idempotency-Key %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"grant":{"id":"11111111-1111-4111-8111-111111111111","sandboxId":"22222222-2222-4222-8222-222222222222","workspaceId":"33333333-3333-4333-8333-333333333333","scope":"sandbox.exec","kind":"exec","state":"active","expiresAt":"2026-08-22T00:00:30Z","createdAt":"2026-08-22T00:00:00Z"},"accessToken":"abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ","endpoint":"https://example.invalid/v1/sandbox-access-grants/11111111-1111-4111-8111-111111111111/exec"}`)
	}))
	defer server.Close()
	client, err := New(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.CreateSandboxAccessGrant(context.Background(), "access", "22222222-2222-4222-8222-222222222222", CreateSandboxAccessGrantRequest{Kind: SandboxGrantExec, ExpiresInSeconds: 30}); err != nil {
		t.Fatal(err)
	}
}

func TestSandboxEventStreamDecodesTypedGlobalCursor(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Last-Event-ID"); got != "prior-event" {
			t.Errorf("Last-Event-ID = %q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "id: 44444444-4444-4444-8444-444444444444\ndata: {\"eventId\":\"44444444-4444-4444-8444-444444444444\",\"sandboxId\":\"22222222-2222-4222-8222-222222222222\",\"operationId\":null,\"sequence\":7,\"type\":\"sandbox.ready\",\"payload\":{},\"createdAt\":\"2026-08-22T00:00:00Z\"}\n\n")
	}))
	defer server.Close()
	client, err := New(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	stream, err := client.StreamSandboxEvents(context.Background(), "access", "22222222-2222-4222-8222-222222222222", "prior-event")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	event, err := stream.Next()
	if err != nil {
		t.Fatal(err)
	}
	if event.Sequence != 7 || event.Type != "sandbox.ready" {
		t.Fatalf("unexpected event: %+v", event)
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
