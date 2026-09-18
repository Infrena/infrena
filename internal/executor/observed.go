package executor

import (
	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/resource"
)

// currentFor builds the `current` one operation's provider call receives:
// what the refresh immediately before planning OBSERVED, with the host's own
// bookkeeping restamped onto it from the last state that was persisted.
//
// WHY NOT STATE. The executor used to hand the provider the state it loaded
// off disk, which is the last apply's record of what it wrote — not what is
// out there now. An apply's whole shape refutes that: it refreshes, plans
// state against the observation, and then executed the plan against the half
// of that diff it had just proven stale. A provider that diffs `current`
// against `desired` to build a patch — which pkg/provider.Provider.Read
// invites in as many words, "the attributes to diff against", and which is
// what the AWS plugin's Cloud Control path does — therefore missed exactly
// the drift the plan had correctly proposed to correct, and re-sent an
// earlier, already-reverted drift as a change of its own. The plan was
// right; only the input to execution was stale.
//
// WHY NOT A STRAIGHT SWAP. A ResourceState is two things at once, and only
// one of them belongs to the provider. The provider owns the ProviderID and
// the Attributes; Address, Type, the provider INSTANCE, Dependencies,
// Lifecycle, CreatedAt and UpdatedAt are the host's bookkeeping, attached on
// the way in and — pkg/provider.Provider.Read says so explicitly — never
// something a provider is asked to report back. An observation therefore
// carries the first half honestly and the second half not at all, so
// replacing `current` outright would hand a provider an empty Lifecycle and
// no Dependencies.
//
// That is not merely untidy, it is destructive, because `current` is not
// only an input. internal/pluginhost.rebuild builds the state the executor
// PERSISTS out of `current`'s bookkeeping fields for every out-of-process
// provider — so a `current` with an empty Lifecycle writes an empty
// Lifecycle to disk, and a prevent_destroy guard silently ceases to exist.
// Restamping here is what keeps observed attributes from costing the host
// everything it knew.
//
// WHY NOT BEFORE PLANNING. The obvious-looking alternative — merge the
// observation into state as soon as refresh returns, so everything
// downstream sees one current truth — destroys the product. The planner's
// diff IS state against observation (internal/planner.Compute takes both);
// merge them first and the diff collapses to nothing, the planner proposes
// no change, and every drift silently stops being corrected. This merge
// belongs strictly between planning and execution, which is why it lives
// here and not in internal/cli's computePlan, and why `st` goes on meaning
// "the last state persisted" everywhere else it appears.
//
// observed is nil, or has no entry, or has a nil entry, whenever the refresh
// saw nothing usable — a failed read, a resource deleted outside infrena, or
// `apply --plan`, which does not refresh at all. All three fall back to
// persisted. That fallback is load-bearing rather than defensive: dispatch
// refuses a nil `current` for update and destroy outright, so returning the
// observation unconditionally would turn each of those into a failed
// operation instead of one running against the best record available.
func currentFor(a address.Address, persisted map[string]*resource.ResourceState, observed map[string]*resource.ResourceState) *resource.ResourceState {
	prior := persisted[a.String()]
	seen := observed[a.String()]
	if seen == nil {
		return prior
	}
	if prior == nil {
		// Nothing persisted but something observed: not reachable through
		// planning today (an address with no state entry is a create, and
		// refresh only ever reads addresses that ARE in state), so there is
		// no bookkeeping to restamp and the observation stands alone.
		return seen
	}

	// Clone: the observation map is shared, read-only, across every worker
	// in this run, and a provider is handed this pointer.
	out := seen.Clone()

	// The host's half, restamped from the last persisted state. This is the
	// same field list pkg/provider.Provider.Read names as the host's, and
	// the same one internal/pluginhost.rebuild carries — deliberately, so a
	// provider sees one consistent answer about who owns what regardless of
	// whether it runs in process or behind a pipe.
	out.Address = prior.Address
	out.Type = prior.Type
	out.Provider = prior.Provider
	out.Dependencies = append([]address.Address(nil), prior.Dependencies...)
	out.Lifecycle = prior.Lifecycle
	out.CreatedAt = prior.CreatedAt
	out.UpdatedAt = prior.UpdatedAt
	if out.ProviderID == "" {
		// The observation's ID wins when it has one — a provider knows its
		// own handle better than the host's record of it does — but an
		// in-process provider is under no obligation to return one from
		// Read, and a `current` with no ProviderID is a provider that cannot
		// find the object it manages.
		out.ProviderID = prior.ProviderID
	}
	return out
}

// deposedOf returns the object a create_before_destroy replacement set aside,
// or nil when there is none.
//
// The FIRST entry, and in practice there is only ever one: a second would mean
// two replacements of one resource whose destroys both failed, which is a
// situation to report rather than to tidy away silently. Taking the first keeps
// each run removing exactly one, so a backlog drains one plan at a time with
// every step visible.
func deposedOf(s *resource.ResourceState) *resource.ResourceState {
	if s == nil || len(s.Deposed) == 0 {
		return nil
	}
	return s.Deposed[0]
}
