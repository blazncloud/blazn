package main

import (
	"context"
	"os"

	"github.com/blazncloud/blazn/internal/cli"
	proxyprocess "github.com/blazncloud/blazn/internal/proxy/process"
)

var (
	version         = "dev"
	commit          = "unknown"
	buildTime       = "unknown"
	contractVersion = "v1alpha1"
)

func main() {
	if proxyprocess.IsChildInvocation(os.Args[1:]) {
		bootstrap := os.NewFile(3, "proxy-bootstrap")
		handshake := os.NewFile(4, "proxy-handshake")
		if bootstrap == nil || handshake == nil || proxyprocess.DefaultChildMain(context.Background(), bootstrap, handshake) != nil {
			os.Exit(1)
		}
		return
	}
	app := cli.New(os.Stdout, os.Stderr, cli.BuildInfo{
		Version:         version,
		Commit:          commit,
		BuildTime:       buildTime,
		ContractVersion: contractVersion,
	})
	os.Exit(app.Run(os.Args[1:]))
}
