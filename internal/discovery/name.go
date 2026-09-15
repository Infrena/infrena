// Package discovery turns what a provider reports into something a
// configuration file can hold: names (this file) and, with internal/generator,
// minimal YAML.
//
// It is compiler-adjacent but not a compiler stage. Nothing here runs during
// `plan` or `apply` — discovery happens when a user asks for it, and its output
// is a file they read and edit before it means anything.
package discovery

import (
	"strconv"
	"strings"

	"github.com/infrena/infrena/internal/config"
	"github.com/infrena/infrena/internal/registry"
	"github.com/infrena/infrena/pkg/provider"
	"github.com/infrena/infrena/pkg/value"
)

// nameAttributes are the attributes consulted for a name, in this order
// (spec §27.2). Fixed and ordered rather than "whichever the provider offers",
// because two providers offering both must not name the same resource
// differently.
var nameAttributes = []string{"name", "Name"}

// nameTags are the keys looked up inside the tag map, in this order. AWS
// spells it `Name`; most other things spell it `name`.
var nameTags = []string{"Name", "name"}

// tagsAttribute is the spelling discovery ASKS FOR. What the plugin actually
// calls the attribute is the schema's business, and tagMap resolves it through
// the alias fold.
const tagsAttribute = "tags"

// Name derives the configuration name for a discovered resource (spec §27.2):
// a name attribute or tag when there is a usable one, else the sanitised
// provider ID.
//
// The provider ID is the fallback rather than a generated `resource_1` because
// a name is what a person uses to find the thing again. `vpc-0a1b2c3d` can be
// pasted into a console; `network_2` cannot be matched to anything.
func Name(reg *registry.Registry, r provider.DiscoveredResource) string {
	if tag, ok := nameFrom(reg, r.Type, r.Attributes); ok {
		if s := sanitise(tag); s != "" {
			return s
		}
	}
	if s := sanitise(r.ProviderID); s != "" {
		return s
	}
	// Reached only by a provider that reported a resource with no ID and no
	// name. Unique's collision suffix keeps several of them distinct.
	return "imported"
}

// Unique returns a name for r that nothing in taken already holds, and RECORDS
// the choice in taken so the next call sees it. taken maps a chosen name to the
// provider ID that claimed it.
//
// A collision suffixes the PROVIDER ID rather than a counter, and never drops a
// resource. Two resources tagged `orders` are two resources: silently merging
// them would lose one, which is the failure this project refuses everywhere
// else — and a counter (`orders_2`) would name a resource after the order it
// happened to be discovered in, which changes when the account does.
func Unique(reg *registry.Registry, taken map[string]string, r provider.DiscoveredResource) string {
	base := Name(reg, r)
	if _, clash := taken[base]; !clash {
		taken[base] = r.ProviderID
		return base
	}

	// The provider ID is unique within a provider, so one suffix almost always
	// settles it. Almost: a resource whose NAME is already `orders_db-9` and
	// one tagged `orders` with ID `db-9` produce the same candidate, so the
	// loop is not decoration.
	candidate := sanitise(base + "_" + r.ProviderID)
	for n := 2; ; n++ {
		if _, clash := taken[candidate]; !clash {
			taken[candidate] = r.ProviderID
			return candidate
		}
		candidate = sanitise(base + "_" + r.ProviderID + "_" + strconv.Itoa(n))
	}
}

// nameFrom looks for a usable name among the attributes, then inside the
// resource's tag map. A value that is unknown, not a string, or blank is not a
// name.
func nameFrom(reg *registry.Registry, resourceType string, attrs map[string]value.Value) (string, bool) {
	for _, key := range nameAttributes {
		if s, ok := stringAttr(attrs[key]); ok {
			return s, true
		}
	}
	m, ok := tagMap(reg, resourceType, attrs)
	if !ok {
		return "", false
	}
	for _, key := range nameTags {
		if s, ok := stringAttr(m[key]); ok {
			return s, true
		}
	}
	return "", false
}

// tagMap finds the resource's tag map by ASKING THE SCHEMA rather than
// guessing its spelling.
//
// It used to read attrs["tags"], a literal lowercase key. The AWS plugin
// spells it "Tags", so the lookup missed on every AWS resource and section
// 27.2's naming rule never once fired against the only real provider.
//
// The fix is not a third hard-coded spelling. That is the same defect with a
// longer list, and the next plugin spells it a fourth way. Canonical is the
// engine's ONE alias fold (PLAN.md section 14.1), already case-insensitive
// across canonical names and aliases, and already what the compiler uses at
// its own boundary. Asking it means discovery cannot drift from the rest of
// the engine about what an attribute is called.
//
// An unknown type, an undeclared tag map, or an attribute that is not a known
// map all return false, and Name falls through to the provider ID exactly as
// before. Discovery reports what a provider returned, so refusing to name
// something would drop it.
func tagMap(reg *registry.Registry, resourceType string, attrs map[string]value.Value) (map[string]value.Value, bool) {
	if reg == nil {
		return nil, false
	}
	def, ok := reg.Definition(resourceType)
	if !ok {
		return nil, false
	}
	canonical, ok := def.Canonical(tagsAttribute)
	if !ok {
		return nil, false
	}
	tags, ok := attrs[canonical]
	if !ok || tags.Kind != value.KindMap || !tags.Known {
		return nil, false
	}
	m, ok := tags.Raw.(map[string]value.Value)
	if !ok {
		return nil, false
	}
	return m, true
}

// stringAttr reports a usable string attribute. An unknown value or one of
// another kind is not a name.
//
// A BLANK string is deliberately NOT rejected here. It was, until a sabotage
// showed the check could be deleted without failing a single test: sanitise
// already returns "" for anything that trims to nothing, and Name falls back on
// that. Two guards for one condition, where removing either changes no
// behaviour, is one guard and one thing nothing tests — so the condition lives
// in sanitise, which needs it for the provider-ID path anyway.
func stringAttr(v value.Value) (string, bool) {
	if !v.Known || v.Kind != value.KindString {
		return "", false
	}
	return v.AsString()
}

// sanitise turns arbitrary provider text into something config.ValidResourceName
// accepts, or returns "" if it cannot.
//
// HYPHENS SURVIVE. A resource name may contain them after the first character —
// this asks config rather than assuming — so `vpc-0a1b2c3d` stays spelled the
// way the provider spells it, and a reader can match generated configuration
// against a console without translating. Rewriting them to underscores would
// make every generated name differ from the ID it came from, for no gain.
//
// A leading digit or hyphen gets an underscore prefix rather than being
// dropped: `0abc` and `abc` are different resources, and a sanitiser that
// collapsed them would hand Unique a collision that is really a data loss.
func sanitise(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := b.String()
	if !config.ValidResourceName(out) {
		// The only remaining reason is a first character that may not lead.
		out = "_" + out
	}
	if !config.ValidResourceName(out) {
		return ""
	}
	return out
}
