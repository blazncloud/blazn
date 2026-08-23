//go:build darwin || linux

package cli

import (
	"io"

	"github.com/blazncloud/blazn/internal/proxy/scopedrun"
)

type appProcessIO struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

func newDefaultProxyCommands(streams appProcessIO) (proxyCommands, error) {
	return scopedrun.NewProduction(streams.stdin, streams.stdout, streams.stderr)
}
