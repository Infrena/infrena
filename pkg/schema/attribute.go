// Package schema describes provider resource types as inspectable data.
//
// Schemas are plain values rather than struct tags so that `infra explain`,
// validation and default resolution all read one source, and so that
// environment-aware default resolvers can be functions. Spec §8.1.
package schema

import "github.com/infrata/infrata/pkg/value"

// DefaultContext is everything a default resolver is allowed to see.
//
// It deliberately excludes other resources' attributes: defaults must never
// depend on unknown values, so that they are always computable at plan time.
// Spec §7.3.
type DefaultContext struct {
	Environment     string
	EnvironmentType string // e.g. "production"
	Region          string
	Account         string
	Project         string
	Type            string
}

// DefaultFunc returns a default datum and whether one applies. Returning false
// means the attribute stays absent.
type DefaultFunc func(DefaultContext) (any, bool)

// Attribute describes a resource attribute.
type Attribute struct {
	Kind        value.Kind
	Required    bool
	Computed    bool // the provider sets it; configuration may not
	Sensitive   bool
	ForceNew    bool // a change replaces the resource rather than updating it
	Default     DefaultFunc
	Description string
	Validate    func(value.Value) error
}

// DatumValue converts what a DefaultFunc returned into a Value of the
// attribute's declared kind, reporting false if the datum is not of that kind.
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
// Matching a Go type is not the same as matching the declared kind: a resolver
// for a float attribute returning int64 builds a perfectly valid KindInt value,
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
