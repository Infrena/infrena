package state

import (
	"encoding/json"
	"testing"

	"infra/pkg/address"
	"infra/pkg/resource"
	"infra/pkg/value"
)

func sampleResource(name string) *resource.ResourceState {
	return &resource.ResourceState{
		Address:    address.Address{Name: name},
		Type:       "test.database",
		Provider:   "test",
		ProviderID: "db-1",
		Attributes: map[string]value.Value{
			"engine": value.String("postgres", value.SourceProvider),
			"size":   value.Int(10, value.SourceProvider),
		},
	}
}

func TestSetGetRemove(t *testing.T) {
	s := New("myapp", "dev")
	addr := address.Address{Name: "db"}

	if _, ok := s.Get(addr); ok {
		t.Error("empty state must not report a resource")
	}
	s.Set(sampleResource("db"))
	if _, ok := s.Get(addr); !ok {
		t.Error("Set then Get failed")
	}
	s.Remove(addr)
	if _, ok := s.Get(addr); ok {
		t.Error("Remove did not remove")
	}
}

func TestAddressesAreSorted(t *testing.T) {
	s := New("myapp", "dev")
	for _, n := range []string{"zebra", "alpha", "middle"} {
		s.Set(sampleResource(n))
	}
	got := s.Addresses()
	want := []string{"alpha", "middle", "zebra"}
	for i := range want {
		if got[i].String() != want[i] {
			t.Fatalf("Addresses() = %v, want %v", got, want)
		}
	}
}

func TestEncodeDecodePreservesTypedValues(t *testing.T) {
	s := New("myapp", "dev")
	s.Set(sampleResource("db"))

	data, err := s.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	r, ok := got.Get(address.Address{Name: "db"})
	if !ok {
		t.Fatal("resource missing after round trip")
	}
	if n, ok := r.Attributes["size"].AsInt(); !ok || n != 10 {
		t.Errorf("size = %d, %v; want 10, true — an integer must not decay to a float", n, ok)
	}
}

func TestDecodeRejectsFutureVersion(t *testing.T) {
	data := []byte(`{"version": 9999, "serial": 1, "resources": {}}`)
	if _, err := Decode(data); err == nil {
		t.Error("state written by a newer infra must be refused, not silently misread")
	}
}

func TestDecodeRunsMigrations(t *testing.T) {
	original := migrations
	t.Cleanup(func() { migrations = original })

	migrations = nil
	RegisterMigration(Migration{From: 0, To: 1, Apply: func(raw map[string]any) error {
		raw["project"] = "migrated"
		return nil
	}})

	got, err := Decode([]byte(`{"version": 0, "serial": 3, "resources": {}}`))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Project != "migrated" {
		t.Errorf("Project = %q, want \"migrated\" — the migration did not run", got.Project)
	}
	if got.Version != CurrentVersion {
		t.Errorf("Version = %d, want %d", got.Version, CurrentVersion)
	}
}

func TestEncodeIsStableAcrossRuns(t *testing.T) {
	s := New("myapp", "dev")
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		s.Set(sampleResource(n))
	}
	first, err := s.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	for i := 0; i < 20; i++ {
		next, err := s.Encode()
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		if string(first) != string(next) {
			t.Fatal("state encoding must be byte-stable; Go map ordering is randomised and a churning state file makes diffs useless")
		}
	}
	var check map[string]any
	if err := json.Unmarshal(first, &check); err != nil {
		t.Fatalf("encoded state is not valid JSON: %v", err)
	}
}
