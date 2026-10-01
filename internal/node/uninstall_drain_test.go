package node

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/blazncloud/blazn/internal/client"
)

type leavingPlatform struct {
	*mockPlatform
	events *[]string
}

func (p leavingPlatform) LeaveCluster(context.Context) error {
	*p.events = append(*p.events, "leave")
	return nil
}

func TestUninstallAsksTheControlPlaneToDrainBeforeLeaving(t *testing.T) {
	for _, drainErr := range []error{nil, errors.New("control plane unreachable")} {
		identity := testIdentity(t)
		plan := installPlan()
		plan.Mode = client.NodeModeFresh
		plan.Mutations = append(plan.Mutations, client.NodeInstallMutation{Ordinal: 3, Kind: "package", Action: "install", Target: "microk8s",
			Desired: map[string]any{"name": "microk8s"}, DesiredDigest: "sha256:" + testHash})
		meta := client.NodeEnrollmentIdentity{Generation: 1, SigningKeyID: "node-identity/v1", PublicKeyFingerprint: mustFingerprint(t, identity), IssuedAt: plan.IssuedAt, ExpiresAt: plan.ExpiresAt}
		events := []string{}
		installer := NewInstaller(leavingPlatform{mockPlatform: &mockPlatform{failAt: -1}, events: &events}, &memoryState{})
		installer.uid = func() int64 { return 0 }
		installer.now = func() time.Time { return time.Date(2026, 8, 22, 12, 1, 0, 0, time.UTC) }
		if installed, err := installer.Install(context.Background(), plan, meta, identity); err != nil || installed.State != "active" {
			t.Fatalf("install=%#v err=%v", installed, err)
		}
		installer.SetDrainer(func(context.Context) error {
			events = append(events, "drain")
			return drainErr
		})
		removed, err := installer.Uninstall(context.Background(), plan, meta, identity, false)
		if err != nil || removed.State != "removed" {
			t.Fatalf("drainErr=%v uninstall=%#v err=%v", drainErr, removed, err)
		}
		// A failed drain never blocks leaving: the local retirement mark still runs.
		if strings.Join(events, ",") != "drain,leave" {
			t.Fatalf("drainErr=%v events=%v", drainErr, events)
		}
	}
}
