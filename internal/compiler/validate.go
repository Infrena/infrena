package compiler

import (
	"strconv"
	"strings"

	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/internal/graph"
	"github.com/infrena/infrena/internal/registry"
	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/value"
)

// validateGraph is compiler stage 8. It runs once schema binding has
// confirmed every resource's type, kinds and required attributes, and checks
// what only the whole graph can reveal: dependency cycles, infrastructure a
// resource needs but configuration never supplies, and lifecycle settings
// that contradict each other. None of these checks contact a provider.
func validateGraph(cfg *ResolvedConfig, reg *registry.Registry) diag.Diagnostics {
	var ds diag.Diagnostics

	if cycle := cycleFor(cfg); cycle != nil {
		ds.Add(cycleDiagnostic(cfg, cycle))
	}

	checkRequirements(cfg, reg, &ds)
	checkLifecycle(cfg, &ds)

	return ds
}

// --- dependency cycles ------------------------------------------------

// cycleFor reports a dependency cycle in the resolved configuration, or nil.
// Detection lives in internal/graph so the compiler and the executor cannot
// disagree about what a cycle is.
//
// Edges run resource → dependency, the direction the diagnostic reads in ("a → b"
// means a depends on b), which is the opposite of the planner's graph, where an
// edge means "must execute first". Both orientations detect the same cycles; only
// this one prints in the order the message claims.
//
// graph.Cycle returns members without repeating the first at the end. The closing
// repeat is re-added here because cycleDiagnostic slices cycle[1:len(cycle)-1]
// for its Related list and would otherwise drop a real member of the cycle.
func cycleFor(cfg *ResolvedConfig) []address.Address {
	g := graph.New[addrNode]()
	for _, addr := range cfg.Addresses() {
		g.Add(addrNode{addr})
	}
	for _, addr := range cfg.Addresses() {
		r := cfg.Resources[addr.String()]
		for _, dep := range r.DependsOn {
			if _, ok := cfg.Get(dep); !ok {
				continue // not a node in this graph; stage 6 already reported it
			}
			g.Edge(addr.String(), dep.String())
		}
	}

	ids := g.Cycle()
	if len(ids) == 0 {
		return nil
	}

	out := make([]address.Address, 0, len(ids)+1)
	for _, id := range ids {
		addr, err := address.Parse(id)
		if err != nil {
			// Impossible: every id came from an Address added above.
			panic("compiler: graph returned an unparseable address " + strconv.Quote(id) + ": " + err.Error())
		}
		out = append(out, addr)
	}
	return append(out, out[0])
}

// addrNode adapts an address to the graph's Node interface.
type addrNode struct{ addr address.Address }

func (n addrNode) ID() string { return n.addr.String() }

// cycleKey renders a closed cycle as "a → b → c → a", used both as the
// diagnostic text and as the deduplication key.
func cycleKey(cycle []address.Address) string {
	parts := make([]string, len(cycle))
	for i, a := range cycle {
		parts[i] = a.String()
	}
	return strings.Join(parts, " → ")
}

// cycleDiagnostic renders one closed cycle as an error diagnostic, pointing
// at the origin of the first resource on it and naming the rest as related.
func cycleDiagnostic(cfg *ResolvedConfig, cycle []address.Address) diag.Diagnostic {
	origin := value.Origin{}
	if r, ok := cfg.Get(cycle[0]); ok {
		origin = r.Origin
	}
	return diag.Diagnostic{
		Severity: diag.SeverityError,
		Summary:  "dependency cycle: " + cycleKey(cycle),
		Detail:   "Each resource in the cycle depends, directly or indirectly, on itself through the others, so none of them could ever be created first.",
		Action:   "Break the cycle by removing one dependency, or restructuring the resources so it is not required.",
		Origin:   origin,
		Related:  cycle[1 : len(cycle)-1],
	}
}

// --- requirements -------------------------------------------------------

// checkRequirements reports a non-optional requirement that nothing in
// configuration satisfies, before any provider is called — "an application needs
// a database", rather than an opaque failure once a provider API is finally asked
// to create it.
//
// Satisfaction is existence within the same provider instance, not a traced
// reference from the resource that declares the requirement: a Requirement names
// types and carries no attribute name to trace against. Per instance because two
// instances are two accounts, and a database in one account is not served by a
// network in another.
//
// This is a pre-flight hint, not a correctness mechanism, and has two known
// limits. State is not consulted, so a project whose VPC is adopted rather than
// declared reports a requirement it does in fact meet — `validate` contacts
// nothing by design. And an attribute like `region` is not compared, so a subnet
// requiring a VPC is satisfied by a VPC in any region of the same account. What
// actually ties a subnet to one VPC is the reference `${vpc.id}`, checked by
// stages 6 and 7. Both limits would be answered by letting a Requirement name the
// attribute that satisfies it.
func checkRequirements(cfg *ResolvedConfig, reg *registry.Registry, ds *diag.Diagnostics) {
	// Keyed by instance and type. Every resource carries an instance name by the
	// time this runs — a project declaring no `providers:` block gets one
	// implicit instance and every resource in it names that.
	present := map[held]bool{}
	for _, r := range cfg.Resources {
		present[held{r.Provider, r.Type}] = true
	}

	for _, addr := range cfg.Addresses() {
		r := cfg.Resources[addr.String()]

		def, ok := reg.Definition(r.Type)
		if !ok {
			// An unresolved type is stage 7's problem. Compile does not reach
			// this stage until stage 7 is clean; a direct caller of
			// validateGraph passing an unresolved type gets no further noise.
			continue
		}

		for _, req := range def.Requirements {
			if req.Optional || satisfiesAny(present, r.Provider, req.Types) {
				continue
			}
			// The instance is named whenever there is one to name. Without it the
			// message describes a configuration the reader can see does contain a
			// resource of that type, and reads as a bug in the tool rather than a
			// gap in their project.
			where := " to this configuration."
			detail := req.Description + "\nSatisfied by a resource of type: " + strings.Join(req.Types, ", ")
			if r.Provider != "" {
				where = " to provider instance " + strconv.Quote(r.Provider) + "."
				detail += "\nIt must belong to the same provider instance, " +
					strconv.Quote(r.Provider) + ": another instance is another account, " +
					"and resources in one cannot reach the other."
			}
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  strconv.Quote(addr.String()) + " is missing required " + req.Name,
				Detail:   detail,
				Action:   "Add a resource of type " + strings.Join(req.Types, " or ") + where,
				Origin:   r.Origin,
			})
		}
	}
}

// held is one resource type present in one provider instance's account. The pair
// is the unit of satisfaction: a type alone says nothing about whether the
// resource declaring the requirement can reach it.
type held struct{ instance, typ string }

// satisfiesAny reports whether any of types is present in instance's account.
func satisfiesAny(present map[held]bool, instance string, types []string) bool {
	for _, t := range types {
		if present[held{instance, t}] {
			return true
		}
	}
	return false
}

// --- lifecycle ------------------------------------------------------------

// checkLifecycle rejects lifecycle settings that contradict each other.
// prevent_destroy refuses to destroy the resource; retain abandons it from
// state without destroying it — a resource cannot mean both at once.
func checkLifecycle(cfg *ResolvedConfig, ds *diag.Diagnostics) {
	for _, addr := range cfg.Addresses() {
		r := cfg.Resources[addr.String()]
		if r.Lifecycle.PreventDestroy && r.Lifecycle.Retain {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  strconv.Quote(addr.String()) + " sets both prevent_destroy and retain",
				Detail:   "prevent_destroy refuses to destroy the resource; retain abandons it from state without destroying it. Setting both is a contradiction.",
				Action:   "Choose one: prevent_destroy to keep infrena managing it and refuse destruction, or retain to let infrena forget it without deleting it.",
				Origin:   r.Origin,
			})
		}
	}
}
