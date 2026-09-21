package planner

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/infrena/infrena/internal/compiler"
	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/internal/expressions"
	"github.com/infrena/infrena/internal/graph"
	"github.com/infrena/infrena/internal/refresh"
	"github.com/infrena/infrena/internal/registry"
	"github.com/infrena/infrena/internal/state"
	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/resource"
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
	// ForceNew attribute from an updatable one, and a computed attribute
	// from desired state. Resolved configuration carries values, not the
	// schema that governs them. A nil Registry is an error, never a
	// degradation.
	Registry *registry.Registry
}

// Compute decides one operation per resource by comparing resolved
// configuration against recorded state and observed provider reality.
//
// It is a pure function of its inputs: no filesystem, no network, no
// provider. The returned plan is always non-nil, even when the diagnostics
// contain errors, so the caller can render both together. A plan carrying an
// error is never applyable.
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

	// An environment mismatch stops planning before a single operation is
	// decided. The diagnostic alone is not enough: diffing this configuration
	// against another environment's state produces a complete, well-formed,
	// savable list of operations that destroys everything in one environment
	// and creates everything in the other. A plan gets written to a file and
	// passed around, so "only dangerous if someone ignores the diagnostics"
	// is not a property to rely on here. Returning no operations makes the
	// dangerous plan impossible to produce rather than merely impolite to
	// use.
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
	// deciding one resource's operation can require another's answer: a
	// reference to another resource's attribute is knowable only once the
	// plan knows what that attribute will be after apply. scope accumulates
	// each decided operation's After, and dependency order is what guarantees
	// a dependency's After is already in it. The operations are sorted by
	// address afterwards, so the plan's ordering contract is unchanged.
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

	// Emitted in planAddresses order rather than by re-sorting the pass
	// above, so the plan's address ordering and the order diagnostics are
	// read in both come from one helper. The two orderings cover the same
	// addresses; only the sequence differs.
	for _, addr := range planAddresses(cfg, st) {
		ds.Extend(reported[addr.String()])
		if op, ok := decided[addr.String()]; ok {
			p.Operations = append(p.Operations, op)
		}
	}

	// Deposed objects, after the ordinary operations and in address order.
	//
	// Nothing else in the plan would mention one: the address is present,
	// healthy and matching configuration, so it decides as a no-op while
	// something real goes on existing and being billed for. Emitting it here
	// is what makes a deposed record a step in a process rather than a place
	// leaks accumulate — every plan proposes the cleanup until it works.
	if st != nil {
		for _, addr := range sortedStateAddresses(st) {
			rs, ok := st.Get(addr)
			if !ok || len(rs.Deposed) == 0 {
				continue
			}
			for _, d := range rs.Deposed {
				p.Operations = append(p.Operations, Operation{
					Address:  addr,
					Type:     rs.Type,
					Provider: rs.Provider,
					Kind:     OpDestroyDeposed,
					Before:   copyAttrs(d.Attributes),
					Reasons: []ChangeReason{{Note: "left over from an interrupted `create_before_destroy` replacement (" +
						d.ProviderID + ")"}},
				})
			}
		}
	}

	p.Diagnostics = append([]diag.Diagnostic(nil), ds...)
	return p, ds
}

// sortedStateAddresses lists every address in state, in canonical order, so a
// deposed cleanup appears in the same place on every run.
func sortedStateAddresses(st *state.State) []address.Address {
	out := make([]address.Address, 0, len(st.Resources))
	for _, rs := range st.Resources {
		out = append(out, rs.Address)
	}
	address.Sort(out)
	return out
}

// operationFor decides the single operation for one address. It returns nil
// when planning for that resource failed, in which case it has reported why.
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
		op, removalDS := removalOperation(addr, rs, actual, cfg.Protections, cfg.Environment)
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
	// reference to another resource is necessarily unknown by then. Planning
	// is the first phase that knows what the referenced resource will look
	// like, so it is the phase that finishes those references. Without this
	// the diff compares a real recorded value against a permanently unknown
	// desired one and proposes the same update forever.
	//
	// Only a reference whose target is already decided and whose attribute is
	// already known resolves here. Everything else is left as the compiler
	// bound it, still carrying its expression, so a genuinely unknowable
	// value still renders as "known after apply" and reaches the executor
	// intact.
	attrs, _, resolveDS := expressions.ResolveDeferred(rc.Attrs, scope)
	ds.Extend(resolveDS)
	if ds.HasErrors() {
		return nil, ds
	}

	if !inState || actual == nil {
		var reasons []ChangeReason
		if inState {
			// Recorded, but the provider no longer has it: something deleted
			// it outside infrena, so the plan recreates it.
			reasons = []ChangeReason{{Note: "the provider no longer reports this resource; it will be recreated"}}
		}
		return &Operation{
			Address:   addr,
			Type:      rc.Type,
			Provider:  rc.Provider,
			Kind:      OpCreate,
			After:     afterAttributes(def, attrs, nil, OpCreate, rc.Lifecycle.IgnoreChanges),
			Reasons:   reasons,
			Lifecycle: rc.Lifecycle,
			DependsOn: append([]address.Address(nil), rc.DependsOn...),
		}, ds
	}

	reasons, diffDS := diffAttributes(addr, def, attrs, actual.Attributes, rc.Lifecycle.IgnoreChanges)
	ds.Extend(diffDS)
	if ds.HasErrors() {
		return nil, ds
	}
	// Lifecycle is diffed against the state record, not the provider
	// observation. It is bookkeeping no provider owns, so the record is its
	// only source of truth; diffing the observation would propose a spurious
	// update on every plan against a provider whose Read did not carry it
	// forward.
	//
	// Without this, adding a lifecycle setting to an existing resource plans
	// as no change, state is never rewritten, and the guard the user just
	// wrote down never takes effect.
	reasons = append(reasons, lifecycleReasons(rc.Lifecycle, rs.Lifecycle)...)
	// Dependency edges, for the same reason and against the same source: a
	// depends_on-only change would otherwise plan as no change, nothing would
	// be written, and the new edge would never reach state.
	reasons = append(reasons, dependencyReasons(rc.DependsOn, rs.Dependencies)...)
	kind := OpNoOp
	switch {
	case forcesReplacement(reasons):
		if rc.Lifecycle.PreventReplace {
			// Refused at plan time, like every other lifecycle guard, so the
			// refusal arrives before the approval rather than after it.
			//
			// The causing attributes are named because this is the harder
			// refusal to act on: the resource is still in configuration and
			// the diff reads as an edit, so without them the reader has to
			// work out why an update became a replacement.
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  addr.String() + " would be replaced, and it sets `lifecycle.prevent_replace`",
				Detail: "Replacing it destroys the existing resource and creates a new one, so anything " +
					"it holds is lost. Forced by: " + replacementCauses(reasons) + ".",
				Action: "Revert " + replacementCauses(reasons) + " to the recorded value, or clear " +
					"`lifecycle.prevent_replace` on " + addr.String() + " if losing and recreating it is " +
					"what you mean.",
				Related: []address.Address{addr},
			})
			return nil, ds
		}
		kind = OpReplace
	case len(reasons) > 0:
		kind = OpUpdate
	}

	return &Operation{
		Address:   addr,
		Type:      rc.Type,
		Provider:  rc.Provider,
		Kind:      kind,
		Before:    copyAttrs(actual.Attributes),
		After:     afterAttributes(def, attrs, actual.Attributes, kind, rc.Lifecycle.IgnoreChanges),
		Reasons:   reasons,
		Lifecycle: rc.Lifecycle,
		DependsOn: append([]address.Address(nil), rc.DependsOn...),
	}, ds
}

// removalOperation decides what happens to a resource that is in state but no
// longer in configuration.
//
// The lifecycle consulted is the one recorded in state, because the resource
// is by definition no longer in configuration.
//
// actual is what refresh observed; nil means the provider no longer reports
// it, which is the forget case below. Before is built from the observation
// wherever there is one, since the state record may be stale the moment a
// resource has drifted, and Before must not show a value the provider has
// already moved past.
func removalOperation(
	addr address.Address, rs *resource.ResourceState, actual *resource.ResourceState,
	protections compiler.Protections, environment string,
) (*Operation, diag.Diagnostics) {
	var ds diag.Diagnostics
	if rs == nil {
		return nil, ds
	}

	if actual == nil {
		// Nothing was observed, so the record in state is all there is left
		// to show.
		//
		// Provider is set even though a forget never calls one, because the
		// operation is written into the plan artifact and a record of a
		// resource leaving state should say which account it was in.
		return &Operation{
			Address:  addr,
			Type:     rs.Type,
			Provider: rs.Provider,
			Kind:     OpForget,
			Before:   copyAttrs(rs.Attributes),
			Reasons:  []ChangeReason{{Note: "already absent from the provider; only the state entry remains"}},
		}, ds
	}

	before := copyAttrs(actual.Attributes)

	// retain is checked first. It destroys nothing, so it already satisfies
	// what prevent_destroy protects: a resource carrying both is forgotten,
	// not refused.
	if rs.Lifecycle.Retain {
		return &Operation{
			Address:  addr,
			Type:     rs.Type,
			Provider: rs.Provider,
			Kind:     OpForget,
			Before:   before,
			Reasons:  []ChangeReason{{Note: "retained; removed from state without calling the provider"}},
		}, ds
	}

	if protections.PreventDestroy {
		// The environment's own prevent_destroy, meaning what its
		// resource-level namesake does: you may not destroy something by
		// deleting it from configuration. It deliberately does not refuse a
		// replace, even though a replace destroys and recreates — refusing
		// those would make any immutable attribute unchangeable in a
		// protected environment, a far larger restriction than anyone asking
		// for destroy protection has in mind.
		where := "environment " + strconv.Quote(environment)
		if protections.PreventDestroyFrom != "" && protections.PreventDestroyFrom != environment {
			where += " (inherited from " + strconv.Quote(protections.PreventDestroyFrom) + ")"
		}
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  addr.String() + " would be destroyed, and " + where + " sets `prevent_destroy`",
			Detail: "It is recorded in state and is no longer in configuration, so applying this would " +
				"destroy it. Refused at plan time rather than at apply time so the refusal arrives " +
				"before the approval, not after.",
			// The environment-level fix leads, because it is the only one
			// right whichever command arrived here. An apply reaches this by a
			// resource being deleted from configuration, where putting it back
			// is the likely fix; a destroy reaches it with an empty
			// configuration by premise, where that advice describes a state of
			// affairs that does not exist.
			Action: "Clear `prevent_destroy` on " + where + " if you really mean to destroy things " +
				"there. If instead " + addr.String() + " left configuration by accident, put it back; " +
				"or set lifecycle.retain on it to drop it from state without destroying it.",
			Related: []address.Address{addr},
		})
		return nil, ds
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
		// From state: a destroy has no configuration, the resource having
		// been deleted from the file, so state is the only thing that can say
		// which account to delete it from. Anywhere else means deleting from
		// whichever instance happened to be consulted.
		Provider: rs.Provider,
		Kind:     OpDestroy,
		Before:   before,
	}, ds
}

// dependentsOf returns the resources that depend on addr, sorted.
//
// The edges come from whichever side actually has them. A resource being
// destroyed or forgotten is no longer in configuration, and neither are some
// of the things that depended on it, so state's Dependencies are the only
// surviving record; reading configuration there would report no dependents
// for exactly the operation most worth calling out loudly. For every other
// kind the resource is in configuration and its DependsOn is current.
//
// It reads the resource maps directly rather than the pre-sorted address
// helpers, and sorts once at the end, so that the sort below is what makes
// the result deterministic and can be tested as such.
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

// resolutionNode adapts an address to the graph's Node interface, which is
// cheaper than giving address.Address a method it exists only to satisfy.
type resolutionNode struct{ addr address.Address }

// ID returns the node's canonical address.
func (n resolutionNode) ID() string { return n.addr.String() }

// resolutionOrder returns every address to decide an operation for, ordered so
// that a configured resource always comes after the resources it depends on.
//
// That order is what lets one resource's references be finished against the
// operation already decided for its dependency: the dependency's After is
// guaranteed to be in scope by the time its dependent is reached. Addresses
// appearing only in state come last, in address order — they are removals, so
// nothing in configuration can reference them.
//
// A cycle falls back to address order rather than failing. The compiler
// already rejects dependency cycles, so this is unreachable through the real
// pipeline, but Compute is a pure function of its arguments rather than of
// "the caller compiled first", and refusing to plan at all — or panicking out
// of graph.Edge — is a worse answer to a malformed input. References inside
// the cycle are simply left unfinished.
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

// HashState fingerprints a state file, so a command applying a saved plan can
// ask whether the state has moved since the plan was made without
// reimplementing the hash. Two implementations of one fingerprint would
// disagree eventually, and the disagreement would read as "the state moved"
// on a state that had not.
//
// State encoding is byte-stable for identical input, which is what makes the
// fingerprint meaningful.
func HashState(st *state.State) (string, error) { return hashState(st) }

func hashState(st *state.State) (string, error) {
	data, err := st.Encode()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// replacementCauses lists the attributes that forced a replacement, for a
// diagnostic that has to say why an update became one.
func replacementCauses(reasons []ChangeReason) string {
	var names []string
	for _, r := range reasons {
		if r.ForceNew && r.Attribute != "" {
			names = append(names, r.Attribute)
		}
	}
	if len(names) == 0 {
		// Defensive: forcesReplacement said yes, so at least one reason forces
		// it. Saying "an attribute" beats an empty list if that ever changes.
		return "an attribute the provider marks as forcing replacement"
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
