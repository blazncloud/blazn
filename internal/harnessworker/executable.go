package harnessworker

import (
	"context"
	"errors"
	"path/filepath"
)

// ExecutableVerifier binds a reviewed executable path to the content digest in
// the workload scope before process launch.
type ExecutableVerifier interface {
	VerifyExecutable(context.Context, string, string) error
}

// ProtectedExecutableVerifier verifies an executable beneath an independently
// trusted directory. TrustedRoot must itself be protected by the deployment.
type ProtectedExecutableVerifier struct {
	TrustedRoot      string
	RequiredOwnerUID int
}

func (v ProtectedExecutableVerifier) VerifyExecutable(ctx context.Context, name, expectedDigest string) error {
	if err := ctx.Err(); err != nil {
		return protocolError("executable_verification_cancelled")
	}
	if v.TrustedRoot == "" || !filepath.IsAbs(v.TrustedRoot) || filepath.Clean(v.TrustedRoot) != v.TrustedRoot ||
		name == "" || !filepath.IsAbs(name) || filepath.Clean(name) != name || !digestPattern.MatchString(expectedDigest) {
		return protocolError("harness_executable_untrusted")
	}
	relative, err := filepath.Rel(v.TrustedRoot, name)
	if err != nil || relative == "." || relative == ".." || filepath.IsAbs(relative) || len(relative) >= 3 && relative[:3] == ".."+string(filepath.Separator) {
		return protocolError("harness_executable_untrusted")
	}
	if v.RequiredOwnerUID < 0 {
		return protocolError("harness_executable_untrusted")
	}
	if err := verifyProtectedExecutable(ctx, v.TrustedRoot, name, expectedDigest, v.RequiredOwnerUID); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return protocolError("executable_verification_cancelled")
		}
		return protocolError("harness_executable_untrusted")
	}
	return nil
}

var _ ExecutableVerifier = ProtectedExecutableVerifier{}
