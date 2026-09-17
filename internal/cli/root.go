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

	// ExitNoApproval: the plan has changes, approval is required, and this run
	// cannot obtain it — --output is set so nobody is reading stdout, or stdin
	// is already at EOF so nobody is there to type.
	//
	// 77 is sysexits.h's EX_NOPERM. It is a fourth row on PLAN.md section 16's
	// table and therefore a documented product API change.
	//
	// It replaces a genuinely misleading outcome: a piped apply reached
	// confirm(), got EOF from bufio.Scanner, and exited 1 saying "you must
	// type yes to approve" — advice nobody in that pipeline could have taken,
	// which is exactly what section 44 says a suggested action must never be.
	ExitNoApproval = 77

	// ExitMigrationPending is 2 DELIBERATELY, the same code `plan` uses for
	// "changes present". Not a collision: both mean the same thing to a
	// pipeline -- something is pending, run the corresponding command. A
	// script reads it and runs `infrena state migrate`.
	ExitMigrationPending = 2
	// ExitMigrationComplete: both ends agree, so there is nothing to do and
	// the `migrate_from:` block is stale. Distinct from 0 so a pipeline can
	// remind somebody to remove it. A script treats it exactly as it treats 0
	// and carries on.
	ExitMigrationComplete = 3
	// ExitMigrationConflict: both ends hold DIFFERENT state, so somebody has
	// been applying to one of them. Distinct from 1 for the reason 77 is: a
	// pipeline that cannot tell "this failed" from "this needs a person" will
	// treat both the same, and they want opposite responses. A script stops
	// and fetches a human; retrying achieves nothing.
	//
	// Both `--check` and the migration itself report a conflict with this
	// code. One condition, one number: a pipeline that ran the migration
	// directly must not have to learn a second spelling of the answer the
	// check would have given it.
	ExitMigrationConflict = 4
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
	// PluginDirs is --plugin-dir: where to look for provider plugin binaries,
	// before every other location. Repeatable, highest precedence, so a plugin
	// author can point at a freshly built binary without installing it.
	PluginDirs []string
}

// NewRootCommand builds the command tree. It is a constructor rather than a
// package-level variable so tests can build independent instances.
func NewRootCommand() *cobra.Command {
	opts := &GlobalOptions{}

	root := &cobra.Command{
		Use:           "infrena",
		Short:         "Declarative infrastructure management",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	f := root.PersistentFlags()
	f.StringArrayVar(&opts.Vars, "var", nil, "set a variable (name=value); repeatable")
	f.StringArrayVar(&opts.VarFiles, "var-file", nil,
		"read variables from a YAML file, as if each entry had been passed with --var; repeatable, later files win")
	f.BoolVar(&opts.Verbose, "verbose", false, "include provider-level detail in output")
	f.StringArrayVar(&opts.PluginDirs, "plugin-dir", nil,
		"where to look for provider plugins, before every other location (repeatable)")
	f.StringVar(&opts.Output, "output", "", "write machine-readable output to this path")
	f.IntVar(&opts.Parallelism, "parallelism", 10, "maximum concurrent operations")
	f.BoolVar(&opts.AutoApprove, "auto-approve", false, "skip interactive approval")
	f.StringVar(&opts.Dir, "chdir", ".", "run as if infra had been started in this directory")

	// One place, so a command added later inherits the check rather than
	// having to remember it.
	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		if err := checkUnsupportedFlags(opts); err != nil {
			return err
		}
		return resolveProjectRoot(cmd, opts)
	}

	root.AddCommand(newVersionCommand(opts))
	root.AddCommand(newInitCommand(opts))
	root.AddCommand(newValidateCommand(opts))
	root.AddCommand(newStateCommand(opts))
	root.AddCommand(newPlanCommand(opts))
	root.AddCommand(newApplyCommand(opts))
	root.AddCommand(newDestroyCommand(opts))
	root.AddCommand(newRefreshCommand(opts))
	root.AddCommand(newExplainCommand(opts))
	root.AddCommand(newGraphCommand(opts))
	root.AddCommand(newDiscoverCommand(opts))
	root.AddCommand(newImportCommand(opts))
	root.AddCommand(newExportCommand(opts))
	root.AddCommand(newPluginsCommand(opts))

	return root
}

// Execute runs the CLI and returns the process exit code. Human output goes to
// stdout; diagnostics go to stderr, so piping works. Spec §16.
func Execute() int {
	root := NewRootCommand()
	root.SetOut(os.Stdout)
	root.SetErr(os.Stderr)

	if err := root.Execute(); err != nil {
		// OUTCOMES RATHER THAN FAILURES, and so not printed as errors:
		// nothing is wrong, something is pending or already done, and the
		// exit code is the whole report. Printing "Error:" over one of these
		// would make a successful check read as a broken run in a log
		// somebody finds weeks later.
		switch {
		case errors.Is(err, errChanges):
			return ExitChanges
		case errors.Is(err, errMigrationPending):
			return ExitMigrationPending
		case errors.Is(err, errMigrationComplete):
			return ExitMigrationComplete
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		// Reported on stderr like any other failure, and only then given its
		// own code: a run that refuses for want of approval HAS failed, and
		// the operator reading the terminal needs the reason as much as the
		// pipeline reading $? needs the number. A migration conflict is the
		// same shape: the person the code sends for needs to be told what
		// they are being sent for.
		switch {
		case errors.Is(err, errNoApproval):
			return ExitNoApproval
		case errors.Is(err, errMigrationConflict):
			return ExitMigrationConflict
		}
		return ExitError
	}
	return ExitOK
}

// checkUnsupportedFlags refuses flags that are registered but not yet wired,
// rather than accepting them and doing nothing. Each entry here is a promise
// the CLI cannot currently keep; the fix is to implement it and delete the
// entry, never to delete the entry alone.
//
// --var-file was the last entry; task 8 wired it, so the list is currently
// empty. The function stays rather than being deleted along with its last
// entry — it is the mechanism the next unwired flag needs, not a fact about
// --var-file specifically.
func checkUnsupportedFlags(opts *GlobalOptions) error {
	return nil
}
