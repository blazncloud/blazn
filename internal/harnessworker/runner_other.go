//go:build !linux && !darwin

package harnessworker

import (
	"context"
	"errors"
)

func (ExecProcessRunner) Run(context.Context, ProcessSpec) (ProcessResult, error) {
	return ProcessResult{}, errors.New("sandbox process-tree execution is unsupported on this platform")
}
