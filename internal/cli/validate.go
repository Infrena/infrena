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
		RunE: func(cmd *cobra.Command, args []string) error {
			ds := validateProject(opts.Dir, buildRegistry(opts.Dir))
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
// returns every diagnostic it produces. `infra validate` does not pin an
// environment, so environment-varying defaults resolve against the empty
// string; that only changes which default is filled in, never whether the
// configuration is valid.
func validateProject(dir string, reg *registry.Registry) diag.Diagnostics {
	files, err := config.Load(dir)
	if err != nil {
		var ds diag.Diagnostics
		ds.Add(diag.Diagnostic{Severity: diag.SeverityError, Summary: err.Error()})
		return ds
	}

	_, ds := compiler.Compile(files, reg, compiler.Options{})
	return ds
}
