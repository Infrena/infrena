package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"infra/internal/compiler"
	"infra/internal/config"
	"infra/internal/diag"
	"infra/internal/registry"
)

// newValidateCommand builds the `infra validate` command.
func newValidateCommand(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "Check configuration for errors without contacting providers",
		Args:  cobra.NoArgs,
		// See newPlanCommand for why these are set per-command as well as on
		// the root: cobra consults this command's own fields when it has no
		// parent, which is how every test constructs it.
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			vars, err := parseVars(opts.Vars)
			if err != nil {
				return err
			}
			ds := validateProject(opts.Dir, buildRegistry(opts.Dir), vars)
			ds.Render(cmd.ErrOrStderr())

			if ds.HasErrors() {
				return errors.New("configuration is not valid")
			}
			fmt.Fprintln(cmd.OutOrStdout(), "✓ Configuration valid")
			return nil
		},
	}
}

// validateProject runs the full compiler pipeline — stages 1 through 8 — and
// returns every diagnostic it produces.
//
// vars carries --var through to the compiler. It used to be omitted, on the
// belief that nothing consumed Options.Vars yet and that the variable system
// was M4 work. That was wrong: internal/compiler has always turned
// Options.Vars into ${name} values. The consequence was measurable and
// user-facing — `infra validate --var cidr=10.0.0.0/16` reported
//
//	Error: undefined variable "cidr"
//
// for a configuration that `infra plan dev --var cidr=10.0.0.0/16` planned
// without complaint. A command whose entire job is answering "is this
// configuration valid?" said no to configuration that was fine.
//
// `infra validate` does not pin an
// environment, so environment-varying defaults resolve against the empty
// string; that only changes which default is filled in, never whether the
// configuration is valid.
func validateProject(dir string, reg *registry.Registry, vars map[string]string) diag.Diagnostics {
	files, err := config.Load(dir)
	if err != nil {
		var ds diag.Diagnostics
		ds.Add(diag.Diagnostic{Severity: diag.SeverityError, Summary: err.Error()})
		return ds
	}

	_, ds := compiler.Compile(files, reg, compiler.Options{Vars: vars})
	return ds
}
