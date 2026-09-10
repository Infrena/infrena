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
			providerInFlight[providerName]++
		}
		queue = deferred

		if r.inFlight == 0 {
			if stopping {
				// Nothing running and nothing will be launched: whatever
				// is left in queue, or still blocked deeper in the graph,
				// is simply never attempted. It is deliberately absent
				// from every Result field — Applied and Failed both
				// require an attempt, and Skipped is specifically the set
				// graph.Walk.Skip returns for a failed dependency, not
				// "everything an early stop left behind."
				break
			}
			if len(queue) == 0 {
				// Every remaining node is blocked on something that will
				// never complete. g.Walk() already rejected a cyclic
				// graph above, so this guards a future bug in this
				// loop's own bookkeeping, not a state reachable today.
				ds.Add(diag.Diagnostic{
					Severity: diag.SeverityError,
					Summary:  fmt.Sprintf("executor: %d operation(s) remain but none are ready or running", w.Remaining()),
				})
				break
			}
		}

		res := <-r.results
		r.inFlight--
		if res.providerName != "" {
			providerInFlight[res.providerName]--
		}

		if res.err != nil {
			result.Failed[res.node.ID()] = res.err
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

	defer func() {
		if err != nil {
			r.emit(Event{Kind: EventFailed, Address: node.Address, Op: op.Kind, Err: err, At: r.now()})
			return
		}
		r.emit(Event{Kind: EventSucceeded, Address: node.Address, Op: op.Kind, At: r.now()})
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
		result, err = dispatch(r.ctx, prov, node, current, desired)
	} else {
		err = Attempt(r.ctx, verb, r.opts.Retry, prov.ClassifyError, func() error {
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
