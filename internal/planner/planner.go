package planner

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"infra/internal/compiler"
	"infra/internal/diag"
	"infra/internal/expressions"
	"infra/internal/graph"
	"infra/internal/refresh"
	"infra/internal/registry"
	"infra/internal/state"
	"infra/pkg/address"
	"infra/pkg/resource"
)

// Options carries what planning needs beyond the three inputs it diffs.
type Options struct {
	// Environment names the environment being planned. It is cross-checked
	// against the configuration and the state: planning one environment's
	// configuration against another's state is the mistake that destroys
	// production.
	Environment string
	// Now supplies the plan's timestamp. It is injectable because Compute is
	// pure, and a function that calls time.Now directly is not.
	Now func() time.Time
	// Registry supplies the schemas the diff needs in order to tell a
	// ForceNew attribute from an updatable one and a computed attribute from
	// desired state. ResolvedConfig carries resolved values, not the schema
	// that governs them, and without it two of spec §11's rules are
	// unimplementable. A nil Registry is an error, never a degradation.
	Registry *registry.Registry
}

// Compute decides one operation per resource by comparing resolved
// configuration against recorded state and observed provider reality.
//
// It is a pure function of its inputs: no filesystem, no network, no provider.
// The returned plan is always non-nil, even when diagnostics contain errors,
// so the caller can render both together; a plan carrying an error is never
// applyable (spec §12.2).
func Compute(cfg compiler.ResolvedConfig, st *state.State, obs refresh.Observations, opts Options) (*Plan, diag.Diagnostics) {
	var ds diag.Diagnostics

	now := opts.Now
	if now == nil {
		now = time.Now
	}

	environment := cfg.Environment
	if environment == "" {
		environment = opts.Environment
	}

	p := &Plan{
		Version:     PlanVersion,
		CreatedAt:   now().UTC(),
		Project:     cfg.Project,
		Environment: environment,
		Operations:  []Operation{},
	}

	if opts.Environment != "" && cfg.Environment != "" && opts.Environment != cfg.Environment {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "configuration was compiled for a different environment",
			Detail: "Planning was asked for environment " + strconv.Quote(opts.Environment) +
				" but the configuration resolves environment " + strconv.Quote(cfg.Environment) + ".",
			Action: "Recompile the configuration for " + opts.Environment + ".",
		})
	}
	if st != nil && st.Environment != "" && st.Environment != environment {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "state belongs to a different environment",
			Detail: "The configuration is for environment " + strconv.Quote(environment) +
				" but the state records environment " + strconv.Quote(st.Environment) +
				". Diffing one environment's desired state against another's record would propose destroying everything in both.",
			Action: "Plan against the state for " + environment + ".",
		})
	}

	if hash, err := cfg.Hash(); err != nil {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "could not fingerprint the configuration",
			Detail:   err.Error(),
			Action:   "This is an engine defect; please report it.",
		})
	} else {
		p.ConfigHash = hash
	}

	if st != nil {
		p.StateSerial = st.Serial
		if hash, err := hashState(st); err != nil {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "could not fingerprint the state",
				Detail:   err.Error(),
				Action:   "This is an engine defect; please report it.",
			})
		} else {
			p.StateHash = hash
		}
	}

	// An environment mismatch stops planning here, before a single operation
	// is decided. The diagnostic alone is not enough protection: diffing this
	// configuration against another environment's state produces a complete,
	// well-formed, savable list of operations that destroys everything in one
	// environment and creates everything in the other. Every caller in this
	// codebase checks HasErrors() first, so nothing today would render it —
	// but a plan is an artifact that gets written to a file, passed around,
	// and read by `apply`, and "it is only dangerous if someone ignores the
	// diagnostics" is not a property worth relying on for the one failure mode
	// that can empty a production environment. Returning no operations makes
	// the dangerous plan impossible to produce rather than merely impolite to
	// use. Compile's returned config follows the same rule for the same
	// reason, and says so in its doc comment.
	if ds.HasErrors() {
		p.Diagnostics = append([]diag.Diagnostic(nil), ds...)
		return p, ds
	}

	if opts.Registry == nil {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "planning requires a provider registry",
			Detail: "Without the schemas the planner cannot tell an attribute that forces replacement " +
				"from one that updates in place, nor a computed attribute from desired state.",
			Action: "Pass Options.Registry.",
		})
		p.Diagnostics = append([]diag.Diagnostic(nil), ds...)
		return p, ds
	}

	// Operations are decided in dependency order, not address order, because
	// deciding one resource's operation can require another's answer:
	// ${network.id} in a database's configuration is knowable only once the
	// plan knows what network.id will be after apply. scope accumulates each
	// decided operation's After for exactly that, and dependency order is what
	// guarantees a dependency's After is already in it. Operations are sorted
	// by address afterwards, so the plan's own ordering contract is unchanged.
	scope := expressions.ResourceScope{}
	decided := map[string]Operation{}
	reported := map[string]diag.Diagnostics{}

	for _, addr := range resolutionOrder(cfg, st) {
		op, opDS := operationFor(addr, cfg, st, obs, opts, scope)
		reported[addr.String()] = opDS
		if op != nil {
			// Which side of the graph the edges come from depends on the
			// operation, so this runs once the kind has been decided.
			op.Dependents = dependentsOf(addr, op.Kind, cfg, st)
			decided[addr.String()] = *op
			if op.After != nil {
				scope[addr.String()] = op.After
			}
		}
	}

	// Operations and their diagnostics are emitted in planAddresses order
	// rather than by re-sorting what the pass above produced, so the plan's
	// "sorted by canonical address" contract — and the order a user reads
	// diagnostics in — still comes from the one helper that has always
	// provided them, unchanged by the fact that the decisions themselves are
	// now made in dependency order. resolutionOrder and planAddresses cover
	// the same set of addresses; only the order differs.
	for _, addr := range planAddresses(cfg, st) {
		ds.Extend(reported[addr.String()])
		if op, ok := decided[addr.String()]; ok {
			p.Operations = append(p.Operations, op)
		}
	}

	p.Diagnostics = append([]diag.Diagnostic(nil), ds...)
	return p, ds
}

// operationFor decides the single operation for one address, per spec §11's
// decision table. It returns nil when planning for that resource failed, in
// which case it has reported why.
func operationFor(
	addr address.Address,
	cfg compiler.ResolvedConfig,
	st *state.State,
	obs refresh.Observations,
	opts Options,
	scope expressions.ResourceScope,
) (*Operation, diag.Diagnostics) {
	var ds diag.Diagnostics

	rc, inConfig := cfg.Get(addr)

	var rs *resource.ResourceState
	if st != nil {
		rs, _ = st.Get(addr)
	}
	inState := rs != nil

	ob, observed := obs[addr.String()]
	if observed && ob.Err != nil {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "could not read " + addr.String() + " from its provider",
			Detail: "The provider reported: " + ob.Err.Error() +
				"\nPlanning cannot continue for this resource. A failed read is not evidence that anything was deleted, " +
				"and treating it as absence would propose destroying or recreating live infrastructure.",
			Action:  "Fix the provider error and run plan again.",
			Related: []address.Address{addr},
		})
		return nil, ds
	}

	// A missing observation is not evidence of absence either — refresh may
	// simply not have covered the address — so the record stands in for it.
	// Only an explicit Observation with a nil State means gone.
	actual := rs
	if observed {
		actual = ob.State
	}

	if !inConfig {
		op, removalDS := removalOperation(addr, rs, actual)
		ds.Extend(removalDS)
		return op, ds
	}

	def, known := opts.Registry.Definition(rc.Type)
	if !known {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "no schema for resource type " + strconv.Quote(rc.Type),
			Detail: "The planner needs the schema to tell an attribute that forces replacement from one that " +
				"updates in place, and a computed attribute from desired state. Without it the plan would " +
				"silently under-report replacements.",
			Action:  "Register a provider that defines " + rc.Type + ". Known types: " + strings.Join(opts.Registry.Types(), ", "),
			Origin:  rc.Origin,
			Related: []address.Address{addr},
		})
		return nil, ds
	}

	// Configuration is bound at compile time, when no resource exists, so a
	// ${other.attr} reference is necessarily unknown by then — correctly, and
	// that must stay correct (spec §6). Planning is the first phase that knows
	// what the referenced resource will actually look like, so it is the phase
	// that finishes those references. Skipping this step is what made
	// invariant 2 false for every configuration containing one: the diff
	// compared a real recorded value against a permanently unknown desired
	// value and proposed the same update forever, apply after apply.
	//
	// Only references whose target is already decided AND whose referenced
	// attribute is already known resolve here; everything else is left exactly
	// as the compiler bound it, still carrying its expression, so a genuinely
	// not-yet-knowable value still plans and renders as "(known after apply)"
	// and still reaches the executor intact.
	attrs, _, resolveDS := expressions.ResolveDeferred(rc.Attrs, scope)
	ds.Extend(resolveDS)
	if ds.HasErrors() {
		return nil, ds
	}

	if !inState || actual == nil {
		var reasons []ChangeReason
		if inState {
			// Recorded, but the provider no longer has it: something deleted
			// it outside infra, so the plan recreates it.
			reasons = []ChangeReason{{Note: "the provider no longer reports this resource; it will be recreated"}}
		}
		return &Operation{
			Address:   addr,
			Type:      rc.Type,
			Kind:      OpCreate,
			After:     afterAttributes(def, attrs, nil, OpCreate),
			Reasons:   reasons,
			Lifecycle: rc.Lifecycle,
		}, ds
	}

	reasons, diffDS := diffAttributes(addr, def, attrs, actual.Attributes)
	ds.Extend(diffDS)
	if ds.HasErrors() {
		return nil, ds
	}
	// Lifecycle is diffed against rs, the state record, not against actual,
	// the provider observation. Lifecycle is bookkeeping infra attaches to a
	// resource and no provider owns it, so the record is its only source of
	// truth; diffing the observation instead would propose a spurious update
	// on every plan against any provider whose Read forgot to carry it
	// forward. rs is necessarily non-nil here — the branch above returned for
	// every case where it is not.
	//
	// Without this, adding lifecycle to a resource that already exists plans
	// as "no changes", state is never rewritten, and the guard the user just
	// wrote down never takes effect: the same silent failure as a create that
	// never recorded it, one apply later.
	reasons = append(reasons, lifecycleReasons(rc.Lifecycle, rs.Lifecycle)...)
	kind := OpNoOp
	switch {
	case forcesReplacement(reasons):
		kind = OpReplace
	case len(reasons) > 0:
		kind = OpUpdate
	}

	return &Operation{
		Address:   addr,
		Type:      rc.Type,
		Kind:      kind,
		Before:    copyAttrs(actual.Attributes),
		After:     afterAttributes(def, attrs, actual.Attributes, kind),
		Reasons:   reasons,
		Lifecycle: rc.Lifecycle,
	}, ds
}

// removalOperation decides what happens to a resource that is in state but no
// longer in configuration: invariant 1, and spec §11's last two rows.
//
// The lifecycle consulted is the one recorded in state, because the resource
// is by definition no longer in configuration — which is why ResourceState
// records it.
//
// actual is what refresh observed (nil means the provider no longer reports
// it, which is the Forget case below). Before is built from actual, not rs,
// whenever actual is available: rs is the state record, which may be stale
// the moment a resource has drifted, and the in-place diff path a few lines
// up already treats the observation as the source of truth for the same
// reason — Before must not show the plan a value the provider has already
// moved past.
func removalOperation(addr address.Address, rs *resource.ResourceState, actual *resource.ResourceState) (*Operation, diag.Diagnostics) {
	var ds diag.Diagnostics
	if rs == nil {
		return nil, ds
	}

	if actual == nil {
		// Nothing was observed, so the historical record in state is all
		// there is left to show.
		return &Operation{
			Address: addr,
			Type:    rs.Type,
			Kind:    OpForget,
			Before:  copyAttrs(rs.Attributes),
			Reasons: []ChangeReason{{Note: "already absent from the provider; only the state entry remains"}},
		}, ds
	}

	before := copyAttrs(actual.Attributes)

	// retain is checked first. It destroys nothing, so it already satisfies
	// what prevent_destroy protects: a resource carrying both is forgotten,
	// not refused.
	if rs.Lifecycle.Retain {
		return &Operation{
			Address: addr,
			Type:    rs.Type,
			Kind:    OpForget,
			Before:  before,
			Reasons: []ChangeReason{{Note: "retained; removed from state without calling the provider"}},
		}, ds
	}

	if rs.Lifecycle.PreventDestroy {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  addr.String() + " is protected by prevent_destroy but is no longer in configuration",
			Detail: "Removing it from configuration would destroy it. prevent_destroy is refused at plan time " +
				"rather than at apply time so that the refusal arrives before the approval, not after.",
			Action: "Put " + addr.String() + " back in configuration, or set lifecycle.retain to drop it from " +
				"state without destroying it, or clear prevent_destroy if you really mean to destroy it.",
			Related: []address.Address{addr},
		})
		return nil, ds
	}

	return &Operation{
		Address: addr,
		Type:    rs.Type,
		Kind:    OpDestroy,
		Before:  before,
	}, ds
}

// dependentsOf returns the resources that depend on addr, sorted.
//
// The edges come from whichever side actually has them. A resource being
// destroyed or forgotten is no longer in configuration, and neither are some
// of the things that depended on it, so state's Dependencies are the only
// surviving record; reading configuration there would report zero dependents
// for exactly the operation spec §20 wants called out loudly. For every other
// kind the resource is in configuration and DependsOn is the current truth.
//
// It walks cfg.Resources / st.Resources directly — plain Go maps, not the
// pre-sorted .Addresses() helper — and sorts once at the end. Routing through
// .Addresses() first (itself sorted) would make the final address.Sort here
// provably redundant: filtering an already-sorted sequence preserves its
// order regardless of whether this function sorts again, so no fixture could
// ever turn a deleted sort into a failing test. Reading the maps directly
// means Go's randomised map iteration is the only thing standing between this
// slice and nondeterminism, which is what makes the sort below load-bearing
// and testable — see TestDestroyReportsDependentsFromState and
// TestConfiguredOperationsReportDependentsFromConfigSorted.
func dependentsOf(target address.Address, kind OpKind, cfg compiler.ResolvedConfig, st *state.State) []address.Address {
	var out []address.Address
	name := target.String()

	dependsOn := func(edges []address.Address) bool {
		for _, edge := range edges {
			if edge.String() == name {
				return true
			}
		}
		return false
	}

	if kind == OpDestroy || kind == OpForget {
		if st == nil {
			return nil
		}
		for _, rs := range st.Resources {
			if dependsOn(rs.Dependencies) {
				out = append(out, rs.Address)
			}
		}
		address.Sort(out)
		return out
	}

	for _, rc := range cfg.Resources {
		if dependsOn(rc.DependsOn) {
			out = append(out, rc.Address)
		}
	}
	address.Sort(out)
	return out
}

// resolutionNode adapts an address to the graph's Node interface. The graph is
// generic over anything with an ID, and an address is not one; a named type
// here is cheaper than giving address.Address a method it exists only to
// satisfy.
type resolutionNode struct{ addr address.Address }

// ID returns the node's canonical address.
func (n resolutionNode) ID() string { return n.addr.String() }

// resolutionOrder returns every address to decide an operation for, ordered so
// that a configured resource always comes after the resources it depends on.
//
// That order is what lets Compute finish one resource's ${other.attr}
// references against the operation already decided for other: a dependency's
// After is guaranteed to be in scope by the time its dependent is reached.
// Addresses that appear only in state come last, in address order — they are
// removals, so nothing in configuration can reference them and nothing about
// them needs resolving.
//
// A cycle falls back to planAddresses order rather than failing. The compiler
// already rejects a dependency cycle (internal/compiler/validate.go), so this
// is unreachable through the real pipeline; but Compute is a pure function of
// its arguments, not of "the caller went through Compile", and refusing to
// produce a plan at all — or panicking out of graph.Edge — would be a worse
// answer to a malformed input than producing the plan this function produced
// before it ordered anything. Resolution simply leaves the references in the
// cycle unfinished, which is exactly what happened for every reference before
// this existed.
func resolutionOrder(cfg compiler.ResolvedConfig, st *state.State) []address.Address {
	configured := cfg.Addresses()

	g := graph.New[resolutionNode]()
	for _, addr := range configured {
		g.Add(resolutionNode{addr: addr})
	}
	// Edges are recorded in a second pass: graph.Edge panics on an ID that was
	// never added, and a dependency may be seen before the node it names.
	for _, addr := range configured {
		rc, ok := cfg.Get(addr)
		if !ok {
			continue
		}
		for _, dep := range rc.DependsOn {
			if _, inConfig := cfg.Get(dep); !inConfig {
				// A dependency on something outside configuration cannot be
				// resolved against anyway; the compiler reports it.
				continue
			}
			g.Edge(dep.String(), addr.String())
		}
	}

	layers, err := g.Layers()
	if err != nil {
		return planAddresses(cfg, st)
	}

	out := make([]address.Address, 0, len(configured))
	for _, layer := range layers {
		for _, node := range layer {
			out = append(out, node.addr)
		}
	}

	if st != nil {
		inConfig := make(map[string]bool, len(configured))
		for _, addr := range configured {
			inConfig[addr.String()] = true
		}
		var removals []address.Address
		for _, addr := range st.Addresses() {
			if !inConfig[addr.String()] {
				removals = append(removals, addr)
			}
		}
		address.Sort(removals)
		out = append(out, removals...)
	}

	return out
}

// planAddresses returns every address in configuration or state, deduplicated
// and sorted. Operations are ordered by canonical address; execution order
// belongs to the graph.
func planAddresses(cfg compiler.ResolvedConfig, st *state.State) []address.Address {
	seen := map[string]bool{}
	var out []address.Address

	add := func(a address.Address) {
		if !seen[a.String()] {
			seen[a.String()] = true
			out = append(out, a)
		}
	}

	for _, a := range cfg.Addresses() {
		add(a)
	}
	if st != nil {
		for _, a := range st.Addresses() {
			add(a)
		}
	}

	address.Sort(out)
	return out
}

// hashState fingerprints state so that M6 can detect a saved plan going stale.
// State.Encode is byte-stable for identical input, which is what makes the
// fingerprint meaningful.
func hashState(st *state.State) (string, error) {
	data, err := st.Encode()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
