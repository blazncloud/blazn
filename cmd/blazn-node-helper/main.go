package main

import (
	"context"
	"fmt"
	nodepkg "github.com/KingJammin/blazn/internal/node"
	"os"
)

func main() {
	if err := nodepkg.RunProductionRootHelper(context.Background(), os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "node root helper failed")
		os.Exit(1)
	}
}
