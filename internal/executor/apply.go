package executor

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/internal/graph"
	"github.com/infrena/infrena/internal/planner"
	"github.com/infrena/infrena/internal/retry"
	"github.com/infrena/infrena/internal/state"
	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/provider"
	"github.com/infrena/infrena/pkg/resource"
	"github.com/infrena/infrena/pkg/value"
)

// Apply drains the execution graph, dispatching each operation's provider
// call through a worker pool bounded twice — globally by opts.Parallelism
// and per provider by opts.PerProvider.
//
// Ownership: this goroutine is the only one that touches the walk, the
// state, or the scheduling bookkeeping. Workers receive a read-only snapshot
// and one node, make the provider call, and send a result back on a channel;
// they share nothing else. State is written only here, between receiving one
// result and dispatching the next batch, and only ever by replacing a map
// entry rather than mutating a stored resource in place.
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
	tr := newTracker(r.emit, r.now)
	persistFailed := false

	for w.Remaining() > 0 {
		stopping := ctx.Err() != nil || persistFailed

		var deferred []planner.OpNode
		// launched is redundant with r.inFlight below, and kept because it
		// names the intent — "this pass started nothing" — at the call site.
		launched := 0
		for _, node := range queue {
			if stopping || r.inFlight >= opts.Parallelism {
				deferred = append(deferred, node)
				continue
			}
			providerName := r.providerNameFor(node)
			if providerName != "" && providerInFlight[providerName] >= providerCeiling(opts, providerName) {
				deferred = append(deferred, node)
				continue
			}
			r.launch(node, providerName)
			if providerName != "" {
				// Symmetric with the decrement below, which also only
				// touches a non-empty name. A forget has no provider, so
				// incrementing unconditionally would grow the "" bucket
				// without bound — harmless only for as long as nothing
				// reads it.
				providerInFlight[providerName]++
			}
			launched++
		}
		queue = deferred

		if stopping && r.inFlight == 0 {
			// Nothing running and nothing more will start, so whatever is
			// left is never attempted. It appears in no Result field:
			// Applied and Failed both require an attempt, and Skipped means
			// specifically "stranded by a failed dependency", not
			// "abandoned by an early stop".
			break
		}
		if !stopping && launched == 0 && r.inFlight == 0 {
			// This pass started nothing and nothing earlier is still
			// running to unblock anything, yet operations remain: nothing
			// will ever make progress. The condition is launched == 0, not
			// an empty queue — a non-empty queue with nothing launched and
			// nothing in flight is exactly the hang case, and falling
			// through to the receive below would deadlock mid-apply with
			// the environment's lock still held. A cyclic graph was already
			// rejected above, so this guards a bug in this loop's own
			// bookkeeping rather than anything reachable today.
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
			tr.recordFailure(w, res.node, res.err, &ds)
			continue
		}

		if err := r.record(res); err != nil {
			d := diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "failed to persist state after " + res.node.ID() + ": " + err.Error(),
				// Deliberately does not claim Result.State reflects the
				// operation: true after a failed write, false when the
				// provider returned no state to record at all.
				Detail: res.node.Address.String() + "'s provider call reported success, but state could not be " +
					"kept in sync with it. The run is stopping rather than scheduling further operations " +
					"against a state file that may no longer describe reality.",
				Related: []address.Address{res.node.Address},
			}
			persistFailed = true

			// The two errors record can return are not the same outcome,
			// and Result must not flatten them into one.
			//
			// A failed write already applied the in-memory change, so state
			// holds the entry and recording success below is accurate; only
			// the flush to disk did not happen.
			//
			// A provider that returned no state never reached the in-memory
			// write, so there is nothing to record as applied. Sent to
			// recordSuccess it would be filtered back out for having no
			// state entry, landing in neither Applied, Failed nor Skipped —
			// a run that may have created real infrastructure reporting
			// that it did nothing at all. It is a failure, and its
			// dependents are stranded.
			if _, ok := errors.AsType[*noResourceStateError](err); ok {
				tr.recordFailureWith(w, res.node, err, &ds, d)
				continue
			}
			ds.Add(d)
		}
		newly := tr.recordSuccess(w, res.node)
		queue = append(queue, newly...)
	}

	return tr.result(st), ds
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
// through, or "" for a forget, which calls no provider and so is bounded
// only by --parallelism.
func (r *run) providerNameFor(node planner.OpNode) string {
	if node.Kind == planner.OpForget {
		return ""
	}
	op, ok := r.ops[node.Address.String()]
	if !ok {
		return ""
	}
	prov, ok := r.opts.Registry.ProviderFor(op.Type, op.Provider)
	if !ok {
		return ""
	}
	return prov.Name()
}

// launch takes a snapshot of state and starts one worker goroutine for
// node. Called only from the owner goroutine.
//
// There is deliberately no recover() here or anywhere in execute's call
// chain. A panic inside a provider call happens after the call was made, so
// a worker that recovered still would not know whether the infrastructure
// operation landed. Reporting failure for a create that panicked after the
// object existed would orphan it: nothing in state points at it, and a later
// apply creates a second one. Crashing the process is worse for
// availability but never silently wrong about what state should say. Do not
// add a recover() as a tidy-up without first deciding what "failed" means
// for an operation whose provider call may have succeeded.
//
// The corollary matters more than panics do: a worker that exits without
// sending on r.results at all — an early return, a runtime.Goexit, a t.Fatal
// in a test double — leaves the owner blocked forever on the receive in
// Apply's loop with the environment's lock still held. Apply's no-progress
// diagnostic cannot catch it, because as far as the bookkeeping knows
// something is still running. Every path through a worker must send exactly
// one result.
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
	maps.Copy(out, r.st.Resources)
	return out
}

// record applies a successful completion to state and persists it
// immediately rather than batching until the run ends, so a crash leaves
// state that accurately describes reality. Called only from the owner
// goroutine, which is what makes the unsynchronised writes safe and what
// serialises successive persists without an extra lock.
//
// A failed write does not undo the in-memory change: what it recorded is
// true, and rolling it back to match a stale file would make Result.State
// lie about reality. State on disk can fall behind by up to Parallelism
// writes, since every operation still in flight attempts its own. That is
// self-correcting: each write serialises the whole document, so the next
// successful one resynchronises everything at once. Apply's loop is what
// decides the run stops after this.
func (r *run) record(res nodeResult) error {
	switch {
	case res.removed:
		r.st.Remove(res.node.Address)
	case res.node.Kind == planner.OpDestroyDeposed,
		res.node.Kind == planner.OpReplace && res.node.Phase == planner.PhaseDestroy && res.node.CreateBeforeDestroy:
		// The deposed object is gone for real, so drop the record of it.
		// The address itself stays: the new resource is there, recorded by
		// the create phase.
		//
		// If this never runs — the delete failed, or the process died
		// between the two phases — the deposed entry survives in state,
		// which is why it is written to disk rather than held in memory. A
		// real object nothing can name is a leak that bills monthly; one
		// state still names is a line in the next plan.
		if cur := r.st.Resources[res.node.Address.String()]; cur != nil {
			next := cur.Clone()
			next.Deposed = nil
			r.st.Set(next)
		}
	case res.state != nil:
		if res.node.Kind == planner.OpReplace && res.node.Phase == planner.PhaseCreate && res.node.CreateBeforeDestroy {
			// Before the new record replaces the old one, because replacing
			// it loses the old object's ProviderID — the only handle
			// anything has on a resource that is still running.
			if prior := r.st.Resources[res.node.Address.String()]; prior != nil {
				deposed := prior.Clone()
				deposed.Deposed = nil
				res.state.Deposed = append(res.state.Deposed, deposed)
			}
		}
		r.st.Set(res.state)
	default:
		// A provider whose Create or Update returned no state and no error.
		// That is a contract violation, not a valid outcome: doing nothing
		// here would report the operation applied with state holding no
		// record of it, and if the call did take effect the resource is
		// orphaned — created for real, tracked nowhere, invisible to a
		// later plan or destroy.
		//
		// The whole run stops, rather than just this operation failing.
		// An ordinary provider failure leaves state accurate, so continuing
		// elsewhere is safe; this leaves this address's accuracy genuinely
		// unknown, the same uncertainty a failed write produces, so it takes
		// the same stopping gate.
		return &noResourceStateError{msg: fmt.Sprintf("%s: provider %q returned success for %s with no resource state — state cannot record what happened, and if the provider call actually took effect the underlying infrastructure is now orphaned", res.node.Address, r.providerNameFor(res.node), res.node.Kind)}
	}
	// WithoutCancel, not r.ctx: a state write must not be cancelled by the
	// same signal that told Apply to stop, since the point of stopping is to
	// record what already happened. It keeps ctx's values — the lock reads
	// one — and strips only cancellation, unlike context.Background. The
	// local backend ignores ctx entirely, but a remote one must still be
	// able to finish this write after Apply's ctx is cancelled, and should
	// derive any timeout from this context rather than from r.ctx.
	return r.opts.Backend.Put(context.WithoutCancel(r.ctx), r.opts.Environment, r.st)
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

// execute resolves an operation's deferred expressions and dispatches its
// provider call through the retry loop. It runs on a worker goroutine and
// reads only its own arguments, never r.st, which is what the snapshot
// exists to make possible.
func (r *run) execute(node planner.OpNode, snapshot map[string]*resource.ResourceState) (result *resource.ResourceState, removed bool, err error) {
	op, ok := r.ops[node.Address.String()]
	if !ok {
		return nil, false, fmt.Errorf("%s: no operation in the plan for this node", node.Address)
	}

	// attempt counts provider calls actually made. It stays 0 for every
	// early return before a dispatch is attempted, which is the "never
	// attempted" meaning Event.Attempt documents. The deferred emit reads it
	// after every write, on this same goroutine, so it needs no
	// synchronisation.
	attempt := 0

	defer func() {
		if err != nil {
			r.emit(Event{Kind: EventFailed, Address: node.Address, Op: op.Kind, Attempt: attempt, Err: err, At: r.now()})
			return
		}
		r.emit(Event{Kind: EventSucceeded, Address: node.Address, Op: op.Kind, Attempt: attempt, At: r.now()})
	}()

	// currentFor, not the snapshot alone: the snapshot is the last state
	// persisted, and a provider needs what is out there now.
	current := currentFor(node.Address, snapshot, r.opts.Observed)

	// A create_before_destroy replacement's destroy phase deletes the
	// deposed object the create phase set aside, not the record at this
	// address, which by now describes the new resource. Handing over the
	// current record would delete what was just built and leave the old one
	// running — and silently, since both calls succeed.
	if node.Kind == planner.OpDestroyDeposed ||
		(node.Kind == planner.OpReplace && node.Phase == planner.PhaseDestroy && node.CreateBeforeDestroy) {
		d := deposedOf(snapshot[node.Address.String()])
		if d == nil {
			return nil, false, fmt.Errorf("%s: create_before_destroy: no deposed object to remove — the create phase either did not run or did not deposit the old one", node.Address)
		}
		current = d
	}

	var prov provider.Provider
	if node.Kind != planner.OpForget {
		p, ok := r.opts.Registry.ProviderFor(op.Type, op.Provider)
		if !ok {
			return nil, false, fmt.Errorf("%s: no provider instance %q offers type %q; declared instances: %s",
				node.Address, op.Provider, op.Type, strings.Join(r.opts.Registry.InstanceNames(), ", "))
		}
		prov = p
	}

	var desired *resource.DesiredResource
	if needsDesired(node) {
		after, resolveDS := resolveAfter(op, snapshot, r.opts.Environment)
		if resolveDS.HasErrors() {
			return nil, false, fmt.Errorf("%s: could not resolve deferred values: %s", node.Address, firstErrorSummary(resolveDS))
		}
		// op.Lifecycle is carried as data, not acted on: nothing in this
		// package branches on it, so the planner stays the single
		// enforcement point. A second check here could disagree with that
		// one, which is how a resource gets destroyed despite a guard.
		//
		// Recording it is not optional, though. A destroy reads lifecycle
		// from state, the resource having left configuration by then, so a
		// lifecycle that never reaches state is a guard that silently does
		// nothing: prevent_destroy would apply cleanly and then destroy
		// without complaint, and retain would delete what it was written to
		// preserve.
		d, desiredErr := (resource.ResolvedResource{Address: node.Address, Type: op.Type, Attrs: after, Lifecycle: op.Lifecycle}).Desired()
		if desiredErr != nil {
			return nil, false, desiredErr
		}
		desired = &d
	}

	r.emit(Event{Kind: EventStarted, Address: node.Address, Op: op.Kind, Attempt: 1, At: r.now()})

	verb, hasVerb := verbFor(node)
	if !hasVerb {
		// A forget makes no provider call, so there is nothing to retry —
		// but this is still the one attempt the operation ever makes.
		attempt = 1
		result, err = dispatch(r.ctx, prov, node, current, desired)
		stampInstance(result, op.Provider)
	} else {
		// A local copy of the retry policy, never r.opts.Retry itself:
		// r.opts is shared read-only across every worker, so mutating its
		// OnRetry in place would race the moment two operations retry at
		// once. Any OnRetry the caller supplied is still called, after this
		// run's own EventRetrying.
		policy := r.opts.Retry
		userOnRetry := policy.OnRetry
		policy.OnRetry = func(a int, retryErr error, delay time.Duration) {
			r.emit(Event{Kind: EventRetrying, Address: node.Address, Op: op.Kind, Attempt: a, Err: retryErr, At: r.now()})
			if userOnRetry != nil {
				userOnRetry(a, retryErr, delay)
			}
		}
		err = retry.Attempt(r.ctx, verb, policy, prov.ClassifyError, func() error {
			attempt++
			var derr error
			result, derr = dispatch(r.ctx, prov, node, current, desired)
			stampInstance(result, op.Provider)
			return derr
		})
	}
	if err != nil {
		return nil, false, err
	}

	// What the engine knew, restamped onto what the provider returned.
	//
	// A provider round trip is lossy in one direction: it reports the
	// attributes it manages and knows nothing of the metadata the engine
	// attached on the way in, so recording its result verbatim drops all of
	// it. Three things must survive:
	//
	//   - Lifecycle, or a later destroy — which reads it from state, the
	//     resource having left configuration by then — finds no guard.
	//   - Dependencies, which are configuration's business and no
	//     provider's, and are the only source of destroy ordering once a
	//     resource leaves configuration.
	//   - Sensitivity that arrived by propagation, which goes through a
	//     provider as a bare string and would come back unmarked, so the
	//     apply summary would print the secret in clear. Schema-declared
	//     sensitivity survives on its own, since providers re-derive it.
	//
	// None of this is enforcement or redaction: lifecycle is copied and
	// never branched on, and formatting remains the only place anything is
	// redacted. What breaks without it is the flag reaching that formatter.
	if desired != nil && result != nil {
		result.Lifecycle = desired.Lifecycle
		result.Dependencies = append([]address.Address(nil), op.DependsOn...)
		result.Attributes = value.CarrySensitivityAttrs(result.Attributes, desired.Attrs)
	}
	// Deposed is the host's too, and an update does not change what a failed
	// replacement left behind. Without this an in-process provider that
	// returns a fresh state erases the only record of an object still
	// running, and the plan that would clean it up never proposes it. Only
	// for an update: a create has nothing deposed yet, and record deposits
	// the old object for a create_before_destroy create phase itself.
	if node.Kind == planner.OpUpdate && current != nil && result != nil {
		result.Deposed = nil
		for _, d := range current.Deposed {
			result.Deposed = append(result.Deposed, d.Clone())
		}
	}

	return result, isRemoval(node), nil
}

// verbFor maps an OpNode to the provider verb its dispatch will use — the
// same Kind and Phase mapping dispatch switches on — so retry classification
// is decided per provider call. A forget has no verb: no call is made.
func verbFor(node planner.OpNode) (retry.Verb, bool) {
	switch {
	case node.Kind == planner.OpCreate,
		node.Kind == planner.OpReplace && node.Phase == planner.PhaseCreate:
		return retry.VerbCreate, true
	case node.Kind == planner.OpUpdate:
		return retry.VerbUpdate, true
	case node.Kind == planner.OpDestroy,
		node.Kind == planner.OpDestroyDeposed,
		node.Kind == planner.OpReplace && node.Phase == planner.PhaseDestroy:
		return retry.VerbDelete, true
	default:
		return retry.VerbInvalid, false
	}
}

// needsDesired reports whether an operation needs its desired values resolved
// before dispatch. A destroy — standalone, or a replace's destroy phase —
// does not: it deletes what state already records, and nothing in the new
// configuration is an input to that call.
//
// This looks like an optimisation and is not. resolveAfter treats a
// still-unknown reference as a hard error, correctly, because by execution
// time ordering should have made every reference resolvable. But a replace's
// destroy phase runs before the resources its new attributes point at, so
// resolving it would demand a value that is not supposed to exist yet. Only
// a replace whose new attributes reference a resource created in the same
// run shows the difference.
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

// noResourceStateError reports a provider whose Create or Update returned
// neither state nor an error.
//
// It is a distinct type so Apply's loop can tell it apart from a failed
// write. Both reach the same stopping gate but have opposite consequences:
// after a failed write state holds the entry, so the operation is applied;
// here it does not, so the operation failed. Matching on message text would
// make that accounting depend on a sentence nobody would treat as
// load-bearing.
type noResourceStateError struct{ msg string }

func (e *noResourceStateError) Error() string { return e.msg }

// stampInstance records which provider instance a resource belongs to, on
// the state a provider just returned.
//
// The engine has to do this, not the provider: a plugin knows its own name
// and has no way to know which of several instances of itself it is. Left to
// the provider, state records the plugin, and two accounts become
// indistinguishable in the one place that must tell them apart — the destroy
// path, which reads the instance from state because the resource has left
// configuration by then. An instance that never reaches state is a destroy
// with no account to delete from.
func stampInstance(rs *resource.ResourceState, instance string) {
	if rs == nil || instance == "" {
		return
	}
	rs.Provider = instance
}

// providerCeiling is how many operations may run at once against one provider:
// what that plugin declared, or opts.PerProvider when it declared nothing.
//
// Below 1 is folded to 1 rather than honoured: a ceiling of zero would admit
// no work at all and the run would hang holding a state lock. The number
// comes from a third-party plugin, so getting it wrong is not hypothetical.
func providerCeiling(opts Options, providerName string) int {
	n, ok := opts.ProviderLimits[providerName]
	if !ok {
		return opts.PerProvider
	}
	if n < 1 {
		return 1
	}
	return n
}
