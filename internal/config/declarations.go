// Package config implements the loading and decoding stages of the compiler.
//
// This is the only package in the engine permitted to touch yaml.Node. Every
// later stage works with the typed declarations defined here.
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

	// Source is where this plugin comes from, when the project says so. Empty
	// means the trusted sources decide.
	//
	// A source named here is a candidate, not a permission. This file is
	// checked into git and travels to whoever clones it, so if it could grant
	// a download source then `git clone && infrena plan` would be enough for a
	// repository to introduce a place infrena fetches executables from. Only
	// the user's own configuration trusts an owner, and installing from an
	// untrusted one takes an explicit confirmation that names it.
	Source       string
	SourceOrigin value.Origin
}

// AttributeDecl is one configured attribute. Value holds the literal datum,
// and any string containing "${" is kept verbatim with HasExpressions set;
// parsing it into an expression tree happens later.
type AttributeDecl struct {
	Name           string
	Value          value.Value
	HasExpressions bool
	Origin         value.Origin
}

// ProviderDecl is one declared provider instance.
//
// Plugin is the implementation; Name is what resources refer to, defaulting to
// the plugin name. Config is handed to the plugin; Defaults are attribute
// defaults for every resource that uses this instance. They are separate maps
// because at the top level of the block `iam-role: x` and `tags: ${var.tags}`
// are indistinguishable while meaning entirely different things — one
// configures the provider, the other defaults a resource.
type ProviderDecl struct {
	Plugin string
	Name   string
	// Default marks the instance resources get when they name none. Set from
	// `default: true`, or from being first when nothing is marked.
	Default bool
	// DefaultExplicit records whether `default: true` was written, which is
	// what lets two of them be refused without also refusing the
	// derived-from-order case.
	DefaultExplicit bool
	Config          map[string]AttributeDecl
	Defaults        map[string]AttributeDecl
	Origin          value.Origin
	// NameOrigin is where the name came from: the `name:` line, or the
	// `plugin:` line when it was derived. A collision diagnostic points at the
	// line that chose the name rather than at the entry as a whole.
	NameOrigin value.Origin
}

// LifecycleDecl configures how a resource is created and destroyed.
type LifecycleDecl struct {
	PreventDestroy bool
	PreventReplace bool
	Retain         bool
	// The ...Set fields record whether the resource wrote the key, which a
	// bare bool cannot express: absent and `false` are the same value.
	//
	// A provider instance's `defaults:` may supply any of these flags, and
	// `prevent_destroy: false` on a resource has to beat an instance default
	// of true. Without the distinction the resource's explicit `false` would
	// read as "said nothing" and the default would win — which is the
	// direction that refuses a destroy the user asked for.
	PreventDestroySet bool
	PreventReplaceSet bool
	// CreateBeforeDestroy reverses a replacement's two halves.
	CreateBeforeDestroy    bool
	CreateBeforeDestroySet bool
	RetainSet              bool

	// IgnoreChanges names attributes whose drift this resource does not want
	// reverted, as written — aliases and casing are resolved against the
	// schema later, at the compiler's one canonicalisation boundary.
	IgnoreChanges []string
	// IgnoreChangesOrigin locates each entry, so a name that matches no
	// attribute is reported at the line the user wrote.
	IgnoreChangesOrigin map[string]value.Origin
}

// ResourceDecl is one declared resource, decoded but not yet resolved.
type ResourceDecl struct {
	Name string
	// Dir is the resources directory this was declared in, slash-separated and
	// relative to resources/ — "database" for resources/database/db.yml. Empty
	// for infrena.yml, for discovered/, and for a module file.
	//
	// It exists so a later stage can give the resource the variable scope of
	// the directory it came from. It is not part of the resource's identity:
	// the address is the name alone, so moving a file between directories does
	// not rename anything.
	Dir        string
	Type       string
	Attributes map[string]AttributeDecl
	// Skip and Only name the environments this resource does or does not
	// belong to. At most one of the two is set; both together is refused at
	// decode time.
	//
	// AttributeDecl rather than []string, for two reasons. The Origin travels
	// with the value, so an unknown environment name is reported against the
	// line that wrote it rather than against the resource. And HasExpressions
	// survives, which is what makes `only: ${var.replica_in}` work — a module
	// written with parts its caller can switch off.
	Skip AttributeDecl
	Only AttributeDecl
	// ForEach declares that this resource is a set of instances, one per entry.
	// A list yields an instance per element, keyed by the element; a map yields
	// one per key. AttributeDecl for the same reasons Skip and Only are.
	//
	// There is deliberately no `count`. An instance's identity is its key,
	// never its position, so removing one entry affects exactly one resource,
	// where an ordinal would shift every later instance and propose destroying
	// and recreating resources that did not change.
	ForEach AttributeDecl
	// Provider names the provider instance this resource belongs to, or is
	// unset for the default one. A name, never a plugin. AttributeDecl for the
	// same reasons Skip and Only are; see resolveProviderName for why an
	// expression here is not resolved.
	Provider  AttributeDecl
	DependsOn []string
	Lifecycle LifecycleDecl
	Origin    value.Origin
}

// VariableDecl is one declared variable, from infrena.yml's `variables:` block.
//
// It is a declaration — a type and its bounds — not a value. Values come from
// variables.yml, from environment overrides and from --var, and are resolved
// against these later. Keeping them apart is what lets `infrena validate`
// report a type error in a declaration with no environment selected.
type VariableDecl struct {
	Name string
	// Type is the declared kind, or KindInvalid when the declaration gave no
	// type. Untyped is legal: variable schemas are optional.
	Type value.Kind
	// Default is the declared default; HasDefault is the authority on whether
	// there is one. A Value's zero is KindInvalid, which is also what a failed
	// decode produces, so the flag rather than the Kind is what consumers
	// check.
	//
	// Scope is deliberately left unset on Default. A default resolves to
	// SourceDefault and ScopeBaseConfig, and resolution stamps that when the
	// default wins: a declaration is not a resolution, and only one place may
	// decide which precedence level won.
	Default    value.Value
	HasDefault bool
	// Min and Max are inclusive bounds, valid only when HasMin/HasMax.
	//
	// value.Value rather than float64, for two reasons. An int64 does not fit
	// a float64 exactly, so a bound above 2^53 would validate wrongly; and a
	// Value carries its Origin, so a range diagnostic can point at the line
	// where the bound was declared rather than only at the value that failed
	// it. The price is that a consumer comparing against these must be
	// kind-aware: value.AsFloat deliberately does not coerce.
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

// EnvironmentDecl is one declared environment.
//
// One environment may be declared in two places — infrena.yml's
// `environments:` block and environments/<name>.yml — and they merge into a
// single decl, because `extends` belongs in the block and overrides belong in
// the file. A key set in both places is an error, because one of the two
// values would silently disappear.
type EnvironmentDecl struct {
	Name string
	// Extends names the parent environment, or "" when it extends nothing.
	// Decoding checks only that it is a usable scalar: an unknown parent and a
	// cycle both need the whole set of environments, which a later stage has
	// and this one does not.
	Extends       string
	ExtendsOrigin value.Origin
	// Overrides is sorted by Name, so the scope stack built from it and every
	// diagnostic that follows are the same on every run.
	//
	// A slice rather than a map deliberately: a map would lose each override's
	// Origin as a first-class field and would force every consumer to re-sort.
	Overrides []OverrideDecl
	Origin    value.Origin

	// RequireApproval and PreventDestroy are the production protections,
	// declared on the environment rather than implied by its name.
	//
	// The ...Set fields exist for the reason the resource lifecycle's do: an
	// unset protection and one explicitly written `false` are different things
	// once inheritance is involved. A child that says nothing inherits its
	// parent's protection; a child that writes `prevent_destroy: false` has
	// deliberately dropped it.
	RequireApproval       bool
	RequireApprovalSet    bool
	RequireApprovalOrigin value.Origin
	PreventDestroy        bool
	PreventDestroySet     bool
	PreventDestroyOrigin  value.Origin
}

// ModuleLoadDecl is one entry in `modules:`.
//
// It makes a module available under a name and says nothing else about it. It
// carries no inputs and no depends_on: loading and instantiating are separate
// steps, and the instantiation is an ordinary ResourceDecl whose Type is
// "module.<Name>".
//
// That separation is why there is no ModuleInputDecl. A caller's input is an
// AttributeDecl in ResourceDecl.Attributes, bound by the same code that binds
// every other attribute, with no module-specific precedence logic.
type ModuleLoadDecl struct {
	// Name is what a resource type refers to: `type: module.<Name>`. It is
	// either written explicitly with `name:` or derived from Source.
	Name string
	// Source is the parsed source, not the text: it has been through
	// internal/modules/source.Parse, so it is a path, or a git remote with a
	// scheme on the allowlist and the required `:tag-or-hash` pin.
	//
	// Parsed rather than raw so that the stage which resolves the module
	// cannot parse it again, and so cannot report a parse failure twice.
	// Parse is pure, so decoding still touches neither the filesystem nor the
	// network; what it gains is a line number for the diagnostic, which only
	// this stage has.
	//
	// There is no SourceOrigin field: source.Source carries its own Origin.
	Source source.Source
	Origin value.Origin
}

// BackendDecl is the decoded `backend:` block: where this project's state lives.
//
// `plugin:` is the only key this package reserves. Every other key is the
// backend's configuration and is carried across untouched, because the engine
// has no way to know which keys an s3 backend accepts and guessing would make
// adding a backend option a change to the core. That is why an unrecognised
// key here is not an error, alone among the blocks in this language.
//
// Config is map[string]any rather than the map[string]AttributeDecl
// ProviderDecl.Config uses, and the difference is the point: an AttributeDecl
// carries HasExpressions, and nothing here may carry an expression.
//
// An empty Plugin means the project declared no backend, which is local — the
// bootstrap that works before anything is installed. Unless Origin is set: a
// block that was present and did not decode records its origin and no plugin,
// so a caller can tell "no backend was asked for" from "a backend was asked
// for and infrena could not work out which". Only the first of those is local.
type BackendDecl struct {
	Plugin string
	Config map[string]any
	Origin value.Origin
}

// ProjectDecl is the decoded, still-unresolved configuration.
type ProjectDecl struct {
	Project string
	// RequiredVersion is the optional `infrena:` floor a project states on the
	// tool itself. Zero means unconstrained, which is every project written
	// before the key existed.
	RequiredVersion       semver.Constraint
	RequiredVersionOrigin value.Origin
	// Plugins constrains provider plugin versions, keyed by plugin name.
	// Absent means unconstrained: whatever is found runs, and `--verbose` says
	// which.
	//
	// Keyed by plugin, not by `providers:` entry, because two instances of one
	// plugin share one process and therefore one version. A per-instance
	// constraint could ask for two versions of one binary.
	Plugins map[string]PluginConstraint

	Resources    []*ResourceDecl   // sorted by Name
	Variables    []VariableDecl    // sorted by Name
	Environments []EnvironmentDecl // sorted by Name
	Modules      []ModuleLoadDecl  // sorted by Name
	// VariableValues is variables.yml's contents: a flat mapping of name to
	// value. Never nil. These are values, not declarations, and are checked
	// against Variables later. variables.yml may not carry a `variables:`
	// schema block; declarations live in infrena.yml and nowhere else, so that
	// "where is this variable declared" has one answer.
	//
	// Distinct from what --var-file supplied, which is a CLI input at a
	// different precedence level. The two must not be merged.
	//
	// Iterate it in sorted key order whenever order is observable; Go's map
	// order is randomised.
	VariableValues map[string]value.Value
	// ScopedValues holds resources/<dir>/vars/** — variable values visible only
	// to the resources declared in that directory, keyed by the same string
	// ResourceDecl.Dir carries. Never nil.
	//
	// A separate map rather than merged into VariableValues, because the two
	// sit on different precedence rungs: a directory's values rank above base
	// configuration and below an environment. Merging would lose that, and the
	// scoping with it.
	ScopedValues map[string]map[string]value.Value
	// Providers holds the provider instances this project uses, in declaration
	// order.
	//
	// A slice, never a map, for two reasons that both matter. Order decides
	// which instance is the default. And a YAML map cannot hold two `aws`
	// keys, so it would refuse that collision silently by losing one, and
	// silence is what sends a resource to the wrong account.
	Providers []ProviderDecl
	// Backend holds where this project's state lives. The zero value means
	// local, which is what every project written before the block existed
	// says.
	Backend BackendDecl
	// MigrateFrom is where `state migrate` reads from. Same shape and same
	// rules as Backend, decoded by the same code, because two blocks that mean
	// the same thing must not be able to disagree about what a key means.
	//
	// No command but `state migrate` acts on it. A migration run through CI is
	// necessarily two commits — one adding this block, one removing it — and
	// between them it sits in committed configuration. If `plan` or `apply`
	// acted on it, a successful migration would break the pipeline until
	// somebody tidied up.
	//
	// Ordinary commands do consult it for one guard: while the destination is
	// empty and the source holds state, the migration has not happened, and a
	// command reading only `backend:` would see every resource as unmanaged
	// and recreate all of it.
	//
	// The zero value means absent, which is every project: no migration is
	// pending and nothing changes.
	MigrateFrom BackendDecl
	Origin      value.Origin
}

// NeededPlugins names every provider plugin a project's declarations imply.
//
// Two sources, both the project's own configuration: every `plugin:` named in
// a `providers:` entry, and every resource type's prefix, since a plugin
// serves `<name>.*` and nothing else, so `aws.instance` can only be served by
// the plugin `aws`. The second source is what keeps `providers:` optional.
//
// It lives beside the declarations it reads because two callers need the
// identical answer: the compiler loads these before resolving instances, and
// internal/cli loads them for the commands that never compile at all. A second
// copy of the rule would be a silent disagreement about which plugins a
// project uses.
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
			// `module.<name>` instantiates a module, not a provider resource.
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
