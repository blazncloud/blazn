package plugin

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestPluginEnvironmentRemovesInstallerCredentials(t *testing.T) {
	filtered := pluginEnvironment([]string{
		"PATH=/usr/bin",
		"GH_TOKEN=secret",
		"GITHUB_TOKEN=secret",
		"GH_ENTERPRISE_TOKEN=secret",
		"GITHUB_ENTERPRISE_TOKEN=secret",
		"BLAZN_PLUGIN_VERSION=v0.1.0",
		"HUNTER_API_KEY=provider-secret",
	})
	joined := strings.Join(filtered, "\n")
	if joined != "PATH=/usr/bin\nHUNTER_API_KEY=provider-secret" {
		t.Fatalf("unexpected plugin environment: %q", joined)
	}
}

type countingRunner struct{ calls int }

func (r *countingRunner) Run(context.Context, string, []string, RuntimeContext, Stdio) (int, error) {
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
	if len(statuses) != 2 {
		t.Fatalf("status=%+v", statuses)
	}
	byName := map[string]Status{}
	for _, status := range statuses {
		byName[status.Name] = status
	}
	if social := byName["social"]; !social.Installed || social.Healthy || social.Message == "" {
		t.Fatalf("social status=%+v", social)
	}
	if content := byName["content"]; content.Installed || content.Healthy || content.Message != "not installed" {
		t.Fatalf("content status=%+v", content)
	}
	runtimeContext, err := NewRuntimeContext("v1.0.0", "human")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Run(context.Background(), definition, []string{"person", "search"}, "human", runtimeContext, Stdio{}); err == nil {
		t.Fatal("incompatible installed plugin was dispatched")
	}
	if runner.calls != 0 {
		t.Fatalf("runner calls=%d", runner.calls)
	}
}

func TestServiceRejectsInvalidRuntimeContextBeforeDispatch(t *testing.T) {
	store, err := NewStoreAt(filepath.Join(t.TempDir(), "plugins"))
	if err != nil {
		t.Fatal(err)
	}
	definition, _ := DefaultCatalog().Plugin("content")
	manifest := validManifest("v1.0.0")
	manifest.Name = definition.Name
	manifest.Executable = definition.Executable
	manifest.Commands = append([]string{definition.CanonicalCommand}, definition.Aliases...)
	if _, err := store.Activate(definition, manifest, candidate(t, t.TempDir(), definition.Executable)); err != nil {
		t.Fatal(err)
	}
	runner := &countingRunner{}
	service := &Service{Catalog: DefaultCatalog(), Store: store, Runner: runner, CoreVersion: "v1.0.0"}
	runtimeContext, err := NewRuntimeContext("v1.0.0", "human")
	if err != nil {
		t.Fatal(err)
	}
	runtimeContext.InvocationID = "invalid"
	if _, err := service.Run(context.Background(), definition, []string{"doctor"}, "human", runtimeContext, Stdio{}); err == nil {
		t.Fatal("invalid runtime context was dispatched")
	}
	if runner.calls != 0 {
		t.Fatalf("runner calls=%d", runner.calls)
	}
}
