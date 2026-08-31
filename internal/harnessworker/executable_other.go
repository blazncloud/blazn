//go:build !linux && !darwin

package harnessworker

import (
	"context"
	"errors"
)

func verifyProtectedExecutable(context.Context, string, string, string, int) error {
	return errors.New("protected executable verification is unsupported on this platform")
}
