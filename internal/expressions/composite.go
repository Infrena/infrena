package expressions

import (
	"slices"
	"strings"

	"github.com/infrena/infrena/pkg/value"
)

// HasInterpolation reports whether s contains an interpolation to resolve.
//
// The same test config.decodeValue uses to set HasExpressions, said once here so
// the walk below and the decoder cannot come to differ about which leaves matter.
func HasInterpolation(s string) bool { return strings.Contains(s, "${") }

// WalkLeaves returns a copy of v with every interpolated string leaf replaced by
// what leaf returns for it (PLAN.md §10.1).
//
// THE WALK IS THE SHARED PART, and only the walk. What happens to a leaf differs
// by caller — compiler stage 6 records dependency edges and checks the reference
// against the schema, a module output does neither — so this takes a callback
// rather than a scope. Sharing the evaluation too would mean one of those
// callers silently acquiring the other's behaviour.
//
// Three places used to REFUSE a composite carrying an interpolation, each with
// its own copy of the refusal. They now share this. Three copies of a rule about
// where expressions may appear is three chances for a module output and a
// resource attribute to disagree about the same YAML.
//
// Structure is preserved exactly: same kinds, same keys, same order. KEYS ARE
// NEVER INTERPOLATED — a configuration's shape must not depend on a value, or
// what a file declares could not be read without resolving it.
//
// Leaves that hold no interpolation are returned untouched rather than reparsed.
// That is not an optimisation: parsing a literal that happens to contain a brace
// would change it.
func WalkLeaves(v value.Value, leaf func(src string, origin value.Origin) value.Value) value.Value {
	switch v.Kind {
	case value.KindList:
		items, ok := v.Raw.([]value.Value)
		if !ok {
			return v
		}
		out := make([]value.Value, len(items))
		for i, item := range items {
			out[i] = WalkLeaves(item, leaf)
		}
		// Rebuilt rather than mutated: the source slice may be shared with a
		// variable's value in the scope, and mutating it would make a second
		// reference to that variable see the resolved result — a value that
		// depended on evaluation order.
		return withRaw(v, out)

	case value.KindMap:
		m, ok := v.Raw.(map[string]value.Value)
		if !ok {
			return v
		}
		out := make(map[string]value.Value, len(m))
		for k, item := range m {
			out[k] = WalkLeaves(item, leaf)
		}
		return withRaw(v, out)

	case value.KindString:
		s, ok := v.AsString()
		if !ok || !HasInterpolation(s) {
			return v
		}
		origin := v.Origin
		return leaf(s, origin)

	default:
		return v
	}
}

// withRaw replaces a composite's contents, keeping everything else about it.
//
// Sensitivity is NOT recomputed here. A composite's own Sensitive flag and its
// leaves' are separate facts (pkg/value models per-leaf sensitivity, and
// value.Format redacts at that granularity), so a leaf that resolves to a secret
// is a secret leaf inside an ordinary map — which is what a reader needs to see.
// Marking the whole map sensitive because one leaf is would hide the other keys
// for no gain.
func withRaw(v value.Value, raw any) value.Value {
	out := v
	out.Raw = raw
	return out
}

// HasUnknownLeaf reports whether v, or any leaf inside it, is unknown.
//
// A composite whose leaves are not all resolved is not usable, and the two
// places that need to know are far apart: compiler stage 6 marks the composite
// unknown so the planner defers it rather than diffing a placeholder against a
// real value, and the executor re-resolves it after its dependencies exist. Said
// once here so those two cannot come to disagree about what "resolved" means.
func HasUnknownLeaf(v value.Value) bool {
	switch v.Kind {
	case value.KindList:
		items, ok := v.Raw.([]value.Value)
		if !ok {
			return false
		}
		return slices.ContainsFunc(items, HasUnknownLeaf)
	case value.KindMap:
		m, ok := v.Raw.(map[string]value.Value)
		if !ok {
			return false
		}
		for _, item := range m {
			if HasUnknownLeaf(item) {
				return true
			}
		}
		return false
	default:
		return !v.Known
	}
}

// WalkDeferred returns a copy of v with every unknown leaf that carries an
// expression replaced by what resolve returns for it.
//
// The counterpart to WalkLeaves, for the other end of the pipeline: WalkLeaves
// runs at compile time over leaves that still hold "${...}" TEXT, this runs at
// apply time over leaves that hold an unresolved EXPRESSION. Separate because the
// predicate differs, shaped alike so the structure handling is recognisably the
// same problem.
func WalkDeferred(v value.Value, resolve func(value.Value) value.Value) value.Value {
	switch v.Kind {
	case value.KindList:
		items, ok := v.Raw.([]value.Value)
		if !ok {
			return v
		}
		out := make([]value.Value, len(items))
		for i, item := range items {
			out[i] = WalkDeferred(item, resolve)
		}
		return withRaw(v, out)
	case value.KindMap:
		m, ok := v.Raw.(map[string]value.Value)
		if !ok {
			return v
		}
		out := make(map[string]value.Value, len(m))
		for k, item := range m {
			out[k] = WalkDeferred(item, resolve)
		}
		return withRaw(v, out)
	default:
		if !v.Known && v.Expr != nil {
			return resolve(v)
		}
		return v
	}
}
