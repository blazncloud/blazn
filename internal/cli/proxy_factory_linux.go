//go:build linux

package cli

import (
	"io"

	"github.com/blazncloud/blazn/internal/proxy/scopedrun"
	"github.com/blazncloud/blazn/internal/proxy/session"
)

type appProcessIO struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

func newDefaultProxyCommands(streams appProcessIO) (proxyCommands, error) {
	scoped, err := scopedrun.NewProduction(streams.stdin, streams.stdout, streams.stderr)
	if err != nil {
		return nil, err
	}
	return session.NewProduction(scoped)
}
