//go:build linux

package process

import (
	"os"

	"github.com/blazncloud/blazn/internal/proxy/state"
)

func defaultRuntimeFactory() RuntimeFactory {
	paths, err := state.AccountPaths("linux", os.Getuid())
	if err != nil {
		return unavailableFactory{}
	}
	return ResolvedRuntimeFactory{Platform: NewUnixPlatform(), ControlDirectory: paths.Root}
}
