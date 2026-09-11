// Command infra is the declarative infrastructure management CLI.
package main

import (
	"os"

	"github.com/infrata/infrata/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
