package modules

import (
	"github.com/infrata/infrata/internal/config"
	"github.com/infrata/infrata/pkg/address"
	"github.com/infrata/infrata/pkg/value"
)

// addressIn builds a resource's canonical address from the instantiation path.
//
// Top-down, folding InModule from the innermost path element outward, rather
// than re-rooting a child's results on the way out of the recursion. Re-rooting
// is the tempting alternative and it does not survive Task 7: a collected module
// output is a value.Value carrying an expression tree whose references name
// qualified addresses, so re-rooting a level would have to rewrite every
// address inside every output's tree, at every level it unwinds through.
// Building the full address at instantiation means no address here is ever
// rewritten.
//
// InModule copies the module slice on each call, so the result cannot alias the
// walker's path — which the walker mutates as it pops.
//
// DEPENDS ON a guard stage 5 does not own: Address.String() is only unambiguous
// because a resource's logical name is an identifier. Without that,
// `resources: { module.prod.database: … }` produces a state key identical to
// the canonical address of `database` inside instance `prod` (contract
// Amendment 16a, verified through the binary; Author A owns the check in
// Task 2). Do NOT add a second check here — stage 2 holds the line number, and
// Amendment 13b already ruled on exactly this shape. Recorded so that the
// dependency is visible from both ends: if that guard is ever removed, this is
// what breaks.
func addressIn(module []string, name string) address.Address {
	a := address.Address{Name: name}
	for i := len(module) - 1; i >= 0; i-- {
		a = a.InModule(module[i])
	}
	return a
}

// instantiateDecl copies one resource declaration for one instantiation and
// stamps the module path into every Origin it carries.
//
// A copy, not stage 2's struct: one module file instantiated twice needs two
// declarations with two different Origin.Modules, and stamping the shared struct
// would give both the path of whichever was expanded last. The maps are rebuilt
// because stage 2's are shared for the same reason.
//
// Every AttributeDecl's Origin is stamped too, not just the resource's. A
// diagnostic about one attribute points at that attribute's line, so an
// unstamped attribute Origin names a file and a line inside a module without
// saying which instantiation of it — exactly what Ruling 7 says the module path
// is for.
func instantiateDecl(decl *config.ResourceDecl, module []string) *config.ResourceDecl {
	out := *decl
	out.Origin = originInPath(decl.Origin, module)
	out.DependsOn = append([]string(nil), decl.DependsOn...)

	out.Attributes = make(map[string]config.AttributeDecl, len(decl.Attributes))
	for name, attr := range decl.Attributes {
		attr.Origin = originInPath(attr.Origin, module)
		attr.Value = attr.Value.WithOrigin(originInPath(attr.Value.Origin, module))
		out.Attributes[name] = attr
	}
	return &out
}

// originInPath stamps an origin with the whole instantiation path.
//
// The task brief that specified this package calls for folding a single-level
// value.Origin.InModule here, matching address.Address.InModule — but no such
// method exists: it belongs to a later task in this milestone's execution
// order (this package's own expand.go already documents the same gap for
// diagnostics, in inModulePath, with the same fix deferred the same way).
// Adding it to pkg/value is outside the files this task may touch, so this
// stamps the Module slice directly, which is equivalent for every Origin this
// package ever sees: instantiateDecl's inputs come straight off a fresh
// config.DecodeModule per instantiation, so Origin.Module is always empty
// here, and prepending the whole path once is exactly what folding InModule
// one level at a time from the innermost element outward would produce. If
// this package starts stamping an Origin that already carries a Module path,
// this still does the right thing — it prepends `module`, preserving what was
// already there, same as Address.InModule.
func originInPath(o value.Origin, module []string) value.Origin {
	// Applied outermost-last, because Origin.InModule PREPENDS: walking the
	// path backwards leaves it in the same order as the address it belongs to.
	for i := len(module) - 1; i >= 0; i-- {
		o = o.InModule(module[i])
	}
	return o
}

// attachEdges gives every address in `to` the edges in `edges`.
//
// Both directions of Amendment 8's fan-out land here: a module call's own
// depends_on becomes edges FROM everything it produced, and a sibling's
// depends_on naming the call becomes edges TO everything it produced. Neither
// can be expressed as a bare name, because after expansion nothing is addressed
// with the call's name.
func (w *walker) attachEdges(to []address.Address, edges []address.Address) {
	if len(to) == 0 || len(edges) == 0 {
		return
	}
	in := make(map[string]bool, len(to))
	for _, a := range to {
		in[a.String()] = true
	}
	for i := range w.instances {
		if in[w.instances[i].Address.String()] {
			w.instances[i].ExtraDeps = append(w.instances[i].ExtraDeps, edges...)
		}
	}
}

// sortAddresses orders edges so the dependency graph is identical on every run.
// Fan-out reads from a map, and invariant 6 requires the same configuration to
// produce the same edges in the same order.
func sortAddresses(a []address.Address) []address.Address {
	address.Sort(a)
	return a
}
