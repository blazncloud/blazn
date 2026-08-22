//go:build linux

package microk8sissuer

import (
	"net/http"
	"net/http/httptest"
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
