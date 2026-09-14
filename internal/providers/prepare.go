package providers

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/infrena/infrena/internal/config"
	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/internal/pluginhost"
	"github.com/infrena/infrena/internal/registry"
	"github.com/infrena/infrena/internal/variables"
	"github.com/infrena/infrena/pkg/schema"
	"github.com/infrena/infrena/pkg/value"
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
	ctx context.Context, project *config.ProjectDecl,
	scope variables.Scope, reg *registry.Registry,
) (Table, diag.Diagnostics) {
	decls := project.Providers
	// LOAD FIRST, from what configuration says. `plugin: aws` in a `providers:`
	// entry is the instruction to go and find infrena-plugin-aws; so is a resource
	// of type `aws.instance` in a project that declares no `providers:` block at
	// all. Nothing outside configuration names a plugin.
	ds := load(ctx, project, reg)
	if ds.HasErrors() {
		// Resolving an instance of a plugin that could not be loaded would report
		// its every attribute against a provider that does not exist.
		return nil, ds
	}

	table, resolveDS := Resolve(decls, scope)
	ds.Extend(resolveDS)
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

// load brings in every plugin this project needs.
//
// Two sources, and both are configuration:
//
//   - every `plugin:` named in a `providers:` entry, which is the explicit form.
//   - every resource type's prefix, which is the same statement made by using one.
//     A plugin serves `<name>.*` and nothing else (§31.1), so a resource of type
//     `aws.instance` can only be served by the plugin `aws` — and a project that
//     configures nothing still needs it loaded.
//
// The second source is what keeps `providers:` optional. Without it every project
// ever written would have to gain a block that says nothing a reader could not
// already see, purely to name something.
func load(ctx context.Context, project *config.ProjectDecl, reg *registry.Registry) diag.Diagnostics {
	var ds diag.Diagnostics

	wanted := map[string]value.Origin{}
	for _, d := range project.Providers {
		if _, seen := wanted[d.Plugin]; !seen {
			wanted[d.Plugin] = d.Origin
		}
	}
	names := project.NeededPlugins()
	neededBy := project.PluginsNeededBy()

	// TWO PASSES. Every plugin is attempted before anything is reported, so that a
	// diagnostic about one that failed can list the types the others DID offer.
	// Loading is alphabetical, and reporting inside the loop meant `aws` failing
	// before `test` had loaded — so the message that most needed to say "here is what
	// you could have meant" was the one that never could.
	failures := map[string]error{}
	for _, name := range names {
		if err := reg.EnsurePlugin(ctx, name); err != nil {
			failures[name] = err
		}
	}

	ds.Extend(checkDeadConstraints(project, names))

	for _, name := range names {
		err, failed := failures[name]
		if !failed {
			continue
		}
		// A plugin that LOADED but failed its `plugins:` constraint is a different
		// problem from one that is missing, and the generic message got it backwards:
		// it said "not available" about a binary sitting right there, and advised
		// installing it. The version is the thing to act on.
		if verr, ok := errorsAsVersion(err); ok {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary: "the " + name + " plugin does not satisfy this project's `plugins` " +
					"constraint" + neededBySummary(neededBy[name]),
				Detail: err.Error(),
				Action: "Install a version matching " + verr.Constraint.String() + ", or widen " +
					"the constraint once you have confirmed this one works.",
				Origin: constraintOrigin(project, name, wanted[name]),
			})
			continue
		}
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			// The implying type goes in the SUMMARY, not only the detail: a user did
			// not necessarily type the plugin's name anywhere, so a summary naming
			// only the plugin leaves them hunting for where they asked for it.
			Summary: "the " + name + " plugin is not available" + neededBySummary(neededBy[name]),
			Detail:  err.Error() + knownTypes(reg),
			Action: "Install the plugin, or correct the name: a resource type's prefix is the " +
				"plugin that serves it, so `" + name + ".…` needs the " + name + " plugin. " +
				"`--verbose` reports where each plugin was loaded from.",
			Origin: wanted[name],
		})
	}
	return ds
}

// neededBySummary names what asked for a plugin, for the summary line.
func neededBySummary(reasons []string) string {
	if len(reasons) == 0 {
		return ""
	}
	return ", and it is needed by " + strings.Join(reasons, ", ")
}

// errorsAsVersion reports whether a load failed its version constraint.
func errorsAsVersion(err error) (*pluginhost.VersionError, bool) {
	return errors.AsType[*pluginhost.VersionError](err)
}

// constraintOrigin points a version failure at the `plugins:` line that caused it,
// rather than at whatever asked for the plugin: the constraint is the thing to change.
func constraintOrigin(project *config.ProjectDecl, name string, fallback value.Origin) value.Origin {
	if c, ok := project.Plugins[name]; ok && c.Origin.File != "" {
		return c.Origin
	}
	return fallback
}

// checkDeadConstraints refuses a `plugins:` entry naming a plugin this project does
// not use (PLAN.md §31.1).
//
// Same reasoning as a `defaults:` key nothing declares (§12.1): `plugins: {awz: ">= 1"}`
// constrains nothing, in every environment, forever, and there is no output in which its
// absence is visible. The user believes they have pinned a version and they have not.
//
// Checked HERE rather than in the loader, because only a compile knows the whole set a
// project uses — a state-only command derives its plugins from state, and discovery
// loads everything available, so neither could tell a dead constraint from one that
// simply does not apply to what it is doing.
func checkDeadConstraints(project *config.ProjectDecl, used []string) diag.Diagnostics {
	var ds diag.Diagnostics
	if len(project.Plugins) == 0 {
		return ds
	}
	inUse := make(map[string]bool, len(used))
	for _, name := range used {
		inUse[name] = true
	}

	names := make([]string, 0, len(project.Plugins))
	for name := range project.Plugins {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		if inUse[name] {
			continue
		}
		detail := "Nothing in this project uses it: no `providers:` entry names it, and no " +
			"resource type is prefixed " + strconv.Quote(name+".") + "."
		if len(used) > 0 {
			detail += "\n\nPlugins this project uses:\n  " + strings.Join(used, "\n  ")
		}
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "`plugins` constrains " + strconv.Quote(name) + ", which this project does not use",
			Detail:   detail,
			Action: "Correct the name, or remove the constraint: as written it pins nothing, " +
				"and nothing would say so.",
			Origin: project.Plugins[name].Origin,
		})
	}
	return ds
}

// knownTypes lists what the plugins that DID load offer.
//
// A missing plugin and a mistyped type are the same diagnostic from where the user
// is sitting — `type: awz.instance` asks for a plugin called `awz`, and the honest
// answer is that no such plugin exists. Which is unhelpful on its own: what they
// need next is the list of types they could have meant.
func knownTypes(reg *registry.Registry) string {
	types := reg.Types()
	if len(types) == 0 {
		return ""
	}
	return "\n\nResource types available from the plugins that did load:\n  " +
		strings.Join(types, "\n  ")
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
// EveryPlugin is one implicit instance per registered plugin — the table DISCOVERY
// needs, which is not the one Implicit returns.
//
// The two answer different questions, and conflating them is what broke `discover`:
//
//   - Implicit answers "which single instance does a resource that names none belong
//     to?", and MUST refuse to guess when there is more than one candidate: a resource
//     landing in an account nobody chose is the failure that matters there.
//   - EveryPlugin answers "which accounts should I survey?". More than one plugin is
//     not an ambiguity to refuse, it is simply more to ask — `discover` exists to
//     report what exists, including in accounts no configuration mentions.
//
// No instance is marked Default, because a default only decides where a RESOURCE goes
// and discovery binds none.
func EveryPlugin(reg *registry.Registry) Table {
	factories := reg.Factories()
	if len(factories) == 0 {
		// Nothing with a factory, but a caller may have supplied constructed
		// providers directly — every test that builds a stub does.
		out := make(Table)
		for _, name := range reg.InstanceNames() {
			out[name] = Instance{Name: name, Plugin: name}
		}
		return out
	}
	out := make(Table, len(factories))
	for _, name := range factories {
		out[name] = Instance{Name: name, Plugin: name}
	}
	return out
}

// Implicit is the ONE instance a project with no `providers:` block gets, for deciding
// where a resource that names no provider belongs. See EveryPlugin for the other
// question, which discovery asks and which this deliberately refuses to answer.
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
	return slices.Contains(registry.ReservedAttributes, key)
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
