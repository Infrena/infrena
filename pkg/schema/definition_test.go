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

// TestASchemaHoldsNoFunctions pins that a schema holds no function fields: a
// provider is a separate process, so a schema has to survive a pipe.
//
// Asserted through encoding/json rather than by reading the struct definition,
// because that is the property that matters and the only one a future field
// cannot quietly break. json.Marshal fails outright on a func value, so a field
// holding one — or a field whose value holds one — fails here.
func TestASchemaHoldsNoFunctions(t *testing.T) {
	d := sampleDefinition()
	if _, err := json.Marshal(d); err != nil {
		t.Fatalf("a schema must survive a pipe, and this one cannot be encoded: %v\n"+
			"A function field cannot cross a process boundary.", err)
	}
	// And a default is a plain datum, convertible against its declared kind.
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
	// is a plugin that will not load: it fails at load rather than surprising
	// someone at apply.
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
		Type:       "test.vpc",
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
// TestAliasesSurviveTheWire, for References and Fields: a plugin declares
// them, and the host learns them only if attributeWire actually carries them
// across. A schema field that round-trips in memory but is dropped silently by
// MarshalJSON is an easy defect to ship, because in-memory tests never cross
// the wire.
//
// The attribute nested inside Fields carries its own References and Sensitive,
// not just a bare Kind, because Fields recurses through Attribute's own
// MarshalJSON/UnmarshalJSON: a shallow probe would prove only that the outer
// struct's fields are wired up, not that the recursion carries everything a
// nested attribute can declare.
func TestReferencesAndFieldsSurviveTheWire(t *testing.T) {
	before := Attribute{
		Kind:       value.KindString,
		References: &Reference{Type: "test.vpc", Attribute: "id"},
		Fields: map[string]Attribute{
			"name": {
				Kind:       value.KindString,
				Sensitive:  true,
				References: &Reference{Type: "test.role", Attribute: "arn"},
			},
		},
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
	name, ok := after.Fields["name"]
	if !ok || name.Kind != value.KindString {
		t.Fatalf("Fields = %+v, want a %q entry of kind string", after.Fields, "name")
	}
	if !name.Sensitive {
		t.Error("a Sensitive attribute nested inside Fields must not lose that flag on the wire — " +
			"the redaction guarantee depends on it surviving as far as the top-level case does")
	}
	if name.References == nil || *name.References != *before.Fields["name"].References {
		t.Errorf("a nested attribute's own References must survive too: got %+v, want %+v",
			name.References, before.Fields["name"].References)
	}
}

// TestValidateRejectsFieldsOnANonMapAttribute. Fields describes a map's known
// keys, so declaring it on an attribute of any other Kind describes a shape
// that cannot exist.
func TestValidateRejectsFieldsOnANonMapAttribute(t *testing.T) {
	d := sampleDefinition()
	d.Attributes["broken"] = Attribute{
		Kind:   value.KindString,
		Fields: map[string]Attribute{"name": {Kind: value.KindString}},
	}
	err := d.Validate()
	if err == nil {
		t.Fatal("Fields on a non-map attribute must be rejected")
	}
	if !strings.Contains(err.Error(), "broken") {
		t.Errorf("error = %q, want it to name the attribute", err)
	}
}

// TestValidateRejectsANestedFieldsOnANonMapAttribute is the same check one
// level down: Fields can nest, and the rule must hold at every level, not
// only the top one.
func TestValidateRejectsANestedFieldsOnANonMapAttribute(t *testing.T) {
	d := sampleDefinition()
	d.Attributes["broken"] = Attribute{
		Kind: value.KindMap,
		Fields: map[string]Attribute{
			"leaf": {Kind: value.KindString, Fields: map[string]Attribute{"x": {Kind: value.KindString}}},
		},
	}
	if err := d.Validate(); err == nil {
		t.Fatal("Fields on a non-map attribute nested inside another Fields must be rejected too")
	}
}

// TestValidateRejectsANestedReferences pins that References inside Fields is
// refused. projectRefs (internal/compiler/bind.go) only ever reads a top-level
// attribute's References, so one declared inside Fields is a declaration
// nothing consults, and refusing it at load time is simpler than teaching
// every consumer of References to recurse.
func TestValidateRejectsANestedReferences(t *testing.T) {
	d := sampleDefinition()
	d.Attributes["broken"] = Attribute{
		Kind: value.KindMap,
		Fields: map[string]Attribute{
			"vpc_id": {Kind: value.KindString, References: &Reference{Type: "fake.network", Attribute: "id"}},
		},
	}
	err := d.Validate()
	if err == nil {
		t.Fatal("a nested attribute's References must be rejected — nothing ever consults it")
	}
	if !strings.Contains(err.Error(), "vpc_id") {
		t.Errorf("error = %q, want it to name the nested attribute", err)
	}
}

// TestValidateRejectsANestedMissingKind pins that the Kind check reaches
// inside Fields, not only the top-level per-attribute loop.
func TestValidateRejectsANestedMissingKind(t *testing.T) {
	d := sampleDefinition()
	d.Attributes["broken"] = Attribute{
		Kind:   value.KindMap,
		Fields: map[string]Attribute{"empty": {}},
	}
	if err := d.Validate(); err == nil {
		t.Fatal("a nested attribute with no Kind must be rejected")
	}
}

// TestValidateAcceptsAWellFormedNestedMap is the positive case: a plugin
// that declares Fields correctly, with no nested References and every
// nested attribute a valid Kind, must still load.
func TestValidateAcceptsAWellFormedNestedMap(t *testing.T) {
	d := sampleDefinition()
	d.Attributes["meta"] = Attribute{
		Kind:   value.KindMap,
		Fields: map[string]Attribute{"name": {Kind: value.KindString}},
	}
	if err := d.Validate(); err != nil {
		t.Fatalf("a well-formed declared map must load: %v", err)
	}
}

// TestValidateAllRefusesANilDefinitionRatherThanPanicking. ValidateAll is
// exported, so a caller that has not screened its input can reach it: a plugin
// sending a null array element must be told what is wrong rather than take the
// host down with it.
func TestValidateAllRefusesANilDefinitionRatherThanPanicking(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("ValidateAll panicked on a nil definition: %v", r)
		}
	}()
	if err := ValidateAll([]*ResourceDefinition{nil}); err == nil {
		t.Fatal("a nil definition must be refused")
	}
}
