package executor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/internal/planner"
	"github.com/infrena/infrena/internal/registry"
	"github.com/infrena/infrena/internal/state"
	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/provider"
	"github.com/infrena/infrena/pkg/resource"
	"github.com/infrena/infrena/pkg/schema"
)

func TestApplyPersistsStateAfterEveryOperationNotJustAtTheEnd(t *testing.T) {
	dir := t.TempDir()
	backend := state.NewLocal(dir)
	if _, err := backend.Lock(context.Background(), "dev"); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	prov := &countingProvider{resourceType: "test.thing", delay: 40 * time.Millisecond}
	reg := registry.New()
	if err := reg.Register("test", prov); err != nil {
		t.Fatalf("Register: %v", err)
	}

	var ops []planner.Operation
	for i := range 3 {
		ops = append(ops, op(addr(fmt.Sprintf("r%d", i)), "test.thing", planner.OpCreate))
	}
	plan := planWith(ops...)
	g, err := planner.BuildExecution(plan, noDeps)
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}
	st := state.New("proj", "dev")

	done := make(chan struct{})
	go func() {
		defer close(done)
		Apply(context.Background(), plan, g, st, Options{
			// Parallelism: 1 forces the three creates to run strictly one
			// after another, ~40ms apart, so polling below has a real
			// window to catch a partially-written file.
			Parallelism: 1, PerProvider: 1, Registry: reg, Backend: backend, Environment: "dev",
		})
	}()

	var sawIntermediate, timedOut bool
	deadline := time.After(3 * time.Second)
poll:
	for {
		select {
		case <-done:
			break poll
		case <-deadline:
			// Do not Fatal here. t.Fatal calls runtime.Goexit, which skips
			// the <-done below and returns from the test function — and
			// returning fires t.TempDir's cleanup, removing dir while
			// Apply's goroutine may still be writing into it. Recording
			// timedOut and falling through to the unconditional <-done
			// makes sure Apply has finished before cleanup can run.
			timedOut = true
			break poll
		default:
		}
		onDisk, err := backend.Get(context.Background(), "dev")
		if err == nil && onDisk.Serial > 0 && len(onDisk.Resources) > 0 && len(onDisk.Resources) < 3 {
			sawIntermediate = true
			break poll
		}
		time.Sleep(2 * time.Millisecond)
	}
	<-done

	if timedOut {
		t.Fatal("Apply did not finish in time")
	}
	if !sawIntermediate {
		t.Fatal("never observed a partially-written state file while Apply was running — state must be persisted after every operation, not batched until the end")
	}

	final, err := backend.Get(context.Background(), "dev")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// Not redundant with sawIntermediate above: a Put that batched writes in
	// chunks larger than one could still be caught mid-write by the poll and
	// leave sawIntermediate true while never reaching Serial == 3. This is
	// the check that pins one Put per operation.
	if final.Serial != 3 {
		t.Errorf("final Serial = %d, want 3 — one Put per operation", final.Serial)
	}
	if len(final.Resources) != 3 {
		t.Errorf("final resource count = %d, want 3", len(final.Resources))
	}
}

func TestApplyStopsSchedulingAfterAPersistFailureButKeepsAccurateState(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions; this test needs a real write failure")
	}

	dir := t.TempDir()
	backend := state.NewLocal(dir)
	if _, err := backend.Lock(context.Background(), "dev"); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	// Local.Put creates its temp file inside <root>/state, so removing write
	// permission there makes every subsequent Put fail. A test double could
	// simulate a failing Put, but only a real permission failure against the
	// real Local proves this codepath survives an OS-level write error, which
	// is the failure mode it is written against.
	stateDir := filepath.Join(dir, "state")
	if err := os.Chmod(stateDir, 0o500); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(stateDir, 0o700) })

	prov := &countingProvider{resourceType: "test.thing"}
	reg := registry.New()
	if err := reg.Register("test", prov); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// root has one dependent, child: if the run kept scheduling after
	// root's Put failed, child — which becomes ready only once root's
	// completion unlocks it, exactly the sequence a real dependency graph
	// produces — would still get created. Its absence is what proves
	// scheduling actually stopped, not merely that a diagnostic came back.
	plan := planWith(
		op(addr("root"), "test.thing", planner.OpCreate),
		op(addr("child"), "test.thing", planner.OpCreate),
	)
	g, err := planner.BuildExecution(plan, depsFrom(map[string][]string{"root": {"child"}}))
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}
	st := state.New("proj", "dev")

	result, ds := Apply(context.Background(), plan, g, st, Options{
		Parallelism: 2, PerProvider: 2, Registry: reg, Backend: backend, Environment: "dev",
	})

	if !ds.HasErrors() {
		t.Fatal("expected a diagnostic reporting the persistence failure")
	}
	// HasErrors alone accepts any of Apply's error paths, including ones
	// unrelated to persistence entirely — pin it to the specific diagnostic
	// this test is actually about.
	var sawPersistDiagnostic bool
	for _, d := range ds {
		if strings.Contains(d.Summary, "failed to persist state after") {
			sawPersistDiagnostic = true
			break
		}
	}
	if !sawPersistDiagnostic {
		t.Errorf("no diagnostic mentioned a persist failure; got: %+v", ds)
	}
	if _, ok := st.Get(addr("root")); !ok {
		t.Error("root's in-memory state must still reflect the successful create — a Put failure must not roll back an accurate mutation")
	}
	if _, ok := st.Get(addr("child")); ok {
		t.Error("child must not have been created — the run must stop scheduling new work once persistence has failed")
	}
	if result.State != st {
		t.Error("Result.State must be the same *state.State the run mutated, Put failure or not")
	}
	if !reflect.DeepEqual(result.Applied, []address.Address{addr("root")}) {
		t.Errorf("Applied = %v, want exactly [root]", result.Applied)
	}
}

// TestApplyPersistsDestroyRemovalToDisk and TestApplyPersistsForgetRemovalToDisk
// close a seam between two halves covered separately elsewhere: that a destroy
// or forget removes a resource from in-memory state, and that a create reaches
// disk. Neither covers the combination — record's removal branch calling Put —
// so without these, dropping that Put leaves the whole suite green.

func TestApplyPersistsDestroyRemovalToDisk(t *testing.T) {
	dir := t.TempDir()
	backend := state.NewLocal(dir)
	if _, err := backend.Lock(context.Background(), "dev"); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	prov := &lifecycleProvider{resourceType: "test.thing"}
	reg := registry.New()
	if err := reg.Register("test", prov); err != nil {
		t.Fatalf("Register: %v", err)
	}

	a := addr("gone")
	plan := planWith(op(a, "test.thing", planner.OpDestroy))
	g, err := planner.BuildExecution(plan, noDeps)
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}
	st := state.New("proj", "dev")
	st.Set(&resource.ResourceState{Address: a, Type: "test.thing", Provider: "lifecycle", ProviderID: "gone-1"})
	// Seed disk with the resource too, the way a real prior apply would have
	// left it. Without this, dropping the removal branch's Put leaves the
	// file exactly as it started — absent — which looks identical to a
	// correctly-persisted removal.
	if err := backend.Put(context.Background(), "dev", st); err != nil {
		t.Fatalf("seed Put: %v", err)
	}
	seededSerial := st.Serial

	result, ds := Apply(context.Background(), plan, g, st, Options{
		Parallelism: 1, PerProvider: 1, Registry: reg, Backend: backend, Environment: "dev",
	})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if len(result.Failed) != 0 {
		t.Fatalf("Failed = %v", result.Failed)
	}

	onDisk, err := backend.Get(context.Background(), "dev")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, ok := onDisk.Resources[a.String()]; ok {
		t.Error("destroyed resource is still on disk — record's removed branch must call Put too, not just st.Remove in memory")
	}
	if onDisk.Serial != seededSerial+1 {
		t.Errorf("on-disk Serial = %d, want %d — the destroy's own Put must have run exactly once", onDisk.Serial, seededSerial+1)
	}
}

func TestApplyPersistsForgetRemovalToDisk(t *testing.T) {
	dir := t.TempDir()
	backend := state.NewLocal(dir)
	if _, err := backend.Lock(context.Background(), "dev"); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	reg := registry.New()
	if err := reg.Register("test", poisonProvider{t: t, resourceType: "test.thing"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	a := addr("old")
	plan := planWith(op(a, "test.thing", planner.OpForget))
	g, err := planner.BuildExecution(plan, noDeps)
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}
	st := state.New("proj", "dev")
	st.Set(&resource.ResourceState{Address: a, Type: "test.thing", Provider: "poison", ProviderID: "old-1"})
	if err := backend.Put(context.Background(), "dev", st); err != nil {
		t.Fatalf("seed Put: %v", err)
	}
	seededSerial := st.Serial

	result, ds := Apply(context.Background(), plan, g, st, Options{
		Parallelism: 1, PerProvider: 1, Registry: reg, Backend: backend, Environment: "dev",
	})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if len(result.Failed) != 0 {
		t.Fatalf("Failed = %v", result.Failed)
	}

	onDisk, err := backend.Get(context.Background(), "dev")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, ok := onDisk.Resources[a.String()]; ok {
		t.Error("forgotten resource is still on disk — record's removed branch must call Put too, not just st.Remove in memory")
	}
	if onDisk.Serial != seededSerial+1 {
		t.Errorf("on-disk Serial = %d, want %d — the forget's own Put must have run exactly once", onDisk.Serial, seededSerial+1)
	}
}

// nilStateOnCreateProvider returns success with no resource state from
// Create — the contract violation Provider.Create's doc comment forbids.
type nilStateOnCreateProvider struct {
	resourceType string
}

func (p *nilStateOnCreateProvider) Name() string { return "nilstate" }
func (p *nilStateOnCreateProvider) Definitions() []*schema.ResourceDefinition {
	return []*schema.ResourceDefinition{{Type: p.resourceType}}
}
func (p *nilStateOnCreateProvider) Create(context.Context, *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, nil
}
func (p *nilStateOnCreateProvider) Read(context.Context, *resource.ResourceState) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *nilStateOnCreateProvider) Update(context.Context, *resource.ResourceState, *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *nilStateOnCreateProvider) Delete(context.Context, *resource.ResourceState) error {
	return provider.ErrNotImplemented
}
func (p *nilStateOnCreateProvider) Discover(context.Context, provider.DiscoverRequest) ([]provider.DiscoveredResource, error) {
	return nil, provider.ErrNotImplemented
}
func (p *nilStateOnCreateProvider) Import(context.Context, string, string) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *nilStateOnCreateProvider) ClassifyError(error) provider.Retryability {
	return provider.NotSafeToRetry
}

var _ provider.Provider = (*nilStateOnCreateProvider)(nil)

// TestApplyRecordsAHardErrorWhenAProviderReturnsSuccessWithNoResourceState
// covers a data-loss path: a Create or Update returning (nil, nil) must not
// mutate nothing, persist nothing and report the run as a plain success,
// because a real create may well have gone through with state left unaware of
// it. A diagnostic must name the address and the provider, and state must
// gain no phantom entry.
func TestApplyRecordsAHardErrorWhenAProviderReturnsSuccessWithNoResourceState(t *testing.T) {
	backend := newLockedBackend(t, "dev")
	reg := registry.New()
	if err := reg.Register("test", &nilStateOnCreateProvider{resourceType: "test.thing"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	a := addr("root")
	plan := planWith(op(a, "test.thing", planner.OpCreate))
	g, err := planner.BuildExecution(plan, noDeps)
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}
	st := state.New("proj", "dev")

	result, ds := Apply(context.Background(), plan, g, st, Options{
		Parallelism: 1, PerProvider: 1, Registry: reg, Backend: backend, Environment: "dev",
	})

	if !ds.HasErrors() {
		t.Fatal("expected a diagnostic — a provider returning (nil, nil) on a successful create must not be a silent success")
	}

	// The operation must be REPRESENTED in Result, not merely absent from
	// Applied. An operation that falls out of all three sets leaves the
	// summary reading "0 applied, 0 failed, 0 skipped" for a run in which the
	// provider may have created real infrastructure, contradicting the
	// diagnostic on stderr.
	id := "create:" + a.String()
	if _, ok := result.Failed[id]; !ok {
		t.Errorf("Failed = %v, want an entry for %s — a hard error must appear in Result, not only in diagnostics", result.Failed, id)
	}
	if len(result.Applied) != 0 {
		t.Errorf("Applied = %v, want empty — state has no entry for %s", result.Applied, a)
	}
	var found bool
	for _, d := range ds {
		if strings.Contains(d.Summary, a.String()) && strings.Contains(d.Summary, "nilstate") {
			found = true
		}
	}
	if !found {
		t.Errorf("no diagnostic named both the address and the provider; got: %+v", ds)
	}
	if _, ok := st.Get(a); ok {
		t.Error("state must not record a resource the provider never actually described")
	}
}

// ctxHonoringBackend wraps a real *state.Local but, unlike it, honours ctx —
// refusing to write once the context passed to Put is already done. It exists
// only for the test below: Local.Put ignores ctx entirely, so testing record
// against a real Local cannot tell "record passed a cancelled context" apart
// from "record passed a fine context to a backend that would not have noticed
// either way". A remote backend is expected to behave like this one.
type ctxHonoringBackend struct {
	*state.Local
}

func (b *ctxHonoringBackend) Put(ctx context.Context, environment string, s *state.State) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return b.Local.Put(ctx, environment, s)
}

// startSignalingProvider blocks inside Create until told to proceed, so a
// test can cancel the run's context at a precise moment — after the
// provider call is already in flight, before it returns — without racing
// on wall-clock timing the way a fixed delay would.
type startSignalingProvider struct {
	resourceType string
	started      chan struct{}
	proceed      chan struct{}
}

func (p *startSignalingProvider) Name() string { return "startsignal" }
func (p *startSignalingProvider) Definitions() []*schema.ResourceDefinition {
	return []*schema.ResourceDefinition{{Type: p.resourceType}}
}
func (p *startSignalingProvider) Create(ctx context.Context, d *resource.DesiredResource) (*resource.ResourceState, error) {
	close(p.started)
	<-p.proceed
	return &resource.ResourceState{Address: d.Address, Type: d.Type, Provider: p.Name(), ProviderID: d.Address.String(), Attributes: d.Attrs}, nil
}
func (p *startSignalingProvider) Read(context.Context, *resource.ResourceState) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *startSignalingProvider) Update(context.Context, *resource.ResourceState, *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *startSignalingProvider) Delete(context.Context, *resource.ResourceState) error {
	return provider.ErrNotImplemented
}
func (p *startSignalingProvider) Discover(context.Context, provider.DiscoverRequest) ([]provider.DiscoveredResource, error) {
	return nil, provider.ErrNotImplemented
}
func (p *startSignalingProvider) Import(context.Context, string, string) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *startSignalingProvider) ClassifyError(error) provider.Retryability {
	return provider.NotSafeToRetry
}

var _ provider.Provider = (*startSignalingProvider)(nil)

// TestApplyPersistsAnAlreadyCompletedOperationEvenAfterItsOwnContextIsCancelled
// pins record's use of context.WithoutCancel: it must persist a completion
// even when the run's own ctx is already cancelled by the time record runs —
// the SIGINT-mid-apply case, and what a context-honouring backend would
// otherwise refuse. Against the real Local the guarantee cannot be observed
// at all, because Local.Put ignores ctx regardless of what record passes it.
func TestApplyPersistsAnAlreadyCompletedOperationEvenAfterItsOwnContextIsCancelled(t *testing.T) {
	dir := t.TempDir()
	local := state.NewLocal(dir)
	if _, err := local.Lock(context.Background(), "dev"); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	backend := &ctxHonoringBackend{Local: local}

	prov := &startSignalingProvider{
		resourceType: "test.thing",
		started:      make(chan struct{}),
		proceed:      make(chan struct{}),
	}
	reg := registry.New()
	if err := reg.Register("test", prov); err != nil {
		t.Fatalf("Register: %v", err)
	}

	plan := planWith(op(addr("root"), "test.thing", planner.OpCreate))
	g, err := planner.BuildExecution(plan, noDeps)
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}
	st := state.New("proj", "dev")

	ctx, cancel := context.WithCancel(context.Background())
	var result Result
	var ds diag.Diagnostics
	done := make(chan struct{})
	go func() {
		defer close(done)
		result, ds = Apply(ctx, plan, g, st, Options{
			Parallelism: 1, PerProvider: 1, Registry: reg, Backend: backend, Environment: "dev",
		})
	}()

	select {
	case <-prov.started:
	case <-time.After(3 * time.Second):
		t.Fatal("Create was never called")
	}
	// The create is now in flight, already dispatched. Cancel the run's own
	// context before it returns: record's Put must still land, because the
	// operation it is about to persist already completed for real by the
	// time the owner goroutine sees it.
	cancel()
	close(prov.proceed)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Apply did not finish in time")
	}

	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if len(result.Applied) != 1 {
		t.Fatalf("Applied = %v, want [root]", result.Applied)
	}

	onDisk, err := local.Get(context.Background(), "dev")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if onDisk.Serial == 0 || len(onDisk.Resources) == 0 {
		t.Fatal("root completed but never reached disk — record must persist with a context that survives Apply's own cancellation (context.WithoutCancel), not r.ctx directly")
	}
}

// TestRecordHardErrorsOnNilStateForEveryKindThatCanReachIt covers all three
// OpNode shapes that reach record's nil-state default arm — create, update,
// and replace's create phase, the same three needsDesired recognizes. The
// end-to-end test above only ever drives it through OpCreate, so a
// misclassified update or replace-create phase would go unnoticed. It calls
// record directly on a hand-built *run, which isolates the switch arm and is
// far cheaper than a full Apply per shape.
func TestRecordHardErrorsOnNilStateForEveryKindThatCanReachIt(t *testing.T) {
	cases := []struct {
		name  string
		kind  planner.OpKind
		phase planner.Phase
	}{
		{"create", planner.OpCreate, planner.PhaseCreate},
		{"update", planner.OpUpdate, planner.PhaseCreate},
		{"replace create phase", planner.OpReplace, planner.PhaseCreate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backend := newLockedBackend(t, "dev")
			reg := registry.New()
			if err := reg.Register("test", &nilStateOnCreateProvider{resourceType: "test.thing"}); err != nil {
				t.Fatalf("Register: %v", err)
			}

			a := addr("root")
			node := planner.OpNode{Address: a, Kind: tc.kind, Phase: tc.phase}
			r := &run{
				ctx: context.Background(),
				st:  state.New("proj", "dev"),
				ops: map[string]*planner.Operation{a.String(): {Address: a, Type: "test.thing", Kind: tc.kind, Provider: "test"}},
				opts: Options{
					Registry: reg, Backend: backend, Environment: "dev",
				},
			}

			err := r.record(nodeResult{node: node, state: nil, removed: false})
			if err == nil {
				t.Fatalf("record returned nil for Kind=%s Phase=%d — a successful create/update/replace-create-phase with no resource state must be a hard error", tc.kind, tc.phase)
			}
			if !strings.Contains(err.Error(), a.String()) {
				t.Errorf("error %q does not name the address", err.Error())
			}
			if !strings.Contains(err.Error(), "nilstate") {
				t.Errorf("error %q does not name the provider", err.Error())
			}
			if _, ok := r.st.Get(a); ok {
				t.Error("state must not record a resource the provider never actually described")
			}
		})
	}
}

// TestApplyStopsSchedulingASiblingAfterANilStateReturn proves the nil-state
// hard error routes through the same stopping gate as a real Put I/O failure:
// not just that Apply reports an error, but that a completely independent
// sibling operation never gets scheduled. See the comment on record's default
// arm (apply.go) for why the whole run stops rather than just this operation.
//
// root's own accounting deliberately differs from the Put-failure case. A Put
// I/O failure still runs st.Set/st.Remove before the write fails, so state
// HAS an entry for the address and Applied recording it is accurate. A
// nil-state return never reaches st.Set at all, so Applied naming it would
// make Result self-contradictory: a caller reading result.State would find
// nothing at an address result.Applied claims succeeded. tracker.result
// enforces this — Applied carries a non-removal address only when
// Result.State actually has an entry for it.
func TestApplyStopsSchedulingASiblingAfterANilStateReturn(t *testing.T) {
	backend := newLockedBackend(t, "dev")
	reg := registry.New()
	if err := reg.Register("nilstate", &nilStateOnCreateProvider{resourceType: "test.nilthing"}); err != nil {
		t.Fatalf("Register nilstate provider: %v", err)
	}
	if err := reg.Register("counting", &countingProvider{resourceType: "test.thing"}); err != nil {
		t.Fatalf("Register sibling provider: %v", err)
	}

	// root and sibling are independent — noDeps, no edge between them at
	// all. What makes root the one that runs first under Parallelism: 1,
	// deterministically, is graph.Walk.Ready sorting node IDs
	// (internal/graph/walk.go): "create:root" sorts before
	// "create:sibling".
	plan := planWith(
		opIn(addr("root"), "test.nilthing", planner.OpCreate, "nilstate"),
		opIn(addr("sibling"), "test.thing", planner.OpCreate, "counting"),
	)
	g, err := planner.BuildExecution(plan, noDeps)
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}
	st := state.New("proj", "dev")

	result, ds := Apply(context.Background(), plan, g, st, Options{
		Parallelism: 1, PerProvider: 1, Registry: reg, Backend: backend, Environment: "dev",
	})

	if !ds.HasErrors() {
		t.Fatal("expected a diagnostic reporting the nil-state failure")
	}
	if _, ok := st.Get(addr("root")); ok {
		t.Error("state must not record root — the provider never described it")
	}
	if _, ok := st.Get(addr("sibling")); ok {
		t.Error("sibling must not have been created — a nil-state return must stop scheduling new work, exactly like a Put I/O failure does")
	}
	if len(result.Applied) != 0 {
		t.Errorf("Applied = %v, want empty — root's provider call succeeded, but nothing was ever recorded for it, so Applied must not claim state has an entry it does not", result.Applied)
	}
}
