package state

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"infra/pkg/address"
	"infra/pkg/resource"
	"infra/pkg/value"
)

// updateGolden regenerates testdata/state-v1.json. Run with
// `go test ./internal/state -run TestStateFileFormatIsFrozen -update` after a
// deliberate, version-bumped format change — never to make a red test go green.
var updateGolden = flag.Bool("update", false, "rewrite the golden state file from the current encoder")

// goldenState builds a state exercising every shape the format has to carry:
// a module-qualified dependency, a composite attribute, a sensitive one, an
// integer beyond float64's exact range, an unknown value, populated and zero
// timestamps, and a resource with no dependencies at all.
func goldenState() *State {
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	updated := time.Date(2026, 1, 2, 3, 4, 6, 0, time.UTC)

	s := New("myapp", "dev")
	s.Serial = 7
	s.UpdatedAt = time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

	s.Set(&resource.ResourceState{
		Address:    address.Address{Name: "db"},
		Type:       "test.database",
		Provider:   "test",
		ProviderID: "db-1",
		Attributes: map[string]value.Value{
			"engine":   value.String("postgres", value.SourceProvider),
			"size":     value.Int(1<<53+1, value.SourceProvider),
			"password": value.String("hunter2", value.SourceProvider).WithSensitive(true),
			"endpoint": value.Unknown(value.KindString, value.SourceComputed),
			"tags": value.Map(map[string]value.Value{
				"env": value.String("dev", value.SourceProvider),
			}, value.SourceProvider),
		},
		Dependencies: []address.Address{{Module: []string{"net"}, Name: "vpc"}},
		Lifecycle:    resource.Lifecycle{PreventDestroy: true},
		CreatedAt:    created,
		UpdatedAt:    updated,
	})

	s.Set(&resource.ResourceState{
		Address:    address.Address{Name: "orphan"},
		Type:       "test.network",
		Provider:   "test",
		ProviderID: "net-9",
		Attributes: map[string]value.Value{},
	})

	return s
}

// TestStateFileFormatIsFrozen compares the encoded state byte for byte against
// a checked-in file.
//
// TestEncodeIsStableAcrossRuns proves map ordering is deterministic, but it
// compares encodings to each other, so it cannot see the on-disk shape change.
// The format is a versioned contract from the first release; renaming a Go
// field must break this test rather than silently rewrite everyone's state.
func TestStateFileFormatIsFrozen(t *testing.T) {
	golden := filepath.Join("testdata", "state-v1.json")

	got, err := goldenState().Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("updated %s", golden)
		return
	}

	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("state file format changed.\n--- want (%s) ---\n%s\n--- got ---\n%s", golden, want, got)
	}
}

// TestGoldenStateDecodes guards the other direction: the frozen bytes must load
// back into the state that produced them, so the golden file cannot be updated
// to something the decoder cannot read.
func TestGoldenStateDecodes(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "state-v1.json"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	s, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	db, ok := s.Get(address.Address{Name: "db"})
	if !ok {
		t.Fatal("golden state did not decode a resource named db")
	}
	if got, ok := db.Attributes["size"].AsInt(); !ok || got != 1<<53+1 {
		t.Errorf("size = %d, %v; want %d, true", got, ok, int64(1)<<53+1)
	}
	if len(db.Dependencies) != 1 || db.Dependencies[0].String() != "module.net.vpc" {
		t.Errorf("dependencies = %v, want [module.net.vpc]", db.Dependencies)
	}
	if !db.Lifecycle.PreventDestroy {
		t.Error("prevent_destroy did not survive the golden round trip")
	}
	if !db.Attributes["password"].Sensitive {
		t.Error("sensitivity did not survive the golden round trip")
	}
	if db.Attributes["endpoint"].Known {
		t.Error("an unknown value came back known")
	}

	orphan, ok := s.Get(address.Address{Name: "orphan"})
	if !ok {
		t.Fatal("golden state did not decode a resource named orphan")
	}
	if !orphan.CreatedAt.IsZero() || len(orphan.Dependencies) != 0 {
		t.Errorf("omitted fields came back populated: %+v", orphan)
	}
}
