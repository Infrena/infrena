package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/infrena/infrena/internal/executor"
	"github.com/infrena/infrena/internal/planner"
	"github.com/infrena/infrena/internal/registry"
	"github.com/infrena/infrena/internal/state"
	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/value"
	testprovider "github.com/infrena/infrena/providers/test"
)

// TestRunInterruptibleFirstSignal delivers a real SIGINT to this test's own
// process (a standard, working pattern on Linux: signal.Notify installs the
// handler before the signal is sent, so the runtime delivers it in-process
// rather than terminating). It would catch two different real bugs: if
// runInterruptible never calls cancel() on the first signal, fn blocks on
// ctx.Done() forever and this test times out; if runInterruptible returns
// as soon as it receives the signal instead of waiting for fn to actually
// finish its post-cancellation cleanup, cleanedUp is not yet closed when
// the assertion below runs.
func TestRunInterruptibleFirstSignal(t *testing.T) {
	started := make(chan struct{})
	cleanedUp := make(chan struct{})
	resultErr := make(chan error, 1)

	go func() {
		resultErr <- runInterruptible("dev", func(ctx context.Context) error {
			close(started)
			<-ctx.Done()                      // unblocks only once the signal cancels ctx
			time.Sleep(20 * time.Millisecond) // stand-in for persist+unlock
			close(cleanedUp)
			return nil
		})
	}()

	<-started
	self, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatalf("finding own process: %v", err)
	}
	if err := self.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("signaling self: %v", err)
	}

	select {
	case err := <-resultErr:
		select {
		case <-cleanedUp:
		default:
			t.Fatal("runInterruptible returned before fn's post-cancellation cleanup finished")
		}
		if !errors.Is(err, errInterrupted) {
			t.Fatalf("err = %v, want errInterrupted", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runInterruptible did not return after a single SIGINT")
	}
}

// TestRunInterruptibleProtectsAnInFlightApplyFromItsOwnSignal is the seam
// test: "the signal is received", "the context is cancelled" and "the
// dispatched provider call survives cancellation" are three separately
// tested halves (TestRunInterruptibleFirstSignal proves the first two;
// executor.TestApplySurvivesCancellationDuringDispatch proves the third
// against a programmatically cancelled context) whose product is not
// automatically true just because each half is. A real bug this test alone
// would catch: a future refactor that has runInterruptible cancel a context
// which never reaches operationContext at all — e.g. because whatever wires
// runInterruptible around executor.Apply (task 12) passes it a *different*
// context than the one it cancels — would still pass both of the narrower
// tests above (each exercises its own half with a context it constructs
// itself) while breaking the real, end-to-end contract this task exists to
// provide: a SIGINT during `infra apply` must not abort an in-flight
// create. This composes the two real production pieces — runInterruptible
// (this package) driving a real executor.Apply — with a real OS signal
// exactly as TestRunInterruptibleFirstSignal does, rather than a
// programmatic cancel() as executor's own test does; the only synthetic
// part is the fake provider's LatencyMS, which is what holds the create in
// flight long enough for the signal to land inside it.
func TestRunInterruptibleProtectsAnInFlightApplyFromItsOwnSignal(t *testing.T) {
	cloudPath := t.TempDir() + "/fake-cloud.json"
	cloud := &testprovider.Cloud{
		Resources: map[string]*testprovider.CloudResource{},
		LatencyMS: 400,
	}
	if err := cloud.Save(cloudPath); err != nil {
		t.Fatalf("seeding fake cloud: %v", err)
	}

	reg := registry.New()
	if err := reg.Register("fake", testprovider.New(cloudPath)); err != nil {
		t.Fatalf("Register: %v", err)
	}

	p := &planner.Plan{
		Version: planner.PlanVersion,
		Operations: []planner.Operation{
			{Provider: "fake", Address: address.Address{Name: "network"}, Type: "fake.network", Kind: planner.OpCreate,
				After: map[string]value.Value{"cidr": value.String("10.0.0.0/24", value.SourceExplicit)}},
		},
	}
	g, err := planner.BuildExecution(p, func(address.Address) []address.Address { return nil })
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}

	backend := state.NewLocal(t.TempDir())
	st, err := backend.Get(context.Background(), "dev")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	started := make(chan struct{})
	var startOnce sync.Once

	var result executor.Result
	resultErr := make(chan error, 1)
	go func() {
		resultErr <- runInterruptible("dev", func(ctx context.Context) error {
			opts := executor.Options{
				Parallelism: 1, PerProvider: 1, Registry: reg, Backend: backend,
				Environment: "dev", Retry: executor.RetryPolicy{MaxAttempts: 1}, Now: time.Now,
				OnEvent: func(e executor.Event) {
					if e.Kind == executor.EventStarted {
						startOnce.Do(func() { close(started) })
					}
				},
			}
			res, ds := executor.Apply(ctx, p, g, st, opts)
			result = res
			if ds.HasErrors() {
				return fmt.Errorf("apply reported diagnostics: %v", ds)
			}
			if len(res.Failed) != 0 {
				return fmt.Errorf("apply reported failures: %v", res.Failed)
			}
			return nil
		})
	}()

	<-started // the create has dispatched and is now inside its 400ms delay
	self, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatalf("finding own process: %v", err)
	}
	if err := self.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("signaling self: %v", err)
	}

	select {
	case err := <-resultErr:
		if !errors.Is(err, errInterrupted) {
			t.Fatalf("err = %v, want errInterrupted", err)
		}
		if len(result.Applied) != 1 || result.Applied[0].String() != "network" {
			t.Fatalf("Applied = %v, Failed = %v — the create did not survive the signal; "+
				"either operationContext is not wired into dispatch, or runInterruptible is "+
				"not cancelling the same context Apply was given",
				result.Applied, result.Failed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runInterruptible did not return within 3s of a single SIGINT during a 400ms-latency create")
	}
}

func TestRunInterruptibleNoSignalReturnsFnsError(t *testing.T) {
	want := errors.New("boom")
	err := runInterruptible("dev", func(ctx context.Context) error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want to wrap %v", err, want)
	}
}

// TestRunInterruptibleErrorDoesNotClaimStateWasSavedWhenFnFailed pins review
// round 1's Minor #2 fix: runInterruptible has no way to know whether fn
// actually persisted state before returning — only fn knows that — so its
// own wrapping must never assert an outcome fn didn't report. Before the
// fix, errInterrupted's text unconditionally said "state was saved, and the
// lock was released" even when fn's own error said persistence failed,
// which is exactly backwards from spec §44 ("state what is wrong"). This
// simulates that failure directly: fn returns a persist-failure error after
// its ctx is cancelled, and the combined error must not contain the false
// claim.
func TestRunInterruptibleErrorDoesNotClaimStateWasSavedWhenFnFailed(t *testing.T) {
	started := make(chan struct{})
	persistErr := errors.New("failed to persist state after 1 operation: disk full")
	resultErr := make(chan error, 1)

	go func() {
		resultErr <- runInterruptible("dev", func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			return persistErr
		})
	}()

	<-started
	self, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatalf("finding own process: %v", err)
	}
	if err := self.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("signaling self: %v", err)
	}

	select {
	case err := <-resultErr:
		if !errors.Is(err, errInterrupted) {
			t.Fatalf("err = %v, want to wrap errInterrupted", err)
		}
		if !errors.Is(err, persistErr) {
			t.Fatalf("err = %v, want to wrap fn's own error too", err)
		}
		if strings.Contains(err.Error(), "state was saved") {
			t.Fatalf("err = %q — claims state was saved despite fn reporting a persist failure", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runInterruptible did not return after a single SIGINT")
	}
}

// TestRunInterruptibleSecondCallStillReceivesItsOwnSignalAfterAPriorNormalReturn
// does NOT verify that defer signal.Stop(sig) runs, or that it prevents a
// registration leak — a prior round's reviewer disproved that reading: Go's
// os/signal delivers to every registered channel independently, so a
// leaked registration from a cleanly-completed call cannot steal or delay a
// later call's delivery (confirmed by mutation: removing the defer left
// this test, and every other test in this file, green). What this test
// does establish is narrower and still worth pinning: one runInterruptible
// call finishing normally does not leave the process unable to correctly
// signal-handle a second, later call. See the comment on
// `defer signal.Stop(sig)` below for why the leak itself has no in-process
// test and is verified by code inspection instead.
func TestRunInterruptibleSecondCallStillReceivesItsOwnSignalAfterAPriorNormalReturn(t *testing.T) {
	if err := runInterruptible("dev", func(ctx context.Context) error { return nil }); err != nil {
		t.Fatalf("first call: %v", err)
	}

	started := make(chan struct{})
	resultErr := make(chan error, 1)
	go func() {
		resultErr <- runInterruptible("dev", func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			return nil
		})
	}()

	<-started
	self, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatalf("finding own process: %v", err)
	}
	if err := self.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("signaling self: %v", err)
	}

	select {
	case err := <-resultErr:
		if !errors.Is(err, errInterrupted) {
			t.Fatalf("err = %v, want errInterrupted — the second call's own signal was not delivered as expected, "+
				"consistent with a leaked registration from the first call interfering", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second runInterruptible call did not return after its own SIGINT")
	}
}
