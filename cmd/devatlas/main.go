// Command devatlas is the DevAtlas command-line entry point.
//
// DevAtlas is a local project registry for a developer workstation. This
// binary currently provides the application bootstrap: a version command and
// a loopback-only HTTP server with a health endpoint and a home page.
package main

import (
	"os"

	"github.com/Ryttis/devatlas/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
