package plugin

import (
	"strings"
	"testing"
)

func validManifest(version string) Manifest {
	return Manifest{SchemaVersion: 1, Name: "social", Version: version, ProtocolVersion: 1, MinimumCoreVersion: "v1.0.0", Executable: "blazn-social", Commands: []string{"social", "person", "company", "contact", "connections", "saved-search", "graph", "post", "evidence", "entity", "data", "providers"}}
}

func TestManifestStrictDecodeAndCompatibility(t *testing.T) {
	encoded := `{"schemaVersion":1,"name":"social","version":"v1.2.0","protocolVersion":1,"minimumCoreVersion":"v1.0.0","executable":"blazn-social","commands":["social"],"unexpected":true}`
	if _, err := DecodeManifest(strings.NewReader(encoded)); err == nil {
		t.Fatal("manifest accepted unknown field")
	}
	manifest := validManifest("v1.2.0")
	if err := Compatible("v1.1.0", manifest); err != nil {
		t.Fatalf("compatible core rejected: %v", err)
	}
	if err := Compatible("v0.9.0", manifest); err == nil {
		t.Fatal("incompatible core accepted")
	}
	manifest.ProtocolVersion = 2
	if err := manifest.Validate(); err == nil {
		t.Fatal("unsupported protocol accepted")
	}
}

func TestManifestRejectsUnsafeExecutableAndDuplicateCommands(t *testing.T) {
	manifest := validManifest("v1.0.0")
	manifest.Executable = "../blazn-social"
	if err := manifest.Validate(); err == nil {
		t.Fatal("unsafe executable accepted")
	}
	manifest = validManifest("v1.0.0")
	manifest.Commands = append(manifest.Commands, "person")
	if err := manifest.Validate(); err == nil {
		t.Fatal("duplicate command accepted")
	}
}

func TestCompatibilityHonorsSemanticPrereleasePrecedence(t *testing.T) {
	manifest := validManifest("v1.2.0")
	manifest.MinimumCoreVersion = "v1.0.0"
	if err := Compatible("v1.0.0-alpha.1", manifest); err == nil {
		t.Fatal("prerelease core accepted as the final minimum release")
	}
	manifest.MinimumCoreVersion = "v1.0.0-alpha.1"
	if err := Compatible("v1.0.0-alpha.2", manifest); err != nil {
		t.Fatalf("newer prerelease rejected: %v", err)
	}
	if _, err := parseVersion("v1.0.0-alpha.01"); err == nil {
		t.Fatal("numeric prerelease identifier with a leading zero was accepted")
	}
	manifest.MinimumCoreVersion = "v1.0.0-10000000000000000000"
	if err := Compatible("v1.0.0-9999999999999999999", manifest); err == nil {
		t.Fatal("smaller arbitrary-precision numeric prerelease identifier was accepted")
	}
}
