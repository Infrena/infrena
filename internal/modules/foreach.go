package modules

import (
	"sort"
	"strconv"

	"github.com/infrena/infrena/internal/config"
	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/internal/expressions"
	"github.com/infrena/infrena/pkg/value"
)

// instance is one entry of a `for_each`.
type instance struct {
	Key   string
	Value value.Value
}

// forEachInstances resolves a resource's `for_each` to the instances it
// declares, or reports nil when the resource is declared once.
//
// Keys must be known. They decide which resources exist at all, so a key
// derived from another resource's attribute would leave the set of resources to
// create unknown at plan time, and a plan could not say what it was going to do.
//
// Values may be unknown: only the keys are load-bearing for identity.
//
// An empty list or map yields zero instances and is not an error. That is the
// optional-resource case, and it is why there is no `count: enabled ? 1 : 0`
// idiom here.
func (w *walker) forEachInstances(r *config.ResourceDecl, scope *Scope) ([]instance, bool) {
	decl := r.ForEach
	if decl.Name == "" {
		return nil, true
	}

	v := decl.Value
	if decl.HasExpressions {
		text, ok := v.AsString()
		if !ok {
			w.ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "`for_each` on resource " + strconv.Quote(r.Name) + " is not resolvable",
				Detail:   "An expression here must appear on its own, as the whole value.",
				Action:   "Write `for_each: ${var.zones}` or a literal list or map.",
				Origin:   decl.Origin,
			})
			return nil, false
		}
		e, parseDiags := expressions.Parse(text, decl.Origin)
		w.ds.Extend(parseDiags)
		if e == nil {
			return nil, false
		}
		evaluated, evalDiags := expressions.Evaluate(e, scope)
		w.ds.Extend(evalDiags)
		v = evaluated
	}

	if !v.Known {
		w.ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "`for_each` on resource " + strconv.Quote(r.Name) + " is not known yet",
			Detail: "It decides which resources exist, so it must be known before anything is " +
				"planned — the keys cannot come from another resource's attribute, because then " +
				"the plan could not say what it was going to create.\n" +
				"The VALUES may be unknown; only the keys may not.",
			Action: "Key it on a variable or a literal, and refer to the unknown thing inside the " +
				"resource instead.",
			Origin: decl.Origin,
		})
		return nil, false
	}

	switch v.Kind {
	case value.KindList:
		items, ok := v.Raw.([]value.Value)
		if !ok {
			return w.refuseForEachKind(r, decl, v)
		}
		out := make([]instance, 0, len(items))
		seen := map[string]value.Origin{}
		for _, item := range items {
			key, ok := item.AsString()
			if !ok {
				w.ds.Add(diag.Diagnostic{
					Severity: diag.SeverityError,
					Summary:  "`for_each` on resource " + strconv.Quote(r.Name) + " has an element that is not a string",
					Detail: "Each element of a list names one instance, so it has to be a name. " +
						"Found a " + item.Kind.String() + ".",
					Action: "Use a list of names, or a map whose keys are the names.",
					Origin: decl.Origin,
				})
				return nil, false
			}
			// Refused, not deduplicated: two entries with one key describe
			// two resources sharing an identity, and one would silently
			// overwrite the other in state.
			if _, dup := seen[key]; dup {
				w.ds.Add(diag.Diagnostic{
					Severity: diag.SeverityError,
					Summary:  "`for_each` on resource " + strconv.Quote(r.Name) + " lists " + strconv.Quote(key) + " twice",
					Detail: "Each entry names one instance, so two with the same name describe two " +
						"resources with one identity and the second would replace the first in state.",
					Action: "Remove the duplicate.",
					Origin: decl.Origin,
				})
				return nil, false
			}
			seen[key] = decl.Origin
			out = append(out, instance{Key: key, Value: item})
		}
		return out, true

	case value.KindMap:
		entries, ok := v.Raw.(map[string]value.Value)
		if !ok {
			return w.refuseForEachKind(r, decl, v)
		}
		keys := make([]string, 0, len(entries))
		for k := range entries {
			keys = append(keys, k)
		}
		// Sorted, because Go randomises map iteration and the expansion order
		// would otherwise differ between runs of one configuration — showing up
		// as a spurious diff rather than as an error.
		sort.Strings(keys)
		out := make([]instance, 0, len(keys))
		for _, k := range keys {
			out = append(out, instance{Key: k, Value: entries[k]})
		}
		return out, true
	}
	return w.refuseForEachKind(r, decl, v)
}

func (w *walker) refuseForEachKind(r *config.ResourceDecl, decl config.AttributeDecl, v value.Value) ([]instance, bool) {
	w.ds.Add(diag.Diagnostic{
		Severity: diag.SeverityError,
		Summary:  "`for_each` on resource " + strconv.Quote(r.Name) + " is a " + v.Kind.String() + ", not a list or a map",
		Detail:   "It names the instances to create, so it has to be a collection of names.",
		Action:   "Write a list of names, or a map whose keys are the names.",
		Origin:   decl.Origin,
	})
	return nil, false
}

// eachScope returns scope with ${each.key} and ${each.value} bound for one
// instance.
//
// Supplied as an ordinary variable named "each" holding a two-key map, so
// `${each.key}` is an existing path step over an existing kind and needs no new
// evaluation machinery. The parser rewrites the `each.` namespace onto that
// variable.
func eachScope(scope *Scope, in instance) *Scope {
	next := *scope
	next.Vars = scope.Vars.With("each", value.Map(map[string]value.Value{
		"key":   value.String(in.Key, value.SourceVariable),
		"value": in.Value,
	}, value.SourceVariable))
	return &next
}
