package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/infrata/infrata/internal/generator"
)

// newExportCommand builds `infrata export <environment>` (spec §28).
//
// The opposite of generation: every configurable attribute of everything in
// state, for auditing and migration. `import --generate` writes what you must
// know; export writes what there is.
//
// It reads STATE, not a provider. Export answers "what is this tool managing",
// which is a question about the record rather than about the account —
// `discover` is the command that asks the account. That also means export needs
// no network and cannot fail on credentials.
//
// It reuses the generator with minimal mode off rather than adding a second
// renderer. Two renderers would mean two answers to "is this attribute
// sensitive", and the one that got it wrong would be the one nobody was
// watching.
func newExportCommand(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:           "export <environment>",
		SilenceUsage:  true,
		SilenceErrors: true,
		Short:         "Write out everything in state, in full",
		Long: "Dump every configurable attribute of every resource this tool manages in an " +
			"environment.\n\nUnlike `import --generate`, nothing is omitted for being equal to a " +
			"default — an audit wants the value, not the reason it did not need writing. " +
			"Sensitive attributes are still omitted: an export is, if anything, more likely to " +
			"be pasted somewhere public than a generated file is.\n\nWritten to stdout.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			environment := args[0]
			reg, closePlugins := buildRegistry(opts)
			defer closePlugins()
			backend := backendFor(opts.Dir)

			// No lock. Export only reads, and taking one would make an audit
			// wait behind — or block — an apply.
			st, err := backend.Get(context.Background(), environment)
			if err != nil {
				return err
			}
			addrs := st.Addresses()
			if len(addrs) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "# Nothing is managed in environment %q.\n", environment)
				return nil
			}

			resources := make([]generator.Resource, 0, len(addrs))
			for _, addr := range addrs {
				r, ok := st.Get(addr)
				if !ok {
					continue
				}
				resources = append(resources, generator.Resource{
					Name:       addr.String(),
					Type:       r.Type,
					ProviderID: r.ProviderID,
					Attributes: r.Attributes,
				})
			}

			files, err := generator.Generate(resources, reg, generator.Options{
				Minimal: false,
				Header: "infrata export of environment " + environment + "\n" +
					"Every configurable attribute, including ones equal to a provider default.\n" +
					"Sensitive attributes are omitted; see the notes beside each resource.",
			})
			if err != nil {
				return err
			}

			for i, f := range files {
				if i > 0 {
					fmt.Fprintln(cmd.OutOrStdout())
				}
				fmt.Fprintf(cmd.OutOrStdout(), "# ---- %s ----\n", f.Name)
				cmd.OutOrStdout().Write(f.Bytes)
			}
			return nil
		},
	}
}
