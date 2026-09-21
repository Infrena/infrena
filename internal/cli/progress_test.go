package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infrena/infrena/internal/discovery"
	"github.com/infrena/infrena/internal/executor"
	"github.com/infrena/infrena/internal/planner"
	"github.com/infrena/infrena/internal/refresh"
	"github.com/infrena/infrena/internal/state"
	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/report"
	"github.com/infrena/infrena/pkg/resource"
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

func refreshObservationFixture() refresh.Observation {
	return refresh.Observation{Address: addr("a")}
}

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

// Append-only OFF A TERMINAL is the property, not a style choice: stdout here
// is as often a CI log or a redirected file as a terminal, and an escape
// sequence in either is noise a reader has to strip. A bytes.Buffer has no
// file descriptor, so this is also the guard that the status line cannot
// leak into a redirected run.
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

// A SWEEP THAT SAYS NOTHING IS INDISTINGUISHABLE FROM A HANG, which is the
// whole reason the heartbeat exists. discover and import were the two
// commands that never fed it: a real AWS account takes minutes to sweep and
// printed not one line while it did.
func TestDiscoveryProgressRegistersForTheHeartbeat(t *testing.T) {
	var buf bytes.Buffer
	c := &clock{t: time.Unix(0, 0)}
	r := newProgressRenderer(&buf, c.now)
	defer r.Stop()

	r.Discovery(discovery.Progress{Instance: "aws", Types: 958})

	if out := buf.String(); !strings.Contains(out, "aws") || !strings.Contains(out, "958") {
		t.Errorf("the start does not say what is being asked or how much of it:\n%s", out)
	}
	// Registered, so the heartbeat can report it. Asserted through the
	// renderer's own state rather than by waiting on a ticker.
	r.mu.Lock()
	_, tracked := r.started["aws"]
	r.mu.Unlock()
	if !tracked {
		t.Error("the sweep is not registered for the heartbeat, so a long wait stays silent")
	}
}

func TestDiscoveryProgressReportsTheCountAndStopsTheHeartbeat(t *testing.T) {
	var buf bytes.Buffer
	c := &clock{t: time.Unix(0, 0)}
	r := newProgressRenderer(&buf, c.now)
	defer r.Stop()

	r.Discovery(discovery.Progress{Instance: "aws", Types: 958})
	r.Discovery(discovery.Progress{Instance: "aws", Types: 958, Found: 82, Done: true})

	if out := buf.String(); !strings.Contains(out, "82") {
		t.Errorf("the finish does not say how much was found:\n%s", out)
	}
	r.mu.Lock()
	_, tracked := r.started["aws"]
	r.mu.Unlock()
	if tracked {
		t.Error("a finished sweep is still registered, so it would heartbeat forever")
	}
}

// A failed sweep must also stop heartbeating, and say so.
func TestAFailedSweepIsReportedAndDeregistered(t *testing.T) {
	var buf bytes.Buffer
	c := &clock{t: time.Unix(0, 0)}
	r := newProgressRenderer(&buf, c.now)
	defer r.Stop()

	r.Discovery(discovery.Progress{Instance: "aws", Types: 958})
	r.Discovery(discovery.Progress{Instance: "aws", Done: true, Err: errors.New("credentials expired")})

	if out := buf.String(); !strings.Contains(out, "failed") {
		t.Errorf("a failed sweep does not say so:\n%s", out)
	}
	r.mu.Lock()
	_, tracked := r.started["aws"]
	r.mu.Unlock()
	if tracked {
		t.Error("a failed sweep is still registered for the heartbeat")
	}
}

// width80 is a terminal 80 columns wide. A non-nil width is what puts the
// renderer in status-line mode, the same nil-means-off convention the writer
// already uses.
func width80() int { return 80 }

// TestStatusLineRewritesInPlaceRatherThanAppending is the whole point of the
// mode: on a terminal, a sweep of 82 resources must not scroll 82 lines past
// the reader while they wait for the plan they actually asked for.
func TestStatusLineRewritesInPlaceRatherThanAppending(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	var buf bytes.Buffer
	r := newProgressRendererWith(&buf, c.now, width80)
	defer r.Stop()

	r.Observation(refresh.Observation{Address: addr("vpc-app1")})
	r.Observation(refresh.Observation{Address: addr("subnet-a")})

	got := buf.String()
	if strings.Contains(got, "\n") {
		t.Errorf("a status update must not end a line, or it scrolls:\n%q", got)
	}
	if !strings.Contains(got, "\r") || !strings.Contains(got, "\x1b[K") {
		t.Errorf("want a carriage return and an erase-to-end-of-line:\n%q", got)
	}
	if !strings.Contains(got, "subnet-a") {
		t.Errorf("the status line must name what is being worked on:\n%q", got)
	}
}

// TestAKeptLineErasesTheStatusFirstAndRedrawsAfter. Without the erase, a
// permanent line is printed starting at column one ON TOP of the status text
// and keeps whatever of it was longer, which is how in-place rendering
// produces the garbled half-lines it is famous for.
func TestAKeptLineErasesTheStatusFirstAndRedrawsAfter(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	var buf bytes.Buffer
	r := newProgressRendererWith(&buf, c.now, width80)
	defer r.Stop()

	r.Observation(refresh.Observation{Address: addr("vpc-app1")})
	r.Event(executor.Event{Kind: executor.EventFailed, Address: addr("subnet-a"), Op: planner.OpCreate, At: c.now()})

	got := buf.String()
	if !strings.Contains(got, "\r\x1b[KCreating subnet-a... failed") {
		t.Errorf("a kept line must be preceded by an erase:\n%q", got)
	}
	if !strings.Contains(got, "failed\n") {
		t.Errorf("a kept line must end its line, or the next one overwrites it:\n%q", got)
	}
	// The property, not the wording: whatever the status line says, it is
	// back on screen after the kept line rather than lost behind it.
	if !strings.HasSuffix(got, "\x1b[K") || strings.LastIndex(got, "vpc-app1") < strings.Index(got, "failed\n") {
		t.Errorf("the status line must be redrawn after a kept line:\n%q", got)
	}
}

// TestStopErasesTheStatusLine is the promise that makes the whole mode safe to
// turn on: whatever the command prints next — the plan, a summary, an error —
// starts on a clean line, and nothing a user copies out of their terminal has
// a progress counter in the middle of it.
func TestStopErasesTheStatusLine(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	var buf bytes.Buffer
	r := newProgressRendererWith(&buf, c.now, width80)

	r.Observation(refresh.Observation{Address: addr("vpc-app1")})
	r.Stop()

	if got := buf.String(); !strings.HasSuffix(got, "\r\x1b[K") {
		t.Errorf("Stop must leave the line empty:\n%q", got)
	}
}

// Stop is called on paths that never rendered anything, and on a writer that
// is not a terminal. Neither may emit an escape sequence into a CI log.
func TestStopEmitsNothingWhenThereWasNoStatusLine(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	var buf bytes.Buffer
	newProgressRendererWith(&buf, c.now, width80).Stop()
	if got := buf.String(); got != "" {
		t.Errorf("nothing was drawn, so nothing should be erased:\n%q", got)
	}

	var plain bytes.Buffer
	r := newProgressRenderer(&plain, c.now)
	r.Observation(refresh.Observation{Address: addr("a")})
	r.Stop()
	if strings.ContainsRune(plain.String(), '\x1b') {
		t.Errorf("no escape sequences off a terminal:\n%q", plain.String())
	}
}

// TestStatusLineIsTruncatedToTheTerminalWidth. A status line longer than the
// terminal WRAPS, and a wrapped line cannot be erased: "\r\x1b[K" returns to
// the start of the LAST screen row and clears that one, leaving every row
// above it on screen for the rest of the session. Addresses in this tool are
// routinely 60 characters and terminals are routinely narrower.
func TestStatusLineIsTruncatedToTheTerminalWidth(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	var buf bytes.Buffer
	narrow := func() int { return 24 }
	r := newProgressRendererWith(&buf, c.now, narrow)
	defer r.Stop()

	r.Observation(refresh.Observation{Address: addr("instance-sandbox-bl-etl-altruist-collector-ec2-spot")})

	got := buf.String()
	visible := strings.TrimSuffix(strings.TrimPrefix(got, "\r"), "\x1b[K")
	if len([]rune(visible)) > 24 {
		t.Errorf("status line is %d columns on a 24 column terminal, so it wraps:\n%q", len([]rune(visible)), visible)
	}
	if visible == "" {
		t.Errorf("truncation must not empty the line:\n%q", got)
	}
}

// TestTheStatusLineCountsTowardsATotalWhenOneIsKnown. refresh, apply and
// import all know how much work there is before they start, and "47/82" is
// the difference between a wait with an end in sight and one without.
func TestTheStatusLineCountsTowardsATotalWhenOneIsKnown(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	var buf bytes.Buffer
	r := newProgressRendererWith(&buf, c.now, width80)
	defer r.Stop()

	r.SetTotal(82)
	r.Observation(refresh.Observation{Address: addr("a")})
	r.Observation(refresh.Observation{Address: addr("b")})

	if got := buf.String(); !strings.Contains(got, "Reading 2/82") {
		t.Errorf("want a count against the total:\n%q", got)
	}
}

// discover cannot know a total: one Discover call covers every type an
// instance serves and the plugin sweeps internally. A denominator invented
// for it would be a number the tool cannot stand behind.
func TestTheStatusLineOmitsTheTotalWhenThereIsNone(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	var buf bytes.Buffer
	r := newProgressRendererWith(&buf, c.now, width80)
	defer r.Stop()

	r.Observation(refresh.Observation{Address: addr("a")})

	got := buf.String()
	if !strings.Contains(got, "Reading 1 ") {
		t.Errorf("want a bare count:\n%q", got)
	}
	if strings.Contains(got, "/") {
		t.Errorf("no denominator should be invented:\n%q", got)
	}
}

// TestTheStatusLineShowsElapsedTime. The heartbeat exists because a subnet
// delete took 2m52s and silence is indistinguishable from a hang. Folding the
// sweep into one line would throw that away, so the line carries the clock:
// a number that keeps moving is the proof that the tool has not wedged.
func TestTheStatusLineShowsElapsedTime(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	var buf bytes.Buffer
	r := newProgressRendererWith(&buf, c.now, width80)
	defer r.Stop()

	c.advance(12 * time.Second)
	r.Observation(refresh.Observation{Address: addr("a")})

	if got := buf.String(); !strings.Contains(got, "(12.0s)") {
		t.Errorf("want the elapsed time on the status line:\n%q", got)
	}
}

// TestApplyKeepsWhatHappenedAndCollapsesWhatIsHappening. The rule the whole
// mode turns on: transient work folds into the status line, consequential
// work stays in the scrollback. A create that ran changed the world and is
// the only record of it unless --output was passed; a create that is merely
// STARTING is a progress report.
func TestApplyKeepsWhatHappenedAndCollapsesWhatIsHappening(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	var buf bytes.Buffer
	r := newProgressRendererWith(&buf, c.now, width80)
	defer r.Stop()

	r.Event(executor.Event{Kind: executor.EventStarted, Address: addr("instance-worker"), Op: planner.OpCreate, At: c.now()})
	c.advance(4200 * time.Millisecond)
	r.Event(executor.Event{Kind: executor.EventSucceeded, Address: addr("instance-worker"), Op: planner.OpCreate, At: c.now()})

	got := buf.String()
	if !strings.Contains(got, "Creating instance-worker... done (4.2s)\n") {
		t.Errorf("a create that happened must stay in the scrollback:\n%q", got)
	}
	if strings.Contains(got, "Creating instance-worker...\n") {
		t.Errorf("the start line must not be kept; it is a progress report:\n%q", got)
	}
}

// A heartbeat is the purest transient line there is: it says only "still
// working", which the elapsed clock on the status line already says. Kept,
// it would scroll a line every 15 seconds and undo the entire feature.
func TestHeartbeatsAreNotKept(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	var buf bytes.Buffer
	r := newProgressRendererWith(&buf, c.now, width80)
	defer r.Stop()

	r.Event(executor.Event{Kind: executor.EventStarted, Address: addr("subnet-slow"), Op: planner.OpDestroy, At: c.now()})
	c.advance(90 * time.Second)
	r.beat()

	if got := buf.String(); strings.Contains(got, "still working") {
		t.Errorf("a heartbeat must not be kept on a terminal:\n%q", got)
	}
}

// TestTerminalWidthIsNilForAnythingThatIsNotATerminal. This is the guard on
// the whole feature: a redirected file, a pipe into `less`, a CI log and a
// test buffer must all keep the append-only output this renderer has always
// produced. Detection failing OPEN would put escape sequences in all four.
func TestTerminalWidthIsNilForAnythingThatIsNotATerminal(t *testing.T) {
	if terminalWidth(&bytes.Buffer{}) != nil {
		t.Error("a buffer has no file descriptor at all and must not be treated as a terminal")
	}
	f, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if terminalWidth(f) != nil {
		t.Error("a regular file has a descriptor but is not a terminal; redirected output must stay clean")
	}
}

// statusRun builds a runOutput whose renderer is in status-line mode, which
// openRun cannot produce in a test because a buffer is not a terminal.
func statusRun(buf *bytes.Buffer, now func() time.Time) *runOutput {
	return &runOutput{out: buf, progress: newProgressRendererWith(buf, now, width80)}
}

// TestRefreshCountsTowardsTheNumberOfResourcesInState. The total has to be set
// somewhere shared, or plan, apply, destroy and refresh each get their own
// chance to forget it — the reason observationHook exists at all.
func TestRefreshCountsTowardsTheNumberOfResourcesInState(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	var buf bytes.Buffer
	ro := statusRun(&buf, c.now)
	defer ro.progress.Stop()

	st := state.New("p", "dev")
	for _, n := range []string{"a", "b", "c"} {
		st.Resources[n] = &resource.ResourceState{Address: addr(n)}
	}

	hook := observationHook(ro, st)
	hook(refresh.Observation{Address: addr("a")})

	if got := buf.String(); !strings.Contains(got, "Reading 1/3") {
		t.Errorf("want the count against everything in state:\n%q", got)
	}
}

// TestApplyCountsTowardsTheOperationsThatWillRun. A no-op is not work: a plan
// of 82 resources where 4 change must count to 4, or the bar sits at 5%%
// through an apply that is actually finished.
func TestApplyCountsTowardsTheOperationsThatWillRun(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	var buf bytes.Buffer
	ro := statusRun(&buf, c.now)
	defer ro.progress.Stop()

	p := &planner.Plan{Operations: []planner.Operation{
		{Address: addr("a"), Kind: planner.OpCreate},
		{Address: addr("b"), Kind: planner.OpNoOp},
		{Address: addr("c"), Kind: planner.OpDestroy},
	}}

	reportPlan(ro, p, report.StageExecuting)
	ro.progress.Event(executor.Event{Kind: executor.EventStarted, Address: addr("a"), Op: planner.OpCreate, At: c.now()})
	ro.progress.Event(executor.Event{Kind: executor.EventSucceeded, Address: addr("a"), Op: planner.OpCreate, At: c.now()})

	if got := buf.String(); !strings.Contains(got, "1/2") {
		t.Errorf("want the count against the operations that will run, not the plan's size:\n%q", got)
	}
}

// TestTheProductIsPrintedOnACleanLine. The plan does not go through the
// renderer — it is written straight to the run's output writer — and Stop
// only runs on a defer at the very end of the command. Without this, the
// first line of the plan is printed at the cursor, which is sitting at the
// end of the status line, and the user's plan begins mid-sentence.
func TestTheProductIsPrintedOnACleanLine(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	var buf bytes.Buffer
	r := newProgressRendererWith(&buf, c.now, width80)
	defer r.Stop()
	out := statusSafeWriter(&buf, r)

	r.Observation(refresh.Observation{Address: addr("vpc-app1")})
	fmt.Fprint(out, "Plan for project \"my-project\":\n")

	got := buf.String()
	if !strings.Contains(got, "\r\x1b[KPlan for project") {
		t.Errorf("the product must be preceded by an erase:\n%q", got)
	}
	if strings.Contains(got, "vpc-app1 (0.0s)Plan") {
		t.Errorf("the product was printed on top of the status line:\n%q", got)
	}
}

// A run with no terminal must write the product through untouched: same
// bytes, no escape sequences, whatever wrapping is in the way.
func TestTheProductIsUntouchedOffATerminal(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	var buf bytes.Buffer
	r := newProgressRenderer(&buf, c.now)
	defer r.Stop()

	fmt.Fprint(statusSafeWriter(&buf, r), "Plan for project \"my-project\":\n")

	if got := buf.String(); got != "Plan for project \"my-project\":\n" {
		t.Errorf("off a terminal the product must pass through unchanged:\n%q", got)
	}
}

// TestAStatusLineWithNothingCountedYetShowsNoNumber. `import` spends its first
// nine minutes in one Discover call, during which nothing is countable, and a
// counter frozen at 0 for that long reads as a stalled command rather than an
// honest "no units yet". Seen in the field as "Asking 0  aws (9m38s)".
func TestAStatusLineWithNothingCountedYetShowsNoNumber(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	var buf bytes.Buffer
	r := newProgressRendererWith(&buf, c.now, width80)
	defer r.Stop()

	c.advance(578 * time.Second)
	r.Discovery(discovery.Progress{Instance: "aws", Types: 1584})

	got := buf.String()
	if strings.Contains(got, " 0 ") || strings.Contains(got, " 0/") {
		t.Errorf("a count of nothing must not be shown:\n%q", got)
	}
	if !strings.Contains(got, "aws") || !strings.Contains(got, "(9m38s)") {
		t.Errorf("the line must still name the work and its age:\n%q", got)
	}
}

// TestStderrAlsoClearsTheStatusLine. The status line is drawn on stdout, but
// stderr lands on the SAME terminal, and import deliberately reports skipped
// resources and provider warnings there. Guarding only stdout is why a real
// run printed "Asking 0  aws (9m38s)Skipped aws.ec2.instance ...".
//
// The erase goes to the renderer's own writer — the stream the status line is
// actually on — so a redirected stderr never receives an escape sequence.
func TestStderrAlsoClearsTheStatusLine(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	var out, errBuf bytes.Buffer
	r := newProgressRendererWith(&out, c.now, width80)
	defer r.Stop()

	r.Observation(refresh.Observation{Address: addr("vpc-app1")})
	fmt.Fprint(statusSafeWriter(&errBuf, r), "Skipped aws.ec2.instance i-1: tagged\n")

	// A bare erase on stdout, after the status line was painted there. The
	// status is redrawn afterwards because the command is still working; what
	// matters is that the clear happened on the right stream at all.
	if !strings.Contains(out.String(), "\x1b[K\r\x1b[K") {
		t.Errorf("stdout must have had its status line cleared:\n%q", out.String())
	}
	if strings.ContainsRune(errBuf.String(), '\x1b') {
		t.Errorf("the erase must go to the stream the status line is on, never to stderr:\n%q", errBuf.String())
	}
	if got := errBuf.String(); got != "Skipped aws.ec2.instance i-1: tagged\n" {
		t.Errorf("the diagnostic itself must be untouched:\n%q", got)
	}
}

// Adoption is not a read. `import` showed "Reading" for the half of its work
// that actually adopts, which is the half a user is waiting on.
func TestAdoptionSaysWhatItIsDoing(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	var buf bytes.Buffer
	r := newProgressRendererWith(&buf, c.now, width80)
	defer r.Stop()

	r.SetTotal(78)
	r.Import("vpc-blueleaf-staging-vpc")

	if got := buf.String(); !strings.Contains(got, "Importing 1/78  vpc-blueleaf-staging-vpc") {
		t.Errorf("want the adoption named and counted:\n%q", got)
	}
}

// TestAFinishedSweepLeavesNoStaleStatusLine. Discovery and adoption are two
// phases, and between them import writes a file per generated config. With
// the sweep's line still registered, each of those repainted "Asking aws
// (9m38s)" underneath — a status line describing work that finished minutes
// ago, redrawn once per line of real output.
func TestAFinishedSweepLeavesNoStaleStatusLine(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	var buf bytes.Buffer
	r := newProgressRendererWith(&buf, c.now, width80)
	defer r.Stop()

	r.Discovery(discovery.Progress{Instance: "aws", Types: 1584})
	c.advance(578 * time.Second)
	r.Discovery(discovery.Progress{Instance: "aws", Found: 1682, Done: true})
	buf.Reset()

	fmt.Fprint(statusSafeWriter(&buf, r), "Wrote discovered/vpcs.yml\n")

	if got := buf.String(); strings.Contains(got, "Asking") {
		t.Errorf("a finished sweep must not be repainted under later output:\n%q", got)
	}
}

// TestASweepSummaryStillClearsTheLineItReplaces. Clearing what the status line
// NAMES and knowing whether one is PAINTED are two different facts, and
// conflating them put the original bug straight back: the end of a sweep
// cleared the name, the erase guard saw no name and skipped, and the summary
// printed onto the status line it was meant to replace.
func TestASweepSummaryStillClearsTheLineItReplaces(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	var buf bytes.Buffer
	r := newProgressRendererWith(&buf, c.now, width80)
	defer r.Stop()

	r.Discovery(discovery.Progress{Instance: "aws", Types: 1584})
	c.advance(4200 * time.Millisecond)
	r.Discovery(discovery.Progress{Instance: "aws", Found: 3, Done: true})

	if got := buf.String(); !strings.Contains(got, "\r\x1b[Kaws: 3 resources found") {
		t.Errorf("the summary must clear the status line it replaces:\n%q", got)
	}
}
