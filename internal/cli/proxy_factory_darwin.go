//go:build darwin

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

// Darwin keeps the stable scoped runner and session-unsupported behavior until
// descriptor-backed child spawn and launchctl inheritance proof are available.
func newDefaultProxyCommands(streams appProcessIO) (proxyCommands, error) {
	return scopedrun.NewProduction(streams.stdin, streams.stdout, streams.stderr)
}
