// Command infra is the declarative infrastructure management CLI.
package main

import (
	"os"

	"github.com/infrena/infrena/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
