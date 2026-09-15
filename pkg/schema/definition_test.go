package schema

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/infrena/infrena/pkg/value"
)

func sampleDefinition() *ResourceDefinition {
	return &ResourceDefinition{
		Type:        "fake.database",
		Description: "A fake database",
		Attributes: map[string]Attribute{
			"engine":   {Kind: value.KindString, Required: true, ForceNew: true},
			"size":     {Kind: value.KindInt, Default: int64(10)},
			"password": {Kind: value.KindString, Sensitive: true},
			"endpoint": {Kind: value.KindString, Computed: true},
		},
		Requirements: []Requirement{{Name: "network", Types: []string{"fake.network"}}},
		Capabilities: Capabilities{Create: true, Read: true, Update: true, Delete: true},
	}
}

func TestAttributeLookup(t *testing.T) {
	d := sampleDefinition()
	if _, ok := d.Attribute("engine"); !ok {
		t.Error("engine should be found")
	}
	if _, ok := d.Attribute("nope"); ok {
		t.Error("unknown attribute should not be found")
	}
}

func TestRequiredAndForceNewAreSortedForDeterminism(t *testing.T) {
	d := sampleDefinition()
	d.Attributes["alpha"] = Attribute{Kind: value.KindString, Required: true, ForceNew: true}

	req := d.RequiredAttributes()
	if len(req) != 2 || req[0] != "alpha" || req[1] != "engine" {
		t.Errorf("RequiredAttributes() = %v, want sorted [alpha engine]", req)
	}
	fn := d.ForceNewAttributes()
	if len(fn) != 2 || fn[0] != "alpha" {
		t.Errorf("ForceNewAttributes() = %v, want sorted", fn)
	}
}

// TestASchemaHoldsNoFunctions is §31.1's requirement on this package, and the
// reason the three function fields are gone: a provider is a separate process, so
// a schema has to survive a pipe.
//
// Asserted through encoding/json rather than by reading the struct definition,
// because that is the property that matters and the only one a future field
// cannot quietly break. json.Marshal fails outright on a func value, so a field
// holding one — or a field whose value holds one — fails here.
func TestASchemaHoldsNoFunctions(t *testing.T) {
	d := sampleDefinition()
	if _, err := json.Marshal(d); err != nil {
		t.Fatalf("a schema must survive a pipe, and this one cannot be encoded: %v\n"+
			"A function field cannot cross a process boundary; see PLAN.md §31.1.", err)
	}
	// And a default, which is the field that used to hold one, is a plain datum
	// convertible against its declared kind.
	attr, _ := d.Attribute("size")
	v, ok := DatumValue(attr.Default, attr.Kind)
	if !ok {
		t.Fatalf("the default %v (%T) is not a %s", attr.Default, attr.Default, attr.Kind)
	}
	if got, _ := v.AsInt(); got != 10 {
		t.Errorf("default = %v, want 10", v)
	}
}

// TestADefaultOfTheWrongKindIsRejected. Nothing checks a datum at authoring time —
// `Default: "10"` on an integer attribute compiles — so the conversion is what
// catches it, and the compiler reports it as a provider bug.
func TestADefaultOfTheWrongKindIsRejected(t *testing.T) {
	if _, ok := DatumValue("10", value.KindInt); ok {
		t.Error("a string default for an integer attribute was accepted")
	}
	if _, ok := DatumValue(nil, value.KindInt); ok {
		t.Error("a nil default was accepted; nil means no default at all")
	}
}

func TestValidateRejectsComputedRequired(t *testing.T) {
	d := sampleDefinition()
	d.Attributes["broken"] = Attribute{Kind: value.KindString, Required: true, Computed: true}
	if err := d.Validate(); err == nil {
		t.Error("an attribute cannot be both Required and Computed — the user may not set it")
	}
}

func TestValidateRejectsComputedWithDefault(t *testing.T) {
	d := sampleDefinition()
	d.Attributes["broken"] = Attribute{
		Kind:     value.KindString,
		Computed: true,
		Default:  "x",
	}
	if err := d.Validate(); err == nil {
		t.Error("a computed attribute cannot carry a default — the provider supplies it")
	}
}

func TestValidateRejectsMissingKindOrType(t *testing.T) {
	d := sampleDefinition()
	d.Attributes["broken"] = Attribute{}
	if err := d.Validate(); err == nil {
		t.Error("an attribute with no Kind must be rejected")
	}

	empty := &ResourceDefinition{Attributes: map[string]Attribute{}}
	if err := empty.Validate(); err == nil {
		t.Error("a definition with no Type must be rejected")
	}
}

func TestValidateRejectsRequirementWithNoTypes(t *testing.T) {
	d := sampleDefinition()
	d.Requirements = append(d.Requirements, Requirement{Name: "cluster"})
	if err := d.Validate(); err == nil {
		t.Error("a requirement that names no satisfying types can never be satisfied")
	}
}

func TestAReferenceToAnUndeclaredAttributeRefusesTheDefinition(t *testing.T) {
	// A plugin whose relationship names an attribute the target does not have
	// is a plugin that will not load, on §14.1's precedent that a name
	// collision should fail at load rather than surprise someone at apply.
	vpc := &ResourceDefinition{
		Type: "test.vpc",
		Attributes: map[string]Attribute{
			"id": {Kind: value.KindString, Computed: true},
		},
	}
	subnet := &ResourceDefinition{
		Type: "test.subnet",
		Attributes: map[string]Attribute{
			"vpc_id": {Kind: value.KindString, Required: true,
				References: &Reference{Type: "test.vpc", Attribute: "arn"}},
		},
	}
	err := ValidateAll([]*ResourceDefinition{vpc, subnet})
	if err == nil {
		t.Fatal("a reference to an attribute the target does not declare must refuse the definition")
	}
	if !strings.Contains(err.Error(), "arn") || !strings.Contains(err.Error(), "test.vpc") {
		t.Errorf("error = %q, want it to name both the attribute and the target type", err)
	}
}

func TestAReferenceToAnUndeclaredTypeRefusesTheDefinition(t *testing.T) {
	subnet := &ResourceDefinition{
		Type: "test.subnet",
		Attributes: map[string]Attribute{
			"vpc_id": {Kind: value.KindString, Required: true,
				References: &Reference{Type: "test.nosuch", Attribute: "id"}},
		},
	}
	if err := ValidateAll([]*ResourceDefinition{subnet}); err == nil {
		t.Fatal("a reference to a type the plugin does not declare must refuse the definition")
	}
}

func TestAWellFormedReferenceLoads(t *testing.T) {
	vpc := &ResourceDefinition{
		Type: "test.vpc",
		Attributes: map[string]Attribute{"id": {Kind: value.KindString, Computed: true}},
	}
	subnet := &ResourceDefinition{
		Type: "test.subnet",
		Attributes: map[string]Attribute{
			"vpc_id": {Kind: value.KindString, Required: true,
				References: &Reference{Type: "test.vpc", Attribute: "id"}},
		},
	}
	if err := ValidateAll([]*ResourceDefinition{vpc, subnet}); err != nil {
		t.Fatalf("a well-formed reference must load: %v", err)
	}
}

// TestReferencesAndFieldsSurviveTheWire. Same shape as
// TestAliasesSurviveTheWire, for the two fields protocol 3 added (PLAN.md
// §14.3): a plugin declares them, and the host learns them only if
// attributeWire actually carries them across. Spec one's own testing section
// records that exactly this shape of bug — a schema field that round-trips in
// memory but is silently dropped by MarshalJSON — reached final review once
// already, because the in-memory tests never crossed the wire.
func TestReferencesAndFieldsSurviveTheWire(t *testing.T) {
	before := Attribute{
		Kind:       value.KindString,
		References: &Reference{Type: "test.vpc", Attribute: "id"},
		Fields:     map[string]Attribute{"name": {Kind: value.KindString}},
	}
	data, err := before.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var after Attribute
	if err := after.UnmarshalJSON(data); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if after.References == nil || *after.References != *before.References {
		t.Errorf("References = %+v, want %+v — a plugin's declared relationship must not be "+
			"dropped silently, or `${vpc}` would report \"no reference target declared\" against "+
			"a plugin that clearly declares one", after.References, before.References)
	}
	if len(after.Fields) != 1 || after.Fields["name"].Kind != value.KindString {
		t.Errorf("Fields = %+v, want {name: string} — a declared map shape must not be dropped, "+
			"or a typo past the top-level key would go back to being caught only at apply", after.Fields)
	}
}
