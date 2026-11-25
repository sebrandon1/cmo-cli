// Command cmo is the Cert Manager Operator CLI. It is the same program as the
// module root's main.go, here so that
//
//	go install github.com/sebrandon1/cmo-cli/cmd/cmo@latest
//
// installs a binary named cmo rather than cmo-cli.
package main

import (
	"os"

	"github.com/sebrandon1/cmo-cli/cmd"
)

var version = "dev"

func main() {
	cmd.SetVersion(version)

	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
