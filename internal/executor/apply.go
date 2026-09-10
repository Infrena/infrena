package executor

import (
	"context"
	"fmt"
	"sort"
	"time"

	"infra/internal/diag"
	"infra/internal/graph"
	"infra/internal/planner"
	"infra/internal/state"
	"infra/pkg/address"
	"infra/pkg/provider"
	"infra/pkg/resource"
)

// Apply drains the execution graph, dispatching each operation's provider
// call through a worker pool bounded twice — globally by opts.Parallelism
// and per provider by opts.PerProvider.
//
// Ownership: this function's own goroutine (the owner) is the only one that
// ever touches w, st, or the scheduling bookkeeping below. Worker
// goroutines receive a read-only snapshot of st and the single OpNode they
// were handed (run.launch), do the provider call (run.execute), and send a
// nodeResult back on a channel; they touch nothing shared. The owner is the
// only reader of that channel, and it is the only place st.Set / st.Remove
// are called (run.record) — always between receiving one nodeResult and
// dispatching the next batch, never while a snapshot in use by a
// still-running worker could be affected, because st.Set/st.Remove only
// ever replace a map entry rather than mutate a *resource.ResourceState in
// place (see run.snapshot).
func Apply(ctx context.Context, p *planner.Plan, g *graph.Graph[planner.OpNode], st *state.State, opts Options) (Result, diag.Diagnostics) {
	var ds diag.Diagnostics
	result := Result{Failed: map[string]error{}, State: st}

	if opts.Parallelism < 1 {
		opts.Parallelism = 1
	}
	if opts.PerProvider < 1 {
		opts.PerProvider = 1
	}

	w, err := g.Walk()
	if err != nil {
		ds.Add(diag.Diagnostic{Severity: diag.SeverityError, Summary: "cannot execute plan: " + err.Error()})
		return result, ds
	}

	r := &run{
		ctx:     ctx,
		st:      st,
		ops:     indexOperations(p),
		opts:    opts,
		results: make(chan nodeResult),
	}

	queue := w.Ready()
	providerInFlight := map[string]int{}
	appliedSet := map[string]address.Address{}
	var skipped []string

	for w.Remaining() > 0 {
		stopping := ctx.Err() != nil

		var deferred []planner.OpNode
		// launched counts how many nodes this pass actually started. It is
		// provably redundant with r.inFlight == 0 below: launch() always
		// increments r.inFlight, and nothing decrements it within this same
		// pass (that only happens after a result is received, later in the
		// loop), so launched > 0 this pass implies r.inFlight > 0 by the
		// time the check below runs, and vice versa. It is kept anyway,
		// spelled out explicitly in the condition rather than relied upon
		// implicitly, because it names the actual intent — "this pass
		// started nothing" — at the cost of one int, which reads better at
		// the call site than re-deriving the same fact from inFlight's
		// value. It is not independently load-bearing; do not read its
		// presence as proof the len(queue) == 0 mistake below could recur
		// through some path r.inFlight == 0 alone would miss.
		launched := 0
		for _, node := range queue {
			if stopping || r.inFlight >= opts.Parallelism {
				deferred = append(deferred, node)
				continue
			}
			providerName := r.providerNameFor(node)
			if providerName != "" && providerInFlight[providerName] >= opts.PerProvider {
				deferred = append(deferred, node)
				continue
			}
			r.launch(node, providerName)
			if providerName != "" {
				// Symmetric with the decrement below, which only ever
				// touches a non-empty name. OpForget's providerName is
				// always "", and unconditionally incrementing
				// providerInFlight[""] here while only ever conditionally
				// decrementing it would grow that bucket without bound
				// over a long run. It is harmless today only because the
				// bounding check above also gates on providerName != "" and
				// so never reads it — but a bucket that grows forever with
				// nothing ever reading it correctly is exactly the kind of
				// pre-existing bookkeeping garbage that turns a future,
				// unrelated change into a hang instead of a clean bound
				// violation.
				providerInFlight[providerName]++
			}
			launched++
		}
		queue = deferred

		if stopping && r.inFlight == 0 {
			// Nothing running and nothing will be launched: whatever is
			// left in queue, or still blocked deeper in the graph, is
			// simply never attempted. It is deliberately absent from every
			// Result field — Applied and Failed both require an attempt,
			// and Skipped is specifically the set graph.Walk.Skip returns
			// for a failed dependency, not "everything an early stop left
			// behind."
			break
		}
		if !stopping && launched == 0 && r.inFlight == 0 {
			// This pass started nothing, and nothing from an earlier pass
			// is still running to ever complete and unblock anything else —
			// so nothing will EVER make further progress, regardless of
			// whether queue itself ended up empty. w.Remaining() > 0 here
			// (the loop's own condition), so every node still outstanding
			// is permanently stuck. g.Walk() already rejected a cyclic
			// graph above, so this guards a future bug in this loop's own
			// bookkeeping, not a state reachable today.
			//
			// Fix note: this used to check len(queue) == 0 instead of
			// launched == 0. That is the wrong condition — it only catches
			// the case where every deferred node happened to get launched
			// (queue ending up empty), and silently falls through to the
			// blocking receive below for the actual hang case: queue
			// non-empty, nothing launched, nothing in flight. That receive
			// then has no sender that will ever arrive, deadlocking
			// mid-apply with the environment's lock still held, instead of
			// reporting the bug — the exact failure mode this diagnostic
			// exists to turn into a clean error.
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  fmt.Sprintf("executor: %d operation(s) remain but none are ready or running", w.Remaining()),
			})
			break
		}

		res := <-r.results
		r.inFlight--
		if res.providerName != "" {
			providerInFlight[res.providerName]--
		}

		if res.err != nil {
			result.Failed[res.node.ID()] = res.err
			// EventSkipped is deliberately not emitted here. Task 10
			// (internal/executor/isolation.go, not yet written) owns the
			// skip cascade end to end — it replaces this inline
			// Failed/Skipped bookkeeping with an extracted tracker
			// (recordSuccess/recordFailure/result) wired into run.record,
			// and emitting EventSkipped belongs with that change, not
			// bolted on here first. This is a known, deliberate gap, not
			// an oversight.
			skipped = append(skipped, w.Skip(res.node.ID())...)
			continue
		}

		r.record(res)
		appliedSet[res.node.Address.String()] = res.node.Address
		queue = append(queue, w.Done(res.node.ID())...)
	}

	applied := make([]address.Address, 0, len(appliedSet))
	for _, a := range appliedSet {
		applied = append(applied, a)
	}
	address.Sort(applied)
	result.Applied = applied

	sort.Strings(skipped)
	result.Skipped = skipped
	return result, ds
}

// run holds the mutable bookkeeping one Apply call owns.
type run struct {
	ctx  context.Context
	st   *state.State
	ops  map[string]*planner.Operation
	opts Options

	results  chan nodeResult
	inFlight int
}

// nodeResult is what a worker goroutine sends back to the owner when one
// operation finishes.
type nodeResult struct {
	node         planner.OpNode
	state        *resource.ResourceState
	removed      bool
	err          error
	providerName string
}

// providerNameFor reports the provider name node's operation will call
// through, or "" for OpForget, which calls no provider and so is bounded
// only by --parallelism, never by a per-provider semaphore protecting rate
// limits it never touches.
func (r *run) providerNameFor(node planner.OpNode) string {
	if node.Kind == planner.OpForget {
		return ""
	}
	op, ok := r.ops[node.Address.String()]
	if !ok {
		return ""
	}
	prov, ok := r.opts.Registry.Provider(op.Type)
	if !ok {
		return ""
	}
	return prov.Name()
}

// launch takes a snapshot of state and starts one worker goroutine for
// node. Called only from the owner goroutine.
//
// Deliberately no recover() here or anywhere in execute's call chain. A
// panic inside a provider call happens AFTER the call was already made — a
// worker that recovered from it still would not know whether the
// underlying infrastructure operation actually landed. Reporting the
// operation as failed when, say, a Create panicked after the object was
// actually created would orphan real infrastructure: nothing in state
// would ever point at it, and a later apply would try to create it a
// second time. Letting an unrecovered panic crash the whole process is
// worse for availability — one bad worker takes down every other operation
// in this run, including ones that would otherwise have finished cleanly —
// but it is never silently wrong about what state should say. This is a
// stated choice, verified in review, not an oversight: do not add a
// recover() here as a tidy-up without also deciding what "failed" should
// mean for an operation whose provider call may have already succeeded.
//
// The more valuable corollary, in production terms: this reasoning is not
// only about panics. ANY worker goroutine that exits without sending a
// nodeResult on r.results — a panic, but just as much a bug that returns
// early, or calls runtime.Goexit some other way — leaves the owner blocked
// forever on the receive in Apply's loop, with the environment's lock still
// held (state.Local.Put's requireOwnLock is the only thing that would ever
// release it, and nothing calls Unlock from inside a stuck Apply). The
// no-progress diagnostic a few lines up in Apply cannot catch this: it
// exists for nothing-launched-and-nothing-running, but here something WAS
// launched and IS, as far as r.inFlight is concerned, still running — the
// bookkeeping is not wrong, the goroutine that was supposed to report back
// is just gone. Measured directly with a worker forced to call t.Fatal
// (apply_test.go's poisonProvider, via TestApplyForgetNeverCallsProvider-
// AndRemovesFromState under a simulated regression): the process does not
// crash and does not report a clean failure — go test's own watchdog is
// what eventually ends it, as "panic: test timed out after 20s". Outside a
// test binary there is no such watchdog; a production apply would hang
// indefinitely holding the lock, and the only way out is an operator
// force-unlocking the environment by hand.
func (r *run) launch(node planner.OpNode, providerName string) {
	r.inFlight++
	snapshot := r.snapshot()
	go func() {
		st, removed, err := r.execute(node, snapshot)
		r.results <- nodeResult{node: node, state: st, removed: removed, err: err, providerName: providerName}
	}()
}

// snapshot copies the resource map's entries, not their contents, into a
// fresh map a worker goroutine can read without racing a later st.Set or
// st.Remove on the owner goroutine. Set and Remove only ever replace a map
// entry — nothing in this codebase mutates a *resource.ResourceState in
// place once it is stored — so a shallow copy is a safe, cheap, read-only
// view of state as of the moment it was taken.
func (r *run) snapshot() map[string]*resource.ResourceState {
	out := make(map[string]*resource.ResourceState, len(r.st.Resources))
	for k, v := range r.st.Resources {
		out[k] = v
	}
	return out
}

// record applies a successful completion to state. Called only from the
// owner goroutine, which is what makes an unsynchronised st.Set / st.Remove
// here safe. Modified by Task 9 to also persist.
func (r *run) record(res nodeResult) {
	if res.removed {
		r.st.Remove(res.node.Address)
		return
	}
	if res.state != nil {
		r.st.Set(res.state)
	}
}

func (r *run) now() time.Time {
	if r.opts.Now != nil {
		return r.opts.Now()
	}
	return time.Now()
}

func (r *run) emit(e Event) {
	if r.opts.OnEvent != nil {
		r.opts.OnEvent(e)
	}
}

// execute resolves an operation's deferred expressions (Task 7) and
// dispatches its provider call (Task 6) through executor.Attempt, which
// retries according to opts.Retry and the classification table in
// retry.go. It runs on a worker goroutine and reads only its own
// arguments — never r.st — which is exactly what the snapshot exists to
// make possible.
func (r *run) execute(node planner.OpNode, snapshot map[string]*resource.ResourceState) (result *resource.ResourceState, removed bool, err error) {
	op, ok := r.ops[node.Address.String()]
	if !ok {
		return nil, false, fmt.Errorf("%s: no operation in the plan for this node", node.Address)
	}

	// attempt tracks how many times a provider call was actually made — the
	// fn passed to Attempt below increments it on every call, and OpForget
	// (which never reaches Attempt at all) sets it to 1 directly. It stays
	// 0 for every early return above a dispatch ever being attempted (no
	// operation found, no provider registered, deferred values would not
	// resolve), which is exactly the "never attempted" meaning types.go
	// documents for Attempt == 0. The deferred emit below reads it only
	// after every write to it has already happened, on this same worker
	// goroutine, so no synchronisation is needed.
	attempt := 0

	defer func() {
		if err != nil {
			r.emit(Event{Kind: EventFailed, Address: node.Address, Op: op.Kind, Attempt: attempt, Err: err, At: r.now()})
			return
		}
		r.emit(Event{Kind: EventSucceeded, Address: node.Address, Op: op.Kind, Attempt: attempt, At: r.now()})
	}()

	current := snapshot[node.Address.String()]

	var prov provider.Provider
	if node.Kind != planner.OpForget {
		p, ok := r.opts.Registry.Provider(op.Type)
		if !ok {
			return nil, false, fmt.Errorf("%s: no provider registered for type %q", node.Address, op.Type)
		}
		prov = p
	}

	var desired *resource.DesiredResource
	if needsDesired(node) {
		after, resolveDS := resolveAfter(op, snapshot, r.opts.Environment)
		if resolveDS.HasErrors() {
			return nil, false, fmt.Errorf("%s: could not resolve deferred values: %s", node.Address, firstErrorSummary(resolveDS))
		}
		lifecycle := resource.Lifecycle{}
		if node.Kind == planner.OpUpdate && current != nil {
			// Operation deliberately carries no Lifecycle: the planner already
			// encodes every lifecycle decision into the operation kind — retain
			// becomes OpForget, prevent_destroy produces no operation at all — so
			// enforcing it again here would be a second enforcement point that can
			// disagree with the first. An
			// update carries forward whatever is already on record.
			lifecycle = current.Lifecycle
		}
		d, desiredErr := (resource.ResolvedResource{Address: node.Address, Type: op.Type, Attrs: after, Lifecycle: lifecycle}).Desired()
		if desiredErr != nil {
			return nil, false, desiredErr
		}
		desired = &d
	}

	r.emit(Event{Kind: EventStarted, Address: node.Address, Op: op.Kind, Attempt: 1, At: r.now()})

	verb, hasVerb := verbFor(node)
	if !hasVerb {
		// OpForget: no provider call, so nothing to retry through Attempt.
		// dispatch itself never calls the provider for OpForget either, but
		// this is still the one "attempt" this operation ever makes.
		attempt = 1
		result, err = dispatch(r.ctx, prov, node, current, desired)
	} else {
		// A local copy of the retry policy, never r.opts.Retry itself:
		// execute runs on a worker goroutine, and r.opts is shared,
		// read-only state across every worker in this run — mutating its
		// OnRetry field in place would race the instant two operations
		// retry at the same time. policy is this call's own value, safe to
		// modify freely; any OnRetry the caller supplied is preserved and
		// still called, just after this run's own EventRetrying.
		policy := r.opts.Retry
		userOnRetry := policy.OnRetry
		policy.OnRetry = func(a int, retryErr error, delay time.Duration) {
			r.emit(Event{Kind: EventRetrying, Address: node.Address, Op: op.Kind, Attempt: a, Err: retryErr, At: r.now()})
			if userOnRetry != nil {
				userOnRetry(a, retryErr, delay)
			}
		}
		err = Attempt(r.ctx, verb, policy, prov.ClassifyError, func() error {
			attempt++
			var derr error
			result, derr = dispatch(r.ctx, prov, node, current, desired)
			return derr
		})
	}
	if err != nil {
		return nil, false, err
	}

	removed = node.Kind == planner.OpForget || node.Kind == planner.OpDestroy ||
		(node.Kind == planner.OpReplace && node.Phase == planner.PhaseDestroy)
	return result, removed, nil
}

// verbFor maps an OpNode to the provider verb its dispatch call will use —
// the same (Kind, Phase) mapping dispatch itself switches on — so Attempt's
// retry classification is decided per provider call, as its own doc
// requires. OpForget has no verb: no call is made, nothing to retry.
func verbFor(node planner.OpNode) (Verb, bool) {
	switch {
	case node.Kind == planner.OpCreate,
		node.Kind == planner.OpReplace && node.Phase == planner.PhaseCreate:
		return VerbCreate, true
	case node.Kind == planner.OpUpdate:
		return VerbUpdate, true
	case node.Kind == planner.OpDestroy,
		node.Kind == planner.OpReplace && node.Phase == planner.PhaseDestroy:
		return VerbDelete, true
	default:
		return VerbInvalid, false
	}
}

func needsDesired(node planner.OpNode) bool {
	return node.Kind == planner.OpCreate || node.Kind == planner.OpUpdate ||
		(node.Kind == planner.OpReplace && node.Phase == planner.PhaseCreate)
}

func indexOperations(p *planner.Plan) map[string]*planner.Operation {
	out := make(map[string]*planner.Operation, len(p.Operations))
	for i := range p.Operations {
		out[p.Operations[i].Address.String()] = &p.Operations[i]
	}
	return out
}

func firstErrorSummary(ds diag.Diagnostics) string {
	for _, d := range ds {
		if d.Severity == diag.SeverityError {
			return d.Summary
		}
	}
	return "unknown error"
}
