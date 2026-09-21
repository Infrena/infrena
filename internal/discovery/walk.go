package discovery

import (
	"context"
	"fmt"
	"sort"

	"github.com/infrena/infrena/internal/registry"
	"github.com/infrena/infrena/internal/retry"
	"github.com/infrena/infrena/pkg/provider"
	"github.com/infrena/infrena/pkg/value"
)

// Result is one discovered resource, with the name configuration would give it.
type Result struct {
	// Name is what Unique settled on. It is a proposal: discovery writes
	// nothing, so a user sees a collision here and can tag the resource before
	// importing rather than after.
	Name       string
	Type       string
	ProviderID string
	Provider   string
	Attributes map[string]value.Value

	// SystemOwned and SystemOwnedReason are carried from the provider
	// unchanged. Discovery could not decide them: knowing that a VPC is an
	// account's default VPC is knowledge about AWS, and the core engine has
	// none.
	SystemOwned       bool
	SystemOwnedReason string
}

// Progress is one notification about a provider instance's sweep, so a command
// can say something while it waits.
//
// Per instance, not per type, because that is all the host can honestly report:
// Walk asks each instance for every type it serves in one Discover call and the
// plugin does the sweep internally, so a per-type count would have to be
// invented here. What this carries is true — the sweep started, how much it
// asked for, and how it ended.
type Progress struct {
	// Instance is the provider instance being asked.
	Instance string
	// Types is how many resource types the sweep covers.
	Types int
	// Found is how many resources came back. Only meaningful when Done.
	Found int
	// Done distinguishes the finish from the start.
	Done bool
	// Err is why the sweep failed, when it did. A failed sweep still
	// reports a finish, so nothing is left running in the heartbeat.
	Err error
}

// Walk asks every provider instance what exists, and names what comes back.
// onProgress may be nil.
//
// types narrows the question when non-empty. An instance is only asked about
// the types it actually offers, and one that offers none of the requested types
// is not asked at all: Discover is API calls, and asking AWS about a Google
// type is a round trip whose answer is known in advance.
//
// A provider that cannot look is reported in the returned errors rather than
// skipped silently, because "found nothing" and "cannot look" are different
// answers to the question of whether an account is empty.
//
// Naming happens after the whole set is sorted, never during the walk: Unique
// is order-dependent by construction, so naming as results arrive would make
// the name a resource gets depend on which provider answered first.
func Walk(ctx context.Context, reg *registry.Registry, types []string, policy retry.Policy, onProgress func(Progress)) ([]Result, []error) {
	report := func(p Progress) {
		if onProgress != nil {
			onProgress(p)
		}
	}
	wanted := map[string]bool{}
	for _, t := range types {
		wanted[t] = true
	}

	var out []Result
	var problems []error
	// Every instance, not every plugin. Two instances of one plugin hold
	// different infrastructure, so asking the plugin once would find one
	// account's resources and silently miss the other's.
	for _, inst := range reg.Instances() {
		p := inst.Provider
		ask := reg.TypesOf(inst.Name)
		if len(wanted) > 0 {
			ask = filterWanted(ask, wanted)
			if len(ask) == 0 {
				continue
			}
		}

		report(Progress{Instance: inst.Name, Types: len(ask)})

		// Retried on the provider's own classification, the same way refresh's
		// Read is: a Discover asks for every type an instance serves, so it is
		// at least as likely to be throttled. A zero policy means one attempt.
		var found []provider.DiscoveredResource
		err := retry.Attempt(ctx, retry.VerbRead, policy, p.ClassifyError, func() error {
			var discoverErr error
			found, discoverErr = p.Discover(ctx, provider.DiscoverRequest{Types: ask})
			return discoverErr
		})
		if err != nil {
			report(Progress{Instance: inst.Name, Types: len(ask), Done: true, Err: err})
			problems = append(problems, fmt.Errorf("provider %s: %w", inst.Name, err))
			continue
		}
		report(Progress{Instance: inst.Name, Types: len(ask), Found: len(found), Done: true})
		for _, r := range found {
			out = append(out, Result{
				Type:       r.Type,
				ProviderID: r.ProviderID,
				// The instance name, because that is what a resource's
				// `provider:` selects and what state must record: a plugin
				// name cannot tell two accounts apart.
				Provider:          inst.Name,
				Attributes:        r.Attributes,
				SystemOwned:       r.SystemOwned,
				SystemOwnedReason: r.SystemOwnedReason,
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
		out[i].Name = Unique(reg, taken, provider.DiscoveredResource{
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
