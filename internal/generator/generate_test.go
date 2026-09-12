package generator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infrata/infrata/internal/compiler"
	"github.com/infrata/infrata/internal/config"
	"github.com/infrata/infrata/internal/registry"
	"github.com/infrata/infrata/pkg/schema"
	"github.com/infrata/infrata/pkg/value"
	testprovider "github.com/infrata/infrata/providers/test"
)

func testRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	reg := registry.New()
	if err := reg.Register(testprovider.New(filepath.Join(t.TempDir(), "cloud.json"))); err != nil {
		t.Fatalf("registering the test provider: %v", err)
	}
	return reg
}

func prov(s string) value.Value { return value.String(s, value.SourceProvider) }
func provInt(n int64) value.Value {
	return value.Int(n, value.SourceProvider)
}

func devContext() schema.DefaultContext {
	return schema.DefaultContext{Environment: "dev", EnvironmentType: "development", Project: "p"}
}

// oneFile renders and returns the single file generated, failing if there is
// not exactly one.
func oneFile(t *testing.T, rs []Resource, ctx schema.DefaultContext) File {
	t.Helper()
	files, err := Generate(rs, testRegistry(t), ctx)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("generated %d files, want 1: %+v", len(files), files)
	}
	return files[0]
}

// TestGenerationOmitsWhatADefaultAlreadyProvides is §27's worked example: a
// resource whose size is what it would have been anyway produces no size line.
func TestGenerationOmitsWhatADefaultAlreadyProvides(t *testing.T) {
	f := oneFile(t, []Resource{{
		Name: "orders", Type: "test.database", ProviderID: "db-9",
		Attributes: map[string]value.Value{
			"engine": prov("postgres"),
			// 10 is test.database's default outside production.
			"size": provInt(10),
		},
	}}, devContext())

	out := string(f.Bytes)
	if !strings.Contains(out, "engine: postgres") {
		t.Errorf("engine has no default and must be emitted:\n%s", out)
	}
	// The ABSENCE is the assertion. Checking only that engine is present passes
	// against a generator that emits every attribute it was handed.
	if strings.Contains(out, "size:") {
		t.Errorf("size equals the provider default for this environment and must be omitted:\n%s", out)
	}
}

// TestADefaultIsJudgedInTheRightEnvironment. test.database's size default is
// 100 in production and 10 elsewhere, so the same discovered resource generates
// differently depending on where it was imported from.
//
// Both directions, because one alone cannot tell a context-aware comparison
// from a hard-coded constant.
func TestADefaultIsJudgedInTheRightEnvironment(t *testing.T) {
	r := []Resource{{
		Name: "orders", Type: "test.database", ProviderID: "db-9",
		Attributes: map[string]value.Value{"engine": prov("postgres"), "size": provInt(100)},
	}}

	prod := string(oneFile(t, r, schema.DefaultContext{
		Environment: "production", EnvironmentType: "production", Project: "p",
	}).Bytes)
	if strings.Contains(prod, "size:") {
		t.Errorf("size 100 IS the production default and must be omitted there:\n%s", prod)
	}

	dev := string(oneFile(t, r, devContext()).Bytes)
	if !strings.Contains(dev, "size: 100") {
		t.Errorf("size 100 is not the development default (10) and must be emitted there:\n%s", dev)
	}
}

// TestGenerationOmitsASensitiveAttributeAndSaysSo is THE hazard of this
// milestone. A generated file is destined for version control, and a secret
// committed to git is a secret ROTATED, not a secret deleted.
func TestGenerationOmitsASensitiveAttributeAndSaysSo(t *testing.T) {
	const secret = "hunter2-correct-horse"
	f := oneFile(t, []Resource{{
		Name: "orders", Type: "test.database", ProviderID: "db-9",
		Attributes: map[string]value.Value{
			"engine":   prov("postgres"),
			"password": prov(secret).WithSensitive(true),
		},
	}}, devContext())

	out := string(f.Bytes)
	// The value appears NOWHERE in the bytes — not as a value, not in a
	// comment, not in a diagnostic that got interpolated.
	if strings.Contains(out, secret) {
		t.Fatalf("the discovered secret was written to a file destined for version control:\n%s", out)
	}
	if strings.Contains(out, "<sensitive>") {
		t.Errorf("a redaction placeholder is not configuration — applying this would set the "+
			"password to the literal string:\n%s", out)
	}
	// And the omission is VISIBLE. A silent one tells a reader the resource has
	// no password, rather than that they must supply one.
	if !strings.Contains(out, "password") {
		t.Errorf("nothing in the file names the omitted attribute, so a reader cannot know "+
			"they must supply it:\n%s", out)
	}
}

// TestGenerationOmitsComputedAttributes. Not tidiness: configuration may not
// set a computed attribute at all, and every discovery returns them.
//
// A generator that emitted `id` produces a file that does not load, with a
// diagnostic pointing at a file the user never wrote.
func TestGenerationOmitsComputedAttributes(t *testing.T) {
	f := oneFile(t, []Resource{{
		Name: "orders", Type: "test.database", ProviderID: "db-9",
		Attributes: map[string]value.Value{
			"engine":   prov("postgres"),
			"id":       prov("db-9"),
			"endpoint": prov("db-9.example.internal"),
		},
	}}, devContext())

	out := string(f.Bytes)
	for _, computed := range []string{"id:", "endpoint:"} {
		if strings.Contains(out, computed) {
			t.Errorf("%s is computed and cannot be set; generated configuration would not "+
				"load:\n%s", computed, out)
		}
	}
}

// TestGenerationIsDeterministic. Generated configuration is read in diffs, and
// a file whose lines reorder between runs against unchanged infrastructure
// cannot be reviewed. The fixture's declaration order differs from its sorted
// order in both dimensions: resources and attributes.
func TestGenerationIsDeterministic(t *testing.T) {
	rs := []Resource{
		{Name: "zeta", Type: "test.network", ProviderID: "net-9", Attributes: map[string]value.Value{
			"cidr": prov("10.9.0.0/16"),
		}},
		{Name: "alpha", Type: "test.network", ProviderID: "net-1", Attributes: map[string]value.Value{
			"cidr": prov("10.1.0.0/16"),
		}},
		{Name: "mid", Type: "test.database", ProviderID: "db-5", Attributes: map[string]value.Value{
			"size": provInt(77), "engine": prov("postgres"), "network": prov("net-1"),
		}},
	}

	var first string
	for i := 0; i < 20; i++ {
		files, err := Generate(rs, testRegistry(t), devContext())
		if err != nil {
			t.Fatal(err)
		}
		var sb strings.Builder
		for _, f := range files {
			sb.WriteString(f.Name)
			sb.WriteString("\n")
			sb.Write(f.Bytes)
		}
		if i == 0 {
			first = sb.String()
			continue
		}
		if sb.String() != first {
			t.Fatalf("run %d differs:\n--- run %d ---\n%s\n--- run 0 ---\n%s", i, i, sb.String(), first)
		}
	}

	// And it is sorted, not merely stable. databases.yml before networks.yml,
	// alpha before zeta.
	if !strings.Contains(first, "databases.yml") || !strings.Contains(first, "networks.yml") {
		t.Fatalf("want one file per type:\n%s", first)
	}
	if strings.Index(first, "databases.yml") > strings.Index(first, "networks.yml") {
		t.Errorf("files are not sorted:\n%s", first)
	}
	if strings.Index(first, "alpha:") > strings.Index(first, "zeta:") {
		t.Errorf("resources within a file are not sorted:\n%s", first)
	}
}

func TestFileNameIsTheTypePluralised(t *testing.T) {
	for _, tc := range []struct{ typ, want string }{
		{"test.database", "databases.yml"},
		{"test.network", "networks.yml"},
		{"aws.s3_bucket", "s3_buckets.yml"},
		{"aws.iam_policy", "iam_policies.yml"},
		{"aws.address", "addresses.yml"},
		{"aws.gateway", "gateways.yml"},
		{"bare", "bares.yml"},
	} {
		if got := FileName(tc.typ); got != tc.want {
			t.Errorf("FileName(%q) = %q, want %q", tc.typ, got, tc.want)
		}
	}
}

// TestGeneratedConfigurationParsesBackAndPlansClean is the only test here that
// proves generation is CORRECT rather than merely plausible.
//
// Everything above checks the bytes against what this package meant to write.
// This one hands them to the thing that will actually read them: the real
// loader and the real compiler. It is the miniature of invariant 3, and it is
// what catches an omission rule that produces tidy YAML the compiler refuses —
// a computed attribute emitted, a required one dropped, a name the decoder will
// not accept.
func TestGeneratedConfigurationParsesBackAndPlansClean(t *testing.T) {
	files, err := Generate([]Resource{
		{Name: "orders", Type: "test.database", ProviderID: "db-9", Attributes: map[string]value.Value{
			"engine":   prov("postgres"),
			"size":     provInt(10), // the default — omitted
			"id":       prov("db-9"),
			"endpoint": prov("db-9.internal"),
			"password": prov("hunter2").WithSensitive(true),
			"network":  prov("net-1"),
		}},
		{Name: "vpc-0a1b", Type: "test.network", ProviderID: "vpc-0a1b", Attributes: map[string]value.Value{
			"cidr": prov("10.0.0.0/16"),
			"id":   prov("vpc-0a1b"),
		}},
	}, testRegistry(t), devContext())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "infra.yml"),
		[]byte("project: p\nenvironments:\n  dev:\n    type: development\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	discovered := filepath.Join(dir, config.DiscoveredDirName)
	if err := os.MkdirAll(discovered, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(discovered, f.Name), f.Bytes, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	loaded, err := config.Load(dir)
	if err != nil {
		t.Fatalf("generated configuration does not load: %v", err)
	}
	cfg, ds := compiler.Compile(loaded, testRegistry(t), compiler.Options{Environment: "dev", Dir: dir})
	if ds.HasErrors() {
		var sb strings.Builder
		ds.Render(&sb)
		for _, f := range files {
			sb.WriteString("\n--- " + f.Name + " ---\n")
			sb.Write(f.Bytes)
		}
		t.Fatalf("generated configuration does not compile:\n%s", sb.String())
	}

	// It compiles AND says what was discovered. A generator that emitted an
	// empty `resources:` block would also compile.
	if len(cfg.Resources) != 2 {
		t.Fatalf("compiled %d resources, want 2: %v", len(cfg.Resources), cfg.Addresses())
	}
	db, ok := cfg.Resources["orders"]
	if !ok {
		t.Fatalf("no resource named `orders`: %v", cfg.Addresses())
	}
	if engine, _ := db.Attrs["engine"].AsString(); engine != "postgres" {
		t.Errorf("engine = %v, want the discovered value", db.Attrs["engine"])
	}
	// The omitted default came back from the schema, which is the whole
	// argument for omitting it.
	if size, _ := db.Attrs["size"].AsInt(); size != 10 {
		t.Errorf("size = %v, want 10 filled in from the provider default", db.Attrs["size"])
	}
	if db.Attrs["size"].Scope != value.ScopeProviderDefault {
		t.Errorf("size scope = %v, want ScopeProviderDefault — the omission must be why it is "+
			"there, not a coincidence", db.Attrs["size"].Scope)
	}
}
