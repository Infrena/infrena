package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/infrena/infrena/internal/refresh"
	"github.com/infrena/infrena/internal/state"
	"github.com/infrena/infrena/pkg/report"
)

// newRefreshCommand builds `infrena refresh <environment>`: read every resource's
// current provider state and persist what was learned. It is the only verb that
// writes what a read observed — `plan` and the preview half of `apply` and `destroy`
// make the same refresh.Refresh call and discard its result.
//
// Unlike apply and destroy, it computes no throwaway preview before taking the lock:
// its entire job is the write, so there is no "nothing to do" outcome worth
// previewing. The lock is taken first and held for the whole run.
//
// The write is not conditioned on drift being found. Every resource that reads
// successfully is written back, even one whose observed state is identical to what
// was recorded, because refresh's job is to record that a read happened and Put
// advances Serial on every write it makes. Only an environment with nothing in state
// to read skips the write entirely — see applyObservations.
//
// The whole run is wrapped in runInterruptible: refresh takes the lock before doing
// anything else and Go does not run deferred functions on SIGINT, so without the
// wrapper an interrupted refresh would leave a stale lock behind to be cleared by
// hand. Unlike apply and destroy, the per-resource reads are not wrapped in
// operationContext — Provider.Read has no side effects, so cancelling one mid-flight
// only loses an observation the next refresh takes again, and that is what lets
// Ctrl-C stop a refresh promptly instead of waiting out every remaining resource.
func newRefreshCommand(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:           "refresh <environment>",
		Short:         "Reconcile recorded state with what providers actually report",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			environment := args[0]

			ro, closeRun, err := openRun(cmd, opts, "refresh", environment)
			if err != nil {
				return err
			}
			defer closeRun()
			rw := ro.Report()

			reg, _, regDiags, closePlugins := stateOnlyRegistry(opts, environment)
			defer closePlugins()
			if regDiags.HasErrors() {
				regDiags.Render(cmd.ErrOrStderr())
				return errProviderInstances
			}
			backend, closeBackend, err := backendFor(cmd.Context(), opts)
			if err != nil {
				return err
			}
			defer closeBackend()

			return withLockedEnvironment(environment, "refresh", backend, cmd.ErrOrStderr(), func(ctx context.Context) error {
				st, err := backend.Get(ctx, environment)
				if err != nil {
					return finishRefresh(cmd.ErrOrStderr(), rw, report.RefreshResult{}, err)
				}
				// State names the plugins here: refresh reads no configuration
				// at all.
				if stateDiags := ensureStateProviders(reg, st, nil); stateDiags.HasErrors() {
					stateDiags.Render(cmd.ErrOrStderr())
					return finishRefresh(cmd.ErrOrStderr(), rw, report.RefreshResult{}, errProviderInstances)
				}

				// The same hook plan, apply and destroy pass, so the four
				// commands cannot drift into reporting different things. st is
				// still exactly as loaded here; applyObservations below mutates
				// it, and the comparison classifyObservation makes has to
				// happen first.
				obs, ds := refresh.Refresh(ctx, st, reg, opts.Parallelism, perProviderParallelism, readRetryPolicy(),
					observationHook(ro, st))
				renderDiagnostics(cmd.ErrOrStderr(), rw, ds)

				// Built before applyObservations mutates st below:
				// classifyObservation compares against what st recorded before
				// this refresh ran, and afterwards that would be comparing a
				// resource's post-refresh state against itself.
				result := buildRefreshResult(st, obs)

				wrote := applyObservations(st, obs, ro.Out())

				if wrote {
					if err := backend.Put(ctx, environment, st); err != nil {
						return finishRefresh(cmd.ErrOrStderr(), rw, result, fmt.Errorf("writing refreshed state: %w", err))
					}
				}

				if ds.HasErrors() {
					return finishRefresh(cmd.ErrOrStderr(), rw, result,
						errors.New("refresh completed with errors; some resources could not be read"))
				}
				return finishRefresh(cmd.ErrOrStderr(), rw, result, nil)
			})
		},
	}
}

// applyObservations decides, per resource in st, what to do with what obs learned
// about it, writing progress lines to out as it goes. It reports whether st was
// mutated at all: an empty st mutates nothing and reports false, which lets the caller
// skip backend.Put rather than write an unchanged document and bump Serial for a run
// that read nothing.
//
// st.Addresses() is sorted, so the loop's order — and stdout with it — is
// deterministic regardless of which read finished first.
//
// The switch has four arms, not three, because a map lookup that misses returns
// Observation's zero value, which has both Err == nil and State == nil: the exact
// shape of "the provider reports this gone" if tested with a plain
// `if o.State == nil`. A missing observation is the absence of a report, not a report
// of absence, and must leave state untouched as a read error does. refresh.Refresh
// returns one Observation per address this loop ranges, so that arm is unreachable
// today; it is guarded anyway, because the failure mode on the other side of removing
// it is silent state deletion.
func applyObservations(st *state.State, obs refresh.Observations, out io.Writer) bool {
	wrote := false
	for _, addr := range st.Addresses() {
		o, ok := obs[addr.String()]
		switch {
		case !ok:
			// No observation at all for a resource recorded in state. Should
			// not happen — treated as unknown, not as gone, for the same
			// reason a read error is.
			fmt.Fprintf(out, "  %s: no observation returned; leaving state unchanged\n", addr)
		case o.Err != nil:
			// Reality is unknown, so state is left exactly as it was —
			// never removed, never overwritten. Writing "gone" here would
			// make the next plan propose recreating infrastructure that may
			// well still exist.
			fmt.Fprintf(out, "  %s: could not refresh (see diagnostics)\n", addr)
		case o.State == nil:
			// The provider affirmatively reports this resource gone. The
			// only case that removes it from state.
			fmt.Fprintf(out, "  %s: no longer exists; removed from state\n", addr)
			st.Remove(addr)
			wrote = true
		default:
			// What drifted, not merely that a read happened: the command whose
			// whole job is detecting drift has to say what it found.
			//
			// attributeChanges rather than a comparison written here: it already
			// exists for the report, and it formats through report.Format, the
			// single redaction path. A private diff in this file would be a
			// second place for a secret to reach a terminal.
			before, _ := st.Get(addr)
			changes := attributeChanges(before, o.State)
			st.Set(o.State)
			wrote = true
			if len(changes) == 0 {
				fmt.Fprintf(out, "  %s: refreshed, no drift\n", addr)
				break
			}
			fmt.Fprintf(out, "  %s: DRIFTED\n", addr)
			for _, c := range changes {
				fmt.Fprintf(out, "      %s: %s -> %s\n", c.Attribute, orNone(c.Before), orNone(c.After))
			}
		}
	}
	return wrote
}

// orNone renders an absent side of a drift line. An attribute that appeared or
// disappeared is drift as much as one that changed value, and an empty string
// would render it as though it had become blank.
func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
