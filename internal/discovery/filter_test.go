package discovery

import (
	"fmt"
	"testing"

	"github.com/infrena/infrena/internal/registry"
	"github.com/infrena/infrena/pkg/provider"
	"github.com/infrena/infrena/pkg/schema"
	"github.com/infrena/infrena/pkg/value"
)

// namedResults builds results named the way Walk names them, through Unique and
// a shared taken map, so a test about filtering is measured against the names a
// real survey would have produced rather than ones a fixture asserted.
func namedResults(t *testing.T, reg *registry.Registry, tags ...string) []Result {
	t.Helper()
	taken := map[string]string{}
	out := make([]Result, 0, len(tags))
	for i, tag := range tags {
		r := Result{
			Type:       "aws.vpc",
			ProviderID: fmt.Sprintf("v-%d", i+1),
			Attributes: tagAttributes(map[string]string{"Name": tag}),
		}
		r.Name = Unique(reg, taken, provider.DiscoveredResource{
			Type: r.Type, ProviderID: r.ProviderID, Attributes: r.Attributes,
		})
		out = append(out, r)
	}
	return out
}

// taggedResults builds results carrying the given tag maps, keyed by provider ID.
func taggedResults(t *testing.T, reg *registry.Registry, byID map[string]map[string]string) []Result {
	t.Helper()
	taken := map[string]string{}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	// Sorted, because Unique is order-dependent and Go randomises map iteration.
	for i := 0; i < len(ids); i++ {
		for j := i + 1; j < len(ids); j++ {
			if ids[j] < ids[i] {
				ids[i], ids[j] = ids[j], ids[i]
			}
		}
	}

	out := make([]Result, 0, len(ids))
	for _, id := range ids {
		r := Result{Type: "aws.vpc", ProviderID: id, Attributes: tagAttributes(byID[id])}
		r.Name = Unique(reg, taken, provider.DiscoveredResource{
			Type: r.Type, ProviderID: r.ProviderID, Attributes: r.Attributes,
		})
		out = append(out, r)
	}
	return out
}

func tagAttributes(tags map[string]string) map[string]value.Value {
	m := make(map[string]value.Value, len(tags))
	for k, v := range tags {
		m[k] = value.String(v, value.SourceProvider)
	}
	return map[string]value.Value{"Tags": value.Map(m, value.SourceProvider)}
}

// Filtering happens after naming, because Unique is order-dependent by
// construction: a filter applied first would change the names of the
// resources that survive it, so the same resource would import under
// different names depending on an unrelated flag.
func TestFilteringAfterNamingLeavesNamesUnchanged(t *testing.T) {
	reg := registryWithType(t, "aws.vpc", map[string]schema.Attribute{"Tags": {Kind: value.KindMap}})
	all := namedResults(t, reg, "app1", "app2", "app3")

	got, err := Filter{NameGlob: "vpc-app2"}.Apply(reg, all)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 1 || got[0].Name != "vpc-app2" {
		t.Errorf("Apply = %v, want one result named vpc-app2", got)
	}
}

func TestTagFiltersAreAndedTogether(t *testing.T) {
	reg := registryWithType(t, "aws.vpc", map[string]schema.Attribute{"Tags": {Kind: value.KindMap}})
	in := taggedResults(t, reg, map[string]map[string]string{
		"vpc-1": {"Name": "app1", "env": "prod"},
		"vpc-2": {"Name": "app1", "env": "dev"},
	})

	got, err := Filter{Tags: map[string]string{"Name": "app1", "env": "prod"}}.Apply(reg, in)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 1 {
		t.Errorf("Apply returned %d results, want 1", len(got))
	}
}

// The tag map is found through the same alias fold naming uses, so --tag works
// whatever the plugin spells the attribute. A second lookup rule here would go
// stale against that one, and a filter would stop matching what naming matched.
func TestTagFiltersFindTheTagMapWhateverItIsCalled(t *testing.T) {
	reg := registryWithType(t, "aws.vpc", map[string]schema.Attribute{
		"TagSet": {Kind: value.KindMap, Aliases: []string{"tags"}},
	})
	in := []Result{{
		Name: "vpc-app1", Type: "aws.vpc", ProviderID: "vpc-1",
		Attributes: map[string]value.Value{"TagSet": value.Map(map[string]value.Value{
			"env": value.String("prod", value.SourceProvider),
		}, value.SourceProvider)},
	}}

	got, err := Filter{Tags: map[string]string{"env": "prod"}}.Apply(reg, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("Apply returned %d results, want the one tagged env=prod", len(got))
	}
}

func TestExcludeTypeRemovesEveryResourceOfThatType(t *testing.T) {
	reg := registryWithTypes(t, map[string]map[string]schema.Attribute{
		"aws.vpc": nil, "aws.subnet": nil,
	})
	in := []Result{{Name: "vpc-a", Type: "aws.vpc"}, {Name: "subnet-a", Type: "aws.subnet"}}

	got, err := Filter{ExcludeTypes: []string{"aws.subnet"}}.Apply(reg, in)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 1 || got[0].Type != "aws.vpc" {
		t.Errorf("Apply = %v, want only the vpc", got)
	}
}

// path.Match reports a syntax error, and a silently-empty result would look
// like an empty account.
func TestABadGlobIsAnErrorNotAnEmptyResult(t *testing.T) {
	reg := registryWithType(t, "aws.vpc", nil)

	if _, err := (Filter{NameGlob: "["}).Apply(reg, []Result{{Name: "vpc-a", Type: "aws.vpc"}}); err == nil {
		t.Fatal("a malformed glob was accepted")
	}
	// And against an empty account too, where a per-result check would never
	// run and the user would read "nothing found" for a typo in a flag.
	if _, err := (Filter{NameGlob: "["}).Apply(reg, nil); err == nil {
		t.Error("a malformed glob was accepted against an empty survey")
	}
}

// The control. An empty Filter is what every command that passes no flag uses,
// and it must return the whole survey rather than nothing.
func TestAnEmptyFilterKeepsEverything(t *testing.T) {
	reg := registryWithType(t, "aws.vpc", nil)
	in := []Result{{Name: "vpc-a", Type: "aws.vpc"}, {Name: "vpc-b", Type: "aws.vpc"}}

	got, err := Filter{}.Apply(reg, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("Apply = %v, want the whole survey", got)
	}
}
