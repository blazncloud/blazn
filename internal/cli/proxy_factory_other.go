//go:build !darwin && !linux

package cli

import (
	"io"

	"github.com/blazncloud/blazn/internal/proxy/activation"
)

type appProcessIO struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

func newDefaultProxyCommands(appProcessIO) (proxyCommands, error) {
	return nil, activation.ErrUnavailable
}
