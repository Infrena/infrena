// Package schema describes provider resource types as inspectable data.
//
// Schemas are plain values rather than struct tags so that `infrena explain`,
// validation and default resolution all read one source.
//
// Everything here is data, and holds no functions at all. A provider is a
// separate process, so a schema has to survive a pipe: the host receives one
// from a binary it did not build and must be able to describe, default and
// validate against it with nothing but what came down the wire.
package schema

import "github.com/infrena/infrena/pkg/value"

// Reference names what an attribute refers to, when it holds another resource's
// identifier rather than a value of its own.
//
// It is data, and that is the whole design. A function-typed field cannot cross
// the pipe to a plugin process, and a plugin that resolved its own references
// would be a second resolver, free to disagree with the engine's about what a
// reference means.
//
// The plugin decides what is referred to. The engine cannot know that Cloud
// Control's AWS::EC2::Subnet.VpcId wants a VPC's id rather than its arn; that is
// knowledge about an API, and it belongs to whoever owns the API. The engine
// reads this and never infers, defaults, or guesses.
type Reference struct {
	// Type is the resource type referred to, in the plugin's own naming —
	// "aws.ec2.vpc", not "AWS::EC2::VPC".
	Type string
	// Attribute is which of that type's attributes this one holds. It is the
	// canonical name, never an alias: the canonical name is the identity, and an
	// alias here would have to be folded at every read.
	Attribute string
}

// Attribute describes a resource attribute.
type Attribute struct {
	Kind      value.Kind
	Required  bool
	Computed  bool // the provider sets it; configuration may not, unless Optional
	Sensitive bool
	ForceNew  bool // a change replaces the resource rather than updating it

	// Optional, with Computed, is the third state a real cloud needs:
	// configuration may set this attribute, and the provider picks a value when
	// configuration does not.
	//
	// Without it the model is binary — configuration's or the provider's — and
	// an attribute the cloud fills in does not fail cleanly, it fails to
	// converge: the planner reads a returned value that configuration does not
	// set as "removed from configuration" and proposes to unset it, so the cloud
	// chooses again on the next apply, forever.
	//
	// Set: an ordinary attribute, diffed normally, ForceNew applying normally.
	// Unset: the provider's value is recorded and never diffed, so ForceNew
	// needs no special case — an unset optional+computed attribute produces no
	// diff at all, and can never produce a replacement however the provider's
	// value moves.
	//
	// Meaningless without Computed, since every non-required attribute is
	// already optional, and Validate refuses it there so the field cannot be set
	// in the belief that it does something.
	Optional bool

	// Aliases are alternative spellings configuration may use for this
	// attribute.
	//
	// Matching is case-insensitive across the canonical name and every alias, so
	// a plugin declaring `CidrBlock` with aliases `cidr` and `cidr_block`
	// accepts all of `CidrBlock`, `cidrblock`, `cidr_block` and `cidr`.
	//
	// The canonical name is the identity. Aliases are input and display only,
	// and that needs no enforcement: the compiler canonicalises at its own
	// boundary, and state, the plan artifact and the plugin wire are all written
	// downstream of it. Adding an alias in a later plugin release therefore
	// changes only what a user may type and what a plan renders, never a stored
	// key.
	//
	// Validate refuses a definition whose names fold together, so a collision is
	// a plugin that will not load rather than a silent runtime surprise about
	// which spelling won.
	Aliases []string

	// References declares that this attribute holds another resource's
	// identifier, which is what lets configuration pass the resource whole —
	// `vpc_id: ${vpc}` — instead of naming the attribute.
	//
	// Nil means "not a reference", and that is the honest default: most
	// attributes are not references, and an empty Reference{} would be
	// indistinguishable from an author who meant to fill it in.
	References *Reference

	// Fields describes a KindMap attribute's known keys, where the provider
	// knows them.
	//
	// Nil means open, and that is a first-class answer rather than a gap: AWS
	// tags take any key and always will, so declaring Fields for them would be a
	// lie. A path into an open map is checked at apply instead.
	//
	// Where it is declared, a typo becomes a compile error listing the keys that
	// exist, rather than something that passes validate, produces a clean plan,
	// and fails halfway through apply once real infrastructure exists.
	Fields map[string]Attribute

	// Elem describes every element of a KindList attribute, where the provider
	// knows their shape. A repeated block — a list of maps with known keys —
	// is an Elem of KindMap carrying its own Fields.
	//
	// Separate from Fields on purpose. The two answer different questions:
	// Fields is "this map's known keys", Elem is "what each element looks
	// like". Overloading one field with both meanings leaves them
	// distinguished only by a sibling Kind, which is a coupling nothing
	// enforces — and the version of this package that tried it had a
	// canonicaliser asserting the list meaning while the validator enforced
	// the map one, so a repeated block's keys could not be declared at all.
	//
	// Nil means the elements have no declared shape, which is the honest
	// answer for a list of strings and for a list whose element keys the
	// provider does not know.
	Elem *Attribute

	// Default is the value this attribute takes when configuration supplies
	// none: a plain Go datum of the attribute's declared Kind — int64(10),
	// "gp3", true — or nil for no default. DatumValue converts it.
	//
	// A datum, deliberately not a function. A provider is a separate process and
	// a function cannot cross a pipe, which is what lets the host receive a
	// schema from a binary somebody else built. A provider default is therefore
	// one value per attribute, and anything that should differ between
	// environments is either a variable (the user decides, and it is visible in
	// the configuration and carries provenance) or a computed attribute (the
	// provider reports it).
	Default any

	Description string
}

// DatumValue converts a default datum into a Value of the attribute's declared
// kind, reporting false if the datum is not of that kind.
//
// It lives in the package that declares what a default may be, because two
// callers need the identical answer: the compiler fills defaults in with it, and
// the generator decides with it whether a discovered value already equals its
// default. If those two conversions differed, generation would omit an attribute
// the compiler then filled with something else, and the resource would change on
// the first apply after an import that reported no changes.
//
// Matching a Go type is not the same as matching the declared kind: a datum for
// a float attribute written as int64 builds a perfectly valid KindInt value,
// which would sail past the kind check that exists to catch exactly this,
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
