package compiler

import (
	"strings"
	"testing"

	"infra/internal/registry"
	"infra/pkg/address"
	"infra/pkg/resource"
	"infra/pkg/value"
	testprovider "infra/providers/test"
)

func testRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	reg := registry.New()
	if err := reg.Register(testprovider.New(t.TempDir() + "/fake-cloud.json")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return reg
}

func oneResource(typ string, attrs map[string]value.Value) *ResolvedConfig {
	return &ResolvedConfig{
		Project:     "myapp",
		Environment: "dev",
		Resources: map[string]*resource.ResolvedResource{
			"r": {Address: address.Address{Name: "r"}, Type: typ, Attrs: attrs},
		},
	}
}

func TestSchemaRejectsUnknownType(t *testing.T) {
	cfg := oneResource("aws.rds", nil)
	ds := bindSchemas(cfg, testRegistry(t), Options{Environment: "dev"})
	if !ds.HasErrors() {
		t.Fatal("an unregistered type must be an error")
	}
	var out strings.Builder
	ds.Render(&out)
	if !strings.Contains(out.String(), "test.database") {
		t.Errorf("the diagnostic should list known types:\n%s", out.String())
	}
}

func TestSchemaRejectsUnknownAttribute(t *testing.T) {
	cfg := oneResource("test.network", map[string]value.Value{
		"cidr":     value.String("10.0.0.0/16", value.SourceExplicit),
		"nonsense": value.Bool(true, value.SourceExplicit),
	})
	if ds := bindSchemas(cfg, testRegistry(t), Options{Environment: "dev"}); !ds.HasErrors() {
		t.Error("an attribute the schema does not define must be an error")
	}
}

func TestSchemaRejectsSettingAComputedAttribute(t *testing.T) {
	cfg := oneResource("test.network", map[string]value.Value{
		"cidr": value.String("10.0.0.0/16", value.SourceExplicit),
		"id":   value.String("net-1", value.SourceExplicit),
	})
	if ds := bindSchemas(cfg, testRegistry(t), Options{Environment: "dev"}); !ds.HasErrors() {
		t.Error("configuration must not set a computed attribute")
	}
}

func TestSchemaRejectsAMissingRequiredAttribute(t *testing.T) {
	cfg := oneResource("test.network", nil) // cidr is required
	if ds := bindSchemas(cfg, testRegistry(t), Options{Environment: "dev"}); !ds.HasErrors() {
		t.Error("a missing required attribute must be an error")
	}
}

func TestSchemaRejectsAWrongKind(t *testing.T) {
	cfg := oneResource("test.database", map[string]value.Value{
		"engine": value.String("postgres", value.SourceExplicit),
		"size":   value.String("large", value.SourceExplicit), // size is an integer
	})
	ds := bindSchemas(cfg, testRegistry(t), Options{Environment: "dev"})
	if !ds.HasErrors() {
		t.Fatal("a string where an integer is required must be an error")
	}
	var out strings.Builder
	ds.Render(&out)
	if !strings.Contains(out.String(), "integer") {
		t.Errorf("the diagnostic should name the expected kind:\n%s", out.String())
	}
}

func TestSchemaSkipsKindCheckOnUnknowns(t *testing.T) {
	// An unknown carries the kind it will have; a mismatch there is not a
	// user error and reporting it would be noise on every reference.
	cfg := oneResource("test.database", map[string]value.Value{
		"engine": value.String("postgres", value.SourceExplicit),
		"size":   value.Unknown(value.KindString, value.SourceComputed),
	})
	if ds := bindSchemas(cfg, testRegistry(t), Options{Environment: "dev"}); ds.HasErrors() {
		t.Errorf("an unknown must not trip kind checking: %+v", ds)
	}
}

func TestSchemaFillsDefaults(t *testing.T) {
	cfg := oneResource("test.database", map[string]value.Value{
		"engine": value.String("postgres", value.SourceExplicit),
	})
	if ds := bindSchemas(cfg, testRegistry(t), Options{Environment: "dev"}); ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	size, ok := cfg.Resources["r"].Attrs["size"]
	if !ok {
		t.Fatal("the default for size was not filled in")
	}
	if size.Source != value.SourceDefault {
		t.Errorf("Source = %v, want SourceDefault — a plan must be able to print [default]", size.Source)
	}
	if n, _ := size.AsInt(); n != 10 {
		t.Errorf("size = %d, want the dev default of 10", n)
	}
}

func TestSchemaDefaultsAreEnvironmentAware(t *testing.T) {
	cfg := oneResource("test.database", map[string]value.Value{
		"engine": value.String("postgres", value.SourceExplicit),
	})
	bindSchemas(cfg, testRegistry(t), Options{Environment: "production"})

	// The fake provider's size default is 100 when EnvironmentType is
	// production, 10 otherwise.
	if n, _ := cfg.Resources["r"].Attrs["size"].AsInt(); n != 100 {
		t.Errorf("size = %d in production, want 100 — defaults may vary by environment", n)
	}
}

func TestSchemaDefaultNeverOverwritesAnExplicitValue(t *testing.T) {
	cfg := oneResource("test.database", map[string]value.Value{
		"engine": value.String("postgres", value.SourceExplicit),
		"size":   value.Int(50, value.SourceExplicit),
	})
	bindSchemas(cfg, testRegistry(t), Options{Environment: "dev"})

	size := cfg.Resources["r"].Attrs["size"]
	if n, _ := size.AsInt(); n != 50 {
		t.Errorf("size = %d, want the explicit 50", n)
	}
	if size.Source != value.SourceExplicit {
		t.Errorf("Source = %v, want SourceExplicit — an explicit value always wins", size.Source)
	}
}

func TestSchemaMarksSensitiveAttributes(t *testing.T) {
	cfg := oneResource("test.database", map[string]value.Value{
		"engine":   value.String("postgres", value.SourceExplicit),
		"password": value.String("hunter2", value.SourceExplicit),
	})
	bindSchemas(cfg, testRegistry(t), Options{Environment: "dev"})

	if !cfg.Resources["r"].Attrs["password"].Sensitive {
		t.Error("an attribute the schema marks Sensitive must come out sensitive")
	}
}

func TestSchemaDoesNotDeclassifyAnAlreadySensitiveValue(t *testing.T) {
	cfg := oneResource("test.database", map[string]value.Value{
		"engine": value.String("postgres", value.SourceExplicit).WithSensitive(true),
	})
	bindSchemas(cfg, testRegistry(t), Options{Environment: "dev"})

	if !cfg.Resources["r"].Attrs["engine"].Sensitive {
		t.Error("schema binding must add sensitivity, never clear it — engine is not a sensitive attribute but this value arrived classified")
	}
}

func TestSchemaReportsEveryProblemAtOnce(t *testing.T) {
	cfg := oneResource("test.network", map[string]value.Value{
		"nonsense_one": value.Bool(true, value.SourceExplicit),
		"nonsense_two": value.Bool(true, value.SourceExplicit),
	})
	ds := bindSchemas(cfg, testRegistry(t), Options{Environment: "dev"})
	if len(ds) < 3 {
		t.Errorf("got %d diagnostics, want at least 3 (two unknown attributes and one missing required)", len(ds))
	}
}
