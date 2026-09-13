// Package refresh reads every resource in state from its provider,
// concurrently, so the planner has current reality to diff configuration
// against. It never writes: spec §10 reserves persistence for the refresh
// command M3 adds, so plan stays safe to run against a locked environment,
// in CI, or repeatedly, and Compute (Task 13) stays a pure function of the
// inputs it is handed.
package refresh

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/infrata/infrata/internal/diag"
	"github.com/infrata/infrata/internal/registry"
	"github.com/infrata/infrata/internal/state"
	"github.com/infrata/infrata/pkg/address"
	"github.com/infrata/infrata/pkg/resource"
	"github.com/infrata/infrata/pkg/value"
)

// Observation is what Refresh learned about one resource recorded in state.
type Observation struct {
	// Address identifies the resource.
	Address address.Address
	// State is the provider's current report, or nil when the resource no
	// longer exists there — Read's (nil, nil) contract — or when Err is set.
	State *resource.ResourceState
	// Err is set when reading the resource failed. It is never a stand-in
	// for absence: a read error and a deleted resource are different facts,
	// and treating the first as the second would propose destroying
	// infrastructure that may well still be there.
	Err error
}

// Observations is what Refresh learned, keyed by Address.String().
type Observations map[string]Observation

// Refresh reads the current provider state of every resource recorded in
// st, concurrently, bounded twice: globally by parallelism, and per provider
// by perProvider (spec §10 — "bounded by the same per-provider semaphore the
// executor uses (§15)"). Values below 1 behave as 1 for both.
//
// The second bound is not decoration. A refresh reads EVERY resource in
// state, so it is the widest fan-out in the product — wider than most
// applies — and it is exactly the operation §34's "bounded per provider or
// account to avoid API throttling" is about: a state file holding two
// hundred resources of one type would otherwise open two hundred reads
// against that one provider the moment the global bound allowed it.
//
// Refresh never writes to st or anywhere else: it is a pure read, and its
// result is meant to be used in memory and discarded, exactly as plan does.
// A read error becomes a diagnostic that fails planning for that resource;
// it is never treated as deletion. Results and diagnostics are assembled in
// address order regardless of which read finishes first, so two runs over
// the same state produce byte-identical output.
//
// onObservation, when non-nil, is called once per resource as its
// Observation is produced, before Refresh returns — the progress hook the
// `infra refresh` command uses to stream machine-readable output. It carries
// the identical concurrency contract as executor.Options.OnEvent: it may be
// called concurrently from multiple worker goroutines (one per resource
// being read at once, up to parallelism), so a receiver that is not itself
// safe for concurrent use must serialize its own access. nil means no
// hook — every other caller of Refresh (plan, and apply/destroy's
// computePlan) passes nil, since neither reports refresh progress of its
// own; only the refresh command does.
func Refresh(ctx context.Context, st *state.State, reg *registry.Registry, parallelism, perProvider int, onObservation func(Observation)) (Observations, diag.Diagnostics) {
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
	// One semaphore per provider, created on demand. Built up front, on the
	// owner goroutine, rather than lazily inside the workers: a map written
	// from several goroutines at once is a data race, and taking a mutex
	// around it would serialise precisely the fan-out this function exists
	// to parallelise. A resource whose type resolves to no provider gets no
	// per-provider bound and needs none — readOne turns it into a
	// diagnostic without ever calling out.
	perProviderSem := map[string]chan struct{}{}
	providerOf := make([]string, len(addrs))
	for i, addr := range addrs {
		rs, ok := st.Get(addr)
		if !ok {
			continue
		}
		// From STATE. Refresh reads only state, and a resource's own entry is the
		// only thing that says which account to ask about it (PLAN.md §12.1).
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

	var wg sync.WaitGroup
	for i, addr := range addrs {
		wg.Add(1)
		sem <- struct{}{}
		if ps := perProviderSem[providerOf[i]]; ps != nil {
			ps <- struct{}{}
		}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if ps := perProviderSem[providerOf[i]]; ps != nil {
				defer func() { <-ps }()
			}
			results[i], problems[i] = readOne(ctx, st, reg, addr)
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

// readOne reads one resource's current provider state.
func readOne(ctx context.Context, st *state.State, reg *registry.Registry, addr address.Address) (Observation, diag.Diagnostics) {
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

	// Checked immediately before the read, not once up front in Refresh: a
	// cancellation that lands mid-flight must still stop resources that
	// haven't started yet from calling the provider, and this is the last
	// point before that call. It is folded into the same "error, never a
	// deletion" path as any other read failure below, so Ctrl+C does not
	// depend on a provider bothering to check its own ctx.
	if err := ctx.Err(); err != nil {
		return readErrorObservation(addr, err)
	}

	// rs is the pointer state.State.Get returns, which is the same pointer
	// state.State.Resources holds — not a copy. Handing it to prov.Read
	// directly would let a provider that mutates its `current` argument in
	// place corrupt live in-memory state with no write call anywhere in the
	// trace; pkg/resource.ResourceState.Clone's own doc comment names this
	// package's obligation here: "Refresh and planning must never mutate the
	// state that was loaded from disk." Cloning is what makes that true
	// regardless of how a given provider's Read happens to behave.
	current, err := prov.Read(ctx, rs.Clone())
	if err != nil {
		return readErrorObservation(addr, err)
	}

	// current is nil exactly when the provider reports the resource no
	// longer exists — Read's (nil, nil) contract, and how deletion outside
	// infra is detected. It is not an error.
	if current != nil {
		// Sensitivity that reached state by PROPAGATION (spec §36 — a value
		// that became secret by flowing through ${db.password}) exists only
		// in the engine's record. A provider re-derives schema-declared
		// sensitivity from its own schema and knows nothing about the other
		// kind, so an observation carries back only half of what state
		// already knew.
		//
		// That matters twice over. `infra refresh` persists whatever Read
		// returns, so without this it would not leave the flag stale, it
		// would ERASE it. And every destroy plan renders its Before from the
		// observation rather than from state, so a propagated secret would be
		// printed in clear by the one command most likely to be run with
		// someone watching.
		//
		// Carrying, not deciding: this adds flags and never clears one, and
		// pkg/value.Format is still the only thing that redacts.
		current.Attributes = value.CarrySensitivityAttrs(current.Attributes, rs.Attributes)
	}
	return Observation{Address: addr, State: current}, nil
}

// readErrorObservation builds the Observation and Diagnostics for a failed
// read, whether the failure came from the provider or from ctx being
// cancelled before the provider was even called. Both are the same fact:
// reality is unknown right now, and that must never be mistaken for the
// resource having been deleted.
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
