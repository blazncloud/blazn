package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/blazncloud/blazn/internal/sandboxio"
)

const (
	bootstrapDirectory = "/run/blazn-bootstrap"
	bootstrapMarker    = "validated"
	bootstrapReceipt   = "materialization.json"
	artifactDirectory  = "/workspace/artifacts"
)

func main() { os.Exit(run(os.Args, os.Stdin, os.Stdout)) }

func run(arguments []string, input *os.File, output *os.File) int {
	if len(arguments) < 2 || input == nil || output == nil {
		fmt.Fprintln(os.Stderr, "sandbox I/O invocation is invalid")
		return 2
	}
	if arguments[1] != "access-upload" && arguments[1] != "access-download" && len(arguments) != 2 {
		fmt.Fprintln(os.Stderr, "sandbox I/O invocation is invalid")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch arguments[1] {
	case "bootstrap":
		ctx, cancel := context.WithTimeout(ctx, sandboxio.SourceTimeout)
		defer cancel()
		state := sandboxio.FileBootstrapState{Directory: bootstrapDirectory, ReceiptName: bootstrapReceipt, MarkerName: bootstrapMarker}
		if err := sandboxio.ServeBootstrap(ctx, input, output, sandboxio.GitMaterializer{Fetcher: sandboxio.SecureGitFetcher{}}, state); err != nil {
			return 1
		}
		return 0
	case "release":
		ctx, cancel := context.WithTimeout(ctx, sandboxio.DefaultTimeout)
		defer cancel()
		state := sandboxio.FileBootstrapState{Directory: bootstrapDirectory, ReceiptName: bootstrapReceipt, MarkerName: bootstrapMarker}
		if err := sandboxio.ServeRelease(ctx, input, output, state); err != nil {
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
	case "wait-access":
		if len(arguments) != 2 || sandboxio.WaitForSignal(ctx) != nil {
			return 1
		}
		return 0
	case "access-upload":
		if len(arguments) != 5 {
			return 2
		}
		size, err := strconv.ParseInt(arguments[3], 10, 64)
		if err != nil {
			return 2
		}
		root, err := sandboxio.OpenRootFileSystem("/workspace")
		if err != nil {
			return 1
		}
		defer root.Close()
		if err := sandboxio.WriteWorkspaceFile(root, arguments[2], arguments[4], size, input); err != nil {
			return 1
		}
		return 0
	case "access-download":
		if len(arguments) != 3 {
			return 2
		}
		root, err := sandboxio.OpenRootFileSystem("/workspace")
		if err != nil {
			return 1
		}
		defer root.Close()
		file, err := sandboxio.ReadWorkspaceFile(root, arguments[2])
		if err != nil {
			return 1
		}
		if _, err := output.Write(file.Body); err != nil {
			return 1
		}
		return 0
	default:
		fmt.Fprintln(os.Stderr, "sandbox I/O operation is unsupported")
		return 2
	}
}
