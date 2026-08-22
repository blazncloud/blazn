//go:build windows

package sandbox

import (
	"errors"
	"os"
)

func openRegularNoFollow(path string) (*os.File, os.FileInfo, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, nil, errors.New("source must be a regular file and not a symlink")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	after, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, nil, err
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		file.Close()
		return nil, nil, errors.New("source changed while it was opened")
	}
	return file, after, nil
}
