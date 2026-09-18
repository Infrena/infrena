package integration

import (
	"strings"
	"testing"
)

const oneNetwork = `
project: myapp
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
`

// TestImportAsChoosesTheName.
//
// Discovery PROPOSES a name from a tag or the provider ID, and until now the
// proposal was permanent: nothing renamed a managed resource, so adopting a VPC
// meant living with `network-vpc-0a1b2c3d` or editing state by hand.
//
// Import time is the one moment renaming is safe, which is why the flag is
// here and not a general `state mv`: nothing references the resource yet, so
// there is no dependency edge, no generated file and no other resource's
// ${...} to rewrite.
func TestImportAsChoosesTheName(t *testing.T) {
	dir := project(t, oneNetwork)
	if a := run(t, dir, "apply", "dev", "--auto-approve"); a.ExitCode != 2 {
		t.Fatalf("seeding: %d\n%s", a.ExitCode, a.combined())
	}
	// Forget it so there is something unmanaged to adopt.
	if r := run(t, dir, "state", "rm", "dev", "net", "--auto-approve"); r.ExitCode != 0 {
		t.Fatalf("state rm: %d\n%s", r.ExitCode, r.combined())
	}

	r := run(t, dir, "import", "dev", "fake.network.net-1", "--as", "web-vpc", "--generate")
	if r.ExitCode != 0 {
		t.Fatalf("import exit = %d\n%s", r.ExitCode, r.combined())
	}
	if !strings.Contains(r.combined(), "web-vpc") {
		t.Errorf("import did not use the chosen name:\n%s", r.combined())
	}
	// The name must reach STATE, not just the message.
	if s := run(t, dir, "state", "show", "dev", "web-vpc"); s.ExitCode != 0 {
		t.Errorf("web-vpc is not in state under that name:\n%s", s.combined())
	}
}

// TestImportAsNeedsExactlyOneSelector. --as names one resource, so anything
// that could select a different number makes it ambiguous — and an ambiguous
// rename puts a name on something the user was not looking at.
func TestImportAsNeedsExactlyOneSelector(t *testing.T) {
	dir := project(t, oneNetwork)

	for _, args := range [][]string{
		{"import", "dev", "--as", "web-vpc"},
		{"import", "dev", "fake.network.net-1", "fake.network.net-2", "--as", "web-vpc"},
	} {
		r := run(t, dir, args...)
		if r.ExitCode == 0 {
			t.Errorf("%v was accepted; --as needs exactly one selector\n%s", args, r.combined())
		}
		if !strings.Contains(r.combined(), "exactly one selector") {
			t.Errorf("%v: the refusal does not explain itself:\n%s", args, r.combined())
		}
	}
}

// TestStateRmStopsManagingWithoutDestroying.
//
// Three import diagnostics told the reader to run `infrena state rm` before it
// existed — a suggested action nobody could take. The danger of the command is
// that it LOOKS like a deletion, so the assertion that matters is the last one:
// the resource is still there afterwards.
func TestStateRmStopsManagingWithoutDestroying(t *testing.T) {
	dir := project(t, oneNetwork)
	if a := run(t, dir, "apply", "dev", "--auto-approve"); a.ExitCode != 2 {
		t.Fatalf("seeding: %d\n%s", a.ExitCode, a.combined())
	}

	r := run(t, dir, "state", "rm", "dev", "net", "--auto-approve")
	if r.ExitCode != 0 {
		t.Fatalf("state rm exit = %d\n%s", r.ExitCode, r.combined())
	}
	if !strings.Contains(r.combined(), "still exists") {
		t.Errorf("the output does not say the resource survives, which is the thing most likely "+
			"to be misunderstood:\n%s", r.combined())
	}
	if s := run(t, dir, "state", "show", "dev", "net"); s.ExitCode == 0 {
		t.Error("the resource is still in state after state rm")
	}

	// THE RESOURCE SURVIVES. discover finds it again, which is only possible
	// if the provider still has it — a destroy would have removed it.
	d := run(t, dir, "discover")
	if !strings.Contains(d.combined(), "net-1") {
		t.Errorf("net-1 is gone from the provider, so state rm destroyed it:\n%s", d.combined())
	}
}

// TestStateRmRefusesAnUnknownAddress. A typo must cost nothing, and must not
// take the lock on the way to finding that out.
func TestStateRmRefusesAnUnknownAddress(t *testing.T) {
	dir := project(t, oneNetwork)
	if a := run(t, dir, "apply", "dev", "--auto-approve"); a.ExitCode != 2 {
		t.Fatalf("seeding: %d\n%s", a.ExitCode, a.combined())
	}

	r := run(t, dir, "state", "rm", "dev", "nope", "--auto-approve")
	if r.ExitCode == 0 {
		t.Fatalf("state rm accepted an address that is not managed\n%s", r.combined())
	}
	// And the real one is untouched.
	if s := run(t, dir, "state", "show", "dev", "net"); s.ExitCode != 0 {
		t.Errorf("a failed state rm disturbed state:\n%s", s.combined())
	}
}
