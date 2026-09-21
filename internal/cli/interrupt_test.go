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
// test. "The signal is received", "the context is cancelled" and "the
// dispatched provider call survives cancellation" are proved separately by
// TestRunInterruptibleFirstSignal and
// executor.TestApplySurvivesCancellationDuringDispatch, and their product is
// not automatically true. A refactor in which runInterruptible cancels a
// context that never reaches operationContext — because whatever wires it
// around executor.Apply passes a different context than the one it cancels —
// would pass both narrower tests and still break the end-to-end contract: a
// SIGINT during `infrena apply` must not abort an in-flight create.
//
// So this composes the two real production pieces with a real OS signal rather
// than a programmatic cancel(). The only synthetic part is the fake provider's
// LatencyMS, which holds the create in flight long enough for the signal to
// land inside it.
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

// TestRunInterruptibleErrorDoesNotClaimStateWasSavedWhenFnFailed pins that
// runInterruptible never asserts an outcome fn did not report: only fn knows
// whether state was persisted. errInterrupted's text used to say "state was
// saved, and the lock was released" unconditionally, even when fn's own error
// said persistence failed. Here fn returns a persist-failure error after its
// ctx is cancelled, and the combined error must not carry the false claim.
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
// does NOT verify that defer signal.Stop(sig) runs or that it prevents a
// registration leak: Go's os/signal delivers to every registered channel
// independently, so a leaked registration from a cleanly-completed call cannot
// steal or delay a later call's delivery. What it does pin is narrower — one
// runInterruptible call finishing normally does not leave the process unable to
// signal-handle a second, later call. The leak itself has no in-process test;
// see the comment on `defer signal.Stop(sig)`.
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
