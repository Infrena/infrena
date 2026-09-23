package pluginhost

import (
	"reflect"
	"testing"
	"time"

	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/pluginproto"
	"github.com/infrena/infrena/pkg/resource"
	"github.com/infrena/infrena/pkg/schema"
	"github.com/infrena/infrena/pkg/value"
)

// rebuildFixture is a definition and a carry state with every field of
// ResourceState set to something recognisable, so a field that is dropped shows
// up as a zero value rather than as a plausible one.
func rebuildFixture() (*remoteProvider, *resource.ResourceState) {
	def := &schema.ResourceDefinition{
		Type: "fake.thing",
		Attributes: map[string]schema.Attribute{
			"size": {Kind: value.KindInt, Optional: true, Computed: true},
		},
	}
	p := &Plugin{byType: map[string]*schema.ResourceDefinition{"fake.thing": def}}
	r := &remoteProvider{plugin: p, handle: "h", instance: "main"}

	carry := &resource.ResourceState{
		Address:      address.Address{Name: "db"},
		Type:         "fake.thing",
		Provider:     "main",
		ProviderID:   "db-1",
		Attributes:   map[string]value.Value{"size": value.Int(1, value.SourceProvider)},
		Dependencies: []address.Address{{Name: "net"}},
		Lifecycle:    resource.Lifecycle{PreventDestroy: true},
		CreatedAt:    time.Unix(100, 0).UTC(),
		UpdatedAt:    time.Unix(200, 0).UTC(),
		Deposed: []*resource.ResourceState{{
			Address:    address.Address{Name: "db"},
			Type:       "fake.thing",
			ProviderID: "db-0",
		}},
	}
	return r, carry
}

// TestRebuildCarriesDeposed pins the specific loss: a deposed object is the only
// record that a create_before_destroy replacement left something real behind, and
// its ProviderID is the only handle anything has on that object. Dropping it here
// makes the leak permanent, because the plan that would propose the cleanup is
// itself gated on the field being present.
func TestRebuildCarriesDeposed(t *testing.T) {
	r, carry := rebuildFixture()

	out, err := r.rebuild("fake.thing", pluginproto.ResourceResult{
		ProviderID: "db-1",
		Attributes: map[string]value.Value{"size": value.Int(2, value.SourceProvider)},
	}, carry)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	if len(out.Deposed) != 1 {
		t.Fatalf("Deposed = %v, want the one entry the carry held: a read must not lose "+
			"the record of an object a failed replacement left behind", out.Deposed)
	}
	if got := out.Deposed[0].ProviderID; got != "db-0" {
		t.Errorf("deposed ProviderID = %q, want %q", got, "db-0")
	}
}

// TestRebuildDoesNotShareDeposedWithTheCarry. ResourceState.Clone deep-copies
// Deposed rather than sharing it, and for the same reason: two states holding one
// slice means a later append or edit through either reaches the other. rebuild's
// result is written to state while the carry is still live in the caller.
func TestRebuildDoesNotShareDeposedWithTheCarry(t *testing.T) {
	r, carry := rebuildFixture()

	out, err := r.rebuild("fake.thing", pluginproto.ResourceResult{
		ProviderID: "db-1",
		Attributes: map[string]value.Value{"size": value.Int(2, value.SourceProvider)},
	}, carry)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	carry.Deposed[0].ProviderID = "mutated-through-the-carry"
	if out.Deposed[0].ProviderID != "db-0" {
		t.Error("the rebuilt state shares its Deposed entries with the carry, so a change to " +
			"one is a change to the other")
	}
}

// TestEveryResourceStateFieldIsTransmittedOrCarried closes the class rather than
// the instance.
//
// A plugin is sent only what it owns, so everything else has to come from the
// state the host already held. That rule is easy to state and easy to forget: a
// field added to ResourceState is silently dropped at this boundary until someone
// remembers to list it, and the symptom appears somewhere else entirely.
//
// This walks ResourceState by reflection, so a new field fails here until it is
// either put on the wire or carried. Deposed is in the list because it was the one
// that got missed.
func TestEveryResourceStateFieldIsTransmittedOrCarried(t *testing.T) {
	// Fields the plugin itself reports, which rebuild therefore takes from the
	// result rather than from the carry.
	transmitted := map[string]string{
		"Type":       "the host passes it in and rebuild echoes it",
		"ProviderID": "the plugin reports what it created or read",
		"Attributes": "the plugin's whole answer",
	}
	// Bookkeeping the plugin is never sent, and so cannot have lost.
	carried := map[string]string{
		"Address":      "which resource this is",
		"Provider":     "which provider instance owns it",
		"Dependencies": "destroy-ordering edges once a resource leaves configuration",
		"Lifecycle":    "prevent_destroy and retain",
		"CreatedAt":    "",
		"UpdatedAt":    "",
		"Deposed":      "objects a failed create_before_destroy left behind",
	}

	r, carry := rebuildFixture()
	out, err := r.rebuild("fake.thing", pluginproto.ResourceResult{
		ProviderID: "db-1",
		Attributes: map[string]value.Value{"size": value.Int(2, value.SourceProvider)},
	}, carry)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	rt := reflect.TypeOf(resource.ResourceState{})
	rebuilt := reflect.ValueOf(*out)
	for i := range rt.NumField() {
		name := rt.Field(i).Name
		_, isTransmitted := transmitted[name]
		_, isCarried := carried[name]

		if !isTransmitted && !isCarried {
			t.Errorf("ResourceState.%s is neither transmitted nor carried, so rebuild drops it. "+
				"Decide which it is: put it on the wire in pkg/pluginproto, or carry it in rebuild, "+
				"then add it to this test's list.", name)
			continue
		}
		if isCarried && rebuilt.Field(i).IsZero() {
			t.Errorf("ResourceState.%s is listed as carried, but rebuild returned it zero. "+
				"The fixture sets every field, so this means the carry does not reach the result.", name)
		}
	}
}
