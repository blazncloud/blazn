package cli

import (
	"bytes"
	"context"
	"os"
	"reflect"
	"strings"
	"testing"

	pluginpkg "github.com/blazncloud/blazn/internal/plugin"
)

type fakePlugins struct {
	installed bool
	installs  int
	runs      int
	args      []string
	format    string
	name      string
	installName string
	runName string
	context   pluginpkg.RuntimeContext
}

func (f *fakePlugins) definition() pluginpkg.Definition {
	d, _ := pluginpkg.DefaultCatalog().Plugin("social")
	return d
}
func (f *fakePlugins) Resolve(command string) (pluginpkg.Definition, bool) {
	return pluginpkg.DefaultCatalog().Resolve(command)
}
func (f *fakePlugins) Installed(string) (pluginpkg.Installed, error) {
	if !f.installed {
		return pluginpkg.Installed{}, os.ErrNotExist
	}
	return pluginpkg.Installed{Receipt: pluginpkg.Receipt{Version: "v1.0.0"}, Path: "/safe/blazn-social"}, nil
}
func (f *fakePlugins) Install(_ context.Context, name string) (pluginpkg.Receipt, error) {
	f.installs++
	f.installed = true
	f.name, f.installName = name, name
	return pluginpkg.Receipt{Version: "v1.0.0"}, nil
}
func (f *fakePlugins) List() []pluginpkg.Status {
	return []pluginpkg.Status{{Name: "social", Installed: f.installed, Healthy: f.installed, Commands: []string{"social", "person"}}}
}
func (f *fakePlugins) Rollback(string) (pluginpkg.Receipt, error) {
	return pluginpkg.Receipt{Version: "v0.9.0"}, nil
}
func (f *fakePlugins) Remove(string) error { f.installed = false; return nil }
func (f *fakePlugins) Run(_ context.Context, definition pluginpkg.Definition, args []string, format string, runtimeContext pluginpkg.RuntimeContext, streams pluginpkg.Stdio) (int, error) {
	f.runs++
	f.args, f.format, f.name, f.runName = append([]string(nil), args...), format, definition.Name, definition.Name
	f.context = runtimeContext
	_, _ = streams.Stdout.Write([]byte("plugin output\n"))
	return 3, nil
}

func TestMissingContentPluginApproveInstallsAndReplaysCanonicalCommand(t *testing.T) {
	fake := &fakePlugins{}
	app, stdout, _ := pluginApp("yes\n", true, fake)
	if code := app.Run([]string{"content", "doctor"}); code != 3 {
		t.Fatalf("code=%d", code)
	}
	if fake.installs != 1 || fake.runs != 1 || fake.name != "content" || !reflect.DeepEqual(fake.args, []string{"doctor"}) || stdout.String() != "plugin output\n" {
		t.Fatalf("unexpected state: %#v stdout=%q", fake, stdout)
	}
}

func TestMissingContentPluginDeclineDoesNotInstall(t *testing.T) {
	fake := &fakePlugins{}
	app, stdout, stderr := pluginApp("n\n", true, fake)
	if code := app.Run([]string{"content", "doctor"}); code != ExitUnavailable {
		t.Fatalf("code=%d", code)
	}
	if fake.installs != 0 || fake.runs != 0 || stdout.String() != "" || !strings.Contains(stderr.String(), "blazn-content") {
		t.Fatalf("unexpected state: %#v stdout=%q stderr=%q", fake, stdout, stderr)
	}
}

func TestContentPluginHelpNeverInstalls(t *testing.T) {
	for _, args := range [][]string{{"content", "--help"}, {"image", "--help"}} {
		fake := &fakePlugins{}
		app, stdout, stderr := pluginApp("yes\n", true, fake)
		if code := app.Run(args); code != ExitSuccess {
			t.Fatalf("args=%v code=%d", args, code)
		}
		if fake.installs != 0 || fake.runs != 0 || !strings.Contains(stdout.String(), "signed content plugin") || stderr.String() != "" {
			t.Fatalf("args=%v unexpected state: %#v stdout=%q stderr=%q", args, fake, stdout, stderr)
		}
	}
}

func TestMissingContentPluginMachineModeNamesExactInstall(t *testing.T) {
	fake := &fakePlugins{}
	app, stdout, _ := pluginApp("yes\n", true, fake)
	if code := app.Run([]string{"image", "generate", "--output=json"}); code != ExitUnavailable {
		t.Fatalf("code=%d", code)
	}
	if fake.installs != 0 || !strings.Contains(stdout.String(), "plugins install content --yes") {
		t.Fatalf("unexpected state: %#v stdout=%q", fake, stdout)
	}
}

func pluginApp(input string, tty bool, plugins pluginCommands) (*App, *bytes.Buffer, *bytes.Buffer) {
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	app := New(stdout, stderr, testBuild)
	app.stdin, app.stdinTTY, app.plugins = strings.NewReader(input), func() bool { return tty }, plugins
	app.pluginContext = func(_ context.Context, format OutputFormat) (pluginpkg.RuntimeContext, error) {
		return pluginpkg.RuntimeContext{SchemaVersion: 1, ProtocolVersion: 1, InvocationID: strings.Repeat("a", 32), CoreVersion: testBuild.Version, OutputFormat: string(format), Status: "unavailable", ReasonCode: "workspace_context_unavailable"}, nil
	}
	return app, stdout, stderr
}

func TestMissingPluginDeclineDoesNotInstall(t *testing.T) {
	fake := &fakePlugins{}
	app, stdout, stderr := pluginApp("n\n", true, fake)
	if code := app.Run([]string{"person", "search", "Jane"}); code != ExitUnavailable {
		t.Fatalf("code=%d", code)
	}
	if fake.installs != 0 || fake.runs != 0 || stdout.String() != "" || !strings.Contains(stderr.String(), "[y/N]") {
		t.Fatalf("unexpected state: %#v stdout=%q stderr=%q", fake, stdout, stderr)
	}
}

func TestMissingPluginApproveInstallsAndReplaysAlias(t *testing.T) {
	fake := &fakePlugins{}
	app, stdout, _ := pluginApp("yes\n", true, fake)
	if code := app.Run([]string{"person", "search", "Jane"}); code != 3 {
		t.Fatalf("code=%d", code)
	}
	if fake.installs != 1 || fake.runs != 1 || !reflect.DeepEqual(fake.args, []string{"person", "search", "Jane"}) || stdout.String() != "plugin output\n" {
		t.Fatalf("unexpected state: %#v stdout=%q", fake, stdout)
	}
}

func TestSocialM2AliasesInstallAndForwardExactly(t *testing.T) {
	for _, args := range [][]string{
		{"saved-search", "list"},
		{"graph", "diff", "leadership-watch", "--from", "run-one", "--to", "run-two"},
	} {
		t.Run(args[0], func(t *testing.T) {
			fake := &fakePlugins{}
			app, stdout, _ := pluginApp("yes\n", true, fake)
			if code := app.Run(args); code != 3 {
				t.Fatalf("code=%d", code)
			}
			if fake.installs != 1 || fake.runs != 1 || fake.installName != "social" || fake.runName != "social" || !reflect.DeepEqual(fake.args, args) || stdout.String() != "plugin output\n" {
				t.Fatalf("args=%v state=%#v stdout=%q", args, fake, stdout)
			}
		})
	}
}

func TestMissingPluginJSONAndNonTTYFailClosed(t *testing.T) {
	for _, tc := range []struct {
		args []string
		tty  bool
	}{{[]string{"person", "search"}, false}, {[]string{"person", "--output=json"}, true}} {
		fake := &fakePlugins{}
		app, stdout, stderr := pluginApp("yes\n", tc.tty, fake)
		if code := app.Run(tc.args); code != ExitUnavailable {
			t.Fatalf("code=%d", code)
		}
		if fake.installs != 0 || fake.runs != 0 {
			t.Fatalf("implicit mutation occurred: %#v", fake)
		}
		if tc.tty && !strings.Contains(stdout.String(), `"code":"plugin_required"`) {
			t.Fatalf("JSON error=%q", stdout)
		}
		if !tc.tty && !strings.Contains(stderr.String(), "plugins install social --yes") {
			t.Fatalf("non-TTY error=%q", stderr)
		}
	}
}

func TestMissingPluginExtendedMachineFormatsFailClosed(t *testing.T) {
	for _, format := range []string{"jsonl", "csv"} {
		fake := &fakePlugins{}
		app, stdout, stderr := pluginApp("yes\n", true, fake)
		if code := app.Run([]string{"person", "search", "--output=" + format}); code != ExitUnavailable {
			t.Fatalf("format=%s code=%d", format, code)
		}
		if fake.installs != 0 || fake.runs != 0 {
			t.Fatalf("format=%s implicit mutation occurred: %#v", format, fake)
		}
		if stderr.String() != "" || !strings.Contains(stdout.String(), "plugin_required") {
			t.Fatalf("format=%s stdout=%q stderr=%q", format, stdout, stderr)
		}
		if format == "csv" && !strings.HasPrefix(stdout.String(), "status,error_code,error_message,exit_code\n") {
			t.Fatalf("CSV error=%q", stdout)
		}
	}
}

func TestInstalledCanonicalDispatchAndJSONForwarding(t *testing.T) {
	fake := &fakePlugins{installed: true}
	app, _, _ := pluginApp("", false, fake)
	if code := app.Run([]string{"social", "person", "search", "--output=json"}); code != 3 {
		t.Fatalf("code=%d", code)
	}
	if !reflect.DeepEqual(fake.args, []string{"person", "search"}) || fake.format != "json" {
		t.Fatalf("args=%v format=%s", fake.args, fake.format)
	}
	if fake.context.OutputFormat != "json" || fake.context.InvocationID == "" {
		t.Fatalf("runtime context=%#v", fake.context)
	}
}

func TestInstalledPluginReceivesExtendedOutputFormats(t *testing.T) {
	for _, format := range []string{"jsonl", "csv"} {
		fake := &fakePlugins{installed: true}
		app, _, _ := pluginApp("", false, fake)
		if code := app.Run([]string{"person", "search", "Jane", "--output=" + format}); code != 3 {
			t.Fatalf("format=%s code=%d", format, code)
		}
		if fake.format != format || !reflect.DeepEqual(fake.args, []string{"person", "search", "Jane"}) {
			t.Fatalf("format=%s forwarded=%s args=%v", format, fake.format, fake.args)
		}
	}
}

func TestPluginHelpNeverInstalls(t *testing.T) {
	for _, args := range [][]string{{"social", "--help"}, {"person", "--help"}} {
		fake := &fakePlugins{}
		app, stdout, stderr := pluginApp("yes\n", true, fake)
		if code := app.Run(args); code != ExitSuccess {
			t.Fatalf("args=%v code=%d", args, code)
		}
		if fake.installs != 0 || fake.runs != 0 || !strings.Contains(stdout.String(), "Commands provided by the signed social plugin") || stderr.String() != "" {
			t.Fatalf("args=%v unexpected state: %#v stdout=%q stderr=%q", args, fake, stdout, stderr)
		}
	}
}

func TestPluginCSVHelpNeverInstallsAndIsParseable(t *testing.T) {
	for _, args := range [][]string{{"social", "--help", "--output=csv"}, {"person", "--help", "--output=csv"}} {
		fake := &fakePlugins{}
		app, stdout, stderr := pluginApp("yes\n", true, fake)
		if code := app.Run(args); code != ExitSuccess {
			t.Fatalf("args=%v code=%d", args, code)
		}
		if fake.installs != 0 || fake.runs != 0 || stderr.String() != "" || !strings.HasPrefix(stdout.String(), "command,usage,summary,subcommand,subcommand_summary\n") {
			t.Fatalf("args=%v fake=%#v stdout=%q stderr=%q", args, fake, stdout, stderr)
		}
	}
}

func TestPluginCommandsRequireConfirmation(t *testing.T) {
	fake := &fakePlugins{}
	app, _, stderr := pluginApp("", false, fake)
	if code := app.Run([]string{"plugins", "install", "social"}); code != ExitUsage || !strings.Contains(stderr.String(), "requires NAME --yes") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	app, stdout, _ := pluginApp("", false, fake)
	if code := app.Run([]string{"plugins", "install", "social", "--yes", "--output=json"}); code != ExitSuccess || fake.installs != 1 || !strings.Contains(stdout.String(), `"status":"installed"`) {
		t.Fatalf("code=%d stdout=%q fake=%#v", code, stdout, fake)
	}
}

var _ pluginCommands = (*fakePlugins)(nil)
