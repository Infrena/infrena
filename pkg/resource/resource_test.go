package resource

import (
	"testing"

	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/value"
)

func TestDesiredRejectsUnknownAttributes(t *testing.T) {
	r := ResolvedResource{
		Address: address.Address{Name: "db"},
		Type:    "fake.database",
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
		Type:      "fake.database",
		Attrs:     map[string]value.Value{"engine": value.String("postgres", value.SourceExplicit)},
		Lifecycle: Lifecycle{PreventDestroy: true},
	}
	d, err := r.Desired()
	if err != nil {
		t.Fatalf("Desired(): %v", err)
	}
	if d.Type != "fake.database" || !d.Lifecycle.PreventDestroy {
		t.Errorf("Desired() lost fields: %#v", d)
	}
	if got, _ := d.Attrs["engine"].AsString(); got != "postgres" {
		t.Errorf("engine = %q", got)
	}
}

func TestCloneIsDeep(t *testing.T) {
	s := &ResourceState{
		Address:      address.Address{Name: "db"},
		Type:         "fake.database",
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

// TestCloneIsDeepInsideACompositeAttribute is the half TestCloneIsDeep did not
// cover, and the gap it closes was real: a fresh Attributes map whose VALUES
// still pointed at the original's maps and slices left two states one write
// apart. Writing through a clone into state loaded from disk is the failure
// the whole Clone-before-refresh discipline exists to prevent, so a clone that
// stops at the top level prevents it only for attributes that happen to be
// scalars.
func TestCloneIsDeepInsideACompositeAttribute(t *testing.T) {
	s := &ResourceState{
		Address: address.Address{Name: "db"},
		Type:    "fake.database",
		Attributes: map[string]value.Value{
			"tags": value.Map(map[string]value.Value{
				"env": value.String("production", value.SourceProvider),
			}, value.SourceProvider),
			"zones": value.List([]value.Value{
				value.String("a", value.SourceProvider),
			}, value.SourceProvider),
		},
	}

	c := s.Clone()
	c.Attributes["tags"].Raw.(map[string]value.Value)["env"] = value.String("dev", value.SourceProvider)
	c.Attributes["zones"].Raw.([]value.Value)[0] = value.String("b", value.SourceProvider)

	original := s.Attributes["tags"].Raw.(map[string]value.Value)
	if got, _ := original["env"].AsString(); got != "production" {
		t.Errorf("a write through the clone reached the original's map: env = %q", got)
	}
	if got, _ := s.Attributes["zones"].Raw.([]value.Value)[0].AsString(); got != "a" {
		t.Errorf("a write through the clone reached the original's slice: zones[0] = %q", got)
	}
}

// TestCloneKeepsWhatItCopies. A deep copy that quietly dropped or reshaped a
// nested value would satisfy the aliasing test above perfectly, so the two
// have to be read together.
func TestCloneKeepsWhatItCopies(t *testing.T) {
	nested := value.Map(map[string]value.Value{
		"creds": value.Map(map[string]value.Value{
			"password": value.String("hunter2", value.SourceProvider).WithSensitive(true),
		}, value.SourceProvider),
	}, value.SourceProvider)
	s := &ResourceState{Attributes: map[string]value.Value{"conf": nested}}

	c := s.Clone()
	creds := c.Attributes["conf"].Raw.(map[string]value.Value)["creds"].Raw.(map[string]value.Value)
	pw := creds["password"]
	if got, _ := pw.AsString(); got != "hunter2" {
		t.Errorf("a leaf two levels down did not survive the clone: %q", got)
	}
	if !pw.Sensitive {
		t.Error("the clone dropped a leaf's Sensitive flag, which would print a secret in clear")
	}
}
