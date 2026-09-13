package generator

import (
	"strings"
	"testing"

	"github.com/infrata/infrata/pkg/value"
)

// §27's minimality against §12.1's `defaults:`. An attribute an instance already
// supplies is not something the reader has to write down, so a generated file that
// restates it is a file whose next diff is noise.

// renderWith generates one file with the given per-instance defaults.
func renderWith(t *testing.T, rs []Resource, defaults map[string]map[string]value.Value) string {
	t.Helper()
	opts := MinimalOptions()
	opts.InstanceDefaults = defaults
	files, err := Generate(rs, testRegistry(t), opts)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("got %d files, want 1", len(files))
	}
	return string(files[0].Bytes)
}

func database(instance string, attrs map[string]value.Value) Resource {
	return Resource{
		Name: "db", Type: "test.database", ProviderID: "db-1",
		Provider: instance, Attributes: attrs,
	}
}

// TestAValueEqualToItsInstanceDefaultIsOmitted. `size` is 200 here, which is NOT the
// plugin's schema default of 10 — so the omission can only come from the instance's own
// block.
func TestAValueEqualToItsInstanceDefaultIsOmitted(t *testing.T) {
	out := renderWith(t, []Resource{database("main", map[string]value.Value{
		"engine": prov("postgres"),
		"size":   provInt(200),
	})}, map[string]map[string]value.Value{
		"main": {"size": value.Int(200, value.SourceDefault)},
	})
	if strings.Contains(out, "size") {
		t.Errorf("size was written although the instance already defaults it to 200:\n%s", out)
	}
	// The other half: an attribute nothing defaults is still written, or this test
	// would pass against a generator that wrote nothing at all.
	if !strings.Contains(out, "engine") {
		t.Errorf("engine was omitted:\n%s", out)
	}
}

// TestAValueDIFFERINGFromItsInstanceDefaultIsWritten — the boundary. Omitting here would
// generate a file that plans a change the moment it is read back, which is invariant 3.
func TestAValueDifferingFromItsInstanceDefaultIsWritten(t *testing.T) {
	out := renderWith(t, []Resource{database("main", map[string]value.Value{
		"engine": prov("postgres"),
		"size":   provInt(500),
	})}, map[string]map[string]value.Value{
		"main": {"size": value.Int(200, value.SourceDefault)},
	})
	if !strings.Contains(out, "500") {
		t.Errorf("a value differing from the instance default was omitted, so the file plans "+
			"a change as soon as it is read back:\n%s", out)
	}
}

// TestAnotherInstancesDefaultDoesNotOmitAnything is the rule that keeps two accounts
// apart, and the one a map lookup keyed on the attribute name alone would break.
//
// Two instances of one plugin exist precisely because they differ. Omitting a value
// because the OTHER account happens to default it writes a file that is wrong for the
// account it describes.
func TestAnotherInstancesDefaultDoesNotOmitAnything(t *testing.T) {
	out := renderWith(t, []Resource{database("acct2", map[string]value.Value{
		"engine": prov("postgres"),
		"size":   provInt(200),
	})}, map[string]map[string]value.Value{
		"main": {"size": value.Int(200, value.SourceDefault)},
	})
	if !strings.Contains(out, "200") {
		t.Errorf("acct2's resource was trimmed against main's `defaults:`:\n%s", out)
	}
}

// TestAResourceWithNoInstanceIsNotTrimmed. A discovered resource whose instance is
// unknown must keep everything: guessing which block applies is how a value silently
// disappears from a file that then plans a change.
func TestAResourceWithNoInstanceIsNotTrimmed(t *testing.T) {
	out := renderWith(t, []Resource{database("", map[string]value.Value{
		"engine": prov("postgres"),
		"size":   provInt(200),
	})}, map[string]map[string]value.Value{
		"main": {"size": value.Int(200, value.SourceDefault)},
	})
	if !strings.Contains(out, "200") {
		t.Errorf("a resource naming no instance was trimmed against some instance's "+
			"`defaults:`:\n%s", out)
	}
}

// TestExportWritesAValueItsInstanceDefaults. §28 is the opposite of §27: a full record
// for auditing, where a value that happens to match a default today is still a fact
// about the resource. Minimal is what governs the omission, and export does not set it.
func TestExportWritesAValueItsInstanceDefaults(t *testing.T) {
	opts := Options{InstanceDefaults: map[string]map[string]value.Value{
		"main": {"size": value.Int(200, value.SourceDefault)},
	}}
	files, err := Generate([]Resource{database("main", map[string]value.Value{
		"engine": prov("postgres"),
		"size":   provInt(200),
	})}, testRegistry(t), opts)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.Contains(string(files[0].Bytes), "200") {
		t.Errorf("export omitted a value equal to an instance default:\n%s", files[0].Bytes)
	}
}
