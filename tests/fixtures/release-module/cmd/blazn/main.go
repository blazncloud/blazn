package main

import "fmt"

var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

func main() {
	fmt.Printf("%s %s %s\n", version, commit, buildDate)
}
