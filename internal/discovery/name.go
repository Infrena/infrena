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

// nameAttributes are the attributes consulted for a name, in this order. Fixed
// and ordered rather than "whichever the provider offers", because two
// providers offering both must not name the same resource differently.
var nameAttributes = []string{"name", "Name"}

// nameTags are the keys looked up inside the tag map, in this order. AWS
// spells it `Name`; most other things spell it `name`.
var nameTags = []string{"Name", "name"}

// tagsAttribute is the spelling discovery asks for. What the plugin actually
// calls the attribute is the schema's business, and tagMap resolves it through
// the alias fold.
const tagsAttribute = "tags"

// Name derives the configuration name for a discovered resource: a name
// attribute or tag when there is a usable one, else the sanitised provider ID.
//
// The provider ID is the fallback rather than a generated `resource_1` because
// a name is what a person uses to find the thing again. `vpc-0a1b2c3d` can be
// pasted into a console; `network_2` cannot be matched to anything.
func Name(reg *registry.Registry, r provider.DiscoveredResource) string {
	prefix := typePrefix(r.Type)
	if tag, ok := nameFrom(reg, r.Type, r.Attributes); ok {
		if s := prefixed(prefix, tag); s != "" {
			return s
		}
	}
	if s := prefixed(prefix, r.ProviderID); s != "" {
		return s
	}
	// Reached only by a provider that reported a resource with no ID and no
	// name. Unique's collision suffix keeps several of them distinct.
	return "imported"
}

// Unique returns a name for r that nothing in taken already holds, and records
// the choice in taken so the next call sees it. taken maps a chosen name to the
// provider ID that claimed it.
//
// A collision suffixes the provider ID rather than a counter, and never drops a
// resource. Two resources tagged `orders` are two resources, and a counter
// (`orders_2`) would name a resource after the order it happened to be
// discovered in, which changes when the account does.
func Unique(reg *registry.Registry, taken map[string]string, r provider.DiscoveredResource) string {
	base := Name(reg, r)
	if _, clash := taken[base]; !clash {
		taken[base] = r.ProviderID
		return base
	}

	// The provider ID is unique within a provider, so one suffix almost always
	// settles it. Almost: a resource already named `orders_db-9` and one tagged
	// `orders` with ID `db-9` produce the same candidate, so the loop is not
	// decoration.
	candidate := sanitise(base + "_" + r.ProviderID)
	for n := 2; ; n++ {
		if _, clash := taken[candidate]; !clash {
			taken[candidate] = r.ProviderID
			return candidate
		}
		candidate = sanitise(base + "_" + r.ProviderID + "_" + strconv.Itoa(n))
	}
}

// typePrefix is the short name a generated resource is prefixed with: the
// last dotted segment of the resource type.
//
// A plugin-published short name is deliberately not taken: adding the field
// would raise the plugin protocol, and generators already collapse unambiguous
// type names (AWS::EC2::VPC becomes aws.vpc), so the last segment is the right
// answer for the types a user meets. An optional ShortName could still be added
// later with this as its fallback.
func typePrefix(resourceType string) string {
	if i := strings.LastIndex(resourceType, "."); i >= 0 {
		return resourceType[i+1:]
	}
	return resourceType
}

// prefixed joins the type prefix to the text a name came from, and sanitises
// the result rather than the parts, so one rule decides what a name may contain
// and a prefix cannot smuggle in a character the parts were checked for
// separately.
//
// The prefix is skipped where the text already carries it: AWS provider IDs are
// themselves prefixed, so prefixing blindly would give vpc-vpc-0a1b2c3d.
//
// "" means there is no usable name here, exactly as sanitise means it, and Name
// falls through to the next source.
func prefixed(prefix, text string) string {
	s := sanitise(text)
	if s == "" || prefix == "" || strings.HasPrefix(s, prefix+"-") {
		return s
	}
	return sanitise(prefix + "-" + text)
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

// tagMap finds the resource's tag map by asking the schema rather than guessing
// its spelling. A literal attrs["tags"] misses every AWS resource, which spells
// it "Tags", and a hard-coded list of spellings is the same defect one plugin
// later. Definition.Canonical is the engine's one alias fold, so discovery
// cannot drift from the compiler about what an attribute is called.
//
// An unknown type, an undeclared tag map, or an attribute that is not a known
// map all return false, and Name falls through to the provider ID: discovery
// reports what a provider returned, so refusing to name something would drop
// it.
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
// A blank string is deliberately not rejected here. sanitise already returns ""
// for anything that trims to nothing, and it needs that check for the
// provider-ID path anyway, so a second guard here would be untested duplication.
func stringAttr(v value.Value) (string, bool) {
	if !v.Known || v.Kind != value.KindString {
		return "", false
	}
	return v.AsString()
}

// sanitise turns arbitrary provider text into something config.ValidResourceName
// accepts, or returns "" if it cannot.
//
// Hyphens survive, so `vpc-0a1b2c3d` stays spelled the way the provider spells
// it and a reader can match generated configuration against a console without
// translating.
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
