package node

import (
	"context"
	"io"
)

func RunProductionRootHelper(ctx context.Context, input io.Reader, output io.Writer) error {
	paths, err := HostProductionNodePaths()
	if err != nil {
		return err
	}
	platform := "linux"
	if paths.RootStateRoot == MacOSNodeRootStateRoot {
		platform = "macos"
	}
	engine := NativeRootEngine{
		Platform:          platform,
		Commands:          FixedCommandExecutor{},
		AuthorityPath:     paths.InstallAuthorityPath(),
		ProfileRoot:       paths.ProfileRoot,
		CurrentBinaryPath: defaultRootBinaryPath,
	}
	return RunRootHelper(ctx, input, output, engine)
}
