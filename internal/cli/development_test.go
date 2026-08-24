package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	developmentpkg "github.com/blazncloud/blazn/internal/development"
)

type fakeDevelopmentCommands struct {
	calls   []string
	project developmentpkg.ProjectDocument
	build   developmentpkg.BuildDocument
	err     error
}

func (f *fakeDevelopmentCommands) Register(_ context.Context, manifest developmentpkg.Manifest, version int, key string) (developmentpkg.ProjectDocument, error) {
	f.calls = append(f.calls, fmt.Sprintf("register:%s:%d:%s", manifest.ProjectID, version, key))
	return f.project, f.err
}

func (f *fakeDevelopmentCommands) Build(_ context.Context, ref, key string) (developmentpkg.BuildDocument, error) {
	f.calls = append(f.calls, "build:"+ref+":"+key)
	return f.build, f.err
}
func (f *fakeDevelopmentCommands) Status(_ context.Context, id string) (developmentpkg.BuildDocument, error) {
	f.calls = append(f.calls, "status:"+id)
	return f.build, f.err
}

func TestDevelopmentCommandsMatchFrozenSurface(t *testing.T) {
	const id = "30000000-0000-4000-8000-000000000001"
	build, err := developmentpkg.DecodeBuild([]byte(`{"schemaVersion":"blazn.dev/build-status/v1alpha1","id":"` + id + `","status":"queued","version":1,"receiptDigest":null}`))
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeDevelopmentCommands{build: build}
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	app := New(stdout, stderr, BuildInfo{})
	app.development = func() (developmentCommands, error) { return fake, nil }
	for _, test := range []struct {
		args []string
		call string
	}{{[]string{"dev", "register", "-f", "../../examples/coding-agent/blazn.yaml", "--request-id", "register-request-1"}, "register:20000000-0000-4000-8000-000000000001:0:register-request-1"}, {[]string{"dev", "build", "--ref", strings.Repeat("1", 40), "--request-id", "request-1"}, "build:" + strings.Repeat("1", 40) + ":request-1"}, {[]string{"dev", "status", id}, "status:" + id}} {
		stdout.Reset()
		stderr.Reset()
		before := len(fake.calls)
		if code := app.Run(test.args); code != ExitSuccess {
			t.Fatalf("%v code=%d stderr=%q", test.args, code, stderr.String())
		}
		if len(fake.calls) != before+1 || fake.calls[before] != test.call {
			t.Fatalf("%v calls=%v", test.args, fake.calls)
		}
	}
}

func TestDevelopmentUsageFailsBeforeRuntime(t *testing.T) {
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	app := New(stdout, stderr, BuildInfo{})
	called := false
	app.development = func() (developmentCommands, error) { called = true; return nil, nil }
	for _, args := range [][]string{{"dev", "register", "--expected-version", "-1", "--request-id", "request-1"}, {"dev", "register", "--expected-version", "0", "--request-id", "short"}, {"dev", "build", "--ref", "main", "--request-id", "request-1"}, {"dev", "status", "bad"}} {
		if code := app.Run(args); code != ExitUsage {
			t.Fatalf("%v code=%d", args, code)
		}
	}
	if called {
		t.Fatal("invalid input constructed runtime")
	}
}

func TestDevelopmentRegisterRejectsInvalidManifestBeforeRuntime(t *testing.T) {
	manifest := filepath.Join(t.TempDir(), "blazn.yaml")
	if err := os.WriteFile(manifest, []byte(`{"schemaVersion":"wrong"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	app := New(stdout, stderr, BuildInfo{})
	called := false
	app.development = func() (developmentCommands, error) { called = true; return nil, nil }
	if code := app.Run([]string{"dev", "register", "-f", manifest, "--expected-version", "0", "--request-id", "register-request-1"}); code != ExitFailure {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if called {
		t.Fatal("invalid manifest constructed runtime")
	}
}

func TestDevelopmentValidateJSONReturnsFailureForInvalidManifest(t *testing.T) {
	manifest := filepath.Join(t.TempDir(), "blazn.yaml")
	if err := os.WriteFile(manifest, []byte("not: [valid"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	app := New(stdout, stderr, BuildInfo{})
	if code := app.Run([]string{"--output", "json", "dev", "validate", "-f", manifest}); code != ExitFailure {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}
