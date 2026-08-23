package listener

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blazncloud/blazn/internal/proxy/router"
	"github.com/blazncloud/blazn/internal/proxycontract"
)

type credentials map[string]string

func (c credentials) DestinationCredential(_ context.Context, ref string) (string, error) {
	value, ok := c[ref]
	if !ok {
		return "", errors.New("missing credential")
	}
	return value, nil
}

type dns map[string][]netip.Addr

func (d dns) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	value, ok := d[host]
	if !ok {
		return nil, errors.New("missing address")
	}
	return value, nil
}

func routerConfig(t *testing.T) router.Config {
	t.Helper()
	file, err := os.Open(filepath.Join("..", "..", "..", "packages", "contracts", "proxy", "fixtures", "poc-policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	policy, err := proxycontract.DecodePolicy(file)
	if err != nil {
		t.Fatal(err)
	}
	return router.Config{
		Policy: policy, ActivationID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		Credentials: credentials{"node-route://qwen38": "local", "workspace-vault://poc/model-providers/openai": "cloud"},
		Resolver:    router.EndpointResolver{DNS: dns{"127.0.0.1": {netip.MustParseAddr("127.0.0.1")}, "api.openai.com": {netip.MustParseAddr("93.184.216.34")}}},
	}
}

func TestCredentialGenerationParsingAndChildEnvironment(t *testing.T) {
	credential, err := GenerateCredential()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(credential.authenticateValue())
	if err != nil || len(raw) != 32 || strings.Contains(credential.authenticateValue(), "=") {
		t.Fatalf("credential does not meet the frozen shape")
	}
	env, err := credential.ChildEnvironment([]string{"PATH=/bin", "OPENAI_API_KEY=old", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN=old"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "=old") || strings.Count(joined, credential.authenticateValue()) != 3 {
		t.Fatalf("child environment replacement failed")
	}
	for _, invalid := range []string{"", "abc", credential.authenticateValue() + "="} {
		if _, err := ParseCredential(invalid); err == nil {
			t.Fatalf("accepted invalid credential shape")
		}
	}
}

func TestRuntimeRejectsNonLoopbackAndShutsDownGracefully(t *testing.T) {
	credential, err := GenerateCredential()
	if err != nil {
		t.Fatal(err)
	}
	for _, address := range []string{"", "localhost", "0.0.0.0", "192.0.2.1", "::"} {
		if runtime, err := Start(Config{Address: address, Credential: credential, Router: routerConfig(t)}); err == nil {
			_ = runtime.Shutdown(context.Background())
			t.Fatalf("accepted unsafe listener address %q", address)
		}
	}
	runtime, err := Start(Config{Address: "127.0.0.1", Router: routerConfig(t)})
	if err != nil {
		t.Fatal(err)
	}
	childEnv, err := runtime.ChildEnvironment([]string{"PATH=/bin", "OPENAI_BASE_URL=https://old.invalid", "ANTHROPIC_BASE_URL=https://old.invalid"})
	if err != nil || len(childEnv) != 6 {
		t.Fatalf("generated listener credential was not available to child env: %v", err)
	}
	joined := strings.Join(childEnv, "\n")
	if strings.Contains(joined, "old.invalid") || !strings.Contains(joined, "OPENAI_BASE_URL=http://"+runtime.Address()+"/v1") || !strings.Contains(joined, "ANTHROPIC_BASE_URL=http://"+runtime.Address()) {
		t.Fatalf("listener endpoints were not safely published: %v", childEnv)
	}
	response, err := http.Get("http://" + runtime.Address() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", response.StatusCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := runtime.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runtime.Done():
	default:
		t.Fatal("runtime did not stop")
	}
}

func TestRuntimeSupportsIPv6Loopback(t *testing.T) {
	credential, err := GenerateCredential()
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := Start(Config{Address: "::1", Credential: credential, Router: routerConfig(t)})
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := runtime.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}
