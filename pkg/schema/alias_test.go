package schema

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/infrena/infrena/pkg/value"
)

func aliasDef() *ResourceDefinition {
	return &ResourceDefinition{
		Type: "aws.ec2.vpc",
		Attributes: map[string]Attribute{
			"CidrBlock":        {Kind: value.KindString, Required: true, Aliases: []string{"cidr", "cidr_block"}},
			"EnableDnsSupport": {Kind: value.KindBool, Computed: true, Optional: true},
		},
		Capabilities: Capabilities{Create: true, Read: true, Delete: true},
	}
}

// TestEverySpellingReachesTheSameAttribute.
//
// The point of aliases: an AWS plugin keeps CloudFormation's own property names, and a
// user writes whichever they find natural. Case-insensitively, so `cidrblock` works
// without the plugin declaring it.
func TestEverySpellingReachesTheSameAttribute(t *testing.T) {
	def := aliasDef()
	for _, written := range []string{"CidrBlock", "cidrblock", "CIDRBLOCK", "cidr", "CIDR", "cidr_block"} {
		got, ok := def.Canonical(written)
		if !ok {
			t.Errorf("%q resolves to nothing", written)
			continue
		}
		if got != "CidrBlock" {
			t.Errorf("%q resolves to %q, want CidrBlock — the canonical name is the identity", written, got)
		}
	}
	if _, ok := def.Canonical("nosuchthing"); ok {
		t.Error("an unknown name must resolve to nothing, so the compiler reports it")
	}
}

// TestFoldingIsCaseOnly.
//
// `cidr_block` and `cidrblock` stay DIFFERENT spellings. Folding punctuation as well
// would make them collide, and a generic AWS plugin that generates a snake_case alias for
// every property would then fail to load on any type with two properties differing only
// by an underscore. A plugin wanting both declares both.
func TestFoldingIsCaseOnly(t *testing.T) {
	def := &ResourceDefinition{
		Type: "t.thing",
		Attributes: map[string]Attribute{
			"cidr_block": {Kind: value.KindString},
			"cidrblock":  {Kind: value.KindString},
		},
		Capabilities: Capabilities{Create: true, Read: true, Delete: true},
	}
	if err := def.Validate(); err != nil {
		t.Fatalf("underscores are not folded, so these do not collide: %v", err)
	}
}

// TestNamesThatFoldTogetherAreRefusedAtLoad.
//
// A collision decided at runtime is a silent wrong answer — whichever attribute the map
// walk reached first — so it is a plugin that will not start instead, with both spellings
// named. Three shapes, because they come from different mistakes: two attributes, an
// attribute and another's alias, and two aliases.
func TestNamesThatFoldTogetherAreRefusedAtLoad(t *testing.T) {
	for _, tc := range []struct {
		name  string
		attrs map[string]Attribute
		both  []string
	}{
		{
			"two attributes",
			map[string]Attribute{
				"CidrBlock": {Kind: value.KindString},
				"cidrblock": {Kind: value.KindString},
			},
			[]string{"CidrBlock", "cidrblock"},
		},
		{
			"an attribute and another's alias",
			map[string]Attribute{
				"CidrBlock": {Kind: value.KindString},
				"other":     {Kind: value.KindString, Aliases: []string{"cidrblock"}},
			},
			[]string{"CidrBlock", "cidrblock"},
		},
		{
			"two aliases",
			map[string]Attribute{
				"one": {Kind: value.KindString, Aliases: []string{"cidr"}},
				"two": {Kind: value.KindString, Aliases: []string{"CIDR"}},
			},
			[]string{"cidr", "CIDR"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			def := &ResourceDefinition{
				Type: "t.thing", Attributes: tc.attrs,
				Capabilities: Capabilities{Create: true, Read: true, Delete: true},
			}
			err := def.Validate()
			if err == nil {
				t.Fatal("names that differ only by case must be refused: configuration naming one " +
					"would reach whichever was found first")
			}
			for _, want := range tc.both {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the error does not name %q, so the author cannot tell which two collide: %v", want, err)
				}
			}
		})
	}
}

// TestOptionalWithoutComputedIsRefused. Every attribute that is not Required is already
// optional, so the flag alone says nothing — and an author setting it believes it does.
func TestOptionalWithoutComputedIsRefused(t *testing.T) {
	def := &ResourceDefinition{
		Type:         "t.thing",
		Attributes:   map[string]Attribute{"name": {Kind: value.KindString, Optional: true}},
		Capabilities: Capabilities{Create: true, Read: true, Delete: true},
	}
	if err := def.Validate(); err == nil {
		t.Error("Optional without Computed must be refused rather than silently meaning nothing")
	}
}

// TestDisplayAndSpellings. Display is what a plan renders; Spellings is what `explain`
// lists. Both are pure functions of the schema — applied at render time, never stored —
// which is why adding an alias later cannot rewrite a stored key.
func TestDisplayAndSpellings(t *testing.T) {
	def := aliasDef()
	if got := def.Display("CidrBlock"); got != "cidr" {
		t.Errorf("Display = %q, want the FIRST alias so a plugin controls what users see", got)
	}
	if got := def.Display("EnableDnsSupport"); got != "EnableDnsSupport" {
		t.Errorf("Display = %q, want the canonical name when there is no alias", got)
	}
	got := def.Spellings("CidrBlock")
	if len(got) != 3 || got[0] != "CidrBlock" {
		t.Errorf("Spellings = %v, want the canonical name first then its aliases", got)
	}
}

// TestAliasesSurviveTheWire. A plugin declares them; the host learns them the same way it
// learns everything else. infrena holds no mapping of its own, so if they did not cross
// the wire they would not exist at all.
func TestAliasesSurviveTheWire(t *testing.T) {
	before := Attribute{Kind: value.KindString, Computed: true, Optional: true, Aliases: []string{"cidr"}}
	data, err := before.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var after Attribute
	if err := after.UnmarshalJSON(data); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !after.Optional {
		t.Error("Optional did not cross the wire, so a plugin could not express a provider-chosen attribute")
	}
	if len(after.Aliases) != 1 || after.Aliases[0] != "cidr" {
		t.Errorf("Aliases = %v, want [cidr]", after.Aliases)
	}
}

// definedAttr is a nested attribute that passes the other validation rules, so a
// test about spelling collisions fails on the collision rather than on something
// else.
func definedAttr(aliases ...string) Attribute {
	return Attribute{Kind: value.KindString, Optional: true, Computed: true, Aliases: aliases}
}

func withFields(fields map[string]Attribute) *ResourceDefinition {
	return &ResourceDefinition{Type: "x.y", Attributes: map[string]Attribute{
		"template": {Kind: value.KindMap, Optional: true, Computed: true, Fields: fields},
	}}
}

// TestNestedSpellingsMustNotCollide. A collision decided at runtime is a silent
// wrong answer — whichever key the map was walked to first — which is the whole
// reason the top-level check refuses one at load. Nested names resolve by the
// same ladder, so they need the same refusal: without it, activating a nested
// alias turns a schema that used to be merely wrong into one that is ambiguous.
func TestNestedSpellingsMustNotCollide(t *testing.T) {
	for _, c := range []struct {
		name   string
		fields map[string]Attribute
	}{
		{"two nested names folding together", map[string]Attribute{
			"serviceName": definedAttr(),
			"servicename": definedAttr(),
		}},
		{"a nested alias colliding with a sibling name", map[string]Attribute{
			"serviceName": definedAttr(),
			"other":       definedAttr("SERVICENAME"),
		}},
		{"two nested aliases colliding with each other", map[string]Attribute{
			"one": definedAttr("shared"),
			"two": definedAttr("SHARED"),
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := withFields(c.fields).Validate()
			if err == nil {
				t.Fatal("accepted, so configuration naming it reaches whichever key was found " +
					"first: a silent wrong answer rather than a plugin that will not start")
			}
			if !strings.Contains(err.Error(), "template") {
				t.Errorf("the error does not say where the collision is: %v", err)
			}
		})
	}
}

// The check has to reach every depth, not just the first nested level.
func TestSpellingCollisionsAreRefusedAtEveryDepth(t *testing.T) {
	def := withFields(map[string]Attribute{
		"containers": {Kind: value.KindList, Optional: true, Computed: true, Fields: map[string]Attribute{
			"imageURI": definedAttr(),
			"imageuri": definedAttr(),
		}},
	})
	if err := def.Validate(); err == nil {
		t.Fatal("a collision two levels down was accepted")
	} else if !strings.Contains(err.Error(), "containers") {
		t.Errorf("the error does not name the path to the collision: %v", err)
	}
}

// Names that only collide ACROSS levels are fine, and must stay fine: a nested
// key and a top-level attribute are never candidates for the same lookup, so
// refusing them would reject schemas that are perfectly unambiguous.
func TestNamesAtDifferentDepthsMayRepeat(t *testing.T) {
	def := &ResourceDefinition{Type: "x.y", Attributes: map[string]Attribute{
		"serviceName": definedAttr(),
		"template": {Kind: value.KindMap, Optional: true, Computed: true, Fields: map[string]Attribute{
			"serviceName": definedAttr(),
		}},
	}}
	if err := def.Validate(); err != nil {
		t.Errorf("the same name at two depths was refused, but they are never looked up "+
			"together: %v", err)
	}
}

// listOf builds a KindList attribute whose elements are maps with the given
// keys — the shape a repeated block takes.
func listOf(fields map[string]Attribute) Attribute {
	return Attribute{
		Kind: value.KindList, Optional: true, Computed: true,
		Elem: &Attribute{Kind: value.KindMap, Fields: fields},
	}
}

// TestElemBelongsOnAListAndFieldsOnAMap. The two describe different things —
// every element of a list, versus one map's known keys — and the whole reason
// Elem exists is that overloading Fields to mean both left the meaning implied
// by a sibling Kind, which nothing enforced.
func TestElemBelongsOnAListAndFieldsOnAMap(t *testing.T) {
	for _, c := range []struct {
		name    string
		attr    Attribute
		refused bool
	}{
		{"Elem on a list", listOf(map[string]Attribute{"imageURI": definedAttr()}), false},
		{"Elem on a map", Attribute{
			Kind: value.KindMap, Optional: true, Computed: true,
			Elem: &Attribute{Kind: value.KindString},
		}, true},
		{"Elem on a string", Attribute{
			Kind: value.KindString, Optional: true, Computed: true,
			Elem: &Attribute{Kind: value.KindString},
		}, true},
		{"Fields on a list", Attribute{
			Kind: value.KindList, Optional: true, Computed: true,
			Fields: map[string]Attribute{"x": definedAttr()},
		}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			def := &ResourceDefinition{Type: "x.y", Attributes: map[string]Attribute{"a": c.attr}}
			err := def.Validate()
			if c.refused && err == nil {
				t.Error("accepted, but the kind and the nesting edge disagree")
			}
			if !c.refused && err != nil {
				t.Errorf("refused a legitimate declaration: %v", err)
			}
		})
	}
}

// A collision inside a list element's keys is as ambiguous as one anywhere
// else, and the check has to reach through Elem to find it.
func TestSpellingCollisionsAreRefusedInsideAListElement(t *testing.T) {
	def := &ResourceDefinition{Type: "x.y", Attributes: map[string]Attribute{
		"containers": listOf(map[string]Attribute{
			"imageURI": definedAttr(),
			"imageuri": definedAttr(),
		}),
	}}
	if err := def.Validate(); err == nil {
		t.Fatal("a collision inside a list element was accepted")
	} else if !strings.Contains(err.Error(), "containers") {
		t.Errorf("the error does not name the path to the collision: %v", err)
	}
}

// Elem has to survive the plugin boundary, or an out-of-process provider
// declares element keys the host never sees — which is the gap this closes.
func TestElemSurvivesTheWire(t *testing.T) {
	in := listOf(map[string]Attribute{"imageURI": {Kind: value.KindString, Optional: true, Aliases: []string{"image"}}})
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Attribute
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Elem == nil {
		t.Fatalf("Elem did not survive the round trip: %s", b)
	}
	got, ok := out.Elem.Fields["imageURI"]
	if !ok {
		t.Fatalf("the element's keys did not survive: %+v", out.Elem)
	}
	if len(got.Aliases) != 1 || got.Aliases[0] != "image" {
		t.Errorf("an alias inside a list element did not survive: %+v", got)
	}
}

// TestAnElementMayCarryTheSameIgnoredFlagsANestedKeyMay. A key inside a map may
// declare Required, Optional, Computed or a Default; nothing reads them at
// nesting depth, and validateFields accepts them. An element is in exactly the
// same position — not configured independently of the composite holding it — so
// refusing it there would be an asymmetry with no rule behind it.
//
// This is not hypothetical tidiness. The natural conversion from a provider's
// own catalog sets Optional and Computed on anything that is neither required
// nor output, so a rule refusing them on elements rejects every list a provider
// declares, for a flag that is ignored either way.
func TestAnElementMayCarryTheSameIgnoredFlagsANestedKeyMay(t *testing.T) {
	for _, c := range []struct {
		name string
		elem Attribute
	}{
		{"Optional and Computed, the default conversion's shape", Attribute{
			Kind: value.KindMap, Optional: true, Computed: true,
			Fields: map[string]Attribute{"imageURI": definedAttr()},
		}},
		{"Required", Attribute{Kind: value.KindString, Required: true}},
		{"Computed alone", Attribute{Kind: value.KindString, Computed: true}},
		{"a Default", Attribute{Kind: value.KindString, Default: "x"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			elem := c.elem
			def := &ResourceDefinition{Type: "x.y", Attributes: map[string]Attribute{
				"containers": {Kind: value.KindList, Optional: true, Computed: true, Elem: &elem},
			}}
			if err := def.Validate(); err != nil {
				t.Errorf("refused: %v\n\nA nested map key carrying the same flag is accepted, so "+
					"this is an asymmetry rather than a rule.", err)
			}
		})
	}

	// The mirror, so the claim above is tested rather than asserted: a nested
	// map key really does carry these today.
	nested := &ResourceDefinition{Type: "x.y", Attributes: map[string]Attribute{
		"template": {Kind: value.KindMap, Optional: true, Computed: true, Fields: map[string]Attribute{
			"k": {Kind: value.KindString, Optional: true, Computed: true, Default: "x"},
		}},
	}}
	if err := nested.Validate(); err != nil {
		t.Errorf("a nested map key was refused for carrying ignored flags, which would make the "+
			"element rule consistent after all: %v", err)
	}
}
