package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/infrena/infrena/internal/discovery"
)

// newDiscoverCommand builds `infrena discover [type...]` (spec §25).
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
	var all bool

	cmd := &cobra.Command{
		Use:           "discover [type...]",
		SilenceUsage:  true,
		SilenceErrors: true,
		Short:         "List infrastructure that exists, without changing anything",
		Long: "Ask every provider what exists and show it, including resources this project " +
			"never created.\n\nNothing is written: no state, no configuration. Pass one or more " +
			"resource types to narrow the question.\n\nResources this project already manages, in " +
			"any environment, are left out: the question a survey answers is what has not been " +
			"adopted yet. --all shows them too, with a STATUS column.\n\nThe `name` column is " +
			"what `infrena import` would call each resource, so a collision is visible here " +
			"rather than after the fact.",
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, _, regDiags, closePlugins := discoveryRegistry(opts, "")
			defer closePlugins()
			if regDiags.HasErrors() {
				regDiags.Render(cmd.ErrOrStderr())
				return errProviderInstances
			}

			// Read before the walk, so a state directory this command cannot
			// read fails before a user has read a table that would be wrong.
			managed, err := managedProviderIDs(cmd.Context(), backendFor(opts.Dir))
			if err != nil {
				return err
			}

			found, problems := discovery.Walk(cmd.Context(), reg, args)

			// Reported before the results, because a partial answer a reader
			// mistakes for a complete one is the failure worth avoiding here.
			for _, p := range problems {
				fmt.Fprintf(cmd.ErrOrStderr(), "Warning: %v\n", p)
			}
			renderDiscovered(cmd.OutOrStdout(), found, args, managed, all)
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false,
		"also show resources this project already manages, with a STATUS column")
	return cmd
}

// renderDiscovered prints §25's table.
//
// managed maps a provider ID to the environment managing it. Those rows are
// left out by default and shown under all, because a real account returns
// hundreds of rows and the question is always what has not been adopted yet.
//
// The footer states BOTH counts either way, because a count a user cannot see
// is one they will assume is zero: a survey that quietly dropped fifty rows
// reads as an account with fifty fewer resources in it.
func renderDiscovered(
	w io.Writer, found []discovery.Result, types []string, managed map[string]string, all bool,
) {
	var unmanaged, held []discovery.Result
	for _, r := range found {
		if _, isManaged := managed[r.ProviderID]; isManaged {
			held = append(held, r)
			continue
		}
		unmanaged = append(unmanaged, r)
	}

	if len(found) == 0 {
		if len(types) > 0 {
			fmt.Fprintf(w, "Nothing found of type %s.\n", strings.Join(types, ", "))
			return
		}
		fmt.Fprintln(w, "Nothing found.")
		return
	}

	shown := unmanaged
	if all {
		shown = found
	}

	if len(shown) == 0 {
		// Everything that exists is already managed. Said differently from an
		// empty account on purpose: the two send a reader to different places.
		fmt.Fprintln(w, "Nothing unmanaged found.")
	} else {
		notes := anyNoted(shown)
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, strings.Join(discoverHeader(all, notes), "\t"))
		for _, r := range shown {
			cells := []string{r.Type, r.ProviderID, r.Name}
			if all {
				cells = append(cells, statusOf(r, managed))
			}
			if notes {
				cells = append(cells, r.SystemOwnedReason)
			}
			fmt.Fprintln(tw, strings.Join(cells, "\t"))
		}
		tw.Flush()
	}

	fmt.Fprintf(w, "\n%d resource%s found: %d unmanaged, %d already managed%s.\n",
		len(found), plural(len(found)), len(unmanaged), len(held), showHint(held, all))
	fmt.Fprintln(w, "Nothing has been imported.")
	fmt.Fprintln(w, "Run `infrena import <environment> --generate` to adopt them.")
}

// discoverHeader is the table's header row. STATUS appears only under all,
// because in the default view every row would read unmanaged and a column with
// one value in it is noise. NOTE appears only when something is noted, for the
// same reason.
func discoverHeader(all, notes bool) []string {
	header := []string{"TYPE", "ID", "NAME"}
	if all {
		header = append(header, "STATUS")
	}
	if notes {
		header = append(header, "NOTE")
	}
	return header
}

// anyNoted reports whether any row carries a plugin's note about itself, which
// today means a system-owned declaration.
//
// The REASON is what the column holds, not the flag. A row reading "system
// owned" tells a user a resource is special and nothing about why, and a flag a
// user overrides without understanding it is a flag that may as well not be
// there — §3.5 requires the plugin to supply words, so they are what is shown.
func anyNoted(results []discovery.Result) bool {
	for _, r := range results {
		if r.SystemOwnedReason != "" {
			return true
		}
	}
	return false
}

// statusOf is the STATUS cell: which environment manages this resource, or that
// nothing does. The environment is NAMED, because "managed" alone leaves a
// reader with nowhere to look and looking is the whole point of the column.
func statusOf(r discovery.Result, managed map[string]string) string {
	if environment, ok := managed[r.ProviderID]; ok {
		return "managed (" + environment + ")"
	}
	return "unmanaged"
}

// showHint names the flag that reveals what was left out, and only when
// something was. §44: a suggested action has to be one the reader can take, and
// one that would show nothing new is not worth printing.
func showHint(held []discovery.Result, all bool) string {
	if all || len(held) == 0 {
		return ""
	}
	return " (--all to show them)"
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
