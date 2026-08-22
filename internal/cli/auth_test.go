package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/blazncloud/blazn/internal/auth"
	"github.com/blazncloud/blazn/internal/client"
)

type fakeAuthCommands struct {
	start      auth.LoginStart
	login      auth.LoginResult
	status     auth.StatusResult
	logout     auth.LogoutResult
	devices    []client.Device
	err        error
	revoked    string
	deviceCode string
}

func (f *fakeAuthCommands) BeginLogin(context.Context) (auth.LoginStart, string, time.Duration, error) {
	return f.start, "device-secret", time.Second, f.err
}
func (f *fakeAuthCommands) CompleteLogin(_ context.Context, code string, _ time.Duration) (auth.LoginResult, error) {
	f.deviceCode = code
	return f.login, f.err
}
func (f *fakeAuthCommands) Status(context.Context) (auth.StatusResult, error) {
	return f.status, f.err
}
func (f *fakeAuthCommands) Logout(context.Context) (auth.LogoutResult, error) {
	return f.logout, f.err
}
func (f *fakeAuthCommands) Devices(context.Context) ([]client.Device, error) {
	return f.devices, f.err
}
func (f *fakeAuthCommands) RevokeDevice(_ context.Context, id string) error {
	f.revoked = id
	return f.err
}

func authApp(fake *fakeAuthCommands) (*App, *bytes.Buffer, *bytes.Buffer) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(stdout, stderr, testBuild)
	app.auth = func() (authCommands, error) { return fake, nil }
	app.openBrowser = func(string) error { return nil }
	return app, stdout, stderr
}

func TestAuthLoginHeadlessHumanShowsCodeAndStoresSession(t *testing.T) {
	fake := &fakeAuthCommands{
		start: auth.LoginStart{UserCode: "ABCD-EFGH", VerificationURI: "https://login.example/device", ExpiresIn: 600},
		login: auth.LoginResult{Status: "authenticated", DeviceID: "dev-1", User: client.User{DisplayName: "Blaze"}},
	}
	app, stdout, stderr := authApp(fake)
	if code := app.Run([]string{"auth", "login", "--no-browser"}); code != ExitSuccess {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "ABCD-EFGH") || !strings.Contains(stdout.String(), "Authenticated as Blaze") || fake.deviceCode != "device-secret" {
		t.Fatalf("stdout=%q fake=%#v", stdout.String(), fake)
	}
}

func TestAuthStatusJSONIsStableAndRedacted(t *testing.T) {
	fake := &fakeAuthCommands{status: auth.StatusResult{Authenticated: true, Store: "test", User: &client.User{ID: "u1", DisplayName: "Blaze"}, Device: &client.Device{ID: "d1", Name: "laptop"}}}
	app, stdout, stderr := authApp(fake)
	if code := app.Run([]string{"--output=json", "auth", "status"}); code != ExitSuccess || stderr.Len() != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	var output map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil || output["authenticated"] != true || strings.Contains(stdout.String(), "Token") {
		t.Fatalf("output=%q err=%v", stdout.String(), err)
	}
}

func TestAuthDevicesAndRevokeJSON(t *testing.T) {
	fake := &fakeAuthCommands{devices: []client.Device{{ID: "dev-1", Name: "laptop", Platform: "darwin", Status: "active"}}}
	app, stdout, _ := authApp(fake)
	if code := app.Run([]string{"auth", "devices", "--output=json"}); code != ExitSuccess || !strings.Contains(stdout.String(), `"id":"dev-1"`) {
		t.Fatalf("devices code=%d output=%q", code, stdout.String())
	}
	stdout.Reset()
	if code := app.Run([]string{"auth", "revoke-device", "dev-1", "--output=json"}); code != ExitSuccess || fake.revoked != "dev-1" {
		t.Fatalf("revoke code=%d output=%q fake=%#v", code, stdout.String(), fake)
	}
}

func TestAuthNotAuthenticatedErrorAndJSONHeadlessGuard(t *testing.T) {
	fake := &fakeAuthCommands{err: auth.ErrNotFound}
	app, stdout, stderr := authApp(fake)
	if code := app.Run([]string{"auth", "devices"}); code != ExitFailure || !strings.Contains(stderr.String(), "not authenticated") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	fake.err = nil
	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"auth", "login", "--no-browser", "--output=json"}); code != ExitUsage || !strings.Contains(stdout.String(), `"code":"usage"`) {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestAuthFactoryFailureIsUnavailable(t *testing.T) {
	app, stdout, stderr := authApp(&fakeAuthCommands{})
	app.auth = func() (authCommands, error) { return nil, errors.New("no keyring") }
	if code := app.Run([]string{"auth", "status", "--output=json"}); code != ExitUnavailable || stderr.Len() != 0 || !strings.Contains(stdout.String(), "credential_store_unavailable") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}
