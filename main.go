package main

import (
	"os"

	"github.com/sebrandon1/cmo-cli/cmd"
)

var version = "dev"

func main() {
	cmd.SetVersion(version)

	if err := cmd.Execute(); err != nil {
		os.Exit(cmd.ExitCode(err))
	}
}
