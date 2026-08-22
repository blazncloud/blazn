package main

import (
	"context"
	"fmt"
	"os"
	"runtime"

	nodepkg "github.com/KingJammin/blazn/internal/node"
)

func main() {
	platform := ""
	switch runtime.GOOS {
	case "linux":
		platform = "linux"
	case "darwin":
		platform = "macos"
	default:
		fmt.Fprintln(os.Stderr, "unsupported root helper platform")
		os.Exit(1)
	}
	engine := nodepkg.NativeRootEngine{Platform: platform, Commands: nodepkg.FixedCommandExecutor{}}
	if err := nodepkg.RunRootHelper(context.Background(), os.Stdin, os.Stdout, engine); err != nil {
		fmt.Fprintln(os.Stderr, "node root helper failed")
		os.Exit(1)
	}
}
