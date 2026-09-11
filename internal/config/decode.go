package config

import (
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"infra/internal/diag"
	"infra/pkg/value"
)

// Decode converts parsed YAML into typed declarations, collecting every problem
// rather than stopping at the first.
func Decode(files []File) (*ProjectDecl, diag.Diagnostics) {
	var ds diag.Diagnostics
	out := &ProjectDecl{VariableValues: map[string]value.Value{}}

	// Uniqueness is tracked across the whole decode rather than per file: spec
	// §5.2 requires logical names be unique within a module, and M4 adds more
	// files to the same root module.
	//
	// Variables, environments and resources get SEPARATE sets. They are
	// separate namespaces: `${db}` is a variable reference and `${db.host}` a
	// resource reference, and stage 6 already tells them apart.
	seenResources := map[string]value.Origin{}
	seenVariables := map[string]value.Origin{}
	// seenEnvironments maps an environment name to its index in
	// out.Environments, because the block and the file MERGE into one decl.
	seenEnvironments := map[string]int{}

	for _, f := range files {
		doc := documentRoot(f.Root)
		if doc == nil {
			// Only the project file must have content. An empty variables.yml
			// or an empty environment file declares nothing, which is a
			// perfectly ordinary state for a file a user has just created.
			if f.Kind != FileProject {
				continue
			}
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "configuration file is empty",
				Origin:   value.Origin{File: f.Path},
				Action:   "Add a `project` name and a `resources` block.",
			})
			continue
		}
		if doc.Kind != yaml.MappingNode {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  f.Kind.String() + " must be a mapping",
				Detail:   topLevelShapeDetail(f),
				Origin:   originOf(f.Path, doc),
			})
			continue
		}

		switch f.Kind {
		case FileProject:
			// Assigned ONLY here. It was previously assigned for every file in
			// the loop, which with M4's extra files would make
			// ProjectDecl.Origin the last environment file's — so every
			// diagnostic that points at "the project" would point at
			// environments/zulu.yml.
			out.Origin = originOf(f.Path, doc)
			decodeDocument(f.Path, doc, out, &ds, seenResources, seenVariables, seenEnvironments)
		case FileVariables:
			decodeVariableValues(f.Path, doc, out, &ds)
		case FileEnvironment:
			decodeEnvironmentBody(f.Path, f.Environment, doc, out, &ds, seenEnvironments)
		}
	}

	sort.Slice(out.Resources, func(i, j int) bool { return out.Resources[i].Name < out.Resources[j].Name })
	sort.Slice(out.Variables, func(i, j int) bool { return out.Variables[i].Name < out.Variables[j].Name })
	sort.Slice(out.Environments, func(i, j int) bool { return out.Environments[i].Name < out.Environments[j].Name })
	// Sorted ONCE, here, so no consumer has to. Overrides are built by
	// appending in file order, which is the filesystem's order across the
	// block and the file, not the user's.
	for i := range out.Environments {
		o := out.Environments[i].Overrides
		sort.Slice(o, func(a, b int) bool { return o[a].Name < o[b].Name })
	}
	return out, ds
}

// topLevelShapeDetail explains what the top level of each file kind holds.
// A generic "must be a mapping" is true of all three and actionable for none.
func topLevelShapeDetail(f File) string {
	switch f.Kind {
	case FileVariables:
		return "The top level of " + VariablesFileName + " is a flat mapping of variable name to value, for example `region: us-east-1`."
	case FileEnvironment:
		return "The top level of an environment file is a mapping of variable name to value, for example `replicas: 10`."
	default:
		return "The top level of " + ProjectFileName + " must be a set of keys such as `project` and `resources`."
	}
}

func decodeDocument(path string, doc *yaml.Node, out *ProjectDecl, ds *diag.Diagnostics,
	seenResources, seenVariables map[string]value.Origin, seenEnvironments map[string]int) {
	for i := 0; i+1 < len(doc.Content); i += 2 {
		key, val := doc.Content[i], doc.Content[i+1]
		switch key.Value {
		case "project":
			if text, ok := requireScalar(path, "`project`", val, ds); ok {
				out.Project = text
			}
		case "resources":
			decodeResources(path, val, out, ds, seenResources)
		case "variables":
			decodeVariables(path, val, out, ds, seenVariables)
		case "environments":
			decodeEnvironments(path, val, out, ds, seenEnvironments)
		default:
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityWarning,
				Summary:  "unrecognised top-level key " + strconv.Quote(key.Value),
				Detail:   ProjectFileName + " understands `project`, `resources`, `variables` and `environments`.",
				Origin:   originOf(path, key),
			})
		}
	}
}

func decodeResources(path string, node *yaml.Node, out *ProjectDecl, ds *diag.Diagnostics, seen map[string]value.Origin) {
	if node.Kind != yaml.MappingNode {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "`resources` must be a mapping of logical name to resource",
			Origin:   originOf(path, node),
		})
		return
	}

	for i := 0; i+1 < len(node.Content); i += 2 {
		nameNode, body := node.Content[i], node.Content[i+1]
		r := &ResourceDecl{
			Name:       nameNode.Value,
			Attributes: map[string]AttributeDecl{},
			Origin:     originOf(path, nameNode),
		}

		// A duplicate name is silent resource loss, not a stylistic problem:
		// M2 keys its resource map by address, so one definition simply
		// disappears from the plan. Stage 2 is the only stage that still has
		// the line numbers to say where the other one is.
		if first, dup := seen[r.Name]; dup {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "resource " + strconv.Quote(r.Name) + " is defined more than once",
				Detail:   "A logical name must be unique within a module. " + strconv.Quote(r.Name) + " is also defined at " + describeOrigin(first) + ".",
				Action:   "Rename one of them, or merge the two definitions.",
				Origin:   r.Origin,
			})
			continue
		}
		seen[r.Name] = r.Origin

		if body.Kind != yaml.MappingNode {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "resource " + strconv.Quote(r.Name) + " must be a mapping",
				Origin:   originOf(path, body),
			})
			continue
		}

		// typeReported records that `type` was present but unusable, so the
		// "has no `type`" check below does not fire a second, misleading
		// diagnostic for the same key.
		typeReported := false

		// Keys are tracked separately from r.Attributes because `type`,
		// `depends_on` and `lifecycle` are keys too, and repeating one of those
		// silently loses a value just as an attribute does.
		seenKeys := map[string]value.Origin{}

		for j := 0; j+1 < len(body.Content); j += 2 {
			key, val := body.Content[j], body.Content[j+1]
			keyOrigin := originOf(path, key)
			if first, dup := seenKeys[key.Value]; dup {
				ds.Add(diag.Diagnostic{
					Severity: diag.SeverityError,
					Summary:  strconv.Quote(key.Value) + " is set more than once on resource " + strconv.Quote(r.Name),
					Detail:   "The last assignment would silently win. " + strconv.Quote(key.Value) + " is also set at " + describeOrigin(first) + ".",
					Action:   "Remove one of the two assignments.",
					Origin:   keyOrigin,
				})
				continue
			}
			seenKeys[key.Value] = keyOrigin

			switch key.Value {
			case "type":
				text, ok := requireScalar(path, "`type`", val, ds)
				if !ok {
					// The `type` key is present but unusable. Reporting "has no
					// `type`" as well would name a symptom rather than the
					// problem.
					typeReported = true
					break
				}
				r.Type = text
			case "depends_on":
				if val.Kind != yaml.SequenceNode {
					ds.Add(diag.Diagnostic{
						Severity: diag.SeverityError,
						Summary:  "`depends_on` must be a list",
						Detail:   "A scalar silently produces no dependencies, and a missing edge means a resource can be created before what it depends on.",
						Action:   "Write depends_on: [" + val.Value + "]",
						Origin:   originOf(path, val),
					})
					break
				}
				for i, item := range val.Content {
					text, ok := requireScalar(path, "`depends_on`["+strconv.Itoa(i)+"]", item, ds)
					if !ok {
						continue
					}
					r.DependsOn = append(r.DependsOn, text)
				}
			case "lifecycle":
				decodeLifecycle(path, val, r, ds)
			default:
				v, hasExpr := decodeValue(path, "attribute "+strconv.Quote(key.Value), val, ds)
				r.Attributes[key.Value] = AttributeDecl{
					Name:           key.Value,
					Value:          v,
					HasExpressions: hasExpr,
					Origin:         keyOrigin,
				}
			}
		}

		if r.Type == "" && !typeReported {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "resource " + strconv.Quote(r.Name) + " has no `type`",
				Detail:   "Every resource must name the provider resource type it manages, for example `type: test.database`.",
				Action:   "Add a `type` key to " + strconv.Quote(r.Name) + ".",
				Origin:   r.Origin,
			})
		}
		out.Resources = append(out.Resources, r)
	}
}

func decodeLifecycle(path string, node *yaml.Node, r *ResourceDecl, ds *diag.Diagnostics) {
	if node.Kind != yaml.MappingNode {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "`lifecycle` must be a mapping",
			Origin:   originOf(path, node),
		})
		return
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, val := node.Content[i], node.Content[i+1]
		switch key.Value {
		case "prevent_destroy":
			if b, ok := decodeLifecycleBool(path, key, val, ds); ok {
				r.Lifecycle.PreventDestroy = b
			}
		case "retain":
			if b, ok := decodeLifecycleBool(path, key, val, ds); ok {
				r.Lifecycle.Retain = b
			}
		default:
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "unknown lifecycle option " + strconv.Quote(key.Value),
				Detail:   "Supported options are `prevent_destroy` and `retain`.",
				Origin:   originOf(path, key),
			})
		}
	}
}

// decodeLifecycleBool decodes a lifecycle flag, letting the YAML decoder judge
// what is boolean rather than comparing the raw scalar text.
//
// Raw comparison against "true" is not merely imprecise here, it is unsafe:
// `prevent_destroy: True` and `prevent_destroy: TRUE` both carry the !!bool tag
// but scalar text that is not literally "true", so a raw check silently
// disables a destruction guard the user believed they had enabled. A quoted
// "true" is a string and is rejected loudly rather than silently accepted.
func decodeLifecycleBool(path string, key, val *yaml.Node, ds *diag.Diagnostics) (bool, bool) {
	var b bool
	if err := val.Decode(&b); err != nil {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "lifecycle option " + strconv.Quote(key.Value) + " must be true or false",
			Detail:   "Got " + strconv.Quote(val.Value) + ", which is not a boolean. Note a quoted value is a string.",
			Action:   "Write " + key.Value + ": true (unquoted).",
			Origin:   originOf(path, val),
		})
		return false, false
	}
	return b, true
}

// requireScalar reads a node's scalar text, refusing anything that is not one.
//
// Reading node.Value without checking the node's kind is the defect class that
// has already bitten this branch twice. A mapping or sequence node has an empty
// Value, so the value silently became ""; an alias node's Value is the
// anchor's name, so `type: *base` silently became the string "base"; and a
// !!null scalar's Value is "" too, so a blank required attribute would satisfy
// requiredness in M2's stage 7. None of them produced a diagnostic.
//
// `what` names the offending position, already quoted or backticked.
func requireScalar(path, what string, node *yaml.Node, ds *diag.Diagnostics) (string, bool) {
	switch {
	case node.Kind == yaml.AliasNode:
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  what + " is a YAML alias, which M1 does not resolve",
			Detail:   "An alias node carries the anchor's name rather than its value, so accepting it would silently substitute " + strconv.Quote(node.Value) + ".",
			Action:   "Write the value out in full.",
			Origin:   originOf(path, node),
		})
	case node.Kind != yaml.ScalarNode:
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  what + " must be a single value, not a list or a mapping",
			Detail:   "A list or mapping has no scalar text, so it would silently read as an empty value.",
			Origin:   originOf(path, node),
		})
	case node.Tag == "!!null":
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  what + " has no value",
			Detail:   "An empty value would silently read as the empty string, which is indistinguishable from a value that was deliberately set to \"\".",
			Action:   "Give it a value, or remove the key.",
			Origin:   originOf(path, node),
		})
	default:
		return node.Value, true
	}
	return "", false
}

// decodeValue converts a YAML node into a typed Value, reporting whether any
// string within it contains an interpolation.
//
// `what` names the position being decoded, for diagnostics.
func decodeValue(path, what string, node *yaml.Node, ds *diag.Diagnostics) (value.Value, bool) {
	origin := originOf(path, node)

	switch node.Kind {
	case yaml.SequenceNode:
		items := make([]value.Value, 0, len(node.Content))
		anyExpr := false
		for i, item := range node.Content {
			v, has := decodeValue(path, what+"["+strconv.Itoa(i)+"]", item, ds)
			anyExpr = anyExpr || has
			items = append(items, v)
		}
		return value.List(items, value.SourceExplicit).WithOrigin(origin), anyExpr

	case yaml.MappingNode:
		items := map[string]value.Value{}
		anyExpr := false
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i]
			v, has := decodeValue(path, what+"."+key.Value, node.Content[i+1], ds)
			anyExpr = anyExpr || has
			items[key.Value] = v
		}
		return value.Map(items, value.SourceExplicit).WithOrigin(origin), anyExpr

	default:
		return decodeScalar(path, what, node, ds, origin)
	}
}

func decodeScalar(path, what string, node *yaml.Node, ds *diag.Diagnostics, origin value.Origin) (value.Value, bool) {
	raw, ok := requireScalar(path, what, node, ds)
	if !ok {
		// KindInvalid rather than an empty string: a value that could not be
		// read must not masquerade as one that was.
		return value.Value{Source: value.SourceExplicit, Origin: origin}, false
	}

	// An interpolation is kept verbatim; stage 6 parses it in M2.
	if strings.Contains(raw, "${") {
		return value.String(raw, value.SourceExplicit).WithOrigin(origin), true
	}

	// A quoted scalar is always a string, whatever it looks like.
	if node.Style == yaml.DoubleQuotedStyle || node.Style == yaml.SingleQuotedStyle {
		return value.String(raw, value.SourceExplicit).WithOrigin(origin), false
	}

	switch node.Tag {
	case "!!int":
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return value.Int(n, value.SourceExplicit).WithOrigin(origin), false
		}
	case "!!float":
		if f, err := strconv.ParseFloat(raw, 64); err == nil {
			return value.Float(f, value.SourceExplicit).WithOrigin(origin), false
		}
	case "!!bool":
		// Let the decoder judge, exactly as decodeLifecycleBool does. `True`
		// and `TRUE` carry the !!bool tag but scalar text that is not literally
		// "true", so a raw comparison silently yields the wrong boolean for
		// every attribute in every resource, with no diagnostic.
		var b bool
		if err := node.Decode(&b); err == nil {
			return value.Bool(b, value.SourceExplicit).WithOrigin(origin), false
		}
	}
	return value.String(raw, value.SourceExplicit).WithOrigin(origin), false
}

func documentRoot(n *yaml.Node) *yaml.Node {
	// An empty file leaves the root at its zero value rather than producing a
	// DocumentNode: yaml.Unmarshal never invokes the decoder for empty input.
	if n == nil || n.Kind == 0 {
		return nil
	}
	if n.Kind == yaml.DocumentNode {
		if len(n.Content) == 0 {
			return nil
		}
		return n.Content[0]
	}
	return n
}

// describeOrigin renders an origin for use inside a sentence, naming the file
// only when it differs from the one the diagnostic already points at.
func describeOrigin(o value.Origin) string {
	if o.Line == 0 {
		return o.File
	}
	return "line " + strconv.Itoa(o.Line) + " of " + o.File
}

func originOf(path string, n *yaml.Node) value.Origin {
	if n == nil {
		return value.Origin{File: path}
	}
	return value.Origin{File: path, Line: n.Line, Column: n.Column}
}

// variableTypeList renders the accepted type spellings for a diagnostic.
//
// The spellings come from pkg/value, which owns them in both directions:
// value.ParseKind is the inverse of value.Kind.String(), pinned as such by
// TestKindStringAndParseKindAreInverses. This package deliberately keeps NO
// table of its own — a second copy would drift from the one the parser
// actually consults.
//
// value.KindNames is already sorted, so the message does not reorder itself
// between identical runs.
func variableTypeList() string {
	return strings.Join(value.KindNames(), ", ")
}

func decodeVariables(path string, node *yaml.Node, out *ProjectDecl, ds *diag.Diagnostics, seen map[string]value.Origin) {
	if node.Kind != yaml.MappingNode {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "`variables` must be a mapping of variable name to declaration",
			Detail:   "Each variable is a key with `type`, `default`, `min` and `max` beneath it (PLAN.md §9).",
			Origin:   originOf(path, node),
		})
		return
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		nameNode, body := node.Content[i], node.Content[i+1]
		origin := originOf(path, nameNode)

		// yaml.v3 does not deduplicate mapping keys when decoding into a Node,
		// so both entries arrive here and the last would silently win —
		// discarding a type or a bound the user wrote.
		if first, dup := seen[nameNode.Value]; dup {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "variable " + strconv.Quote(nameNode.Value) + " is declared more than once",
				Detail:   "The last declaration would silently win, discarding a type or a bound. " + strconv.Quote(nameNode.Value) + " is also declared at " + describeOrigin(first) + ".",
				Action:   "Remove one of the two declarations, or merge them.",
				Origin:   origin,
			})
			continue
		}
		seen[nameNode.Value] = origin
		out.Variables = append(out.Variables, decodeVariable(path, nameNode.Value, body, origin, ds))
	}
}

func decodeVariable(path, name string, body *yaml.Node, origin value.Origin, ds *diag.Diagnostics) VariableDecl {
	v := VariableDecl{Name: name, Origin: origin}

	if body.Kind != yaml.MappingNode {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "variable " + strconv.Quote(name) + " must be a mapping",
			Detail:   "A `variables:` entry is a schema — `type`, `default`, `min`, `max` — not a value. A bare value here would be ambiguous with a declaration whose type is `map`.",
			Action:   "Write `default:` beneath " + strconv.Quote(name) + ", or put the value in " + VariablesFileName + " instead.",
			Origin:   originOf(path, body),
		})
		return v
	}

	// minNode and maxNode are held back and checked AFTER the whole mapping is
	// walked. `type:` may appear textually after `min:`, and an implementation
	// that checks each key as it goes would accept `min: 1` on a string
	// whenever the file happened to be written in that order.
	var minNode, maxNode *yaml.Node
	// typeReported and defaultReported suppress the "must specify at least a
	// type or a default" check below when the key WAS present but unusable.
	// Telling a user their declaration is empty, one line under a diagnostic
	// explaining why their `type` was rejected, describes a symptom.
	typeReported := false
	defaultReported := false
	seenKeys := map[string]value.Origin{}

	for i := 0; i+1 < len(body.Content); i += 2 {
		key, val := body.Content[i], body.Content[i+1]
		keyOrigin := originOf(path, key)
		if first, dup := seenKeys[key.Value]; dup {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  strconv.Quote(key.Value) + " is set more than once on variable " + strconv.Quote(name),
				Detail:   "The last assignment would silently win. " + strconv.Quote(key.Value) + " is also set at " + describeOrigin(first) + ".",
				Action:   "Remove one of the two assignments.",
				Origin:   keyOrigin,
			})
			continue
		}
		seenKeys[key.Value] = keyOrigin

		switch key.Value {
		case "type":
			text, ok := requireScalar(path, "variable "+strconv.Quote(name)+"'s `type`", val, ds)
			if !ok {
				typeReported = true
				break
			}
			kind, known := value.ParseKind(text)
			if !known {
				ds.Add(diag.Diagnostic{
					Severity: diag.SeverityError,
					Summary:  "unknown variable type " + strconv.Quote(text),
					Detail:   "Variable " + strconv.Quote(name) + " declares a type the engine does not have. Supported types are " + variableTypeList() + ".",
					Action:   "Change `type` to one of " + variableTypeList() + ", or remove it to leave " + strconv.Quote(name) + " untyped.",
					Origin:   originOf(path, val),
				})
				typeReported = true
				break
			}
			v.Type = kind
		case "default":
			dv, hasExpr := decodeValue(path, "variable "+strconv.Quote(name)+"'s `default`", val, ds)
			if hasExpr {
				defaultReported = true
				ds.Add(diag.Diagnostic{
					Severity: diag.SeverityError,
					Summary:  "variable " + strconv.Quote(name) + "'s default contains an interpolation",
					Detail:   "Defaults are resolved before any expression scope exists, so `${...}` here has nothing to refer to. Accepting it would store the literal text " + strconv.Quote(val.Value) + " as the default.",
					Action:   "Write a literal value, or set " + strconv.Quote(name) + " per environment instead.",
					Origin:   originOf(path, val),
				})
				break
			}
			v.Default, v.HasDefault = dv, true
		case "min":
			minNode = val
		case "max":
			maxNode = val
		default:
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "unknown key " + strconv.Quote(key.Value) + " in variable " + strconv.Quote(name),
				Detail:   "A variable declaration understands `type`, `default`, `min` and `max`.",
				Action:   "Remove " + strconv.Quote(key.Value) + ".",
				Origin:   keyOrigin,
			})
		}
	}

	v.Min, v.HasMin = decodeBound(path, name, "min", minNode, v.Type, typeReported, ds)
	v.Max, v.HasMax = decodeBound(path, name, "max", maxNode, v.Type, typeReported, ds)

	// A declaration with neither a type nor a default carries no information:
	// it names a variable and says nothing about it, so stage 4 has nothing to
	// validate against and nothing to fall back on.
	//
	// This is an ERROR rather than a dropped declaration with a warning.
	// Dropping it discards something the user wrote and continues as though
	// they had not — the silent-loss shape this engine refuses everywhere
	// else, with a log line in front of it. A user who writes `replicas:` under
	// `variables:` meant something by it; the only safe response is to say the
	// declaration is incomplete, at the line it is on, and stop.
	//
	// Three suppressions, all of the same kind: say the root cause once.
	// typeReported and defaultReported mean the key WAS present but unusable
	// and has already been reported. A present min or max means decodeBound
	// just said "add `type: integer` or `type: float`" — the same advice this
	// would give, about the same declaration, one line apart.
	incomplete := v.Type == value.KindInvalid && !v.HasDefault
	alreadyExplained := typeReported || defaultReported || minNode != nil || maxNode != nil
	if incomplete && !alreadyExplained {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "variable " + strconv.Quote(name) + " must specify at least a `type` or a `default`",
			Detail:   "The declaration says nothing about " + strconv.Quote(name) + ", so nothing can be validated against it and there is no value to fall back on when it is not set.",
			Action:   "Add `type: string` (or another type), or `default:` with a value, or remove the declaration and set " + strconv.Quote(name) + " in " + VariablesFileName + ".",
			Origin:   origin,
		})
	}

	// min > max can never be satisfied. Reported here, where the declaration
	// is in hand, because at stage 4 the only thing to point at is a VALUE
	// that failed a range check — naming the symptom instead of the cause.
	// min == max is legal: it pins a variable to one value.
	if v.HasMin && v.HasMax && boundGreater(v.Min, v.Max) {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "variable " + strconv.Quote(name) + " has a `min` greater than its `max`",
			Detail:   "No value can satisfy this declaration, so every plan would fail with a range error naming the value rather than the declaration.",
			Action:   "Swap `min` and `max`, or remove one of them.",
			Origin:   v.Min.Origin,
		})
	}
	return v
}

// boundGreater reports whether bound a is greater than bound b.
//
// It switches on Kind rather than coercing both to float64, because an int64
// does not fit a float64: above 2^53 the conversion rounds and two bounds that
// differ would compare equal. That is the same reason Min and Max are
// value.Value rather than float64, and it is the shape Task 4's range check
// takes too.
//
// There is no mixed case to handle. decodeBound has already coerced both
// bounds to the variable's declared type, so if both are present they have the
// same Kind — which is exactly what that coercion buys.
//
// A non-numeric value is never greater: decodeBound refused it and said why,
// and a second diagnostic about the same declaration would describe the
// symptom.
func boundGreater(a, b value.Value) bool {
	if ai, ok := a.AsInt(); ok {
		bi, ok2 := b.AsInt()
		return ok2 && ai > bi
	}
	if af, ok := a.AsFloat(); ok {
		bf, ok2 := b.AsFloat()
		return ok2 && af > bf
	}
	return false
}

// decodeBound decodes `min` or `max`, refusing it on a type that has no
// ordering.
//
// Numeric-only is PLAN.md §9's rule, and it matters more than it looks: a
// `min` on a string would have to mean either "shortest" or "lowest in some
// collation", and stage 4 would have to pick one silently.
//
// typeReported suppresses this when `type` itself was already rejected —
// "`min` is not valid on an untyped variable" names a symptom of a problem
// already reported one line up.
func decodeBound(path, name, which string, node *yaml.Node, kind value.Kind, typeReported bool, ds *diag.Diagnostics) (value.Value, bool) {
	if node == nil {
		return value.Value{}, false
	}
	if kind != value.KindInt && kind != value.KindFloat {
		if typeReported {
			return value.Value{}, false
		}
		detail := "Variable " + strconv.Quote(name) + " has type " + kind.String() + ", which has no ordering, so `" + which + "` could not be checked against anything."
		action := "Remove `" + which + "`, or declare `type: integer` or `type: float`."
		if kind == value.KindInvalid {
			detail = "Variable " + strconv.Quote(name) + " declares no `type`, so `" + which + "` has no ordering to be checked against."
			action = "Add `type: integer` or `type: float`, or remove `" + which + "`."
		}
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "`" + which + "` is not valid on variable " + strconv.Quote(name),
			Detail:   detail,
			Action:   action,
			Origin:   originOf(path, node),
		})
		return value.Value{}, false
	}

	// Both numeric kinds are accepted here whatever the declared type. YAML
	// tags `min: 1` as !!int even under `type: float`, so refusing KindInt
	// would reject the obvious spelling of a float bound.
	bv, hasExpr := decodeValue(path, "variable "+strconv.Quote(name)+"'s `"+which+"`", node, ds)
	if hasExpr || !bv.Known || (bv.Kind != value.KindInt && bv.Kind != value.KindFloat) {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "`" + which + "` on variable " + strconv.Quote(name) + " must be a number",
			Detail:   "Got " + strconv.Quote(node.Value) + ". A quoted number is a string, and a string bound compares by text: \"10\" sorts before \"9\".",
			Action:   "Write `" + which + ": " + node.Value + "` unquoted.",
			Origin:   originOf(path, node),
		})
		return value.Value{}, false
	}
	return coerceBound(path, name, which, node, bv, kind, ds)
}

// coerceBound converts a decoded bound to the variable's DECLARED kind.
//
// This is why stage 2 is the right place: it is the only stage that knows both
// the declared type and the line number. After it, a bound's Kind always equals
// its variable's Type, so every consumer switches on one Kind instead of
// handling a cross product — and value.AsFloat, which deliberately does not
// coerce an int64, is safe to use directly.
//
// A conversion that would lose information is a DIAGNOSTIC, never a silent
// truncation. `type: integer` with `min: 1.5` must not quietly become 1: the
// user would see values below their stated minimum accepted, with nothing
// printed, which is the silent-loss shape this engine refuses everywhere else.
func coerceBound(path, name, which string, node *yaml.Node, bv value.Value, kind value.Kind, ds *diag.Diagnostics) (value.Value, bool) {
	origin := originOf(path, node)

	switch {
	case kind == value.KindInt && bv.Kind == value.KindFloat:
		f, _ := bv.AsFloat()
		n := int64(f)
		// Exactness both ways: float64(n) == f rejects a fractional part, and
		// it also rejects a float too large to survive the round trip.
		if float64(n) != f {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "`" + which + "` on variable " + strconv.Quote(name) + " is not a whole number",
				Detail:   "Variable " + strconv.Quote(name) + " has type integer, so " + strconv.Quote(node.Value) + " cannot be its " + which + ". Rounding it would silently change the bound the user asked for.",
				Action:   "Write a whole number, or declare `type: float`.",
				Origin:   origin,
			})
			return value.Value{}, false
		}
		return value.Int(n, value.SourceExplicit).WithOrigin(origin), true

	case kind == value.KindFloat && bv.Kind == value.KindInt:
		n, _ := bv.AsInt()
		f := float64(n)
		// An int64 above 2^53 does not survive this. Vanishingly rare for a
		// bound, and a diagnostic beats a bound that silently is not the one
		// that was written.
		if int64(f) != n {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "`" + which + "` on variable " + strconv.Quote(name) + " is too large to represent as a float",
				Detail:   "Variable " + strconv.Quote(name) + " has type float, and " + strconv.Quote(node.Value) + " cannot be converted without changing its value.",
				Action:   "Use a smaller bound, or declare `type: integer`.",
				Origin:   origin,
			})
			return value.Value{}, false
		}
		return value.Float(f, value.SourceExplicit).WithOrigin(origin), true
	}

	// Already the declared kind.
	return bv, true
}

// decodeVariableValues decodes variables.yml: a flat mapping of variable name
// to value (PLAN.md §8).
func decodeVariableValues(path string, doc *yaml.Node, out *ProjectDecl, ds *diag.Diagnostics) {
	for i := 0; i+1 < len(doc.Content); i += 2 {
		key, val := doc.Content[i], doc.Content[i+1]
		origin := originOf(path, key)

		// A key that names one of infra.yml's blocks is almost certainly the
		// wrong file. A WARNING rather than an error: a variable may
		// legitimately be called "project", and refusing it would break a
		// valid configuration in order to catch a mistake.
		switch key.Value {
		case "project", "resources", "variables", "environments":
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityWarning,
				Summary:  strconv.Quote(key.Value) + " in " + VariablesFileName + " is a variable, not a configuration block",
				Detail: VariablesFileName + " is a flat mapping of variable name to value (PLAN.md §8). " +
					"A `" + key.Value + "` block belongs in " + ProjectFileName + " — in particular, variable DECLARATIONS " +
					"(`type`, `default`, `min`, `max`) live only under " + ProjectFileName + "'s `variables:` key, so that " +
					"where a variable is declared has one answer.",
				Action: "Move it to " + ProjectFileName + ", or ignore this if you really do have a variable called " + strconv.Quote(key.Value) + ".",
				Origin: origin,
			})
		}

		if first, dup := out.VariableValues[key.Value]; dup {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "variable " + strconv.Quote(key.Value) + " is set more than once in " + VariablesFileName,
				Detail:   "The last assignment would silently win. It is also set at " + describeOrigin(first.Origin) + ".",
				Action:   "Remove one of the two assignments.",
				Origin:   origin,
			})
			continue
		}

		v, hasExpr := decodeValue(path, "variable "+strconv.Quote(key.Value), val, ds)
		if hasExpr {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "variable " + strconv.Quote(key.Value) + " contains an interpolation",
				Detail:   VariablesFileName + " is resolved before any expression scope exists, so `${...}` here has nothing to refer to.",
				Action:   "Write a literal value.",
				Origin:   originOf(path, val),
			})
			continue
		}
		out.VariableValues[key.Value] = retagSource(v, value.SourceVariable).WithOrigin(origin)
	}
}

// retagSource rewrites a decoded value's provenance recursively.
//
// decodeValue marks everything SourceExplicit, because that is what an
// attribute in infra.yml is. A value from variables.yml is a variable and one
// from an environment file is an environment override, and provenance is
// PER-LEAF (spec §5.1) — setting only the top level would leave every element
// of a list variable claiming to be explicit configuration, which `explain`
// and minimal generation both read straight off the leaves.
//
// Scope is deliberately NOT set: a declaration is not a resolution, and which
// precedence level won is stages 3 and 4's answer. Two places deciding
// provenance is how a plan starts disagreeing with itself.
func retagSource(v value.Value, src value.ValueSource) value.Value {
	switch v.Kind {
	case value.KindList:
		items, ok := v.Raw.([]value.Value)
		if !ok {
			// Malformed: a diagnostic was already emitted where it was
			// decoded. Retag the shell and stop rather than panicking.
			return v.WithSource(src)
		}
		retagged := make([]value.Value, len(items))
		for i, item := range items {
			retagged[i] = retagSource(item, src)
		}
		v.Raw = retagged
	case value.KindMap:
		m, ok := v.Raw.(map[string]value.Value)
		if !ok {
			return v.WithSource(src)
		}
		retagged := make(map[string]value.Value, len(m))
		for k, item := range m {
			retagged[k] = retagSource(item, src)
		}
		v.Raw = retagged
	}
	return v.WithSource(src)
}

func decodeEnvironments(path string, node *yaml.Node, out *ProjectDecl, ds *diag.Diagnostics, seen map[string]int) {
	if node.Kind != yaml.MappingNode {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "`environments` must be a mapping of environment name to configuration",
			Detail:   "Each environment is a key, optionally with `extends` and its overrides beneath it (PLAN.md §7).",
			Origin:   originOf(path, node),
		})
		return
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		nameNode, body := node.Content[i], node.Content[i+1]
		decodeEnvironmentBody(path, nameNode.Value, body, out, ds, seen)
	}
}

// environmentFor returns the decl for name, creating it on first sight.
//
// One environment may be declared in infra.yml's `environments:` block AND in
// environments/<name>.yml, and the two MERGE: PLAN.md §7 puts `extends` in the
// block, §8 puts overrides in the file, so a user doing both is following the
// spec. Load's fixed file order (project, variables, environments by name) is
// what makes "which one was first" deterministic, which the duplicate
// diagnostics below depend on.
func environmentFor(out *ProjectDecl, name string, origin value.Origin, seen map[string]int) *EnvironmentDecl {
	if idx, ok := seen[name]; ok {
		return &out.Environments[idx]
	}
	out.Environments = append(out.Environments, EnvironmentDecl{Name: name, Origin: origin})
	seen[name] = len(out.Environments) - 1
	return &out.Environments[len(out.Environments)-1]
}

// decodeEnvironmentBody decodes one environment's mapping, from either the
// `environments:` block or a whole environments/<name>.yml document.
//
// Two override spellings are accepted because PLAN.md uses both: §6 nests them
// under `variables:` and §7 writes them as flat keys. `extends` is reserved in
// both, and in the file form.
func decodeEnvironmentBody(path, name string, body *yaml.Node, out *ProjectDecl, ds *diag.Diagnostics, seen map[string]int) {
	origin := originOf(path, body)
	if body.Kind != yaml.MappingNode {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "environment " + strconv.Quote(name) + " must be a mapping",
			Detail:   "An environment holds an optional `extends` and its overrides.",
			Action:   "Write `" + name + ": {}` if it has no settings of its own.",
			Origin:   origin,
		})
		return
	}
	env := environmentFor(out, name, origin, seen)

	for i := 0; i+1 < len(body.Content); i += 2 {
		key, val := body.Content[i], body.Content[i+1]
		keyOrigin := originOf(path, key)

		switch key.Value {
		case "extends":
			// requireScalar reports the alias, non-scalar and null cases, each
			// of which would otherwise silently produce a wrong parent — an
			// alias node's Value is the ANCHOR'S NAME, so `extends: *base`
			// would become the string "base", which might even name a real
			// environment.
			text, ok := requireScalar(path, "environment "+strconv.Quote(name)+"'s `extends`", val, ds)
			if !ok {
				continue
			}
			if env.Extends != "" {
				ds.Add(diag.Diagnostic{
					Severity: diag.SeverityError,
					Summary:  "`extends` is set more than once on environment " + strconv.Quote(name),
					Detail:   "The last assignment would silently win, changing which values " + strconv.Quote(name) + " inherits. It is also set at " + describeOrigin(env.ExtendsOrigin) + ".",
					Action:   "Remove one of the two assignments.",
					Origin:   keyOrigin,
				})
				continue
			}
			env.Extends, env.ExtendsOrigin = text, keyOrigin

		case "variables":
			// PLAN.md §6's spelling.
			if val.Kind != yaml.MappingNode {
				ds.Add(diag.Diagnostic{
					Severity: diag.SeverityError,
					Summary:  "`variables` in environment " + strconv.Quote(name) + " must be a mapping",
					Origin:   originOf(path, val),
				})
				continue
			}
			for j := 0; j+1 < len(val.Content); j += 2 {
				addOverride(path, env, val.Content[j], val.Content[j+1], ds)
			}

		default:
			// PLAN.md §7's spelling: any other key is an override.
			addOverride(path, env, key, val, ds)
		}
	}
}

// addOverride records one environment override, refusing a second setting of
// the same name.
//
// A collision is reachable three ways — the two spellings within one
// environment, the same key twice in one file, and the block plus the file —
// and every one of them silently discards a value the user wrote, producing a
// plan that is wrong with nothing printed.
func addOverride(path string, env *EnvironmentDecl, key, val *yaml.Node, ds *diag.Diagnostics) {
	origin := originOf(path, key)
	// APPENDS IN DOCUMENT ORDER. That is not the order the field promises:
	// Decode sorts every environment's Overrides by name once, after all files
	// are decoded, which is the only point at which the block's and the file's
	// contributions are both present. Do not add a sort here — a reader who
	// looks only at this function concludes the slice is unsorted, which is a
	// real misreading that has already happened once during review.
	//
	// The linear scan below is what lets the duplicate diagnostic name where
	// the FIRST assignment was.
	for _, existing := range env.Overrides {
		if existing.Name == key.Value {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  strconv.Quote(key.Value) + " is set more than once in environment " + strconv.Quote(env.Name),
				Detail:   "The last assignment would silently win. It is also set at " + describeOrigin(existing.Origin) + ".",
				Action:   "Remove one of the two assignments.",
				Origin:   origin,
			})
			return
		}
	}

	v, hasExpr := decodeValue(path, "environment "+strconv.Quote(env.Name)+"'s "+strconv.Quote(key.Value), val, ds)
	if hasExpr {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "environment override " + strconv.Quote(key.Value) + " contains an interpolation",
			Detail:   "Environment overrides are resolved before any expression scope exists, so `${...}` here has nothing to refer to.",
			Action:   "Write a literal value.",
			Origin:   originOf(path, val),
		})
		return
	}
	env.Overrides = append(env.Overrides, OverrideDecl{
		Name:   key.Value,
		Value:  retagSource(v, value.SourceEnvironment).WithOrigin(origin),
		Origin: origin,
	})
}
