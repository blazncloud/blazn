package cli

import "fmt"

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
