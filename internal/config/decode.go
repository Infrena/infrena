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
			out.Project = val.Value
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
				r.Type = val.Value
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
				for _, item := range val.Content {
					r.DependsOn = append(r.DependsOn, item.Value)
				}
			case "lifecycle":
				decodeLifecycle(path, val, r, ds)
			default:
				v, hasExpr := decodeValue(path, val)
				r.Attributes[key.Value] = AttributeDecl{
					Name:           key.Value,
					Value:          v,
					HasExpressions: hasExpr,
					Origin:         keyOrigin,
				}
			}
		}

		if r.Type == "" {
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

// decodeValue converts a YAML node into a typed Value, reporting whether any
// string within it contains an interpolation.
func decodeValue(path string, node *yaml.Node) (value.Value, bool) {
	origin := originOf(path, node)

	switch node.Kind {
	case yaml.SequenceNode:
		items := make([]value.Value, 0, len(node.Content))
		anyExpr := false
		for _, item := range node.Content {
			v, has := decodeValue(path, item)
			anyExpr = anyExpr || has
			items = append(items, v)
		}
		return value.List(items, value.SourceExplicit).WithOrigin(origin), anyExpr

	case yaml.MappingNode:
		items := map[string]value.Value{}
		anyExpr := false
		for i := 0; i+1 < len(node.Content); i += 2 {
			v, has := decodeValue(path, node.Content[i+1])
			anyExpr = anyExpr || has
			items[node.Content[i].Value] = v
		}
		return value.Map(items, value.SourceExplicit).WithOrigin(origin), anyExpr

	default:
		return decodeScalar(node, origin)
	}
}

func decodeScalar(node *yaml.Node, origin value.Origin) (value.Value, bool) {
	raw := node.Value

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
