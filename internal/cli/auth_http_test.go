package cli

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KingJammin/blazn/internal/auth"
	"github.com/KingJammin/blazn/internal/client"
)

type e2eStore struct{ value []byte }

func (s *e2eStore) Get() ([]byte, error) {
	if len(s.value) == 0 {
		return nil, auth.ErrNotFound
	}
	return append([]byte(nil), s.value...), nil
}
func (s *e2eStore) Put(value []byte) error { s.value = append([]byte(nil), value...); return nil }
func (s *e2eStore) Delete() error          { s.value = nil; return nil }
func (s *e2eStore) Description() string    { return "integration keyring" }

func TestAuthCLIEndToEndAgainstManagementAPIContract(t *testing.T) {
	var publicKey ed25519.PublicKey
	var revoked string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/auth/device/authorizations":
			var request client.DeviceAuthorizationRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			decoded, err := base64.RawStdEncoding.DecodeString(request.DevicePublicKey)
			if err != nil || len(decoded) != ed25519.PublicKeySize || request.DeviceName == "" || request.Platform == "" {
				t.Fatalf("authorization request=%#v key-size=%d err=%v", request, len(decoded), err)
			}
			publicKey = decoded
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"deviceCode":"device-secret","userCode":"ABCD-EFGH","verificationUri":"https://login.example/device","challenge":"challenge-1","expiresIn":60,"interval":1}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/auth/device/sessions":
			var request client.DeviceSessionRequest
			_ = json.NewDecoder(r.Body).Decode(&request)
			proof, err := base64.RawStdEncoding.DecodeString(request.Proof)
			if err != nil || !ed25519.Verify(publicKey, []byte("blazn-device-session-v1\ndevice-secret\nchallenge-1"), proof) {
				t.Fatal("device proof did not verify end to end")
			}
			_, _ = w.Write([]byte(`{"accessToken":"access-secret","refreshToken":"refresh-secret","expiresIn":300,"deviceId":"dev-1"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/auth/me":
			if r.Header.Get("Authorization") != "Bearer access-secret" {
				t.Fatalf("authorization header = %q", r.Header.Get("Authorization"))
			}
			_, _ = w.Write([]byte(`{"user":{"id":"user-1","displayName":"Blaze","status":"active"},"device":{"id":"dev-1","name":"test-machine","platform":"linux/amd64","status":"active","createdAt":"2026-08-21T00:00:00Z"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/auth/devices":
			_, _ = w.Write([]byte(`{"items":[{"id":"dev-1","name":"test-machine","platform":"linux/amd64","status":"active","createdAt":"2026-08-21T00:00:00Z"}]}`))
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/auth/devices/"):
			revoked = strings.TrimPrefix(r.URL.Path, "/v1/auth/devices/")
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	api, err := client.New(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	service := auth.NewService(api, &e2eStore{})
	app, stdout, stderr := authApp(&fakeAuthCommands{})
	app.auth = func() (authCommands, error) { return service, nil }

	if code := app.Run([]string{"auth", "login", "--no-browser"}); code != ExitSuccess || !strings.Contains(stdout.String(), "ABCD-EFGH") || stderr.Len() != 0 {
		t.Fatalf("login code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	if code := app.Run([]string{"auth", "status", "--output=json"}); code != ExitSuccess || !strings.Contains(stdout.String(), `"authenticated":true`) {
		t.Fatalf("status code=%d stdout=%q", code, stdout.String())
	}
	stdout.Reset()
	if code := app.Run([]string{"auth", "devices", "--output=json"}); code != ExitSuccess || !strings.Contains(stdout.String(), `"id":"dev-1"`) {
		t.Fatalf("devices code=%d stdout=%q", code, stdout.String())
	}
	stdout.Reset()
	if code := app.Run([]string{"auth", "revoke-device", "dev-1", "--output=json"}); code != ExitSuccess || revoked != "dev-1" {
		t.Fatalf("revoke code=%d revoked=%q stdout=%q", code, revoked, stdout.String())
	}
	if _, err := service.Status(context.Background()); err != nil {
		t.Fatalf("post-revocation status: %v", err)
	}
}
