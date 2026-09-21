package expressions

import (
	"slices"
	"strings"

	"github.com/infrena/infrena/pkg/value"
)

// HasInterpolation reports whether s contains an interpolation to resolve.
//
// The same test config.decodeValue uses to set HasExpressions, said once so the
// decoder and the walk below cannot come to disagree about which leaves matter.
func HasInterpolation(s string) bool { return strings.Contains(s, "${") }

// WalkLeaves returns a copy of v with every interpolated string leaf replaced by
// what leaf returns for it.
//
// Only the walk is shared. What happens to a leaf differs by caller — compiler
// stage 6 records dependency edges and checks the reference against the schema, a
// module output does neither — so this takes a callback rather than a scope.
//
// Structure is preserved exactly: same kinds, same keys, same order. Keys are
// never interpolated, because a configuration's shape must not depend on a value.
//
// A leaf holding no interpolation is returned untouched rather than reparsed:
// parsing a literal that happens to contain a brace would change it.
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
		// variable's value in the scope, so mutating it would make a second
		// reference to that variable see this one's resolved result.
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
// Sensitivity is deliberately not recomputed. Sensitivity is per leaf and
// value.Format redacts at that granularity, so a leaf that resolves to a secret
// stays a secret leaf inside an ordinary map; marking the whole map sensitive
// because one leaf is would hide the other keys for no gain.
func withRaw(v value.Value, raw any) value.Value {
	out := v
	out.Raw = raw
	return out
}

// HasUnknownLeaf reports whether v, or any leaf inside it, is unknown.
//
// Said once because two distant callers must agree on the answer: compiler stage
// 6 marks such a composite unknown so the planner defers it rather than diffing a
// placeholder against a real value, and the executor re-resolves it once its
// dependencies exist.
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
// The counterpart to WalkLeaves at the other end of the pipeline: WalkLeaves runs
// at compile time over leaves that still hold "${...}" text, this runs at apply
// time over leaves that hold an unresolved expression.
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
