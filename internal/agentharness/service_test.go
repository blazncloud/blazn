package agentharness

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadAndValidateDocument(t *testing.T) {
	p := filepath.Join(t.TempDir(), "agent.json")
	if e := os.WriteFile(p, []byte(`{"id":"i","agentId":"a","workspaceId":"w","version":1,"digest":"d","createdBy":"u","createdAt":"now"}`), 0600); e != nil {
		t.Fatal(e)
	}
	d, e := ReadDocument(p)
	if e != nil {
		t.Fatal(e)
	}
	if e = ValidateDocument("agent-version", d); e != nil {
		t.Fatal(e)
	}
	delete(d, "digest")
	if e = ValidateDocument("agent-version", d); e == nil {
		t.Fatal("missing required field accepted")
	}
}
func TestReadDocumentRejectsTrailingJSON(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(p, []byte(`{} {}`), 0600)
	if _, e := ReadDocument(p); e == nil {
		t.Fatal("trailing JSON accepted")
	}
}
