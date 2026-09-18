package expressions

import (
	"bytes"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"text/template"

	"github.com/infrena/infrena/pkg/value"
)

// renderTemplate runs the `{{ }}` pass over a template's contents.
//
// TWO PASSES, IN THIS ORDER, and the order is the design. This one runs first
// and sees ONLY the arguments given at the call site; the `${ }` pass runs
// second over whatever this produces, against the project's own scope. So:
//
//	{{ }} sees what you passed it.  ${ } sees the project.
//
// That split is what makes the combination safe rather than merely powerful.
// A template engine handed the project's scope could write a secret into the
// rendered text as a LITERAL, and the second pass would then see a plain string
// with no sensitivity, which reaches the plan, the state and the report in
// clear. It cannot reach anything it was not handed, and what it was handed was
// evaluated by us — so we still know what was sensitive.
//
// It is also why `${bucket.arn}` inside a template keeps working: the first pass
// leaves `${...}` alone (it is not `{{...}}`), so the reference survives to the
// second pass with its deferral and its dependency edge intact.
func renderTemplate(name, content string, data map[string]any) (string, error) {
	t, err := template.New(name).Funcs(templateFuncs).Option("missingkey=error").Parse(content)
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	if err := t.Execute(&out, data); err != nil {
		return "", err
	}
	return out.String(), nil
}

// templateFuncs is the function set the `{{ }}` pass may use.
//
// OURS, NOT SPRIG, and the reason is not dependency count. sprig ships 211
// functions across 26 modules, and sixteen of them contradict guarantees this
// project asserts: `env` and `expandenv` read the environment directly, so a
// secret reaches rendered text as a literal with its sensitivity stripped;
// `uuidv4`, `now` and the `rand*` family break plan determinism (invariant 6),
// so two plans of one configuration would differ; and `getHostByName` performs
// a DNS lookup while rendering. Excluding them would mean maintaining a denylist
// against an API that grows on somebody else's release schedule, where a miss is
// silent.
//
// EVERY FUNCTION HERE IS PURE: same inputs, same output, no I/O, no clock, no
// randomness. That is a property a small set can actually keep, and it is what
// makes the rendered document a deterministic function of the configuration.
//
// The set is pinned by a test, the same way internal/expressions' own six
// built-ins are, so adding one is a deliberate act rather than a side effect.
var templateFuncs = template.FuncMap{
	// until 3 yields 0 1 2 — the counting most loops want, and the reason a
	// template needs any function at all.
	"until": func(n int) []int {
		if n < 0 {
			return nil
		}
		out := make([]int, n)
		for i := range out {
			out[i] = i
		}
		return out
	},
	// seq 1 3 yields 1 2 3, for the cases that count from something other
	// than zero and read worse written as an arithmetic offset.
	"seq": func(from, to int) []int {
		if to < from {
			return nil
		}
		out := make([]int, 0, to-from+1)
		for i := from; i <= to; i++ {
			out = append(out, i)
		}
		return out
	},
	// indent matters because this output is usually YAML or JSON, where the
	// wrong indentation is not cosmetic.
	"indent": func(spaces int, s string) string {
		if spaces < 0 {
			spaces = 0
		}
		pad := strings.Repeat(" ", spaces)
		lines := strings.Split(s, "\n")
		for i, l := range lines {
			if l == "" {
				continue // never pad a blank line into trailing whitespace
			}
			lines[i] = pad + l
		}
		return strings.Join(lines, "\n")
	},
	// quote produces a JSON/YAML string literal, escaping what has to be
	// escaped — the thing hand-written templates get wrong when a value
	// contains a quote or a newline.
	"quote": strconv.Quote,
	"upper": strings.ToUpper,
	"lower": strings.ToLower,
	"trim":  strings.TrimSpace,
	"join":  func(sep string, items []string) string { return strings.Join(items, sep) },
	// sortAlpha, so a template that ranges over something unordered can make
	// itself deterministic rather than hoping.
	"sortAlpha": func(items []string) []string {
		out := append([]string(nil), items...)
		sort.Strings(out)
		return out
	},
}

// TemplateFuncNames lists the function set, sorted. Exported for the test that
// pins it.
func TemplateFuncNames() []string {
	out := make([]string, 0, len(templateFuncs))
	for name := range templateFuncs {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// nativeOf converts a resolved value into what text/template can range over and
// index.
//
// Values arrive as value.Value, which carries provenance and sensitivity that a
// template has no use for and must not print — a template writing a Value would
// render the struct. Converting to native Go types keeps `{{.team}}` reading as
// the string it is.
//
// An UNKNOWN never reaches here: evaluateTemplate defers the whole render when
// any argument is unknown, because a template cannot represent "not yet".
func nativeOf(v value.Value) any {
	switch v.Kind {
	case value.KindMap:
		m, ok := v.Raw.(map[string]value.Value)
		if !ok {
			return v.Raw
		}
		out := make(map[string]any, len(m))
		for k, item := range m {
			out[k] = nativeOf(item)
		}
		return out
	case value.KindList:
		items, ok := v.Raw.([]value.Value)
		if !ok {
			return v.Raw
		}
		out := make([]any, len(items))
		for i, item := range items {
			out[i] = nativeOf(item)
		}
		return out
	}
	return v.Raw
}

// templateArguments evaluates a template call's argument map.
//
// Reports whether every argument is known, and whether any is sensitive. Both
// are decided HERE, at the call site, because this is the last point at which
// the values are still value.Value and still carry that information — once they
// are native Go types handed to the template, both facts are gone.
func templateArguments(v value.Value) (data map[string]any, known, sensitive bool, err error) {
	if !v.Known {
		return nil, false, value.HasSensitive(v), nil
	}
	if v.Kind != value.KindMap {
		return nil, true, false, fmt.Errorf("a template's argument is a map of names to values, not a %s", v.Kind)
	}
	m, ok := v.Raw.(map[string]value.Value)
	if !ok {
		return nil, true, false, fmt.Errorf("a template's argument is a map of names to values")
	}
	data = make(map[string]any, len(m))
	for k, item := range m {
		if !item.Known {
			return nil, false, value.HasSensitive(v), nil
		}
		data[k] = nativeOf(item)
	}
	return data, true, value.HasSensitive(v), nil
}
