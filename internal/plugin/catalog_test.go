package plugin

import "testing"

func TestDefaultCatalogResolvesCanonicalAndAliases(t *testing.T) {
	catalog := DefaultCatalog()
	for _, command := range []string{"social", "person", "company", "contact", "connections", "post", "evidence", "entity", "data", "providers"} {
		definition, ok := catalog.Resolve(command)
		if !ok || definition.Name != "social" {
			t.Fatalf("Resolve(%q) = %#v, %v", command, definition, ok)
		}
	}
	for _, command := range []string{"content", "media", "image", "video", "audio", "render", "remix"} {
		definition, ok := catalog.Resolve(command)
		if !ok || definition.Name != "content" {
			t.Fatalf("Resolve(%q) = %#v, %v", command, definition, ok)
		}
	}
	if _, ok := catalog.Resolve("auth"); ok {
		t.Fatal("reserved core command resolved as a plugin")
	}
	if _, ok := catalog.Resolve("project"); ok {
		t.Fatal("Project core command resolved as a plugin")
	}
	if _, ok := catalog.Resolve("unknown"); ok {
		t.Fatal("unknown command resolved as a plugin")
	}
}

func TestContentUsesIndependentRepositoryAndSigner(t *testing.T) {
	definition, ok := DefaultCatalog().Plugin("content")
	if !ok {
		t.Fatal("content plugin is missing")
	}
	if definition.Repository != "blazncloud/blazn-content" || definition.Executable != "blazn-content" {
		t.Fatalf("content distribution = %#v", definition)
	}
	if definition.SigningIdentity != "blazn-content-release" || definition.SignatureNamespace != "blazn-content-release" || definition.AllowedSigner == socialDefinition.AllowedSigner {
		t.Fatalf("content trust root = %#v", definition)
	}
}

func TestCatalogRejectsConflictingOwnership(t *testing.T) {
	definition := socialDefinition
	definition.CanonicalCommand = "auth"
	if _, err := NewCatalog([]Definition{definition}, []string{"auth"}); err == nil {
		t.Fatal("catalog accepted a reserved command conflict")
	}
	definition.CanonicalCommand = "social"
	definition.Aliases = []string{"person", "person"}
	if _, err := NewCatalog([]Definition{definition}, nil); err == nil {
		t.Fatal("catalog accepted duplicate aliases")
	}
}
