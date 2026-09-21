package executor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/internal/graph"
	"github.com/infrena/infrena/internal/planner"
	"github.com/infrena/infrena/internal/registry"
	"github.com/infrena/infrena/internal/state"
	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/resource"
	"github.com/infrena/infrena/pkg/value"
	testprovider "github.com/infrena/infrena/providers/test"
)

func opnode(name string) planner.OpNode {
	return planner.OpNode{Address: address.Address{Name: name}, Kind: planner.OpCreate, Phase: planner.PhaseCreate}
}

func idsOf(nodes []planner.OpNode) []string {
	ids := make([]string, len(nodes))
	for i, n := range nodes {
		ids[i] = n.ID()
	}
	return ids
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestTrackerSkipsTransitivelyAndLeavesUnrelatedBranchesAlone is a pure unit
// test of the tracker alone — no provider, no coordinator loop. The chain
// a -> b -> c is three nodes, not two: a two-node fixture cannot tell "b was
// skipped because Skip is transitive" apart from "b was skipped because it is
// a's one direct dependent", so c is what forces the implementation to use
// Skip's transitive return. d shares no edge with anything, and proves a
// failure in one branch does not stop an unrelated one.
func TestTrackerSkipsTransitivelyAndLeavesUnrelatedBranchesAlone(t *testing.T) {
	g := graph.New[planner.OpNode]()
	a, b, c, d := opnode("a"), opnode("b"), opnode("c"), opnode("d")
	for _, n := range []planner.OpNode{a, b, c, d} {
		g.Add(n)
	}
	g.Edge(a.ID(), b.ID())
	g.Edge(b.ID(), c.ID())
	// d has no edge to anything: an unrelated branch.

	w, err := g.Walk()
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	ready := w.Ready()
	if len(ready) != 2 || ready[0].ID() != a.ID() || ready[1].ID() != d.ID() {
		t.Fatalf("Ready() = %v, want [%s %s]", idsOf(ready), a.ID(), d.ID())
	}

	tr := newTracker(nil, nil)
	var ds diag.Diagnostics

	tr.recordFailure(w, a, errors.New("boom"), &ds)
	tr.recordSuccess(w, d)

	res := tr.result(nil)

	if got := res.Applied; len(got) != 1 || got[0].String() != d.Address.String() {
		t.Fatalf("Applied = %v, want [d]", got)
	}
	if _, ok := res.Failed[a.ID()]; !ok || len(res.Failed) != 1 {
		t.Fatalf("Failed = %v, want exactly {%s: <err>}", res.Failed, a.ID())
	}
	if want := []string{b.ID(), c.ID()}; !equalStrings(res.Skipped, want) {
		t.Fatalf("Skipped = %v, want %v", res.Skipped, want)
	}
	if w.Remaining() != 0 {
		t.Fatalf("Remaining() = %d, want 0 — something is stuck neither applied, failed nor skipped", w.Remaining())
	}
}

// TestApplyIsolatesAFailureEndToEnd proves the isolation above is actually
// wired into Apply's real coordinator loop, not merely that tracker is correct
// sitting on its own. a's create is configured to fail; b depends on a; c is
// unrelated.
func TestApplyIsolatesAFailureEndToEnd(t *testing.T) {
	cloudPath := t.TempDir() + "/fake-cloud.json"
	cloud := &testprovider.Cloud{
		Resources: map[string]*testprovider.CloudResource{},
		Failures: []testprovider.FailureRule{
			{Op: "create", Address: "a", Nth: 1},
		},
	}
	if err := cloud.Save(cloudPath); err != nil {
		t.Fatalf("seeding fake cloud: %v", err)
	}

	reg := registry.New()
	if err := reg.Register("test", testprovider.New(cloudPath)); err != nil {
		t.Fatalf("Register: %v", err)
	}

	p := &planner.Plan{
		Version: planner.PlanVersion,
		Operations: []planner.Operation{
			{Provider: "test", Address: address.Address{Name: "a"}, Type: "fake.network", Kind: planner.OpCreate,
				After: map[string]value.Value{"cidr": value.String("10.0.0.0/24", value.SourceExplicit)}},
			{Provider: "test", Address: address.Address{Name: "b"}, Type: "fake.network", Kind: planner.OpCreate,
				After: map[string]value.Value{"cidr": value.String("10.0.1.0/24", value.SourceExplicit)}},
			{Provider: "test", Address: address.Address{Name: "c"}, Type: "fake.network", Kind: planner.OpCreate,
				After: map[string]value.Value{"cidr": value.String("10.0.2.0/24", value.SourceExplicit)}},
		},
	}
	deps := func(a address.Address) []address.Address {
		if a.Name == "a" {
			return []address.Address{{Name: "b"}}
		}
		return nil
	}
	g, err := planner.BuildExecution(p, deps)
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}

	backend := state.NewLocal(t.TempDir())
	st, err := backend.Get(context.Background(), "dev")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	opts := Options{
		Parallelism: 2, PerProvider: 2, Registry: reg, Backend: backend,
		Environment: "dev", Retry: RetryPolicy{MaxAttempts: 1}, Now: time.Now,
	}

	type outcome struct {
		res Result
		ds  diag.Diagnostics
	}
	done := make(chan outcome, 1)
	go func() {
		res, ds := Apply(context.Background(), p, g, st, opts)
		done <- outcome{res, ds}
	}()

	var got outcome
	select {
	case got = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Apply did not return within 10s — a failed but b was never marked done, failed or skipped, so the coordinator loop is stuck waiting on it")
	}

	if len(got.res.Applied) != 1 || got.res.Applied[0].String() != "c" {
		t.Fatalf("Applied = %v, want [c]", got.res.Applied)
	}
	if _, ok := got.res.Failed["create:a"]; !ok || len(got.res.Failed) != 1 {
		t.Fatalf("Failed = %v, want exactly {create:a: <err>}", got.res.Failed)
	}
	if want := []string{"create:b"}; !equalStrings(got.res.Skipped, want) {
		t.Fatalf("Skipped = %v, want %v", got.res.Skipped, want)
	}
}

// TestTrackerCollapsesAReplacesTwoPhasesIntoOneAppliedEntryFavoringTheCreatePhase
// pins the dedup property Result.Applied documents: an OpReplace is two
// OpNodes sharing one address (destroy phase, then create phase), and both
// call recordSuccess — Applied must report that address exactly once, and the
// entry kept must be the CREATE phase's, the one that leaves the resource
// present, not the destroy phase's now-stale "removal" judgement.
//
// This assertion alone does not force keeping the LAST write over the first:
// a destroy-phase entry skips the presence-in-state check unconditionally, so
// it would satisfy the count here by accident. The sibling test below is what
// actually forces it.
func TestTrackerCollapsesAReplacesTwoPhasesIntoOneAppliedEntryFavoringTheCreatePhase(t *testing.T) {
	a := address.Address{Name: "swap"}
	destroy := planner.OpNode{Address: a, Kind: planner.OpReplace, Phase: planner.PhaseDestroy}
	create := planner.OpNode{Address: a, Kind: planner.OpReplace, Phase: planner.PhaseCreate}

	g := graph.New[planner.OpNode]()
	g.Add(destroy)
	g.Add(create)
	w, err := g.Walk()
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	w.Ready() // dispatches both — no edge between them in this fixture

	tr := newTracker(nil, nil)
	tr.recordSuccess(w, destroy)
	tr.recordSuccess(w, create)

	st := state.New("proj", "dev")
	st.Set(&resource.ResourceState{Address: a, Type: "test.thing", Provider: "p", ProviderID: "swap-new"})

	res := tr.result(st)
	if len(res.Applied) != 1 || res.Applied[0].String() != a.String() {
		t.Fatalf("Applied = %v, want exactly [%s] — the replace's two phases must collapse into one entry", res.Applied, a.String())
	}
}

// TestTrackerExcludesAReplaceWhoseCreatePhaseNeverReachedState is the test
// that forces favouring the create phase over the destroy phase: it
// reproduces a replace whose destroy phase genuinely succeeded — st no longer
// has the address, exactly as run.record's removed branch leaves it — but
// whose create phase hit the nil-state hard error (run.record's default arm),
// so state was NEVER re-set. Apply calls recordSuccess for the create phase
// regardless of what record returned, so the tracker sees both phases
// complete either way.
//
// swap must be ABSENT from Applied: the create phase's entry (removal: false)
// is the one dedup keeps, and state has no entry for it. Keeping the destroy
// phase's entry instead (removal: true) skips the presence check
// unconditionally and reports swap as applied even though the replace never
// finished — a false success.
func TestTrackerExcludesAReplaceWhoseCreatePhaseNeverReachedState(t *testing.T) {
	a := address.Address{Name: "swap"}
	destroy := planner.OpNode{Address: a, Kind: planner.OpReplace, Phase: planner.PhaseDestroy}
	create := planner.OpNode{Address: a, Kind: planner.OpReplace, Phase: planner.PhaseCreate}

	g := graph.New[planner.OpNode]()
	g.Add(destroy)
	g.Add(create)
	w, err := g.Walk()
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	w.Ready()

	tr := newTracker(nil, nil)
	tr.recordSuccess(w, destroy)
	tr.recordSuccess(w, create)

	// st never gets the address back — the create phase's nil-state hard
	// error means run.record never called st.Set for it.
	st := state.New("proj", "dev")

	res := tr.result(st)
	if len(res.Applied) != 0 {
		t.Fatalf("Applied = %v, want empty — swap's create phase never reached state, so it must not be reported applied", res.Applied)
	}
}

// TestRecordFailureEmitsExactlyOneEventSkippedPerStrandedNode proves
// recordFailure reports EventSkipped for every node Walk.Skip strands, and
// only those. The chain a -> b -> c strands TWO nodes from one failure: a
// single-dependent fixture could not tell "one event per skipped node" apart
// from "one event per failure, no matter how many nodes it strands". d is an
// independent branch that succeeds on its own, proving isolation extends to
// events too.
func TestRecordFailureEmitsExactlyOneEventSkippedPerStrandedNode(t *testing.T) {
	g := graph.New[planner.OpNode]()
	a, b, c, d := opnode("a"), opnode("b"), opnode("c"), opnode("d")
	for _, n := range []planner.OpNode{a, b, c, d} {
		g.Add(n)
	}
	g.Edge(a.ID(), b.ID())
	g.Edge(b.ID(), c.ID())

	w, err := g.Walk()
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	w.Ready() // dispatches a and d

	fixedTime := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	var events []Event
	tr := newTracker(
		func(e Event) { events = append(events, e) },
		func() time.Time { return fixedTime },
	)

	var ds diag.Diagnostics
	tr.recordFailure(w, a, errors.New("boom"), &ds)
	tr.recordSuccess(w, d)

	var skipEvents []Event
	for _, e := range events {
		if e.Kind == EventSkipped {
			skipEvents = append(skipEvents, e)
		}
	}
	if len(skipEvents) != 2 {
		t.Fatalf("EventSkipped events = %+v, want exactly 2 — one per stranded node (b and c)", skipEvents)
	}

	byAddr := map[string]Event{}
	for _, e := range skipEvents {
		byAddr[e.Address.String()] = e
	}
	for _, addr := range []string{b.Address.String(), c.Address.String()} {
		e, ok := byAddr[addr]
		if !ok {
			t.Fatalf("no EventSkipped for %s; got %+v", addr, skipEvents)
		}
		if e.Op != planner.OpCreate {
			t.Errorf("%s: Op = %v, want %v", addr, e.Op, planner.OpCreate)
		}
		if e.Attempt != 0 {
			t.Errorf("%s: Attempt = %d, want 0 — a skipped operation is never attempted (types.go)", addr, e.Attempt)
		}
		if !e.At.Equal(fixedTime) {
			t.Errorf("%s: At = %v, want the tracker's injected clock value %v — a fixed clock must produce a deterministic timestamp", addr, e.At, fixedTime)
		}
	}

	// d succeeded on its own, unrelated branch: it must produce no
	// EventSkipped at all. An over-broad emit shows up here first.
	if _, ok := byAddr[d.Address.String()]; ok {
		t.Fatalf("unexpected EventSkipped for independent branch d: %+v", skipEvents)
	}
}

// TestApplyEmitsExactlyOneEventSkippedPerStrandedNodeThroughOnEvent is the
// end-to-end counterpart of the tracker unit test above: it drives a real
// Apply run through Options.OnEvent, the entry point a caller actually
// observes. a's create fails; b depends on a; c depends on b, so ONE failure
// strands TWO nodes transitively; d is unrelated and succeeds.
//
// Options.OnEvent is called concurrently from worker goroutines, so the
// collector below is mutex-guarded — recording without one is a data race
// Apply's real worker pool can trigger, caught by `go test -race`.
func TestApplyEmitsExactlyOneEventSkippedPerStrandedNodeThroughOnEvent(t *testing.T) {
	cloudPath := t.TempDir() + "/fake-cloud.json"
	cloud := &testprovider.Cloud{
		Resources: map[string]*testprovider.CloudResource{},
		Failures: []testprovider.FailureRule{
			{Op: "create", Address: "a", Nth: 1},
		},
	}
	if err := cloud.Save(cloudPath); err != nil {
		t.Fatalf("seeding fake cloud: %v", err)
	}

	reg := registry.New()
	if err := reg.Register("test", testprovider.New(cloudPath)); err != nil {
		t.Fatalf("Register: %v", err)
	}

	p := &planner.Plan{
		Version: planner.PlanVersion,
		Operations: []planner.Operation{
			{Provider: "test", Address: address.Address{Name: "a"}, Type: "fake.network", Kind: planner.OpCreate,
				After: map[string]value.Value{"cidr": value.String("10.0.0.0/24", value.SourceExplicit)}},
			{Provider: "test", Address: address.Address{Name: "b"}, Type: "fake.network", Kind: planner.OpCreate,
				After: map[string]value.Value{"cidr": value.String("10.0.1.0/24", value.SourceExplicit)}},
			{Provider: "test", Address: address.Address{Name: "c"}, Type: "fake.network", Kind: planner.OpCreate,
				After: map[string]value.Value{"cidr": value.String("10.0.2.0/24", value.SourceExplicit)}},
			{Provider: "test", Address: address.Address{Name: "d"}, Type: "fake.network", Kind: planner.OpCreate,
				After: map[string]value.Value{"cidr": value.String("10.0.3.0/24", value.SourceExplicit)}},
		},
	}
	// deps reports DEPENDENTS of the given address (BuildExecution's own
	// doc comment): deps(a) = [b] means b depends on a. Chaining a -> b -> c
	// strands both b and c from a's single failure; d has no entry at all,
	// so it is nobody's dependent and nobody's dependency — genuinely
	// unrelated.
	deps := func(addr address.Address) []address.Address {
		switch addr.Name {
		case "a":
			return []address.Address{{Name: "b"}}
		case "b":
			return []address.Address{{Name: "c"}}
		}
		return nil
	}
	g, err := planner.BuildExecution(p, deps)
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}

	backend := state.NewLocal(t.TempDir())
	st, err := backend.Get(context.Background(), "dev")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	fixedTime := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

	var mu sync.Mutex
	var events []Event
	opts := Options{
		Parallelism: 2, PerProvider: 2, Registry: reg, Backend: backend,
		Environment: "dev", Retry: RetryPolicy{MaxAttempts: 1},
		Now: func() time.Time { return fixedTime },
		OnEvent: func(e Event) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, e)
		},
	}

	type outcome struct {
		res Result
		ds  diag.Diagnostics
	}
	done := make(chan outcome, 1)
	go func() {
		res, ds := Apply(context.Background(), p, g, st, opts)
		done <- outcome{res, ds}
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Apply did not return within 10s")
	}

	mu.Lock()
	defer mu.Unlock()

	var skipEvents []Event
	for _, e := range events {
		if e.Kind == EventSkipped {
			skipEvents = append(skipEvents, e)
		}
	}
	if len(skipEvents) != 2 {
		t.Fatalf("EventSkipped events = %+v, want exactly 2 — one per stranded node (b and c)", skipEvents)
	}

	byAddr := map[string]Event{}
	for _, e := range skipEvents {
		byAddr[e.Address.String()] = e
	}
	for _, addr := range []string{"b", "c"} {
		e, ok := byAddr[addr]
		if !ok {
			t.Fatalf("no EventSkipped for %s; got %+v", addr, skipEvents)
		}
		if e.Op != planner.OpCreate {
			t.Errorf("%s: Op = %v, want %v", addr, e.Op, planner.OpCreate)
		}
		if e.Attempt != 0 {
			t.Errorf("%s: Attempt = %d, want 0 — a skipped operation is never attempted (types.go)", addr, e.Attempt)
		}
		// Equality with the fixed clock Options.Now supplies is what proves
		// the clock is plumbed through rather than reached for directly.
		if !e.At.Equal(fixedTime) {
			t.Errorf("%s: At = %v, want %v (Options.Now) — a real time.Now() call would not match a fixed injected clock", addr, e.At, fixedTime)
		}
	}

	// d is a genuinely unrelated, independent, successful branch: it must
	// produce no EventSkipped.
	if _, ok := byAddr["d"]; ok {
		t.Fatalf("unexpected EventSkipped for independent branch d: %+v", skipEvents)
	}
}

// TestRecordFailureNeverEmitsEventSkippedTwiceForASharedDependent: c depends
// on BOTH a and b, and both fail in two separate recordFailure calls. c still
// gets exactly one EventSkipped, and the reason is worth stating precisely —
// Walk.Skip marks c skipped the first time and excludes an already-skipped
// node from every later Skip() return, so the second w.Skip never hands c to
// this loop at all. tracker's own skipped[id] guard is therefore defensive
// rather than the thing preventing the double; no fixture in this package can
// tell the two apart, because that would need Walk.Skip to hand back a node
// it has already skipped.
func TestRecordFailureNeverEmitsEventSkippedTwiceForASharedDependent(t *testing.T) {
	g := graph.New[planner.OpNode]()
	a, b, c := opnode("a"), opnode("b"), opnode("c")
	for _, n := range []planner.OpNode{a, b, c} {
		g.Add(n)
	}
	g.Edge(a.ID(), c.ID())
	g.Edge(b.ID(), c.ID())

	w, err := g.Walk()
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	w.Ready() // dispatches a and b

	var events []Event
	tr := newTracker(func(e Event) { events = append(events, e) }, nil)
	var ds diag.Diagnostics

	tr.recordFailure(w, a, errors.New("boom a"), &ds)
	tr.recordFailure(w, b, errors.New("boom b"), &ds)

	var skipEvents []Event
	for _, e := range events {
		if e.Kind == EventSkipped {
			skipEvents = append(skipEvents, e)
		}
	}
	if len(skipEvents) != 1 {
		t.Fatalf("EventSkipped for shared dependent c = %+v, want exactly 1", skipEvents)
	}
}

// TestTrackerExcludesAnUpdateNeverReachedByStateEvenThoughItSucceeded pins
// the upper bound on isRemoval: ONLY OpForget, OpDestroy and the destroy
// phase of OpReplace are exempt from the presence-in-state check — not, for
// instance, OpUpdate. An update whose provider call succeeded but whose
// result never reached state (run.record's nil-state hard error) must NOT
// appear in Applied: state genuinely has nothing for it, and an update's goal
// is a resource left present and current.
//
// An over-broad isRemoval would skip the presence check here and report the
// address as applied anyway. The positive destroy and forget cases only prove
// isRemoval is not too NARROW; this is what catches it being too broad.
func TestTrackerExcludesAnUpdateNeverReachedByStateEvenThoughItSucceeded(t *testing.T) {
	a := address.Address{Name: "widget"}
	update := planner.OpNode{Address: a, Kind: planner.OpUpdate, Phase: planner.PhaseCreate}

	g := graph.New[planner.OpNode]()
	g.Add(update)
	w, err := g.Walk()
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	w.Ready()

	tr := newTracker(nil, nil)
	tr.recordSuccess(w, update)

	// st never gets an entry for widget — exactly what run.record's
	// nil-state hard error leaves behind for a real Update call.
	st := state.New("proj", "dev")

	res := tr.result(st)
	if len(res.Applied) != 0 {
		t.Fatalf("Applied = %v, want empty — an update is not a removal, so it must not bypass the presence-in-state check", res.Applied)
	}
}
