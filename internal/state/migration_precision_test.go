package state

import (
	"encoding/json"
	"strings"
	"testing"
)

// The MIGRATION path's fidelity, which the current-version path has had since M1
// (TestDecodePreservesLargeIntegers) and this one never did.
//
// It was untested because it is unreachable: CurrentVersion is 1, nothing registers a
// migration, so the generic map[string]any intermediate is dead code in production. That
// is exactly why it matters — M7 and M11 both declined to bump the state version, each
// citing this defect, so the first real format change is the run where a silent rounding
// lands on every file at once.

// atVersionZero is a state document one version behind, so Decode routes it through the
// migration path.
const atVersionZero = `{
  "version": 0,
  "project": "myapp",
  "environment": "dev",
  "serial": 3,
  "resources": {
    "db": {
      "address": {"name": "db"},
      "type": "test.database",
      "provider": "test",
      "provider_id": "db-1",
      "attributes": {
        "big": {"kind": "integer", "known": true, "raw": 9007199254740993, "source": "provider"},
        "huge": {"kind": "integer", "known": true, "raw": -9007199254740993, "source": "provider"},
        "fraction": {"kind": "float", "known": true, "raw": 0.1, "source": "provider"},
        "region": {"kind": "string", "known": true, "raw": "eu-west-1", "source": "provider"}
      },
      "lifecycle": {"prevent_destroy": false, "retain": false}
    }
  },
  "updated_at": "2026-09-12T00:00:00Z"
}`

// withNoOpMigration registers a migration that changes nothing, so the only thing under
// test is what the migration PATH does to the document on its way through.
func withNoOpMigration(t *testing.T) {
	t.Helper()
	original := migrations
	t.Cleanup(func() { migrations = original })
	migrations = nil
	// To CurrentVersion, not to 1: this test is about what the migration PATH does to a
	// document, not about any particular version, and pinning 1 broke it the first time
	// CurrentVersion moved.
	RegisterMigration(Migration{From: 0, To: CurrentVersion, Apply: func(map[string]any) error { return nil }})
}

// TestMigrationPreservesLargeIntegers. 2^53+1 is the smallest positive integer float64
// cannot represent, so a document re-marshalled from map[string]any comes back one less.
//
// State's entire job is fidelity. An attribute that is silently off by one is worse than
// a load that fails: a plan then proposes an update to correct a value the user never
// changed, and applying it writes the rounded number back as though it were desired.
func TestMigrationPreservesLargeIntegers(t *testing.T) {
	withNoOpMigration(t)

	got, err := Decode([]byte(atVersionZero))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	for name, want := range map[string]int64{
		"big":  9007199254740993,
		"huge": -9007199254740993, // the negative side rounds too
	} {
		back, ok := got.Resources["db"].Attributes[name].AsInt()
		if !ok {
			t.Errorf("%s did not decode as an integer: %#v", name, got.Resources["db"].Attributes[name])
			continue
		}
		if back != want {
			t.Errorf("%s = %d, want %d — the migration path rounded it through float64",
				name, back, want)
		}
	}
}

// TestMigrationPreservesEverythingElseItPassesThrough is the boundary. A fix that made
// integers exact by turning every number into something else would break floats and
// strings instead.
func TestMigrationPreservesEverythingElseItPassesThrough(t *testing.T) {
	withNoOpMigration(t)

	got, err := Decode([]byte(atVersionZero))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	attrs := got.Resources["db"].Attributes
	if f, ok := attrs["fraction"].AsFloat(); !ok || f != 0.1 {
		t.Errorf("fraction = %#v, want 0.1", attrs["fraction"])
	}
	if s, ok := attrs["region"].AsString(); !ok || s != "eu-west-1" {
		t.Errorf("region = %#v, want eu-west-1", attrs["region"])
	}
	if got.Serial != 3 {
		t.Errorf("Serial = %d, want 3", got.Serial)
	}
	if got.Project != "myapp" || got.Environment != "dev" {
		t.Errorf("project/environment lost: %q/%q", got.Project, got.Environment)
	}
}

// TestAMigrationSeesNumbersAsJSONNumber pins the contract a migration author depends on.
//
// The fix changes what `raw` holds: a number arrives as json.Number rather than float64,
// so `raw["serial"].(float64)` in a migration would stop compiling its way to a correct
// answer and start failing its type assertion. There are no migrations today, which is
// the only reason this is a free change — and it is documented on Migration.Apply so the
// first author is not surprised.
func TestAMigrationSeesNumbersAsJSONNumber(t *testing.T) {
	original := migrations
	t.Cleanup(func() { migrations = original })

	var seen any
	migrations = nil
	RegisterMigration(Migration{From: 0, To: CurrentVersion, Apply: func(raw map[string]any) error {
		seen = raw["serial"]
		return nil
	}})

	if _, err := Decode([]byte(atVersionZero)); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	n, ok := seen.(json.Number)
	if !ok {
		t.Fatalf("a migration saw serial as %T, want json.Number", seen)
	}
	if n.String() != "3" {
		t.Errorf("serial = %q, want \"3\"", n)
	}
}

// TestAMigrationMaySetANumberTheOrdinaryWay. A json.Number intermediate must not make
// WRITING a value awkward: an int or a float assigned by a migration has to survive the
// re-marshal, or every future migration carries a conversion nobody would remember.
func TestAMigrationMaySetANumberTheOrdinaryWay(t *testing.T) {
	original := migrations
	t.Cleanup(func() { migrations = original })

	migrations = nil
	RegisterMigration(Migration{From: 0, To: CurrentVersion, Apply: func(raw map[string]any) error {
		raw["serial"] = 42
		return nil
	}})

	got, err := Decode([]byte(atVersionZero))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Serial != 42 {
		t.Errorf("Serial = %d, want 42 — a plain int assigned by a migration was lost", got.Serial)
	}
}

// TestMalformedJSONOnTheMigrationPathStillFails. UseNumber defers number parsing, so a
// document whose number is not a number must still be refused rather than reaching the
// typed struct and failing there with a worse message.
func TestMalformedJSONOnTheMigrationPathStillFails(t *testing.T) {
	withNoOpMigration(t)

	_, err := Decode([]byte(`{"version": 0, "serial": nope}`))
	if err == nil {
		t.Fatal("malformed JSON must be refused")
	}
	if !strings.Contains(err.Error(), "JSON") {
		t.Errorf("the error does not say the file is not valid JSON: %v", err)
	}
}
