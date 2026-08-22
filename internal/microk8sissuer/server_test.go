//go:build linux

package microk8sissuer

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestServerDeniesMissingPeerCredential(t *testing.T) {
	server := &Server{AllowedUID: 1000, AllowedGID: 1000, Timeout: time.Second}
	request := httptest.NewRequest(http.MethodPost, "http://unix/v1/worker-credentials", nil)
	response := httptest.NewRecorder()
	server.handle(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status %d", response.Code)
	}
}

func TestSocketPathRecoversOnlyOwnedStaleSocket(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0750); err != nil {
		t.Fatal(err)
	}
	server := &Server{SocketUID: uint32(os.Getuid()), AllowedGID: uint32(os.Getgid())}
	path := filepath.Join(root, "issuer.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	listener.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := os.Chmod(path, 0660); err != nil {
		t.Fatal(err)
	}
	listener.Close()
	if err := server.prepareSocketPath(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("stale socket was not removed")
	}
	if err := os.WriteFile(path, []byte("not a socket"), 0660); err != nil {
		t.Fatal(err)
	}
	if err := server.prepareSocketPath(path); err == nil {
		t.Fatal("regular file accepted")
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatal("unsafe path was removed")
	}
}

func TestSocketPathRefusesActiveSocketAndSymlinkParent(t *testing.T) {
	root := t.TempDir()
	_ = os.Chmod(root, 0750)
	server := &Server{SocketUID: uint32(os.Getuid()), AllowedGID: uint32(os.Getgid())}
	path := filepath.Join(root, "issuer.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_ = os.Chmod(path, 0660)
	if err := server.prepareSocketPath(path); err == nil {
		t.Fatal("active socket accepted")
	}
	real := filepath.Join(t.TempDir(), "real")
	if err := os.Mkdir(real, 0750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := server.prepareSocketPath(filepath.Join(link, "issuer.sock")); err == nil {
		t.Fatal("symlink parent accepted")
	}
}

func TestServerAllowsExactPeerAndClosedRequest(t *testing.T) {
	now := time.Now().UTC()
	backend := &fakeBackend{now: now}
	service, _ := NewService(secureTempDir(t), []byte("0123456789abcdef0123456789abcdef"), backend)
	service.now = func() time.Time { return now }
	server := &Server{Service: service, AllowedUID: uint32(os.Getuid()), AllowedGID: uint32(os.Getgid()), Timeout: time.Second}
	payload, _ := json.Marshal(requestFixture())
	request := httptest.NewRequest(http.MethodPost, "http://unix/v1/worker-credentials", bytes.NewReader(payload))
	request.Header.Set("content-type", "application/json")
	request = request.WithContext(context.WithValue(request.Context(), peerKey{}, Peer{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}))
	response := httptest.NewRecorder()
	server.handle(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}
}
