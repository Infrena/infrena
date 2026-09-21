package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// declaredProviderProject writes a project whose `providers:` block names one
// key, so a test can ask whether that key reached the plugin.
//
// `cloud:` is the fake provider's stand-in for an ACCOUNT — see its New — so a
// project that names one and a plugin that never hears about it is exactly the
// shape of the defect these tests exist for: infrena read a different AWS
// account from the one `profile:` named, silently, and proposed creating 64
// resources that already existed.
func declaredProviderProject(t *testing.T, providerKey, providerValue string) string {
	t.Helper()
	return projectDir(t, `
project: myapp
providers:
  - plugin: fake
    `+providerKey+`: `+providerValue+`
resources:
  network:
    type: fake.network
    cidr: 10.20.0.0/16
`)
}

// THE REGRESSION TEST FOR THE WRONG-ACCOUNT BUG, in the form the user met it:
// apply, then plan, and the plan proposes rebuilding what was just built.
//
// STATE IS THE PRECONDITION and the reason this was never caught. A project
// with nothing in state loads no plugin before compiling, so the implicit
// instance cannot be derived and the declared one registers normally. Once
// state names a resource type, ensureStateProviders loads that plugin early,
// derives a CONFIG-LESS implicit instance from it, and the declared instance
// is then skipped as a name already taken — so every provider call runs
// against the default account.
func TestPlanAfterApplyIsANoOpWhenTheProvidersBlockNamesAnAccount(t *testing.T) {
	dir := declaredProviderProject(t, "cloud", ".infrena/declared-cloud.json")

	if _, stderr, code := runCommand(t, dir, "apply", "dev", "--auto-approve"); code == ExitError {
		t.Fatalf("apply failed: %s", stderr)
	}

	// The second command is the one that used to lie: state now names
	// fake.network, so the plugin is loaded before the compiler runs.
	stdout, stderr, code := runCommand(t, dir, "plan", "dev")

	if code == ExitChanges {
		t.Errorf("plan proposes changes to infrastructure it just created — it read a different account:\n%s%s", stdout, stderr)
	}
}

// The mechanism, asserted directly: the same `providers:` block must reach the
// plugin from every command that dispatches to one. The fake refuses a key it
// does not understand, so a command that reports nothing is a command that
// handed the plugin nothing.
//
// `validate` is the control. It refused the key correctly while plan, apply,
// refresh and destroy accepted it in silence, and that disagreement is what
// proved the configuration never left the host.
func TestEveryCommandPassesDeclaredProviderConfigToThePlugin(t *testing.T) {
	for _, command := range []string{"validate", "plan", "apply", "refresh", "destroy"} {
		t.Run(command, func(t *testing.T) {
			dir := declaredProviderProject(t, "cloud", ".infrena/declared-cloud.json")
			// State first, for the reason the test above documents: without it
			// the implicit instance is never derived and the bug cannot appear.
			if _, stderr, code := runCommand(t, dir, "apply", "dev", "--auto-approve"); code == ExitError {
				t.Fatalf("apply failed: %s", stderr)
			}
			misspell(t, dir)

			_, stderr, _ := runCommandWithStdin(t, dir, "dev\n", command, "dev", "--auto-approve")

			if !strings.Contains(stderr, "clowd") {
				t.Errorf("%s did not report the misspelled provider key, so the `providers:` block never reached the plugin:\n%s", command, stderr)
			}
		})
	}
}

// misspell breaks the provider key after state exists, so the plugin has
// something to refuse.
func misspell(t *testing.T, dir string) {
	t.Helper()
	path := filepath.Join(dir, "infrena.yml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	broken := strings.Replace(string(body), "    cloud:", "    clowd:", 1)
	if broken == string(body) {
		t.Fatal("fixture no longer contains the key this test rewrites")
	}
	if err := os.WriteFile(path, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Not merely that the key arrives, but that its VALUE decides which account is
// touched: with the declared `cloud:` dropped, an instance falls back to the
// default path — the fake's equivalent of falling back to the default AWS
// credential chain.
func TestApplyWritesToTheCloudTheProvidersBlockNames(t *testing.T) {
	dir := declaredProviderProject(t, "cloud", ".infrena/declared-cloud.json")

	if _, stderr, code := runCommand(t, dir, "apply", "dev", "--auto-approve"); code == ExitError {
		t.Fatalf("apply failed: %s", stderr)
	}
	// A second apply, so state exists for the one that matters.
	if _, stderr, code := runCommand(t, dir, "apply", "dev", "--auto-approve"); code == ExitError {
		t.Fatalf("second apply failed: %s", stderr)
	}

	if _, err := os.Stat(filepath.Join(dir, ".infrena", "declared-cloud.json")); err != nil {
		t.Errorf("nothing was written to the cloud the project declared: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".infrena", "fake-cloud.json")); err == nil {
		t.Error("apply wrote to the DEFAULT cloud, so the declared `providers:` configuration was discarded")
	}
}

// NEITHER COMMAND SAID ANYTHING WHILE IT WORKED. A discover against a real
// AWS account sweeps 958 types and an import then reads every resource it
// adopts, one provider call each — minutes of silence, which reads as a hang.
// Both now report, and the renderer's heartbeat covers the gaps.
func TestDiscoverSaysWhatItIsAsking(t *testing.T) {
	dir := newProjectFixture(t)

	stdout, _, _ := runCommand(t, dir, "discover")

	if !strings.Contains(stdout, "Asking fake what exists") {
		t.Errorf("discover ran silently:\n%s", stdout)
	}
	if !strings.Contains(stdout, "resources found") {
		t.Errorf("discover never said what it found:\n%s", stdout)
	}
}

func TestImportReportsEachResourceItAdopts(t *testing.T) {
	// A resource that exists in the cloud but in no project's state, which is
	// what there is to adopt. Built by applying in one project and pointing a
	// second at the same cloud file.
	source := newProjectFixture(t)
	if _, stderr, code := runCommand(t, source, "apply", "dev", "--auto-approve"); code == ExitError {
		t.Fatalf("apply failed: %s", stderr)
	}
	cloud, err := os.ReadFile(filepath.Join(source, ".infrena", "fake-cloud.json"))
	if err != nil {
		t.Fatal(err)
	}
	adopter := projectDir(t, "project: adopter\nenvironments:\n  dev: {}\n")
	if err := os.MkdirAll(filepath.Join(adopter, ".infrena"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(adopter, ".infrena", "fake-cloud.json"), cloud, 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := runCommand(t, adopter, "import", "dev", "fake.network.net-1", "--generate")
	if code == ExitError {
		t.Fatalf("import failed: %s%s", stdout, stderr)
	}

	if !strings.Contains(stdout, "Asking fake what exists") {
		t.Errorf("import's discovery phase ran silently:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Reading ") {
		t.Errorf("import adopted a resource without saying so:\n%s", stdout)
	}
}
