package value

// Clone returns a copy of v that shares no mutable container with it.
//
// Raw holds a Go value matching Kind, and for KindMap and KindList that is a
// map or a slice — reference types. Copying the struct alone copies the
// HEADER: the copy and the original address the same backing map or array, so
// a caller that reaches into the copy's Raw and writes a key, or assigns to an
// element, writes through to the original. Composites hold Values recursively,
// so this walks the whole interior and rebuilds every container it finds.
//
// Expr is shared rather than copied. A parsed expression is immutable once
// built — the evaluator reads it and returns new Values rather than writing
// back into it — so sharing costs nothing, while copying an expression tree
// per cloned value would be paid on every refresh of every resource.
//
// Origin.Module is shared for the same reason: InModule allocates a fresh
// slice rather than appending to the one it was given, so no holder of an
// Origin can grow another's path.
//
// A Raw that does not hold what its Kind claims is copied as it stands. There
// is no container to rebuild, and inventing an empty one would change what the
// value holds in order to protect a caller that cannot reach into it anyway.
func (v Value) Clone() Value {
	switch v.Kind {
	case KindMap:
		m, ok := v.Raw.(map[string]Value)
		if !ok {
			return v
		}
		out := make(map[string]Value, len(m))
		for k, item := range m {
			out[k] = item.Clone()
		}
		v.Raw = out
	case KindList:
		items, ok := v.Raw.([]Value)
		if !ok {
			return v
		}
		out := make([]Value, len(items))
		for i, item := range items {
			out[i] = item.Clone()
		}
		v.Raw = out
	}
	return v
}

// CloneMap returns a copy of an attribute map whose values share no mutable
// container with the originals. A nil or empty map comes back as it went in,
// which keeps a resource with no attributes from allocating one.
func CloneMap(m map[string]Value) map[string]Value {
	if len(m) == 0 {
		return m
	}
	out := make(map[string]Value, len(m))
	for k, v := range m {
		out[k] = v.Clone()
	}
	return out
}
