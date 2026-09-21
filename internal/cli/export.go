package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/infrena/infrena/internal/generator"
)

// newExportCommand builds `infrena export <environment>`: every configurable
// attribute of everything in state, for auditing and migration.
//
// It reads state, not a provider, so it needs no network and cannot fail on
// credentials. `discover` is the command that asks the account.
//
// It reuses the generator with minimal mode off rather than adding a second
// renderer, so there is only one answer to "is this attribute sensitive".
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
			backend, closeBackend, err := backendFor(cmd.Context(), opts)
			if err != nil {
				return err
			}
			defer closeBackend()

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

			// The edges are discarded: export writes no files, so there is no
			// state alongside them for an edge to agree or disagree with.
			files, _, err := generator.Generate(resources, reg, generator.Options{
				Minimal: false,
				Header: "infrena export of environment " + environment + "\n" +
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
