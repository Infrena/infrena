package cli

import (
	"strings"
	"testing"
)

func explainOut(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := NewRootCommand()
	var sb strings.Builder
	cmd.SetOut(&sb)
	cmd.SetErr(&sb)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return sb.String(), err
}

// TestExplainRendersEveryGroup. Required, Optional, Computed and Capabilities
// are all rendered, and an attribute appears in exactly one group — a reader
// counting them is entitled to see the whole surface.
func TestExplainRendersEveryGroup(t *testing.T) {
	out, err := explainOut(t, "explain", "fake.database")
	if err != nil {
		t.Fatalf("explain: %v\n%s", err, out)
	}
	for _, want := range []string{
		"Required:", "engine",
		"Optional:", "password", "size",
		"Computed:", "endpoint",
		"Capabilities:", "create",
		"Import ID:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("explain output is missing %q:\n%s", want, out)
		}
	}
	// A computed attribute must NOT also be offered as settable: `endpoint`
	// appearing under Optional would tell a user to write something the
	// provider owns.
	optional := sectionOf(t, out, "Optional:")
	if strings.Contains(optional, "endpoint") {
		t.Errorf("a computed attribute was listed as settable:\n%s", optional)
	}
}

// TestExplainStatesADefaultOnce. A default no longer varies by environment, so
// there is exactly one number to print.
//
// The absence assertion is the load-bearing half: if a per-environment
// classifier survived anywhere, this output is where it would surface first,
// because `explain` reads the schema directly rather than through a compile.
func TestExplainStatesADefaultOnce(t *testing.T) {
	out, _ := explainOut(t, "explain", "fake.database")
	if !strings.Contains(out, "default: 10") {
		t.Errorf("the default must be stated:\n%s", out)
	}
	if strings.Contains(out, "in production") || strings.Contains(out, "100") {
		t.Errorf("`explain` still renders an environment-varying default:\n%s", out)
	}
}

// TestExplainSensitiveAndForceNewAreMarked. Both change what a user may safely
// do with an attribute, and neither is visible from its name or kind.
func TestExplainMarksSensitiveAndForceNew(t *testing.T) {
	out, _ := explainOut(t, "explain", "fake.database")
	if !strings.Contains(out, "sensitive") {
		t.Errorf("a sensitive attribute must be marked:\n%s", out)
	}
	if !strings.Contains(out, "replaces on change") {
		t.Errorf("a ForceNew attribute must be marked — changing it destroys the resource:\n%s", out)
	}
}

// TestExplainRendersDeclaredReferences. A provider-declared reference is what
// lets `vpc_id: ${vpc}` project to the right attribute
// instead of a user guessing id versus arn. `explain` is where that
// declaration must be discoverable, so a user can learn what a resource may
// be passed without reading the plugin's source.
func TestExplainRendersDeclaredReferences(t *testing.T) {
	out, err := explainOut(t, "explain", "fake.subnet")
	if err != nil {
		t.Fatalf("explain: %v\n%s", err, out)
	}
	var vpcIDLine, cidrLine string
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.Contains(line, "vpc_id"):
			vpcIDLine = line
		case strings.Contains(line, "cidr"):
			cidrLine = line
		}
	}
	if !strings.Contains(vpcIDLine, "refers to fake.vpc.id") {
		t.Errorf("vpc_id must show its declared reference (target type and attribute):\n%s", out)
	}
	// cidr declares no reference (providers/test/definitions.go says so
	// explicitly), so it must not be marked as one.
	if strings.Contains(cidrLine, "refers to") {
		t.Errorf("cidr declares no reference and must not be shown as one:\n%s", out)
	}
}

// TestExplainRendersDeclaredFields. Where a provider declares a map
// attribute's known keys, `${vpc.tags.Nmae}` is a compile-time typo —
// but only if a reader can discover the keys in the first place. Without
// this, a declared map's keys were undiscoverable while a typo in them was
// still a compile error.
func TestExplainRendersDeclaredFields(t *testing.T) {
	out, err := explainOut(t, "explain", "fake.vpc")
	if err != nil {
		t.Fatalf("explain: %v\n%s", err, out)
	}
	var metaLine, tagsLine string
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.Contains(line, "meta"):
			metaLine = line
		case strings.Contains(line, "tags"):
			tagsLine = line
		}
	}
	if !strings.Contains(metaLine, "keys: name") {
		t.Errorf("meta declares Fields{name: ...} and must list its keys:\n%s", out)
	}
	// tags is deliberately an OPEN map (no Fields declared) — like AWS tags,
	// it takes any key — so it must not be shown with a keys list at all.
	if strings.Contains(tagsLine, "keys:") {
		t.Errorf("tags declares no Fields and must not be shown with a keys list:\n%s", out)
	}
}

// TestExplainATypeWhoseProviderIsNotInstalledSaysWhereToPutIt.
//
// `explain` needs no project: the TYPE names the plugin, so `explain aws.rds` is
// itself the instruction to load infrena-plugin-aws. When that binary is not there,
// nothing can list aws's types — nothing has ever seen them — so the actionable
// answer is the plugin, every place that was searched, and what to do about it.
//
// It deliberately does not assert that the error lists `fake.database`: with
// plugins loaded on demand, a command that never mentions `test` has no reason
// to have loaded that plugin.
func TestExplainATypeWhoseProviderIsNotInstalledSaysWhereToPutIt(t *testing.T) {
	out, err := explainOut(t, "explain", "aws.rds")
	if err == nil {
		t.Fatal("a type whose plugin is not installed must be an error")
	}
	msg := err.Error() + out
	for _, want := range []string{"aws.rds", "infrena-plugin-aws", "--plugin-dir"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error does not mention %q, so a reader cannot act on it:\n%s", want, msg)
		}
	}
}

// TestExplainListsTheKnownTypesWhenTheProviderIsThere is the other half: a type the
// loaded plugin does not offer must still list what it does.
func TestExplainListsTheKnownTypesWhenTheProviderIsThere(t *testing.T) {
	out, err := explainOut(t, "explain", "fake.nosuchthing")
	if err == nil {
		t.Fatal("an unknown type from an installed plugin must be an error")
	}
	msg := err.Error() + out
	if !strings.Contains(msg, "fake.database") {
		t.Errorf("the error must list the types that DO exist:\n%s", msg)
	}
}

// section returns the lines under a heading, up to the next blank line.
func sectionOf(t *testing.T, out, heading string) string {
	t.Helper()
	i := strings.Index(out, heading)
	if i < 0 {
		t.Fatalf("no %q section in:\n%s", heading, out)
	}
	rest := out[i+len(heading):]
	if before, _, ok := strings.Cut(rest, "\n\n"); ok {
		return before
	}
	return rest
}
