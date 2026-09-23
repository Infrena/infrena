package schema

import (
	"fmt"
	"sort"
	"strings"
)

// Canonical resolves a spelling a user wrote to the attribute name the plugin
// declared, matching case-insensitively across canonical names and aliases.
//
// It returns the canonical name and whether anything matched. The caller decides
// what an unmatched name means — the compiler reports "has no attribute", the
// planner treats it as the provider's own business.
//
// One place resolves aliases, and it is the compiler's boundary with the schema.
// Every lookup downstream of that uses Attribute and an exact name, because by
// then the name is canonical. That separation is the whole safety argument: an
// alias reaching sensitivity marking unresolved would leave a sensitive
// attribute unmarked and print a secret in clear, and an alias reaching the diff
// would make one attribute look like two and propose a change forever.
func (d *ResourceDefinition) Canonical(written string) (string, bool) {
	if _, ok := d.Attributes[written]; ok {
		// The common case, and free: an exact hit needs no folding.
		return written, true
	}
	folded := foldName(written)
	for name, attr := range d.Attributes {
		if foldName(name) == folded {
			return name, true
		}
		for _, alias := range attr.Aliases {
			if foldName(alias) == folded {
				return name, true
			}
		}
	}
	return "", false
}

// Spellings lists every name that resolves to one attribute, canonical first and
// aliases sorted after it, for `infrena explain` to show and for a diagnostic to
// offer. It returns nil when the definition declares no such attribute.
func (d *ResourceDefinition) Spellings(name string) []string {
	attr, ok := d.Attributes[name]
	if !ok {
		return nil
	}
	out := append([]string{name}, attr.Aliases...)
	sort.Strings(out[1:])
	return out
}

// Display is the name a plan, `infrena explain` or generated configuration shows
// for an attribute: the first declared alias where there is one, and the
// canonical name otherwise.
//
// First rather than shortest or sorted, so a plugin controls which spelling it
// puts in front of users by writing it first — `Aliases: []string{"cidr",
// "cidr_block"}` shows `cidr`. Display is a pure function of the schema, applied
// at render time only, so it has no storage consequence and changing it never
// rewrites anything.
func (d *ResourceDefinition) Display(name string) string {
	if attr, ok := d.Attributes[name]; ok && len(attr.Aliases) > 0 {
		return attr.Aliases[0]
	}
	return name
}

// foldName is the case-insensitive comparison aliases match under.
//
// Case only. It deliberately does not strip underscores or hyphens, so
// `cidr_block` and `cidrblock` are different spellings and a plugin that wants
// both declares both. Folding punctuation would make them collide, and a generic
// AWS plugin generating a snake_case alias for every property would then fail to
// load on any type with two such properties.
func foldName(s string) string { return strings.ToLower(s) }

// checkSpellings refuses a definition whose names fold together.
//
// A collision decided at runtime is a silent wrong answer — whichever attribute
// the map happened to be walked to first — so it is refused at load, as a plugin
// that will not start and an error naming both spellings.
func (d *ResourceDefinition) checkSpellings() error {
	seen := map[string]string{} // folded -> the spelling that claimed it

	for _, name := range d.attributeNamesSorted() {
		attr := d.Attributes[name]
		for _, spelling := range append([]string{name}, attr.Aliases...) {
			folded := foldName(spelling)
			if first, taken := seen[folded]; taken {
				return fmt.Errorf(
					"%s: %q and %q are the same name ignoring case, so configuration naming it "+
						"would reach whichever attribute was found first",
					d.Type, first, spelling)
			}
			seen[folded] = spelling
		}
		if err := checkFieldSpellings(d.Type, name, attr.Fields); err != nil {
			return err
		}
	}
	return nil
}

// checkFieldSpellings is checkSpellings one level down, and then further.
//
// Nested names resolve by the same ladder as top-level ones — exact, then
// case-folded, then alias — so they need the same refusal. Without it a schema
// whose nested names fold together loads happily and answers whichever key the
// map was walked to first, which is the silent wrong answer the top-level check
// exists to prevent.
//
// Each sibling group is checked against itself only. A nested key and a
// top-level attribute are never candidates for the same lookup, so a name that
// repeats at another depth is unambiguous and stays allowed.
//
// Nil Fields is an open map, whose keys are the user's rather than the schema's,
// and there is nothing to collide.
func checkFieldSpellings(typeName, path string, fields map[string]Attribute) error {
	if fields == nil {
		return nil
	}
	seen := map[string]string{} // folded -> the spelling that claimed it
	for _, name := range sortedAttributeKeys(fields) {
		attr := fields[name]
		for _, spelling := range append([]string{name}, attr.Aliases...) {
			folded := foldName(spelling)
			if first, taken := seen[folded]; taken {
				return fmt.Errorf(
					"%s: %s has keys %q and %q, which are the same name ignoring case, so "+
						"configuration naming it would reach whichever was found first",
					typeName, path, first, spelling)
			}
			seen[folded] = spelling
		}
		if err := checkFieldSpellings(typeName, path+"."+name, attr.Fields); err != nil {
			return err
		}
	}
	return nil
}

// sortedAttributeKeys makes the refusal deterministic: Go randomises map
// iteration, and a plugin that loads on one run and is refused on the next
// would be worse than either answer.
func sortedAttributeKeys(fields map[string]Attribute) []string {
	out := make([]string, 0, len(fields))
	for k := range fields {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// attributeNamesSorted orders the attribute map, so a collision is reported
// against the same pair on every run rather than whichever the map yielded
// first.
func (d *ResourceDefinition) attributeNamesSorted() []string {
	out := make([]string, 0, len(d.Attributes))
	for name := range d.Attributes {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
