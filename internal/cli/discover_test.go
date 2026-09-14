package cli

import (
	"strings"
	"testing"

	"github.com/infrena/infrena/internal/discovery"
)

func result(typ, id, name string) discovery.Result {
	return discovery.Result{Type: typ, ProviderID: id, Name: name, Provider: "fake"}
}

// TestDiscoverShowsTheNameEachResourceWouldGet. §25's table exists so a user
// sees what import WOULD do before it does it.
func TestDiscoverShowsTheNameEachResourceWouldGet(t *testing.T) {
	var sb strings.Builder
	renderDiscovered(&sb, []discovery.Result{
		result("fake.database", "db-9", "orders"),
		result("fake.network", "vpc-0a1b", "vpc-0a1b"),
	}, nil)
	got := sb.String()

	for _, want := range []string{"TYPE", "ID", "NAME", "fake.database", "db-9", "orders", "vpc-0a1b"} {
		if !strings.Contains(got, want) {
			t.Errorf("output does not contain %q:\n%s", want, got)
		}
	}
	// The command is read-only and must say so. A user who thinks `discover`
	// adopted something will not run `import`, and will find out at the next
	// apply — which is the apply that destroys what they thought was managed.
	if !strings.Contains(got, "Nothing has been imported") {
		t.Errorf("the output does not say that nothing was imported:\n%s", got)
	}
}

// TestDiscoverMakesACollisionVisible is the reason the NAME column exists at
// all. Two resources tagged the same must show two different proposed names,
// here, before anything is written.
func TestDiscoverMakesACollisionVisible(t *testing.T) {
	var sb strings.Builder
	renderDiscovered(&sb, []discovery.Result{
		result("fake.database", "db-9", "orders"),
		result("fake.database", "db-10", "orders_db-10"),
	}, nil)
	got := sb.String()

	if strings.Count(got, "orders") < 2 {
		t.Fatalf("both proposed names must appear:\n%s", got)
	}
	// And they must DIFFER. A table printing `orders` twice tells a user
	// nothing is wrong, and the import then silently merges two resources.
	lines := strings.Split(got, "\n")
	var names []string
	for _, l := range lines {
		if f := strings.Fields(l); len(f) == 3 && strings.HasPrefix(f[0], "fake.") {
			names = append(names, f[2])
		}
	}
	if len(names) != 2 {
		t.Fatalf("want 2 resource rows, got %v:\n%s", names, got)
	}
	if names[0] == names[1] {
		t.Errorf("both resources are shown as %q — a collision that is invisible here is one "+
			"a user meets after importing", names[0])
	}
}

// TestDiscoverOnAnEmptyAccountSaysSo, and distinguishes a filtered question
// from an unfiltered one — "nothing of that type" and "nothing at all" send a
// user to different next steps.
func TestDiscoverOnAnEmptyAccountSaysSo(t *testing.T) {
	var all, filtered strings.Builder
	renderDiscovered(&all, nil, nil)
	renderDiscovered(&filtered, nil, []string{"fake.database"})

	if !strings.Contains(all.String(), "Nothing found.") {
		t.Errorf("unfiltered empty output = %q", all.String())
	}
	if !strings.Contains(filtered.String(), "fake.database") {
		t.Errorf("a filtered empty result must name the type asked about, or a user cannot "+
			"tell it from an empty account: %q", filtered.String())
	}
}

// TestDiscoverCountsWhatItFound — the summary line is what a user reads when
// the table is long enough to scroll.
func TestDiscoverCountsWhatItFound(t *testing.T) {
	var one, two strings.Builder
	renderDiscovered(&one, []discovery.Result{result("fake.database", "db-9", "orders")}, nil)
	renderDiscovered(&two, []discovery.Result{
		result("fake.database", "db-9", "orders"),
		result("fake.network", "net-1", "net-1"),
	}, nil)

	if !strings.Contains(one.String(), "1 resource found") {
		t.Errorf("singular count is wrong: %q", one.String())
	}
	if !strings.Contains(two.String(), "2 resources found") {
		t.Errorf("plural count is wrong: %q", two.String())
	}
}
