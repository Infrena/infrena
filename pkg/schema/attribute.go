// Package schema describes provider resource types as inspectable data.
//
// Schemas are plain values rather than struct tags so that `infra explain`,
// validation and default resolution all read one source. Spec §8.1.
//
// EVERYTHING HERE IS DATA, and holds no functions at all. A provider is a
// separate process (PLAN.md §31.1), so a schema has to survive a pipe: the host
// receives one from a binary it did not build and must be able to describe,
// default and validate against it with nothing but what came down the wire.
package schema

import "github.com/infrata/infrata/pkg/value"

// Attribute describes a resource attribute.
type Attribute struct {
	Kind      value.Kind
	Required  bool
	Computed  bool // the provider sets it; configuration may not, unless Optional
	Sensitive bool
	ForceNew  bool // a change replaces the resource rather than updating it

	// Optional, WITH Computed, is the third state a real cloud needs (PLAN.md §14.1):
	// configuration MAY set this attribute, and the provider picks a value when
	// configuration does not.
	//
	// Without it the model is binary — configuration's or the provider's — and an
	// attribute the cloud fills in does not fail cleanly, it fails to CONVERGE: the
	// planner reads a returned value that configuration does not set as "removed from
	// configuration" and proposes to unset it, so the cloud chooses again on the next
	// apply, forever.
	//
	// Set: an ordinary attribute, diffed normally, ForceNew applying normally.
	// Unset: the provider's value is recorded and never diffed. ForceNew therefore
	// needs no special case — an unset optional+computed attribute produces no diff at
	// all, so it can never produce a replacement however the provider's value moves.
	//
	// Meaningless without Computed, since every non-required attribute is already
	// optional, and Validate refuses it there so the field cannot be set in the belief
	// that it does something.
	Optional bool

	// Aliases are alternative spellings configuration may use for this attribute.
	//
	// Matching is CASE-INSENSITIVE across the canonical name and every alias, so a
	// plugin declaring `CidrBlock` with aliases `cidr` and `cidr_block` accepts all of
	// `CidrBlock`, `cidrblock`, `cidr_block` and `cidr`.
	//
	// THE CANONICAL NAME IS THE IDENTITY. Aliases are input and display only, and that
	// needs no enforcement: the compiler canonicalises at its own boundary, and state,
	// the plan artifact and the plugin wire are all written downstream of it, so none
	// of them can carry an alias. Adding an alias in a later plugin release therefore
	// changes only what a user may type and what a plan renders — never a stored key.
	//
	// Validate refuses a definition whose names fold together, so a collision is a
	// plugin that will not load rather than a silent runtime surprise about which
	// spelling won.
	Aliases []string

	// Default is the value this attribute takes when configuration supplies
	// none: a plain Go datum of the attribute's declared Kind — int64(10),
	// "gp3", true — or nil for no default. DatumValue converts it.
	//
	// A DATUM, not a function, and that is the whole of this field's history.
	// It used to be a DefaultFunc taking a DefaultContext of environment,
	// region, account and project, which existed so a default could differ
	// between environments. PLAN.md §13 withdrew that: a provider default is
	// one value per attribute, and anything that should differ between
	// environments is a variable, which is visible in the configuration and
	// carries provenance.
	//
	// With §13 gone the function had no remaining purpose, and §31.1 gave it a
	// cost: a provider is a separate process, and a function cannot cross a
	// pipe. A schema is DATA — that is what lets the host receive one from a
	// binary somebody else built. So the field is a datum, and a default that
	// really varies by region or account is either a variable (the user
	// decides) or a computed attribute (the provider reports it).
	Default any

	Description string
}

// DatumValue converts a default datum into a Value of the attribute's declared
// kind, reporting false if the datum is not of that kind.
//
// It lives here, in the package that DECLARES what a default may be, because
// two callers need the identical answer and a second copy would be a silent
// disagreement rather than a visible one. internal/compiler fills defaults in
// with it; internal/generator decides whether a discovered value already EQUALS
// its default with it. If those two conversions ever differed, generation would
// omit an attribute the compiler then filled with something else — and the
// resource would change on the first apply after an import that reported no
// changes.
//
// Matching a Go type is not the same as matching the declared kind: a datum for
// a float attribute written as int64 builds a perfectly valid KindInt value,
// which would then sail past the kind check that exists to catch exactly this,
// because that check runs on configuration rather than on the default. The
// declared kind is the contract; the Go type is only how it happens to arrive.
func DatumValue(raw any, kind value.Kind) (value.Value, bool) {
	var v value.Value
	switch d := raw.(type) {
	case string:
		v = value.String(d, value.SourceDefault)
	case int64:
		v = value.Int(d, value.SourceDefault)
	case int:
		v = value.Int(int64(d), value.SourceDefault)
	case float64:
		v = value.Float(d, value.SourceDefault)
	case bool:
		v = value.Bool(d, value.SourceDefault)
	case []value.Value:
		v = value.List(d, value.SourceDefault)
	case map[string]value.Value:
		v = value.Map(d, value.SourceDefault)
	default:
		return value.Value{}, false
	}
	if v.Kind != kind {
		return value.Value{}, false
	}
	return v, true
}
