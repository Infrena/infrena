package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestImportRefusesAResourceAnotherAddressAlreadyManages.
//
// The hazard is §26's, arriving through a door the guard did not cover. `import`
// refused a selection whose NAME was already in state, which is the collision a
// user expects to hit. It never checked the PROVIDER ID — so importing the same
// real resource under a different name sailed through, and state ended up with
// two addresses managing one piece of infrastructure.
//
// What makes that data loss rather than untidiness: the new address declares
// nothing in configuration, so invariant 1 schedules it for destruction, and
// destroying it calls Delete on the provider ID the OTHER address still manages
// and configuration still declares. The next `apply` deletes live, declared,
// managed infrastructure and reports success. Measured before the fix: the real
// resource was gone from the fake cloud and `network` remained in state pointing
// at nothing.
//
// On AWS that is a VPC.
func TestImportRefusesAResourceAnotherAddressAlreadyManages(t *testing.T) {
	dir := project(t, `
project: dup
environments:
  dev: {}
resources:
  network:
    type: fake.network
    cidr: 10.0.0.0/16
`)

	// Exit 2 is "changes were applied", this suite's convention.
	if r := run(t, dir, "apply", "dev", "--auto-approve"); r.ExitCode != 2 {
		t.Fatalf("apply: %d\n%s", r.ExitCode, r.combined())
	}

	// net-1 is what the fake provider allocated for `network` above. Importing it
	// under its own ID as a name is the shape a user reaches for after `discover`
	// lists something they did not realise was already managed.
	imp := run(t, dir, "import", "dev", "fake.network.net-1")
	if imp.ExitCode == 0 {
		t.Errorf("import succeeded; adopting a provider ID another address manages must be refused\n%s", imp.combined())
	}
	for _, want := range []string{
		"net-1",   // the provider ID at issue
		"network", // the address that already manages it — without this a user cannot act
	} {
		requireContains(t, imp.combined(), want)
	}

	// The refusal has to happen BEFORE state is written, or the message is advice
	// about damage already done.
	list := run(t, dir, "state", "list", "dev")
	if strings.Contains(list.Stdout, "fake.network.net-1") {
		t.Errorf("state gained a second address for one resource:\n%s", list.Stdout)
	}

	// The real test of the guard: nothing is now scheduled for destruction.
	plan := run(t, dir, "plan", "dev")
	requireContains(t, plan.combined(), "0 to destroy")

	// And the resource itself is still there. A plan that proposes nothing proves
	// the state is consistent; only the cloud file proves nothing was deleted.
	cloud, err := os.ReadFile(filepath.Join(dir, ".infra", "fake-cloud.json"))
	if err != nil {
		t.Fatalf("read cloud: %v", err)
	}
	if !strings.Contains(string(cloud), "net-1") {
		t.Errorf("the real resource is gone from the cloud:\n%s", cloud)
	}
}

// TestImportRefusesAnIDTwoAccountsBothHold.
//
// The unit tests in internal/cli cover the choosing; this is the shape a user is in
// when they meet it, and the only place the two halves are proved together: discovery
// really does return one ID twice when two instances hold it, and import really does
// refuse rather than pick.
//
// The fixture writes both cloud files by hand, because two accounts holding the same
// provider ID is exactly what infrata cannot produce itself — it allocates IDs per
// cloud file, so this is pre-existing infrastructure, which is the only way the
// situation arises and precisely the situation `import` is for.
func TestImportRefusesAnIDTwoAccountsBothHold(t *testing.T) {
	dir := project(t, `
project: amb
environments:
  dev: {}
providers:
  - plugin: fake
    name: main
  - plugin: fake
    name: acct2
resources: {}
`)
	if err := os.MkdirAll(filepath.Join(dir, ".infra"), 0o755); err != nil {
		t.Fatal(err)
	}
	const oneNetwork = `{"resources":{"net-1":{"type":"fake.network",` +
		`"attributes":{"cidr":"10.0.0.0/16","id":"net-1"}}}}`
	for _, instance := range []string{"main", "acct2"} {
		writeFile(t, filepath.Join(dir, ".infra", "fake-cloud-"+instance+".json"), oneNetwork)
	}

	ambiguous := run(t, dir, "import", "dev", "fake.network.net-1")
	if ambiguous.ExitCode == 0 {
		t.Errorf("import adopted one of two candidates silently:\n%s", ambiguous.combined())
	}
	for _, want := range []string{"more than one provider instance", "main", "acct2", "--provider"} {
		requireContains(t, ambiguous.combined(), want)
	}

	// Nothing was written: the refusal must come before state is touched, or the
	// message is advice about damage already done.
	if list := run(t, dir, "state", "list", "dev"); strings.Contains(list.Stdout, "net-1") {
		t.Errorf("a refused import still wrote state:\n%s", list.Stdout)
	}

	// The way out works, and adopts from the account named rather than the other one.
	narrowed := run(t, dir, "import", "dev", "fake.network.net-1", "--provider", "acct2")
	if narrowed.ExitCode != 0 {
		t.Fatalf("--provider did not resolve the ambiguity:\n%s", narrowed.combined())
	}
	show := run(t, dir, "state", "show", "dev", "net-1")
	requireContains(t, show.combined(), "acct2")
}
