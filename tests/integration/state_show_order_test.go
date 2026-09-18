package integration

import (
	"sort"
	"strings"
	"testing"
)

// TestStateShowPrintsAttributesInAStableOrder covers the one sort in the CLI
// with no upstream protector.
//
// cli.sortedAttributeKeys is the ONLY thing making `state show`'s attribute
// order deterministic — Go randomises map iteration, and measurement found 4
// distinct orderings in 15 runs with the sort removed. Nothing failed when it
// was removed, which is what made it worth a test rather than a note: a sort
// that nothing depends on looks exactly like a sort nothing would miss.
//
// What hid it was FIXTURE SIZE. The existing tests that exercise `state show`
// print resources with two or three attributes, where a randomised order is
// often the sorted one by luck. fake.database has six, so a run landing sorted
// by chance is 1 in 720 and ten consecutive such runs are not worth worrying
// about.
//
// Asserted two ways on purpose. Sorted order is the property the code claims;
// run-to-run identity is the property a user actually notices, when a diff of
// two `state show` outputs is all churn. Either alone is satisfiable by
// something that is not the other.
func TestStateShowPrintsAttributesInAStableOrder(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  db:
    type: fake.database
    engine: postgres
    size: 20
    password: hunter2
    network: ${net}
    tags:
      team: platform
`)

	if r := run(t, dir, "apply", "dev", "--auto-approve"); r.ExitCode != 2 {
		t.Fatalf("apply exit = %d, want 2\n%s", r.ExitCode, r.combined())
	}

	var first []string
	for i := range 10 {
		r := run(t, dir, "state", "show", "dev", "db")
		if r.ExitCode != 0 {
			t.Fatalf("state show exit = %d\n%s", r.ExitCode, r.combined())
		}
		got := attributeNames(r.Stdout)
		if len(got) < 5 {
			t.Fatalf("run %d printed %d attributes, want the database's six — "+
				"a fixture this small cannot detect a random order:\n%s", i, len(got), r.Stdout)
		}
		if !sort.StringsAreSorted(got) {
			t.Fatalf("run %d printed attributes out of order: %v\n%s", i, got, r.Stdout)
		}
		if first == nil {
			first = got
			continue
		}
		if strings.Join(got, ",") != strings.Join(first, ",") {
			t.Fatalf("run %d printed %v, run 0 printed %v — the order changes between runs",
				i, got, first)
		}
	}
}

// attributeNames pulls the attribute names out of `state show` output: the
// indented "name  value" lines, minus the provider bookkeeping printed above
// them, which is deliberately NOT sorted with the attributes and would break
// the ordering assertion if counted as one.
func attributeNames(stdout string) []string {
	var out []string
	for _, line := range strings.Split(stdout, "\n") {
		if !strings.HasPrefix(line, "  ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "provider", "provider_id":
			continue
		}
		out = append(out, fields[0])
	}
	return out
}
