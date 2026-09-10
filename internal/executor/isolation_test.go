package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"infra/internal/diag"
	"infra/internal/graph"
	"infra/internal/planner"
	"infra/internal/registry"
	"infra/internal/state"
	"infra/pkg/address"
	"infra/pkg/resource"
	"infra/pkg/value"
	testprovider "infra/providers/test"
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

// TestTrackerSkipsTransitivelyAndLeavesUnrelatedBranchesAlone is a pure, fast
// unit test of the tracker alone — no provider, no coordinator loop. The
// chain a -> b -> c is three nodes, not two: a two-node fixture cannot tell
// "b was skipped because Skip is transitive" apart from "b was skipped
// because it is a's one direct dependent" — c is what forces the
// implementation to actually use Skip's transitive return rather than only
// handling the immediate dependent. d shares no edge with anything, and
// proves a failure in one branch does not stop an unrelated one.
//
// A naive non-transitive implementation fails this test concretely: b would
// be Skipped but c would be neither Applied, Failed nor Skipped, so
// Remaining() would never reach zero — caught below even if the Skipped
// slice assertion were somehow satisfied by accident.
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

	tr := newTracker()
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
// wired into Apply's real coordinator loop (Group B, tasks 7–8), not merely
// that tracker is correct sitting on its own. a's create is configured to
// fail; b depends on a; c is unrelated.
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
	if err := reg.Register(testprovider.New(cloudPath)); err != nil {
		t.Fatalf("Register: %v", err)
	}

	p := &planner.Plan{
		Version: planner.PlanVersion,
		Operations: []planner.Operation{
			{Address: address.Address{Name: "a"}, Type: "test.network", Kind: planner.OpCreate,
				After: map[string]value.Value{"cidr": value.String("10.0.0.0/24", value.SourceExplicit)}},
			{Address: address.Address{Name: "b"}, Type: "test.network", Kind: planner.OpCreate,
				After: map[string]value.Value{"cidr": value.String("10.0.1.0/24", value.SourceExplicit)}},
			{Address: address.Address{Name: "c"}, Type: "test.network", Kind: planner.OpCreate,
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
// call recordSuccess — Applied must report that address exactly once, not
// twice, and it must land in Applied because the CREATE phase (the one
// that leaves the resource present) is what the state check below sees, not
// the destroy phase's now-stale "removal" judgement.
//
// A naive slice-based tracker.applied (append, never dedup) fails the count
// assertion outright. A map keyed on address that OVERWRITES on every
// recordSuccess also gets the count right but, if it were the FIRST write
// that survived instead of the LAST, would still pass this specific
// assertion by accident, because destroy's "removal: true" entry always
// skips the presence-in-state check regardless of whether state actually
// has the address — see the sibling test below, which is what actually
// forces last-write-wins over first-write-wins.
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

	tr := newTracker()
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
// that actually forces last-write-wins (favor the create phase) over
// first-write-wins (favor the destroy phase): it reproduces a replace whose
// destroy phase genuinely succeeded — st no longer has the address, exactly
// as run.record's removed branch leaves it — but whose create phase hit the
// nil-state hard error (run.record's default arm), so state was NEVER
// re-set. recordSuccess is still called for the create phase regardless
// (Apply's wiring calls it unconditionally, whatever run.record returned —
// see apply.go), so the tracker sees both phases complete either way.
//
// The correct answer is that swap is ABSENT from Applied: the create phase
// is what recordSuccess's own dedup keeps (removal: false), and state has
// no entry for it, so tracker.result's presence check must exclude it. A
// first-write-wins tracker would instead keep the destroy phase's entry
// (removal: true), which skips the presence check unconditionally and
// reports swap as applied even though the replace never actually finished —
// exactly the false success this task's ruling exists to prevent.
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

	tr := newTracker()
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
