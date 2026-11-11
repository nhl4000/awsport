// Command awsport opens SSM port-forward tunnels and shells to EC2 and RDS
// with stable local ports.
package main

import (
	"os"

	"github.com/nhl4000/awsport/internal/cli"
)

// Set at build time via -ldflags "-X main.version=... -X main.commit=...".
var (
	version = "dev"
	commit  = "none"
)

func main() {
	os.Exit(cli.Execute(version, commit))
}
