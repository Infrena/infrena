package discovery

import (
	"context"
	"fmt"
	"sort"

	"github.com/infrata/infrata/internal/registry"
	"github.com/infrata/infrata/pkg/provider"
	"github.com/infrata/infrata/pkg/value"
)

// Result is one discovered resource, with the name configuration would give it.
type Result struct {
	// Name is what Unique settled on. It is a PROPOSAL: discovery writes
	// nothing, so a user sees a collision here and can tag the resource before
	// importing rather than after.
	Name       string
	Type       string
	ProviderID string
	Provider   string
	Attributes map[string]value.Value
}

// Walk asks every registered provider what exists, and names what comes back
// (spec §25).
//
// types narrows the question when non-empty. A provider is only asked about the
// types it actually offers, and one that offers none of the requested types is
// not asked at all — a real provider's Discover is API calls, and asking AWS
// about a Google type is a round trip whose answer is known in advance.
//
// A provider that does not implement discovery is REPORTED, not skipped
// silently. "found nothing" and "cannot look" are different answers, and a user
// deciding whether their account is empty needs to know which one they got.
//
// Naming happens after the whole set is sorted, never during the walk: Unique
// is order-dependent by construction, so naming as results arrive would make
// the name a resource gets depend on which provider answered first.
func Walk(ctx context.Context, reg *registry.Registry, types []string) ([]Result, []error) {
	wanted := map[string]bool{}
	for _, t := range types {
		wanted[t] = true
	}

	var out []Result
	var problems []error
	for _, p := range reg.Providers() {
		ask := reg.TypesOf(p.Name())
		if len(wanted) > 0 {
			ask = filterWanted(ask, wanted)
			if len(ask) == 0 {
				continue
			}
		}

		found, err := p.Discover(ctx, provider.DiscoverRequest{Types: ask})
		if err != nil {
			problems = append(problems, fmt.Errorf("provider %s: %w", p.Name(), err))
			continue
		}
		for _, r := range found {
			out = append(out, Result{
				Type:       r.Type,
				ProviderID: r.ProviderID,
				Provider:   p.Name(),
				Attributes: r.Attributes,
			})
		}
	}

	// Sorted before naming, so the names themselves are deterministic.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Type != out[j].Type {
			return out[i].Type < out[j].Type
		}
		return out[i].ProviderID < out[j].ProviderID
	})

	taken := map[string]string{}
	for i := range out {
		out[i].Name = Unique(taken, provider.DiscoveredResource{
			Type:       out[i].Type,
			ProviderID: out[i].ProviderID,
			Attributes: out[i].Attributes,
		})
	}
	return out, problems
}

func filterWanted(types []string, wanted map[string]bool) []string {
	var out []string
	for _, t := range types {
		if wanted[t] {
			out = append(out, t)
		}
	}
	return out
}
