package resource

import (
	"testing"

	"infra/pkg/address"
	"infra/pkg/value"
)

func TestDesiredRejectsUnknownAttributes(t *testing.T) {
	r := ResolvedResource{
		Address: address.Address{Name: "db"},
		Type:    "test.database",
		Attrs: map[string]value.Value{
			"engine": value.String("postgres", value.SourceExplicit),
			"url":    value.Unknown(value.KindString, value.SourceComputed),
		},
	}
	if _, err := r.Desired(); err == nil {
		t.Fatal("Desired() must refuse unknown attributes: providers are never called with unresolved values (spec §5.3)")
	}
}

func TestDesiredSucceedsWhenFullyKnown(t *testing.T) {
	r := ResolvedResource{
		Address:   address.Address{Name: "db"},
		Type:      "test.database",
		Attrs:     map[string]value.Value{"engine": value.String("postgres", value.SourceExplicit)},
		Lifecycle: Lifecycle{PreventDestroy: true},
	}
	d, err := r.Desired()
	if err != nil {
		t.Fatalf("Desired(): %v", err)
	}
	if d.Type != "test.database" || !d.Lifecycle.PreventDestroy {
		t.Errorf("Desired() lost fields: %#v", d)
	}
	if got, _ := d.Attrs["engine"].AsString(); got != "postgres" {
		t.Errorf("engine = %q", got)
	}
}

func TestCloneIsDeep(t *testing.T) {
	s := &ResourceState{
		Address:      address.Address{Name: "db"},
		Type:         "test.database",
		Attributes:   map[string]value.Value{"engine": value.String("postgres", value.SourceProvider)},
		Dependencies: []address.Address{{Name: "net"}},
	}
	c := s.Clone()
	c.Attributes["engine"] = value.String("mysql", value.SourceProvider)
	c.Dependencies[0] = address.Address{Name: "other"}

	if got, _ := s.Attributes["engine"].AsString(); got != "postgres" {
		t.Error("Clone shares its attribute map with the original")
	}
	if s.Dependencies[0].Name != "net" {
		t.Error("Clone shares its dependency slice with the original")
	}
}
