package cli

import (
	"context"

	"github.com/infrena/infrena/internal/state"
)

// managedProviderIDs indexes every resource this project already manages,
// anywhere, as provider ID to the environment managing it.
//
// The scope is every environment, not the one a command is pointed at: a
// resource managed in production is managed whichever environment you are
// importing into. Adopting it a second time would put one real resource under
// two addresses, the second declaring nothing, so the next apply proposes
// destroying infrastructure the first address still manages.
//
// Keyed on the provider ID rather than the address, because the address is what
// infrena chose to call the resource and a resource adopted twice has two of
// them; the provider ID is the only half of the pair discovery and state agree
// on.
//
// A project with no state at all is an empty index, not an error.
func managedProviderIDs(ctx context.Context, backend state.Backend) (map[string]string, error) {
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
				// mistake; keeping the first is at least stable, because List
				// is sorted.
				continue
			}
			out[r.ProviderID] = environment
		}
	}
	return out, nil
}
