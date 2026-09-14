// Package report is the machine-readable output apply, refresh and validate
// write to --output: newline-delimited JSON, one self-describing object per
// line, each carrying a "type" field. A frontend tails the file; the last
// line is always the outcome.
//
// This is deliberately not what `infra plan --output` writes. A plan
// artifact (internal/planner.Plan) is a single JSON document with cleartext
// values, saved at 0600, because M6 reads it back to APPLY it — it is a
// replayable INPUT (PLAN.md §12.1, §12.2). What this package writes is a
// REPORT of a run that already happened, consumed by a wider audience (a web
// frontend), so every value it carries is redacted: see Format below. The
// two are distinct on purpose and must stay that way — do not point
// `infra plan --output` at this package, and do not give the plan artifact a
// second redaction path.
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
// understands version 1, which is why it is always present and always
// first.
const Version = 1

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
	Type      string            `json:"type"`
	Applied   []string          `json:"applied,omitempty"`
	Forgotten []string          `json:"forgotten,omitempty"`
	Failed    map[string]string `json:"failed,omitempty"`
	Skipped   []string          `json:"skipped,omitempty"`
	Error     string            `json:"error,omitempty"`
}

// WriteApplyResult writes apply's final line.
func (w *Writer) WriteApplyResult(r ApplyResult) error {
	r.Type = "result"
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
