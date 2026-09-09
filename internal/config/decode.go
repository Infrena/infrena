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
		decodeDocument(f.Path, doc, out, &ds)
	}

	sort.Slice(out.Resources, func(i, j int) bool { return out.Resources[i].Name < out.Resources[j].Name })
	return out, ds
}

func decodeDocument(path string, doc *yaml.Node, out *ProjectDecl, ds *diag.Diagnostics) {
	for i := 0; i+1 < len(doc.Content); i += 2 {
		key, val := doc.Content[i], doc.Content[i+1]
		switch key.Value {
		case "project":
			out.Project = val.Value
		case "resources":
			decodeResources(path, val, out, ds)
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

func decodeResources(path string, node *yaml.Node, out *ProjectDecl, ds *diag.Diagnostics) {
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

		if body.Kind != yaml.MappingNode {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "resource " + strconv.Quote(r.Name) + " must be a mapping",
				Origin:   originOf(path, body),
			})
			continue
		}

		for j := 0; j+1 < len(body.Content); j += 2 {
			key, val := body.Content[j], body.Content[j+1]
			switch key.Value {
			case "type":
				r.Type = val.Value
			case "depends_on":
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
					Origin:         originOf(path, key),
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
			r.Lifecycle.PreventDestroy = val.Value == "true"
		case "retain":
			r.Lifecycle.Retain = val.Value == "true"
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
		return value.Bool(raw == "true", value.SourceExplicit).WithOrigin(origin), false
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

func originOf(path string, n *yaml.Node) value.Origin {
	if n == nil {
		return value.Origin{File: path}
	}
	return value.Origin{File: path, Line: n.Line, Column: n.Column}
}
