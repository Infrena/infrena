// Command infra is the declarative infrastructure management CLI.
package main

import (
	"os"

	"infra/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
