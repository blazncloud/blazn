//go:build windows

package node

import (
	"errors"
	"os"
)

func fileOwner(os.FileInfo) (int64, uint64, bool) { return 0, 1, true }
func ensurePrivateDirectory(string, int64) error {
	return errors.New("privileged Node state is unsupported on Windows")
}
func lockInstallFile(string) (func(), error) {
	return nil, errors.New("privileged Node install locking is unsupported on Windows")
}
