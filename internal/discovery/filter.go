package discovery

import (
	"fmt"
	"path"

	"github.com/infrena/infrena/internal/registry"
	"github.com/infrena/infrena/pkg/value"
)

// Filter narrows a survey. A zero Filter keeps everything, which is what every
// command that was given no flag passes.
//
// The fields are ANDed: a resource survives only if it satisfies all of them.
// Each one on its own is an OR of nothing, so there is no ambiguity to document
// — `--tag a=1 --tag b=2` means both, because a user who meant either would say
// so with two runs and a user who meant both has no other way to say it.
type Filter struct {
	// Tags are tag key to exact value, looked up inside the resource's tag map.
	Tags map[string]string
	// ExcludeTypes removes every resource of these types. Exclusion rather than
	// inclusion, because `discover [type...]` already answers "only these" and a
	// second inclusive flag would be two spellings of one question.
	ExcludeTypes []string
	// NameGlob is a path.Match pattern against the PROPOSED name.
	NameGlob string
}

// Apply narrows in, keeping order.
//
// AFTER NAMING, always. Unique is order-dependent by construction: it settles a
// collision by suffixing the provider ID of whichever resource it met second,
// so filtering first would change the names of the resources that survive. The
// same resource would then import under one name with a flag and another
// without it, which is the kind of difference nobody thinks to look for.
//
// That is also why this takes named Results rather than DiscoveredResources:
// the signature makes the ordering hard to get wrong.
func (f Filter) Apply(reg *registry.Registry, in []Result) ([]Result, error) {
	// Checked BEFORE the loop, so a malformed pattern is an error even when the
	// survey is empty. A per-result check alone would report "nothing found" for
	// a typo in a flag against an account that has nothing in it, and an empty
	// account is exactly the answer a user cannot distinguish from a mistake.
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
// The tag map is resolved through tagMap, the SAME alias fold naming uses, so
// `--tag Name=app1` works whatever the plugin spells the attribute. A second
// lookup rule here would be the defect Task 1 removed, reintroduced one file
// over, and it would be invisible: the flag would simply match nothing.
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
