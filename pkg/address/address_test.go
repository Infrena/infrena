package address

import "testing"

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
