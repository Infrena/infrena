package expressions

import (
	"fmt"
	"sort"
	"strings"

	"infra/pkg/value"
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
			WithSensitive(args[0].Sensitive).
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
		WithSensitive(args[0].Sensitive || args[2].Sensitive).
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
	sensitive := args[0].Sensitive || args[1].Sensitive
	for _, item := range items {
		s, ok := item.AsString()
		if !ok {
			return value.Value{}, fmt.Errorf("join needs a list of strings, found %s", item.Kind)
		}
		sensitive = sensitive || item.Sensitive
		parts = append(parts, s)
	}
	return value.String(strings.Join(parts, sep), value.SourceComputed).
		WithSensitive(sensitive).
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
