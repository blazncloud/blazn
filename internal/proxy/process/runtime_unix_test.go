//go:build darwin || linux

package process

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/blazncloud/blazn/internal/proxycontract"
)

func TestResolvedRuntimeFactoryConsumesOnlyFrozenBootstrapAndCleansSecrets(t *testing.T) {
	bootstrap := resolvedRuntimeBootstrap(t)
	directory := socketTempDir(t)
	factory := ResolvedRuntimeFactory{Platform: NewUnixPlatform(), ControlDirectory: directory, ControlTimeout: time.Second}
	started, err := factory.Start(context.Background(), bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	runtime, ok := started.(*resolvedRuntime)
	if !ok {
		t.Fatalf("unexpected runtime type %T", started)
	}
	if runtime.Address() == "" || runtime.ControlAddress() == "" || !validListenerToken(runtime.ListenerToken()) {
		t.Fatal("runtime did not expose bounded listener/control material")
	}
	info, err := os.Lstat(runtime.ControlAddress())
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0600 || statUID(info) != os.Getuid() {
		t.Fatalf("unsafe control socket: info=%v err=%v", info, err)
	}

	// RunChild zeroes its owned bootstrap immediately after Start returns. The
	// runtime must therefore retain its own private copy and no resolver handle.
	zeroBootstrap(&bootstrap)
	credential, err := runtime.credentials.DestinationCredential(context.Background(), "node-route://qwen38")
	if err != nil || credential != "already-resolved-secret" {
		t.Fatalf("runtime did not own its resolved credential copy: %q %v", credential, err)
	}

	serveCtx, cancelServe := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- runtime.ServeControl(serveCtx, func(_ context.Context, request ControlRequest) (ControlResponse, error) {
			return ControlResponse{Version: ProtocolVersion, Kind: "control_response", Action: request.Action, Challenge: request.Challenge, Accepted: true}, nil
		})
	}()
	connection, err := NewUnixPlatform().DialControl(context.Background(), os.Getpid(), runtime.ControlAddress())
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := freshChallenge()
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFrame(connection, MaxControlBytes, ControlRequest{Version: ProtocolVersion, Kind: "control_request", Action: "inspect", Challenge: challenge}); err != nil {
		t.Fatal(err)
	}
	var response ControlResponse
	if err := readFrame(connection, MaxControlBytes, &response); err != nil || response.Challenge != challenge {
		t.Fatalf("control response failed: %#v %v", response, err)
	}
	_ = connection.Close()
	cancelServe()
	select {
	case err := <-serveDone:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled control server returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("control server ignored cancellation")
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := runtime.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(runtime.ControlAddress()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("control socket survived shutdown: %v", err)
	}
	if _, err := runtime.credentials.DestinationCredential(context.Background(), "node-route://qwen38"); err == nil {
		t.Fatal("resolved credential survived shutdown")
	}
}

func TestResolvedRuntimeFactoryRejectsDirectoryAndEvidenceSubstitution(t *testing.T) {
	bootstrap := resolvedRuntimeBootstrap(t)
	platform := NewUnixPlatform()

	unsafeDirectory := t.TempDir()
	if err := os.Chmod(unsafeDirectory, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := (ResolvedRuntimeFactory{Platform: platform, ControlDirectory: unsafeDirectory}).Start(context.Background(), bootstrap); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unsafe control directory returned %v", err)
	}

	directory := socketTempDir(t)
	wrong := bootstrap
	wrong.Metadata.BinaryDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, err := (ResolvedRuntimeFactory{Platform: platform, ControlDirectory: directory}).Start(context.Background(), wrong); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("substituted binary evidence returned %v", err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (ResolvedRuntimeFactory{Platform: platform, ControlDirectory: directory}).Start(cancelled, bootstrap); err == nil {
		t.Fatal("cancelled runtime start succeeded")
	}
}

func resolvedRuntimeBootstrap(t *testing.T) Bootstrap {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "packages", "contracts", "proxy", "fixtures", "poc-policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	policy, err := proxycontract.DecodePolicy(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	policy.Routes = policy.Routes[:1]
	for name, alias := range policy.Aliases {
		alias.RouteIDs = []string{policy.Routes[0].ID}
		policy.Aliases[name] = alias
	}
	raw, err = json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	platform := NewUnixPlatform()
	evidence, live, err := platform.Evidence(context.Background(), os.Getpid())
	if err != nil || !live {
		t.Fatalf("current process evidence: live=%t err=%v", live, err)
	}
	return Bootstrap{
		Version:   ProtocolVersion,
		Kind:      "bootstrap",
		Challenge: testNonce,
		Metadata: Metadata{
			ActivationID:    "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
			Nonce:           testNonce,
			Generation:      2,
			OwnerUID:        os.Getuid(),
			Mode:            "session",
			SessionIdentity: "uid:test/session:test",
			BinaryPath:      executable,
			BinaryDigest:    evidence.BinaryDigest,
		},
		Policy:      raw,
		Credentials: []Credential{{Reference: "node-route://qwen38", Value: []byte("already-resolved-secret")}},
	}
}
