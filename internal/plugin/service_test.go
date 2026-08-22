package plugin

import (
	"context"
	"path/filepath"
	"testing"
)

type countingRunner struct{ calls int }

func (r *countingRunner) Run(context.Context, string, []string, Stdio) (int, error) {
	r.calls++
	return 0, nil
}

func TestServiceRechecksCompatibilityForHealthAndDispatch(t *testing.T) {
	store, err := NewStoreAt(filepath.Join(t.TempDir(), "plugins"))
	if err != nil {
		t.Fatal(err)
	}
	definition, _ := DefaultCatalog().Plugin("social")
	manifest := validManifest("v1.0.0")
	manifest.MinimumCoreVersion = "v2.0.0"
	if _, err := store.Activate(definition, manifest, candidate(t, t.TempDir(), "plugin")); err != nil {
		t.Fatal(err)
	}
	runner := &countingRunner{}
	service := &Service{Catalog: DefaultCatalog(), Store: store, Runner: runner, CoreVersion: "v1.0.0"}
	statuses := service.List()
	if len(statuses) != 1 || !statuses[0].Installed || statuses[0].Healthy || statuses[0].Message == "" {
		t.Fatalf("status=%+v", statuses)
	}
	if _, err := service.Run(context.Background(), definition, []string{"person", "search"}, "human", Stdio{}); err == nil {
		t.Fatal("incompatible installed plugin was dispatched")
	}
	if runner.calls != 0 {
		t.Fatalf("runner calls=%d", runner.calls)
	}
}
