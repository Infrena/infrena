package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infrena/infrena/internal/discovery"
	"github.com/infrena/infrena/providers/test"
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
	}, nil, nil, false)
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
	}, nil, nil, false)
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
	renderDiscovered(&all, nil, nil, nil, false)
	renderDiscovered(&filtered, nil, []string{"fake.database"}, nil, false)

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
	renderDiscovered(&one, []discovery.Result{result("fake.database", "db-9", "orders")}, nil, nil, false)
	renderDiscovered(&two, []discovery.Result{
		result("fake.database", "db-9", "orders"),
		result("fake.network", "net-1", "net-1"),
	}, nil, nil, false)

	if !strings.Contains(one.String(), "1 resource found") {
		t.Errorf("singular count is wrong: %q", one.String())
	}
	if !strings.Contains(two.String(), "2 resources found") {
		t.Errorf("plural count is wrong: %q", two.String())
	}
}

// newProjectWithDiscoverableResources stands up a project whose fake cloud
// already holds resources this project never created, which is the only
// situation discovery exists for.
//
// The type is fake.vpc so that a provider ID of `vpc-1` names itself: naming is
// type-prefixed, and a prefix the ID already carries is not repeated, so the
// proposed name is the string a reader can match against the cloud file.
func newProjectWithDiscoverableResources(t *testing.T, ids ...string) string {
	t.Helper()
	dir := projectDir(t, "project: myapp\nresources: {}\n")
	cloud := &test.Cloud{Resources: map[string]*test.CloudResource{}}
	for _, id := range ids {
		cloud.Resources[id] = &test.CloudResource{
			Type:       "fake.vpc",
			Attributes: map[string]any{"id": id},
		}
	}
	if err := cloud.Save(filepath.Join(dir, test.DefaultCloudPath)); err != nil {
		t.Fatal(err)
	}
	return dir
}

// importOne adopts one discovered resource into an environment, so a later
// assertion is measured against state a real command wrote rather than against
// a hand-built fixture that could disagree with it.
func importOne(t *testing.T, dir, environment, providerID string) {
	t.Helper()
	_, stderr, code := runCommand(t, dir, "import", environment, "fake.vpc."+providerID)
	if code != ExitOK {
		t.Fatalf("importing %s into %s: exit %d\n%s", providerID, environment, code, stderr)
	}
}

func TestDiscoverHidesWhatIsAlreadyManaged(t *testing.T) {
	dir := newProjectWithDiscoverableResources(t, "vpc-1", "vpc-2")
	importOne(t, dir, "dev", "vpc-1")

	stdout, _, _ := runCommand(t, dir, "discover")

	if strings.Contains(stdout, "vpc-1") {
		t.Errorf("discover listed a managed resource:\n%s", stdout)
	}
	if !strings.Contains(stdout, "vpc-2") {
		t.Errorf("discover hid an unmanaged resource:\n%s", stdout)
	}
	// A count a user cannot see is a count they will assume is zero.
	if !strings.Contains(stdout, "already managed") {
		t.Errorf("footer does not state the exclusion:\n%s", stdout)
	}
}

// Scope is EVERY environment, not the one you happen to be importing into.
func TestManagedMeansManagedInAnyEnvironment(t *testing.T) {
	dir := newProjectWithDiscoverableResources(t, "vpc-1")
	importOne(t, dir, "production", "vpc-1")

	stdout, _, _ := runCommand(t, dir, "discover")

	if strings.Contains(stdout, "vpc-1") {
		t.Errorf("discover listed a resource managed in another environment:\n%s", stdout)
	}
}

// The STATUS column only earns its place under --all: in the default view
// every row would read unmanaged, and a column with one value is noise.
func TestAllShowsEverythingWithAStatusColumn(t *testing.T) {
	dir := newProjectWithDiscoverableResources(t, "vpc-1", "vpc-2")
	importOne(t, dir, "dev", "vpc-1")

	all, _, _ := runCommand(t, dir, "discover", "--all")
	def, _, _ := runCommand(t, dir, "discover")

	if !strings.Contains(all, "STATUS") || !strings.Contains(all, "managed (dev)") {
		t.Errorf("--all lacks the status column:\n%s", all)
	}
	if strings.Contains(def, "STATUS") {
		t.Errorf("default view grew a status column:\n%s", def)
	}
}

// Refused, not silently skipped. A selector that names a managed resource is a
// user asking for something specific, and quietly importing nothing would
// report success for a thing that did not happen.
func TestImportWillNotReadoptAManagedResource(t *testing.T) {
	dir := newProjectWithDiscoverableResources(t, "vpc-1")
	importOne(t, dir, "dev", "vpc-1")

	_, stderr, _ := runCommand(t, dir, "import", "dev", "fake.vpc.vpc-1", "--generate")

	if !strings.Contains(stderr, "already managed") {
		t.Errorf("import re-adopted a managed resource, or did not say why not:\n%s", stderr)
	}
}

// The managed index is keyed on PROVIDER ID rather than on the address that was
// chosen for it: the same real resource adopted twice would have two addresses,
// and the question discover asks is whether the RESOURCE is under management.
func TestTheManagedIndexIsKeyedOnTheProviderIDAcrossEnvironments(t *testing.T) {
	dir := newProjectWithDiscoverableResources(t, "vpc-1", "vpc-2")
	importOne(t, dir, "dev", "vpc-1")
	importOne(t, dir, "production", "vpc-2")

	managed, err := managedProviderIDs(context.Background(), backendFor(dir))
	if err != nil {
		t.Fatal(err)
	}
	if managed["vpc-1"] != "dev" || managed["vpc-2"] != "production" {
		t.Errorf("managedProviderIDs = %v, want vpc-1 in dev and vpc-2 in production", managed)
	}
}

// A project that has never applied anything is the ordinary case for discover,
// and it must not be an error.
func TestTheManagedIndexIsEmptyForAProjectWithNoState(t *testing.T) {
	dir := newProjectWithDiscoverableResources(t, "vpc-1")

	managed, err := managedProviderIDs(context.Background(), backendFor(dir))
	if err != nil {
		t.Fatalf("managedProviderIDs on a project with no state: %v", err)
	}
	if len(managed) != 0 {
		t.Errorf("managedProviderIDs = %v, want empty", managed)
	}
}

// newProjectWithSystemOwnedResource stands up a project whose fake cloud holds
// one resource THE CLOUD declares it owns.
//
// The declaration lives in the cloud file, which is the plugin's own world,
// because that is the whole point of §3.5: the engine cannot decide that a VPC
// is a default VPC without learning about AWS, so the plugin is the only party
// that can say it.
func newProjectWithSystemOwnedResource(t *testing.T, id, reason string) string {
	t.Helper()
	dir := projectDir(t, "project: myapp\nresources: {}\n")
	cloud := &test.Cloud{Resources: map[string]*test.CloudResource{
		id: {
			Type:              "fake.vpc",
			Attributes:        map[string]any{"id": id},
			SystemOwned:       true,
			SystemOwnedReason: reason,
		},
	}}
	if err := cloud.Save(filepath.Join(dir, test.DefaultCloudPath)); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestDiscoverFlagsWhatTheCloudOwns(t *testing.T) {
	dir := newProjectWithSystemOwnedResource(t, "vpc-default", "the account's default VPC")

	stdout, _, _ := runCommand(t, dir, "discover")

	if !strings.Contains(stdout, "the account's default VPC") {
		t.Errorf("discover did not say why the resource is flagged:\n%s", stdout)
	}
}

// Never adopted by default and never silently. Naming it explicitly is the
// escape hatch, because adopting a default VPC is occasionally right and the
// engine should not be the one forbidding it.
func TestImportSkipsSystemOwnedUnlessNamed(t *testing.T) {
	dir := newProjectWithSystemOwnedResource(t, "vpc-default", "the account's default VPC")

	_, stderr, _ := runCommand(t, dir, "import", "dev", "--generate")
	if !strings.Contains(stderr, "skipped") {
		t.Errorf("import did not report the skip:\n%s", stderr)
	}
	// And the plugin's own words, so a user overriding the flag understands
	// what they are overriding.
	if !strings.Contains(stderr, "the account's default VPC") {
		t.Errorf("the skip does not say why:\n%s", stderr)
	}
	if stateExists(t, dir, "dev") {
		t.Error("a system-owned resource was adopted with no selector naming it")
	}

	_, stderr, code := runCommand(t, dir, "import", "dev", "fake.vpc.vpc-default", "--generate")
	if code != ExitOK {
		t.Errorf("naming it explicitly did not import it: exit %d\n%s", code, stderr)
	}
	if !stateExists(t, dir, "dev") {
		t.Error("naming it explicitly wrote no state")
	}
}
