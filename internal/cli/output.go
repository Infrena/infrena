package cli

import (
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/infrena/infrena/internal/executor"
	"github.com/infrena/infrena/internal/planner"
	"github.com/infrena/infrena/internal/refresh"
	"github.com/infrena/infrena/internal/state"
	"github.com/infrena/infrena/pkg/report"
)

// runOutput is the single decision about where a run's output goes, made once
// at the top of a command instead of at every print site.
//
// THE RULE (spec 2.1): --output silences stdout completely and moves
// everything into the file. stdout carries progress and the product; stderr
// carries diagnostics and is untouched by this type, because a run that fails
// must say so on a channel the operator sees whether or not a frontend is
// reading the file.
//
// Progress does NOT go to stderr. That was considered and rejected: it splits
// one narrative across two channels for a human who is watching neither in
// isolation, and the machine-readable file already exists for the frontend
// that wants the structured form.
//
// The reason this is a type rather than an `if opts.Output != ""` at each
// print: a command added later inherits the rule instead of having to
// remember it. The same reason root.go's PersistentPreRunE is one place.
type runOutput struct {
	out      io.Writer
	progress *progressRenderer
	report   *report.Writer
}

// openRun opens a command's output for the whole run: the report file when
// --output is set, the progress renderer, and the writer the command's own
// product is printed through.
//
// The returned close function must be deferred. It stops the progress
// renderer BEFORE closing the report file, so no heartbeat can land after
// the command has printed its summary and nothing can be written to a file
// that is already closed.
func openRun(cmd *cobra.Command, opts *GlobalOptions, command, environment string) (*runOutput, func(), error) {
	rw, closeReport, err := openReport(opts, command, environment, cmd.ErrOrStderr())
	if err != nil {
		return nil, func() {}, err
	}

	ro := &runOutput{report: rw}
	if opts.Output != "" {
		// One decision, made here: nothing reaches stdout for the rest of
		// the run. io.Discard rather than a nil writer so every print site
		// stays an unconditional Fprint, and a nil renderer writer for the
		// same reason on the progress half.
		ro.out = io.Discard
		ro.progress = newProgressRenderer(nil, time.Now)
	} else {
		ro.out = cmd.OutOrStdout()
		ro.progress = newProgressRenderer(cmd.OutOrStdout(), time.Now)
	}

	return ro, func() {
		ro.progress.Stop()
		closeReport()
	}, nil
}

// Out is the writer a command prints its product through — the rendered
// plan, the result summary, a teardown notice. io.Discard when --output is
// set.
func (ro *runOutput) Out() io.Writer { return ro.out }

// Progress is the run's progress renderer. Never nil, so a caller does not
// have to check: with --output set it is a renderer over a nil writer, which
// renders nothing.
func (ro *runOutput) Progress() *progressRenderer { return ro.progress }

// Report is the run's NDJSON writer, or nil when --output is absent — the
// same nil-means-no-report convention openReport already returns and every
// caller in this package already handles.
func (ro *runOutput) Report() *report.Writer { return ro.report }

// observationHook feeds one refresh observation to both consumers, and is
// shared so that plan, apply, destroy and refresh cannot drift into
// reporting different things. Nil-safe on both halves: the report is nil
// without --output, and the progress renderer renders nothing with it.
//
// st is the state as it was loaded, BEFORE refresh applies anything it
// learned, because that is what classifyObservation — the one conversion in
// this package, in report.go — compares each observation against. Passing
// state mutated by this same refresh would compare a resource against
// itself.
func observationHook(ro *runOutput, st *state.State) func(refresh.Observation) {
	return func(o refresh.Observation) {
		ro.Progress().Observation(o)
		if rw := ro.Report(); rw != nil {
			// Best effort and silent, the same rule OnEvent's call site
			// already documents: this runs on worker goroutines, so there is
			// no race-free place here to report a write failure. It surfaces
			// once, non-concurrently, from the final result line.
			_ = rw.WriteObservation(classifyObservation(st, o))
		}
	}
}

// reportPlan writes one plan line for a run that is about to do something,
// or does nothing when there is no report to write.
//
// THE RULE, and it is the whole reason this is a function rather than four
// call sites that each remember it: stage "proposed" goes beside the human
// render, and stage "executing" goes immediately before executor.Apply.
// Stated that way it survives a fifth command being added; stated as "apply
// writes two lines" it would not. The two are not the same plan — apply and
// destroy re-plan inside the environment lock — which is why both exist.
//
// Best effort and silent, the same terms as observationHook and eventHook:
// an apply's product is the change to the infrastructure, and failing a run
// that is already holding the environment lock because a line did not reach
// the disk would be the wrong trade. A write failure surfaces once, from the
// final result line, which finishApply does report.
func reportPlan(ro *runOutput, p *planner.Plan, stage string) {
	rw := ro.Report()
	if rw == nil || p == nil {
		return
	}
	_ = rw.WritePlanChanges(planChanges(p, stage))
}

// eventHook is observationHook's counterpart for executor progress, shared
// by apply, destroy and the saved-plan path for the same reason: three
// copies of this wiring is three chances for one of them to stop rendering
// or stop reporting without anyone noticing.
func eventHook(ro *runOutput) func(executor.Event) {
	return func(e executor.Event) {
		ro.Progress().Event(e)
		if rw := ro.Report(); rw != nil {
			// Best effort and silent — see observationHook.
			_ = rw.WriteEvent(toReportEvent(e))
		}
	}
}
