package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infrata/infrata/pkg/address"
	"github.com/infrata/infrata/pkg/value"
)

// Version 1 → 2: the fake provider's rename from `test` to `fake`. PLAN.md §21.1.
//
// THE FIRST MIGRATION EVER TO RUN. The mechanism has existed since M1 and nothing has
// used it, which is why §21.1 argued for exercising it here rather than first on AWS.
//
// The fixture is testdata/state-v1.json — the FROZEN GOLDEN a version-1 infrata actually
// wrote, kept when the golden moved to v2 rather than regenerated away. Real historical
// bytes beat a hand-written approximation of them, and this one happens to carry a
// 2^53+1 integer, so it also proves the non-lossy migration decode on a real file.

func migrationFixture(t *testing.T) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "state-v1.json"))
	if err != nil {
		t.Fatalf("reading the version-1 fixture: %v", err)
	}
	// The fixture must actually be version 1 and actually hold test.* types, or this
	// whole file asserts nothing.
	if !strings.Contains(string(body), `"version": 1`) {
		t.Fatalf("testdata/state-v1.json is not version 1; this test needs a real v1 file")
	}
	if !strings.Contains(string(body), "test.") {
		t.Fatalf("testdata/state-v1.json holds no test.* type, so the migration has nothing to do")
	}
	return body
}

// TestTheFakeRenameMigrationRewritesTypesAndTheImplicitInstance.
func TestTheFakeRenameMigrationRewritesTypesAndTheImplicitInstance(t *testing.T) {
	got, err := Decode(migrationFixture(t))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Version != CurrentVersion {
		t.Errorf("Version = %d, want %d", got.Version, CurrentVersion)
	}

	db, ok := got.Get(address.Address{Name: "db"})
	if !ok {
		t.Fatalf("db is missing after migration; addresses = %v", got.Addresses())
	}
	if db.Type != "fake.database" {
		t.Errorf("Type = %q, want fake.database", db.Type)
	}
	// The INSTANCE name too. The implicit instance is named after the plugin, so a
	// state file written when the plugin was `test` records `test` — and nothing would
	// dispatch it after the rename.
	if db.Provider != "fake" {
		t.Errorf("Provider = %q, want fake", db.Provider)
	}
}

// TestTheMigrationPreservesEverythingItIsNotRewriting. A migration that fixed the type
// and lost an attribute would be worse than no migration: the plan after it would propose
// changes nobody made.
func TestTheMigrationPreservesEverythingItIsNotRewriting(t *testing.T) {
	got, err := Decode(migrationFixture(t))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	db, _ := got.Get(address.Address{Name: "db"})

	// The 2^53+1 integer, which is why this fixture is the right one: the migration path
	// used to round it, and nothing else in the suite reads a real file through it.
	if n, ok := db.Attributes["size"].AsInt(); !ok || n != 1<<53+1 {
		t.Errorf("size = %v, want %d — the migration path rounded a real state file",
			db.Attributes["size"], int64(1)<<53+1)
	}
	if !db.Attributes["password"].Sensitive {
		t.Error("the sensitive flag was lost")
	}
	if db.Attributes["tags"].Kind != value.KindMap {
		t.Errorf("the composite attribute did not survive: %#v", db.Attributes["tags"])
	}
	if db.Attributes["endpoint"].Known {
		t.Error("an unknown value came back known")
	}
	if db.ProviderID != "db-1" {
		t.Errorf("ProviderID = %q", db.ProviderID)
	}
	if db.CreatedAt.IsZero() {
		t.Error("CreatedAt was lost")
	}
	if got.Project != "myapp" || got.Environment != "dev" || got.Serial != 7 {
		t.Errorf("project/environment/serial lost: %q/%q/%d", got.Project, got.Environment, got.Serial)
	}
	// Dependencies are the only source of destroy ordering once a resource leaves
	// configuration, so losing them is silent corruption.
	if len(got.Resources) < 2 {
		t.Errorf("resources lost: %v", got.Addresses())
	}
}

// TestAnInstanceTheUserNAMEDIsLeftAlone is the boundary, and the half a careless
// migration gets wrong.
//
// Only the IMPLICIT instance is called `test` — it takes the plugin's name. A user who
// wrote `providers: [{plugin: test, name: main}]` has state recording `main`, and after
// the rename their configuration says `plugin: fake` while the instance is still `main`.
// Rewriting every provider name to `fake` would break exactly those projects.
func TestAnInstanceTheUserNamedIsLeftAlone(t *testing.T) {
	got, err := Decode([]byte(`{
  "version": 1,
  "serial": 1,
  "project": "myapp",
  "environment": "dev",
  "resources": {
    "a": {"address": {"name": "a"}, "type": "test.network", "provider": "main",
          "provider_id": "net-1", "attributes": {}, "lifecycle": {}},
    "b": {"address": {"name": "b"}, "type": "test.network", "provider": "test",
          "provider_id": "net-2", "attributes": {}, "lifecycle": {}}
  },
  "updated_at": "2026-09-13T00:00:00Z"
}`))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	a, _ := got.Get(address.Address{Name: "a"})
	b, _ := got.Get(address.Address{Name: "b"})

	if a.Provider != "main" {
		t.Errorf("a named instance was rewritten to %q; only the implicit `test` moves", a.Provider)
	}
	if b.Provider != "fake" {
		t.Errorf("the implicit instance = %q, want fake", b.Provider)
	}
	// Both types move regardless of instance name: the TYPES belong to the plugin.
	for _, r := range []string{a.Type, b.Type} {
		if r != "fake.network" {
			t.Errorf("type = %q, want fake.network", r)
		}
	}
}

// TestAStateFileAlreadyAtTheCurrentVersionIsNotRewritten. A file written after the rename
// holds `fake.*` already, and touching it would be a migration running twice.
func TestAStateFileAlreadyAtTheCurrentVersionIsNotRewritten(t *testing.T) {
	s := New("myapp", "dev")
	r := sampleResource("db")
	r.Type = "fake.database"
	r.Provider = "fake"
	s.Set(r)
	encoded, err := s.Encode()
	if err != nil {
		t.Fatal(err)
	}

	got, err := Decode(encoded)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	db, _ := got.Get(address.Address{Name: "db"})
	if db.Type != "fake.database" || db.Provider != "fake" {
		t.Errorf("a current-version file was altered: %q / %q", db.Type, db.Provider)
	}
}
