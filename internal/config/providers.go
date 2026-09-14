package config

import (
	"strconv"

	"gopkg.in/yaml.v3"

	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/pkg/value"
)

// providersListHint shows the shape `providers:` takes, for every diagnostic that
// has to correct it. One copy, because a reader who gets two different spellings
// of the same example has to work out which is current.
const providersListHint = "providers:\n  - plugin: aws\n    name: main      # optional; defaults to the plugin name\n    iam-role: ...   # the plugin's own configuration"

// decodeProviders decodes `providers:` (PLAN.md §12.1).
//
// It is a LIST. A mapping is refused rather than accommodated: a YAML map cannot
// hold two `aws` keys, so the map form cannot express two instances of one plugin
// — and it would refuse that SILENTLY, by losing one, which is the failure mode
// §12.1 exists to prevent.
func decodeProviders(path string, node *yaml.Node, out *ProjectDecl, ds *diag.Diagnostics) {
	if node.Kind != yaml.SequenceNode {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "`providers` must be a list",
			Detail: "A mapping cannot hold two instances of one plugin — `aws` twice is one key — " +
				"so it would silently lose one, and which resources went to which account would " +
				"depend on that.",
			Action: "Write it as a list:\n" + providersListHint,
			Origin: originOf(path, node),
		})
		return
	}

	for i, entry := range node.Content {
		d, ok := decodeProviderEntry(path, i, entry, ds)
		if !ok {
			continue
		}
		out.Providers = append(out.Providers, d)
	}

	checkProviderNames(out.Providers, ds)
	chooseDefaultProvider(out.Providers, ds)
}

func decodeProviderEntry(path string, index int, node *yaml.Node, ds *diag.Diagnostics) (ProviderDecl, bool) {
	origin := originOf(path, node)
	position := "providers[" + strconv.Itoa(index) + "]"

	if node.Kind != yaml.MappingNode {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  position + " must be a mapping",
			Detail:   "Each entry names a plugin and, optionally, a name and its configuration.",
			Action:   "Write it as:\n" + providersListHint,
			Origin:   origin,
		})
		return ProviderDecl{}, false
	}

	d := ProviderDecl{
		Config:   map[string]AttributeDecl{},
		Defaults: map[string]AttributeDecl{},
		Origin:   origin,
	}

	seen := map[string]value.Origin{}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, val := node.Content[i], node.Content[i+1]
		keyOrigin := originOf(path, key)

		if first, dup := seen[key.Value]; dup {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  strconv.Quote(key.Value) + " is set more than once on " + position,
				Detail:   "The last assignment would silently win. It is also set at " + describeOrigin(first) + ".",
				Action:   "Remove one of the two assignments.",
				Origin:   keyOrigin,
			})
			continue
		}
		seen[key.Value] = keyOrigin

		switch key.Value {
		case "plugin":
			if text, ok := requireScalar(path, position+"'s `plugin`", val, ds); ok {
				d.Plugin = text
			}
		case "name":
			if text, ok := requireScalar(path, position+"'s `name`", val, ds); ok {
				d.Name, d.NameOrigin = text, keyOrigin
			}
		case "default":
			if b, ok := decodeLifecycleBool(path, key, val, ds); ok {
				d.Default, d.DefaultExplicit = b, b
			}
		case "defaults":
			decodeProviderDefaults(path, position, val, &d, ds)
		default:
			// Everything else is the PLUGIN'S configuration. Which keys are
			// meaningful is the plugin's business, not this decoder's — it has no
			// way to know what `aws` accepts, and guessing would make adding a
			// provider option a change to the core.
			v, hasExpr := decodeValue(path, position+"."+key.Value, val, ds)
			d.Config[key.Value] = AttributeDecl{
				Name: key.Value, Value: v, HasExpressions: hasExpr, Origin: keyOrigin,
			}
		}
	}

	if d.Plugin == "" {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  position + " has no `plugin`",
			Detail:   "Every provider instance names the implementation behind it.",
			Action:   "Add `plugin: <name>` to this entry.",
			Origin:   origin,
		})
		return ProviderDecl{}, false
	}

	// An unnamed instance is named after its plugin. That is also what makes a
	// state file written before instances existed already correct: it holds the
	// plugin name, which IS this instance's name.
	if d.Name == "" {
		d.Name, d.NameOrigin = d.Plugin, seen["plugin"]
	}
	return d, true
}

func decodeProviderDefaults(path, position string, node *yaml.Node, d *ProviderDecl, ds *diag.Diagnostics) {
	if node.Kind != yaml.MappingNode {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  position + "'s `defaults` must be a mapping",
			Detail:   "It holds attribute defaults for every resource that uses this instance.",
			Action:   "Write `defaults:` with one key per attribute, or remove it.",
			Origin:   originOf(path, node),
		})
		return
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, val := node.Content[i], node.Content[i+1]
		v, hasExpr := decodeValue(path, position+".defaults."+key.Value, val, ds)
		d.Defaults[key.Value] = AttributeDecl{
			Name: key.Value, Value: v, HasExpressions: hasExpr, Origin: originOf(path, key),
		}
	}
}

// checkProviderNames refuses two instances resolving to one name.
//
// There is no precedence to invent here. Whichever entry won, the other's
// resources would be created in, and destroyed from, an account nobody named —
// and nothing in any output would say so.
func checkProviderNames(decls []ProviderDecl, ds *diag.Diagnostics) {
	first := map[string]value.Origin{}
	for _, d := range decls {
		if at, dup := first[d.Name]; dup {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "two provider instances are both called " + strconv.Quote(d.Name),
				Detail: "The first is at " + describeOrigin(at) + ". An instance name is how a " +
					"resource chooses its account, so two with one name would send resources to " +
					"whichever happened to win.",
				Action: "Give one of them a different `name:`.",
				Origin: d.NameOrigin,
			})
			continue
		}
		first[d.Name] = d.NameOrigin
	}
}

// chooseDefaultProvider settles which instance a resource gets when it names
// none: the one marked `default: true`, else the first.
//
// Two marked is refused for the same reason two names are. Written as a separate
// pass over the whole slice rather than decided during decoding, because "is
// there another one" is not answerable until every entry has been read.
func chooseDefaultProvider(decls []ProviderDecl, ds *diag.Diagnostics) {
	var marked []int
	for i := range decls {
		if decls[i].DefaultExplicit {
			marked = append(marked, i)
		}
	}

	switch {
	case len(marked) > 1:
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "two provider instances are both marked `default: true`",
			Detail: strconv.Quote(decls[marked[0]].Name) + " at " + describeOrigin(decls[marked[0]].Origin) +
				" and " + strconv.Quote(decls[marked[1]].Name) + " are both marked. Either choice " +
				"would send every resource that names no provider to an account nobody chose.",
			Action: "Remove `default: true` from all but one.",
			Origin: decls[marked[1]].Origin,
		})
	case len(marked) == 1:
		// Already set by the decoder; nothing to derive.
	case len(decls) > 0:
		decls[0].Default = true
	}
}
