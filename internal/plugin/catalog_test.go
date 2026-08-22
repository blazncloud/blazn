package plugin

import "testing"

func TestDefaultCatalogResolvesCanonicalAndAliases(t *testing.T) {
	catalog := DefaultCatalog()
	for _, command := range []string{"social", "person", "company", "contact", "connections", "content", "post", "evidence", "entity", "data", "providers"} {
		definition, ok := catalog.Resolve(command)
		if !ok || definition.Name != "social" {
			t.Fatalf("Resolve(%q) = %#v, %v", command, definition, ok)
		}
	}
	if _, ok := catalog.Resolve("auth"); ok {
		t.Fatal("reserved core command resolved as a plugin")
	}
	if _, ok := catalog.Resolve("unknown"); ok {
		t.Fatal("unknown command resolved as a plugin")
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
