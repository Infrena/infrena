package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/value"
)

// newStateCommand builds the `infra state` command group.
func newStateCommand(opts *GlobalOptions) *cobra.Command {
	cmd := &cobra.Command{Use: "state", Short: "Inspect and manage recorded state"}
	cmd.AddCommand(newStateListCommand(opts), newStateShowCommand(opts), newStateUnlockCommand(opts))
	return cmd
}

// newStateListCommand builds `infra state list`.
func newStateListCommand(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:           "list <environment>",
		SilenceUsage:  true,
		SilenceErrors: true,
		Short:         "List managed resources",
		Args:          cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, closeBackend, err := backendFor(cmd.Context(), opts)
			if err != nil {
				return err
			}
			defer closeBackend()

			s, err := b.Get(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			addrs := s.Addresses()
			if len(addrs) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "No resources are managed in environment %q.\n", args[0])
				return nil
			}
			for _, a := range addrs {
				r, _ := s.Get(a)
				fmt.Fprintf(cmd.OutOrStdout(), "%s.%s\n", r.Type, a.String())
			}
			return nil
		},
	}
}

// newStateShowCommand builds `infra state show`.
func newStateShowCommand(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:           "show <environment> <address>",
		SilenceUsage:  true,
		SilenceErrors: true,
		Short:         "Show one managed resource",
		Args:          cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, closeBackend, err := backendFor(cmd.Context(), opts)
			if err != nil {
				return err
			}
			defer closeBackend()

			s, err := b.Get(cmd.Context(), args[0])
			if err != nil {
				return err
			}

			addr, err := resolveAddress(args[1], s.Addresses(), func(a address.Address) string {
				r, _ := s.Get(a)
				return r.Type
			})
			if err != nil {
				return err
			}

			r, ok := s.Get(addr)
			if !ok {
				return fmt.Errorf("%s is not managed in environment %q", addr, args[0])
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%s.%s\n", r.Type, r.Address)
			fmt.Fprintf(out, "  provider     %s\n", r.Provider)
			fmt.Fprintf(out, "  provider_id  %s\n", r.ProviderID)
			for _, name := range sortedAttributeKeys(r.Attributes) {
				fmt.Fprintf(out, "  %-12s %s\n", name, formatValue(r.Attributes[name]))
			}
			return nil
		},
	}
}

// newStateUnlockCommand builds `infra state unlock`.
func newStateUnlockCommand(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:           "unlock <environment>",
		SilenceUsage:  true,
		SilenceErrors: true,
		Short:         "Release a lock left behind by an interrupted run",
		Args:          cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, closeBackend, err := backendFor(cmd.Context(), opts)
			if err != nil {
				return err
			}
			defer closeBackend()

			lock, held, err := b.Inspect(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if !held {
				return fmt.Errorf("environment %q is not locked", args[0])
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Releasing lock held by %s on %s (pid %d) since %s.\n",
				lock.User, lock.Host, lock.PID, lock.At.Format("2006-01-02 15:04:05 MST"))
			return b.ForceUnlock(cmd.Context(), args[0])
		},
	}
}

// resolveAddress accepts a canonical address, or a "type.name" display form
// when it is unambiguous. Spec §5.2.
func resolveAddress(input string, known []address.Address, typeOf func(address.Address) string) (address.Address, error) {
	if addr, err := address.Parse(input); err == nil {
		for _, k := range known {
			if k.String() == addr.String() {
				return addr, nil
			}
		}
	}

	var matches []address.Address
	for _, k := range known {
		if typeOf(k)+"."+k.String() == input {
			matches = append(matches, k)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return address.Address{}, fmt.Errorf("no managed resource matches %q", input)
	default:
		var names []string
		for _, m := range matches {
			names = append(names, m.String())
		}
		return address.Address{}, fmt.Errorf("%q is ambiguous; it matches %s", input, strings.Join(names, ", "))
	}
}

// formatValue renders an attribute for display, redacting sensitive data.
//
// Sensitivity is a per-leaf flag (pkg/value's Value doc comment: "Composites
// hold Values recursively so provenance is per-leaf"), so a composite value
// can be non-sensitive overall while holding a sensitive leaf. Checking only
// the top-level flag and otherwise printing Raw with %v would leak such a leaf
// straight through Go's struct formatting. formatValue instead walks lists and
// maps and redacts every sensitive leaf it finds, wherever it is nested.
// formatValue renders one value for `state show`, redacting sensitive data.
//
// The whole implementation lives in value.Format, which is the ONLY copy in
// the tree. It used to be duplicated here and in the plan renderer, and the
// two had already diverged in three ways — which is how a leak fixed in one
// survived in the other. See value.Format's comment for the two measured
// leaks that produced this rule.
func formatValue(v value.Value) string {
	// value.ProseFormatOptions, not a package-local copy: `state show` is
	// for reading, not diffing — see that value's doc comment for the bare-
	// versus-quoted distinction and why a second literal here is how this
	// inspector and the plan renderer drifted apart in M2.
	return value.Format(v, value.ProseFormatOptions)
}
