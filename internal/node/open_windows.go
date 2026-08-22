//go:build windows

package node

import "os"

func openNoFollow(path string) (*os.File, error) { return os.Open(path) }
