package cli

import (
	"context"

	"github.com/infrena/infrena/internal/state"
)

// managedProviderIDs indexes every resource this project already manages,
// anywhere, as provider ID to the environment managing it.
//
// SCOPE IS EVERY ENVIRONMENT, not the one a command happens to be pointed at.
// PLAN.md §6.1 makes an environment reachable if it is declared or it has
// state, and a resource managed in production is managed whichever environment
// you are importing into. Adopting it a second time would put one real resource
// under two addresses, and the second declares nothing — which is exactly what
// invariant 1 reads as "removed from configuration", so the next apply proposes
// destroying infrastructure the first address still manages.
//
// KEYED ON THE PROVIDER ID, not on the address. An address is what infrena
// chose to call the resource, and the same real resource adopted twice would
// have two of them; the provider ID is what the cloud calls it, and it is the
// only half of the pair that discovery and state can agree on.
//
// A project with no state at all is an empty index and not an error: that is
// the ordinary case for the command that needs this.
func managedProviderIDs(ctx context.Context, backend *state.Local) (map[string]string, error) {
	environments, err := backend.List(ctx)
	if err != nil {
		return nil, err
	}

	out := map[string]string{}
	for _, environment := range environments {
		st, err := backend.Get(ctx, environment)
		if err != nil {
			return nil, err
		}
		for _, addr := range st.Addresses() {
			r, ok := st.Get(addr)
			if !ok || r.ProviderID == "" {
				// An entry with no provider ID names no real resource, so it
				// manages nothing that discovery could report.
				continue
			}
			if _, held := out[r.ProviderID]; held {
				// One resource adopted into two environments is already a
				// mistake; naming the first is stable because List is sorted,
				// and naming a different one on each run would be worse than
				// naming an arbitrary one consistently.
				continue
			}
			out[r.ProviderID] = environment
		}
	}
	return out, nil
}
