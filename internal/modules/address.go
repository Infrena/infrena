package modules

import (
	"slices"
	"strconv"

	"github.com/infrena/infrena/internal/config"
	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/value"
)

// addressIn builds a resource's canonical address from the instantiation path.
//
// Built top-down at instantiation rather than by re-rooting a child's results on
// the way out of the recursion. A collected module output carries an expression
// tree whose references name qualified addresses, so re-rooting one level would
// have to rewrite every address inside every output's tree at every level it
// unwinds through. Built this way, no address is ever rewritten.
//
// InModule copies the module slice on each call, so the result cannot alias the
// walker's path, which the walker mutates as it pops.
//
// This relies on a guard elsewhere: Address.String() is only unambiguous
// because a resource's logical name is validated as an identifier. Without that
// check, `resources: { module.prod.database: … }` produces a state key
// identical to the canonical address of `database` inside instance `prod`.
// Do not add a second check here — config decoding holds the line number and is
// where the diagnostic belongs.
func addressIn(module []string, name string) address.Address {
	a := address.Address{Name: name}
	for _, m := range slices.Backward(module) {
		a = a.InModule(m)
	}
	return a
}

// instantiateDecl copies one resource declaration for one instantiation and
// stamps the module path into every Origin it carries.
//
// A copy, not the decoded struct: one module file instantiated twice needs two
// declarations with two different Origin.Modules, and stamping the shared struct
// would give both the path of whichever was expanded last. The maps are rebuilt
// because the decoded ones are shared for the same reason.
//
// Every AttributeDecl's Origin is stamped too, not just the resource's, so that
// a diagnostic about one attribute says which instantiation it came from rather
// than only a file and line inside the module.
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

// originInPath stamps an origin with the whole instantiation path, preserving
// any module path the origin already carries.
func originInPath(o value.Origin, module []string) value.Origin {
	// Applied outermost-last, because Origin.InModule prepends: walking the
	// path backwards leaves it in the same order as the address it belongs to.
	for _, m := range slices.Backward(module) {
		o = o.InModule(m)
	}
	return o
}

// attachEdges gives every address in `to` the edges in `edges`.
//
// Both directions of a module call's fan-out land here: the call's own
// depends_on becomes edges from everything it produced, and a sibling's
// depends_on naming the call becomes edges to everything it produced. Neither
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
// Fan-out reads from a map, and the same configuration must produce the same
// edges in the same order.
func sortAddresses(a []address.Address) []address.Address {
	address.Sort(a)
	return a
}

// keyedCallName renders one instance of a `for_each` module call as the name of
// a module level: `store["orders"]`.
//
// Deliberately the same spelling address.Address.String() gives a keyed
// resource: a user reading `module.store["orders"].db` in a plan should not have
// to learn that the brackets mean something different one segment to the left.
// Quoted because a key is user data and may contain a dot, which a dotted form
// would confuse with the module path separator.
func keyedCallName(name, key string) string {
	return name + "[" + strconv.Quote(key) + "]"
}
