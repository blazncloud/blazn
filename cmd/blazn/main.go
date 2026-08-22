package main

import (
	"os"

	"github.com/blazncloud/blazn/internal/cli"
)

var (
	version         = "dev"
	commit          = "unknown"
	buildTime       = "unknown"
	contractVersion = "v1alpha1"
)

func main() {
	app := cli.New(os.Stdout, os.Stderr, cli.BuildInfo{
		Version:         version,
		Commit:          commit,
		BuildTime:       buildTime,
		ContractVersion: contractVersion,
	})
	os.Exit(app.Run(os.Args[1:]))
}
