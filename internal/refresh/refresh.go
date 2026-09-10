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

	"infra/internal/diag"
	"infra/internal/registry"
	"infra/internal/state"
	"infra/pkg/address"
	"infra/pkg/resource"
	"infra/pkg/value"
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
// st, concurrently, bounded by parallelism (values below 1 behave as 1).
//
// Refresh never writes to st or anywhere else: it is a pure read, and its
// result is meant to be used in memory and discarded, exactly as plan does.
// A read error becomes a diagnostic that fails planning for that resource;
// it is never treated as deletion. Results and diagnostics are assembled in
// address order regardless of which read finishes first, so two runs over
// the same state produce byte-identical output.
func Refresh(ctx context.Context, st *state.State, reg *registry.Registry, parallelism int) (Observations, diag.Diagnostics) {
	if parallelism < 1 {
		parallelism = 1
	}

	addrs := st.Addresses() // already sorted
	results := make([]Observation, len(addrs))
	problems := make([]diag.Diagnostics, len(addrs))

	sem := make(chan struct{}, parallelism)
	var wg sync.WaitGroup
	for i, addr := range addrs {
		i, addr := i, addr
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			results[i], problems[i] = readOne(ctx, st, reg, addr)
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

	prov, ok := reg.Provider(rs.Type)
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
