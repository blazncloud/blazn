//go:build linux

package microk8sissuer

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
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
