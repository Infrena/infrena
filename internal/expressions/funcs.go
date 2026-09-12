package expressions

import (
	"fmt"
	"sort"
	"strings"

	"github.com/infrata/infrata/pkg/value"
)

// Func is a built-in expression function. Every one is pure, total and
// side-effect free.
type Func func(args []value.Value) (value.Value, error)

type builtin struct {
	fn    Func
	arity int // -1 for variadic
}

// Lookup returns a built-in function, its arity, and whether it exists.
// An arity of -1 means variadic.
func Lookup(name string) (Func, int, bool) {
	b, ok := builtins[name]
	return b.fn, b.arity, ok
}

// Names returns every built-in function name, sorted, for diagnostics.
func Names() []string {
	out := make([]string, 0, len(builtins))
	for name := range builtins {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// builtins is the complete set. PLAN.md §10.2 fixes it at SEVEN; adding one is a
// configuration-language change requiring an amendment to that section.
//
// Every one is pure, total and side-effect free, and that is a hard rule rather
// than a coincidence: `now()`, `uuid()` and reading a file are the three most
// often asked for next, and each silently breaks invariant 6 — the same
// configuration and state would plan differently on a second run, which is the
// property the whole plan/apply split rests on.
var builtins = map[string]builtin{
	"lower":   {arity: 1, fn: stringFunc(strings.ToLower)},
	"upper":   {arity: 1, fn: stringFunc(strings.ToUpper)},
	"trim":    {arity: 1, fn: stringFunc(strings.TrimSpace)},
	"replace": {arity: 3, fn: replaceFunc},
	"join":    {arity: 2, fn: joinFunc},
	"default": {arity: 2, fn: defaultFunc},
	"merge":   {arity: -1, fn: mergeFunc},
}

// sensitiveAnywhere reports whether a value, or any leaf inside it, is
// sensitive. Sensitivity is a per-leaf property: a list is classified when any
// element is, even when the list itself carries no flag.
func sensitiveAnywhere(v value.Value) bool {
	if v.Sensitive {
		return true
	}
	switch v.Kind {
	case value.KindList:
		items, ok := v.Raw.([]value.Value)
		if !ok {
			// A value whose Raw does not match its Kind cannot be inspected.
			// Its sensitivity is unknown, so classify it: over-redacting a
			// corrupt value is recoverable, leaking a secret is not.
			return true
		}
		for _, item := range items {
			if sensitiveAnywhere(item) {
				return true
			}
		}
	case value.KindMap:
		m, ok := v.Raw.(map[string]value.Value)
		if !ok {
			return true
		}
		for _, item := range m {
			if sensitiveAnywhere(item) {
				return true
			}
		}
	}
	return false
}

// anySensitive reports whether any argument contributes sensitivity.
//
// Every built-in whose result derives from all its arguments uses this rather
// than hand-writing its own union. Hand-written unions are how this shipped
// wrong twice: join() omitted its separator, and replace() omitted its search
// string — which let a secret search term reveal its own position through an
// unclassified result.
func anySensitive(args ...value.Value) bool {
	for _, a := range args {
		if sensitiveAnywhere(a) {
			return true
		}
	}
	return false
}

// stringFunc lifts a string transform into a Func, preserving sensitivity:
// transforming a secret does not declassify it.
func stringFunc(transform func(string) string) Func {
	return func(args []value.Value) (value.Value, error) {
		if len(args) != 1 {
			return value.Value{}, fmt.Errorf("expected 1 argument, got %d", len(args))
		}
		s, ok := args[0].AsString()
		if !ok {
			return value.Value{}, fmt.Errorf("expected a string, got %s", args[0].Kind)
		}
		return value.String(transform(s), value.SourceComputed).
			WithSensitive(anySensitive(args...)).
			WithOrigin(args[0].Origin), nil
	}
}

func replaceFunc(args []value.Value) (value.Value, error) {
	if len(args) != 3 {
		return value.Value{}, fmt.Errorf("expected 3 arguments (string, old, new), got %d", len(args))
	}
	in, ok := args[0].AsString()
	if !ok {
		return value.Value{}, fmt.Errorf("first argument must be a string, got %s", args[0].Kind)
	}
	old, ok := args[1].AsString()
	if !ok {
		return value.Value{}, fmt.Errorf("second argument must be a string, got %s", args[1].Kind)
	}
	replacement, ok := args[2].AsString()
	if !ok {
		return value.Value{}, fmt.Errorf("third argument must be a string, got %s", args[2].Kind)
	}
	return value.String(strings.ReplaceAll(in, old, replacement), value.SourceComputed).
		WithSensitive(anySensitive(args...)).
		WithOrigin(args[0].Origin), nil
}

func joinFunc(args []value.Value) (value.Value, error) {
	if len(args) != 2 {
		return value.Value{}, fmt.Errorf("expected 2 arguments (separator, list), got %d", len(args))
	}
	sep, ok := args[0].AsString()
	if !ok {
		return value.Value{}, fmt.Errorf("separator must be a string, got %s", args[0].Kind)
	}
	items, ok := args[1].Raw.([]value.Value)
	if !ok || args[1].Kind != value.KindList {
		return value.Value{}, fmt.Errorf("second argument must be a list, got %s", args[1].Kind)
	}

	parts := make([]string, 0, len(items))
	for _, item := range items {
		s, ok := item.AsString()
		if !ok {
			return value.Value{}, fmt.Errorf("join needs a list of strings, found %s", item.Kind)
		}
		parts = append(parts, s)
	}
	return value.String(strings.Join(parts, sep), value.SourceComputed).
		WithSensitive(anySensitive(args...)).
		WithOrigin(args[1].Origin), nil
}

// defaultFunc is the only built-in that inspects knownness: it exists to
// supply a fallback when a value is unknown or blank.
func defaultFunc(args []value.Value) (value.Value, error) {
	if len(args) != 2 {
		return value.Value{}, fmt.Errorf("expected 2 arguments (value, fallback), got %d", len(args))
	}
	if !args[0].Known {
		return args[1], nil
	}
	if s, ok := args[0].AsString(); ok && s == "" {
		return args[1], nil
	}
	return args[0], nil
}

// mergeFunc unions maps, with LATER arguments winning per key (PLAN.md §10.2).
//
// It exists because §12.1's provider block REPLACES rather than merges: making
// the union explicit is better than a rule that silently combines structures,
// where the combining is invisible in the configuration and a reader cannot tell
// which keys came from where.
//
// SENSITIVITY IS PER LEAF HERE, and this is the one built-in that does NOT wrap
// its result in WithSensitive(anySensitive(args...)). Every other one returns a
// STRING, where the whole result is the only thing there is to classify. A map is
// different: marking the whole thing sensitive because one leaf is would redact
// every key, hiding the ones a reader needs in order to act — and it is
// unnecessary, because each leaf arrives carrying its own flag and value.Format
// redacts at that granularity.
//
// A source map whose OWN flag is set has that classification pushed down onto the
// entries it contributes, rather than onto the result. That keeps the granularity
// where it is actionable and means no secret can arrive unclassified: a leaf is
// either marked itself or marked on the way in.
//
// This is the THIRD time this codebase has had to get a sensitivity union right.
// join() omitted its separator and replace() omitted its search string, and the
// second of those let a secret search term reveal its own position through an
// unclassified result. merge()'s result is the one most likely to be written into
// a tag, printed in a plan, and committed.
func mergeFunc(args []value.Value) (value.Value, error) {
	if len(args) < 2 {
		return value.Value{}, fmt.Errorf("expected at least 2 maps, got %d", len(args))
	}

	out := map[string]value.Value{}
	for i, a := range args {
		m, ok := a.Raw.(map[string]value.Value)
		if !ok || a.Kind != value.KindMap {
			return value.Value{}, fmt.Errorf("argument %d must be a map, got %s", i+1, a.Kind)
		}
		for k, v := range m {
			if a.Sensitive {
				v = v.WithSensitive(true)
			}
			// Later arguments win. Written as an unconditional assignment over
			// arguments in order, rather than a "does it exist" check, so which
			// side wins cannot depend on map iteration.
			out[k] = v
		}
	}
	return value.Map(out, value.SourceComputed).WithOrigin(args[0].Origin), nil
}
