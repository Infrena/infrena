package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/value"
)

// newStateCommand builds the `infrena state` command group.
func newStateCommand(opts *GlobalOptions) *cobra.Command {
	cmd := &cobra.Command{Use: "state", Short: "Inspect and manage recorded state"}
	cmd.AddCommand(newStateListCommand(opts), newStateShowCommand(opts), newStateUnlockCommand(opts), newStateRmCommand(opts),
		newStateMigrateCommand(opts))
	return cmd
}

// newStateListCommand builds `infrena state list`.
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

// newStateShowCommand builds `infrena state show`.
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

// newStateUnlockCommand builds `infrena state unlock`.
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
// when it is unambiguous.
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

// formatValue renders one attribute for `state show`, redacting sensitive data.
//
// Sensitivity is a per-leaf flag, so a composite can be non-sensitive overall
// while holding a sensitive leaf; value.Format walks the whole value and is the
// only copy of that logic in the tree. Reimplementing it here is how a leak
// fixed in one renderer once survived in the other.
func formatValue(v value.Value) string {
	// ProseFormatOptions rather than a local literal: `state show` is for
	// reading, not diffing, and a second copy of the options is how this
	// inspector and the plan renderer drifted apart once already.
	return value.Format(v, value.ProseFormatOptions)
}

// newStateRmCommand builds `infrena state rm <environment> <address>`: the
// supported way back for a user who imported the wrong thing, which otherwise
// means editing the state document by hand.
//
// It does not touch the cloud, and the output says so plainly, because the
// danger is precisely that it looks like a deletion. A resource removed here is
// orphaned rather than destroyed, and the next `discover` will offer it back.
//
// It takes the environment lock, because it writes state, and it needs approval
// for the same reason apply does.
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

			// Resolved before the lock, so a typo costs nothing and leaves
			// nothing locked.
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
				// Re-read inside the lock: the state resolved above was read
				// without it, and writing back that stale copy would silently
				// undo whatever another run changed in the meantime.
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
