package cli

import (
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/infrena/infrena/internal/executor"
	"github.com/infrena/infrena/internal/planner"
	"github.com/infrena/infrena/internal/refresh"
)

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
	beats   map[string]int
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
	r := &progressRenderer{
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

// Observation renders one refresh observation.
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

// humanDuration reads the way the live AWS suite reports times: 15.7s under a
// minute, 2m52s at or above one.
func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%dm%ds", int(d/time.Minute), int((d%time.Minute)/time.Second))
}
