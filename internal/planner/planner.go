package planner

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"infra/internal/compiler"
	"infra/internal/diag"
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

	for _, addr := range planAddresses(cfg, st) {
		op, opDS := operationFor(addr, cfg, st, obs, opts)
		ds.Extend(opDS)
		if op != nil {
			// Which side of the graph the edges come from depends on the
			// operation, so this runs once the kind has been decided.
			op.Dependents = dependentsOf(addr, op.Kind, cfg, st)
			p.Operations = append(p.Operations, *op)
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
		op, removalDS := removalOperation(addr, rs, actual != nil)
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

	if !inState || actual == nil {
		var reasons []ChangeReason
		if inState {
			// Recorded, but the provider no longer has it: something deleted
			// it outside infra, so the plan recreates it.
			reasons = []ChangeReason{{Note: "the provider no longer reports this resource; it will be recreated"}}
		}
		return &Operation{
			Address: addr,
			Type:    rc.Type,
			Kind:    OpCreate,
			After:   afterAttributes(def, rc.Attrs, nil, OpCreate),
			Reasons: reasons,
		}, ds
	}

	reasons := diffAttributes(def, rc.Attrs, actual.Attributes)
	kind := OpNoOp
	switch {
	case forcesReplacement(reasons):
		kind = OpReplace
	case len(reasons) > 0:
		kind = OpUpdate
	}

	return &Operation{
		Address: addr,
		Type:    rc.Type,
		Kind:    kind,
		Before:  copyAttrs(actual.Attributes),
		After:   afterAttributes(def, rc.Attrs, actual.Attributes, kind),
		Reasons: reasons,
	}, ds
}

// removalOperation decides what happens to a resource that is in state but no
// longer in configuration: invariant 1, and spec §11's last two rows.
//
// The lifecycle consulted is the one recorded in state, because the resource
// is by definition no longer in configuration — which is why ResourceState
// records it.
func removalOperation(addr address.Address, rs *resource.ResourceState, present bool) (*Operation, diag.Diagnostics) {
	var ds diag.Diagnostics
	if rs == nil {
		return nil, ds
	}
	before := copyAttrs(rs.Attributes)

	if !present {
		return &Operation{
			Address: addr,
			Type:    rs.Type,
			Kind:    OpForget,
			Before:  before,
			Reasons: []ChangeReason{{Note: "already absent from the provider; only the state entry remains"}},
		}, ds
	}

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
		for _, candidate := range st.Addresses() {
			rs, ok := st.Get(candidate)
			if ok && dependsOn(rs.Dependencies) {
				out = append(out, candidate)
			}
		}
		address.Sort(out)
		return out
	}

	for _, candidate := range cfg.Addresses() {
		rc, ok := cfg.Get(candidate)
		if ok && dependsOn(rc.DependsOn) {
			out = append(out, candidate)
		}
	}
	address.Sort(out)
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
