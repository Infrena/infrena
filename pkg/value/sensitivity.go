package value

// This file carries sensitivity ACROSS a boundary where a value leaves the
// engine and comes back. It never renders anything and never decides what a
// user sees: Format remains the one and only redaction path (see format.go's
// note on why there is exactly one). What is fixed here is the input to it —
// a value that reaches Format having lost Sensitive: true is printed in clear,
// and no amount of correctness in Format can recover it.
//
// The boundary is a provider call. A provider receives raw data and returns
// raw data; it has no idea that one of the strings it was handed came from
// ${db.password} and is therefore secret. Schema-declared sensitivity survives
// because a provider re-derives it from its own schema, but PROPAGATED
// sensitivity — the kind spec §36 creates by flowing a secret through a
// reference — exists only in the engine, and is lost the moment the engine
// records what the provider returned instead of what it knew.

// CarrySensitivity returns dst carrying any sensitivity src has, per leaf.
//
// It only ever ADDS. Nothing here can clear a flag: a value that is already
// sensitive stays sensitive whatever src says, so schema-declared sensitivity
// on the provider's side and propagated sensitivity on the engine's side
// combine rather than compete. Values with no sensitivity on either side come
// back untouched, which is what keeps "not sensitive" meaningful — a
// classifier that marked everything would redact a plan into uselessness and
// teach users to ignore the marker.
//
// Structure is matched leaf by leaf, because sensitivity is per-leaf: a map
// that is not itself sensitive may hold one key that is, and marking the whole
// map would hide four harmless attributes to protect one secret.
//
// When the structures cannot be matched — different kinds, or a Raw that does
// not hold what its Kind claims — and src carries sensitivity SOMEWHERE
// inside, dst is marked sensitive whole. That is the conservative direction on
// purpose. The alternative, returning dst unchanged because the shapes
// disagreed, is a silent decision to print something that may be a secret, and
// the failure is invisible in exactly the case that matters.
func CarrySensitivity(dst, src Value) Value {
	if dst.Sensitive || !HasSensitive(src) {
		return dst
	}

	switch {
	case dst.Kind == KindMap && src.Kind == KindMap:
		dm, dok := dst.Raw.(map[string]Value)
		sm, sok := src.Raw.(map[string]Value)
		if !dok || !sok {
			break
		}
		out := make(map[string]Value, len(dm))
		for k, dv := range dm {
			if sv, ok := sm[k]; ok {
				out[k] = CarrySensitivity(dv, sv)
				continue
			}
			out[k] = dv
		}
		dst.Raw = out
		dst.Sensitive = src.Sensitive
		return dst

	case dst.Kind == KindList && src.Kind == KindList:
		dl, dok := dst.Raw.([]Value)
		sl, sok := src.Raw.([]Value)
		if !dok || !sok {
			break
		}
		out := make([]Value, len(dl))
		copy(out, dl)
		for i := range out {
			if i < len(sl) {
				out[i] = CarrySensitivity(out[i], sl[i])
			}
		}
		dst.Raw = out
		dst.Sensitive = src.Sensitive
		return dst
	}

	// Scalars, mismatched kinds and malformed composites all land here. src
	// is sensitive somewhere and there is no structure to align, so the whole
	// value is treated as sensitive.
	return dst.WithSensitive(true)
}

// CarrySensitivityAttrs returns a copy of dst whose values carry any
// sensitivity the matching key in src has. Keys absent from src are copied
// unchanged; keys present only in src are ignored, since nothing in dst can be
// holding their data.
//
// It returns a new map rather than writing through: the caller's src is
// usually recorded state or a desired resource that other code still reads,
// and a helper that mutated either would be a data race waiting for the first
// concurrent apply.
func CarrySensitivityAttrs(dst, src map[string]Value) map[string]Value {
	if len(dst) == 0 {
		return dst
	}
	out := make(map[string]Value, len(dst))
	for name, dv := range dst {
		if sv, ok := src[name]; ok {
			out[name] = CarrySensitivity(dv, sv)
			continue
		}
		out[name] = dv
	}
	return out
}

// HasSensitive reports whether a value, or any leaf inside a composite, is
// marked sensitive.
//
// It recurses for the same reason Format does: a composite is not itself
// marked when only one of its leaves is, so a top-level check alone would
// report a map containing a password as carrying no sensitivity at all.
func HasSensitive(v Value) bool {
	if v.Sensitive {
		return true
	}
	switch v.Kind {
	case KindMap:
		m, ok := v.Raw.(map[string]Value)
		if !ok {
			return false
		}
		for _, item := range m {
			if HasSensitive(item) {
				return true
			}
		}
	case KindList:
		items, ok := v.Raw.([]Value)
		if !ok {
			return false
		}
		for _, item := range items {
			if HasSensitive(item) {
				return true
			}
		}
	}
	return false
}
