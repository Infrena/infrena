// Package report is the machine-readable output apply, refresh and validate
// write to --output: newline-delimited JSON, one self-describing object per
// line, each carrying a "type" field. A frontend tails the file; the last
// line is always the outcome.
//
// `infrena plan --output` writes this format too, carrying its artifact on a
// single "plan" line (see PlanLine), so a frontend tails one format for
// every command. That line is the ONE exception to the rule below, and it is
// an exception rather than a softening of it. A plan artifact
// (internal/planner.Plan) is a single JSON document with cleartext values,
// saved at 0600, because M6 reads it back to APPLY it — it is a replayable
// INPUT (PLAN.md §12.1, §12.2). Everything else this package writes is a
// REPORT of a run that already happened, consumed by a wider audience (a web
// frontend), so every value it carries is redacted: see Format below. Do not
// give the plan artifact a second redaction path, and do not let any other
// line kind carry a raw value.
package report

import (
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/infrena/infrena/pkg/value"
)

// Version is the wire format's schema version, written on every meta line.
// It is the single most important field in the format: it is the handshake
// that lets the format evolve later without breaking a consumer that only
// understands an older version, which is why it is always present and
// always first.
//
// Version 2 added the "plan" line (see PlanLine), so that
// `infrena plan --output` writes this format rather than a second one. A
// consumer written against version 1 can tell from the meta line that a
// line kind it does not know may appear.
//
// Version 3 added MigrateResult, the result line `infrena state migrate
// --check` writes. A consumer written against version 2 has never seen a
// result line carrying a "status", and the meta line is how it finds out one
// may appear.
const Version = 3

// Format renders one attribute value the way every line in this package
// must: through pkg/value.Format, the engine's one redaction path, so a
// sensitive value never reaches a report as anything but "<sensitive>".
// Every caller in this tree that puts a value.Value into a report.* type
// must go through this rather than reading v.Raw directly.
//
// value.ReportFormatOptions, not a package-local copy — see its doc comment
// for why a report of what already happened (this package's whole job)
// takes different Unknown text than a plan's promise about the future
// (value.PlanFormatOptions), while sharing the same quoting as both: this is
// a diff-like listing (an observation's before/after), the same shape a
// plan's Before/After is.
func Format(v value.Value) string {
	return value.Format(v, value.ReportFormatOptions)
}

// Writer serializes NDJSON lines to an underlying io.Writer, one line per
// call, safe for concurrent use.
//
// It exists because two of this package's producers document exactly this
// requirement: executor.Options.OnEvent's doc comment says it "may be called
// concurrently from multiple workers; a receiver that is not itself safe for
// concurrent use must serialize its own access", and the OnObservation hook
// added to refresh.Refresh carries the identical contract. Writer is that
// serialization, so neither caller has to build its own.
type Writer struct {
	mu sync.Mutex
	w  io.Writer
	// approvedBy is stamped onto the result line, so the value is set once for
	// a run rather than carried to each of finishApply's ten call sites — where
	// the one that forgot it would be the failure path, and a run that failed
	// after somebody approved it is exactly the one an audit asks about.
	approvedBy string
}

// SetApprovedBy records who or what approved this run. See ApplyResult.ApprovedBy:
// it is a record, not a permission.
func (w *Writer) SetApprovedBy(s string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.approvedBy = s
}

// NewWriter wraps w. Nothing here opens or closes files — that is the
// caller's business, same as every other io.Writer in this codebase.
func NewWriter(w io.Writer) *Writer {
	return &Writer{w: w}
}

// writeLine marshals v and appends a single newline, both under the lock —
// one Write call to the underlying io.Writer per line, so two goroutines
// racing this method cannot interleave their bytes into a line neither of
// them wrote, and cannot produce a line missing its trailing newline either.
func (w *Writer) writeLine(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	w.mu.Lock()
	defer w.mu.Unlock()
	_, err = w.w.Write(data)
	return err
}

// Meta writes the first line of every run this package reports on.
func (w *Writer) Meta(command, environment string, startedAt time.Time) error {
	return w.writeLine(metaLine{
		Type:        "meta",
		Version:     Version,
		Command:     command,
		Environment: environment,
		StartedAt:   startedAt,
	})
}

type metaLine struct {
	Type        string    `json:"type"`
	Version     int       `json:"version"`
	Command     string    `json:"command"`
	Environment string    `json:"environment"`
	StartedAt   time.Time `json:"startedAt"`
}

// PlanLine carries a plan artifact inside a report stream, so that
// `infrena plan --output` writes ONE format rather than a second one (spec
// 2.4). It is always the last line before the result.
//
// Plan is json.RawMessage rather than a decoded type, and that is deliberate
// twice over. This package must not import internal/planner, which would
// invert the dependency and drag the planner into a package plugin authors
// compile against. And the artifact is the executor's complete instruction
// set: carrying its bytes verbatim means a large integer attribute cannot be
// reshaped in transit, the hazard planner.DecodePlan's UseNumber guards on
// the way in.
//
// It is the ONE line in this package that is not redacted, and the exception
// is the whole reason the two formats were kept apart until now: a plan
// artifact holds cleartext values because apply --plan reads it back and
// needs the real ones (PLAN.md 12.1). A file containing this line is
// therefore as sensitive as a plan artifact has always been, and is written
// 0600 for the same reason.
type PlanLine struct {
	Type string          `json:"type"`
	Plan json.RawMessage `json:"plan"`
}

// WritePlan writes the plan artifact line.
func (w *Writer) WritePlan(artifact json.RawMessage) error {
	return w.writeLine(PlanLine{Type: "plan", Plan: artifact})
}

// Event is one apply/destroy progress notification, one per
// executor.Event. Event.Message on the executor side is already redacted
// (executor.Event's own doc comment: pre-formatted text, never a raw
// attribute) — this type carries it through unchanged rather than
// re-deriving it, since executor is not this package's business to trust
// twice.
type Event struct {
	Type    string    `json:"type"`
	Event   string    `json:"event"` // started|succeeded|failed|retrying|skipped
	Address string    `json:"address"`
	Op      string    `json:"op"` // create|update|replace|destroy|forget
	Attempt int       `json:"attempt,omitempty"`
	Error   string    `json:"error,omitempty"`
	Message string    `json:"message,omitempty"`
	At      time.Time `json:"at"`
}

// WriteEvent writes one apply-progress line.
func (w *Writer) WriteEvent(e Event) error {
	e.Type = "event"
	return w.writeLine(e)
}

// AttributeChange is one attribute's before/after, both already run through
// Format — never a raw value.Value, which would bypass redaction entirely.
type AttributeChange struct {
	Attribute string `json:"attribute"`
	Before    string `json:"before,omitempty"`
	After     string `json:"after,omitempty"`
}

// Observation is one resource's refresh result.
type Observation struct {
	Type    string            `json:"type"`
	Address string            `json:"address"`
	Status  string            `json:"status"` // unchanged|changed|removed|error
	Changes []AttributeChange `json:"changes,omitempty"`
	Error   string            `json:"error,omitempty"`
	At      time.Time         `json:"at"`
}

// WriteObservation writes one refresh-progress line.
func (w *Writer) WriteObservation(o Observation) error {
	o.Type = "observation"
	return w.writeLine(o)
}

// DiagnosticOrigin locates a diagnostic in source. Deliberately structured,
// not a pre-formatted "file:line:column" string: a frontend that wants to
// link to a file and line needs the parts separately, and flattening them
// here would make it re-parse what diag.Diagnostic already had as fields.
type DiagnosticOrigin struct {
	File   string   `json:"file,omitempty"`
	Line   int      `json:"line,omitempty"`
	Column int      `json:"column,omitempty"`
	Module []string `json:"module,omitempty"`
}

// Diagnostic mirrors diag.Diagnostic's four parts (PLAN.md §44) as four
// separate fields — summary, detail, action, origin — because flattening
// them into one string is exactly what §44 exists to prevent: a diagnostic
// says what is wrong, where, what was expected, and what to do, and a
// frontend consuming this format needs to render or filter on each part
// independently.
type Diagnostic struct {
	Type     string            `json:"type"`
	Severity string            `json:"severity"` // error|warning
	Summary  string            `json:"summary"`
	Detail   string            `json:"detail,omitempty"`
	Action   string            `json:"action,omitempty"`
	Origin   *DiagnosticOrigin `json:"origin,omitempty"`
	Related  []string          `json:"related,omitempty"`
}

// WriteDiagnostic writes one diagnostic line.
func (w *Writer) WriteDiagnostic(d Diagnostic) error {
	d.Type = "diagnostic"
	return w.writeLine(d)
}

// ApplyResult is apply's — and destroy's, which shares this shape since it
// runs the same executor.Apply and produces the same executor.Result —
// final line: what was applied, forgotten, failed and skipped.
//
// The rule every field here was checked against: list what changed (or
// needs attention), count what did not. Applied, Forgotten and Skipped stay
// LISTS rather than counts even though a consumer already saw one "event"
// line per operation during the run, same as RefreshResult's Drifted and
// Removed — see that type's doc comment for why a duplicate-of-the-stream
// list still earns its place in the final line. Skipped in particular is
// NOT this result's equivalent of RefreshResult.Unchanged, despite both
// being the "nothing happened here" case at first glance: an unchanged
// resource was actively confirmed correct, while a skipped one has an
// UNAPPLIED change still pending because a dependency failed — exactly the
// kind of thing a consumer needs to name, not merely count, to know what
// still needs remediation.
//
// Failed is keyed by planner.OpNode.ID() ("<verb>:<address>"), the same key
// executor.Result.Failed uses, for the same reason: a Replace is two nodes
// at one address, and an address alone cannot say which half failed.
//
// Error is set only when the run itself could not complete — configuration
// failed to compile, refresh failed, planning failed, the lock could not be
// taken. It is deliberately NOT set for a successful apply that had changes
// (the CLI's own errChanges sentinel, exit code 2): that is success, not
// failure, and a report consumer must be able to tell the two apart from
// this field alone.
type ApplyResult struct {
	Type string `json:"type"`
	// ApprovedBy is what `--approved-by` recorded: who or what approved this
	// run, as the operator spelled it — a pull request URL, a name, a change
	// ticket. Free text, written verbatim, and absent when the flag was not
	// passed.
	//
	// IT IS A RECORD, NOT A PERMISSION. infrena cannot tell a real pull-request
	// URL from an invented one, so nothing is allowed on the strength of it —
	// §38's `require_approval` is satisfied by a person confirming or by a
	// saved plan, never by this string. What it buys is that a run can say
	// under whose authority it happened, which is the part of an audit trail
	// that belongs in the free CLI.
	//
	// NO VERSION BUMP. Versions 2 and 3 marked new LINE KINDS, which a consumer
	// could not have anticipated; an optional field on a line it already
	// decodes is ignored by any consumer that does not know it and read by one
	// that does.
	ApprovedBy string            `json:"approved_by,omitempty"`
	Applied    []string          `json:"applied,omitempty"`
	Forgotten  []string          `json:"forgotten,omitempty"`
	Failed     map[string]string `json:"failed,omitempty"`
	Skipped    []string          `json:"skipped,omitempty"`
	Error      string            `json:"error,omitempty"`
}

// WriteApplyResult writes apply's final line.
func (w *Writer) WriteApplyResult(r ApplyResult) error {
	r.Type = "result"
	if r.ApprovedBy == "" {
		w.mu.Lock()
		r.ApprovedBy = w.approvedBy
		w.mu.Unlock()
	}
	return w.writeLine(r)
}

// RefreshResult is refresh's final line: which addresses drifted, which were
// found removed, how many were read successfully with no change, and which
// could not be read at all.
//
// Unchanged is a COUNT, not a list, and every other field here is a list —
// this is the one field that fails the "list what changed, count what did
// not" rule if it stays a list. The stream already carried one
// "observation" line per resource as refresh ran (including every unchanged
// one), so a consumer that wants to know WHICH resources were confirmed
// correct already saw that in the stream; repeating every address again
// here only duplicates data already delivered, and on a large environment
// where most resources are unchanged it is the one field that could make
// this line's size scale with the environment rather than with what
// actually needs attention. Drifted, Removed and Errors stay lists despite
// the same stream duplication argument technically applying to them too,
// because unlike Unchanged they are exactly what a consumer acts on —
// naming them is the final line's whole job.
type RefreshResult struct {
	Type      string   `json:"type"`
	Drifted   []string `json:"drifted,omitempty"`
	Removed   []string `json:"removed,omitempty"`
	Unchanged int      `json:"unchanged,omitempty"`
	Errors    []string `json:"errors,omitempty"`
	// Error is set only when refresh itself could not run at all (e.g. the
	// lock could not be taken) — never merely because some resources failed
	// to read; those are already named in Errors above.
	Error string `json:"error,omitempty"`
}

// WriteRefreshResult writes refresh's final line.
func (w *Writer) WriteRefreshResult(r RefreshResult) error {
	r.Type = "result"
	return w.writeLine(r)
}

// ValidateResult is validate's final line: whether the configuration is
// valid. validate has no progress to report, so its stream is exactly meta,
// then diagnostics, then this — still valid NDJSON, and the same format as
// apply and refresh.
type ValidateResult struct {
	Type  string `json:"type"`
	Valid bool   `json:"valid"`
}

// WriteValidateResult writes validate's final line.
func (w *Writer) WriteValidateResult(r ValidateResult) error {
	r.Type = "result"
	return w.writeLine(r)
}

// MigrateResult is `state migrate --check`'s final line: which of the three
// situations the two backends are in, and which environments the answer is
// about.
//
// STATUS IS A WORD, NOT THE EXIT CODE. The command's exit code is what a
// shell script branches on, because a shell has nothing better; a consumer
// reading this file has, and asking it to map 3 back to "already complete"
// is asking it to keep a copy of a table that lives somewhere else. The two
// are derived from one decision inside the CLI, so they cannot disagree, and
// only one of them is self-describing.
//
// The words are "none" (no `migrate_from:` block, so nothing is pending),
// "pending", "complete", "conflict" and "error".
//
// Environments names the environments the status is ABOUT: the ones to be
// copied when pending, the ones that differ when conflicting. Not every
// environment involved — naming the ones that agree alongside the ones that
// do not is how the ones a person has to look at get buried.
type MigrateResult struct {
	Type   string `json:"type"`
	Status string `json:"status"`
	// Environments the status is about, omitted when there are none.
	Environments []string `json:"environments,omitempty"`
	// Error is set only when the check could not be made at all — a backend
	// that would not open, configuration that would not read. It is NEVER
	// set for a conflict, which is an answer rather than a failure.
	Error string `json:"error,omitempty"`
}

// WriteMigrateResult writes the migration check's final line.
func (w *Writer) WriteMigrateResult(r MigrateResult) error {
	r.Type = "result"
	return w.writeLine(r)
}
