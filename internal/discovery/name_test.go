package discovery

import (
	"strings"
	"testing"

	"github.com/infrata/infrata/internal/config"
	"github.com/infrata/infrata/pkg/provider"
	"github.com/infrata/infrata/pkg/value"
)

func discovered(id string, attrs map[string]value.Value) provider.DiscoveredResource {
	return provider.DiscoveredResource{Type: "fake.database", ProviderID: id, Attributes: attrs}
}

func str(s string) value.Value { return value.String(s, value.SourceProvider) }

func TestNameComesFromTheNameTagWhenThereIsOne(t *testing.T) {
	for _, tc := range []struct {
		label string
		attrs map[string]value.Value
	}{
		{"a name attribute", map[string]value.Value{"name": str("orders")}},
		{"a capitalised Name attribute", map[string]value.Value{"Name": str("orders")}},
		{"an AWS-style Name tag", map[string]value.Value{
			"tags": value.Map(map[string]value.Value{"Name": str("orders")}, value.SourceProvider),
		}},
		{"a lowercase name tag", map[string]value.Value{
			"tags": value.Map(map[string]value.Value{"name": str("orders")}, value.SourceProvider),
		}},
	} {
		t.Run(tc.label, func(t *testing.T) {
			// The ID is deliberately a usable name too, so a build that ignored
			// the tag would produce something valid rather than something
			// broken — and would still be wrong.
			if got := Name(discovered("db-9", tc.attrs)); got != "orders" {
				t.Errorf("Name = %q, want %q from %s", got, "orders", tc.label)
			}
		})
	}
}

// TestNameFallsBackToTheProviderID — the fallback is what a person pastes into
// a console to find the thing again, so it stays spelled the way the provider
// spells it wherever that is legal.
func TestNameFallsBackToTheProviderID(t *testing.T) {
	for _, tc := range []struct{ id, want string }{
		{"net-1", "net-1"},
		{"i-0abc123", "i-0abc123"},
		{"vpc-0a1b2c3d", "vpc-0a1b2c3d"},
		{"db_9", "db_9"},
		// Only what the name rule actually refuses is rewritten.
		{"my db", "my_db"},
		{"a.b", "a_b"},
		{"arn:aws:rds:x", "arn_aws_rds_x"},
		// A leading digit or hyphen may not lead, and is prefixed rather than
		// dropped: `0abc` and `abc` are different resources.
		{"0abc", "_0abc"},
		{"-abc", "_-abc"},
	} {
		if got := Name(discovered(tc.id, nil)); got != tc.want {
			t.Errorf("Name(%q) = %q, want %q", tc.id, got, tc.want)
		}
	}
}

// TestEveryGeneratedNameIsOneTheDecoderAccepts is the assertion that makes the
// rest of this file worth anything.
//
// Generation writes configuration a user never typed. A name this package emits
// and internal/config refuses produces a project that does not load, with a
// diagnostic pointing at a file nobody wrote — so the two must agree by
// CONSTRUCTION, which is why sanitise asks config.ValidResourceName rather than
// keeping its own copy of the rule. This test is what notices if that ever
// becomes a second copy.
func TestEveryGeneratedNameIsOneTheDecoderAccepts(t *testing.T) {
	ids := []string{
		"net-1", "i-0abc123", "0abc", "-abc", "a.b", "my db", "arn:aws:rds:prod-db",
		"__", "---", "9", "x", "Ünïcödé", "tab\there", "a/b/c", "", "  ",
		strings.Repeat("z", 300),
	}
	taken := map[string]string{}
	for _, id := range ids {
		for _, name := range []string{Name(discovered(id, nil)), Unique(taken, discovered(id, nil))} {
			if name == "" {
				t.Errorf("id %q produced an empty name", id)
				continue
			}
			if !config.ValidResourceName(name) {
				t.Errorf("id %q produced %q, which internal/config refuses — generated "+
					"configuration would not load", id, name)
			}
		}
	}
}

// TestCollidingNamesBothSurvive is the one that matters. Two resources tagged
// `orders` are TWO resources; a silently merged pair is a resource dropped, and
// a dropped resource is one invariant 1 proposes destroying.
func TestCollidingNamesBothSurvive(t *testing.T) {
	taken := map[string]string{}
	a := Unique(taken, discovered("db-9", map[string]value.Value{"name": str("orders")}))
	b := Unique(taken, discovered("db-10", map[string]value.Value{"name": str("orders")}))

	if a != "orders" {
		t.Errorf("the first `orders` = %q, want the unsuffixed name", a)
	}
	if b == a {
		t.Fatalf("both resources are called %q — one of them has been lost", a)
	}
	// The suffix is the provider ID, so the second is still findable.
	if !strings.Contains(b, "db-10") {
		t.Errorf("the suffixed name %q does not contain its provider ID, so nothing connects "+
			"it to the resource it describes", b)
	}
	if len(taken) != 2 {
		t.Errorf("taken holds %d names for 2 resources: %v", len(taken), taken)
	}
}

// TestACollidingSuffixAlsoResolves — the case the loop in Unique exists for. A
// resource already NAMED `orders_db-9` and one tagged `orders` with ID `db-9`
// generate the same candidate.
func TestACollidingSuffixAlsoResolves(t *testing.T) {
	taken := map[string]string{}
	first := Unique(taken, discovered("x-1", map[string]value.Value{"name": str("orders")}))
	second := Unique(taken, discovered("y-1", map[string]value.Value{"name": str("orders_x-1")}))
	third := Unique(taken, discovered("x-1", map[string]value.Value{"name": str("orders")}))

	names := map[string]bool{first: true, second: true, third: true}
	if len(names) != 3 {
		t.Errorf("three resources produced %d distinct names: %q, %q, %q", len(names), first, second, third)
	}
	for _, n := range []string{first, second, third} {
		if !config.ValidResourceName(n) {
			t.Errorf("%q is not a valid resource name", n)
		}
	}
}

// TestAnEmptyOrUnusableNameTagFallsBack. A tag of "", "  ", an unknown value or
// a non-string is not a name, and must not produce an empty or invalid one.
func TestAnEmptyOrUnusableNameTagFallsBack(t *testing.T) {
	for _, tc := range []struct {
		label string
		attrs map[string]value.Value
	}{
		{"an empty tag", map[string]value.Value{"name": str("")}},
		{"a whitespace tag", map[string]value.Value{"name": str("   ")}},
		{"a numeric tag", map[string]value.Value{"name": value.Int(123, value.SourceProvider)}},
		{"an unknown tag", map[string]value.Value{"name": value.Unknown(value.KindString, value.SourceProvider)}},
		{"a tags value that is not a map", map[string]value.Value{"tags": str("nope")}},
		{"no attributes at all", nil},
	} {
		t.Run(tc.label, func(t *testing.T) {
			if got := Name(discovered("db-9", tc.attrs)); got != "db-9" {
				t.Errorf("Name = %q, want the provider ID fallback for %s", got, tc.label)
			}
		})
	}
}

// TestNamingIsDeterministic — the same resource named twice from a fresh table
// is the same name. Attribute lookup walks a fixed list rather than a map, and
// Go's map iteration is randomised.
func TestNamingIsDeterministic(t *testing.T) {
	r := discovered("db-9", map[string]value.Value{
		"name": str("orders"),
		"Name": str("ORDERS"),
		"tags": value.Map(map[string]value.Value{"Name": str("tagged"), "name": str("also")}, value.SourceProvider),
	})
	first := Name(r)
	for i := range 50 {
		if got := Name(r); got != first {
			t.Fatalf("run %d named it %q, first run said %q", i, got, first)
		}
	}
	if first != "orders" {
		t.Errorf("Name = %q; `name` is first in the documented order", first)
	}
}
