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
	out := &ProjectDecl{}

	// Uniqueness is tracked across the whole decode rather than per file: spec
	// §5.2 requires logical names be unique within a module, and M4 adds more
	// files to the same root module.
	seen := map[string]value.Origin{}

	for _, f := range files {
		doc := documentRoot(f.Root)
		if doc == nil {
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
				Summary:  "configuration must be a mapping",
				Detail:   "The top level of " + ProjectFileName + " must be a set of keys such as `project` and `resources`.",
				Origin:   originOf(f.Path, doc),
			})
			continue
		}
		out.Origin = originOf(f.Path, doc)
		decodeDocument(f.Path, doc, out, &ds, seen)
	}

	sort.Slice(out.Resources, func(i, j int) bool { return out.Resources[i].Name < out.Resources[j].Name })
	return out, ds
}

func decodeDocument(path string, doc *yaml.Node, out *ProjectDecl, ds *diag.Diagnostics, seen map[string]value.Origin) {
	for i := 0; i+1 < len(doc.Content); i += 2 {
		key, val := doc.Content[i], doc.Content[i+1]
		switch key.Value {
		case "project":
			if text, ok := requireScalar(path, "`project`", val, ds); ok {
				out.Project = text
			}
		case "resources":
			decodeResources(path, val, out, ds, seen)
		default:
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityWarning,
				Summary:  "unrecognised top-level key " + strconv.Quote(key.Value),
				Detail:   "M1 understands `project` and `resources`.",
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
