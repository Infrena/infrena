package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitScaffoldsIntoInfrenaByDefault(t *testing.T) {
	dir := t.TempDir()

	if _, _, code := runCommandIn(t, dir, "init"); code != ExitOK {
		t.Fatalf("init exit = %d", code)
	}

	for _, want := range []string{
		"infrena/infra.yml",
		"infrena/resources/network.yml",
		"infrena/vars/default.yml",
		"infrena/vars/production.yml",
		"infrena/vars/staging.yml",
		"infrena/modules/.gitkeep",
		"infrena/.gitignore",
	} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Errorf("init did not create %s", want)
		}
	}
}

// The promise init has always made, and the one that matters most: a shipped
// infrena carries no provider, so the scaffold must validate on a machine
// with nothing installed. That is why the example resource is commented out.
func TestScaffoldValidatesWithNoPluginInstalled(t *testing.T) {
	dir := t.TempDir()
	runCommandIn(t, dir, "init")

	_, stderr, code := runCommandInWithoutPlugins(t, dir, "validate")

	if code != ExitOK {
		t.Errorf("scaffold does not validate bare: exit %d\n%s", code, stderr)
	}
}

// Environments must match the vars filenames, or vars/staging.yml applies to
// nothing and the one file a user reads to learn the language is dead
// configuration.
func TestScaffoldedEnvironmentsMatchTheVarsFiles(t *testing.T) {
	dir := t.TempDir()
	runCommandIn(t, dir, "init")

	body := read(t, filepath.Join(dir, "infrena", "infra.yml"))
	for _, env := range []string{"production", "staging"} {
		if !strings.Contains(body, env+":") {
			t.Errorf("infra.yml does not declare %s", env)
		}
	}
}

func TestInitTakesADirectoryArgument(t *testing.T) {
	dir := t.TempDir()

	runCommandIn(t, dir, "init", "myinfra")

	if _, err := os.Stat(filepath.Join(dir, "myinfra", "infra.yml")); err != nil {
		t.Errorf("init ignored its directory argument: %v", err)
	}
}

// A directory findProjectRoot does not discover must be told the flag, or the
// closing message is advice that does not work from where the user stands.
func TestClosingMessageCarriesChdirForAnUndiscoverableDirectory(t *testing.T) {
	dir := t.TempDir()

	stdout, _, _ := runCommandIn(t, dir, "init", "myinfra")

	if !strings.Contains(stdout, "--chdir myinfra") {
		t.Errorf("closing message omits the flag:\n%s", stdout)
	}
}

// The other half: ./infrena and . ARE discovered, so the flag would be noise
// there. Without this, a message that carried --chdir unconditionally would
// pass the test above and still be wrong in the default case.
func TestClosingMessageOmitsChdirForADiscoverableDirectory(t *testing.T) {
	dir := t.TempDir()

	stdout, _, _ := runCommandIn(t, dir, "init")

	if strings.Contains(stdout, "--chdir") {
		t.Errorf("closing message names a flag the user does not need:\n%s", stdout)
	}
	if !strings.Contains(stdout, "infrena plan staging") {
		t.Errorf("closing message does not name the next command:\n%s", stdout)
	}
}

func TestInitWithProviderAWSScaffoldsARealProvidersBlock(t *testing.T) {
	dir := t.TempDir()

	runCommandIn(t, dir, "init", "--provider", "aws")

	body := read(t, filepath.Join(dir, "infrena", "infra.yml"))
	if !strings.Contains(body, "plugin: aws") {
		t.Errorf("no aws provider instance:\n%s", body)
	}
}

// Naming a provider is the user saying they have one, so the example resource
// stops being a comment. Without this the flag could write a providers block
// and leave the example commented out, which teaches nothing new.
func TestInitWithProviderScaffoldsALiveExampleResource(t *testing.T) {
	dir := t.TempDir()

	runCommandIn(t, dir, "init", "--provider", "aws")

	body := read(t, filepath.Join(dir, "infrena", "resources", "network.yml"))
	if !strings.Contains(body, "\nresources:") {
		t.Errorf("the example resource is still commented out:\n%s", body)
	}
	if !strings.Contains(body, "type: aws.vpc") {
		t.Errorf("the example does not use an aws type:\n%s", body)
	}
}

// Refusing matters more here than anywhere else in the CLI, and it is checked
// before anything is written so a refusal leaves the directory as it was.
func TestInitRefusesToOverwriteAndWritesNothing(t *testing.T) {
	dir := t.TempDir()
	runCommandIn(t, dir, "init")
	before := read(t, filepath.Join(dir, "infrena", "infra.yml"))
	if err := os.Remove(filepath.Join(dir, "infrena", "vars", "staging.yml")); err != nil {
		t.Fatal(err)
	}

	_, stderr, code := runCommandIn(t, dir, "init")

	if code == ExitOK {
		t.Error("init overwrote an existing project")
	}
	if !strings.Contains(stderr, "already exists") {
		t.Errorf("refusal does not say why:\n%s", stderr)
	}
	if read(t, filepath.Join(dir, "infrena", "infra.yml")) != before {
		t.Error("init modified infra.yml before refusing")
	}
	// The half-scaffolded case: the file removed above must NOT come back.
	if _, err := os.Stat(filepath.Join(dir, "infrena", "vars", "staging.yml")); err == nil {
		t.Error("init wrote a file despite refusing")
	}
}

// TestInitRefusesWhenOnlyALaterFileExists, not just infra.yml. The scaffold is
// written in sorted order and infra.yml is not last, so a conflict on a file
// that sorts AFTER it is the only shape that can tell a check-first
// implementation from a check-as-you-go one.
func TestInitRefusesWhenOnlyALaterFileExists(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "infrena")
	write(t, filepath.Join(target, "vars", "staging.yml"), "{}\n")

	if _, err := scaffold(target, ""); err == nil {
		t.Fatal("init wrote over a directory that already held one of its files")
	}
	if _, err := os.Stat(filepath.Join(target, "infra.yml")); err == nil {
		t.Error("a refused init left infra.yml behind: the existence check must run over EVERY " +
			"file before ANY is written, or a refusal half-scaffolds the directory")
	}
}

// TestInitScaffoldsAVersionFloor. PLAN.md §61.2: the key turns "unknown key"
// into "this project needs infrena >= X; this is Y" for the NEXT break, which
// is the one nobody will remember to prepare for.
func TestInitScaffoldsAVersionFloor(t *testing.T) {
	dir := t.TempDir()
	if _, err := scaffold(dir, ""); err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	if !strings.Contains(read(t, filepath.Join(dir, "infra.yml")), "infrena:") {
		t.Error("infra.yml must declare a version floor")
	}
}

// TestInitIsDeterministic. The file list is reported to the user and the order
// must not come from Go's map iteration.
func TestInitIsDeterministic(t *testing.T) {
	var first []string
	for i := range 20 {
		got, err := scaffold(t.TempDir(), "")
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = got
			continue
		}
		if strings.Join(got, ",") != strings.Join(first, ",") {
			t.Fatalf("run %d listed %v, want %v", i, got, first)
		}
	}
}
