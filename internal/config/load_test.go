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
