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

	"infra/internal/diag"
	"infra/internal/planner"
	"infra/internal/registry"
	"infra/internal/state"
	"infra/pkg/address"
	"infra/pkg/provider"
	"infra/pkg/resource"
	"infra/pkg/schema"
)

func TestApplyPersistsStateAfterEveryOperationNotJustAtTheEnd(t *testing.T) {
	dir := t.TempDir()
	backend := state.NewLocal(dir)
	if _, err := backend.Lock(context.Background(), "dev"); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	prov := &countingProvider{resourceType: "test.thing", delay: 40 * time.Millisecond}
	reg := registry.New()
	if err := reg.Register(prov); err != nil {
		t.Fatalf("Register: %v", err)
	}

	var ops []planner.Operation
	for i := 0; i < 3; i++ {
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
			// Do not Fatal here: this select case can still be racing
			// Apply's own goroutine, which is still writing into dir. A
			// t.Fatal on this goroutine calls runtime.Goexit immediately,
			// skipping the <-done below and letting the test function
			// return — and returning is what fires t.TempDir's cleanup
			// (removing dir) — while that goroutine may still be mid-write
			// into it. Recording timedOut and falling through to the
			// unconditional <-done first (same as the normal path) makes
			// sure Apply has actually finished, one way or another, before
			// this test function can return and trigger cleanup.
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
	// This is the assertion that actually carries the invariant, not
	// sawIntermediate above: a Put implementation that batches writes in
	// chunks larger than one (say, every two operations) can still get
	// caught mid-write by the poll above at just the right instant and
	// leave sawIntermediate true, while still never reaching Serial == 3 —
	// measured directly, mutating record to skip every other Put left
	// sawIntermediate true and only this check failed (final Serial = 2,
	// want 3). Do not delete this as redundant with sawIntermediate; it
	// is not.
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

	// Local.Put creates its temp file inside <root>/state (internal/state/local.go),
	// so removing write permission there makes every subsequent Put fail.
	// Options.Backend is state.Backend, an interface, so a test double
	// could simulate a failing Put directly — but only a real permission
	// failure against the real Local proves this codepath actually
	// survives an OS-level write error, which is the failure mode spec §15
	// is written against, so this test deliberately uses the real thing.
	stateDir := filepath.Join(dir, "state")
	if err := os.Chmod(stateDir, 0o500); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(stateDir, 0o700) })

	prov := &countingProvider{resourceType: "test.thing"}
	reg := registry.New()
	if err := reg.Register(prov); err != nil {
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
// close a seam between two well-tested halves: Task 8 proved a destroy/forget
// removes a resource from in-memory state (TestApplyDestroyRemovesFromState,
// TestApplyForgetNeverCallsProviderAndRemovesFromState in apply_test.go);
// TestApplyPersistsStateAfterEveryOperationNotJustAtTheEnd above proves a
// create reaches disk. Neither tested the combination — record's removal
// branch calling Put — and until these, nothing in the repo did either:
// record can drop the removal branch's Put entirely (see the mutation note
// below) and the full suite, `go test ./... -count=1`, stays green.

func TestApplyPersistsDestroyRemovalToDisk(t *testing.T) {
	dir := t.TempDir()
	backend := state.NewLocal(dir)
	if _, err := backend.Lock(context.Background(), "dev"); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	prov := &lifecycleProvider{resourceType: "test.thing"}
	reg := registry.New()
	if err := reg.Register(prov); err != nil {
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
	// Seed disk with the resource too, the way a real prior apply would
	// have left it. Without this, a record bug that drops the removal
	// branch's Put entirely leaves the file exactly as it started — absent
	// — which looks identical to a correctly-persisted removal; seeding
	// first is what makes "still there" and "never written" distinguishable.
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
	if err := reg.Register(poisonProvider{t: t, resourceType: "test.thing"}); err != nil {
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
// Create — the exact contract violation Provider.Create's doc comment
// (pkg/provider/provider.go) now explicitly forbids.
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
// covers the data-loss-class defect the round-1 review found: a Create or
// Update returning (nil, nil) used to mutate nothing, persist nothing, and
// report the run as a plain success — a real create might have gone
// through, with state left completely unaware of it. This proves the
// silent path is gone: a diagnostic naming the address and the provider,
// and no phantom entry in state.
func TestApplyRecordsAHardErrorWhenAProviderReturnsSuccessWithNoResourceState(t *testing.T) {
	backend := newLockedBackend(t, "dev")
	reg := registry.New()
	if err := reg.Register(&nilStateOnCreateProvider{resourceType: "test.thing"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	a := addr("root")
	plan := planWith(op(a, "test.thing", planner.OpCreate))
	g, err := planner.BuildExecution(plan, noDeps)
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}
	st := state.New("proj", "dev")

	_, ds := Apply(context.Background(), plan, g, st, Options{
		Parallelism: 1, PerProvider: 1, Registry: reg, Backend: backend, Environment: "dev",
	})

	if !ds.HasErrors() {
		t.Fatal("expected a diagnostic — a provider returning (nil, nil) on a successful create must not be a silent success")
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

// ctxHonoringBackend wraps a real *state.Local but, unlike it, actually
// honours ctx — refusing to write once the context passed to Put is already
// done. It exists only for the test below: Local.Put today ignores ctx
// entirely, so testing record's behaviour against a real Local cannot tell
// "record passed a cancelled context" apart from "record passed a fine
// context to a backend that would not have noticed either way." A future
// backend — Phase 4 remote state, in particular — is expected to behave
// like this one.
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
// even when the run's own ctx is already cancelled by the time record runs
// — exactly Task 11's SIGINT-mid-apply case, and exactly what a future
// context-honouring backend would otherwise refuse (see
// ctxHonoringBackend). Against the real Local this guarantee cannot be
// observed at all, because Local.Put ignores ctx regardless of what record
// passes it.
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
	if err := reg.Register(prov); err != nil {
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

// TestRecordHardErrorsOnNilStateForEveryKindThatCanReachIt is the verbFor
// pattern again (see TestVerbForMapsEveryOpNodeShapeToTheRightVerb in
// apply_test.go): record's nil-state default arm is reached by three
// distinct OpNode shapes — create, update, and replace's create phase, the
// same three needsDesired recognizes — and
// TestApplyRecordsAHardErrorWhenAProviderReturnsSuccessWithNoResourceState
// above only ever drove it through OpCreate. Nothing proved the identical
// switch arm for OpUpdate or OpReplace/PhaseCreate before this test — the
// exact shape of gap a Task 8 fix round was needed for, where a
// misclassified create was invisible to every test in the package. Calls
// record directly, on a hand-built *run, rather than driving a full Apply
// run for each shape — cheaper, and isolates the switch arm itself the
// same way TestVerbForMapsEveryOpNodeShapeToTheRightVerb isolates verbFor.
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
			if err := reg.Register(&nilStateOnCreateProvider{resourceType: "test.thing"}); err != nil {
				t.Fatalf("Register: %v", err)
			}

			a := addr("root")
			node := planner.OpNode{Address: a, Kind: tc.kind, Phase: tc.phase}
			r := &run{
				ctx: context.Background(),
				st:  state.New("proj", "dev"),
				ops: map[string]*planner.Operation{a.String(): {Address: a, Type: "test.thing", Kind: tc.kind}},
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
// hard error routes through the same stopping gate as a real Put I/O
// failure (TestApplyStopsSchedulingAfterAPersistFailureButKeepsAccurateState
// above): not just that Apply reports an error, but that a completely
// independent sibling operation — ready to launch the moment root finishes,
// with no dependency relationship to it at all — never gets scheduled. See
// the comment on record's default arm (apply.go) for why stopping the
// whole run, not just failing this one operation, is the right call here.
func TestApplyStopsSchedulingASiblingAfterANilStateReturn(t *testing.T) {
	backend := newLockedBackend(t, "dev")
	reg := registry.New()
	if err := reg.Register(&nilStateOnCreateProvider{resourceType: "test.nilthing"}); err != nil {
		t.Fatalf("Register nilstate provider: %v", err)
	}
	if err := reg.Register(&countingProvider{resourceType: "test.thing"}); err != nil {
		t.Fatalf("Register sibling provider: %v", err)
	}

	// root and sibling are independent — noDeps, no edge between them at
	// all. What makes root the one that runs first under Parallelism: 1,
	// deterministically, is graph.Walk.Ready sorting node IDs
	// (internal/graph/walk.go): "create:root" sorts before
	// "create:sibling".
	plan := planWith(
		op(addr("root"), "test.nilthing", planner.OpCreate),
		op(addr("sibling"), "test.thing", planner.OpCreate),
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
	if !reflect.DeepEqual(result.Applied, []address.Address{addr("root")}) {
		t.Errorf("Applied = %v, want exactly [root] — root's provider call did succeed, same accounting as a Put failure", result.Applied)
	}
}
