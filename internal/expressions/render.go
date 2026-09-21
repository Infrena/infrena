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
// Two passes, in this order:
//
//	{{ }} sees what you passed it.  ${ } sees the project.
//
// This one runs first and sees only the arguments given at the call site. Handed
// the project's scope instead, it could write a secret into the rendered text as
// a literal, and the second pass would see a plain string with no sensitivity —
// which reaches the plan, the state and the report in clear.
//
// It is also why `${bucket.arn}` inside a template keeps working: this pass
// leaves `${...}` alone, so the reference survives to the second pass with its
// deferral and its dependency edge intact.
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
// Every function here is pure: same inputs, same output, no I/O, no clock, no
// randomness. That is what makes a rendered document a deterministic function of
// the configuration, and it is why this is a small hand-written set rather than a
// general-purpose template library — several of the functions such libraries ship
// read the environment, the clock or a name server, and excluding them would mean
// maintaining a denylist against an API that grows on somebody else's schedule,
// where a miss is silent.
//
// The set is pinned by a test, so adding one is a deliberate act.
var templateFuncs = template.FuncMap{
	// until 3 yields 0 1 2 — the counting most loops want.
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
	// seq 1 3 yields 1 2 3, for loops that count from something other than zero.
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
	// quote produces a JSON/YAML string literal, escaping what has to be escaped
	// — what hand-written templates get wrong when a value contains a quote or a
	// newline.
	"quote": strconv.Quote,
	"upper": strings.ToUpper,
	"lower": strings.ToLower,
	"trim":  strings.TrimSpace,
	"join":  func(sep string, items []string) string { return strings.Join(items, sep) },
	// sortAlpha, so a template ranging over something unordered can make itself
	// deterministic.
	"sortAlpha": func(items []string) []string {
		out := append([]string(nil), items...)
		sort.Strings(out)
		return out
	},
}

// TemplateFuncNames lists the function set, sorted. Exported for the test that
// pins it, and for the diagnostic that lists what a template may call.
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
// Values arrive as value.Value, which carries provenance a template has no use
// for and must not print — `{{.team}}` on a Value would render the struct.
//
// An unknown never reaches here: evaluateTemplate defers the whole render when
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

// templateArguments evaluates a template call's argument map, reporting whether
// every argument is known and whether any is sensitive.
//
// Both are decided here because this is the last point at which the values are
// still value.Value: once they are the native Go types the template sees, both
// facts are gone.
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
