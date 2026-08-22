package node

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const RootPrepareStateSubcommand = "node-root-helper-init"

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

func prepareProductionServiceState(ctx context.Context, expected, binary string) error {
	paths, err := HostProductionNodePaths()
	if err != nil || paths.ServiceStateRoot != expected {
		return errors.New("production service state path is invalid")
	}
	if !filepath.IsAbs(binary) || filepath.Clean(binary) != binary {
		return errors.New("node binary path is invalid")
	}
	command := exec.CommandContext(ctx, "/usr/bin/sudo", binary, RootPrepareStateSubcommand)
	command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := command.Run(); err != nil {
		return errors.New("prepare node service state failed")
	}
	return nil
}

func PrepareProductionServiceState() error {
	if currentUID() != 0 {
		return errors.New("service-state preparation requires UID 0")
	}
	uid, uidErr := strconv.Atoi(os.Getenv("SUDO_UID"))
	gid, gidErr := strconv.Atoi(os.Getenv("SUDO_GID"))
	if uidErr != nil || gidErr != nil || uid <= 0 || gid <= 0 {
		return errors.New("service-state preparation requires an authenticated sudo caller")
	}
	paths, err := HostProductionNodePaths()
	if err != nil {
		return err
	}
	if err := ensurePrivateDirectory(paths.ServiceStateRoot, 0); err != nil {
		return err
	}
	if err := os.Chown(paths.ServiceStateRoot, uid, gid); err != nil {
		return err
	}
	if err := os.Chmod(paths.ServiceStateRoot, 0700); err != nil {
		return err
	}
	source, err := os.Executable()
	if err != nil {
		return err
	}
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		return err
	}
	value, err := readBoundedRegular(source, 512<<20)
	if err != nil {
		return err
	}
	if existing, readErr := readBoundedRegular(defaultRootBinaryPath, 512<<20); readErr == nil {
		if !bytes.Equal(existing, value) {
			return errors.New("system Blazn binary differs from the authenticated installer")
		}
		return nil
	} else if !errors.Is(readErr, os.ErrNotExist) && !strings.Contains(readErr.Error(), "material path is unsafe") {
		return readErr
	}
	return writeRootAtomic(defaultRootBinaryPath, value, 0755, 0, 0)
}
