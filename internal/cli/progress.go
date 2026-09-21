package cli

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/infrena/infrena/internal/discovery"
	"github.com/infrena/infrena/internal/executor"
	"github.com/infrena/infrena/internal/planner"
	"github.com/infrena/infrena/internal/refresh"
)

// progressRenderer is the one writer to stdout while work is in flight. Both
// executor.Options.OnEvent and refresh.Refresh's onObservation may be called
// from several workers at once, so every write here is serialized; without
// that, two goroutines interleave bytes in a line neither of them wrote.
//
// A nil writer renders nothing, which is how --output silences stdout: decided
// once at construction rather than at every print site.
//
// Lines are append-only unless stdout is a terminal. stdout is as often a CI
// log or a redirected file, where an escape sequence is noise a reader has to
// strip mentally, so terminalWidth fails closed and every non-terminal writer
// gets plain appended lines. On a terminal, transient updates instead fold into
// a single status line that is erased before anything permanent is printed.
//
// What decides where a line goes is whether it is worth keeping, not which
// command emitted it. Transient: a resource read, an adoption, an operation
// starting, a heartbeat. Kept: an operation that actually ran, any failure, any
// retry, a discovery summary — unless --output was passed, an apply's terminal
// output is the only record of what it did to real infrastructure.
//
// The non-terminal path is the default rather than a branch: width is nil
// unless proven otherwise, so a forgotten check degrades to plain output.
type progressRenderer struct {
	mu      sync.Mutex
	w       io.Writer
	now     func() time.Time
	started map[string]time.Time
	beats   map[string]int
	done    chan struct{}
	stopped bool
	// width reports the terminal's column count. Nil means append-only.
	width func() int
	// count, verb and last are what the status line says: how many units of
	// work have finished, what kind of work it is, and the most recent one
	// by name. They are never read when width is nil.
	count int
	total int
	verb  string
	last  string
	// painted says a status line is on screen right now and must be cleared
	// before anything else is written.
	//
	// It is deliberately not derived from last, which answers a different
	// question: the end of a sweep clears the name, so an erase guarded on the
	// name would skip and the summary would print on top of the status line it
	// was meant to replace.
	painted bool
	// begun is when the renderer was built. The status line reports elapsed
	// time from it.
	begun time.Time
}

// heartbeat is how long an operation may run without output before the
// renderer says it is still working, and again between each such line.
//
// Real provider calls run for minutes, and without a heartbeat they are
// indistinguishable from a hang — which invites a Ctrl-C this engine makes
// expensive, since a second SIGINT leaves the lock behind.
const heartbeat = 15 * time.Second

func newProgressRenderer(w io.Writer, now func() time.Time) *progressRenderer {
	return newProgressRendererWith(w, now, terminalWidth(w))
}

// terminalWidth reports a writer's column count, or nil when it is not a
// terminal, which puts the renderer into append-only mode.
//
// It fails closed at every step: a writer with no Fd, a descriptor that is not
// a terminal, and a terminal whose size cannot be read all return nil. A false
// negative costs output that scrolls; a false positive costs escape sequences
// in a log file.
//
// The width is re-read on every paint rather than cached, because a user
// resizing their terminal mid-apply is ordinary and a stale width wraps.
func terminalWidth(w io.Writer) func() int {
	f, ok := w.(interface{ Fd() uintptr })
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return nil
	}
	return func() int {
		cols, _, err := term.GetSize(int(f.Fd()))
		if err != nil {
			return 0
		}
		return cols
	}
}

func newProgressRendererWith(w io.Writer, now func() time.Time, width func() int) *progressRenderer {
	r := &progressRenderer{
		width:   width,
		begun:   now(),
		w:       w,
		now:     now,
		started: map[string]time.Time{},
		beats:   map[string]int{},
		done:    make(chan struct{}),
	}
	if w != nil {
		go r.tick()
	}
	return r
}

// line is the one write path. Everything else formats and calls this, so the
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
	r.unlockedErase()
	fmt.Fprintf(r.w, format+"\n", args...)
	r.unlockedRedraw()
}

// unlockedErase clears the status line so something permanent can be written
// where it was. A no-op when there is no status line, which is what keeps the
// non-terminal path free of escape sequences.
func (r *progressRenderer) unlockedErase() {
	// Nothing painted means nothing to clear, and an escape sequence would be
	// the only thing a quiet command ever wrote.
	if r.w == nil || r.width == nil || !r.painted {
		return
	}
	fmt.Fprint(r.w, "\r\x1b[K")
	r.painted = false
}

// Event renders one executor progress notification.
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
		r.unlockedStart(verb, key, "%s %s...", verb, key)
	case executor.EventRetrying:
		// The started entry stays: another attempt is coming, and dropping it
		// here would silence the heartbeat for the rest of the operation.
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
		// A finished operation is a unit of work done, whether or not the
		// line naming it is kept.
		r.count++
		if !ok {
			// No paired start: report the outcome without inventing a
			// duration rather than printing one measured from the zero time.
			r.unlockedLine("%s %s... %s", verb, key, outcome)
			return
		}
		r.unlockedLine("%s %s... %s (%s)", verb, key, outcome, humanDuration(e.At.Sub(started)))
	}
}

// Discovery renders one provider instance's sweep.
//
// The instance is registered for the heartbeat: one Discover call covers every
// type an instance serves, the plugin does the whole sweep internally, and on a
// real account that takes minutes with nothing to report in between except that
// it is still running.
//
// A failed sweep is still a finish. Leaving a failed instance registered would
// have it announce itself as still working for the rest of the command.
func (r *progressRenderer) Discovery(p discovery.Progress) {
	if r.w == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	if !p.Done {
		r.started[p.Instance] = r.now()
		r.beats[p.Instance] = 0
		r.unlockedStart("Asking", p.Instance, "Asking %s what exists (%d types)...", p.Instance, p.Types)
		return
	}

	started, ok := r.started[p.Instance]
	delete(r.started, p.Instance)
	delete(r.beats, p.Instance)
	// The sweep has ended, so clear what the status line names. Otherwise the
	// finished sweep is repainted under everything import prints next, and the
	// line stays blank until the adoption half gives it something true to say.
	r.last = ""

	took := ""
	if ok {
		took = " (" + humanDuration(r.now().Sub(started)) + ")"
	}
	if p.Err != nil {
		r.unlockedLine("%s: failed%s", p.Instance, took)
		return
	}
	r.unlockedLine("%s: %d resources found%s", p.Instance, p.Found, took)
}

// Import renders one resource being adopted, in the same words a refresh
// observation uses: from the reader's side it is the same act, the provider
// asked about one resource and its answer recorded. Adoption is sequential, one
// provider call per resource, so without this a large account is silent for
// minutes.
func (r *progressRenderer) Import(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.unlockedTransient("Importing", name, "Reading %s... done", name)
}

// Observation renders one refresh observation.
func (r *progressRenderer) Observation(o refresh.Observation) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.unlockedTransient("Reading", o.Address.String(), "Reading %s... done", o.Address.String())
}

// SetTotal records how much work there is, so the status line can count
// towards it. Callers that cannot know — discover, whose one call covers every
// type an instance serves — never call it, and the line then shows a bare count
// rather than a denominator the tool cannot stand behind.
func (r *progressRenderer) SetTotal(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.total = n
}

// unlockedTransient records one unit of work that is finished but not worth
// keeping: a resource read, an adoption, a start. On a terminal it folds into
// the status line, which is erased before anything permanent is printed. Off a
// terminal it appends a line, because a CI log has no status line and would
// otherwise be silent for the whole sweep.
func (r *progressRenderer) unlockedTransient(verb, name, format string, args ...any) {
	r.count++
	r.unlockedStart(verb, name, format, args...)
}

// unlockedStart names what is happening now without counting it as finished.
// apply learns of a resource twice — started, then succeeded — and only the
// second is a unit of work done; refresh learns once and uses the pair above.
func (r *progressRenderer) unlockedStart(verb, name, format string, args ...any) {
	if r.w == nil || r.stopped {
		return
	}
	r.verb, r.last = verb, name
	if r.width == nil {
		r.unlockedLine(format, args...)
		return
	}
	r.unlockedRedraw()
}

// unlockedBeat is a transient line carrying no new work: the heartbeat, whose
// entire content is "still running". On a terminal the status line's own clock
// already says that, so this only refreshes it; off one it prints as it always
// did, because a CI log has no clock to watch.
func (r *progressRenderer) unlockedBeat(format string, args ...any) {
	if r.w == nil || r.stopped {
		return
	}
	if r.width == nil {
		r.unlockedLine(format, args...)
		return
	}
	r.unlockedRedraw()
}

// unlockedRedraw paints the status line: carriage return to column one, the
// text, then erase-to-end-of-line so a shorter line does not leave the tail of
// a longer one behind it.
func (r *progressRenderer) unlockedRedraw() {
	// An empty last means nothing has been reported yet, and a status line
	// naming nothing is worse than none.
	if r.w == nil || r.stopped || r.width == nil || r.last == "" {
		return
	}
	fmt.Fprintf(r.w, "\r%s\x1b[K", truncate(r.statusText(), r.width()))
	r.painted = true
}

// statusText is the one line the reader watches while they wait.
func (r *progressRenderer) statusText() string {
	elapsed := humanDuration(r.now().Sub(r.begun))
	// No counter until there is something to count: import spends its first
	// minutes inside one Discover call, and "Asking 0  aws (9m38s)" reads as a
	// command that has stalled rather than one with nothing to report yet.
	if r.count == 0 && r.total == 0 {
		return fmt.Sprintf("  %s %s (%s)", r.verb, r.last, elapsed)
	}
	done := strconv.Itoa(r.count)
	if r.total > 0 {
		done += "/" + strconv.Itoa(r.total)
	}
	return fmt.Sprintf("  %s %s  %s (%s)", r.verb, done, r.last, elapsed)
}

// truncate cuts a status line to the terminal's width, counting runes rather
// than bytes: a line one column too wide wraps, and a wrapped line cannot be
// erased — "\r\x1b[K" returns to the start of the last screen row and clears
// only that, stranding every row above it for the session.
//
// The tail is dropped rather than the head, because the head carries the
// counter. A width at or below zero means the terminal could not be measured,
// and the line goes out whole rather than not at all.
func truncate(s string, width int) string {
	if width <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= width {
		return s
	}
	return string(runes[:width])
}

// tick prints a heartbeat for any operation that has been running longer than
// the next multiple of `heartbeat` without a terminal event.
//
// It reads r.now() rather than the ticker's own timestamp, so an injected
// clock drives it.
func (r *progressRenderer) tick() {
	t := time.NewTicker(heartbeat / 3)
	defer t.Stop()
	for {
		select {
		case <-r.done:
			return
		case <-t.C:
			r.beat()
		}
	}
}

// beat is one heartbeat sweep, separated from the ticker so it can be driven
// on an injected clock rather than by sleeping.
func (r *progressRenderer) beat() {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	// Sorted, so two operations crossing the same beat print in a stable
	// order rather than Go's randomised map order.
	keys := make([]string, 0, len(r.started))
	for k := range r.started {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		elapsed := now.Sub(r.started[k])
		b := int(elapsed / heartbeat)
		if b > r.beats[k] {
			r.beats[k] = b
			r.unlockedBeat("  %s... still working (%s)", k, humanDuration(elapsed))
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
	// Erase before setting stopped, because unlockedErase is a no-op once it
	// is set and the status line would be left on screen for the plan to
	// print beside.
	r.unlockedErase()
	r.stopped = true
	close(r.done)
}

// statusSafeWriter wraps the writer a command prints its product through — the
// rendered plan, an apply's result — so that product never lands on top of the
// status line.
//
// The product does not go through the renderer, and Stop runs on a defer at the
// very end of the command, so between the last observation and that defer the
// status line is still on screen with the cursor at the end of it, which is
// exactly where the plan would otherwise begin. Wrapping one writer keeps that
// rule out of every print site.
//
// Off a terminal this is a pass-through to the byte, because erase and redraw
// are both no-ops when width is nil.
func statusSafeWriter(w io.Writer, p *progressRenderer) io.Writer {
	return &statusSafe{w: w, p: p}
}

type statusSafe struct {
	w io.Writer
	p *progressRenderer
}

// Write clears the status line, writes, and puts the status line back.
//
// The redraw afterwards matters mid-run: apply prints while it is still
// working, and a writer that only erased would retire the status line the first
// time that happened. After the last write, Stop undoes the redraw.
//
// The renderer's mutex is held across the write so a worker goroutine's
// heartbeat cannot repaint the status line into the middle of the product.
func (s *statusSafe) Write(b []byte) (int, error) {
	s.p.mu.Lock()
	defer s.p.mu.Unlock()
	s.p.unlockedErase()
	n, err := s.w.Write(b)
	s.p.unlockedRedraw()
	return n, err
}

// verbFor names the work an operation kind is doing, in the present
// continuous, because the line is printed while it is still happening.
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

// humanDuration renders a duration as 15.7s under a minute, 2m52s at or above
// one.
func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%dm%ds", int(d/time.Minute), int((d%time.Minute)/time.Second))
}
