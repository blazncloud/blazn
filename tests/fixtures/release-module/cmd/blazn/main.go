package main

import "fmt"

var (
	version   = "dev"
	commit    = "unknown"
	buildTime = "unknown"
)

func main() {
	fmt.Printf("%s %s %s\n", version, commit, buildTime)
}
