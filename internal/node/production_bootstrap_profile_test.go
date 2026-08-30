package node

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestProvisionBootstrapProfilesIsExactAndIdempotent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	paths := ProductionNodePaths{
		ProfileRoot:   filepath.Join(root, "etc", "blazn", "node", "profiles"),
		RootStateRoot: filepath.Join(root, "state"),
	}
	if err := os.Mkdir(filepath.Join(root, "etc"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(paths.RootStateRoot, 0700); err != nil {
		t.Fatal(err)
	}
	profile := []byte(`{"schemaVersion":1,"id":"test/v1"}`)
	profiles := map[string][]byte{"fresh.json": profile}
	uid, gid := os.Getuid(), os.Getgid()
	if err := provisionBootstrapProfiles(paths, uid, gid, uid, gid, profiles); err != nil {
		t.Fatal(err)
	}
	if err := provisionBootstrapProfiles(paths, uid, gid, uid, gid, profiles); err != nil {
		t.Fatalf("idempotent provision: %v", err)
	}
	stored, err := os.ReadFile(filepath.Join(paths.ProfileRoot, "fresh.json"))
	if err != nil || !bytes.Equal(bytes.TrimSpace(stored), profile) {
		t.Fatalf("stored=%q err=%v", stored, err)
	}
	if err := os.WriteFile(filepath.Join(paths.ProfileRoot, "fresh.json"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := provisionBootstrapProfiles(paths, uid, gid, uid, gid, profiles); err == nil {
		t.Fatal("modified profile was replaced")
	}
}

func TestProductionLinuxBootstrapProfileParses(t *testing.T) {
	binaryRoot := filepath.Join(t.TempDir(), "binary-private")
	if err := os.Mkdir(binaryRoot, 0700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(binaryRoot, "blazn")
	if err := os.WriteFile(binary, []byte("binary"), 0700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "profile-private")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "profile.json")
	encoded := productionBootstrapProfiles()["ubuntu-26.04-amd64-worker.json"]
	if err := os.WriteFile(path, append(encoded, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	profile, err := LoadTrustedProfile(path, binary, "v-test")
	if err != nil || profile.ID != "ubuntu-26.04-amd64-worker/v1" {
		t.Fatalf("profile=%#v err=%v", profile, err)
	}
}
