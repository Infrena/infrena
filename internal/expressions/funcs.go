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

// builtins is the complete set. Spec §6 fixes it at six; adding one is a
// configuration-language change requiring a spec amendment.
var builtins = map[string]builtin{
	"lower":   {arity: 1, fn: stringFunc(strings.ToLower)},
	"upper":   {arity: 1, fn: stringFunc(strings.ToUpper)},
	"trim":    {arity: 1, fn: stringFunc(strings.TrimSpace)},
	"replace": {arity: 3, fn: replaceFunc},
	"join":    {arity: 2, fn: joinFunc},
	"default": {arity: 2, fn: defaultFunc},
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
