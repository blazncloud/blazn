package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDeviceAuthorizationAndSessionContract(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/auth/device/authorizations":
			var request DeviceAuthorizationRequest
			_ = json.NewDecoder(r.Body).Decode(&request)
			if request.DevicePublicKey != "public" {
				t.Fatalf("public key = %q", request.DevicePublicKey)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"deviceCode":"secret","userCode":"ABCD-EFGH","verificationUri":"https://login.example/device","challenge":"challenge","expiresIn":600,"interval":1}`))
		case "/v1/auth/device/sessions":
			var request DeviceSessionRequest
			_ = json.NewDecoder(r.Body).Decode(&request)
			if request.DeviceCode != "secret" || request.Proof != "proof" {
				t.Fatalf("session request = %#v", request)
			}
			_, _ = w.Write([]byte(`{"accessToken":"access","refreshToken":"refresh","expiresIn":300,"deviceId":"dev-1"}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	api, err := New(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	authorization, err := api.CreateDeviceAuthorization(context.Background(), DeviceAuthorizationRequest{DevicePublicKey: "public", DeviceName: "test", Platform: "linux/amd64"})
	if err != nil || authorization.UserCode != "ABCD-EFGH" {
		t.Fatalf("authorization=%#v err=%v", authorization, err)
	}
	session, err := api.ExchangeDeviceAuthorization(context.Background(), DeviceSessionRequest{DeviceCode: authorization.DeviceCode, Proof: "proof"})
	if err != nil || session.RefreshToken != "refresh" || calls != 2 {
		t.Fatalf("session=%#v calls=%d err=%v", session, calls, err)
	}
}

func TestErrorContractAndBearerHeader(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer token" {
			t.Fatalf("authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":"session_revoked","message":"device was revoked","requestId":"req-1"}`))
	}))
	defer server.Close()
	api, _ := New(server.URL, server.Client())
	_, err := api.GetCurrentUser(context.Background(), "token")
	if !IsCode(err, "session_revoked") {
		t.Fatalf("error = %T %v", err, err)
	}
}

func TestRejectsUnsafeBaseURL(t *testing.T) {
	for _, value := range []string{"file:///tmp/api", "https://user:pass@example.com", "https://example.com?token=x"} {
		if _, err := New(value, nil); err == nil {
			t.Fatalf("New(%q) succeeded", value)
		}
	}
}
