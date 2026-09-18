package generator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infrena/infrena/internal/compiler"
	"github.com/infrena/infrena/internal/config"
	"github.com/infrena/infrena/internal/registry"
	"github.com/infrena/infrena/pkg/value"
	testprovider "github.com/infrena/infrena/providers/test"
)

func testRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	reg := registry.New()
	if err := reg.Register("fake", testprovider.New(filepath.Join(t.TempDir(), "cloud.json"))); err != nil {
		t.Fatalf("registering the test provider: %v", err)
	}
	return reg
}

func prov(s string) value.Value { return value.String(s, value.SourceProvider) }
func provInt(n int64) value.Value {
	return value.Int(n, value.SourceProvider)
}

// oneFile renders and returns the single file generated, failing if there is
// not exactly one.
func oneFile(t *testing.T, rs []Resource) File {
	t.Helper()
	files, _, err := Generate(rs, testRegistry(t), MinimalOptions())
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
		Name: "orders", Type: "fake.database", ProviderID: "db-9",
		Attributes: map[string]value.Value{
			"engine": prov("postgres"),
			// 10 is fake.database's default.
			"size": provInt(10),
		},
	}})

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

// TestADefaultIsJudgedInTheRightEnvironment was here until M9 withdrew PLAN.md
// §13. It asserted that the same discovered `size: 100` was omitted when
// importing into production and emitted elsewhere, because the default differed.
// A default no longer varies, so the two environments no longer differ and the
// test has nothing left to distinguish.
//
// What survives is the rung itself, covered by
// TestGenerationOmitsWhatADefaultAlreadyProvides: a value equal to THE default
// is omitted.

// TestGenerationOmitsASensitiveAttributeAndSaysSo is THE hazard of this
// milestone. A generated file is destined for version control, and a secret
// committed to git is a secret ROTATED, not a secret deleted.
func TestGenerationOmitsASensitiveAttributeAndSaysSo(t *testing.T) {
	const secret = "hunter2-correct-horse"
	f := oneFile(t, []Resource{{
		Name: "orders", Type: "fake.database", ProviderID: "db-9",
		Attributes: map[string]value.Value{
			"engine":   prov("postgres"),
			"password": prov(secret).WithSensitive(true),
		},
	}})

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
		Name: "orders", Type: "fake.database", ProviderID: "db-9",
		Attributes: map[string]value.Value{
			"engine":   prov("postgres"),
			"id":       prov("db-9"),
			"endpoint": prov("db-9.example.internal"),
		},
	}})

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
		{Name: "zeta", Type: "fake.network", ProviderID: "net-9", Attributes: map[string]value.Value{
			"cidr": prov("10.9.0.0/16"),
		}},
		{Name: "alpha", Type: "fake.network", ProviderID: "net-1", Attributes: map[string]value.Value{
			"cidr": prov("10.1.0.0/16"),
		}},
		{Name: "mid", Type: "fake.database", ProviderID: "db-5", Attributes: map[string]value.Value{
			"size": provInt(77), "engine": prov("postgres"), "network": prov("net-1"),
		}},
	}

	var first string
	for i := range 20 {
		files, _, err := Generate(rs, testRegistry(t), MinimalOptions())
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
		{"fake.database", "databases.yml"},
		{"fake.network", "networks.yml"},
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
	files, _, err := Generate([]Resource{
		{Name: "orders", Type: "fake.database", ProviderID: "db-9", Attributes: map[string]value.Value{
			"engine":   prov("postgres"),
			"size":     provInt(10), // the default — omitted
			"id":       prov("db-9"),
			"endpoint": prov("db-9.internal"),
			"password": prov("hunter2").WithSensitive(true),
			"network":  prov("net-1"),
		}},
		{Name: "vpc-0a1b", Type: "fake.network", ProviderID: "vpc-0a1b", Attributes: map[string]value.Value{
			"cidr": prov("10.0.0.0/16"),
			"id":   prov("vpc-0a1b"),
		}},
	}, testRegistry(t), MinimalOptions())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "infra.yml"),
		[]byte("project: p\nenvironments:\n  dev: {}\n"), 0o644); err != nil {
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

// TestExportModeKeepsDefaultsButStillOmitsSecrets is §28 against §27.
//
// The two modes differ in exactly ONE of the three omissions. A value equal to
// its default is a fact about the resource that an audit wants and a
// configuration file does not need. The other two omissions are not
// negotiable: a computed attribute cannot be set at all, and a secret in an
// export is if anything more likely to be pasted somewhere public than one in a
// generated file.
func TestExportModeKeepsDefaultsButStillOmitsSecrets(t *testing.T) {
	const secret = "hunter2-correct-horse"
	rs := []Resource{{
		Name: "orders", Type: "fake.database", ProviderID: "db-9",
		Attributes: map[string]value.Value{
			"engine":   prov("postgres"),
			"size":     provInt(10), // the development default
			"endpoint": prov("db-9.internal"),
			"password": prov(secret).WithSensitive(true),
		},
	}}

	full, _, err := Generate(rs, testRegistry(t), Options{Minimal: false})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	out := string(full[0].Bytes)

	// The one difference.
	if !strings.Contains(out, "size: 10") {
		t.Errorf("export must keep a value equal to its default:\n%s", out)
	}
	// The two non-differences. Asserting only the first would pass against an
	// export mode that simply skipped every omission.
	if strings.Contains(out, secret) {
		t.Fatalf("the secret is in the export:\n%s", out)
	}
	if strings.Contains(out, "endpoint:") {
		t.Errorf("a computed attribute cannot be set and must stay out of both modes:\n%s", out)
	}

	// And minimal mode, from the same input, still omits the default — so the
	// difference is the flag rather than the fixture.
	min := string(oneFile(t, rs).Bytes)
	if strings.Contains(min, "size:") {
		t.Errorf("minimal mode emitted the default:\n%s", min)
	}
}

// TestTheOmissionNoteSuitsItsReader. A generated file is configuration someone
// must complete; an export is a record someone is reading. "TODO: set this" in
// an audit dump is an instruction to edit a file nobody will apply.
func TestTheOmissionNoteSuitsItsReader(t *testing.T) {
	rs := []Resource{{
		Name: "orders", Type: "fake.database", ProviderID: "db-9",
		Attributes: map[string]value.Value{
			"engine":   prov("postgres"),
			"password": prov("s3cret").WithSensitive(true),
		},
	}}

	min := string(oneFile(t, rs).Bytes)
	full, _, err := Generate(rs, testRegistry(t), Options{Minimal: false})
	if err != nil {
		t.Fatal(err)
	}
	exp := string(full[0].Bytes)

	if !strings.Contains(min, "TODO") {
		t.Errorf("a generated file must tell the reader to supply the secret:\n%s", min)
	}
	if strings.Contains(exp, "TODO") {
		t.Errorf("an export must not instruct the reader to edit it:\n%s", exp)
	}
	// Both must still NAME the attribute, whatever the wording.
	for label, out := range map[string]string{"generated": min, "export": exp} {
		if !strings.Contains(out, "password") {
			t.Errorf("the %s output does not name the omitted attribute:\n%s", label, out)
		}
	}
}

// TestTheOmissionNoteNamesTheSyntax.
//
// The note used to say "TODO: set password" and stop, which left the reader
// holding the one question the file could actually answer: set it to WHAT,
// when the value must not be written here? ${secret.NAME} is that answer and
// it arrived after the note did, so the note went on describing a gap it could
// have closed.
//
// The environment variable name is asserted as an EXAMPLE, not a rule. infrena
// imposes no convention — upper-casing the attribute is a plausible guess
// offered so the reader adapts it.
func TestTheOmissionNoteNamesTheSyntax(t *testing.T) {
	rs := []Resource{{
		Name: "orders", Type: "fake.database", ProviderID: "db-9",
		Attributes: map[string]value.Value{
			"engine":   prov("postgres"),
			"password": prov("s3cret").WithSensitive(true),
		},
	}}

	min := string(oneFile(t, rs).Bytes)
	if strings.Contains(min, "s3cret") {
		t.Fatalf("the secret itself was written into generated configuration:\n%s", min)
	}
	if !strings.Contains(min, "${secret.PASSWORD}") {
		t.Errorf("the note does not show how to supply the value, which is the only question "+
			"a reader has left:\n%s", min)
	}
}
