package discovery

import (
	"fmt"
	"path"

	"github.com/infrena/infrena/internal/registry"
	"github.com/infrena/infrena/pkg/value"
)

// Filter narrows a survey. A zero Filter keeps everything.
//
// The fields are ANDed: a resource survives only if it satisfies all of them.
// `--tag a=1 --tag b=2` means both.
type Filter struct {
	// Tags are tag key to exact value, looked up inside the resource's tag map.
	Tags map[string]string
	// ExcludeTypes removes every resource of these types. Exclusion rather than
	// inclusion, because `discover [type...]` already answers "only these".
	ExcludeTypes []string
	// NameGlob is a path.Match pattern against the proposed name, not the
	// provider's own.
	NameGlob string
}

// Apply narrows in, keeping order.
//
// Call it after naming, always. Unique is order-dependent by construction: it
// settles a collision by suffixing the provider ID of whichever resource it met
// second, so filtering first would change the names of the resources that
// survive, and the same resource would import under one name with a flag and
// another without it. Taking named Results rather than DiscoveredResources is
// what makes that ordering hard to get wrong.
func (f Filter) Apply(reg *registry.Registry, in []Result) ([]Result, error) {
	// Checked before the loop, so a malformed pattern is an error even when the
	// survey is empty — otherwise a typo in the flag reads as "nothing found",
	// which is indistinguishable from an empty account.
	//
	// The empty name is a probe, not a match: path.Match parses the whole
	// pattern before it can answer anything, so it reports ErrBadPattern here
	// for the same patterns it would reject against a real name.
	if f.NameGlob != "" {
		if _, err := path.Match(f.NameGlob, ""); err != nil {
			return nil, fmt.Errorf("--name %q is not a valid pattern: %w\n"+
				"It is a glob: * matches any run of characters, ? matches one, and "+
				"[abc] matches one of a set", f.NameGlob, err)
		}
	}

	excluded := make(map[string]bool, len(f.ExcludeTypes))
	for _, t := range f.ExcludeTypes {
		excluded[t] = true
	}

	var out []Result
	for _, r := range in {
		if excluded[r.Type] {
			continue
		}
		if f.NameGlob != "" {
			matched, err := path.Match(f.NameGlob, r.Name)
			if err != nil {
				return nil, fmt.Errorf("--name %q is not a valid pattern: %w", f.NameGlob, err)
			}
			if !matched {
				continue
			}
		}
		if !f.matchesTags(reg, r) {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// matchesTags reports whether r carries every tag the filter names, with the
// exact value it names.
//
// The tag map is resolved through tagMap, the same alias fold naming uses, so
// `--tag Name=app1` works whatever the plugin spells the attribute. A second
// lookup rule here would fail silently: the flag would simply match nothing.
//
// A resource with no tag map at all matches no tag filter, and a filter that
// names no tags matches everything.
func (f Filter) matchesTags(reg *registry.Registry, r Result) bool {
	if len(f.Tags) == 0 {
		return true
	}
	tags, ok := tagMap(reg, r.Type, r.Attributes)
	if !ok {
		return false
	}
	for key, want := range f.Tags {
		v, held := tags[key]
		if !held || !v.Known || v.Kind != value.KindString {
			return false
		}
		got, ok := v.AsString()
		if !ok || got != want {
			return false
		}
	}
	return true
}
