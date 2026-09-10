package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"infra/internal/refresh"
	"infra/internal/state"
)

// newRefreshCommand builds `infra refresh <environment>`: read every
// resource's current provider state and persist what was learned. The only
// M3 verb that writes what a read observed (spec §10) — `plan` and the
// preview half of `apply`/`destroy` use the exact same refresh.Refresh call
// and discard its result.
//
// Unlike apply/destroy, refresh does not compute a throwaway preview before
// taking the lock: its entire job is the write, so there is no "nothing to
// do" outcome worth previewing. The lock is taken first and held for the
// whole run.
//
// The whole run is wrapped in runInterruptible (Task 11): refresh takes the
// lock before doing anything else and Go does not run deferred functions on
// SIGINT, so without this wrapper an interrupted refresh would leave a
// stale lock behind for the user to clear by hand. Unlike apply/destroy,
// the per-resource provider reads are NOT wrapped in operationContext —
// Provider.Read has no side effects, so cancelling one mid-flight only
// loses an observation (the next refresh reads it again), and that is what
// lets Ctrl-C stop a refresh promptly instead of waiting out every
// remaining resource.
func newRefreshCommand(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:           "refresh <environment>",
		Short:         "Reconcile recorded state with what providers actually report",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			environment := args[0]

			reg := buildRegistry(opts.Dir)
			backend := backendFor(opts.Dir)

			return runInterruptible(environment, func(ctx context.Context) error {
				ctx = state.WithOperation(ctx, "refresh")
				if _, err := backend.Lock(ctx, environment); err != nil {
					// Lock's own error already names the holder (spec §9.2) —
					// nothing to add.
					return err
				}
				defer releaseLock(backend, environment, cmd.ErrOrStderr())

				st, err := backend.Get(ctx, environment)
				if err != nil {
					return err
				}

				obs, ds := refresh.Refresh(ctx, st, reg, opts.Parallelism)
				ds.Render(cmd.ErrOrStderr())

				// st.Addresses() is sorted, so this — and therefore stdout —
				// is deterministic regardless of which read finished first.
				out := cmd.OutOrStdout()
				wrote := false
				for _, addr := range st.Addresses() {
					o := obs[addr.String()]
					switch {
					case o.Err != nil:
						// Reality is UNKNOWN. State is left exactly as it
						// was — never removed, never overwritten — because
						// writing "gone" here would make the next plan
						// propose recreating infrastructure that may well
						// still exist.
						fmt.Fprintf(out, "  %s: could not refresh (see diagnostics)\n", addr)
					case o.State == nil:
						// The provider affirmatively reports this resource
						// gone. This is the ONLY case that removes it from
						// state.
						fmt.Fprintf(out, "  %s: no longer exists; removed from state\n", addr)
						st.Remove(addr)
						wrote = true
					default:
						fmt.Fprintf(out, "  %s: refreshed\n", addr)
						st.Set(o.State)
						wrote = true
					}
				}

				if wrote {
					if err := backend.Put(ctx, environment, st); err != nil {
						return fmt.Errorf("writing refreshed state: %w", err)
					}
				}

				if ds.HasErrors() {
					return errors.New("refresh completed with errors; some resources could not be read")
				}
				return nil
			})
		},
	}
}
