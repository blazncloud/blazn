package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
	const want = "{\"command\":\"blazn\",\"usage\":\"blazn [--output human|json] <command>\",\"summary\":\"Control Blazn from the command line.\",\"commands\":[{\"name\":\"auth\",\"summary\":\"Authenticate this device and manage sessions\"},{\"name\":\"content\",\"summary\":\"Manage Content media workflows (plugin)\"},{\"name\":\"doctor\",\"summary\":\"Run offline readiness checks\"},{\"name\":\"help\",\"summary\":\"Show help for a command\"},{\"name\":\"node\",\"summary\":\"Enroll, install, recover, and heartbeat a Node\"},{\"name\":\"plugins\",\"summary\":\"Install and manage signed Blazn plugins\"},{\"name\":\"social\",\"summary\":\"Search public entities and manage social content (plugin)\"},{\"name\":\"uninstall\",\"summary\":\"Remove a receipt-owned direct installation\"},{\"name\":\"version\",\"summary\":\"Show build and contract version information\"},{\"name\":\"workspace\",\"summary\":\"Create, select, and manage workspaces\"}]}\n"
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
	if code != ExitUsage || !strings.Contains(stderr, "expected human, json, jsonl, or csv") {
		t.Fatalf("invalid output: code=%d stderr=%q", code, stderr)
	}
}

func TestExtendedOutputIsRejectedForCoreCommands(t *testing.T) {
	for _, format := range []string{"jsonl", "csv"} {
		code, _, stderr := runApp(t, "version", "--output", format)
		if code != ExitUsage || !strings.Contains(stderr, "supported only by plugin commands") {
			t.Fatalf("format=%s code=%d stderr=%q", format, code, stderr)
		}
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
	if (report.Status != "ok" && report.Status != "warning") || len(report.Checks) != 7 {
		t.Fatalf("doctor report = %#v", report)
	}
	wantNames := []string{"runtime.os", "runtime.architecture", "build.metadata", "install.path", "config.permissions", "installer.tools", "credential_store.command"}
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
	if ExitSuccess != 0 || ExitFailure != 1 || ExitUsage != 2 || ExitUnavailable != 7 || ExitPartial != 9 {
		t.Fatalf("exit codes changed: %d %d %d %d %d", ExitSuccess, ExitFailure, ExitUsage, ExitUnavailable, ExitPartial)
	}
}

func TestUninstallRequiresConfirmation(t *testing.T) {
	code, _, stderr := runApp(t, "uninstall")
	if code != ExitUsage || !strings.Contains(stderr, "requires --yes") {
		t.Fatalf("uninstall without confirmation: code=%d stderr=%q", code, stderr)
	}
}

func TestUninstallCommandJSON(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := New(&stdout, &stderr, testBuild)
	app.uninstall = func() (UninstallResult, error) {
		return UninstallResult{Command: "uninstall", Status: "removed", Path: "/tmp/bin/blazn", ConfigPreserved: true}, nil
	}
	if code := app.Run([]string{"uninstall", "--yes", "--output=json"}); code != ExitSuccess {
		t.Fatalf("uninstall code=%d stderr=%q", code, stderr.String())
	}
	const want = "{\"command\":\"uninstall\",\"status\":\"removed\",\"path\":\"/tmp/bin/blazn\",\"configPreserved\":true}\n"
	if stdout.String() != want {
		t.Fatalf("uninstall JSON=%q want=%q", stdout.String(), want)
	}
}

func TestUninstallCommandReportsRemovedWithResidue(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := New(&stdout, &stderr, testBuild)
	app.uninstall = func() (UninstallResult, error) {
		return UninstallResult{Command: "uninstall", Status: "removed_with_residue", Path: "/tmp/bin/blazn", ConfigPreserved: true, Residues: []string{"/tmp/bin/.receipt.removing"}}, nil
	}
	if code := app.Run([]string{"uninstall", "--yes", "--output=json"}); code != ExitFailure {
		t.Fatalf("partial uninstall code=%d want=%d", code, ExitFailure)
	}
	if !strings.Contains(stdout.String(), `"status":"removed_with_residue"`) || !strings.Contains(stdout.String(), `"residues":["/tmp/bin/.receipt.removing"]`) {
		t.Fatalf("partial uninstall JSON=%q", stdout.String())
	}
}

func TestRunUninstallAtRemovesOnlyReceiptOwnedFiles(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "blazn")
	content := []byte("standalone-binary")
	if err := os.WriteFile(executable, content, 0o755); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	receipt := filepath.Join(dir, installReceiptName)
	receiptBody := fmt.Sprintf("version=v1.2.3\nbinary_sha256=%x\n", digest)
	if err := os.WriteFile(receipt, []byte(receiptBody), 0o644); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "config-preserved")
	if err := os.WriteFile(config, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := runUninstallAt(executable)
	if err != nil {
		t.Fatalf("runUninstallAt: %v", err)
	}
	if result.Status != "removed" || !result.ConfigPreserved {
		t.Fatalf("result=%#v", result)
	}
	for _, removed := range []string{executable, receipt} {
		if _, err := os.Stat(removed); !os.IsNotExist(err) {
			t.Fatalf("expected %s removed, err=%v", removed, err)
		}
	}
	if got, err := os.ReadFile(config); err != nil || string(got) != "keep" {
		t.Fatalf("config changed: %q err=%v", got, err)
	}
}

func TestRunUninstallAtRefusesModifiedBinary(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "blazn")
	if err := os.WriteFile(executable, []byte("modified"), 0o755); err != nil {
		t.Fatal(err)
	}
	receipt := filepath.Join(dir, installReceiptName)
	if err := os.WriteFile(receipt, []byte("version=v1.2.3\nbinary_sha256="+strings.Repeat("0", 64)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := runUninstallAt(executable); err == nil || !strings.Contains(err.Error(), "differs from its receipt") {
		t.Fatalf("expected checksum refusal, got %v", err)
	}
	if _, err := os.Stat(executable); err != nil {
		t.Fatalf("modified binary was removed: %v", err)
	}
	if _, err := os.Stat(receipt); err != nil {
		t.Fatalf("receipt was removed: %v", err)
	}
}

func TestRunUninstallAtReportsReceiptResidueAfterBinaryRemoval(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "blazn")
	content := []byte("standalone-binary")
	if err := os.WriteFile(executable, content, 0o755); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	receipt := filepath.Join(dir, installReceiptName)
	if err := os.WriteFile(receipt, []byte(fmt.Sprintf("version=v1.2.3\nbinary_sha256=%x\n", digest)), 0o644); err != nil {
		t.Fatal(err)
	}

	ops := defaultUninstallOps
	ops.remove = func(path string) error {
		if filepath.Base(path) == ".blazn-uninstall-receipt" {
			return errors.New("injected receipt cleanup failure")
		}
		return os.Remove(path)
	}
	result, err := runUninstallAtWithOps(executable, ops)
	if err != nil {
		t.Fatalf("unexpected error after material uninstall: %v", err)
	}
	if result.Status != "removed_with_residue" || len(result.Residues) != 1 {
		t.Fatalf("result=%#v", result)
	}
	if _, err := os.Stat(executable); !os.IsNotExist(err) {
		t.Fatalf("binary should be removed, err=%v", err)
	}
	if _, err := os.Stat(result.Residues[0]); err != nil {
		t.Fatalf("expected staged receipt residue: %v", err)
	}
}

func TestRunUninstallAtHonorsLifecycleLock(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "blazn")
	content := []byte("standalone-binary")
	if err := os.WriteFile(executable, content, 0o755); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	if err := os.WriteFile(filepath.Join(dir, installReceiptName), []byte(fmt.Sprintf("version=v1.2.3\nbinary_sha256=%x\n", digest)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, ".blazn-install.lock"), 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := runUninstallAt(executable); err == nil || !strings.Contains(err.Error(), "another or stale Blazn install or uninstall") {
		t.Fatalf("expected lifecycle lock refusal, got %v", err)
	}
	if _, err := os.Stat(executable); err != nil {
		t.Fatalf("locked binary changed: %v", err)
	}
}

func TestRunUninstallAtReportsFailedWithResidueWhenRestoreFails(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "blazn")
	content := []byte("standalone-binary")
	if err := os.WriteFile(executable, content, 0o755); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	receipt := filepath.Join(dir, installReceiptName)
	if err := os.WriteFile(receipt, []byte(fmt.Sprintf("version=v1.2.3\nbinary_sha256=%x\n", digest)), 0o644); err != nil {
		t.Fatal(err)
	}

	ops := defaultUninstallOps
	stageComplete := false
	ops.rename = func(oldPath, newPath string) error {
		if filepath.Base(oldPath) == ".blazn-uninstall-receipt" && stageComplete {
			return errors.New("injected receipt restore failure")
		}
		err := os.Rename(oldPath, newPath)
		if err == nil && oldPath == receipt {
			stageComplete = true
		}
		return err
	}
	ops.remove = func(path string) error {
		if path == executable {
			return errors.New("injected executable removal failure")
		}
		return os.Remove(path)
	}
	result, err := runUninstallAtWithOps(executable, ops)
	if err != nil {
		t.Fatalf("expected structured residue result, got %v", err)
	}
	if result.Status != "failed_with_residue" || len(result.Residues) != 1 {
		t.Fatalf("result=%#v", result)
	}
	if _, err := os.Stat(executable); err != nil {
		t.Fatalf("binary should remain after failed removal: %v", err)
	}
	if _, err := os.Stat(result.Residues[0]); err != nil {
		t.Fatalf("staged receipt residue missing: %v", err)
	}
}

func TestRunUninstallAtPreservesMultipleResidues(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "blazn")
	content := []byte("standalone-binary")
	if err := os.WriteFile(executable, content, 0o755); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	receipt := filepath.Join(dir, installReceiptName)
	if err := os.WriteFile(receipt, []byte(fmt.Sprintf("version=v1.2.3\nbinary_sha256=%x\n", digest)), 0o644); err != nil {
		t.Fatal(err)
	}

	ops := defaultUninstallOps
	ops.remove = func(path string) error {
		base := filepath.Base(path)
		switch {
		case base == ".blazn-uninstall-receipt", base == ".blazn-install.journal", base == ".blazn-install.lock", strings.HasPrefix(base, ".blazn-lock-owner."):
			return errors.New("injected cleanup failure")
		default:
			return os.Remove(path)
		}
	}
	result, err := runUninstallAtWithOps(executable, ops)
	if err != nil {
		t.Fatalf("expected structured residue result, got %v", err)
	}
	if result.Status != "removed_with_residue" || len(result.Residues) != 4 {
		t.Fatalf("result=%#v", result)
	}
	want := map[string]bool{".blazn-uninstall-receipt": false, ".blazn-install.journal": false, ".blazn-install.lock": false, "candidate": false}
	for _, residue := range result.Residues {
		base := filepath.Base(residue)
		if strings.HasPrefix(base, ".blazn-lock-owner.") {
			want["candidate"] = true
		} else if _, ok := want[base]; ok {
			want[base] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Fatalf("missing residue %s in %v", name, result.Residues)
		}
	}
}

func TestInstallPathCheckDetectsShadowingBinary(t *testing.T) {
	dir := t.TempDir()
	shadow := filepath.Join(dir, "blazn")
	if err := os.WriteFile(shadow, []byte("shadow"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	check := installPathCheck()
	if check.Status != "warn" || !strings.Contains(check.Message, "instead of the running executable") {
		t.Fatalf("check=%#v", check)
	}
}

func TestConfigOwnerMatches(t *testing.T) {
	known, matches := configOwnerMatches(&syscall.Stat_t{Uid: 1001}, 1001)
	if !known || !matches {
		t.Fatalf("matching owner: known=%v matches=%v", known, matches)
	}
	known, matches = configOwnerMatches(&syscall.Stat_t{Uid: 1002}, 1001)
	if !known || matches {
		t.Fatalf("different owner: known=%v matches=%v", known, matches)
	}
	known, matches = configOwnerMatches(struct{}{}, 1001)
	if known || matches {
		t.Fatalf("unknown metadata: known=%v matches=%v", known, matches)
	}
}

func TestConfigOwnershipFailureOverridesPermissiveModeWarning(t *testing.T) {
	base := DoctorCheck{Name: "config.permissions", Severity: "info", Status: "pass", Message: "private", Remediation: "none"}
	check := evaluateConfigSecurity(base, 0o755, &syscall.Stat_t{Uid: 2000}, 1000)
	if check.Status != "fail" || check.Severity != "error" || !strings.Contains(check.Message, "owned by another user") {
		t.Fatalf("check=%#v", check)
	}
}
