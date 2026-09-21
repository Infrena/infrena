// Package report is the machine-readable output apply, refresh and validate
// write to --output: newline-delimited JSON, one self-describing object per
// line, each carrying a "type" field. A frontend tails the file; the last line
// is always the outcome.
//
// `infrena plan --output` writes this format too, carrying its artifact on a
// single "plan" line (see PlanLine), so that a frontend tails one format for
// every command.
//
// That line is the one exception to the rule that governs everything else here:
// every value this package writes is redacted through Format, because a report
// describes a run that already happened and is read by a wider audience than the
// operator. A plan artifact is not a report but a replayable input — `apply
// --plan` reads it back — so it carries cleartext values and the file is written
// 0600. Do not give the plan artifact a second redaction path, and do not let
// any other line kind carry a raw value.
package report

import (
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/infrena/infrena/pkg/value"
)

// FormatPlanned is Format for a value that has not happened yet — the
// before/after of a plan line, and nothing else in this package.
//
// The two differ on exactly one point, and the difference is a claim rather than
// a wording preference: an unknown value in a report of a completed run is an
// anomaly ("(unknown)"), while an unknown value in a plan is a promise ("(known
// after apply)"). A frontend showing a plan line beside a terminal showing the
// same plan must not find two words for one thing.
//
// It is still value.Format, so redaction is identical and a sensitive value
// never renders either way.
func FormatPlanned(v value.Value) string {
	return value.Format(v, value.PlanFormatOptions)
}

// Version is the wire format's schema version, written first on every meta line.
// It is the handshake that lets the format evolve without breaking a consumer
// that understands only an older version.
//
// Version 2 added the "plan" line (see PlanLine); version 3 added MigrateResult,
// the first result line to carry a "status"; version 4 added the "applying" line
// (see PlanChanges), which apply and destroy write before they execute. A
// consumer reads the meta line to learn that a line kind it does not know may
// appear.
const Version = 4

// Format renders one attribute value the way every line in this package must:
// through pkg/value.Format, the engine's one redaction path, so that a sensitive
// value never reaches a report as anything but "<sensitive>". Every caller that
// puts a value.Value into a report type must go through this rather than reading
// v.Raw directly.
//
// It uses value.ReportFormatOptions rather than a package-local copy. See
// FormatPlanned for why a report of what already happened takes different
// Unknown text than a plan's promise about the future.
func Format(v value.Value) string {
	return value.Format(v, value.ReportFormatOptions)
}

// Writer serializes NDJSON lines to an underlying io.Writer, one line per call,
// safe for concurrent use.
//
// The producers of these lines — the executor's event hook and refresh's
// observation hook — may be called concurrently from several workers and require
// a receiver that serializes its own access. Writer is that serialization, so
// neither caller has to build its own.
type Writer struct {
	mu sync.Mutex
	w  io.Writer
	// approvedBy is stamped onto the result line, so the value is set once for a
	// run rather than carried to each of the result-writing call sites — where
	// the one that forgot it would be the failure path, and a run that failed
	// after somebody approved it is exactly the one an audit asks about.
	approvedBy string
}

// SetApprovedBy records who or what approved this run. See
// ApplyResult.ApprovedBy: it is a record, not a permission.
func (w *Writer) SetApprovedBy(s string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.approvedBy = s
}

// NewWriter wraps w. Nothing here opens or closes files; that is the caller's
// business.
func NewWriter(w io.Writer) *Writer {
	return &Writer{w: w}
}

// writeLine marshals v and appends a single newline, both under the lock — one
// Write call to the underlying io.Writer per line, so two goroutines racing this
// method cannot interleave their bytes into a line neither of them wrote, and
// cannot produce a line missing its trailing newline either.
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
//
// The infrena argument is the build that produced the report, which is not the
// same question as version: version says how to decode these lines, infrena says
// what wrote them. A report archived for a year is often read by somebody
// establishing what happened rather than replaying it, and "which build did
// this" is the first thing they ask.
//
// It is empty for a development build, which is more honest than stamping a
// number that names no release.
func (w *Writer) Meta(command, environment, infrena string, startedAt time.Time) error {
	return w.writeLine(metaLine{
		Type:        "meta",
		Version:     Version,
		Infrena:     infrena,
		Command:     command,
		Environment: environment,
		StartedAt:   startedAt,
	})
}

type metaLine struct {
	Type        string    `json:"type"`
	Version     int       `json:"version"`
	Infrena     string    `json:"infrena,omitempty"`
	Command     string    `json:"command"`
	Environment string    `json:"environment"`
	StartedAt   time.Time `json:"startedAt"`
}

// PlanLine carries a plan artifact inside a report stream, so that `infrena plan
// --output` writes one format rather than a second one. It is always the last
// line before the result.
//
// Plan is json.RawMessage rather than a decoded type, and that is deliberate
// twice over. This package must not import the planner, which would invert the
// dependency and drag it into a package plugin authors compile against. And the
// artifact is the executor's complete instruction set: carrying its bytes
// verbatim means a large integer attribute cannot be reshaped in transit.
//
// It is the one line in this package that is not redacted, because `apply
// --plan` reads it back and needs the real values. A file containing this line
// is therefore as sensitive as a plan artifact, and is written 0600 for the same
// reason.
type PlanLine struct {
	Type string          `json:"type"`
	Plan json.RawMessage `json:"plan"`
}

// WritePlan writes the plan artifact line.
func (w *Writer) WritePlan(artifact json.RawMessage) error {
	return w.writeLine(PlanLine{Type: "plan", Plan: artifact})
}

// Stages a plan line can carry. A run emits at most one of each.
//
// The two are not the same plan, which is the reason the field exists: apply and
// destroy plan once for the preview and again inside the environment lock, and
// only the second is what the executor is handed. A consumer that drew a
// progress bar from the proposed line and never looked at the executing one
// would be drawing the plan that did not run.
const (
	// StageProposed is the plan as it stood before the environment lock was
	// taken, or, for `apply --plan`, the artifact as it was read. It is emitted
	// even when the run stops here — no changes, or a saved plan refused because
	// state moved — which is the point: it is the only account of what the run
	// was going to do.
	StageProposed = "proposed"
	// StageExecuting is the plan the executor received. Its presence is the
	// claim that execution started.
	StageExecuting = "executing"
)

// ChangeReason explains one attribute's contribution to an operation. It mirrors
// the planner's own type, which this package cannot import — see PlanLine for
// why that dependency stays pointed the way it is.
//
// It names attributes, never values, so unlike everything else here it needs no
// redaction: sensitivity is per-leaf, and a reason has no way to redact.
type ChangeReason struct {
	// Attribute is the attribute the reason is about.
	Attribute string `json:"attribute,omitempty"`
	// ForceNew reports that changing this attribute forces a replacement.
	ForceNew bool `json:"force_new,omitempty"`
	// Note is free text for a reason no attribute explains.
	Note string `json:"note,omitempty"`
}

// ResourceChange is one resource's proposed change, redacted.
//
// Before and After arrive as one per-attribute diff rather than as two maps,
// which is not how the planner holds them. A consumer renders a before/after
// pair; giving it the same AttributeChange an observation line already carries
// means one renderer serves both, and means the merge happens once here instead
// of in every consumer.
type ResourceChange struct {
	Address string `json:"address"`
	Type    string `json:"type"`
	// Provider is the instance this change runs against.
	Provider string `json:"provider,omitempty"`
	// Kind is the operation as a word — create, update, replace, destroy,
	// forget — for the same reason MigrateResult carries its status as one.
	Kind       string            `json:"kind"`
	Changes    []AttributeChange `json:"changes,omitempty"`
	Reasons    []ChangeReason    `json:"reasons,omitempty"`
	Dependents []string          `json:"dependents,omitempty"`
}

// PlanChanges is what a run is about to do, as a report rather than as an
// artifact.
//
// This is not the "plan" line and must never become it. PlanLine carries the
// executor's instruction set with cleartext values because `apply --plan` reads
// it back; this carries a redacted account of the same plan so that a frontend
// can show a run before it happens. The type name differs for a load-bearing
// reason: reading a saved plan means scanning a stream for a line typed "plan"
// and applying what is found, so a redacted plan wearing that type would be
// applied with "<sensitive>" where its values used to be.
//
// The fingerprints are carried because they are what a refusal later in the run
// will be about: a saved plan is refused when state has moved, and a frontend
// holding StateSerial can say which state that was.
type PlanChanges struct {
	Type        string           `json:"type"`
	Stage       string           `json:"stage"`
	Project     string           `json:"project,omitempty"`
	Environment string           `json:"environment"`
	ConfigHash  string           `json:"config_hash,omitempty"`
	StateSerial uint64           `json:"state_serial"`
	StateHash   string           `json:"state_hash,omitempty"`
	CreatedAt   time.Time        `json:"created_at"`
	Changes     []ResourceChange `json:"changes"`
}

// WritePlanChanges writes one plan line. Type is stamped here rather than taken
// from the caller, the same way WriteObservation stamps its own.
func (w *Writer) WritePlanChanges(pc PlanChanges) error {
	pc.Type = "applying"
	return w.writeLine(pc)
}

// Event is one apply or destroy progress notification, one per executor event.
// Message is already redacted on the executor side — pre-formatted text, never a
// raw attribute — and is carried through unchanged rather than re-derived.
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

// DiagnosticOrigin locates a diagnostic in source. Deliberately structured, not
// a pre-formatted "file:line:column" string: a frontend that wants to link to a
// file and line needs the parts separately.
type DiagnosticOrigin struct {
	File   string   `json:"file,omitempty"`
	Line   int      `json:"line,omitempty"`
	Column int      `json:"column,omitempty"`
	Module []string `json:"module,omitempty"`
}

// Diagnostic mirrors the engine's diagnostic as four separate fields — summary,
// detail, action, origin — rather than one string. A diagnostic says what is
// wrong, where, what was expected and what to do, and a frontend needs to render
// or filter on each part independently.
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

// ApplyResult is apply's final line — and destroy's, which runs the same
// executor and produces the same result: what was applied, forgotten, failed and
// skipped.
//
// The rule every field here was checked against: list what changed or needs
// attention, count what did not. Skipped is a list rather than a count even
// though it looks like the "nothing happened here" case, because a skipped
// resource has an unapplied change still pending after a dependency failed, and
// a consumer needs to name those to know what still needs remediation.
//
// Failed is keyed by "<verb>:<address>", the same key the executor uses: a
// replace is two nodes at one address, and an address alone cannot say which
// half failed.
type ApplyResult struct {
	Type string `json:"type"`
	// ApprovedBy is what `--approved-by` recorded: who or what approved this
	// run, as the operator spelled it — a pull request URL, a name, a change
	// ticket. Free text, written verbatim, and absent when the flag was not
	// passed.
	//
	// It is a record, not a permission. infrena cannot tell a real pull-request
	// URL from an invented one, so nothing is allowed on the strength of it:
	// `require_approval` is satisfied by a person confirming or by a saved plan,
	// never by this string. What it buys is that a run can say under whose
	// authority it happened.
	//
	// Adding it needed no version bump. Earlier bumps marked new line kinds,
	// which a consumer could not have anticipated; an optional field on a line it
	// already decodes is ignored by any consumer that does not know it.
	ApprovedBy string `json:"approved_by,omitempty"`
	// StateSerial is the state's serial after the run, tying an archived report
	// to the exact state version it produced.
	//
	// It is the strongest link a report can carry. Addresses say what was touched
	// and timestamps say when, but a serial says which state this run made — so
	// two reports can be ordered against one another, a gap between serials shows
	// a run that was never archived, and a report can be matched against a state
	// file rather than believed on its own.
	//
	// Zero means no state was written: a failure before the first write, or a
	// command that writes none.
	StateSerial uint64            `json:"state_serial,omitempty"`
	Applied     []string          `json:"applied,omitempty"`
	Forgotten   []string          `json:"forgotten,omitempty"`
	Failed      map[string]string `json:"failed,omitempty"`
	Skipped     []string          `json:"skipped,omitempty"`
	// Error is set only when the run itself could not complete — configuration
	// failed to compile, refresh failed, planning failed, the lock could not be
	// taken. It is deliberately not set for a successful apply that had changes:
	// that is success, not failure, and a consumer must be able to tell the two
	// apart from this field alone.
	Error string `json:"error,omitempty"`
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
// found removed, how many were read successfully with no change, and which could
// not be read at all.
//
// Unchanged is a count while everything else here is a list. The stream already
// carried one "observation" line per resource, including every unchanged one, so
// naming them again would only duplicate data already delivered — and on a large
// environment it is the one field that would make this line's size scale with
// the environment rather than with what needs attention. Drifted, Removed and
// Errors stay lists because they are exactly what a consumer acts on.
type RefreshResult struct {
	Type      string   `json:"type"`
	Drifted   []string `json:"drifted,omitempty"`
	Removed   []string `json:"removed,omitempty"`
	Unchanged int      `json:"unchanged,omitempty"`
	Errors    []string `json:"errors,omitempty"`
	// Error is set only when refresh itself could not run at all (e.g. the lock
	// could not be taken) — never merely because some resources failed to read;
	// those are already named in Errors above.
	Error string `json:"error,omitempty"`
}

// WriteRefreshResult writes refresh's final line.
func (w *Writer) WriteRefreshResult(r RefreshResult) error {
	r.Type = "result"
	return w.writeLine(r)
}

// ValidateResult is validate's final line: whether the configuration is valid.
// validate has no progress to report, so its stream is exactly meta, then
// diagnostics, then this — still valid NDJSON, and the same format as apply and
// refresh.
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
// The status is a word, not the exit code. A shell script branches on the exit
// code because a shell has nothing better; a consumer reading this file has, and
// asking it to map 3 back to "already complete" is asking it to keep a copy of a
// table that lives somewhere else. Both are derived from one decision, so they
// cannot disagree, and only one of them is self-describing.
type MigrateResult struct {
	Type string `json:"type"`
	// Status is one of "none" (no `migrate_from:` block, so nothing is pending),
	// "pending", "complete", "conflict" or "error".
	Status string `json:"status"`
	// Environments the status is about, omitted when there are none: the ones to
	// be copied when pending, the ones that differ when conflicting. Not every
	// environment involved — naming the ones that agree alongside the ones that
	// do not is how the ones a person has to look at get buried.
	Environments []string `json:"environments,omitempty"`
	// Error is set only when the check could not be made at all — a backend that
	// would not open, configuration that would not read. It is never set for a
	// conflict, which is an answer rather than a failure.
	Error string `json:"error,omitempty"`
}

// WriteMigrateResult writes the migration check's final line.
func (w *Writer) WriteMigrateResult(r MigrateResult) error {
	r.Type = "result"
	return w.writeLine(r)
}
