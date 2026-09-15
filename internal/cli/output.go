package cli

import (
	"io"
	"time"

	"github.com/spf13/cobra"

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
