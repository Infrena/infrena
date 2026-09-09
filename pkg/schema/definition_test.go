package schema

import (
	"testing"

	"infra/pkg/value"
)

func sampleDefinition() *ResourceDefinition {
	return &ResourceDefinition{
		Type:        "test.database",
		Description: "A fake database",
		Attributes: map[string]Attribute{
			"engine":   {Kind: value.KindString, Required: true, ForceNew: true},
			"size":     {Kind: value.KindInt, Default: func(DefaultContext) (any, bool) { return int64(10), true }},
			"password": {Kind: value.KindString, Sensitive: true},
			"endpoint": {Kind: value.KindString, Computed: true},
		},
		Requirements: []Requirement{{Name: "network", Types: []string{"test.network"}}},
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

func TestDefaultResolverSeesOnlyItsContext(t *testing.T) {
	d := sampleDefinition()
	attr, _ := d.Attribute("size")
	got, ok := attr.Default(DefaultContext{Environment: "dev", Type: "test.database"})
	if !ok || got != int64(10) {
		t.Errorf("default = %v, %v; want 10, true", got, ok)
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
		Default:  func(DefaultContext) (any, bool) { return "x", true },
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
