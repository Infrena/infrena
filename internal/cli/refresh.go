package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

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
// "The write" is not conditioned on drift being found: every resource that
// reads successfully is written back via applyObservations below, even one
// whose observed state is byte-identical to what was already recorded —
// state.Local.Put's own contract advances Serial on every write it makes,
// unconditionally (see internal/state/local_test.go's
// TestPutIncrementsSerial), and refresh's job is to record that a read
// happened, not only to record when a read changed something. The one case
// that skips the write entirely is an environment with nothing in state to
// read at all — see applyObservations's own doc comment.
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
			if err := rejectVariableFlags(opts, "refresh"); err != nil {
				return err
			}

			environment := args[0]

			reg := buildRegistry(opts.Dir)
			backend := backendFor(opts.Dir)

			return withLockedEnvironment(environment, "refresh", backend, cmd.ErrOrStderr(), func(ctx context.Context) error {
				st, err := backend.Get(ctx, environment)
				if err != nil {
					return err
				}

				obs, ds := refresh.Refresh(ctx, st, reg, opts.Parallelism, perProviderParallelism)
				ds.Render(cmd.ErrOrStderr())

				wrote := applyObservations(st, obs, cmd.OutOrStdout())

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

// applyObservations decides, per resource in st, what to do with what obs
// learned about it, writing progress lines to out as it goes. It reports
// whether st was mutated at all — an empty st (nothing recorded, so nothing
// to read) mutates nothing and reports false, which is what lets the
// caller skip backend.Put entirely rather than writing an unchanged empty
// document and bumping Serial for a run that read nothing.
//
// st.Addresses() is sorted, so both this loop's order and therefore stdout
// are deterministic regardless of which read finished first.
//
// The switch has four arms, not three, specifically to close a second
// entrance to the bug this command exists to prevent (spec §10): a map
// lookup that misses returns Observation's zero value, which has both
// Err == nil and State == nil — the exact shape of "provider affirmatively
// reports this gone" if tested with a plain `if o.State == nil`. A missing
// observation is not an affirmative report of anything; it is the absence
// of one, and must be handled exactly as an unknown-reality read error is:
// state left untouched. refresh.Refresh always returns one Observation per
// address in st.Addresses() (the same list this loop ranges), so the !ok
// arm is unreachable through the real call path today — it is guarded
// anyway, because "unreachable today" is not a promise about tomorrow, and
// the failure mode on the other side of removing this guard is silent
// state deletion.
func applyObservations(st *state.State, obs refresh.Observations, out io.Writer) bool {
	wrote := false
	for _, addr := range st.Addresses() {
		o, ok := obs[addr.String()]
		switch {
		case !ok:
			// No observation at all for a resource recorded in state.
			// Should not happen (see doc comment above) — treated as
			// unknown, not as gone, for the same reason a read error is.
			fmt.Fprintf(out, "  %s: no observation returned; leaving state unchanged\n", addr)
		case o.Err != nil:
			// Reality is UNKNOWN. State is left exactly as it was — never
			// removed, never overwritten — because writing "gone" here
			// would make the next plan propose recreating infrastructure
			// that may well still exist.
			fmt.Fprintf(out, "  %s: could not refresh (see diagnostics)\n", addr)
		case o.State == nil:
			// The provider affirmatively reports this resource gone. This
			// is the ONLY case that removes it from state.
			fmt.Fprintf(out, "  %s: no longer exists; removed from state\n", addr)
			st.Remove(addr)
			wrote = true
		default:
			fmt.Fprintf(out, "  %s: refreshed\n", addr)
			st.Set(o.State)
			wrote = true
		}
	}
	return wrote
}
