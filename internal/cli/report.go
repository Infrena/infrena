package cli

import (
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/internal/executor"
	"github.com/infrena/infrena/internal/planner"
	"github.com/infrena/infrena/internal/refresh"
	"github.com/infrena/infrena/internal/state"
	"github.com/infrena/infrena/internal/version"
	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/report"
	"github.com/infrena/infrena/pkg/resource"
	"github.com/infrena/infrena/pkg/value"
)

// openReport opens --output for a command that streams pkg/report's
// NDJSON format (validate, apply, refresh), writes the mandatory first
// "meta" line, and returns the writer to use plus a close function to defer.
//
// opts.Output == "" returns a nil *report.Writer and a no-op close; every caller
// below treats a nil Writer as "do not report", the same nil-means-no-hook
// convention executor.Options.OnEvent uses.
//
// 0600: even with every value redacted, this file names the caller's infrastructure
// by address and type.
func openReport(opts *GlobalOptions, command, environment string, errOut io.Writer) (*report.Writer, func(), error) {
	if opts.Output == "" {
		return nil, func() {}, nil
	}
	f, err := os.OpenFile(opts.Output, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, func() {}, fmt.Errorf("opening --output %q: %w", opts.Output, err)
	}
	rw := report.NewWriter(f)
	// Set once for the run, before anything can fail: a run that failed AFTER
	// somebody approved it is exactly the one an audit asks about, so the
	// record must not depend on reaching a success path.
	rw.SetApprovedBy(opts.ApprovedBy)
	if err := rw.Meta(command, environment, version.Version(), time.Now()); err != nil {
		f.Close()
		return nil, func() {}, fmt.Errorf("writing --output meta line: %w", err)
	}
	return rw, func() {
		if cerr := f.Close(); cerr != nil {
			fmt.Fprintf(errOut, "warning: failed to close --output file %q: %v\n", opts.Output, cerr)
		}
	}, nil
}

// renderDiagnostics renders ds to stderr and, when rw is non-nil, writes one
// "diagnostic" line per entry as well. A write failure is reported, not fatal: the
// diagnostics already reached the user on stderr, and a broken --output stream must
// not mask that.
func renderDiagnostics(errOut io.Writer, rw *report.Writer, ds diag.Diagnostics) {
	ds.Render(errOut)
	if rw == nil {
		return
	}
	for _, d := range ds {
		if err := rw.WriteDiagnostic(toReportDiagnostic(d)); err != nil {
			fmt.Fprintf(errOut, "warning: failed to write --output diagnostic: %v\n", err)
			return
		}
	}
}

// toReportDiagnostic converts one diag.Diagnostic to the wire shape, keeping its
// four parts — summary, detail, action, origin — as separate fields rather than
// flattening them into a message.
func toReportDiagnostic(d diag.Diagnostic) report.Diagnostic {
	rd := report.Diagnostic{
		Severity: severityWire(d.Severity),
		Summary:  d.Summary,
		Detail:   d.Detail,
		Action:   d.Action,
	}
	if d.Origin.File != "" || d.Origin.Line != 0 || d.Origin.Column != 0 || len(d.Origin.Module) > 0 {
		rd.Origin = &report.DiagnosticOrigin{
			File:   d.Origin.File,
			Line:   d.Origin.Line,
			Column: d.Origin.Column,
			Module: append([]string(nil), d.Origin.Module...),
		}
	}
	if len(d.Related) > 0 {
		// Sort a copy: encoding must be deterministic without reordering the
		// caller's own slice.
		related := append([]address.Address(nil), d.Related...)
		address.Sort(related)
		rd.Related = make([]string, 0, len(related))
		for _, a := range related {
			rd.Related = append(rd.Related, a.String())
		}
	}
	return rd
}

// severityWire is the lowercase wire spelling for diag.Severity, kept independent of
// diag.Severity.String(), which is capitalized for human-readable rendering.
//
// Anything that is not the warning constant reports as "error", the more actionable
// answer: silently downgrading an unrecognised severity would hide a real problem
// from a consumer filtering on it.
func severityWire(s diag.Severity) string {
	if s == diag.SeverityWarning {
		return "warning"
	}
	return "error"
}

// toReportEvent converts one executor.Event to the wire shape. Event.Message is
// already redacted by whatever set it, so it is carried through verbatim rather than
// re-derived.
func toReportEvent(e executor.Event) report.Event {
	ev := report.Event{
		Event:   e.Kind.String(),
		Address: e.Address.String(),
		Op:      e.Op.String(),
		Attempt: e.Attempt,
		Message: e.Message,
		At:      e.At,
	}
	if e.Err != nil {
		ev.Error = e.Err.Error()
	}
	return ev
}

// finishApply writes an apply-shaped command's final "result" line, when rw is
// non-nil, and returns err unchanged, so every return site in a RunE can hand its
// error straight through instead of repeating the nil-check. Shared by apply and
// destroy: both run through executor.Apply.
//
// err is folded into result.Error unless it is errChanges. A successful apply that
// found and applied changes returns errChanges purely to drive the process exit code,
// and recording that as a failure would tell a frontend the run failed when it did
// exactly what it was asked to.
func finishApply(errOut io.Writer, rw *report.Writer, result report.ApplyResult, err error) error {
	if rw == nil {
		return err
	}
	if err != nil && err != errChanges {
		result.Error = err.Error()
	}
	if werr := rw.WriteApplyResult(result); werr != nil {
		fmt.Fprintf(errOut, "warning: failed to write --output result: %v\n", werr)
	}
	return err
}

// finishRefresh is finishApply's counterpart for `infrena refresh`.
func finishRefresh(errOut io.Writer, rw *report.Writer, result report.RefreshResult, err error) error {
	if rw == nil {
		return err
	}
	if err != nil {
		result.Error = err.Error()
	}
	if werr := rw.WriteRefreshResult(result); werr != nil {
		fmt.Fprintf(errOut, "warning: failed to write --output result: %v\n", werr)
	}
	return err
}

// applyResultFrom builds report.ApplyResult from one executor.Result — the same
// fields executor.Render puts on the terminal, so the report and the summary cannot
// disagree about what was applied, forgotten, failed or skipped.
func applyResultFrom(res executor.Result) report.ApplyResult {
	applied := append([]address.Address(nil), res.Applied...)
	address.Sort(applied)

	out := report.ApplyResult{}
	for _, a := range applied {
		out.Applied = append(out.Applied, a.String())
	}

	forgotten := append([]address.Address(nil), res.Forgotten...)
	address.Sort(forgotten)
	for _, a := range forgotten {
		out.Forgotten = append(out.Forgotten, a.String())
	}

	if len(res.Failed) > 0 {
		out.Failed = make(map[string]string, len(res.Failed))
		for id, ferr := range res.Failed {
			out.Failed[id] = ferr.Error()
		}
	}

	skipped := append([]string(nil), res.Skipped...)
	sort.Strings(skipped)
	out.Skipped = skipped

	return out
}

// classifyObservation turns one refresh.Observation into a report.Observation,
// comparing it against what st recorded for the same address before this refresh ran.
//
// Called twice: concurrently, as the streaming per-resource hook inside
// refresh.Refresh, which never mutates st, and again sequentially to build the
// aggregate result. Both must run before applyObservations mutates st, or st.Get
// reports the new state as "before" and a resource is compared against itself.
func classifyObservation(st *state.State, o refresh.Observation) report.Observation {
	out := report.Observation{Address: o.Address.String(), At: time.Now()}
	switch {
	case o.Err != nil:
		out.Status = "error"
		out.Error = o.Err.Error()
	case o.State == nil:
		out.Status = "removed"
	default:
		before, _ := st.Get(o.Address)
		changes := attributeChanges(before, o.State)
		if len(changes) == 0 {
			out.Status = "unchanged"
		} else {
			out.Status = "changed"
			out.Changes = changes
		}
	}
	return out
}

// attributeChanges reports every attribute that differs between before and after,
// both run through report.Format so a sensitive attribute reaches the line only as
// "<sensitive>".
//
// A plain diff over the union of attribute names, deliberately: refresh compares two
// actual reads of the same resource taken at different times, so it has no
// schema-driven ForceNew or computed distinction to make the way the planner's own
// diff does over desired configuration.
func attributeChanges(before, after *resource.ResourceState) []report.AttributeChange {
	var beforeAttrs, afterAttrs map[string]value.Value
	if before != nil {
		beforeAttrs = before.Attributes
	}
	if after != nil {
		afterAttrs = after.Attributes
	}

	var out []report.AttributeChange
	for _, name := range unionAttributeNames(beforeAttrs, afterAttrs) {
		b, bok := beforeAttrs[name]
		a, aok := afterAttrs[name]
		if bok && aok && b.Equal(a) {
			continue
		}
		change := report.AttributeChange{Attribute: name}
		if bok {
			change.Before = report.FormatPlanned(b)
		}
		if aok {
			change.After = report.FormatPlanned(a)
		}
		out = append(out, change)
	}
	return out
}

// planChanges converts a plan into the redacted account of it that apply and
// destroy report before they execute (report.PlanChanges).
//
// The conversion is the redaction: report.PlanChanges holds strings where
// planner.Operation holds value.Value, so there is no way to build one except
// through report.Format, and no way for a raw value to reach the stream by someone
// forgetting. That is why pkg/report defines its own types rather than re-exporting
// the planner's.
//
// Before and After collapse into one per-attribute diff here rather than in the
// consumer. Unlike attributeChanges above, an attribute whose value is unchanged is
// kept: this is a plan rather than a diff of observed state, and a frontend showing
// a create has nothing to show if the attributes merely being set are dropped.
func planChanges(p *planner.Plan, stage string) report.PlanChanges {
	pc := report.PlanChanges{
		Stage:       stage,
		Project:     p.Project,
		Environment: p.Environment,
		ConfigHash:  p.ConfigHash,
		StateSerial: p.StateSerial,
		StateHash:   p.StateHash,
		CreatedAt:   p.CreatedAt,
		// Never nil: `"changes":null` and `"changes":[]` mean the same thing
		// to a reader who checks the length and different things to one who
		// iterates without checking, and a no-change run is exactly when this
		// is empty.
		Changes: []report.ResourceChange{},
	}
	for _, op := range p.Operations {
		// A NoOp is not a change, and `changes` is what this field is. The
		// planner keeps them for Render's verbose mode; carrying them here
		// would put every attribute of every untouched resource into the
		// report on every run, and would make a no-change run
		// indistinguishable from a busy one without the consumer
		// reimplementing Plan.HasChanges.
		if op.Kind == planner.OpNoOp {
			continue
		}
		rc := report.ResourceChange{
			Address:  op.Address.String(),
			Type:     op.Type,
			Provider: op.Provider,
			Kind:     op.Kind.String(),
			Changes:  planAttributeChanges(op.Before, op.After),
		}
		for _, r := range op.Reasons {
			rc.Reasons = append(rc.Reasons, report.ChangeReason{
				Attribute: r.Attribute,
				ForceNew:  r.ForceNew,
				Note:      r.Note,
			})
		}
		for _, d := range op.Dependents {
			rc.Dependents = append(rc.Dependents, d.String())
		}
		pc.Changes = append(pc.Changes, rc)
	}
	return pc
}

// planAttributeChanges is attributeChanges for a plan's two attribute maps:
// every attribute either side names, in sorted order.
//
// FormatPlanned, not Format: a plan says what apply WOULD do, so an unknown
// value here is "(known after apply)" and not the "(unknown)" that every
// other line in a report uses. Same redaction, different tense.
func planAttributeChanges(before, after map[string]value.Value) []report.AttributeChange {
	var out []report.AttributeChange
	for _, name := range unionAttributeNames(before, after) {
		change := report.AttributeChange{Attribute: name}
		if b, ok := before[name]; ok {
			change.Before = report.FormatPlanned(b)
		}
		if a, ok := after[name]; ok {
			change.After = report.FormatPlanned(a)
		}
		out = append(out, change)
	}
	return out
}

// unionAttributeNames lists every key across the given maps, sorted, so a map's
// iteration order cannot reach the output.
func unionAttributeNames(maps ...map[string]value.Value) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range maps {
		for name := range m {
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	sort.Strings(out)
	return out
}

// buildRefreshResult aggregates every observation into the four buckets refresh's
// final "result" line reports: Drifted, Removed and Errors in sorted address order,
// and Unchanged as a bare count.
//
// Called after refresh.Refresh returns but before applyObservations mutates st — see
// classifyObservation for why that order matters.
func buildRefreshResult(st *state.State, obs refresh.Observations) report.RefreshResult {
	var result report.RefreshResult
	addrs := make([]string, 0, len(obs))
	for k := range obs {
		addrs = append(addrs, k)
	}
	sort.Strings(addrs)

	for _, k := range addrs {
		line := classifyObservation(st, obs[k])
		switch line.Status {
		case "changed":
			result.Drifted = append(result.Drifted, line.Address)
		case "removed":
			result.Removed = append(result.Removed, line.Address)
		case "unchanged":
			result.Unchanged++
		case "error":
			result.Errors = append(result.Errors, line.Address)
		}
	}
	return result
}
