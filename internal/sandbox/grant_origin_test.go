package sandbox

import (
	"testing"
	"time"

	"github.com/blazncloud/blazn/internal/client"
)

func TestValidateGrantBindsEndpointToAPIOrigin(t *testing.T) {
	sandboxID := "11111111-1111-4111-8111-111111111111"
	created := grant(client.SandboxGrantExec, sandboxID)
	created.Endpoint = "https://api.blazn.frontro.com/v1/sandbox-access-grants/22222222-2222-4222-8222-222222222222/exec"
	now := time.Now().UTC()
	if err := validateGrant(created, sandboxID, client.SandboxGrantExec, now, "https://api.blazn.frontro.com"); err != nil {
		t.Fatalf("same-origin endpoint rejected: %v", err)
	}
	if err := validateGrant(created, sandboxID, client.SandboxGrantExec, now, "https://blazn.benpelo.com"); err == nil {
		t.Fatal("cross-origin endpoint accepted")
	}
	for _, endpoint := range []string{
		"http://api.blazn.frontro.com/v1/sandbox-access-grants/x",
		"https://api.blazn.frontro.com.evil.test/v1/sandbox-access-grants/x",
		"https://user@api.blazn.frontro.com/v1/sandbox-access-grants/x",
		"https://api.blazn.frontro.com/v1/other/x",
	} {
		created.Endpoint = endpoint
		if err := validateGrant(created, sandboxID, client.SandboxGrantExec, now, "https://api.blazn.frontro.com"); err == nil {
			t.Fatalf("endpoint %q accepted", endpoint)
		}
	}
}
