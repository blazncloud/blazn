package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	developmentpkg "github.com/blazncloud/blazn/internal/development"
)

type fakeDevelopmentCommands struct {
	calls []string
	build developmentpkg.BuildDocument
	err   error
}

func (f *fakeDevelopmentCommands) Build(_ context.Context, ref, key string) (developmentpkg.BuildDocument, error) {
	f.calls = append(f.calls, "build:"+ref+":"+key)
	return f.build, f.err
}
func (f *fakeDevelopmentCommands) Test(_ context.Context, id, suite, key string) (developmentpkg.BuildDocument, error) {
	f.calls = append(f.calls, "test:"+id+":"+suite+":"+key)
	return f.build, f.err
}
func (f *fakeDevelopmentCommands) Status(_ context.Context, id string) (developmentpkg.BuildDocument, error) {
	f.calls = append(f.calls, "status:"+id)
	return f.build, f.err
}
func (f *fakeDevelopmentCommands) Evidence(_ context.Context, id, dir string) (developmentpkg.EvidenceExport, error) {
	f.calls = append(f.calls, "evidence:"+id+":"+dir)
	return developmentpkg.EvidenceExport{BuildID: id, Directory: dir, ManifestDigest: "sha256:" + strings.Repeat("a", 64), ArtifactIDs: []string{}}, f.err
}
func (f *fakeDevelopmentCommands) Publish(_ context.Context, id string, version int, key string) (developmentpkg.BuildDocument, error) {
	f.calls = append(f.calls, "publish:"+id+":"+key)
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
	}{{[]string{"dev", "build", "--ref", strings.Repeat("1", 40), "--request-id", "request-1"}, "build:"}, {[]string{"dev", "test", id, "--suite", "poc", "--request-id", "request-2"}, "test:"}, {[]string{"dev", "status", id}, "status:"}, {[]string{"dev", "evidence", id, "--output-dir", "evidence"}, "evidence:"}, {[]string{"dev", "publish", id, "--expected-version", "1", "--request-id", "request-3"}, "publish:"}} {
		stdout.Reset()
		stderr.Reset()
		before := len(fake.calls)
		if code := app.Run(test.args); code != ExitSuccess {
			t.Fatalf("%v code=%d stderr=%q", test.args, code, stderr.String())
		}
		if len(fake.calls) != before+1 || !strings.HasPrefix(fake.calls[before], test.call) {
			t.Fatalf("%v calls=%v", test.args, fake.calls)
		}
	}
}

func TestDevelopmentUsageFailsBeforeRuntime(t *testing.T) {
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	app := New(stdout, stderr, BuildInfo{})
	called := false
	app.development = func() (developmentCommands, error) { called = true; return nil, nil }
	for _, args := range [][]string{{"dev", "build", "--ref", "main", "--request-id", "request-1"}, {"dev", "test", "bad", "--suite", "poc", "--request-id", "request-2"}, {"dev", "publish", "30000000-0000-4000-8000-000000000001", "--expected-version", "0", "--request-id", "request-3"}} {
		if code := app.Run(args); code != ExitUsage {
			t.Fatalf("%v code=%d", args, code)
		}
	}
	if called {
		t.Fatal("invalid input constructed runtime")
	}
}
