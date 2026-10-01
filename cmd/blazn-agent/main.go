// Command blazn-agent is the Blazn reference agent harness. It runs inside a
// Sandbox; see internal/sandboxagent for the file protocol.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/blazncloud/blazn/internal/sandboxagent"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(arguments []string, stdout, stderr io.Writer) int {
	if len(arguments) == 0 {
		fmt.Fprintln(stderr, "usage: blazn-agent version|init|start|serve|wait [--dir DIRECTORY]")
		return 2
	}
	flags := flag.NewFlagSet(arguments[0], flag.ContinueOnError)
	flags.SetOutput(stderr)
	directory := flags.String("dir", sandboxagent.DefaultDirectory, "agent state directory")
	after := flags.Int64("after", 0, "return events after this sequence number")
	timeout := flags.Int("timeout", 20, "seconds to wait for a new event")
	if err := flags.Parse(arguments[1:]); err != nil || flags.NArg() != 0 {
		return 2
	}
	switch arguments[0] {
	case "version":
		return emit(stdout, map[string]any{"version": sandboxagent.Version, "schemaVersion": sandboxagent.SchemaVersion})
	case "init":
		if err := sandboxagent.Init(*directory); err != nil {
			return fail(stderr, err)
		}
		return emit(stdout, map[string]any{"directory": *directory})
	case "start":
		executable, err := os.Executable()
		if err != nil {
			return fail(stderr, err)
		}
		pid, err := sandboxagent.Start(*directory, executable)
		if err != nil {
			return fail(stderr, err)
		}
		return emit(stdout, map[string]any{"pid": pid, "version": sandboxagent.Version})
	case "serve":
		agent, err := sandboxagent.New(*directory, sandboxagent.Options{})
		if err != nil {
			return fail(stderr, err)
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := agent.Serve(ctx); err != nil {
			return fail(stderr, err)
		}
		return 0
	case "wait":
		if *after < 0 || *timeout < 0 || *timeout > 50 {
			fmt.Fprintln(stderr, "wait needs --after >= 0 and --timeout 0..50")
			return 2
		}
		if err := sandboxagent.Wait(*directory, *after, time.Duration(*timeout)*time.Second, stdout); err != nil {
			return fail(stderr, err)
		}
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", arguments[0])
		return 2
	}
}

func emit(output io.Writer, value any) int {
	if err := json.NewEncoder(output).Encode(value); err != nil {
		return 1
	}
	return 0
}

func fail(output io.Writer, err error) int {
	fmt.Fprintln(output, "blazn-agent:", err)
	return 1
}
