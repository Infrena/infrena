// Package cli wires the infra command-line interface.
package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// Exit codes. Spec §16: 0 success with no changes, 1 error, 2 success with
// changes present.
const (
	ExitOK      = 0
	ExitError   = 1
	ExitChanges = 2
)

// GlobalOptions holds flags shared by every subcommand.
type GlobalOptions struct {
	Vars        []string
	VarFiles    []string
	Verbose     bool
	Output      string
	Parallelism int
	AutoApprove bool
	Dir         string
}

// NewRootCommand builds the command tree. It is a constructor rather than a
// package-level variable so tests can build independent instances.
func NewRootCommand() *cobra.Command {
	opts := &GlobalOptions{}

	root := &cobra.Command{
		Use:           "infra",
		Short:         "Declarative infrastructure management",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	f := root.PersistentFlags()
	f.StringArrayVar(&opts.Vars, "var", nil, "set a variable (name=value); repeatable")
	f.StringArrayVar(&opts.VarFiles, "var-file", nil, "load variables from a file; repeatable")
	f.BoolVar(&opts.Verbose, "verbose", false, "include provider-level detail in output")
	f.StringVar(&opts.Output, "output", "", "write machine-readable output to this path")
	f.IntVar(&opts.Parallelism, "parallelism", 10, "maximum concurrent operations")
	f.BoolVar(&opts.AutoApprove, "auto-approve", false, "skip interactive approval")
	f.StringVar(&opts.Dir, "chdir", ".", "run as if infra had been started in this directory")

	root.AddCommand(newValidateCommand(opts))
	root.AddCommand(newStateCommand(opts))

	return root
}

// Execute runs the CLI and returns the process exit code. Human output goes to
// stdout; diagnostics go to stderr, so piping works. Spec §16.
func Execute() int {
	root := NewRootCommand()
	root.SetOut(os.Stdout)
	root.SetErr(os.Stderr)

	if err := root.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return ExitError
	}
	return ExitOK
}
