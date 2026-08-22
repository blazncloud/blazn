package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

var testBuild = BuildInfo{
	Version:         "1.2.3",
	Commit:          "abc123",
	BuildTime:       "2026-08-21T12:00:00Z",
	GOOS:            "linux",
	GOARCH:          "amd64",
	ContractVersion: "v1alpha1",
}

func runApp(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := New(&stdout, &stderr, testBuild)
	code := app.Run(args)
	return code, stdout.String(), stderr.String()
}

func TestRootAndExplicitHelp(t *testing.T) {
	for _, args := range [][]string{nil, {"help"}, {"--help"}} {
		code, stdout, stderr := runApp(t, args...)
		if code != ExitSuccess {
			t.Fatalf("Run(%v) code = %d, want %d", args, code, ExitSuccess)
		}
		if !strings.Contains(stdout, "Usage:") || !strings.Contains(stdout, "doctor") || !strings.Contains(stdout, "version") {
			t.Fatalf("Run(%v) output missing help content: %q", args, stdout)
		}
		if stderr != "" {
			t.Fatalf("Run(%v) stderr = %q", args, stderr)
		}
	}
}

func TestJSONHelpIsDeterministic(t *testing.T) {
	code, first, stderr := runApp(t, "--output", "json", "help")
	if code != ExitSuccess || stderr != "" {
		t.Fatalf("first run code=%d stderr=%q", code, stderr)
	}
	code, second, _ := runApp(t, "help", "--output=json")
	if code != ExitSuccess || first != second {
		t.Fatalf("JSON help differs:\n%s\n%s", first, second)
	}
	const want = "{\"command\":\"blazn\",\"usage\":\"blazn [--output human|json] <command>\",\"summary\":\"Control Blazn from the command line.\",\"commands\":[{\"name\":\"doctor\",\"summary\":\"Run offline readiness checks\"},{\"name\":\"help\",\"summary\":\"Show help for a command\"},{\"name\":\"version\",\"summary\":\"Show build and contract version information\"}]}\n"
	if first != want {
		t.Fatalf("JSON help = %q, want %q", first, want)
	}
}

func TestUnknownCommandHumanAndJSON(t *testing.T) {
	code, stdout, stderr := runApp(t, "missing")
	if code != ExitUsage || stdout != "" || !strings.Contains(stderr, `unknown command "missing"`) {
		t.Fatalf("human unknown: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	code, stdout, stderr = runApp(t, "missing", "--output", "json")
	if code != ExitUsage || stderr != "" {
		t.Fatalf("JSON unknown: code=%d stderr=%q", code, stderr)
	}
	const want = "{\"error\":{\"code\":\"unknown_command\",\"message\":\"unknown command \\\"missing\\\"\"},\"exitCode\":2}\n"
	if stdout != want {
		t.Fatalf("JSON unknown = %q, want %q", stdout, want)
	}
}

func TestInvalidOutputIsUsageError(t *testing.T) {
	code, _, stderr := runApp(t, "--output", "yaml", "version")
	if code != ExitUsage || !strings.Contains(stderr, "expected human or json") {
		t.Fatalf("invalid output: code=%d stderr=%q", code, stderr)
	}
}

func TestVersionJSON(t *testing.T) {
	code, stdout, stderr := runApp(t, "version", "--output=json")
	if code != ExitSuccess || stderr != "" {
		t.Fatalf("version: code=%d stderr=%q", code, stderr)
	}
	var got BuildInfo
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("decode version: %v", err)
	}
	if got != testBuild {
		t.Fatalf("version = %#v, want %#v", got, testBuild)
	}
}

func TestCommandHelpAndExtraArguments(t *testing.T) {
	code, stdout, _ := runApp(t, "doctor", "--help")
	if code != ExitSuccess || !strings.Contains(stdout, "Run deterministic checks") {
		t.Fatalf("doctor help: code=%d stdout=%q", code, stdout)
	}
	code, _, stderr := runApp(t, "version", "extra")
	if code != ExitUsage || !strings.Contains(stderr, "does not accept arguments") {
		t.Fatalf("version extra: code=%d stderr=%q", code, stderr)
	}
}

func TestDoctorJSONAndExitCodes(t *testing.T) {
	code, stdout, stderr := runApp(t, "doctor", "--output", "json")
	if code != ExitSuccess || stderr != "" {
		t.Fatalf("doctor: code=%d stderr=%q", code, stderr)
	}
	var report DoctorReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("decode doctor: %v", err)
	}
	if report.Status != "ok" || len(report.Checks) != 3 {
		t.Fatalf("doctor report = %#v", report)
	}
	wantNames := []string{"runtime.os", "runtime.architecture", "build.metadata"}
	for i, name := range wantNames {
		if report.Checks[i].Name != name || report.Checks[i].Severity == "" || report.Checks[i].Status == "" || report.Checks[i].Remediation == "" {
			t.Fatalf("doctor check %d = %#v", i, report.Checks[i])
		}
	}

	var failOut bytes.Buffer
	var failErr bytes.Buffer
	app := New(&failOut, &failErr, testBuild)
	app.doctor = func() DoctorReport {
		return DoctorReport{Command: "doctor", ContractVersion: "v1alpha1", Status: "error", Checks: []DoctorCheck{{Name: "runtime.os", Severity: "error", Status: "fail", Message: "unsupported", Remediation: "use Linux"}}}
	}
	if got := app.Run([]string{"doctor", "--output=json"}); got != ExitUnavailable {
		t.Fatalf("failed doctor exit = %d, want %d", got, ExitUnavailable)
	}
}

func TestExitCodeConstants(t *testing.T) {
	if ExitSuccess != 0 || ExitFailure != 1 || ExitUsage != 2 || ExitUnavailable != 7 {
		t.Fatalf("exit codes changed: %d %d %d %d", ExitSuccess, ExitFailure, ExitUsage, ExitUnavailable)
	}
}
