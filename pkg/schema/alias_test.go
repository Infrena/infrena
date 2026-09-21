package schema

import (
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
