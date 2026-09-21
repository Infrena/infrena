package executor

import (
	"fmt"
	"sort"
	"time"

	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/internal/graph"
	"github.com/infrena/infrena/internal/planner"
	"github.com/infrena/infrena/internal/state"
	"github.com/infrena/infrena/pkg/address"
)

// trackedApply is one completed (or attempted) operation as tracker
// remembers it, kept only long enough for tracker.result to decide whether
// it belongs in Result.Applied.
type trackedApply struct {
	addr address.Address
	// removal is true for the shapes where success means the address is
	// gone rather than present. tracker.result needs it to tell a removal's
	// expected absence from state apart from the unexpected absence that
	// means a provider reported success with nothing to record.
	removal bool
	// forget distinguishes the one removal that is not a deletion: it drops
	// a resource from management and leaves it standing at the provider, so
	// the summary must not render it as destroyed.
	forget bool
}

// tracker accumulates what happened to every operation the coordinating
// goroutine has processed. Like the Walk it advances, it is not safe for
// concurrent use and is only ever touched from that one goroutine.
type tracker struct {
	// applied is keyed by address rather than appended to, because a replace
	// is two nodes at one address and both call recordSuccess. Keying
	// collapses them into the one entry per address Result.Applied promises.
	//
	// The later write wins, and a completed replace must leave the address
	// recorded as present. That cannot rest on phase order, which
	// create_before_destroy reverses; it rests on isRemoval, under which a
	// create_before_destroy destroy phase is not a removal of the address at
	// all, so whichever phase lands last says present.
	applied map[string]trackedApply
	failed  map[string]error
	skipped map[string]bool

	// emit and now are how recordFailure reports EventSkipped, using the
	// run's own event sink and clock rather than a bare time.Now, so an
	// injected clock governs skip timestamps too. Either may be nil, which
	// gives diagnostics-only behaviour; Apply always passes both.
	emit func(Event)
	now  func() time.Time
}

// newTracker returns a tracker reporting skips through emit and now.
func newTracker(emit func(Event), now func() time.Time) *tracker {
	return &tracker{applied: map[string]trackedApply{}, failed: map[string]error{}, skipped: map[string]bool{}, emit: emit, now: now}
}

// isRemoval reports whether node's completion means the address is gone
// rather than present — see trackedApply.removal.
func isRemoval(node planner.OpNode) bool {
	if node.Kind == planner.OpReplace && node.Phase == planner.PhaseDestroy {
		// A create_before_destroy replacement's destroy phase removes the
		// deposed object, not the address: the new one is already in state.
		// Saying otherwise would delete a live resource's record, and since
		// this phase runs last under that flag, last-write-wins would report
		// a completed replacement as a removal.
		return !node.CreateBeforeDestroy
	}
	return node.Kind == planner.OpForget || node.Kind == planner.OpDestroy
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

// recordFailure stops a failure's branch without stopping the run. It marks
// node failed, asks the Walk which dependents that transitively strands, and
// marks each of those skipped, emitting one diagnostic per failure and one
// per skip. Diagnostics are the one place such explanations live, so the
// package needs no second channel for them.
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
// Only the persistence path needs it, where the failure is not a provider
// call going wrong but state failing to record one that went right. Letting
// it add its own diagnostic and call recordFailure would emit two errors for
// one event, the second contradicting the first. The bookkeeping is
// identical, so only the sentence is a parameter.
func (t *tracker) recordFailureWith(w *graph.Walk[planner.OpNode], node planner.OpNode, err error, ds *diag.Diagnostics, d diag.Diagnostic) {
	t.failed[node.ID()] = err
	// A prior success at this address can only be the destroy phase of the
	// same replace whose create phase just failed. A replace's unit of
	// success is the whole replacement, so one that deleted the old resource
	// and failed to create the new must not appear in Applied as well as
	// Failed, with no state entry either. For every other failure shape this
	// is a no-op.
	//
	// Safe only because "at most one node per address, except a replace's
	// two phases" is enforced: BuildExecution rejects a plan with two
	// operations at one address before a graph ever exists. Without that, a
	// corrupted plan could make this delete wipe an unrelated applied entry.
	delete(t.applied, node.Address.String())
	ds.Add(d)

	// Both halves of this guard are defensive. Walk.Skip already excludes
	// the failed id itself from what it returns, and returns any given node
	// at most once per Walk. If either guarantee changes, its half here
	// becomes load-bearing.
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
			// Attempt is 0 because a skipped operation is never attempted,
			// and Err is unset because the skip is not itself a failure —
			// the failure that caused it has its own EventFailed.
			t.emit(Event{Kind: EventSkipped, Address: n.Address, Op: n.Kind, Attempt: 0, At: at})
		}
	}
}

// result assembles the run's Result. Sorting happens here rather than as
// operations complete, which is what keeps the output independent of
// completion order under concurrency.
//
// An address reaches Applied only if state has an entry for it, unless the
// operation was a removal, whose whole point is that the address is now
// absent. For every other kind a missing entry means a provider reported
// success with no resource state to record; promoting it into Applied would
// make the summary contradict the state it ships with. A failed write to
// disk does not reach this path, because the in-memory change happened
// before the write was attempted.
//
// st may be nil, in which case nothing is filtered: there is nothing to
// check until a real state is in hand.
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
	// No dedup needed: applied is built from map values, so it already holds
	// at most one entry per address.

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
