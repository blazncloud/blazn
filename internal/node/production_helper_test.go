package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/blazncloud/blazn/internal/client"
)

type managementPrepareFixture struct {
	preparation serviceStatePreparation
	parent      string
	systemValue []byte
}

// newManagementPrepareFixture lays out an installed node: a root authority
// whose signed plan pins pinnedValue, daemon-owned service state under a
// 0755 parent, and the pinned system binary. The preparation runs
// executableValue.
func newManagementPrepareFixture(t *testing.T, pinnedValue, executableValue []byte) managementPrepareFixture {
	t.Helper()
	base := testRoot(t)
	parent := filepath.Join(base, "var-lib-blazn")
	paths := ProductionNodePaths{ServiceStateRoot: filepath.Join(parent, "node"), RootStateRoot: filepath.Join(base, "root-state"), ProfileRoot: filepath.Join(base, "profiles")}
	if err := os.Mkdir(parent, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(paths.ServiceStateRoot, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"identity.json", "runtime.json", "enrollment-pin.json"} {
		if err := os.WriteFile(filepath.Join(paths.ServiceStateRoot, name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	authorization, _ := validBootstrapAuthorization(t)
	plan := authorization.Expected.Plan
	sum := sha256.Sum256(pinnedValue)
	pinned := false
	for index, component := range plan.Components {
		if component.SourceClass == "current_binary" && component.ArtifactType == "binary" {
			plan.Components[index].SHA256 = hex.EncodeToString(sum[:])
			pinned = true
			// The file mutation that installs this component signs the same digest.
			for mutationIndex, mutation := range plan.Mutations {
				if mutation.Kind == "file" && mutation.Desired["contentSha256"] == component.SHA256 {
					desired := map[string]any{}
					for key, value := range mutation.Desired {
						desired[key] = value
					}
					desired["contentSha256"] = hex.EncodeToString(sum[:])
					plan.Mutations[mutationIndex].Desired = desired
					plan.Mutations[mutationIndex].DesiredDigest = "sha256:" + hex.EncodeToString(sum[:])
				}
			}
		}
	}
	if !pinned {
		t.Fatal("fixture plan lacks a current binary component")
	}
	digest, err := client.NodeInstallPlanDigest(plan)
	if err != nil {
		t.Fatal(err)
	}
	plan.Digest = digest
	profile := trustedBootstrapProfile(plan)
	authority := RootInstallAuthority{SchemaVersion: RootInstallAuthoritySchema, Plan: plan, Identity: authorization.Expected.Identity, PlanSigningKey: authorization.PlanSigningKey, NodePublicKey: authorization.NodePublicKey, KubernetesBinding: authorization.KubernetesBinding, ProfileID: profile.ID, ProfileSHA256: "sha256:" + testHash, ControlPlaneOrigin: profile.ControlPlaneOrigin, AuthorizedAt: plan.IssuedAt}
	if authority.Digest, err = RootInstallAuthorityDigest(authority); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(authority)
	if err != nil {
		t.Fatal(err)
	}
	if err := writePrivateAtomic(paths.InstallAuthorityPath(), encoded); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRootAuthority(paths.InstallAuthorityPath()); err != nil {
		t.Fatalf("fixture authority does not load: %v", err)
	}
	executable := filepath.Join(base, "running-blazn")
	if err := os.WriteFile(executable, executableValue, 0755); err != nil {
		t.Fatal(err)
	}
	systemBinary := filepath.Join(base, "system-blazn")
	if err := os.WriteFile(systemBinary, pinnedValue, 0755); err != nil {
		t.Fatal(err)
	}
	uid, gid := int(currentUID()), os.Getgid()
	return managementPrepareFixture{
		parent:      parent,
		systemValue: pinnedValue,
		preparation: serviceStatePreparation{
			Paths:            paths,
			UID:              uid,
			GID:              gid,
			Allowed:          map[int64]bool{int64(uid): true},
			ReturnParent:     true,
			Executable:       executable,
			SystemBinaryPath: systemBinary,
			WriteSystemBinary: func(string, []byte) error {
				t.Error("system binary was rewritten")
				return errors.New("unexpected system binary write")
			},
			ProvisionProfiles: func(ProductionNodePaths, int, int) error { return nil },
		},
	}
}

func (f managementPrepareFixture) assertUnchanged(t *testing.T, before serviceStateOwnershipSnapshot) {
	t.Helper()
	after, err := snapshotServiceStateOwnership(f.preparation.Paths.ServiceStateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("service-state ownership changed:\nbefore=%#v\nafter=%#v", before, after)
	}
	if after.Parent.Mode != 0755 {
		t.Fatalf("service-state parent mode=%v, want 0755", after.Parent.Mode)
	}
	value, err := os.ReadFile(f.preparation.SystemBinaryPath)
	if err != nil || string(value) != string(f.systemValue) {
		t.Fatalf("system binary changed: %q err=%v", value, err)
	}
	if _, err := os.Lstat(filepath.Join(f.preparation.Paths.RootStateRoot, serviceStateHandoffName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("service-state handoff record remains: %v", err)
	}
}

func TestManagementPrepareRefusesUnpinnedBinaryWithoutMutation(t *testing.T) {
	fixture := newManagementPrepareFixture(t, []byte("blazn v0.1.0-poc.132"), []byte("blazn v0.1.0-poc.134"))
	fixture.preparation.ProvisionProfiles = func(ProductionNodePaths, int, int) error {
		t.Error("bootstrap profiles were provisioned for an unpinned binary")
		return nil
	}
	before, err := snapshotServiceStateOwnership(fixture.preparation.Paths.ServiceStateRoot)
	if err != nil {
		t.Fatal(err)
	}
	err = fixture.preparation.prepare()
	if err == nil || !strings.Contains(err.Error(), "not the binary pinned by the signed install plan") {
		t.Fatalf("unpinned management binary was accepted: %v", err)
	}
	fixture.assertUnchanged(t, before)
}

func TestManagementPrepareRestoresOwnershipAfterLaterFailure(t *testing.T) {
	value := []byte("blazn v0.1.0-poc.132")
	fixture := newManagementPrepareFixture(t, value, value)
	fixture.preparation.ProvisionProfiles = func(ProductionNodePaths, int, int) error {
		info, err := os.Lstat(fixture.parent)
		if err != nil || info.Mode().Perm() != 0711 {
			t.Errorf("failure was injected before the ownership transition: info=%v err=%v", info, err)
		}
		if _, err := os.Lstat(filepath.Join(fixture.preparation.Paths.RootStateRoot, serviceStateHandoffName)); err != nil {
			t.Errorf("ownership was transitioned before it was recorded: %v", err)
		}
		return errors.New("bootstrap profile provisioning failed")
	}
	before, err := snapshotServiceStateOwnership(fixture.preparation.Paths.ServiceStateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.preparation.prepare(); err == nil || !strings.Contains(err.Error(), "bootstrap profile provisioning failed") {
		t.Fatalf("later failure was not reported: %v", err)
	}
	fixture.assertUnchanged(t, before)
}

func TestManagementRestoreReturnsRecordedOwnershipAfterCommandFailure(t *testing.T) {
	value := []byte("blazn v0.1.0-poc.132")
	fixture := newManagementPrepareFixture(t, value, value)
	before, err := snapshotServiceStateOwnership(fixture.preparation.Paths.ServiceStateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.preparation.prepare(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(fixture.parent); err != nil || info.Mode().Perm() != 0711 {
		t.Fatalf("preparation did not hand the parent to the caller: info=%v err=%v", info, err)
	}
	// A file the failed command wrote after the handoff joins the daemon's
	// private state rather than staying with the caller.
	created := filepath.Join(fixture.preparation.Paths.ServiceStateRoot, "uninstall-cleanup.json")
	if err := os.WriteFile(created, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := fixture.preparation.restoreRecorded(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(created); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("created state was not made private: info=%v err=%v", info, err)
	}
	if err := os.Remove(created); err != nil {
		t.Fatal(err)
	}
	fixture.assertUnchanged(t, before)
	if err := fixture.preparation.restoreRecorded(); err != nil {
		t.Fatalf("restore without a record must be a no-op: %v", err)
	}
}

func TestManagementCommandsRestoreStateOnlyAfterSuccessfulPreparation(t *testing.T) {
	for _, test := range []struct {
		name        string
		prepareErr  error
		wantRestore int
	}{
		{name: "later failure restores", wantRestore: 1},
		{name: "refused preparation does not restore", prepareErr: errors.New("running Blazn executable is not the binary pinned"), wantRestore: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := testRoot(t)
			prepares, restores := 0, 0
			runtime := &CommandRuntime{
				State:        FileStateStore{Root: root},
				Identities:   FileIdentityStore{Path: filepath.Join(root, "identity.json")},
				PrepareState: func(context.Context) error { prepares++; return test.prepareErr },
				RestoreState: func(context.Context) error { restores++; return nil },
			}
			if _, err := runtime.Uninstall(context.Background(), false); err == nil {
				t.Fatal("uninstall without runtime state succeeded")
			}
			if prepares != 1 || restores != test.wantRestore {
				t.Fatalf("prepares=%d restores=%d, want 1 and %d", prepares, restores, test.wantRestore)
			}
		})
	}
}
