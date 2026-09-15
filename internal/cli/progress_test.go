package cli

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infrena/infrena/internal/executor"
	"github.com/infrena/infrena/internal/planner"
	"github.com/infrena/infrena/internal/refresh"
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
