// Package config implements compiler stages 1 (load) and 2 (decode).
//
// Stage 2 is the only place in the engine permitted to touch yaml.Node. Every
// later stage works with the typed declarations defined here. Spec §7.
package config

import (
	"slices"

	"github.com/infrena/infrena/internal/modules/source"
	"github.com/infrena/infrena/pkg/semver"
	"github.com/infrena/infrena/pkg/value"
	"sort"
	"strings"
)

// PluginConstraint is one plugin's accepted version range, and where it was written.
type PluginConstraint struct {
	Constraint semver.Constraint
	Origin     value.Origin

	// Source is where this plugin comes from, when the project says so
	// (PLAN.md §31.3). Empty means the trusted sources decide.
	//
	// A SOURCE NAMED HERE IS A CANDIDATE, NOT A PERMISSION. This file is
	// checked into git and travels to whoever clones it, so if it could grant
	// a download source then `git clone && infrena plan` would be enough for a
	// repository to introduce a place infrena fetches executables from. Only
	// the user's own configuration trusts an owner, and installing from an
	// untrusted one takes an explicit confirmation that names it.
	Source       string
	SourceOrigin value.Origin
}

// AttributeDecl is one configured attribute.
//
// In M1, Value holds the literal datum, and any string containing "${" is kept
// verbatim with HasExpressions set. M2's stage 6 replaces this with a parsed
// expression tree.
type AttributeDecl struct {
	Name           string
	Value          value.Value
	HasExpressions bool
	Origin         value.Origin
}

// ProviderDecl is one declared provider instance (PLAN.md §12.1).
//
// Plugin is the implementation; Name is what resources refer to, defaulting to
// the plugin name. Config is handed to the plugin; Defaults are attribute
// defaults for every resource that uses this instance. They are separate maps
// because at the top level of the block `iam-role: x` and `tags: ${var.tags}` are
// indistinguishable while meaning entirely different things — one configures the
// PROVIDER, the other defaults a RESOURCE.
type ProviderDecl struct {
	Plugin string
	Name   string
	// Default marks the instance resources get when they name none. Set from
	// `default: true`, or from being first when nothing is marked.
	Default bool
	// DefaultExplicit records whether `default: true` was WRITTEN, which is what
	// lets two of them be refused without also refusing the derived-from-order
	// case.
	DefaultExplicit bool
	Config          map[string]AttributeDecl
	Defaults        map[string]AttributeDecl
	Origin          value.Origin
	// NameOrigin is where the name came from — the `name:` line, or the `plugin:`
	// line when it was derived. A collision diagnostic points at both entries, and
	// pointing at the line that CHOSE the name is more useful than pointing at the
	// entry as a whole.
	NameOrigin value.Origin
}

// LifecycleDecl configures how a resource is created and destroyed.
type LifecycleDecl struct {
	PreventDestroy bool
	PreventReplace bool
	Retain         bool
	// PreventDestroySet and RetainSet record whether the resource WROTE the key,
	// which a bare bool cannot express: absent and `false` are the same value.
	//
	// They exist because a provider instance's `defaults:` may supply either flag
	// (PLAN.md §12.1), and `prevent_destroy: false` on a resource has to beat an
	// instance default of true. Without the distinction the resource's explicit
	// `false` would read as "said nothing" and the default would win — which is the
	// direction that REFUSES a destroy the user asked for.
	PreventDestroySet bool
	PreventReplaceSet bool
	// CreateBeforeDestroy reverses a replacement's two halves (§38.2).
	CreateBeforeDestroy bool
	// CreateBeforeDestroySet distinguishes "written false" from "not written",
	// so a provider default can supply it and an explicit false can refuse one.
	CreateBeforeDestroySet bool
	RetainSet              bool

	// IgnoreChanges names attributes whose drift this resource does not want reverted,
	// as WRITTEN — aliases and casing are resolved against the schema later, at the
	// compiler's one canonicalisation boundary (PLAN.md §14.2).
	IgnoreChanges []string
	// IgnoreChangesOrigin locates each entry, so a name that matches no attribute is
	// reported at the line the user wrote rather than at the resource.
	IgnoreChangesOrigin map[string]value.Origin
}

// ResourceDecl is one declared resource, decoded but not yet resolved.
type ResourceDecl struct {
	Name string
	// Dir is the resources directory this was declared in, slash-separated and
	// relative to resources/ — "database" for resources/database/db.yml. Empty
	// for infra.yml, for discovered/, and for a module file.
	//
	// It exists so stage 4 can give the resource the variable scope of the
	// directory it came from (§4.1). It is NOT part of the resource's identity:
	// the address is the name alone, so moving a file between directories does
	// not rename anything.
	Dir        string
	Type       string
	Attributes map[string]AttributeDecl
	// Skip and Only name the environments this resource does or does not belong
	// to (PLAN.md §6.2). At most one of the two is set; both together is refused
	// at decode time.
	//
	// AttributeDecl rather than []string, for two reasons that are not
	// interchangeable. The Origin travels with the value, so stage 5 reports an
	// unknown environment name against the line that wrote it rather than
	// against the resource. And HasExpressions survives, which is what makes
	// `only: ${var.replica_in}` work — a module written with parts its caller can
	// switch off. A bare slice loses both and cannot hold an expression at all.
	Skip AttributeDecl
	Only AttributeDecl
	// ForEach declares that this resource is a SET of instances, one per entry
	// (PLAN.md §40). A list yields an instance per element, keyed by the
	// element; a map yields one per key.
	//
	// AttributeDecl for the same reasons Skip and Only are: the Origin travels
	// so a diagnostic lands on the line that wrote it, and HasExpressions
	// survives so `for_each: ${var.zones}` works at all.
	//
	// There is deliberately NO `count`. An instance's identity is its key,
	// never its position, so removing one entry affects exactly one resource —
	// where an ordinal would shift every later instance and propose destroying
	// and recreating resources that did not change.
	ForEach AttributeDecl
	// Provider names the provider INSTANCE this resource belongs to (PLAN.md
	// §12.1), or is unset for the default one. A name, never a plugin.
	//
	// AttributeDecl for the same reasons Skip and Only are: the Origin travels, so
	// "no such instance" points at the line that chose it. Expressions are
	// possible in the shape but not yet resolved — see resolveProviderName, which
	// records why.
	Provider  AttributeDecl
	DependsOn []string
	Lifecycle LifecycleDecl
	Origin    value.Origin
}

// VariableDecl is one declared variable, from infra.yml's `variables:` block
// (PLAN.md §9).
//
// It is a DECLARATION — a type and its bounds — not a value. Values come from
// variables.yml, from environment overrides and from --var; stage 4 resolves
// the two against each other. Keeping them apart is what lets `infra validate`
// report a type error in a declaration with no environment selected.
type VariableDecl struct {
	Name string
	// Type is the declared kind, or KindInvalid when the declaration gave no
	// type. Untyped is legal: PLAN.md §9 calls variable schemas optional.
	Type value.Kind
	// Default is the declared default; HasDefault is the authority on whether
	// there is one. A Value's zero is KindInvalid, which is also what a FAILED
	// decode produces, so the flag rather than the Kind is what stage 4 checks.
	// Scope is deliberately left unset on Default. A default resolves to
	// SourceDefault + ScopeBaseConfig, and STAGE 4 stamps that when the
	// default wins — a declaration is not a resolution, and only one place may
	// decide which precedence level won.
	Default    value.Value
	HasDefault bool
	// Min and Max are inclusive bounds, valid only when HasMin/HasMax.
	//
	// value.Value rather than float64, for two reasons. An int64 does not fit
	// a float64 exactly, so a bound above 2^53 would validate wrongly; and a
	// Value carries its Origin, so a range diagnostic can point at the line
	// where the minimum was DECLARED rather than only at the value that failed
	// it. The price is that a consumer comparing against these must be
	// kind-aware: int64 against int64 exactly, float64 only when a side
	// genuinely is one. value.AsFloat deliberately does not coerce.
	Min, Max       value.Value
	HasMin, HasMax bool
	Origin         value.Origin
}

// OverrideDecl is one key an environment sets.
type OverrideDecl struct {
	Name   string
	Value  value.Value
	Origin value.Origin
}

// EnvironmentDecl is one declared environment (PLAN.md §6, §7).
//
// One environment may be declared in two places — infra.yml's `environments:`
// block and environments/<name>.yml — and they MERGE into a single decl:
// PLAN.md §7 puts `extends` in the block and §8 puts overrides in the file, so
// a user doing both is following the spec. A key set in both places is an
// error, because one of the two values would silently disappear.
type EnvironmentDecl struct {
	Name string
	// Extends names the parent environment, or "" when it extends nothing.
	// Stage 2 checks only that it is a usable scalar: an unknown parent and a
	// cycle both need the whole set of environments, which is stage 3's job
	// (spec §7). Two implementations of one concept is the defect that leaked
	// a plaintext secret in M2.
	Extends       string
	ExtendsOrigin value.Origin
	// Overrides is sorted by Name, so stage 3's scope stack and every
	// diagnostic built from it are the same on every run — invariant 6.
	//
	// A slice rather than a map deliberately. A map would lose each override's
	// Origin as a first-class field and would force every consumer to re-sort:
	// M3 measured ELEVEN redundant sorts in this tree whose only job was
	// undoing map iteration. Sorting once, here, is what stops the twelfth.
	Overrides []OverrideDecl
	Origin    value.Origin

	// RequireApproval and PreventDestroy are §38's production protections,
	// declared on the environment rather than implied by its name (§13).
	//
	// The `...Set` fields exist for the reason the resource lifecycle's do: an
	// unset protection and one explicitly written `false` are different things
	// once inheritance is involved. A child environment that extends a protected
	// parent inherits the protection when it says nothing, and a child that
	// writes `prevent_destroy: false` has deliberately dropped it. Without the
	// distinction those two are the same zero value and the deliberate one
	// cannot be expressed.
	RequireApproval       bool
	RequireApprovalSet    bool
	RequireApprovalOrigin value.Origin
	PreventDestroy        bool
	PreventDestroySet     bool
	PreventDestroyOrigin  value.Origin
}

// ModuleLoadDecl is one entry in `modules:` (PLAN.md §11.1).
//
// It makes a module AVAILABLE under a name and says nothing else about it. It
// carries no inputs and no depends_on: loading and instantiating are separate
// steps, and both of those belong to the instantiation, which is an ordinary
// ResourceDecl whose Type is "module.<Name>" (§11.2).
//
// That separation is why there is no ModuleInputDecl. A caller's input is an
// AttributeDecl in ResourceDecl.Attributes, bound by stage 6's existing
// bindAttribute, so it carries whatever provenance its source gave it with no
// module-specific rung logic — which is a thing the compiler already gets right
// for every other attribute.
type ModuleLoadDecl struct {
	// Name is what a resource type refers to: `type: module.<Name>`. It is
	// either written explicitly with `name:` or derived from Source.
	Name string
	// Source is the PARSED source, not the text (contract Amendment 15b). It has
	// been through internal/modules/source.Parse, so it is one Parse accepted: a
	// path, or a git remote with a scheme on the allowlist and the required
	// `:tag-or-hash` pin.
	//
	// Parsed rather than raw so stage 5 CANNOT re-report. Stage 5 hands this
	// straight to Cache.Resolve, which takes a source.Source — so it never
	// parses, so it cannot emit a parse diagnostic a second time. Storing the
	// string instead would leave "do not report this twice" as a rule an
	// implementer has to remember, and "call Parse and pass the diagnostics up"
	// is the obvious thing to write.
	//
	// Stage 2 still touches neither the filesystem nor the network: Parse is
	// pure. What moved to stage 2 is the REPORTING, because Parse's diagnostics
	// want a line and stage 2 is the only stage that has one — so a missing pin,
	// a refused scheme and `ext::` all surface at `infra validate`.
	//
	// There is no SourceOrigin field: source.Source carries its own Origin,
	// stamped from the one passed to Parse, and a second copy of that fact is a
	// second thing to keep true.
	Source source.Source
	Origin value.Origin
}

// BackendDecl is the decoded `backend:` block: where this project's state
// lives (PLAN.md §52).
//
// `plugin:` is the ONLY key this package reserves. Every other key is the
// BACKEND'S configuration and is carried across untouched, because the engine
// has no way to know which keys an s3 backend accepts and guessing would make
// adding a backend option a change to the core. That is why an unrecognised key
// here is not an error, alone among the blocks in this language.
//
// Config is map[string]any rather than map[string]AttributeDecl, which is what
// ProviderDecl.Config uses, and the difference is the point: an AttributeDecl
// carries HasExpressions, and nothing here may carry an expression. A shape
// that could express one would be a shape someone later tries to resolve.
//
// An empty Plugin means the project declared no backend, which is local — the
// bootstrap that works before anything is installed. UNLESS Origin is set: a
// block that was present and did not decode records its origin and no plugin,
// so a caller can tell "no backend was asked for" from "a backend was asked for
// and infrena could not work out which". Only the first of those is local.
type BackendDecl struct {
	Plugin string
	Config map[string]any
	Origin value.Origin
}

// ProjectDecl is the decoded, still-unresolved configuration.
type ProjectDecl struct {
	Project string
	// RequiredVersion is the optional `infrena:` floor a project states on the tool
	// itself (PLAN.md §61.2). Zero means unconstrained, which is every project
	// written before the key existed.
	RequiredVersion       semver.Constraint
	RequiredVersionOrigin value.Origin
	// Plugins constrains provider plugin versions, keyed by plugin name
	// (PLAN.md §31.1). Absent means unconstrained: whatever is found runs, and
	// `--verbose` says which.
	//
	// Keyed by PLUGIN, not by `providers:` entry, because two instances of one plugin
	// share one process and therefore necessarily share one version. A per-instance
	// constraint could ask for two versions of one binary.
	Plugins map[string]PluginConstraint

	Resources    []*ResourceDecl   // sorted by Name
	Variables    []VariableDecl    // sorted by Name
	Environments []EnvironmentDecl // sorted by Name
	Modules      []ModuleLoadDecl  // sorted by Name
	// VariableValues is variables.yml's contents: a flat mapping of name to
	// value (PLAN.md §8). Never nil. These are VALUES, not declarations —
	// stage 4 checks them against Variables. variables.yml may NOT carry a
	// `variables:` schema block; declarations live in infra.yml and nowhere
	// else, so that "where is this variable declared" has one answer.
	//
	// Distinct from compiler.Options.FileVars, which is what --var-file
	// supplied. That is a CLI input at a different precedence level and does
	// not belong here; the two must not be merged.
	//
	// Iterate it in sorted key order whenever order is observable; Go's map
	// order is randomised.
	VariableValues map[string]value.Value
	// ScopedValues holds resources/<dir>/vars/** — variable values visible only
	// to the resources declared in that directory, keyed by the same string
	// ResourceDecl.Dir carries (§4.1). Never nil.
	//
	// A separate map rather than merged into VariableValues, because the two sit
	// on different precedence rungs: §7 puts a directory's values ABOVE base
	// configuration and BELOW an environment. Merging them would lose that, and
	// lose the scoping with it.
	ScopedValues map[string]map[string]value.Value
	// Providers holds `providers:` — the provider INSTANCES this project uses
	// (PLAN.md §12.1), in DECLARATION ORDER.
	//
	// A slice, never a map, for two reasons that both matter. Order decides which
	// instance is the default. And a YAML map cannot hold two `aws` keys, which is
	// exactly the collision §12.1 refuses — a shape unable to express it would
	// refuse it silently by losing one, and silence is what sends a resource to the
	// wrong account.
	Providers []ProviderDecl
	// Backend holds `backend:` — where this project's state lives (PLAN.md
	// §52). The zero value means local, which is what every project written
	// before the block existed says.
	Backend BackendDecl
	// MigrateFrom is where `state migrate` reads from. Same shape and same
	// rules as Backend, decoded by the same code, because two blocks that
	// mean the same thing must not be able to disagree about what a key means.
	//
	// NO COMMAND BUT `state migrate` ACTS ON IT (spec §7). A migration run
	// through CI is necessarily two commits — one adding this block, one
	// removing it — and between them it sits in committed configuration. If
	// `plan` or `apply` acted on it, a SUCCESSFUL migration would break the
	// pipeline until somebody tidied up.
	//
	// Ordinary commands DO consult it for one GUARD, added after the first
	// draft: while the destination is empty and the source holds state, the
	// migration has not happened, and a command reading only `backend:` would
	// see every resource as unmanaged and RECREATE ALL OF IT.
	//
	// The zero value means absent, which is every project: no migration is
	// pending and nothing changes.
	MigrateFrom BackendDecl
	Origin      value.Origin
}

// NeededPlugins names every provider plugin a project's declarations imply.
//
// TWO SOURCES, and both are the project's own configuration:
//
//   - every `plugin:` named in a `providers:` entry, which is the explicit form.
//   - every resource type's PREFIX, which is the same statement made by using one.
//     A plugin serves `<name>.*` and nothing else (PLAN.md §31.1), so a resource of
//     type `aws.instance` can only be served by the plugin `aws`.
//
// The second source is what keeps `providers:` optional. Without it, every project
// ever written would have to gain a block naming something a reader can already see.
//
// It lives here, beside the declarations it reads, because two callers need the
// identical answer: compiler stage 4.5 loads these before resolving instances, and
// internal/cli loads them for the commands that never compile at all. A second copy
// of the rule would be a silent disagreement about which plugins a project uses.
func (p *ProjectDecl) NeededPlugins() []string {
	out, _ := p.neededPlugins()
	return out
}

// PluginsNeededBy is NeededPlugins with the resource types that implied each, so a
// diagnostic about a missing plugin can name what asked for it.
func (p *ProjectDecl) PluginsNeededBy() map[string][]string {
	_, by := p.neededPlugins()
	return by
}

func (p *ProjectDecl) neededPlugins() ([]string, map[string][]string) {
	seen := map[string]bool{}
	by := map[string][]string{}
	var out []string
	add := func(name, reason string) {
		if name == "" {
			return
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
		if reason != "" && !contains(by[name], reason) {
			by[name] = append(by[name], reason)
		}
	}

	for _, d := range p.Providers {
		add(d.Plugin, "`plugin: "+d.Plugin+"`")
	}
	for _, r := range p.Resources {
		prefix, _, ok := strings.Cut(r.Type, ".")
		if !ok || prefix == "module" {
			// `module.<name>` instantiates a module, not a provider resource (§11).
			continue
		}
		add(prefix, r.Type)
	}
	sort.Strings(out)
	for name := range by {
		sort.Strings(by[name])
	}
	return out, by
}

func contains(haystack []string, needle string) bool {
	return slices.Contains(haystack, needle)
}
