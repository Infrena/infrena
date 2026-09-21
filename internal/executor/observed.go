package executor

import (
	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/resource"
)

// currentFor builds the current state one operation's provider call
// receives: what the refresh before planning observed, with the host's own
// bookkeeping restamped onto it from the last persisted state.
//
// Not the persisted state alone, because that is the last apply's record of
// what it wrote rather than what is out there now. A provider that diffs
// current against desired to build a patch would then miss exactly the drift
// the plan had proposed to correct.
//
// Not the observation alone, because a ResourceState is two things at once.
// The provider owns ProviderID and Attributes; Address, Type, the provider
// instance, Dependencies, Lifecycle and the timestamps are the host's, and a
// provider is never asked to report them back. The merged state is also what
// gets persisted for an out-of-process provider, so an observation used raw
// would write an empty Lifecycle to disk and a prevent_destroy guard would
// silently cease to exist.
//
// And not merged before planning, however tempting one current truth is: the
// planner's diff is persisted state against observation, so merging them
// first collapses the diff and every drift silently stops being corrected.
// The merge belongs strictly between planning and execution.
//
// A missing or nil observation — a failed read, a resource deleted outside
// infrena, or an apply from a saved plan, which does not refresh — falls back
// to the persisted state. That fallback is load-bearing: dispatch refuses a
// nil current for update and destroy.
func currentFor(a address.Address, persisted map[string]*resource.ResourceState, observed map[string]*resource.ResourceState) *resource.ResourceState {
	prior := persisted[a.String()]
	seen := observed[a.String()]
	if seen == nil {
		return prior
	}
	if prior == nil {
		// Nothing persisted but something observed: not reachable through
		// planning today, since refresh only reads addresses that are in
		// state. There is no bookkeeping to restamp.
		return seen
	}

	// Clone: the observation map is shared, read-only, across every worker
	// in this run, and a provider is handed this pointer.
	out := seen.Clone()

	// The host's half, restamped from the last persisted state. It is the
	// same field list the plugin host carries, so a provider sees one answer
	// about who owns what whether it runs in process or behind a pipe.
	out.Address = prior.Address
	out.Type = prior.Type
	out.Provider = prior.Provider
	out.Dependencies = append([]address.Address(nil), prior.Dependencies...)
	out.Lifecycle = prior.Lifecycle
	out.CreatedAt = prior.CreatedAt
	out.UpdatedAt = prior.UpdatedAt
	if out.ProviderID == "" {
		// The observation's ID wins when it has one, but Read is not obliged
		// to return one, and a current state with no ProviderID is a provider
		// that cannot find the object it manages.
		out.ProviderID = prior.ProviderID
	}
	return out
}

// deposedOf returns the object a create_before_destroy replacement set aside,
// or nil when there is none.
//
// The first entry, and in practice there is only ever one: a second would
// mean two replacements of one resource whose destroys both failed, which is
// worth reporting rather than tidying away. Taking the first drains a backlog
// one plan at a time, with every step visible.
func deposedOf(s *resource.ResourceState) *resource.ResourceState {
	if s == nil || len(s.Deposed) == 0 {
		return nil
	}
	return s.Deposed[0]
}
