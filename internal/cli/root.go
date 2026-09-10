// Package cli wires the infra command-line interface.
package cli

import (
	"errors"
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
	// Registered so the flag exists and reports honestly, NOT because it
	// works. Nothing reads opts.VarFiles; variable files arrive in M4. A
	// flag that is advertised in --help and silently ignored is worse than
	// an absent one, and --var working end to end since M2 gives a user
	// every reason to assume its sibling does too. checkUnsupportedFlags
	// turns any use of it into an error.
	f.StringArrayVar(&opts.VarFiles, "var-file", nil, "load variables from a file; repeatable (not yet supported)")
	f.BoolVar(&opts.Verbose, "verbose", false, "include provider-level detail in output")
	f.StringVar(&opts.Output, "output", "", "write machine-readable output to this path")
	f.IntVar(&opts.Parallelism, "parallelism", 10, "maximum concurrent operations")
	f.BoolVar(&opts.AutoApprove, "auto-approve", false, "skip interactive approval")
	f.StringVar(&opts.Dir, "chdir", ".", "run as if infra had been started in this directory")

	// One place, so a command added later inherits the check rather than
	// having to remember it.
	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		return checkUnsupportedFlags(opts)
	}

	root.AddCommand(newValidateCommand(opts))
	root.AddCommand(newStateCommand(opts))
	root.AddCommand(newPlanCommand(opts))
	root.AddCommand(newApplyCommand(opts))
	root.AddCommand(newDestroyCommand(opts))

	return root
}

// Execute runs the CLI and returns the process exit code. Human output goes to
// stdout; diagnostics go to stderr, so piping works. Spec §16.
func Execute() int {
	root := NewRootCommand()
	root.SetOut(os.Stdout)
	root.SetErr(os.Stderr)

	if err := root.Execute(); err != nil {
		if errors.Is(err, errChanges) {
			return ExitChanges
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return ExitError
	}
	return ExitOK
}

// checkUnsupportedFlags refuses flags that are registered but not yet wired,
// rather than accepting them and doing nothing. Each entry here is a promise
// the CLI cannot currently keep; the fix is to implement it and delete the
// entry, never to delete the entry alone.
func checkUnsupportedFlags(opts *GlobalOptions) error {
	if len(opts.VarFiles) > 0 {
		return errors.New("--var-file is not supported yet; variable files arrive with the " +
			"variable system. Pass individual variables with --var name=value")
	}
	return nil
}
