package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/infrata/infrata/internal/discovery"
)

// newDiscoverCommand builds `infrata discover [type...]` (spec §25).
//
// READ-ONLY, and it says so where a user looks: nothing is written, nothing is
// adopted into state, and no configuration is generated. It answers "what is
// out there" so that `import` is a decision rather than a leap.
//
// It takes no environment argument. Discovery asks a provider what exists,
// which is a question about an account rather than about a deployment — there
// is no state to read and no configuration to compile, so requiring an
// environment would imply a scoping the command does not have.
func newDiscoverCommand(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:           "discover [type...]",
		SilenceUsage:  true,
		SilenceErrors: true,
		Short:         "List infrastructure that exists, without changing anything",
		Long: "Ask every provider what exists and show it, including resources this project " +
			"never created.\n\nNothing is written: no state, no configuration. Pass one or more " +
			"resource types to narrow the question.\n\nThe `name` column is what `infrata import` " +
			"would call each resource, so a collision is visible here rather than after the fact.",
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, _, regDiags := stateOnlyRegistry(opts.Dir)
			if regDiags.HasErrors() {
				regDiags.Render(cmd.ErrOrStderr())
				return errProviderInstances
			}
			found, problems := discovery.Walk(cmd.Context(), reg, args)

			// Reported before the results, because a partial answer a reader
			// mistakes for a complete one is the failure worth avoiding here.
			for _, p := range problems {
				fmt.Fprintf(cmd.ErrOrStderr(), "Warning: %v\n", p)
			}
			renderDiscovered(cmd.OutOrStdout(), found, args)
			return nil
		},
	}
}

// renderDiscovered prints §25's table.
func renderDiscovered(w io.Writer, found []discovery.Result, types []string) {
	if len(found) == 0 {
		if len(types) > 0 {
			fmt.Fprintf(w, "Nothing found of type %s.\n", strings.Join(types, ", "))
			return
		}
		fmt.Fprintln(w, "Nothing found.")
		return
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TYPE\tID\tNAME")
	for _, r := range found {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", r.Type, r.ProviderID, r.Name)
	}
	tw.Flush()

	fmt.Fprintf(w, "\n%d resource%s found. Nothing has been imported.\n",
		len(found), plural(len(found)))
	fmt.Fprintln(w, "Run `infrata import <environment> --generate` to adopt them.")
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
