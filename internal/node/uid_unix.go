//go:build !windows

package node

import "os"

func currentUID() int64 { return int64(os.Getuid()) }
