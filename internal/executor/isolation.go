package executor

import (
	"fmt"
	"sort"

	"infra/internal/diag"
	"infra/internal/graph"
	"infra/internal/planner"
	"infra/internal/state"
	"infra/pkg/address"
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
	// already makes for nodeResult.removed — there is no shared accessor to
	// call instead because, unlike Walk.Skip's transitivity, this is a
	// plain field mapping, not a rule this package has ever gotten wrong by
	// duplicating.
	removal bool
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
}

func newTracker() *tracker {
	return &tracker{applied: map[string]trackedApply{}, failed: map[string]error{}, skipped: map[string]bool{}}
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
	t.applied[node.Address.String()] = trackedApply{addr: node.Address, removal: isRemoval(node)}
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
// walker never lost.
func (t *tracker) recordFailure(w *graph.Walk[planner.OpNode], node planner.OpNode, err error, ds *diag.Diagnostics) {
	t.failed[node.ID()] = err
	ds.Add(diag.Diagnostic{
		Severity: diag.SeverityError,
		Summary:  fmt.Sprintf("%s %s failed", node.Kind, node.Address),
		Detail:   err.Error(),
		Related:  []address.Address{node.Address},
	})

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
	for _, a := range t.applied {
		if !a.removal && st != nil {
			if _, ok := st.Get(a.addr); !ok {
				continue
			}
		}
		applied = append(applied, a.addr)
	}
	address.Sort(applied)
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
		Applied: applied,
		Failed:  t.failed,
		Skipped: skipped,
		State:   st,
	}
}
