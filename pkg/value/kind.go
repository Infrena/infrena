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
