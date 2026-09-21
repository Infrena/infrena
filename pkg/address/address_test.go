package address

import (
	"strings"
	"testing"
)

func TestStringForRootAndModule(t *testing.T) {
	if got := (Address{Name: "main"}).String(); got != "main" {
		t.Errorf("root address = %q, want \"main\"", got)
	}
	a := Address{Module: []string{"network", "inner"}, Name: "vpc"}
	if got := a.String(); got != "module.network.module.inner.vpc" {
		t.Errorf("nested address = %q", got)
	}
}

func TestParseRoundTrips(t *testing.T) {
	for _, in := range []string{"main", "module.network.vpc", "module.a.module.b.c"} {
		got, err := Parse(in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", in, err)
		}
		if got.String() != in {
			t.Errorf("round trip of %q gave %q", in, got.String())
		}
	}
}

func TestParseRejectsMalformed(t *testing.T) {
	for _, in := range []string{"", "module", "module.", "module.a", "module..x", "a.b"} {
		if _, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) succeeded; want an error", in)
		}
	}
}

func TestInModuleNestsWithoutAliasing(t *testing.T) {
	base := Address{Module: []string{"a"}, Name: "x"}
	child := base.InModule("b")
	if child.String() != "module.b.module.a.x" {
		t.Errorf("InModule gave %q", child.String())
	}
	if len(base.Module) != 1 {
		t.Error("InModule must not mutate the receiver's slice")
	}
}

func TestSortIsDeterministic(t *testing.T) {
	in := []Address{
		{Name: "zebra"},
		{Module: []string{"net"}, Name: "a"},
		{Name: "alpha"},
	}
	Sort(in)
	want := []string{"alpha", "module.net.a", "zebra"}
	for i, w := range want {
		if in[i].String() != w {
			t.Fatalf("position %d = %q, want %q", i, in[i].String(), w)
		}
	}
}

// TestAnAddressSurvivesARoundTrip is the property that matters: String and
// Parse are inverses, for every shape an address can take.
//
// A for_each key is user data. It routinely contains a dot — a hostname, a
// bucket name, a version — so a parser that splits on dots blindly turns
// subnet["eu.west.1a"] into three segments and refuses it.
//
// The apply path never parses an address back from text, so plan-and-apply
// keeps working while everything that reads an address back breaks. The worst
// of those is `apply --plan`: `plan --output` writes the address happily and
// the saved plan cannot be applied, after a human has reviewed and approved it.
func TestAnAddressSurvivesARoundTrip(t *testing.T) {
	for _, want := range []Address{
		{Name: "net"},
		{Name: "subnet", Key: "eu-west-1a"},
		// Dots inside the key.
		{Name: "subnet", Key: "eu.west.1a"},
		{Name: "host", Key: "api.example.com"},
		{Name: "pin", Key: "v1.2.3"},
		// A key containing the delimiters themselves.
		{Name: "odd", Key: `a]b`},
		{Name: "odd", Key: `a"b`},
		{Name: "odd", Key: `a[b].c`},
		{Module: []string{"net"}, Name: "db"},
		{Module: []string{"net"}, Name: "db", Key: "primary.eu"},
		{Module: []string{"a", "b"}, Name: "db"},
		// A keyed module call renders as a level whose name is bracketed.
		{Module: []string{`store["orders.eu"]`}, Name: "db"},
		{Module: []string{`store["orders"]`}, Name: "db", Key: "primary"},
	} {
		rendered := want.String()
		got, err := Parse(rendered)
		if err != nil {
			t.Errorf("Parse(%q) failed: %v", rendered, err)
			continue
		}
		if got.String() != rendered {
			t.Errorf("Parse(%q).String() = %q", rendered, got.String())
		}
		if got.Name != want.Name || got.Key != want.Key {
			t.Errorf("Parse(%q) = {Name:%q Key:%q}, want {Name:%q Key:%q}",
				rendered, got.Name, got.Key, want.Name, want.Key)
		}
		if len(got.Module) != len(want.Module) {
			t.Errorf("Parse(%q) module = %v, want %v", rendered, got.Module, want.Module)
			continue
		}
		for i := range want.Module {
			if got.Module[i] != want.Module[i] {
				t.Errorf("Parse(%q) module = %v, want %v", rendered, got.Module, want.Module)
				break
			}
		}
	}
}

// TestParseSetsTheKeyRatherThanLeavingItInTheName. Leaving the whole bracketed
// string in Name compares equal to the rendered form by coincidence, so the
// fields are asserted here rather than the round trip.
func TestParseSetsTheKeyRatherThanLeavingItInTheName(t *testing.T) {
	got, err := Parse(`subnet["eu-west-1a"]`)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "subnet" {
		t.Errorf("Name = %q, want %q", got.Name, "subnet")
	}
	if got.Key != "eu-west-1a" {
		t.Errorf("Key = %q, want %q", got.Key, "eu-west-1a")
	}
}

// TestParseRefusesANumericIndex, for the reason for_each exists: identity is
// the key, never the position.
func TestParseRefusesANumericIndex(t *testing.T) {
	if _, err := Parse("subnet[0]"); err == nil {
		t.Fatal("subnet[0] was accepted; an instance is named, not numbered")
	} else if !strings.Contains(err.Error(), "named, not numbered") {
		t.Errorf("the refusal does not explain itself: %v", err)
	}
}

// TestParseRefusesMalformedBrackets rather than producing a name containing
// punctuation that binds to nothing.
func TestParseRefusesMalformedBrackets(t *testing.T) {
	for _, s := range []string{`subnet["a`, `subnet[a"]`, `subnet]`, `subnet["a".id`} {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) was accepted", s)
		}
	}
}

// TestInModuleKeepsTheKey. Rebuilding the address dropped it, which is
// harmless only while every caller applies WithKey afterwards — and a keyed
// address that lost its key on the way into a module is a different resource
// in state.
func TestInModuleKeepsTheKey(t *testing.T) {
	a := Address{Name: "db", Key: "primary"}.InModule("stack")
	if a.Key != "primary" {
		t.Errorf("Key = %q, want %q", a.Key, "primary")
	}
	if got, want := a.String(), `module.stack.db["primary"]`; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}
