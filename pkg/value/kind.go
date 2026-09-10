// Package value defines the typed, provenance-carrying values that flow through
// the infra engine. Resolved configuration never uses bare Go values or
// map[string]any — see spec §5.1.
package value

// Kind is the type of a Value. An unknown value still carries its Kind so that
// type checking happens at plan time rather than apply time.
type Kind uint8

const (
	KindInvalid Kind = iota
	KindString
	KindInt
	KindFloat
	KindBool
	KindList
	KindMap
)

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
// INVERSE OF Kind.String(), AND THEY MUST BE CHANGED TOGETHER. This is a
// deliberate switch rather than a shared table, so the pair reads as one unit
// in one file; TestKindStringAndParseKindAreInverses pins the round trip over
// every Kind, so drift cannot survive a test run.
//
// Because this exists, those six strings are no longer merely diagnostic
// output: they are what a user writes as `type:` in infra.yml, and CLAUDE.md
// makes the configuration language a product API. Renaming one breaks every
// configuration file that used it, so a rename needs the same deliberation as
// a state format change — see kindWireNames in json.go, which keeps the
// ON-DISK spelling independent of both and has its own migration path.
//
// "invalid" is deliberately NOT accepted. Kind.String() answers it for
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
// that must tell a user what is available.
//
// Sorted and fixed so an error message does not reorder itself between
// identical runs — invisible in a test suite, obvious to a user diffing two
// outputs.
func KindNames() []string {
	return []string{"boolean", "float", "integer", "list", "map", "string"}
}
