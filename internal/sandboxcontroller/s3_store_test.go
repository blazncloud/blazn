package sandboxcontroller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestS3ArtifactStoreConditionallyCreatesAndVerifiesExactObject(t *testing.T) {
	directory := t.TempDir()
	accessPath, secretPath := filepath.Join(directory, "access"), filepath.Join(directory, "secret")
	if err := os.WriteFile(accessPath, []byte("ACCESSKEY123456\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secretPath, []byte("secret-key-material-1234567890\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	body := []byte("artifact body\n")
	digest := sha256.Sum256(body)
	spec := ArtifactObjectSpec{WorkspaceID: "40000000-0000-4000-8000-000000000001", SandboxID: "30000000-0000-4000-8000-000000000001",
		Name: "result", MediaType: "text/plain", Digest: "sha256:" + hex.EncodeToString(digest[:]), Size: int64(len(body))}
	spec.Key, _ = ArtifactObjectKey(spec.WorkspaceID, spec.SandboxID, spec.Name)
	var mu sync.Mutex
	created := false
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if request.URL.EscapedPath() != "/artifacts/"+spec.Key || request.URL.RawQuery != "" ||
			!strings.HasPrefix(request.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=ACCESSKEY123456/") ||
			request.Header.Get("X-Amz-Content-Sha256") == "" || request.Header.Get("X-Amz-Date") != "20260824T120000Z" {
			t.Errorf("unsafe signed request: method=%s path=%s", request.Method, request.URL.EscapedPath())
		}
		switch request.Method {
		case http.MethodPut:
			contents, err := io.ReadAll(request.Body)
			if err != nil || !bytes.Equal(contents, body) || request.Header.Get("If-None-Match") != "*" ||
				request.Header.Get("Content-Type") != spec.MediaType || request.Header.Get("X-Amz-Meta-Blazn-Digest") != spec.Digest {
				t.Errorf("PUT boundary changed: body=%q err=%v", contents, err)
			}
			if created {
				response.WriteHeader(http.StatusPreconditionFailed)
				return
			}
			created = true
			response.WriteHeader(http.StatusCreated)
		case http.MethodHead:
			if !created {
				response.WriteHeader(http.StatusNotFound)
				return
			}
			response.Header().Set("Content-Length", "14")
			response.Header().Set("Content-Type", spec.MediaType)
			response.Header().Set("X-Amz-Meta-Blazn-Digest", spec.Digest)
			response.Header().Set("X-Amz-Meta-Blazn-Workspace", spec.WorkspaceID)
			response.Header().Set("X-Amz-Meta-Blazn-Sandbox", spec.SandboxID)
			response.Header().Set("X-Amz-Meta-Blazn-Artifact", spec.Name)
		case http.MethodGet:
			if !created {
				response.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = response.Write(body)
		default:
			response.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()
	endpoint, _ := url.Parse(server.URL)
	client := server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	store := &S3ArtifactStore{endpoint: endpoint, region: "us-test-1", bucket: "artifacts", accessKeyFile: accessPath,
		secretKeyFile: secretPath, client: client, now: func() time.Time { return time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC) }}
	if value, err := store.Put(context.Background(), spec, body); err != nil || !value {
		t.Fatalf("first PUT created=%v err=%v", value, err)
	}
	if value, err := store.Put(context.Background(), spec, body); err != nil || value {
		t.Fatalf("replay PUT created=%v err=%v", value, err)
	}
	contents, found, err := store.Get(context.Background(), spec)
	if err != nil || !found || !bytes.Equal(contents, body) {
		t.Fatalf("GET=%q found=%v err=%v", contents, found, err)
	}
	head, found, err := store.Head(context.Background(), spec)
	if err != nil || !found || head.Size != spec.Size || head.Digest != spec.Digest || head.MediaType != spec.MediaType ||
		head.WorkspaceID != spec.WorkspaceID || head.SandboxID != spec.SandboxID || head.Name != spec.Name {
		t.Fatalf("HEAD=%#v found=%v err=%v", head, found, err)
	}
}

func TestS3ArtifactStoreRejectsUnsafeIdentityCredentialsAndEndpoints(t *testing.T) {
	if _, err := ArtifactObjectKey("not-a-uuid", "30000000-0000-4000-8000-000000000001", "result"); err == nil {
		t.Fatal("unsafe object identity accepted")
	}
	directory := t.TempDir()
	access, secret := filepath.Join(directory, "access"), filepath.Join(directory, "secret")
	if err := os.WriteFile(access, []byte("ACCESSKEY123456"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, []byte("secret-key-material-1234567890"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readS3Credentials(access, secret); err == nil {
		t.Fatal("world-readable object access key accepted")
	}
	if _, err := NewS3ArtifactStore(S3ArtifactStoreConfig{Endpoint: "http://s3.example.test:443", Region: "us-test-1", Bucket: "artifacts",
		AccessKeyFile: access, SecretKeyFile: secret}); err == nil {
		t.Fatal("insecure object endpoint accepted")
	}
}

func TestS3ArtifactStoreGetFailsClosedOnInvalidResponses(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		length int64
		body   []byte
	}{
		{name: "truncated", status: http.StatusOK, length: 7, body: []byte("short")},
		{name: "declared oversized", status: http.StatusOK, length: maxArtifactBytes + 1},
		{name: "streamed oversized", status: http.StatusOK, length: -1, body: make([]byte, maxArtifactBytes+1)},
		{name: "store error", status: http.StatusServiceUnavailable, length: -1, body: []byte("unavailable")},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			accessPath, secretPath := filepath.Join(directory, "access"), filepath.Join(directory, "secret")
			if err := os.WriteFile(accessPath, []byte("ACCESSKEY123456\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(secretPath, []byte("secret-key-material-1234567890\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodGet {
					t.Errorf("method=%s", request.Method)
				}
				if test.length >= 0 {
					response.Header().Set("Content-Length", fmt.Sprint(test.length))
				}
				response.WriteHeader(test.status)
				_, _ = response.Write(test.body)
			}))
			defer server.Close()
			endpoint, _ := url.Parse(server.URL)
			store := &S3ArtifactStore{endpoint: endpoint, region: "us-test-1", bucket: "artifacts", accessKeyFile: accessPath,
				secretKeyFile: secretPath, client: server.Client(), now: time.Now}
			body := []byte("artifact body\n")
			digest := sha256.Sum256(body)
			spec := ArtifactObjectSpec{WorkspaceID: "40000000-0000-4000-8000-000000000001", SandboxID: "30000000-0000-4000-8000-000000000001",
				Name: "result", MediaType: "text/plain", Digest: "sha256:" + hex.EncodeToString(digest[:]), Size: int64(len(body))}
			spec.Key, _ = ArtifactObjectKey(spec.WorkspaceID, spec.SandboxID, spec.Name)
			if contents, found, err := store.Get(context.Background(), spec); err == nil || found || contents != nil {
				t.Fatalf("GET=%q found=%v err=%v", contents, found, err)
			}
		})
	}
}
