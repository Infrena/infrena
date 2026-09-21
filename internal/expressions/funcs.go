package expressions

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/infrena/infrena/pkg/value"
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

// builtins is the complete set. It is fixed at seven; adding one is a change to
// the configuration language, not an implementation detail.
//
// Every one is pure, total and side-effect free, and that is a hard rule. `now()`,
// `uuid()` and reading a file are the three most often asked for next, and each
// would silently break plan determinism: the same configuration and state would
// plan differently on a second run, which is the property the plan/apply split
// rests on.
var builtins = map[string]builtin{
	"lower":   {arity: 1, fn: stringFunc(strings.ToLower)},
	"upper":   {arity: 1, fn: stringFunc(strings.ToUpper)},
	"trim":    {arity: 1, fn: stringFunc(strings.TrimSpace)},
	"replace": {arity: 3, fn: replaceFunc},
	"join":    {arity: 2, fn: joinFunc},
	"default": {arity: 2, fn: defaultFunc},
	"merge":   {arity: -1, fn: mergeFunc},
}

// anySensitive reports whether any argument contributes sensitivity, at any
// depth: sensitivity is a per-leaf property, so a list is classified when any
// element is, even when the list itself carries no flag.
//
// Every built-in whose result derives from all its arguments must use this rather
// than hand-write its own union — a hand-written one is how a function comes to
// omit an argument, and an omitted argument is a secret reaching a plan
// unclassified. The recursion itself is value.HasSensitive's, so there is exactly
// one answer to "is this sensitive" in the tree.
func anySensitive(args ...value.Value) bool {
	return slices.ContainsFunc(args, value.HasSensitive)
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

// mergeFunc unions maps, with later arguments winning per key.
//
// It exists because a provider instance's `defaults:` block replaces rather than
// merges: an explicit union is better than a rule that silently combines
// structures, where the combining is invisible in the file and a reader cannot
// tell which keys came from where.
//
// Sensitivity is per leaf here, and this is the one built-in that does not wrap
// its result in WithSensitive(anySensitive(args...)). Every other one returns a
// string, where the whole result is the only thing to classify. Marking a whole
// map sensitive because one leaf is would redact every key, hiding the ones a
// reader needs in order to act.
//
// A source map whose own flag is set has that classification pushed down onto the
// entries it contributes instead, so no secret arrives unclassified: a leaf is
// either marked itself or marked on the way in.
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
			// side wins cannot depend on map iteration order.
			out[k] = v
		}
	}
	return value.Map(out, value.SourceComputed).WithOrigin(args[0].Origin), nil
}
