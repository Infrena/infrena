package value

import "testing"

func TestCarrySensitivityMarksAScalar(t *testing.T) {
	dst := String("hunter2", SourceProvider)
	src := String("hunter2", SourceExplicit).WithSensitive(true)

	got := CarrySensitivity(dst, src)
	if !got.Sensitive {
		t.Fatal("a value the engine knew was sensitive came back unmarked")
	}
	if got.Raw != "hunter2" || got.Source != SourceProvider {
		t.Errorf("CarrySensitivity changed more than the flag: %#v", got)
	}
	if src.Raw != "hunter2" || !src.Sensitive {
		t.Error("CarrySensitivity mutated src")
	}
}

func TestCarrySensitivityLeavesPlainValuesAlone(t *testing.T) {
	// The direction that matters as much as the leak: a classifier that
	// marked everything would redact a plan into uselessness.
	dst := String("nginx", SourceProvider)
	got := CarrySensitivity(dst, String("nginx", SourceExplicit))
	if got.Sensitive {
		t.Fatal("a value nothing made sensitive was classified sensitive")
	}
}

func TestCarrySensitivityNeverClearsAFlag(t *testing.T) {
	// Schema-declared sensitivity lives on dst; propagated sensitivity on
	// src. They must combine, never compete.
	dst := String("hunter2", SourceProvider).WithSensitive(true)
	got := CarrySensitivity(dst, String("hunter2", SourceExplicit))
	if !got.Sensitive {
		t.Fatal("CarrySensitivity cleared a flag dst already carried")
	}
}

func TestCarrySensitivityIsPerLeafInAMap(t *testing.T) {
	dst := Map(map[string]Value{
		"password": String("hunter2", SourceProvider),
		"host":     String("db.internal", SourceProvider),
	}, SourceProvider)
	src := Map(map[string]Value{
		"password": String("hunter2", SourceExplicit).WithSensitive(true),
		"host":     String("db.internal", SourceExplicit),
	}, SourceExplicit)

	got := CarrySensitivity(dst, src)
	if got.Sensitive {
		t.Error("the whole map was marked sensitive; only one leaf is")
	}
	m := got.Raw.(map[string]Value)
	if !m["password"].Sensitive {
		t.Error("the sensitive leaf was not marked")
	}
	if m["host"].Sensitive {
		t.Error("a plain leaf beside a sensitive one was marked")
	}
	// Rendering is the reason this granularity matters.
	rendered := Format(got, FormatOptions{QuoteStrings: true})
	if rendered != `{host: "db.internal", password: <sensitive>}` {
		t.Errorf("Format(%q) hides more or less than the one secret", rendered)
	}
}

func TestCarrySensitivityIsPerElementInAList(t *testing.T) {
	dst := List([]Value{
		String("public", SourceProvider),
		String("hunter2", SourceProvider),
	}, SourceProvider)
	src := List([]Value{
		String("public", SourceExplicit),
		String("hunter2", SourceExplicit).WithSensitive(true),
	}, SourceExplicit)

	items := CarrySensitivity(dst, src).Raw.([]Value)
	if items[0].Sensitive || !items[1].Sensitive {
		t.Fatalf("per-element sensitivity is wrong: %#v", items)
	}
}

func TestCarrySensitivityFallsBackToTheWholeValueWhenShapesDisagree(t *testing.T) {
	// A provider that reshapes a value takes the per-leaf alignment away. The
	// safe direction is to redact the whole thing: returning it unmarked
	// because the shapes disagreed is a silent decision to print what may be
	// a secret, invisible in exactly the case that matters.
	cases := []struct {
		name string
		dst  Value
		src  Value
	}{
		{"map flattened to a string", String("password=hunter2", SourceProvider),
			Map(map[string]Value{"password": String("hunter2", SourceExplicit).WithSensitive(true)}, SourceExplicit)},
		{"kind changed", List([]Value{String("hunter2", SourceProvider)}, SourceProvider),
			Map(map[string]Value{"p": String("hunter2", SourceExplicit).WithSensitive(true)}, SourceExplicit)},
		{"src Raw does not match its Kind", String("hunter2", SourceProvider),
			Value{Kind: KindMap, Known: true, Raw: "not a map", Sensitive: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CarrySensitivity(tc.dst, tc.src); !got.Sensitive {
				t.Fatalf("a value carrying an unmatched secret came back renderable: %s",
					Format(got, FormatOptions{}))
			}
		})
	}
}

func TestCarrySensitivityAttrsCopiesRatherThanWritingThrough(t *testing.T) {
	dst := map[string]Value{"database_url": String("hunter2", SourceProvider)}
	src := map[string]Value{"database_url": String("hunter2", SourceExplicit).WithSensitive(true)}

	got := CarrySensitivityAttrs(dst, src)
	if !got["database_url"].Sensitive {
		t.Fatal("the attribute was not marked")
	}
	if dst["database_url"].Sensitive {
		t.Error("CarrySensitivityAttrs wrote through to the caller's map")
	}
}

func TestCarrySensitivityAttrsIgnoresKeysOnlyOneSideHas(t *testing.T) {
	dst := map[string]Value{"url": String("https://app.test", SourceProvider)}
	src := map[string]Value{"password": String("hunter2", SourceExplicit).WithSensitive(true)}

	got := CarrySensitivityAttrs(dst, src)
	if got["url"].Sensitive {
		t.Error("an unrelated attribute was marked because another one was secret")
	}
	if _, ok := got["password"]; ok {
		t.Error("CarrySensitivityAttrs invented an attribute dst never had")
	}
}

func TestHasSensitiveFindsALeafInsideAComposite(t *testing.T) {
	// The reason HasSensitive recurses: a composite is not itself marked when
	// only one of its leaves is, so a top-level check would report a map
	// containing a password as carrying no sensitivity at all.
	nested := Map(map[string]Value{
		"creds": Map(map[string]Value{
			"password": String("hunter2", SourceExplicit).WithSensitive(true),
		}, SourceExplicit),
	}, SourceExplicit)

	if !HasSensitive(nested) {
		t.Error("HasSensitive missed a leaf two levels down")
	}
	if HasSensitive(Map(map[string]Value{"host": String("db", SourceExplicit)}, SourceExplicit)) {
		t.Error("HasSensitive reported sensitivity in a map that has none")
	}
	if HasSensitive(Value{Kind: KindMap, Known: true, Raw: "not a map"}) {
		t.Error("HasSensitive trusted Kind over Raw")
	}
}
