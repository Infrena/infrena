package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// write creates a file and every directory above it, because the fixtures in
// this package are mostly about a file's PLACE rather than its content and
// spelling out MkdirAll at each one buries that.
func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFindProjectRootPrefersTheClosestProject(t *testing.T) {
	// A directory holding both is a project at its root that also happens to
	// have a directory called infrena. The root is the answer.
	dir := t.TempDir()
	write(t, filepath.Join(dir, "infra.yml"), "project: outer\n")
	write(t, filepath.Join(dir, "infrena", "infra.yml"), "project: inner\n")

	got, err := findProjectRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != dir {
		t.Errorf("findProjectRoot = %q, want %q", got, dir)
	}
}

func TestFindProjectRootDescendsIntoInfrena(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "infrena", "infra.yml"), "project: p\n")

	got, err := findProjectRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(dir, "infrena") {
		t.Errorf("findProjectRoot = %q, want the infrena subdirectory", got)
	}
}

// Not walking up is deliberate: a command run in a subdirectory quietly
// mutating a project the user did not realise they were in is the wrong
// place for that kind of convenience.
func TestFindProjectRootDoesNotWalkUp(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "infra.yml"), "project: p\n")
	sub := filepath.Join(dir, "deep", "nested")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := findProjectRoot(sub); err == nil {
		t.Fatal("findProjectRoot walked up to a parent project")
	}
}

// Section 44: an error says what was expected and what to do.
func TestFindProjectRootNamesBothPlacesItLooked(t *testing.T) {
	_, err := findProjectRoot(t.TempDir())
	if err == nil {
		t.Fatal("findProjectRoot accepted a directory with no project")
	}
	for _, want := range []string{"infra.yml", "infrena/infra.yml", "infrena init"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

// An explicit --chdir means what it says. A user who named a directory has
// already answered the question findProjectRoot exists to ask, so the search
// must not run and quietly relocate the command into ./infrena.
func TestExplicitChdirSkipsTheSearch(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "infrena", "infra.yml"), "project: p\n")

	// runCommand always passes --chdir, which is exactly the case under test.
	_, stderr, code := runCommand(t, dir, "validate")

	if code == ExitOK {
		t.Fatal("--chdir was overridden by the project search")
	}
	if !strings.Contains(stderr, "infra.yml") {
		t.Errorf("the failure does not name the file it wanted:\n%s", stderr)
	}
}
