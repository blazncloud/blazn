package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

type DoctorReport struct {
	Command         string        `json:"command"`
	ContractVersion string        `json:"contractVersion"`
	Status          string        `json:"status"`
	Checks          []DoctorCheck `json:"checks"`
}

type DoctorCheck struct {
	Name        string `json:"name"`
	Severity    string `json:"severity"`
	Status      string `json:"status"`
	Message     string `json:"message"`
	Remediation string `json:"remediation"`
}

func RunDoctor(build BuildInfo) DoctorReport {
	checks := []DoctorCheck{
		platformCheck(build.GOOS),
		architectureCheck(build.GOARCH),
		buildMetadataCheck(build),
		installPathCheck(),
		configPermissionsCheck(),
		installerToolsCheck(),
		credentialStoreCheck(build.GOOS),
	}
	report := DoctorReport{
		Command:         "doctor",
		ContractVersion: build.ContractVersion,
		Status:          "ok",
		Checks:          checks,
	}
	for _, check := range checks {
		switch check.Status {
		case "fail":
			report.Status = "error"
		case "warn":
			if report.Status == "ok" {
				report.Status = "warning"
			}
		}
	}
	return report
}

func installPathCheck() DoctorCheck {
	check := DoctorCheck{Name: "install.path", Severity: "info", Status: "pass", Message: "executable path is available on PATH", Remediation: "none"}
	executable, err := os.Executable()
	if err != nil {
		check.Severity = "warning"
		check.Status = "warn"
		check.Message = "could not resolve the running executable path"
		check.Remediation = "reinstall Blazn from the signed curl installer"
		return check
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		check.Severity = "warning"
		check.Status = "warn"
		check.Message = "could not normalize the running executable path"
		check.Remediation = "reinstall Blazn into an absolute user-owned path"
		return check
	}
	runningInfo, err := os.Stat(executable)
	if err != nil {
		check.Severity = "warning"
		check.Status = "warn"
		check.Message = "could not inspect the running executable"
		check.Remediation = "reinstall Blazn from the signed curl installer"
		return check
	}
	resolved, err := exec.LookPath("blazn")
	if err == nil {
		resolvedInfo, statErr := os.Stat(resolved)
		if statErr == nil && os.SameFile(runningInfo, resolvedInfo) {
			return check
		}
	}
	directory := filepath.Dir(executable)
	check.Severity = "warning"
	check.Status = "warn"
	if resolved != "" {
		check.Message = fmt.Sprintf("PATH resolves blazn to %s instead of the running executable", resolved)
		check.Remediation = fmt.Sprintf("place %s before the shadowing directory on PATH; Blazn will not edit shell configuration", directory)
	} else {
		check.Message = fmt.Sprintf("running executable %s is not resolvable as blazn on PATH", executable)
		check.Remediation = fmt.Sprintf("add %s to PATH; Blazn will not edit shell configuration", directory)
	}
	return check
}

func configPermissionsCheck() DoctorCheck {
	check := DoctorCheck{Name: "config.permissions", Severity: "info", Status: "pass", Message: "Blazn configuration directory is absent or private", Remediation: "none"}
	base, err := os.UserConfigDir()
	if err != nil {
		check.Severity = "warning"
		check.Status = "warn"
		check.Message = "could not resolve the user configuration directory"
		check.Remediation = "set a valid user home/configuration directory before authentication"
		return check
	}
	path := filepath.Join(base, "blazn")
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return check
	}
	if err != nil {
		check.Severity = "warning"
		check.Status = "warn"
		check.Message = "could not inspect the Blazn configuration directory"
		check.Remediation = "verify ownership and permissions on the Blazn configuration directory"
		return check
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		check.Severity = "error"
		check.Status = "fail"
		check.Message = "Blazn configuration path is not a real directory"
		check.Remediation = "replace it with a private user-owned directory after preserving authorized data"
		return check
	}
	return evaluateConfigSecurity(check, info.Mode().Perm(), info.Sys(), uint32(os.Geteuid()))
}

func evaluateConfigSecurity(check DoctorCheck, permissions os.FileMode, systemInfo any, effectiveUID uint32) DoctorCheck {
	if permissions&0o077 != 0 {
		check.Severity = "warning"
		check.Status = "warn"
		check.Message = fmt.Sprintf("Blazn configuration directory permissions are %04o", permissions)
		check.Remediation = "restrict the Blazn configuration directory to mode 0700"
	}
	knownOwner, matchingOwner := configOwnerMatches(systemInfo, effectiveUID)
	if !knownOwner {
		check.Severity = "warning"
		check.Status = "warn"
		check.Message = "could not establish ownership of the Blazn configuration directory"
		check.Remediation = "verify the directory is owned by the current user before authentication"
	} else if !matchingOwner {
		check.Severity = "error"
		check.Status = "fail"
		check.Message = "Blazn configuration directory is owned by another user"
		check.Remediation = "use a private configuration directory owned by the current user"
	}
	return check
}

func configOwnerMatches(systemInfo any, effectiveUID uint32) (bool, bool) {
	stat, ok := systemInfo.(*syscall.Stat_t)
	if !ok {
		return false, false
	}
	return true, stat.Uid == effectiveUID
}

func installerToolsCheck() DoctorCheck {
	check := DoctorCheck{Name: "installer.tools", Severity: "info", Status: "pass", Message: "installer verification tools are available", Remediation: "none"}
	missing := make([]string, 0)
	for _, command := range []string{"curl", "tar", "ssh-keygen", "awk", "grep", "mktemp"} {
		if _, err := exec.LookPath(command); err != nil {
			missing = append(missing, command)
		}
	}
	if _, err := exec.LookPath("sha256sum"); err != nil {
		if _, fallbackErr := exec.LookPath("shasum"); fallbackErr != nil {
			missing = append(missing, "sha256sum or shasum")
		}
	}
	if len(missing) > 0 {
		check.Severity = "warning"
		check.Status = "warn"
		check.Message = "installer tools unavailable: " + strings.Join(missing, ", ")
		check.Remediation = "install the missing OS baseline tools before the next Blazn upgrade"
		return check
	}
	output, err := exec.Command("ssh-keygen", "-Y", "verify").CombinedOutput()
	if err != nil && (strings.Contains(string(output), "illegal option -- Y") || strings.Contains(string(output), "unknown option -- Y")) {
		check.Severity = "warning"
		check.Status = "warn"
		check.Message = "ssh-keygen is installed but does not support signed-manifest verification"
		check.Remediation = "install an OpenSSH release with ssh-keygen -Y verify support before the next Blazn upgrade"
	}
	return check
}

func credentialStoreCheck(goos string) DoctorCheck {
	check := DoctorCheck{Name: "credential_store.command", Severity: "info", Status: "pass", Message: "supported credential-store command is installed; unlock and service availability are checked during authentication", Remediation: "none"}
	var command string
	switch goos {
	case "darwin":
		command = "/usr/bin/security"
	case "linux":
		command = "secret-tool"
	default:
		check.Severity = "warning"
		check.Status = "warn"
		check.Message = "credential-store command is unknown on this platform"
		check.Remediation = "use a supported macOS or Linux platform"
		return check
	}
	if _, err := exec.LookPath(command); err != nil {
		if goos == "linux" {
			check.Message = "Secret Service is unavailable; authentication will use the built-in protected credential file"
			return check
		}
		check.Severity = "warning"
		check.Status = "warn"
		check.Message = "supported credential-store command is unavailable"
		check.Remediation = "install or unlock the supported credential store"
	}
	return check
}

func platformCheck(goos string) DoctorCheck {
	check := DoctorCheck{Name: "runtime.os", Severity: "info", Status: "pass", Message: fmt.Sprintf("operating system %s is supported", goos), Remediation: "none"}
	if goos != "darwin" && goos != "linux" {
		check.Severity = "error"
		check.Status = "fail"
		check.Message = fmt.Sprintf("operating system %s is not supported", goos)
		check.Remediation = "run blazn on a supported macOS or Linux system"
	}
	return check
}

func architectureCheck(goarch string) DoctorCheck {
	check := DoctorCheck{Name: "runtime.architecture", Severity: "info", Status: "pass", Message: fmt.Sprintf("architecture %s is supported", goarch), Remediation: "none"}
	if goarch != "amd64" && goarch != "arm64" {
		check.Severity = "error"
		check.Status = "fail"
		check.Message = fmt.Sprintf("architecture %s is not supported", goarch)
		check.Remediation = "install blazn on a supported amd64 or arm64 system"
	}
	return check
}

func buildMetadataCheck(build BuildInfo) DoctorCheck {
	check := DoctorCheck{Name: "build.metadata", Severity: "info", Status: "pass", Message: "build metadata is present", Remediation: "none"}
	if build.Version == "" || build.Version == "dev" || build.Commit == "" || build.Commit == "unknown" || build.BuildTime == "" || build.BuildTime == "unknown" {
		check.Severity = "warning"
		check.Status = "warn"
		check.Message = "binary contains development build metadata"
		check.Remediation = "install a signed Blazn release for qualification or production use"
	}
	return check
}

func (a *App) writeDoctor(format OutputFormat) int {
	report := a.doctor()
	if format == OutputJSON {
		if result := a.writeJSON(report); result != ExitSuccess {
			return result
		}
	} else {
		fmt.Fprintf(a.stdout, "Blazn doctor: %s\n", report.Status)
		for _, check := range report.Checks {
			fmt.Fprintf(a.stdout, "[%s] %s (%s): %s\n", check.Status, check.Name, check.Severity, check.Message)
			fmt.Fprintf(a.stdout, "  remediation: %s\n", check.Remediation)
		}
	}
	if report.Status == "error" {
		return ExitUnavailable
	}
	return ExitSuccess
}
