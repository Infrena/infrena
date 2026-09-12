package providers

import (
	"strconv"

	"github.com/infrata/infrata/internal/config"
	"github.com/infrata/infrata/internal/diag"
	"github.com/infrata/infrata/internal/registry"
	"github.com/infrata/infrata/internal/variables"
	"github.com/infrata/infrata/pkg/value"
)

// Prepare resolves the `providers:` block and constructs one provider per
// instance, registering each in reg.
//
// This is the seam the factory split exists for. reg arrives holding SCHEMAS ONLY
// — enough for the compiler to have resolved the variables in scope — and leaves
// holding provider objects built from those resolved values. Which is what makes
// `cloud: ${path}` choose the file a provider opens, rather than merely resolving
// to a string nothing reads.
func Prepare(
	decls []config.ProviderDecl, scope variables.Scope, reg *registry.Registry,
) (Table, diag.Diagnostics) {
	table, ds := Resolve(decls, scope)
	if ds.HasErrors() {
		// Registering an instance whose configuration did not resolve would build
		// a provider from values nobody chose. The diagnostics already name what
		// could not be resolved; a second failure from the plugin about the same
		// value says the same thing worse.
		return table, ds
	}
	if len(table) == 0 {
		table = Implicit(reg)
	}
	ds.Extend(Register(table, reg))
	return table, ds
}

// Implicit is the instance table of a project with no `providers:` block.
//
// Every project written before §12.1 existed is this one, and every one of them
// must keep working — so the absence of the block means ONE instance, named after
// the only thing it could be. Which is also what a state file written then already
// records, so nothing needs migrating.
//
// "The only thing it could be" is a registered plugin when there is exactly one,
// and otherwise an already-registered instance when there is exactly one of those
// — the shape a caller that built its own provider object leaves behind. Two
// candidates is not a default to guess: a resource would land in an account nobody
// named, and the fix (`providers:`, or `provider:` on the resource) is the user's
// to choose. Zero candidates is a registry that dispatches nothing, which the
// unknown-type diagnostic reports first and more clearly.
func Implicit(reg *registry.Registry) Table {
	name := ""
	switch factories := reg.Factories(); {
	case len(factories) == 1:
		name = factories[0]
	case len(factories) > 1:
		return nil
	default:
		if names := reg.InstanceNames(); len(names) == 1 {
			name = names[0]
		}
	}
	if name == "" {
		return nil
	}
	return Table{name: Instance{Name: name, Plugin: name, Default: true}}
}

// Register constructs every instance the table names.
//
// SORTED, so two bad instances are reported in the same order every run
// (invariant 6 applies to diagnostics as much as to plans).
func Register(table Table, reg *registry.Registry) diag.Diagnostics {
	var ds diag.Diagnostics
	for _, name := range table.Names() {
		inst := table[name]
		if reg.HasInstance(name) {
			// The caller supplied this provider object itself, which is what every
			// test that builds a stub does. Rebuilding it from configuration would
			// throw that object away and replace it with a different one — so the
			// caller's wins, and nothing in the production path reaches here
			// because internal/cli registers plugins only.
			continue
		}
		if err := reg.RegisterInstance(name, inst.Plugin, inst.Config); err != nil {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "provider instance " + strconv.Quote(name) + " could not be configured",
				Detail:   err.Error(),
				Action:   correctionFor(name, inst.Origin),
				Origin:   inst.Origin,
			})
		}
	}
	return ds
}

// correctionFor says what to fix, without repeating the location the diagnostic
// already renders above it.
//
// The implicit instance is the case worth spelling out: there is no `providers:` entry
// to correct, so a reader told to correct one goes looking for a block that is not
// there.
func correctionFor(name string, o value.Origin) string {
	if o.File == "" {
		return "This project declares no `providers:` block, so it has one implicit instance " +
			"named " + strconv.Quote(name) + ". Declare the block to configure it."
	}
	return "Correct the `providers:` entry for " + strconv.Quote(name) + "."
}
