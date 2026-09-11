package executor

import (
	"fmt"
	"sort"
	"time"

	"github.com/infrata/infrata/internal/diag"
	"github.com/infrata/infrata/internal/graph"
	"github.com/infrata/infrata/internal/planner"
	"github.com/infrata/infrata/internal/state"
	"github.com/infrata/infrata/pkg/address"
)

// trackedApply is one completed (or attempted) operation as tracker
// remembers it, kept only long enough for tracker.result to decide whether
// it belongs in Result.Applied.
type trackedApply struct {
	addr address.Address
	// removal is true for OpDestroy, OpForget, and the destroy phase of
	// OpReplace — the shapes where "applied" means the address is gone, not
	// present. tracker.result needs this alongside the address because it
	// is the one thing that tells apart a removal's expected absence from
	// state from the nil-state contract violation's UNEXPECTED absence (see
	// result's doc comment). node only carries Kind/Phase, not this
	// judgement, so it is computed once here, at recordSuccess time,
	// mirroring the identical (Kind, Phase) -> removed mapping run.execute
	// already makes for nodeResult.removed — through the same isRemoval
	// call, which run.execute now uses too rather than re-writing its body
	// inline.
	removal bool
	// forget distinguishes the one removal that is NOT a deletion. A forget
	// drops a resource from management and deliberately leaves it standing
	// at the provider (spec §11's retain), so the summary must not mark it
	// with a destroy's "-": telling a user their retained resource was
	// deleted is the exact opposite of what happened, and it is what
	// `retain` exists to prevent. The plan renderer already distinguishes
	// them ("=" versus "-"); the apply summary could not, because Result
	// carries addresses and state, and a forgotten address is absent from
	// state for the same reason a destroyed one is.
	forget bool
}

// tracker accumulates what happened to every operation the coordinating
// goroutine has processed, in a form Apply's loop can update one event at a
// time. It is not safe for concurrent use, which is fine: Walk itself is
// "not safe for concurrent use; the executor owns it from one goroutine and
// hands work out" (contract), and tracker is only ever touched from that
// same goroutine, never from a worker.
type tracker struct {
	// applied is keyed by address string, not appended to as a slice — an
	// OpReplace is two OpNodes at ONE address (destroy phase, then create
	// phase), and both call recordSuccess. Keying on address, exactly like
	// the appliedSet map this tracker replaced, is what collapses those two
	// completions into Result.Applied's single documented entry per
	// address; a plain slice would report a replace twice. The later write
	// wins, which for a replace means the create phase's entry (removal:
	// false) is what survives — correct, because a completed replace
	// leaves the address PRESENT in state, not absent.
	applied map[string]trackedApply
	failed  map[string]error
	skipped map[string]bool

	// emit and now are how recordFailure reports EventSkipped — the same
	// r.emit/r.now Apply's other Event sites already use (execute, in
	// apply.go), passed in rather than reached for globally so tracker
	// stays testable without a *run. Either may be nil: a pure tracker
	// test that has no opinion about events can call newTracker(nil, nil)
	// and get diagnostics-only behaviour, same as before EventSkipped
	// existed. Apply itself always passes real, non-nil closures.
	emit func(Event)
	now  func() time.Time
}

// newTracker's emit and now let recordFailure report EventSkipped with the
// run's own event sink and clock (Options.OnEvent / Options.Now) — never a
// bare time.Now(), so a test that injects a fixed clock sees deterministic
// timestamps on skip events exactly as it does on every other Event kind.
func newTracker(emit func(Event), now func() time.Time) *tracker {
	return &tracker{applied: map[string]trackedApply{}, failed: map[string]error{}, skipped: map[string]bool{}, emit: emit, now: now}
}

// isRemoval reports whether node's completion means the address is gone
// rather than present — see trackedApply.removal.
func isRemoval(node planner.OpNode) bool {
	return node.Kind == planner.OpForget || node.Kind == planner.OpDestroy ||
		(node.Kind == planner.OpReplace && node.Phase == planner.PhaseDestroy)
}

// recordSuccess records that node completed and advances the walk exactly
// as a direct call to Walk.Done would — call this INSTEAD of Done, never
// alongside it, or a completed node gets marked done twice.
func (t *tracker) recordSuccess(w *graph.Walk[planner.OpNode], node planner.OpNode) []planner.OpNode {
	t.applied[node.Address.String()] = trackedApply{
		addr:    node.Address,
		removal: isRemoval(node),
		forget:  node.Kind == planner.OpForget,
	}
	return w.Done(node.ID())
}

// recordFailure is the failure path spec §15 describes: "a failure stops
// its branch, not the world." It marks node itself failed, asks Walk which
// dependents that transitively strands, and marks each of those skipped —
// emitting one diagnostic per failure and one per skip, which is how the
// CLI (§16: diagnostics on stderr) reports "what failed and why" and "what
// was skipped and because of what" without this package needing a second,
// duplicate channel for that text: diag.Diagnostics is already the one
// place explanations like this live.
//
// Walk.Skip now returns the skipped nodes themselves, not their ids
// (internal/graph/walk.go) — precisely so this loop can read a skipped
// node's own Address and Kind straight off the value Walk already held,
// instead of rebuilding an id -> node map purely to recover information the
// walker never lost. That is also what lets this loop emit EventSkipped
// itself, once per node, right where the diagnostic for it is already
// built — the only other place with this information is Walk, which knows
// nothing about Event or Options.OnEvent and should not.
func (t *tracker) recordFailure(w *graph.Walk[planner.OpNode], node planner.OpNode, err error, ds *diag.Diagnostics) {
	t.recordFailureWith(w, node, err, ds, diag.Diagnostic{
		Severity: diag.SeverityError,
		Summary:  fmt.Sprintf("%s %s failed", node.Kind, node.Address),
		Detail:   err.Error(),
		Related:  []address.Address{node.Address},
	})
}

// recordFailureWith is recordFailure with the caller supplying the
// diagnostic that explains the failure.
//
// Only one caller needs it: Apply's persistence path, whose failure is not a
// provider call going wrong but state failing to record one that went right,
// and which has already composed a diagnostic saying so in those terms. The
// alternative — letting it add its own diagnostic and calling recordFailure
// — emits two error diagnostics for one event, the second of which
// ("create x failed") contradicts the first ("the provider call reported
// success"). The bookkeeping is identical either way; only the sentence
// differs, so only the sentence is a parameter.
func (t *tracker) recordFailureWith(w *graph.Walk[planner.OpNode], node planner.OpNode, err error, ds *diag.Diagnostics, d diag.Diagnostic) {
	t.failed[node.ID()] = err
	// A prior recordSuccess at this same address, if one exists, can only
	// be the destroy phase of the SAME OpReplace that node's create phase
	// just failed: every other kind puts exactly one node per address in
	// the graph, so recordSuccess and recordFailure can never otherwise
	// land on the same address. Removing it here is the ruling from Task
	// 10's review round 1 (C2): a replace's unit of success is the
	// replacement, not its destroy half alone, so a replace that deleted
	// the old resource and failed to create the new one must not appear in
	// Applied — reporting it there while it is ALSO in Failed, with no
	// entry in state either, is exactly the self-contradictory summary the
	// Applied/State consistency rule exists to prevent. This delete is a
	// no-op for every other failure shape, where no entry exists yet.
	//
	// Safe only because "at most one node per address, except a replace's
	// two phases" is an ENFORCED invariant, not just true by convention:
	// planner.BuildExecution (internal/planner/execution.go) rejects a plan
	// with two operations at the same address before a graph — and this
	// tracker — ever sees it. Review round 2 found that without that check,
	// a hand-built or corrupted plan with two DIFFERENT-kind operations at
	// one address could reach here and have this delete wipe a
	// legitimately-applied, unrelated entry.
	delete(t.applied, node.Address.String())
	ds.Add(d)

	// Both halves of this guard are defensive depth, not what currently
	// makes it true, and both trace back to the same two facts documented
	// once on Walk's own type doc (internal/graph/walk.go) rather than
	// re-derived here: id == node.ID() can never hold because Skip's "id
	// itself is never in the returned slice" excludes it before this loop
	// ever runs (pinned by TestSkipPropagatesTransitivelyAndSortsByID); and
	// t.skipped[id] can never already be true because Skip returns any
	// given node at most once per Walk, ever (pinned by
	// TestSkipOnASharedDependentIsNotDoubleCounted, internal/graph/
	// walk_test.go). If either guarantee ever changes, its half of this
	// guard becomes load-bearing, and there is no test today that could
	// have caught it silently stopping being true, because there is
	// nothing beneath it to break yet.
	for _, n := range w.Skip(node.ID()) {
		id := n.ID()
		if id == node.ID() || t.skipped[id] {
			continue
		}
		t.skipped[id] = true
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityWarning,
			Summary:  fmt.Sprintf("%s skipped", id),
			Detail:   fmt.Sprintf("skipped because %s %s failed", node.Kind, node.Address),
			Related:  []address.Address{n.Address},
		})
		if t.emit != nil {
			at := time.Time{}
			if t.now != nil {
				at = t.now()
			}
			// Attempt: 0 — types.go documents this explicitly: a skipped
			// operation is never attempted. Err is deliberately left unset:
			// the skip is not itself a failure, and the failure that caused
			// it already has its own EventFailed, emitted from execute.
			t.emit(Event{Kind: EventSkipped, Address: n.Address, Op: n.Kind, Attempt: 0, At: at})
		}
	}
}

// result assembles the Result the package contract promises. Sorting here,
// not as operations complete, is what keeps output independent of
// completion order under concurrency (spec §15: "determinism").
//
// An address only makes it into Applied if st actually has an entry for it,
// unless the operation was a removal — Applied is documented as "created,
// updated or destroyed", and a removal's whole point is that the address is
// now ABSENT from state, so its absence is the expected, correct outcome,
// not a red flag. For every other kind, an address missing from st means
// run.record's nil-state hard error fired (a provider returned success with
// no resource state to record) — the operation is not silently promoted
// into Applied for that, because st, which Result.State also points at,
// would then contradict Result.Applied: an address the summary reports as
// applied that state has no record of. A Put I/O failure does not hit this
// path at all: st.Set/st.Remove already ran before Put was attempted
// (run.record), so st still has the entry even though the write to disk
// failed.
//
// st is allowed to be nil — tracker is also exercised as a pure unit
// against a hand-built Walk with no state involved at all — in which case
// nothing is filtered; the invariant this guards only has something to
// check once a real *state.State is in hand.
func (t *tracker) result(st *state.State) Result {
	applied := make([]address.Address, 0, len(t.applied))
	var forgotten []address.Address
	for _, a := range t.applied {
		if !a.removal && st != nil {
			if _, ok := st.Get(a.addr); !ok {
				continue
			}
		}
		applied = append(applied, a.addr)
		if a.forget {
			forgotten = append(forgotten, a.addr)
		}
	}
	address.Sort(applied)
	address.Sort(forgotten)
	// address.Sort alone does not dedup: applied is built from map values
	// above, so it already has at most one entry per address — nothing left
	// to collapse here. The dedup lives in recordSuccess's map write, not
	// in this sort.

	skipped := make([]string, 0, len(t.skipped))
	for id := range t.skipped {
		skipped = append(skipped, id)
	}
	sort.Strings(skipped)

	return Result{
		Applied:   applied,
		Forgotten: forgotten,
		Failed:    t.failed,
		Skipped:   skipped,
		State:     st,
	}
}
