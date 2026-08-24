package sandboxcontroller

import (
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestKubernetesClientPinsCAAndReadsRotatingProjectedTokenPerRequest(t *testing.T) {
	var mutex sync.Mutex
	var tokens []string
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		mutex.Lock()
		tokens = append(tokens, request.Header.Get("Authorization"))
		mutex.Unlock()
		if request.URL.Path != "/apis/agents.x-k8s.io/v1beta1/namespaces/blazn-poc-sandboxes/sandboxes" || request.URL.Query().Get("limit") != "1" {
			t.Errorf("unexpected health request: %s", request.URL.String())
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"items":[]}`))
	}))
	server.StartTLS()
	defer server.Close()
	config, rotate := tlsKubernetesFixture(t, server)
	client, err := newKubernetesHTTPClient(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := kubernetesAPIHealth(context.Background(), client, config.BaseURL); err != nil {
		t.Fatal(err)
	}
	rotate("second-projected-token")
	if err := kubernetesAPIHealth(context.Background(), client, config.BaseURL); err != nil {
		t.Fatal(err)
	}
	mutex.Lock()
	defer mutex.Unlock()
	if fmt.Sprint(tokens) != "[Bearer first-projected-token Bearer second-projected-token]" {
		t.Fatalf("token was cached or exposed incorrectly: %q", tokens)
	}
}

func TestKubernetesClientRejectsWrongCAWithoutLeakingToken(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	other := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer other.Close()
	config, _ := tlsKubernetesFixture(t, other)
	parsed, _ := url.Parse(server.URL)
	config.BaseURL = "https://" + parsed.Host
	client, err := newKubernetesHTTPClient(config)
	if err != nil {
		t.Fatal(err)
	}
	err = kubernetesAPIHealth(context.Background(), client, config.BaseURL)
	if err == nil || strings.Contains(err.Error(), "first-projected-token") {
		t.Fatalf("wrong CA failure leaked token or succeeded: %v", err)
	}
}

func TestKubernetesClientDisablesProxyAndRedirects(t *testing.T) {
	redirected := 0
	target := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		redirected++
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"items":[]}`))
	}))
	defer target.Close()
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, target.URL, http.StatusTemporaryRedirect)
	}))
	server.StartTLS()
	defer server.Close()
	config, _ := tlsKubernetesFixture(t, server)
	client, err := newKubernetesHTTPClient(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := kubernetesAPIHealth(context.Background(), client, config.BaseURL); err == nil || redirected != 0 {
		t.Fatalf("redirect followed or accepted: redirected=%d err=%v", redirected, err)
	}
	bounded := client.Transport.(*boundedKubernetesTransport)
	authenticated := bounded.base.(*bearerTokenTransport)
	if authenticated.base.(*http.Transport).Proxy != nil {
		t.Fatal("environment proxy was enabled")
	}
}

func TestKubernetesClientBoundsHeadersBodiesContentTypeAndStalls(t *testing.T) {
	for _, test := range []struct {
		name    string
		handler http.HandlerFunc
	}{
		{name: "content type", handler: func(response http.ResponseWriter, _ *http.Request) {
			response.Header().Set("Content-Type", "text/plain")
			_, _ = response.Write([]byte(`{"items":[]}`))
		}},
		{name: "declared body", handler: func(response http.ResponseWriter, _ *http.Request) {
			response.Header().Set("Content-Type", "application/json")
			response.Header().Set("Content-Length", fmt.Sprint(maxKubernetesResponseBytes+1))
			response.WriteHeader(http.StatusOK)
		}},
		{name: "streamed body", handler: func(response http.ResponseWriter, _ *http.Request) {
			response.Header().Set("Content-Type", "application/json")
			response.WriteHeader(http.StatusOK)
			_, _ = response.Write([]byte(`{"items":[]}` + strings.Repeat(" ", maxKubernetesResponseBytes)))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewUnstartedServer(test.handler)
			server.StartTLS()
			defer server.Close()
			config, _ := tlsKubernetesFixture(t, server)
			client, err := newKubernetesHTTPClient(config)
			if err != nil {
				t.Fatal(err)
			}
			if err := kubernetesAPIHealth(context.Background(), client, config.BaseURL); err == nil {
				t.Fatal("invalid response was accepted")
			}
		})
	}
	t.Run("stall cancellation", func(t *testing.T) {
		started := make(chan struct{})
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			close(started)
			<-request.Context().Done()
		}))
		server.StartTLS()
		defer server.Close()
		config, _ := tlsKubernetesFixture(t, server)
		client, err := newKubernetesHTTPClient(config)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		if err := kubernetesAPIHealth(ctx, client, config.BaseURL); err == nil || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("stalled request was not canceled: %v", err)
		}
		<-started
	})
}

func TestKubernetesCredentialFilesRejectUnsafeCAAndTokenProjection(t *testing.T) {
	directory := t.TempDir()
	ca := filepath.Join(directory, "ca.crt")
	if err := os.WriteFile(ca, []byte("not-a-ca"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "ca-link.crt")
	if err := os.Symlink(ca, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readKubernetesCA(link); err == nil {
		t.Fatal("symlinked CA was accepted")
	}
	token := filepath.Join(directory, "token")
	secret := "do-not-leak-projected-token"
	if err := os.WriteFile(token, []byte(secret), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(token, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := readProjectedServiceAccountToken(token); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("unsafe token accepted or leaked: %v", err)
	}
	escaped := filepath.Join(t.TempDir(), "escaped-token")
	if err := os.WriteFile(escaped, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(token); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(escaped, token); err != nil {
		t.Fatal(err)
	}
	if _, err := readProjectedServiceAccountToken(token); err == nil {
		t.Fatal("projection symlink escaping its volume was accepted")
	}
	unsafeVolume := filepath.Join(t.TempDir(), "unsafe-volume")
	if err := os.Mkdir(unsafeVolume, 0o700); err != nil {
		t.Fatal(err)
	}
	unsafeToken := filepath.Join(unsafeVolume, "token")
	if err := os.WriteFile(unsafeToken, []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unsafeVolume, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := readProjectedServiceAccountToken(unsafeToken); err == nil {
		t.Fatal("token in a writable projection directory was accepted")
	}
}

func TestKubernetesCARequiresStrictConcatenatedCertificateBlocks(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	validBundle := append(append(append([]byte{}, certificate...), []byte(" \n\t\r\n")...), certificate...)
	validPath := filepath.Join(t.TempDir(), "valid-ca.crt")
	if err := os.WriteFile(validPath, validBundle, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readKubernetesCA(validPath); err != nil {
		t.Fatalf("valid concatenated CA certificates rejected: %v", err)
	}

	for _, test := range []struct {
		name     string
		contents []byte
	}{
		{name: "leading garbage", contents: append([]byte("not-a-certificate\n"), certificate...)},
		{name: "inter-block garbage", contents: append(append(append([]byte{}, certificate...), []byte("not-whitespace\n")...), certificate...)},
		{name: "trailing garbage", contents: append(append([]byte{}, certificate...), []byte("not-whitespace\n")...)},
		{name: "duplicate begin marker", contents: append([]byte(kubernetesCertificatePEM+"\n"), certificate...)},
		{name: "duplicate trailing begin marker", contents: append(append([]byte{}, certificate...), []byte(kubernetesCertificatePEM+"\n")...)},
		{name: "non-certificate block", contents: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("not-a-key")})},
		{name: "malformed certificate block", contents: []byte(kubernetesCertificatePEM + "\n%%%\n-----END CERTIFICATE-----\n")},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ca.crt")
			if err := os.WriteFile(path, test.contents, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := readKubernetesCA(path); err == nil {
				t.Fatal("malformed CA bundle was accepted")
			}
		})
	}
}

func TestProjectedTokenAcceptsSafeKubernetesDataProjection(t *testing.T) {
	directory := t.TempDir()
	version := filepath.Join(directory, "..2026_08_23_00_00_00")
	if err := os.Mkdir(version, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(version, "token"), []byte("safe-projected-token\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Base(version), filepath.Join(directory, "..data")); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(directory, "token")
	if err := os.Symlink("..data/token", token); err != nil {
		t.Fatal(err)
	}
	value, err := readProjectedServiceAccountToken(token)
	if err != nil || value != "safe-projected-token" {
		t.Fatalf("safe Kubernetes projection rejected: value=%q err=%v", value, err)
	}
}

func TestProjectedTokenRejectsWritableIntermediateDirectory(t *testing.T) {
	for _, mode := range []os.FileMode{0o770, 0o777} {
		t.Run(fmt.Sprintf("mode-%#o", mode), func(t *testing.T) {
			directory := t.TempDir()
			intermediate := filepath.Join(directory, "nested")
			version := filepath.Join(intermediate, "version")
			if err := os.MkdirAll(version, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(version, "token"), []byte("unsafe-token"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(intermediate, mode); err != nil {
				t.Fatal(err)
			}
			token := filepath.Join(directory, "token")
			if err := os.Symlink("nested/version/token", token); err != nil {
				t.Fatal(err)
			}
			if _, err := readProjectedServiceAccountToken(token); err == nil {
				t.Fatal("token beneath a writable intermediate directory was accepted")
			}
		})
	}
}

func TestProjectedTokenRejectsIntermediateDirectoryChangeDuringRead(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(string) error
	}{
		{name: "mode", mutate: func(path string) error { return os.Chmod(path, 0o770) }},
		{name: "symlink swap", mutate: func(path string) error {
			original := path + ".original"
			if err := os.Rename(path, original); err != nil {
				return err
			}
			return os.Symlink(filepath.Base(original), path)
		}},
		{name: "inode replacement", mutate: func(path string) error {
			original := path + ".original"
			if err := os.Rename(path, original); err != nil {
				return err
			}
			if err := os.Mkdir(path, 0o700); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(path, "token"), []byte("replacement-token"), 0o600)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			intermediate := filepath.Join(directory, "nested")
			if err := os.Mkdir(intermediate, 0o700); err != nil {
				t.Fatal(err)
			}
			resolved := filepath.Join(intermediate, "token")
			if err := os.WriteFile(resolved, []byte("original-token"), 0o600); err != nil {
				t.Fatal(err)
			}
			token := filepath.Join(directory, "token")
			if err := os.Symlink("nested/token", token); err != nil {
				t.Fatal(err)
			}
			ops := kubernetesFileOps{lstat: os.Lstat, evalSymlinks: filepath.EvalSymlinks,
				open: func(name string) (*os.File, error) {
					return os.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
				},
				afterRead: func() error { return test.mutate(intermediate) }}
			if _, err := readProjectedServiceAccountTokenWithOps(token, ops); err == nil {
				t.Fatal("intermediate directory mutation during read was accepted")
			}
		})
	}
}

func TestProjectedTokenRejectsInodeOrModeChangeDuringRead(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(string) error
	}{
		{name: "mode", mutate: func(path string) error { return os.Chmod(path, 0o666) }},
		{name: "inode", mutate: func(path string) error {
			replacement := path + ".new"
			if err := os.WriteFile(replacement, []byte("replacement"), 0o600); err != nil {
				return err
			}
			return os.Rename(replacement, path)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			token := filepath.Join(directory, "token")
			if err := os.WriteFile(token, []byte("original"), 0o600); err != nil {
				t.Fatal(err)
			}
			ops := kubernetesFileOps{lstat: os.Lstat, evalSymlinks: filepath.EvalSymlinks,
				open: func(name string) (*os.File, error) {
					return os.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
				},
				afterRead: func() error { return test.mutate(token) }}
			if _, err := readProjectedServiceAccountTokenWithOps(token, ops); err == nil {
				t.Fatal("credential mutation during read was accepted")
			}
		})
	}
}

func tlsKubernetesFixture(t *testing.T, server *httptest.Server) (KubernetesConfig, func(string)) {
	t.Helper()
	directory := t.TempDir()
	caFile := filepath.Join(directory, "kubernetes-ca.crt")
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(caFile, certificate, 0o600); err != nil {
		t.Fatal(err)
	}
	version := 1
	writeVersion := func(token string) string {
		name := fmt.Sprintf("..2026_%d", version)
		version++
		destination := filepath.Join(directory, name)
		if err := os.Mkdir(destination, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(destination, "token"), []byte(token+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return name
	}
	first := writeVersion("first-projected-token")
	if err := os.Symlink(first, filepath.Join(directory, "..data")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("..data/token", filepath.Join(directory, "token")); err != nil {
		t.Fatal(err)
	}
	rotate := func(token string) {
		name := writeVersion(token)
		next := filepath.Join(directory, "..data.next")
		if err := os.Symlink(name, next); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(next, filepath.Join(directory, "..data")); err != nil {
			t.Fatal(err)
		}
	}
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return KubernetesConfig{BaseURL: "https://" + parsed.Host, CAFile: caFile, TokenFile: filepath.Join(directory, "token"), HelperImage: testSandboxIOImage}, rotate
}
