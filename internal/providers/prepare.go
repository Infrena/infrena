package providers

import (
	"sort"
	"strconv"
	"strings"

	"github.com/infrata/infrata/internal/config"
	"github.com/infrata/infrata/internal/diag"
	"github.com/infrata/infrata/internal/registry"
	"github.com/infrata/infrata/internal/variables"
	"github.com/infrata/infrata/pkg/schema"
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
		// An instance the caller already supplied a provider object for keeps it —
		// which is what every test that builds a stub relies on. Rebuilding it from
		// configuration would throw that object away and replace it with a different
		// one. Nothing in the production path reaches this, because internal/cli
		// registers plugins only.
		if !reg.HasInstance(name) {
			if err := reg.RegisterInstance(name, inst.Plugin, inst.Config); err != nil {
				ds.Add(diag.Diagnostic{
					Severity: diag.SeverityError,
					Summary:  "provider instance " + strconv.Quote(name) + " could not be configured",
					Detail:   err.Error(),
					Action:   correctionFor(name, inst.Origin),
					Origin:   inst.Origin,
				})
				// Nothing further about an instance that does not exist: reporting a
				// `defaults:` key against a provider the user is about to reconfigure
				// stacks a second problem on the first.
				continue
			}
		}
		// OUTSIDE the construction branch. `defaults:` is checked against the
		// SCHEMAS, which have nothing to do with who built the provider object — and
		// skipping the check for a pre-registered instance would leave every
		// compiler-level test exercising the feature with the check switched off.
		ds.Extend(checkDefaults(inst, reg))
	}
	return ds
}

// checkDefaults refuses a `defaults:` key no resource type of that plugin declares.
//
// FAIL CLOSED, and this is the key case where that matters most: `tag:` for `tags:`
// applies to nothing, in every environment, forever, and there is no output in which
// its absence is visible. A plan looks right, an apply succeeds, and the tag is
// simply never there.
//
// The union across the plugin's types, not the intersection: `defaults:` applies to
// every resource that uses the instance, and a key only some types declare is the
// normal case — `tags:` on everything taggable, ignored by the rest.
func checkDefaults(inst Instance, reg *registry.Registry) diag.Diagnostics {
	var ds diag.Diagnostics
	if len(inst.Defaults) == 0 {
		return ds
	}

	// Every attribute the instance's types declare, by name, collecting EVERY
	// declaration of each: two types may declare one name, and a key is usable if it
	// fits any of them.
	declared := map[string][]schema.Attribute{}
	for _, t := range reg.TypesOf(inst.Name) {
		def, ok := reg.Definition(t)
		if !ok {
			continue
		}
		for name, attr := range def.Attributes {
			declared[name] = append(declared[name], attr)
		}
	}

	keys := make([]string, 0, len(inst.Defaults))
	for k := range inst.Defaults {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		v := inst.Defaults[key]
		if isReserved(key) {
			// Accepted for every resource and declared by no schema — the engine owns
			// these, which is why registry refuses a plugin attribute that collides.
			if v.Kind != value.KindBool {
				ds.Add(diag.Diagnostic{
					Severity: diag.SeverityError,
					Summary: "provider instance " + strconv.Quote(inst.Name) + " defaults " +
						strconv.Quote(key) + " to a " + v.Kind.String() + ", which must be boolean",
					Detail: strconv.Quote(key) + " is a lifecycle option, and every resource " +
						"accepts it as true or false.",
					Action: "Write `" + key + ": true` or `" + key + ": false`.",
					Origin: inst.Origin,
				})
			}
			continue
		}

		attrs, known := declared[key]
		if !known {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary: "provider instance " + strconv.Quote(inst.Name) + " defaults " +
					strconv.Quote(key) + ", which no resource it serves accepts",
				Detail: "A `defaults:` key must be an attribute some resource type of plugin " +
					inst.Plugin + " declares, or a lifecycle option.\nAccepted here:\n  " +
					strings.Join(acceptedKeys(declared), "\n  "),
				Action: "Correct the key, or remove it: as written it would apply to nothing, " +
					"in every environment, with no output in which its absence is visible.",
				Origin: inst.Origin,
			})
			continue
		}
		ds.Extend(checkDefaultFits(inst, key, v, attrs))
	}
	return ds
}

// checkDefaultFits reports a `defaults:` value that no declaration of its name could
// accept — one every type marks computed, or one of the wrong kind everywhere.
//
// ANY declaration accepting it is enough. `defaults:` applies to every resource that
// uses the instance, and a key only some of its types declare is the ordinary case:
// `tags:` on everything taggable, absent from the rest. Stage 7 then fills it in
// exactly where it fits, so the types that do not declare it are not a problem to
// report — only a key that fits nothing at all is.
func checkDefaultFits(inst Instance, key string, v value.Value, attrs []schema.Attribute) diag.Diagnostics {
	var ds diag.Diagnostics
	settable, kindFits := false, false
	kinds := map[string]bool{}
	for _, a := range attrs {
		kinds[a.Kind.String()] = true
		if a.Computed {
			continue
		}
		settable = true
		if a.Kind == v.Kind {
			kindFits = true
		}
	}

	switch {
	case !settable:
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary: "provider instance " + strconv.Quote(inst.Name) + " defaults " +
				strconv.Quote(key) + ", which is computed and cannot be set",
			Detail: "A computed attribute is reported by the provider after the resource " +
				"exists, so nothing can supply it in advance.",
			Action: "Remove " + strconv.Quote(key) + " from `defaults:`.",
			Origin: inst.Origin,
		})
	case !kindFits:
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary: "provider instance " + strconv.Quote(inst.Name) + " defaults " +
				strconv.Quote(key) + " to a " + v.Kind.String() + ", which no resource accepts",
			Detail: strconv.Quote(key) + " is declared as " + strings.Join(sortedKeys(kinds), " or ") +
				" by the resource types this instance serves.",
			Action: "Correct the value's type.",
			Origin: inst.Origin,
		})
	}
	return ds
}

// isReserved reports whether a `defaults:` key is a lifecycle option.
func isReserved(key string) bool {
	for _, name := range registry.ReservedAttributes {
		if key == name {
			return true
		}
	}
	return false
}

// acceptedKeys lists what a `defaults:` key may be, schema attributes and lifecycle
// options together, because from where the user is sitting they are one list.
func acceptedKeys(declared map[string][]schema.Attribute) []string {
	out := make([]string, 0, len(declared)+len(registry.ReservedAttributes))
	for k := range declared {
		out = append(out, k)
	}
	out = append(out, registry.ReservedAttributes...)
	sort.Strings(out)
	return out
}

// sortedKeys lists a set's members in order.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
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
