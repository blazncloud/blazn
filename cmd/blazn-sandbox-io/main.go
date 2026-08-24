package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/blazncloud/blazn/internal/sandboxio"
)

const (
	bootstrapDirectory = "/run/blazn-bootstrap"
	bootstrapMarker    = "validated"
	artifactDirectory = "/workspace/artifacts"
)

func main() { os.Exit(run(os.Args, os.Stdin, os.Stdout)) }

func run(arguments []string, input *os.File, output *os.File) int {
	if len(arguments) != 2 || input == nil || output == nil {
		fmt.Fprintln(os.Stderr, "sandbox I/O invocation is invalid")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch arguments[1] {
	case "bootstrap":
		ctx, cancel := context.WithTimeout(ctx, sandboxio.DefaultTimeout)
		defer cancel()
		if err := sandboxio.ServeBootstrap(ctx, input, output, sandboxio.FileCompleter{Directory: bootstrapDirectory, Name: bootstrapMarker}); err != nil {
			return 1
		}
		return 0
	case "wait-bootstrap":
		if err := sandboxio.WaitForBootstrap(ctx, bootstrapDirectory, bootstrapMarker, 100*time.Millisecond); err != nil {
			return 1
		}
		return 0
	case "artifact":
		fileSystem, err := sandboxio.OpenRootFileSystem(artifactDirectory)
		if err != nil {
			return 1
		}
		defer fileSystem.Close()
		ctx, cancel := context.WithTimeout(ctx, sandboxio.DefaultTimeout)
		defer cancel()
		if err := sandboxio.ServeArtifact(ctx, input, output, fileSystem); err != nil {
			return 1
		}
		return 0
	case "wait-artifact":
		if err := sandboxio.WaitForSignal(ctx); err != nil {
			return 1
		}
		return 0
	default:
		fmt.Fprintln(os.Stderr, "sandbox I/O operation is unsupported")
		return 2
	}
}
