package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTree writes files into a fresh temp dir. Keys are slash-separated paths
// relative to the dir.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range files {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	return dir
}

const minimalProject = "project: demo\nresources: {}\n"

// TestLoadOrderIsFixedAndNotTheFilesystemOrder pins ruling 5.
//
// The fixture is built so that the natural orders CONTRADICT the required
// order, which is the only way an ordering assertion can fail against broken
// code:
//
//   - "aaa" and "bbb" sort before "infra.yml" as full paths
//     ("<dir>/environments/aaa.yml" < "<dir>/infra.yml"), so a global path
//     sort puts the environments first and fails here.
//   - the environments are created zulu, aaa, bbb, so creation order fails.
//   - "variables.yml" sorts after all of them, so a global path sort also
//     puts it last and fails here.
func TestLoadOrderIsFixedAndNotTheFilesystemOrder(t *testing.T) {
	dir := t.TempDir()
	writeFile := func(rel, body string) {
		t.Helper()
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("environments/zulu.yml", "replicas: 1\n")
	writeFile("environments/aaa.yml", "replicas: 2\n")
	writeFile("environments/bbb.yml", "replicas: 3\n")
	writeFile("variables.yml", "region: us-east-1\n")
	writeFile(ProjectFileName, minimalProject)

	files, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := []string{
		ProjectFileName,
		VariablesFileName,
		filepath.Join(EnvironmentsDirName, "aaa.yml"),
		filepath.Join(EnvironmentsDirName, "bbb.yml"),
		filepath.Join(EnvironmentsDirName, "zulu.yml"),
	}
	if len(files) != len(want) {
		t.Fatalf("Load returned %d files, want %d: %v", len(files), len(want), pathsOf(files))
	}
	for i, w := range want {
		if got := filepath.Join(dir, w); files[i].Path != got {
			t.Errorf("files[%d].Path = %s, want %s", i, files[i].Path, got)
		}
	}
}

func pathsOf(files []File) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.Path
	}
	return out
}

// TestLoadTagsEachFileWithItsKind pins that Task 3 can tell the three shapes
// apart. Without Kind, stage 2 would have to guess from the path, and a flat
// variables.yml decoded as a project document produces "unrecognised top-level
// key" for every variable in it.
func TestLoadTagsEachFileWithItsKind(t *testing.T) {
	dir := writeTree(t, map[string]string{
		ProjectFileName:               minimalProject,
		VariablesFileName:             "region: us-east-1\n",
		"environments/production.yml": "replicas: 10\n",
	})
	files, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(files) != 3 {
		t.Fatalf("got %d files, want 3: %v", len(files), pathsOf(files))
	}
	if files[0].Kind != FileProject || files[0].Environment != "" {
		t.Errorf("infra.yml: Kind=%v Environment=%q", files[0].Kind, files[0].Environment)
	}
	if files[1].Kind != FileVariables || files[1].Environment != "" {
		t.Errorf("variables.yml: Kind=%v Environment=%q", files[1].Kind, files[1].Environment)
	}
	if files[2].Kind != FileEnvironment || files[2].Environment != "production" {
		t.Errorf("environments/production.yml: Kind=%v Environment=%q", files[2].Kind, files[2].Environment)
	}
	for i, f := range files {
		if f.Root == nil {
			t.Errorf("files[%d] (%s) has a nil Root", i, f.Path)
		}
	}
}

// TestLoadTreatsTheOptionalFilesAsOptional pins rulings 1, 2, 3 and 7. Each
// case must return exactly the project file and no error.
func TestLoadTreatsTheOptionalFilesAsOptional(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		dirs  []string
	}{
		{
			name:  "neither present",
			files: map[string]string{ProjectFileName: minimalProject},
		},
		{
			name:  "environments dir exists but is empty",
			files: map[string]string{ProjectFileName: minimalProject},
			dirs:  []string{EnvironmentsDirName},
		},
		{
			name: "environments dir holds only non-YAML",
			files: map[string]string{
				ProjectFileName:          minimalProject,
				"environments/README.md": "notes\n",
				"environments/.gitkeep":  "",
				"environments/notes.txt": "x\n",
			},
		},
		{
			name: "environments dir holds a subdirectory with no YAML extension",
			files: map[string]string{
				ProjectFileName:             minimalProject,
				"environments/old/prod.yml": "replicas: 1\n",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeTree(t, tc.files)
			for _, d := range tc.dirs {
				if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			files, err := Load(dir)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if len(files) != 1 || files[0].Kind != FileProject {
				t.Fatalf("got %v, want just the project file", pathsOf(files))
			}
		})
	}
}

// TestLoadSkipsADirectoryNamedLikeAnEnvironmentFile pins the IsDir guard in
// loadEnvironmentDir specifically — the "subdirectory" case above names its
// subdirectory "old", which has no YAML extension and so is already rejected
// by environmentNameFor's ok check before the IsDir guard would ever matter.
// That case would keep passing with the IsDir guard deleted entirely.
//
// A directory whose name DOES end in .yml is the case the guard exists for: a
// typo, a `mkdir` where `touch` was meant, or a bad merge. Without the guard,
// environmentNameFor("staging.yml") accepts it, and loadOptionalFile calls
// os.ReadFile on a directory, which fails with EISDIR — not os.IsNotExist —
// and propagates out of Load as a raw OS error instead of being silently
// skipped per ruling 7.
//
// staging2.yml sits alongside it so that a mutant which skips everything in
// environments/ (not just directories) also fails this test.
func TestLoadSkipsADirectoryNamedLikeAnEnvironmentFile(t *testing.T) {
	dir := writeTree(t, map[string]string{
		ProjectFileName:             minimalProject,
		"environments/staging2.yml": "replicas: 2\n",
	})
	if err := os.MkdirAll(filepath.Join(dir, "environments", "staging.yml"), 0o755); err != nil {
		t.Fatal(err)
	}

	files, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var envs []string
	for _, f := range files {
		if f.Kind == FileEnvironment {
			envs = append(envs, f.Environment)
		}
	}
	if len(envs) != 1 || envs[0] != "staging2" {
		t.Fatalf("environments = %v, want [staging2] (the directory environments/staging.yml/ must be skipped, not read)", envs)
	}
}

// TestLoadAcceptsBothYAMLSpellings is the other half of ruling 6: .yaml must
// work, not merely fail loudly when doubled.
func TestLoadAcceptsBothYAMLSpellings(t *testing.T) {
	dir := writeTree(t, map[string]string{
		ProjectFileName:            minimalProject,
		"environments/dev.yaml":    "replicas: 1\n",
		"environments/staging.yml": "replicas: 2\n",
	})
	files, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var envs []string
	for _, f := range files {
		if f.Kind == FileEnvironment {
			envs = append(envs, f.Environment)
		}
	}
	if len(envs) != 2 || envs[0] != "dev" || envs[1] != "staging" {
		t.Fatalf("environments = %v, want [dev staging]", envs)
	}
}

// TestLoadRefusesAnAmbiguousEnvironment pins ruling 6's error half. Picking one
// silently would drop the other file's overrides from every plan, with nothing
// printed.
func TestLoadRefusesAnAmbiguousEnvironment(t *testing.T) {
	dir := writeTree(t, map[string]string{
		ProjectFileName:                minimalProject,
		"environments/production.yml":  "replicas: 10\n",
		"environments/production.yaml": "replicas: 99\n",
	})
	_, err := Load(dir)
	if err == nil {
		t.Fatal("Load accepted production.yml and production.yaml together; one would silently shadow the other")
	}
	for _, want := range []string{"production", "production.yml", "production.yaml"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %q: %v", want, err)
		}
	}
}

// TestLoadReportsMalformedOptionalFiles pins ruling 4 for both new shapes.
func TestLoadReportsMalformedOptionalFiles(t *testing.T) {
	cases := []struct{ name, path string }{
		{"variables.yml", VariablesFileName},
		{"environment file", "environments/production.yml"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeTree(t, map[string]string{
				ProjectFileName: minimalProject,
				tc.path:         "replicas: [1, 2\n",
			})
			_, err := Load(dir)
			if err == nil {
				t.Fatal("Load accepted malformed YAML")
			}
			if !strings.Contains(err.Error(), filepath.Base(tc.path)) {
				t.Errorf("error does not name the file: %v", err)
			}
		})
	}
}

// TestLoadStillDemandsTheProjectFile is the regression guard for M1's
// behaviour: making three files optional must not make the fourth one optional
// too. An empty dir returning zero files and no error would compile a config
// with no resources, and invariant 1 reads that as "everything was removed" —
// a plan destroying every managed resource.
func TestLoadStillDemandsTheProjectFile(t *testing.T) {
	dir := writeTree(t, map[string]string{
		VariablesFileName: "region: us-east-1\n",
	})
	_, err := Load(dir)
	if err == nil {
		t.Fatal("Load accepted a directory with no infra.yml")
	}
	if !strings.Contains(err.Error(), "infra init") {
		t.Errorf("error should still suggest `infra init`: %v", err)
	}
}

// TestLoadSkipsAFileNamedJustAnExtension pins environmentNameFor's name == ""
// guard. Without it, a file literally named ".yml" or ".yaml" strips down to
// an empty environment name, which then flows into paths[""], the sorted
// name list, and out of Load as a File with Environment: "" — an environment
// stage 2 would be asked to decode with no name at all.
//
// dev.yml sits alongside it for the same reason staging2.yml did in
// TestLoadSkipsADirectoryNamedLikeAnEnvironmentFile: without a real file
// present, a mutant that skips everything in environments/ would also pass.
func TestLoadSkipsAFileNamedJustAnExtension(t *testing.T) {
	dir := writeTree(t, map[string]string{
		ProjectFileName:        minimalProject,
		"environments/.yml":    "replicas: 1\n",
		"environments/dev.yml": "replicas: 2\n",
	})
	files, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var envs []string
	for _, f := range files {
		if f.Kind == FileEnvironment {
			envs = append(envs, f.Environment)
		}
	}
	if len(envs) != 1 || envs[0] != "dev" {
		t.Fatalf("environments = %v, want [dev] (environments/.yml names no environment and must be skipped)", envs)
	}
}

// TestLoadReportsEnvironmentsAsAPlainFile pins spec §44 for the case where
// environments/ exists but is a plain file rather than a directory — a typo,
// a `touch` where `mkdir` was meant, or a bad merge. os.ReadDir returns
// ENOTDIR for this, which is not os.IsNotExist, so without special handling
// it propagates as a bare OS error ("open .../environments: not a
// directory") naming the path but neither the expectation nor a suggested
// action, the same silent-shape §44 already refuses for the ambiguous-spelling
// error.
func TestLoadReportsEnvironmentsAsAPlainFile(t *testing.T) {
	dir := writeTree(t, map[string]string{ProjectFileName: minimalProject})
	envPath := filepath.Join(dir, EnvironmentsDirName)
	if err := os.WriteFile(envPath, []byte("not a directory\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Load(dir)
	if err == nil {
		t.Fatal("Load accepted environments/ as a plain file")
	}
	if !strings.Contains(err.Error(), envPath) {
		t.Errorf("error does not name the path: %v", err)
	}
	if !strings.Contains(err.Error(), "directory") {
		t.Errorf("error does not say what was expected (a directory): %v", err)
	}
	// The raw ENOTDIR message already contains the path and the word
	// "directory" ("open .../environments: not a directory"), so those two
	// checks alone would pass against the unhandled error. This one only
	// passes once the error carries an actual suggested action.
	if !strings.Contains(err.Error(), "Remove") && !strings.Contains(err.Error(), "remove") {
		t.Errorf("error does not suggest an action: %v", err)
	}
}

// loadedBase reports whether a file with the given base name was loaded.
func loadedBase(files []File, base string) bool {
	for _, f := range files {
		if filepath.Base(f.Path) == base {
			return true
		}
	}
	return false
}

// TestResourcesDirectoryIsLoaded. §4.1: a resource declared under resources/ is as real as one
// in infra.yml, and the two forms coexist — the directory is how a project is ORGANISED, not a
// replacement for the inline block.
func TestResourcesDirectoryIsLoaded(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"infra.yml":                       "project: p\nresources:\n  inline:\n    type: test.network\n    cidr: 10.0.0.0/16\n",
		"resources/database/database.yml": "resources:\n  store:\n    type: test.database\n    engine: postgres\n",
	})
	files, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// BOTH: a loader that replaced infra.yml's resources rather than adding to them would
	// pass a test that only looked for database.yml.
	if !loadedBase(files, "database.yml") {
		t.Error("resources/** was not loaded")
	}
	if !loadedBase(files, "infra.yml") {
		t.Error("infra.yml stopped being read")
	}

	// Loading is NOT the feature. A file can be read and then decoded by
	// nothing, which is how this first shipped: `validate` reported the project
	// valid because it had validated an empty resource set. Assert the resource
	// is DECLARED, which is what a plan would act on.
	decl, ds := Decode(files)
	if ds.HasErrors() {
		t.Fatalf("decode: %+v", ds)
	}
	var names []string
	for _, r := range decl.Resources {
		names = append(names, r.Name)
	}
	if strings.Join(names, ",") != "inline,store" {
		t.Errorf("declared resources = %v, want both inline and store — a file that is loaded "+
			"and decoded by nothing produces a valid project with nothing in it", names)
	}
}

// TestNestedResourceDirectoriesAreLoaded — `resources/**`, not `resources/*`. A project
// organised by region or team nests, and §4.1 says so.
func TestNestedResourceDirectoriesAreLoaded(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"infra.yml":                 "project: p\n",
		"resources/eu/west/net.yml": "resources:\n  n:\n    type: test.network\n    cidr: 10.0.0.0/16\n",
	})
	files, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !loadedBase(files, "net.yml") {
		t.Error("a nested resource file was not loaded; resources/** must recurse")
	}
	decl, ds := Decode(files)
	if ds.HasErrors() || len(decl.Resources) != 1 {
		t.Errorf("nested file did not decode: %+v %+v", decl.Resources, ds)
	}
}

// TestVarsDirectoryIsLoaded.
func TestVarsDirectoryIsLoaded(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"infra.yml":           "project: p\n",
		"vars/default.yml":    "size: 50\n",
		"vars/production.yml": "size: 100\n",
	})
	files, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, base := range []string{"default.yml", "production.yml"} {
		if !loadedBase(files, base) {
			t.Errorf("vars/%s was not loaded", base)
		}
	}
}

// TestDiscoveredDirectoryIsLoaded. §27.1: import adds a resource to state, and a resource in
// state that no configuration declares is scheduled for destruction by invariant 1 — so the
// generated file is part of the project from the moment it is written.
func TestDiscoveredDirectoryIsLoaded(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"infra.yml":                "project: p\n",
		"discovered/databases.yml": "resources:\n  imported:\n    type: test.database\n    engine: postgres\n",
	})
	files, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !loadedBase(files, "databases.yml") {
		t.Error("discovered/** was not loaded; an imported resource would be scheduled for destruction")
	}
	decl, ds := Decode(files)
	if ds.HasErrors() {
		t.Fatalf("decode: %+v", ds)
	}
	if len(decl.Resources) != 1 || decl.Resources[0].Name != "imported" {
		t.Errorf("declared = %+v, want the imported resource; loading it without decoding it "+
			"leaves state and configuration disagreeing, which invariant 1 resolves by destroying", decl.Resources)
	}
}

// TestConventionalDirectoriesAreSortedOnce. The fixture declares zeta before alpha so a
// missing sort shows rather than passing by luck, and runs 20 times because map and readdir
// order are not stable.
func TestConventionalDirectoriesAreSortedOnce(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"infra.yml":           "project: p\n",
		"resources/zeta.yml":  "resources:\n  z:\n    type: test.network\n    cidr: 10.1.0.0/16\n",
		"resources/alpha.yml": "resources:\n  a:\n    type: test.network\n    cidr: 10.2.0.0/16\n",
		"resources/mid.yml":   "resources:\n  m:\n    type: test.network\n    cidr: 10.3.0.0/16\n",
	})
	var first string
	for i := 0; i < 20; i++ {
		files, err := Load(dir)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, f := range files {
			if f.Kind == FileResources {
				names = append(names, filepath.Base(f.Path))
			}
		}
		got := strings.Join(names, ",")
		if i == 0 {
			first = got
			continue
		}
		if got != first {
			t.Fatalf("run %d loaded %s, want %s", i, got, first)
		}
	}
	if first != "alpha.yml,mid.yml,zeta.yml" {
		t.Errorf("conventional files are not sorted: %s", first)
	}
}

// TestAMissingConventionalDirectoryIsFine — most projects use none of them.
func TestAMissingConventionalDirectoryIsFine(t *testing.T) {
	dir := writeTree(t, map[string]string{"infra.yml": "project: p\n"})
	if _, err := Load(dir); err != nil {
		t.Errorf("a project with no conventional directories must load: %v", err)
	}
}

// TestANonYamlFileInAConventionalDirectoryIsIgnored. A README, a .gitkeep or an editor backup
// is an ordinary thing to find in a repository, and refusing to load a project because someone
// left notes in it is hostile.
func TestANonYamlFileInAConventionalDirectoryIsIgnored(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"infra.yml":             "project: p\n",
		"resources/README.md":   "# notes\n",
		"resources/.gitkeep":    "",
		"resources/net.yml.bak": "resources:\n  bad: {}\n",
		"resources/net.yml":     "resources:\n  n:\n    type: test.network\n    cidr: 10.0.0.0/16\n",
	})
	files, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !loadedBase(files, "net.yml") {
		t.Error("the real file was not loaded")
	}
	for _, base := range []string{"README.md", ".gitkeep", "net.yml.bak"} {
		if loadedBase(files, base) {
			t.Errorf("%s was loaded; only .yml files are configuration", base)
		}
	}
}
