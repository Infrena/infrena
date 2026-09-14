package expressions

import (
	"sort"
	"strconv"
	"strings"

	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/pkg/value"
)

// applyPath walks v along steps and returns the value at the end.
//
// Sensitivity UNIONS along the whole path. pkg/value marks a container
// sensitive as well as its leaves (see CarrySensitivity), so a sensitive map
// variable carries the flag at the top and may carry nothing on the leaf.
// Returning the leaf as found would declassify it, and value.Format — the one
// and only redaction path — would then print a secret in clear. This is
// PLAN.md §10.2's rule ("sensitivity unions across ALL arguments") in a new
// position; join() and replace() each shipped it wrong.
func applyPath(v value.Value, steps []value.Step, ref string, origin value.Origin, ds *diag.Diagnostics) (value.Value, bool) {
	sensitive := v.Sensitive
	cur := v
	for _, st := range steps {
		if !cur.Known {
			// A dependency that does not exist yet. Leave it deferred with its
			// expression intact rather than reporting a missing member of a
			// value nobody has seen.
			return cur, false
		}
		next, ok := stepInto(cur, st, ref, origin, ds)
		if !ok {
			return value.Value{}, false
		}
		sensitive = sensitive || next.Sensitive
		cur = next
	}
	cur.Sensitive = sensitive
	return cur, true
}

func stepInto(v value.Value, st value.Step, ref string, origin value.Origin, ds *diag.Diagnostics) (value.Value, bool) {
	switch {
	case st.Kind == value.StepKey && v.Kind == value.KindMap:
		m, _ := v.Raw.(map[string]value.Value)
		if got, ok := m[st.Key]; ok {
			return got, true
		}
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "${" + ref + "} has no key " + strconv.Quote(st.Key),
			Detail:   "Known keys:\n  " + strings.Join(sortedKeys(m), "\n  "),
			Action:   "Correct the key.",
			Origin:   origin,
		})
		return value.Value{}, false

	case st.Kind == value.StepIndex && v.Kind == value.KindList:
		l, _ := v.Raw.([]value.Value)
		if st.Index < len(l) {
			return l[st.Index], true
		}
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary: "${" + ref + "} has " + strconv.Itoa(len(l)) +
				" entries; there is no index " + strconv.Itoa(st.Index),
			Detail: "An index must name an entry that exists. The list is fully resolved " +
				"before a plan is made, so this cannot become valid later.",
			Action: "Use an index below " + strconv.Itoa(len(l)) + ".",
			Origin: origin,
		})
		return value.Value{}, false

	case st.Kind == value.StepIndex && v.Kind == value.KindMap:
		m, _ := v.Raw.(map[string]value.Value)
		example := "<key>"
		if ks := sortedKeys(m); len(ks) > 0 {
			example = ks[0]
		}
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "${" + ref + "} is a map; it cannot be indexed",
			Detail:   "Brackets index a list. A map is read by key.",
			Action:   "Read it by key, as ${" + ref + "." + example + "}.",
			Origin:   origin,
		})
		return value.Value{}, false

	case st.Kind == value.StepKey && v.Kind == value.KindList:
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "${" + ref + "} is a list; it has no key " + strconv.Quote(st.Key),
			Detail:   "A dotted name reads a map key. A list is read by index.",
			Action:   "Index it, as ${" + ref + "[0]}.",
			Origin:   origin,
		})
		return value.Value{}, false

	default:
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "${" + ref + "} is " + kindName(v.Kind) + "; it has no members",
			Detail:   "Only a map or a list can be stepped into.",
			Action:   "Reference it whole, without a path.",
			Origin:   origin,
		})
		return value.Value{}, false
	}
}

// sortedKeys keeps a diagnostic byte-identical across runs (invariant 6). Go
// randomises map iteration, so listing keys unsorted would make two runs of
// identical input differ.
func sortedKeys(m map[string]value.Value) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func kindName(k value.Kind) string {
	switch k {
	case value.KindString:
		return "a string"
	case value.KindInt:
		return "an integer"
	case value.KindFloat:
		return "a number"
	case value.KindBool:
		return "a boolean"
	default:
		return "not a container"
	}
}
