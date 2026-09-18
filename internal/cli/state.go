package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/value"
)

// newStateCommand builds the `infra state` command group.
func newStateCommand(opts *GlobalOptions) *cobra.Command {
	cmd := &cobra.Command{Use: "state", Short: "Inspect and manage recorded state"}
	cmd.AddCommand(newStateListCommand(opts), newStateShowCommand(opts), newStateUnlockCommand(opts), newStateRmCommand(opts),
		newStateMigrateCommand(opts))
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

// newStateRmCommand builds `infra state rm <environment> <address>`.
//
// IT WAS ALREADY DOCUMENTED BY THREE ERROR MESSAGES BEFORE IT EXISTED. Import
// refuses a resource whose name or provider ID state already holds and told the
// reader to "use `infrena state rm <address>`", in three places, for a command
// that was never built — a suggested action nobody could take (§44). Three
// independent messages concluding it should exist is a strong argument that it
// should, and without it a user who imported the wrong thing had no supported
// way back at all: the only remedy was editing the state document by hand.
//
// IT DOES NOT TOUCH THE CLOUD, and the output says so plainly. This is
// `lifecycle.retain`'s shape as a command — state stops recording a resource
// that goes on existing — and the danger is precisely that it looks like a
// deletion. A resource removed here is not destroyed; it is ORPHANED, managed
// by nothing, and the next `discover` will offer it back.
//
// It takes the environment lock, because it writes state, and it is refused
// without approval for the same reason apply is: this is a mutation somebody
// should agree to rather than discover.
func newStateRmCommand(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:           "rm <environment> <address>",
		SilenceUsage:  true,
		SilenceErrors: true,
		Short:         "Stop managing a resource, without destroying it",
		Long: "Remove one resource from recorded state. The resource itself is NOT destroyed " +
			"and goes on existing, managed by nothing — `infrena discover` will offer it back.\n\n" +
			"To destroy a resource instead, remove it from configuration and apply.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			environment, target := args[0], args[1]

			b, closeBackend, err := backendFor(cmd.Context(), opts)
			if err != nil {
				return err
			}
			defer closeBackend()

			// Resolved BEFORE the lock, so a typo costs nothing and leaves
			// nothing locked — the same order apply resolves its approval in.
			s, err := b.Get(cmd.Context(), environment)
			if err != nil {
				return err
			}
			addr, err := resolveAddress(target, s.Addresses(), func(a address.Address) string {
				r, _ := s.Get(a)
				return r.Type
			})
			if err != nil {
				return err
			}
			r, ok := s.Get(addr)
			if !ok {
				return fmt.Errorf("%s is not managed in environment %q", addr, environment)
			}

			if !opts.AutoApprove {
				if approvalUnobtainable(opts) {
					return errNoApproval
				}
				prompt := fmt.Sprintf("\n%s (%s, provider ID %s) will stop being managed.\n"+
					"It will NOT be destroyed: it goes on existing with nothing recording it.\n"+
					"Type the address to confirm: ", addr, r.Type, r.ProviderID)
				switch confirm(cmd.InOrStdin(), cmd.OutOrStdout(), prompt, addr.String()) {
				case approvalNoInput:
					return errNoApproval
				case approvalDeclined:
					return fmt.Errorf("cancelled: you must type %q to confirm", addr)
				}
			}

			return withLockedEnvironment(environment, "state rm", b, cmd.ErrOrStderr(), func(ctx context.Context) error {
				// RE-READ INSIDE THE LOCK. The state resolved above was read
				// without it, so another run may have changed it since —
				// removing from the stale copy would write back a document
				// that silently undoes their work, which is the hazard the
				// lock exists for.
				locked, err := b.Get(ctx, environment)
				if err != nil {
					return err
				}
				if _, ok := locked.Get(addr); !ok {
					return fmt.Errorf("%s is no longer managed in environment %q: "+
						"something else changed this state while this command was waiting", addr, environment)
				}
				locked.Remove(addr)
				if err := b.Put(ctx, environment, locked); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(),
					"Removed %s from state. %s (%s) still exists and is now managed by nothing.\n",
					addr, r.ProviderID, r.Type)
				return nil
			})
		},
	}
}
