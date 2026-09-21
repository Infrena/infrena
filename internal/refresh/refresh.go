// Package refresh reads every resource in state from its provider,
// concurrently, so the planner has current reality to diff configuration
// against.
//
// It never writes. Persistence belongs to the refresh command, which keeps
// plan safe to run repeatedly, in CI, or against a locked environment, and
// keeps plan computation a pure function of its inputs.
package refresh

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/internal/registry"
	"github.com/infrena/infrena/internal/retry"
	"github.com/infrena/infrena/internal/state"
	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/resource"
	"github.com/infrena/infrena/pkg/value"
)

// Observation is what Refresh learned about one resource recorded in state.
type Observation struct {
	// Address identifies the resource.
	Address address.Address
	// State is the provider's current report, or nil when the resource no
	// longer exists there — Read's (nil, nil) contract — or when Err is set.
	State *resource.ResourceState
	// Err is set when reading the resource failed. It is never a stand-in
	// for absence: treating a read error as a deletion would propose
	// destroying infrastructure that may well still be there.
	Err error
}

// Observations is what Refresh learned, keyed by Address.String().
type Observations map[string]Observation

// Refresh reads the current provider state of every resource recorded in st,
// concurrently, bounded twice: globally by parallelism, and per provider by
// perProvider. Values below 1 behave as 1 for both.
//
// The per-provider bound is what keeps a refresh from being throttled. A
// refresh reads every resource in state, so it is the widest fan-out in the
// product: a state file holding two hundred resources of one type would
// otherwise open two hundred reads against that one provider.
//
// Refresh never writes to st or anywhere else. A read error becomes a
// diagnostic that fails planning for that resource; it is never treated as
// deletion. Results and diagnostics are assembled in address order whatever
// order the reads finish in, so two runs over the same state produce
// identical output.
//
// onObservation, when non-nil, is called once per resource as its
// Observation is produced — the progress hook the refresh command uses to
// stream output. It may be called concurrently from several worker
// goroutines, so a receiver that is not itself safe for concurrent use must
// serialize its own access. Only the refresh command passes one.
func Refresh(ctx context.Context, st *state.State, reg *registry.Registry, parallelism, perProvider int, policy retry.Policy, onObservation func(Observation)) (Observations, diag.Diagnostics) {
	if parallelism < 1 {
		parallelism = 1
	}
	if perProvider < 1 {
		perProvider = 1
	}

	addrs := st.Addresses() // already sorted
	results := make([]Observation, len(addrs))
	problems := make([]diag.Diagnostics, len(addrs))

	sem := make(chan struct{}, parallelism)
	// One semaphore per provider, built here on the owner goroutine rather
	// than lazily inside the workers: a map written from several goroutines
	// is a data race, and a mutex around it would serialise the fan-out this
	// function exists to parallelise. A resource whose type resolves to no
	// provider needs no bound — readOne turns it into a diagnostic without
	// ever calling out.
	perProviderSem := map[string]chan struct{}{}
	providerOf := make([]string, len(addrs))
	for i, addr := range addrs {
		rs, ok := st.Get(addr)
		if !ok {
			continue
		}
		// From state, not configuration: a resource's own state entry is the
		// only thing that says which account to ask about it.
		prov, ok := reg.ProviderFor(rs.Type, rs.Provider)
		if !ok {
			continue
		}
		name := prov.Name()
		providerOf[i] = name
		if _, ok := perProviderSem[name]; !ok {
			perProviderSem[name] = make(chan struct{}, perProvider)
		}
	}

	// The per-provider semaphore is taken inside the worker, the global one
	// here, and that split matters. Taking the per-provider semaphore on this
	// goroutine would let one saturated provider block the dispatch of work
	// for every other provider, including ones sitting idle. Taking the
	// global semaphore here is what bounds live goroutines — what
	// --parallelism means to a user — so this loop applies back-pressure
	// instead of spawning one goroutine per resource in state.
	//
	// The residue: a worker waiting on its provider's semaphore still holds a
	// global slot, so a saturated provider can hold slots a faster provider
	// would use. That is bounded by parallelism and self-clearing.
	var wg sync.WaitGroup
	for i, addr := range addrs {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if ps := perProviderSem[providerOf[i]]; ps != nil {
				ps <- struct{}{}
				defer func() { <-ps }()
			}
			results[i], problems[i] = readOne(ctx, st, reg, addr, policy)
			if onObservation != nil {
				onObservation(results[i])
			}
		}()
	}
	wg.Wait()

	out := make(Observations, len(addrs))
	var ds diag.Diagnostics
	for i, addr := range addrs {
		out[addr.String()] = results[i]
		ds.Extend(problems[i])
	}
	return out, ds
}

// States reduces the observations to what the executor needs: the resource
// state seen at each address, keyed as Observations is.
//
// A failed read and a resource that no longer exists both map to a nil
// entry. Callers have already acted on that distinction by this point — a
// read error fails planning, so an apply never reaches the executor with
// one — and what to hand the provider is the same either way. Nothing
// downstream may use a nil entry to conclude a resource was deleted.
func (o Observations) States() map[string]*resource.ResourceState {
	out := make(map[string]*resource.ResourceState, len(o))
	for key, obs := range o {
		out[key] = obs.State
	}
	return out
}

// readOne reads one resource's current provider state.
func readOne(ctx context.Context, st *state.State, reg *registry.Registry, addr address.Address, policy retry.Policy) (Observation, diag.Diagnostics) {
	rs, ok := st.Get(addr)
	if !ok {
		// Unreachable in practice: addr always comes from st.Addresses().
		// Guarded rather than assumed, so a future caller that builds its
		// own address list cannot silently read garbage.
		var ds diag.Diagnostics
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  addr.String() + " is not in state",
			Related:  []address.Address{addr},
		})
		return Observation{Address: addr, Err: fmt.Errorf("%s: not in state", addr)}, ds
	}

	prov, ok := reg.ProviderFor(rs.Type, rs.Provider)
	if !ok {
		var ds diag.Diagnostics
		err := fmt.Errorf("%s: resource type %q is no longer registered", addr, rs.Type)
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "resource type " + strconv.Quote(rs.Type) + " is no longer registered",
			Detail: addr.String() + " is recorded in state as " + rs.Type +
				", but no provider registers that type.\nKnown types:\n  " + strings.Join(reg.Types(), "\n  "),
			Action:  "Restore the provider that registers " + rs.Type + ", or remove this resource from state once you have confirmed it is safe to.",
			Related: []address.Address{addr},
		})
		return Observation{Address: addr, Err: err}, ds
	}

	// Checked here rather than once up front, so a cancellation landing
	// mid-run still stops resources that have not started from calling out.
	// It takes the same "error, never a deletion" path as any read failure,
	// so cancellation does not depend on a provider checking its own ctx.
	if err := ctx.Err(); err != nil {
		return readErrorObservation(addr, err)
	}

	// st.Get returns the live pointer, not a copy, so the read is handed a
	// clone: a provider that mutates its argument in place would otherwise
	// corrupt in-memory state with no write call anywhere in the trace.
	// Refresh and planning must never mutate the state loaded from disk.
	//
	// The read is retried, with the provider classifying its own errors. A
	// refresh makes more provider calls than anything else, so it is the most
	// likely thing to be throttled, and a throttle is refused before it acts:
	// waiting is the whole fix. A zero policy means exactly one attempt.
	//
	// The clone is taken per attempt: handing a second attempt the object a
	// failed first attempt already touched would defeat the point of it.
	var current *resource.ResourceState
	err := retry.Attempt(ctx, retry.VerbRead, policy, prov.ClassifyError, func() error {
		var readErr error
		current, readErr = prov.Read(ctx, rs.Clone())
		return readErr
	})
	if err != nil {
		return readErrorObservation(addr, err)
	}

	// current is nil exactly when the provider reports the resource no longer
	// exists — Read's (nil, nil) contract, and how deletion outside infrena
	// is detected. It is not an error.
	if current != nil {
		// Sensitivity that reached state by propagation — a value that became
		// secret by flowing through another sensitive value — exists only in
		// the engine's record. A provider re-derives sensitivity from its own
		// schema and knows nothing of the propagated kind, so an observation
		// carries back only half of what state already knew.
		//
		// That matters twice: refresh persists what Read returns, so without
		// this it would erase the flag rather than leave it stale, and a
		// destroy plan renders its before-state from the observation, so a
		// propagated secret would be printed in clear.
		//
		// This carries flags and never clears one; formatting still does all
		// the redacting.
		current.Attributes = value.CarrySensitivityAttrs(current.Attributes, rs.Attributes)
	}
	return Observation{Address: addr, State: current}, nil
}

// readErrorObservation builds the Observation and Diagnostics for a failed
// read, whether the provider failed or ctx was cancelled before it was
// called. Both say the same thing: reality is unknown, which must never be
// mistaken for the resource having been deleted.
func readErrorObservation(addr address.Address, err error) (Observation, diag.Diagnostics) {
	var ds diag.Diagnostics
	ds.Add(diag.Diagnostic{
		Severity: diag.SeverityError,
		Summary:  "failed to read " + addr.String() + ": " + err.Error(),
		Detail:   "A read failure is a diagnostic, never a deletion: treating it as absence would propose destroying infrastructure that may still exist.",
		Related:  []address.Address{addr},
	})
	return Observation{Address: addr, Err: err}, ds
}
