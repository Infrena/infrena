// Package config implements compiler stages 1 (load) and 2 (decode).
//
// Stage 2 is the only place in the engine permitted to touch yaml.Node. Every
// later stage works with the typed declarations defined here. Spec §7.
package config

import (
	"github.com/infrata/infrata/internal/modules/source"
	"github.com/infrata/infrata/pkg/value"
)

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
// because at the top level of the block `iam-role: x` and `tags: ${tags}` are
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
	Retain         bool
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
	// `only: ${replica_in}` work — a module written with parts its caller can
	// switch off. A bare slice loses both and cannot hold an expression at all.
	Skip      AttributeDecl
	Only      AttributeDecl
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

// ProjectDecl is the decoded, still-unresolved configuration.
type ProjectDecl struct {
	Project      string
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
	Origin    value.Origin
}
