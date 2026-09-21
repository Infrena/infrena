// Package value defines the typed, provenance-carrying values that flow through
// the infrena engine. Resolved configuration never uses bare Go values or
// map[string]any.
package value

// Kind is the type of a Value. An unknown value still carries its Kind so that
// type checking happens at plan time rather than apply time.
type Kind uint8

const (
	// KindInvalid is the zero Kind, held by a Value whose type was never set.
	KindInvalid Kind = iota
	// KindString is a string.
	KindString
	// KindInt is a 64-bit signed integer.
	KindInt
	// KindFloat is a 64-bit floating-point number.
	KindFloat
	// KindBool is a boolean.
	KindBool
	// KindList is an ordered list of Values.
	KindList
	// KindMap is a string-keyed map of Values.
	KindMap
)

// String names the kind as it is written in configuration. These are the
// spellings ParseKind accepts; the two must change together.
func (k Kind) String() string {
	switch k {
	case KindString:
		return "string"
	case KindInt:
		return "integer"
	case KindFloat:
		return "float"
	case KindBool:
		return "boolean"
	case KindList:
		return "list"
	case KindMap:
		return "map"
	default:
		return "invalid"
	}
}

// ParseKind resolves a type name written in configuration back to its Kind.
//
// It is the inverse of Kind.String(), and the two must change together. Those
// six strings are what a user writes as `type:` in infrena.yml, so renaming one
// breaks every configuration file that used it. The on-disk spelling is
// deliberately a separate table — see kindWireNames in json.go — with its own
// migration path.
//
// "invalid" is deliberately not accepted. Kind.String() answers it for
// KindInvalid so a diagnostic can name an unset Kind, but `type: invalid` in
// configuration is a user error and must reach the unknown-type diagnostic
// rather than quietly producing the zero Kind.
func ParseKind(name string) (Kind, bool) {
	switch name {
	case "string":
		return KindString, true
	case "integer":
		return KindInt, true
	case "float":
		return KindFloat, true
	case "boolean":
		return KindBool, true
	case "list":
		return KindList, true
	case "map":
		return KindMap, true
	default:
		return KindInvalid, false
	}
}

// KindNames returns every type name ParseKind accepts, sorted, for diagnostics
// that must tell a user what is available. The order is fixed so an error
// message does not reorder itself between identical runs.
func KindNames() []string {
	return []string{"boolean", "float", "integer", "list", "map", "string"}
}
