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
		if errors.Is(err, errChanges) {
			return ExitChanges
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		// Reported on stderr like any other failure, and only then given its
		// own code: a run that refuses for want of approval HAS failed, and
		// the operator reading the terminal needs the reason as much as the
		// pipeline reading $? needs the number.
		if errors.Is(err, errNoApproval) {
			return ExitNoApproval
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
