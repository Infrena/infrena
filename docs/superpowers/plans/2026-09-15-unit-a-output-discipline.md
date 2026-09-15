# Unit A — Output Discipline Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make infrena report progress as it works, and make `--output` produce a file a web frontend can tail with stdout left completely silent.

**Architecture:** A single `progressRenderer` type in `internal/cli` owns stdout during a run and is the only writer to it while work is in flight; it is constructed nil-valued when `--output` is set, so silencing is one decision made once rather than a condition threaded through every print site. The executor and refresh hooks that feed it already exist — `executor.Options.OnEvent` is wired, and `refresh.Refresh`'s `onObservation` parameter is passed `nil` by two of its three callers. `plan --output` stops writing a bare JSON document and writes the same NDJSON stream every other command writes, carrying the plan artifact on a final `plan` line.

**Tech Stack:** Go 1.27, Cobra, `gopkg.in/yaml.v3`. No new dependencies — the two-package budget holds.

**Spec:** `docs/superpowers/specs/2026-09-15-cli-output-discover-init-design.md` (§2)

## Global Constraints

- Go 1.27.0 is the module floor (`go.mod`) and the `mise.toml` pin. `mise` is not active in non-interactive shells: use the `make` targets or `export PATH="$HOME/.local/share/mise/shims:$PATH"` first, or a bare `go` resolves to 1.20 and fails.
- **Third-party dependency budget is two packages: `github.com/spf13/cobra` and `gopkg.in/yaml.v3`.** Adding any other is out of scope for this plan.
- **The core engine must not know about AWS.** No provider-specific branching anywhere in this unit.
- **`pkg/report` redacts through `pkg/value.Format`.** Any value reaching a report line goes through `report.Format`, never `v.Raw`.
- **Human output to stdout, diagnostics to stderr.** Progress is human output and belongs on stdout (§2.1), never stderr.
- Every command in `internal/cli` is constructed with `SilenceUsage: true, SilenceErrors: true` and returns errors for `Execute()` to map to an exit code.
- Run `gofmt -l .` and `go vet ./...` before each commit; both must be clean.
- Commit messages: plain English, no em-dashes, minimal, and **never** mention Claude, AI, or the model.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/cli/progress.go` *(new)* | `progressRenderer`: the only stdout writer during a run. Owns the mutex, the heartbeat timer, and the line formats. |
| `internal/cli/progress_test.go` *(new)* | Renderer tests against a fake clock and a buffer. |
| `internal/cli/output.go` *(new)* | `runOutput`: decides once whether stdout is live, and hands out the writer every command prints through. |
| `internal/cli/planstream.go` *(new)* | Writing and sniffing the plan-carrying NDJSON envelope. |
| `internal/cli/planstream_test.go` *(new)* | Envelope round trip, and the `meta`-line hazard. |
| `internal/cli/plan.go` | Stop printing to stdout under `--output`; write the stream instead of a bare artifact; wire the refresh hook. |
| `internal/cli/apply.go` | Wire the refresh hook and the progress renderer; add the approval-unobtainable check. |
| `internal/cli/apply_saved.go` | Read either envelope. |
| `internal/cli/destroy.go`, `refresh.go`, `validate.go` | Route prints through `runOutput`. |
| `internal/planner/plan.go` | `DecodePlan` refuses a document carrying `"type"`. |
| `pkg/report/report.go` | `Version` to 2; `Plan` line kind and `WritePlan`. |
| `internal/cli/root.go` | `ExitNoApproval = 77`, mapped in `Execute`. |

---

## Task 1: The plan artifact refuses a typed document

Closing the §2.4 hazard first, alone, because every later task in this plan depends on the sniff being safe and this guard holds even for a caller that skips the sniffer.

**Files:**
- Modify: `internal/planner/plan.go` (`DecodePlan`, around line 432)
- Test: `internal/planner/plan_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `planner.DecodePlan([]byte) (*Plan, error)` — unchanged signature, now erroring on a document with a top-level `"type"` field.

- [ ] **Step 1: Write the failing test**

```go
// TestDecodePlanRefusesAReportLine pins the hazard in spec 2.4: json.Decoder
// reads one value and stops, so pointed at a report stream DecodePlan would
// consume the meta line. Unknown fields are ignored and report.Version is 1,
// the same integer PlanVersion checks, so it would succeed and yield a plan
// with no operations that applies nothing, silently.
func TestDecodePlanRefusesAReportLine(t *testing.T) {
	meta := []byte(`{"type":"meta","version":1,"command":"plan","environment":"dev"}`)

	_, err := DecodePlan(meta)
	if err == nil {
		t.Fatal("DecodePlan accepted a report meta line as a plan artifact")
	}
	if !strings.Contains(err.Error(), "meta") {
		t.Errorf("error does not name what it actually got: %v", err)
	}
}

// A plan artifact has never carried a "type" field, so nothing legitimate is
// refused by the guard above.
func TestDecodePlanStillAcceptsABareArtifact(t *testing.T) {
	p := &Plan{Version: PlanVersion, Project: "p", Environment: "dev"}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		t.Fatal(err)
	}

	got, err := DecodePlan(data)
	if err != nil {
		t.Fatalf("DecodePlan refused a bare artifact: %v", err)
	}
	if got.Environment != "dev" {
		t.Errorf("Environment = %q, want dev", got.Environment)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestDecodePlanRefuses|TestDecodePlanStillAccepts' ./internal/planner/ -v`
Expected: `TestDecodePlanRefusesAReportLine` FAILs with "DecodePlan accepted a report meta line as a plan artifact". The second test passes already.

- [ ] **Step 3: Write minimal implementation**

In `internal/planner/plan.go`, add the field to `planWire` and the guard at the top of `DecodePlan`, before the version check:

```go
// Type is never set on a plan artifact. It exists on the wire struct only so
// DecodePlan can REFUSE a report line, which is the one document that would
// otherwise decode cleanly into this struct: report.Version is 1, the same
// integer PlanVersion checks for, and encoding/json ignores unknown fields by
// default. See the spec's 2.4. A caller that reaches here without going
// through the envelope sniffer still fails loudly rather than applying a plan
// with no operations.
Type string `json:"type,omitempty"`
```

```go
	if w.Type != "" {
		return nil, fmt.Errorf(
			"this is a %q line from a report stream, not a plan artifact\n"+
				"Point --plan at the file `infrena plan --output` wrote, not at a line from it",
			w.Type)
	}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/planner/ -v 2>&1 | tail -20`
Expected: PASS, no regressions in the package.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./internal/planner/
git add internal/planner/plan.go internal/planner/plan_test.go
git commit -m "Refuse a report line where a plan artifact is expected

DecodePlan reads one JSON value and stops, so a report stream decodes as
its meta line. Unknown fields are ignored and the report version is also 1,
so it would succeed and apply a plan with no operations."
```

---

## Task 2: `report` learns the plan line

**Files:**
- Modify: `pkg/report/report.go`
- Test: `pkg/report/report_test.go`

**Interfaces:**
- Consumes: Task 1's guarantee that a `"type"` field is refused by `DecodePlan`.
- Produces: `report.Version = 2`; `report.PlanLine{Type string, Plan json.RawMessage}`; `(*report.Writer).WritePlan(json.RawMessage) error`.

- [ ] **Step 1: Write the failing test**

```go
func TestWritePlanEmitsATypedLineCarryingTheArtifact(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)

	artifact := json.RawMessage(`{"version":1,"environment":"dev"}`)
	if err := w.WritePlan(artifact); err != nil {
		t.Fatal(err)
	}

	var got struct {
		Type string          `json:"type"`
		Plan json.RawMessage `json:"plan"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &got); err != nil {
		t.Fatal(err)
	}
	if got.Type != "plan" {
		t.Errorf("type = %q, want plan", got.Type)
	}
	// Carried through verbatim: the artifact is the executor's instruction set,
	// and re-encoding it risks changing a number's exact text, which is the
	// hazard DecodePlan's UseNumber already guards on the way in.
	if string(got.Plan) != string(artifact) {
		t.Errorf("plan = %s, want %s", got.Plan, artifact)
	}
}

func TestVersionIsTwo(t *testing.T) {
	// The format gained a line kind, so a consumer that only understands
	// version 1 must be able to tell. PLAN.md section 61 keeps this
	// independent of every other format version.
	if Version != 2 {
		t.Errorf("Version = %d, want 2", Version)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestWritePlan|TestVersionIsTwo' ./pkg/report/ -v`
Expected: FAIL — `w.WritePlan undefined` and `Version = 1, want 2`.

- [ ] **Step 3: Write minimal implementation**

In `pkg/report/report.go`, change `Version` to `2` and update its doc comment to say version 2 added the `plan` line. Then add:

```go
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./pkg/report/ -v 2>&1 | tail -20`
Expected: PASS. If an existing test asserts `Version == 1`, update it — the version genuinely moved.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./pkg/report/
git add pkg/report/
git commit -m "Report format 2 carries a plan line

So that plan --output writes the same stream every other command writes
instead of a second format. The artifact travels as raw bytes, which keeps
this package free of an internal/planner import and stops a large integer
being reshaped in transit."
```

---

## Task 3: `progressRenderer`

The renderer alone, with no caller, so its concurrency and heartbeat are tested in isolation before anything depends on them.

**Files:**
- Create: `internal/cli/progress.go`
- Test: `internal/cli/progress_test.go`

**Interfaces:**
- Consumes: `executor.Event`, `executor.EventKind` (`EventStarted`/`Succeeded`/`Failed`/`Retrying`/`Skipped`), `refresh.Observation`.
- Produces:
  - `func newProgressRenderer(w io.Writer, now func() time.Time) *progressRenderer` — `w` nil means render nothing.
  - `(*progressRenderer).Event(executor.Event)`
  - `(*progressRenderer).Observation(refresh.Observation)`
  - `(*progressRenderer).Stop()` — halts the heartbeat and must be called before anything else writes to the same writer.

- [ ] **Step 1: Write the failing test**

```go
package cli

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infrena/infrena/internal/executor"
	"github.com/infrena/infrena/internal/planner"
	"github.com/infrena/infrena/pkg/address"
)

// clock is a manually advanced time source. executor.Options.Now already
// injects one in this codebase, so the renderer takes the same seam rather
// than inventing a second way to control time in tests.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func addr(name string) address.Address { return address.Address{Name: name} }

func TestRendererPairsStartAndFinishWithADuration(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	var buf bytes.Buffer
	r := newProgressRenderer(&buf, c.now)
	defer r.Stop()

	r.Event(executor.Event{Kind: executor.EventStarted, Address: addr("vpc-app1"), Op: planner.OpCreate, At: c.now()})
	c.advance(15700 * time.Millisecond)
	r.Event(executor.Event{Kind: executor.EventSucceeded, Address: addr("vpc-app1"), Op: planner.OpCreate, At: c.now()})

	got := buf.String()
	if !strings.Contains(got, "Creating vpc-app1...") {
		t.Errorf("no start line:\n%s", got)
	}
	if !strings.Contains(got, "done (15.7s)") {
		t.Errorf("no duration on the finish line:\n%s", got)
	}
}

// Append-only is the property, not a style choice: stdout here is as often a
// CI log or a redirected file as a terminal, and an escape sequence in either
// is noise a reader has to strip. It also means no TTY detection, so there is
// one behaviour to test rather than two.
func TestRendererWritesNoEscapeSequences(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	var buf bytes.Buffer
	r := newProgressRenderer(&buf, c.now)
	defer r.Stop()

	r.Event(executor.Event{Kind: executor.EventStarted, Address: addr("a"), Op: planner.OpCreate, At: c.now()})
	c.advance(time.Second)
	r.Event(executor.Event{Kind: executor.EventFailed, Address: addr("a"), Op: planner.OpCreate, At: c.now()})

	if strings.ContainsRune(buf.String(), '\x1b') || strings.ContainsRune(buf.String(), '\r') {
		t.Errorf("renderer emitted cursor control:\n%q", buf.String())
	}
}

// A nil writer is how --output silences stdout: the decision is made once, at
// construction, rather than threaded through every call site as a condition.
func TestRendererWithNoWriterRendersNothingAndDoesNotPanic(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	r := newProgressRenderer(nil, c.now)
	defer r.Stop()

	r.Event(executor.Event{Kind: executor.EventStarted, Address: addr("a"), Op: planner.OpCreate, At: c.now()})
	r.Observation(refreshObservationFixture())
}

// OnEvent's own doc comment says it may be called concurrently from several
// workers, so the renderer is the serialization. Run with -race.
func TestRendererIsSafeForConcurrentUse(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	var buf bytes.Buffer
	r := newProgressRenderer(&buf, c.now)
	defer r.Stop()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a := addr(strings.Repeat("x", i%7+1))
			r.Event(executor.Event{Kind: executor.EventStarted, Address: a, Op: planner.OpCreate, At: c.now()})
			r.Event(executor.Event{Kind: executor.EventSucceeded, Address: a, Op: planner.OpCreate, At: c.now()})
		}(i)
	}
	wg.Wait()

	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line != "" && !strings.HasPrefix(line, "Creating ") {
			t.Fatalf("interleaved line: %q", line)
		}
	}
}
```

Add the fixture helper in the same file:

```go
func refreshObservationFixture() refresh.Observation {
	return refresh.Observation{Address: addr("a")}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestRenderer' ./internal/cli/ -v`
Expected: FAIL — `newProgressRenderer` undefined.

- [ ] **Step 3: Write minimal implementation**

Create `internal/cli/progress.go`. Read `internal/refresh/refresh.go`'s `Observation` type first and match its real field names — the fixture above and the `Observation` method must use what that type actually has.

```go
package cli

// progressRenderer is the ONE writer to stdout while work is in flight.
//
// It exists because executor.Options.OnEvent's doc comment says it "may be
// called concurrently from multiple workers; a receiver that is not itself
// safe for concurrent use must serialize its own access", and the identical
// contract applies to refresh.Refresh's onObservation. Two goroutines
// printing to stdout without this is interleaved bytes in a line neither of
// them wrote.
//
// A NIL WRITER RENDERS NOTHING, and that is how --output silences stdout
// (spec 2.1): the decision is made once at construction instead of becoming
// a condition every print site has to remember. It is the same
// nil-means-no-hook convention executor.Options.OnEvent and
// refresh.Refresh's onObservation already use.
//
// LINES ARE APPEND-ONLY. No cursor control, no in-place rewriting, no
// spinner. stdout here is as often a CI log or a redirected file as it is a
// terminal, and escape sequences in either are noise a reader has to strip
// mentally. It also means the renderer needs no TTY detection, so there is
// one behaviour to test rather than two.
type progressRenderer struct {
	mu      sync.Mutex
	w       io.Writer
	now     func() time.Time
	started map[string]time.Time
	done    chan struct{}
	stopped bool
}

// heartbeat is how long an operation may run without output before the
// renderer says it is still working, and again between each such line.
//
// It is not decoration. The live AWS suite recorded an IAM role create at
// 24.7 s and one subnet delete at 2 m 52 s. Without a heartbeat those are
// indistinguishable from a hang, and the only recovery a user has is
// Ctrl-C, which this engine deliberately makes expensive because the second
// SIGINT leaves the lock behind.
const heartbeat = 15 * time.Second

func newProgressRenderer(w io.Writer, now func() time.Time) *progressRenderer {
	r := &progressRenderer{w: w, now: now, started: map[string]time.Time{}, done: make(chan struct{})}
	if w != nil {
		go r.tick()
	}
	return r
}
```

```go
// line is the ONE write path. Everything else formats and calls this, so the
// lock is taken in one place and a caller cannot forget it.
func (r *progressRenderer) line(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.unlockedLine(format, args...)
}

// unlockedLine is line's body for callers that already hold the mutex — tick
// holds it across the whole sweep so two heartbeats cannot interleave.
func (r *progressRenderer) unlockedLine(format string, args ...any) {
	if r.w == nil || r.stopped {
		return
	}
	fmt.Fprintf(r.w, format+"\n", args...)
}

func (r *progressRenderer) Event(e executor.Event) {
	if r.w == nil {
		return
	}
	key := e.Address.String()
	verb := verbFor(e.Op)

	r.mu.Lock()
	defer r.mu.Unlock()
	switch e.Kind {
	case executor.EventStarted:
		r.started[key] = e.At
		r.beats[key] = 0
		r.unlockedLine("%s %s...", verb, key)
	case executor.EventRetrying:
		// The entry STAYS: another attempt is coming, and dropping it here
		// would silence the heartbeat for the rest of the operation.
		r.unlockedLine("%s %s... retrying (attempt %d)", verb, key, e.Attempt+1)
	case executor.EventSucceeded, executor.EventFailed, executor.EventSkipped:
		outcome := map[executor.EventKind]string{
			executor.EventSucceeded: "done",
			executor.EventFailed:    "failed",
			executor.EventSkipped:   "skipped",
		}[e.Kind]
		started, ok := r.started[key]
		delete(r.started, key)
		delete(r.beats, key)
		if !ok {
			// No paired start: report the outcome without inventing a
			// duration rather than printing one measured from the zero time.
			r.unlockedLine("%s %s... %s", verb, key, outcome)
			return
		}
		r.unlockedLine("%s %s... %s (%s)", verb, key, outcome, humanDuration(e.At.Sub(started)))
	}
}

func (r *progressRenderer) Observation(o refresh.Observation) {
	if r.w == nil {
		return
	}
	r.line("Reading %s... done", o.Address.String())
}

// tick prints a heartbeat for any operation that has been running longer than
// the next multiple of `heartbeat` without a terminal event.
//
// It reads r.now() rather than the ticker's own timestamp so the fake clock in
// the tests drives it, the same seam executor.Options.Now already provides.
func (r *progressRenderer) tick() {
	t := time.NewTicker(heartbeat / 3)
	defer t.Stop()
	for {
		select {
		case <-r.done:
			return
		case <-t.C:
			now := r.now()
			r.mu.Lock()
			// Sorted, so two operations crossing the same beat print in a
			// stable order rather than Go's randomised map order.
			keys := make([]string, 0, len(r.started))
			for k := range r.started {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				elapsed := now.Sub(r.started[k])
				beat := int(elapsed / heartbeat)
				if beat > r.beats[k] {
					r.beats[k] = beat
					r.unlockedLine("  %s... still working (%s)", k, humanDuration(elapsed))
				}
			}
			r.mu.Unlock()
		}
	}
}

// Stop halts the heartbeat and closes the renderer to further output. It must
// be called before anything else writes to the same writer, or a heartbeat
// lands in the middle of the final summary.
func (r *progressRenderer) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return
	}
	r.stopped = true
	close(r.done)
}

func verbFor(op planner.OpKind) string {
	switch op {
	case planner.OpCreate:
		return "Creating"
	case planner.OpUpdate:
		return "Updating"
	case planner.OpReplace:
		return "Replacing"
	case planner.OpDestroy:
		return "Destroying"
	case planner.OpForget:
		return "Forgetting"
	default:
		return "Processing"
	}
}

// humanDuration reads the way the live AWS suite reports times: 15.7s under a
// minute, 2m52s at or above one.
func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%dm%ds", int(d/time.Minute), int((d%time.Minute)/time.Second))
}
```

Add `beats map[string]int` to the struct and initialise it in `newProgressRenderer` beside `started`. Check `planner.OpKind`'s real constant names in `internal/planner/plan.go` (they start at `OpNoOp`) and match them; check `refresh.Observation`'s real field names in `internal/refresh/refresh.go` and match those too.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race -run 'TestRenderer' ./internal/cli/ -v`
Expected: PASS, including under `-race`.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./internal/cli/
git add internal/cli/progress.go internal/cli/progress_test.go
git commit -m "Render progress while the executor and refresh work

One writer owns stdout during a run, because the event hook is called from
several workers at once. A nil writer renders nothing, which is how output
mode will silence stdout. Lines are append only so a CI log stays readable."
```

---

## Task 4: `runOutput` decides once whether stdout is live

**Files:**
- Create: `internal/cli/output.go`
- Modify: `internal/cli/report.go` (`openReport` returns a `*runOutput`)
- Test: `internal/cli/output_test.go`

**Interfaces:**
- Consumes: Task 3's `newProgressRenderer`; existing `openReport`.
- Produces:
  - `type runOutput struct { … }`
  - `func openRun(cmd *cobra.Command, opts *GlobalOptions, command, environment string) (*runOutput, func(), error)`
  - `(*runOutput).Out() io.Writer` — the writer every command prints its product through; `io.Discard` when `--output` is set.
  - `(*runOutput).Progress() *progressRenderer`
  - `(*runOutput).Report() *report.Writer` — nil when `--output` is absent.

- [ ] **Step 1: Write the failing test**

```go
func TestOpenRunSilencesStdoutWhenOutputIsSet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "run.ndjson")
	cmd, stdout := commandWithBuffers(t)

	ro, closeRun, err := openRun(cmd, &GlobalOptions{Output: path}, "apply", "dev")
	if err != nil {
		t.Fatal(err)
	}
	defer closeRun()

	fmt.Fprint(ro.Out(), "this must not reach the terminal")
	if stdout.Len() != 0 {
		t.Errorf("stdout got %q, want nothing", stdout.String())
	}
	if ro.Report() == nil {
		t.Error("Report() is nil with --output set")
	}
}

func TestOpenRunWritesToStdoutWhenOutputIsAbsent(t *testing.T) {
	cmd, stdout := commandWithBuffers(t)

	ro, closeRun, err := openRun(cmd, &GlobalOptions{}, "apply", "dev")
	if err != nil {
		t.Fatal(err)
	}
	defer closeRun()

	fmt.Fprint(ro.Out(), "hello")
	if stdout.String() != "hello" {
		t.Errorf("stdout = %q, want hello", stdout.String())
	}
	if ro.Report() != nil {
		t.Error("Report() is non-nil with no --output")
	}
}
```

Write `commandWithBuffers` in the test file if `internal/cli`'s existing test helpers do not already provide one — check `internal/cli/context_test_helpers_test.go` and `main_test.go` first and reuse what is there.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestOpenRun' ./internal/cli/ -v`
Expected: FAIL — `openRun` undefined.

- [ ] **Step 3: Write minimal implementation**

Create `internal/cli/output.go`:

```go
package cli

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
```

`openRun` calls the existing `openReport`, then:
- `--output` set: `out = io.Discard`, `progress = newProgressRenderer(nil, time.Now)`.
- absent: `out = cmd.OutOrStdout()`, `progress = newProgressRenderer(cmd.OutOrStdout(), time.Now)`.

Return a close func that calls `progress.Stop()` **before** `openReport`'s close, so no progress line can land after the file is closed.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run 'TestOpenRun' ./internal/cli/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./internal/cli/
git add internal/cli/output.go internal/cli/output_test.go internal/cli/report.go
git commit -m "Decide once where a run's output goes

Output mode silences stdout and moves everything into the file. Making it
one type rather than a condition at every print site means a command added
later inherits the rule instead of having to remember it."
```

---

## Task 5: Every command routes through `runOutput`

The behavioural change users see. This is where §2.1's table becomes true.

**Files:**
- Modify: `internal/cli/plan.go`, `apply.go`, `destroy.go`, `refresh.go`, `validate.go`
- Test: `internal/cli/output_test.go`

**Interfaces:**
- Consumes: Task 4's `openRun`, `runOutput.Out()`, `runOutput.Progress()`.
- Produces: no new symbols. Every `fmt.Fprint(cmd.OutOrStdout(), …)` in a command that takes `--output` becomes `fmt.Fprint(ro.Out(), …)`.

- [ ] **Step 1: Write the failing test**

```go
// The property, asserted directly rather than inferred from any one command's
// behaviour: with --output set, stdout is byte-empty. A frontend reading the
// file must not also have to strip a human-readable plan from stdout.
func TestOutputModeLeavesStdoutByteEmpty(t *testing.T) {
	for _, command := range []string{"validate", "plan", "apply", "refresh", "destroy"} {
		t.Run(command, func(t *testing.T) {
			dir := newProjectFixture(t)
			out := filepath.Join(t.TempDir(), "run.ndjson")

			stdout, _, _ := runCommand(t, dir, command, "dev", "--output", out, "--auto-approve")

			if stdout != "" {
				t.Errorf("%s --output wrote %q to stdout, want nothing", command, stdout)
			}
			if fi, err := os.Stat(out); err != nil || fi.Size() == 0 {
				t.Errorf("%s --output wrote no file: %v", command, err)
			}
		})
	}
}

// The other half: without --output, progress reaches stdout. A test asserting
// only the silence would pass against a command that prints nothing at all.
func TestWithoutOutputProgressReachesStdout(t *testing.T) {
	dir := newProjectFixture(t)

	stdout, _, _ := runCommand(t, dir, "apply", "dev", "--auto-approve")

	if !strings.Contains(stdout, "Creating ") {
		t.Errorf("no progress on stdout:\n%s", stdout)
	}
}
```

`newProjectFixture` and `runCommand` follow the existing patterns in `internal/cli/apply_test.go` — read it and reuse its fixture construction and its injected fake provider rather than writing a third way to build a project.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestOutputModeLeavesStdoutByteEmpty|TestWithoutOutputProgress' ./internal/cli/ -v`
Expected: FAIL — `plan` and `apply` write the rendered plan to stdout; no progress lines exist yet.

- [ ] **Step 3: Write minimal implementation**

In each command's `RunE`, replace the `openReport` call with `openRun`, then:

- `plan.go:148` — `fmt.Fprint(ro.Out(), planner.Render(…))`.
- `plan.go:108` — pass the hook instead of `nil`:

```go
obs, refreshDiags := refresh.Refresh(cmd.Context(), st, reg, opts.Parallelism, perProviderParallelism,
	observationHook(ro))
```
- `apply.go:276` (inside `computePlan`) — same change; `computePlan` takes the `*runOutput` in place of its current `rw *report.Writer` parameter, since it already needs both halves.
- `apply.go` — the plan render, the teardown notice, the "no changes remained" line and `executor.Render`'s summary all move to `ro.Out()`.
- `apply.go:214` — the `OnEvent` closure calls `ro.Progress().Event(e)` as well as `rw.WriteEvent`.
- `destroy.go`, `refresh.go`, `validate.go` — same substitution for their own prints.

Add the shared hook builder to `output.go`:

```go
// observationHook feeds one refresh observation to both consumers, and is
// shared so that plan, apply and refresh cannot drift into reporting
// different things. Nil-safe on both halves: report is nil without --output,
// and progress renders nothing with it.
func observationHook(ro *runOutput) func(refresh.Observation) {
	return func(o refresh.Observation) {
		ro.Progress().Observation(o)
		if rw := ro.Report(); rw != nil {
			// Best effort and silent, the same rule OnEvent's call site
			// already documents: this runs on worker goroutines, so there is
			// no race-free place here to report a write failure. It surfaces
			// once, non-concurrently, from the final result line.
			_ = rw.WriteObservation(toReportObservation(o))
		}
	}
}
```

Reuse `internal/cli/refresh.go`'s existing conversion for `toReportObservation` rather than writing a second one — move it to `report.go` if it is currently a local closure.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ 2>&1 | tail -20`
Expected: PASS. Existing tests that assert on stdout under `--output` will fail and are **correct to update** — that behaviour is what this task changes.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./...
git add internal/cli/
git commit -m "Output mode writes only the file, and progress reaches stdout

Plan and apply printed the rendered plan to stdout and then wrote the file
as well, so a frontend got both halves and had to sort them out. Plan and
apply also passed nil for the refresh observation hook, which is the phase
that dominates the wait on a real account."
```

---

## Task 6: `plan --output` writes the stream

**Files:**
- Create: `internal/cli/planstream.go`, `internal/cli/planstream_test.go`
- Modify: `internal/cli/plan.go` (the `opts.Output != ""` block, around line 152), `internal/cli/apply_saved.go`

**Interfaces:**
- Consumes: Task 1's `DecodePlan` guard; Task 2's `report.Writer.WritePlan`; Task 4's `runOutput`.
- Produces:
  - `func readSavedPlan(path string) (*planner.Plan, error)` — accepts either envelope.

- [ ] **Step 1: Write the failing test**

```go
// Saved plans already exist on disk, so the old bare artifact must keep
// working. Refusing it would break apply --plan for every plan saved before
// this release.
func TestReadSavedPlanAcceptsABareArtifact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plan.json")
	p := &planner.Plan{Version: planner.PlanVersion, Project: "proj", Environment: "dev"}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := readSavedPlan(path)
	if err != nil {
		t.Fatalf("readSavedPlan refused a bare artifact: %v", err)
	}
	if got.Environment != "dev" {
		t.Errorf("Environment = %q, want dev", got.Environment)
	}
}

func TestReadSavedPlanAcceptsAStream(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plan.ndjson")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	w := report.NewWriter(f)
	if err := w.Meta("plan", "dev", time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	artifact, err := json.Marshal(&planner.Plan{Version: planner.PlanVersion, Project: "proj", Environment: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.WritePlan(artifact); err != nil {
		t.Fatal(err)
	}
	f.Close()

	got, err := readSavedPlan(path)
	if err != nil {
		t.Fatalf("readSavedPlan refused a stream: %v", err)
	}
	if got.Environment != "dev" {
		t.Errorf("Environment = %q, want dev", got.Environment)
	}
}

// The hazard from spec 2.4, at the level a user meets it: a stream with no
// plan line is an error naming the problem, never an empty plan that applies
// nothing.
func TestReadSavedPlanRefusesAStreamWithNoPlanLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.ndjson")
	f, _ := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	w := report.NewWriter(f)
	_ = w.Meta("apply", "dev", time.Unix(0, 0))
	_ = w.WriteApplyResult(report.ApplyResult{})
	f.Close()

	_, err := readSavedPlan(path)
	if err == nil {
		t.Fatal("readSavedPlan accepted a report with no plan line")
	}
	if !strings.Contains(err.Error(), "no plan") {
		t.Errorf("error does not say what is missing: %v", err)
	}
}

// Round trip through the real command, which is the only test that proves the
// two halves agree about the format.
func TestPlanOutputRoundTripsThroughApply(t *testing.T) {
	dir := newProjectFixture(t)
	path := filepath.Join(t.TempDir(), "plan.ndjson")

	if _, _, code := runCommand(t, dir, "plan", "dev", "--output", path); code != ExitChanges {
		t.Fatalf("plan exit = %d, want %d", code, ExitChanges)
	}
	if _, _, code := runCommand(t, dir, "apply", "dev", "--plan", path); code != ExitChanges {
		t.Fatalf("apply --plan exit = %d, want %d", code, ExitChanges)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestReadSavedPlan|TestPlanOutputRoundTrips' ./internal/cli/ -v`
Expected: FAIL — `readSavedPlan` undefined.

- [ ] **Step 3: Write minimal implementation**

Create `internal/cli/planstream.go`:

```go
package cli

// readSavedPlan reads a plan artifact from either envelope.
//
// TWO SHAPES, and accepting both is a compatibility requirement rather than a
// courtesy: saved plans already exist on disk, and PLAN.md section 37's
// saved-plan path is refused-not-degraded everywhere else, so a silent
// misinterpretation here would be the worst outcome available.
//
//   - A bare JSON document: what every infrena before this release wrote.
//   - A report stream whose `plan` line carries the artifact: what this one
//     writes, so a frontend tails one format for every command (spec 2.4).
//
// The sniff is on the first line's `type` field, and planner.DecodePlan
// carries an independent guard refusing any document that has one. Both are
// needed: without the second, a caller that reaches DecodePlan without coming
// through here decodes a meta line into an empty plan and applies nothing.
func readSavedPlan(path string) (*planner.Plan, error)
```

```go
func readSavedPlan(path string) (*planner.Plan, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("%s is empty: it holds no plan to apply", path)
	}

	// The sniff. A pretty-printed bare artifact's first line is "{", which
	// does not unmarshal, and that is the signal rather than an error: only a
	// stream's first line is a complete JSON object carrying a type.
	first, _, _ := bytes.Cut(bytes.TrimSpace(data), []byte("\n"))
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(first, &probe); err != nil || probe.Type == "" {
		// The old shape, written by every infrena before this release.
		// DecodePlan carries its own guard against a typed document, so a
		// half-written stream cannot slip through here either.
		return planner.DecodePlan(data)
	}

	sc := bufio.NewScanner(bytes.NewReader(data))
	// The 64 KiB default is not enough for a plan line on a real environment,
	// and its failure mode is a TRUNCATED READ reported as a clean end of
	// input — which would surface as "no plan line" on a file that has one.
	sc.Buffer(make([]byte, 0, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var pl report.PlanLine
		if err := json.Unmarshal(line, &pl); err != nil || pl.Type != "plan" {
			continue
		}
		return planner.DecodePlan(pl.Plan)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return nil, fmt.Errorf(
		"%s is a report of a %q run and carries no plan line\n"+
			"Point --plan at a file written by `infrena plan --output`",
		path, commandOf(data))
}
```

`commandOf` reads the stream's `meta` line and returns its `command` field, or `"unknown"`. Naming the command the file actually came from is what turns this from "wrong file" into "you passed the apply report instead of the plan".

In `plan.go`, replace the `os.WriteFile` block: when `opts.Output` is set, `ro.Report().WritePlan(artifact)` where `artifact` is `json.Marshal(p)`. **`openRun` must open this file 0600** — it already does, via `openReport` — and the plan line's cleartext values are why that matters.

In `apply_saved.go`, replace `os.ReadFile` + `planner.DecodePlan` with `readSavedPlan(planPath)`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ ./internal/planner/ 2>&1 | tail -20`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./...
git add internal/cli/
git commit -m "Plan writes its artifact inside the report stream

So a frontend tails one format for every command instead of two. Apply
reads either shape, because plans saved before this release are still on
disk and refusing them would break applying a plan that was already
reviewed."
```

---

## Task 7: Exit 77 when approval cannot be obtained

**Files:**
- Modify: `internal/cli/root.go`, `internal/cli/apply.go`
- Test: `internal/cli/apply_test.go`

**Interfaces:**
- Consumes: Task 4's `runOutput`.
- Produces:
  - `const ExitNoApproval = 77` in `root.go`
  - `var errNoApproval = errors.New(…)`, mapped in `Execute()`
  - `func approvalUnobtainable(cmd *cobra.Command, opts *GlobalOptions) bool`

- [ ] **Step 1: Write the failing test**

```go
func TestApplyRefusesWhenApprovalCannotBeObtained(t *testing.T) {
	cases := []struct {
		name string
		args []string
		stdin string
	}{
		// --output means nobody is reading stdout, so nobody can see a prompt.
		{"output mode", []string{"apply", "dev", "--output", "OUT"}, ""},
		// stdin at EOF means nobody is there to type.
		{"stdin at eof", []string{"apply", "dev"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := newProjectFixture(t)
			args := withOutputPath(t, tc.args)

			_, stderr, code := runCommandWithStdin(t, dir, tc.stdin, args...)

			if code != ExitNoApproval {
				t.Errorf("exit = %d, want %d", code, ExitNoApproval)
			}
			if !strings.Contains(stderr, "--auto-approve") {
				t.Errorf("stderr does not name the fix:\n%s", stderr)
			}
			// The whole point of refusing before the lock: nothing was touched.
			if stateExists(t, dir, "dev") {
				t.Error("apply wrote state despite refusing for want of approval")
			}
		})
	}
}

// --auto-approve and --plan are both escapes, and a clean plan never needed
// approval at all, so none of the three may hit the new code path.
func TestApprovalRuleDoesNotFireWhenItShouldNot(t *testing.T) {
	dir := newProjectFixture(t)
	out := filepath.Join(t.TempDir(), "run.ndjson")

	if _, _, code := runCommand(t, dir, "apply", "dev", "--output", out, "--auto-approve"); code == ExitNoApproval {
		t.Error("--auto-approve still hit the approval refusal")
	}
	// Applied once, so the second run has no changes and must exit 0.
	if _, _, code := runCommand(t, dir, "apply", "dev", "--output", out); code != ExitOK {
		t.Errorf("clean apply exit = %d, want %d", code, ExitOK)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestApplyRefusesWhenApproval|TestApprovalRuleDoesNot' ./internal/cli/ -v`
Expected: FAIL — `ExitNoApproval` undefined; today the run reaches `confirm()`, gets EOF and exits 1.

- [ ] **Step 3: Write minimal implementation**

In `root.go`, beside the existing codes:

```go
	// ExitNoApproval: the plan has changes, approval is required, and this run
	// cannot obtain it — --output is set so nobody is reading stdout, or stdin
	// is already at EOF so nobody is there to type.
	//
	// 77 is sysexits.h's EX_NOPERM. It is a fourth row on PLAN.md section 16's
	// table and therefore a documented product API change.
	//
	// It replaces a genuinely misleading outcome: a piped apply reached
	// confirm(), got EOF from bufio.Scanner, and exited 1 saying "you must
	// type yes to approve" — advice nobody in that pipeline could have taken,
	// which is exactly what section 44 says a suggested action must never be.
	ExitNoApproval = 77
```

Map `errNoApproval` in `Execute()` beside `errChanges`.

In `apply.go`, after `p.HasChanges()` is known and **before** `withLockedEnvironment`:

```go
			if !opts.AutoApprove && approvalUnobtainable(cmd, opts) {
				// ro.Report(), not rw: Task 5 replaced openReport with openRun
				// throughout this RunE, and the report writer now comes off the
				// runOutput. It is nil without --output, which finishApply
				// already handles.
				return finishApply(cmd.ErrOrStderr(), ro.Report(), report.ApplyResult{
					Error: "changes require approval that this run cannot obtain",
				}, errNoApproval)
			}
```

`approvalUnobtainable` reports true when `opts.Output != ""`, or when a one-byte peek on `cmd.InOrStdin()` through a `*bufio.Reader` returns `io.EOF`. **Keep that reader** and hand it to `confirm` — peeking with a throwaway reader consumes the byte a real user typed.

The `--plan` path returns before this point, so it needs no exemption. Apply the identical check in `destroy.go`: a higher confirmation bar does not make it obtainable.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ 2>&1 | tail -20`
Expected: PASS. An existing test asserting exit 1 for a piped apply is **correct to update**.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./...
git add internal/cli/
git commit -m "Exit 77 when changes need approval no one can give

A piped apply used to reach the prompt, read end of input, and exit 1
telling the user to type yes, which nobody in that pipeline could do. It
now refuses before taking the lock and names an escape that works."
```

---

## Task 8: Documentation

Docs are part of the task, not a follow-up.

**Files:**
- Modify: `PLAN.md` (§16 exit codes, §37 `--output`), `CLAUDE.md`

**Interfaces:**
- Consumes: every task above.
- Produces: nothing executable.

- [ ] **Step 1: Update `PLAN.md` §16**

Add the fourth row to the exit-code table, with the condition spelled out: changes present, no `--auto-approve`, no `--plan`, and approval unobtainable because `--output` is set or stdin is at EOF.

- [ ] **Step 2: Update `PLAN.md` §37**

Record that `--output` silences stdout for every command, that `plan --output` now writes the report stream with the artifact on a `plan` line, and that `apply --plan` reads both envelopes.

- [ ] **Step 3: Update `CLAUDE.md`**

In the `--output` paragraph, which currently says the two meanings are "deliberately" different: they are now one format with a `plan` line, `report.Version` is 2, and `planner.PlanVersion` deliberately did not move because the plan's own schema is unchanged.

- [ ] **Step 4: Verify the whole suite**

Run: `go test ./... && go vet ./... && gofmt -l .`
Expected: all pass, `gofmt -l .` prints nothing.

- [ ] **Step 5: Commit**

```bash
git add PLAN.md CLAUDE.md
git commit -m "Document the output rule, exit 77 and the plan envelope"
```
