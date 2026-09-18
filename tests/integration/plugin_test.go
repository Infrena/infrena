package integration

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// THE REAL PLUGIN, not an in-process double. PLAN.md §31.1.
//
// A shipped infrena carries no provider, so the binary this suite runs has none either —
// which is the whole point of the cutover. Every other suite in this repository registers
// the fake provider in process; this one builds `infrena-plugin-fake` from its own
// repository and puts it on the search path, so the path a USER takes is proved somewhere:
// a subprocess, a handshake, a cookie, stderr forwarding, and the host's trust rules over
// a real pipe.
//
// That is also the only place the SDK's `os.Stdout` redirect is observable at all — in
// process, Serve writes to the pipe it is given and a plugin's stray Println goes
// somewhere harmless.

// pluginRepoEnv lets a checkout elsewhere be named explicitly.
const pluginRepoEnv = "INFRENA_PLUGIN_FAKE_REPO"

// requireEnv turns the skip below into a FAILURE.
//
// CI sets it. A continuous integration run that silently skips its whole integration suite
// is worse than one with no integration suite at all: it reports green for a suite that
// never executed, and the day the plugin checkout breaks is the day nobody notices. A
// contributor without the plugin cloned still gets a skip, because that is a setup problem
// rather than a defect.
const requireEnv = "INFRENA_REQUIRE_PLUGIN"

var (
	pluginOnce sync.Once
	pluginDir  string
	pluginErr  error
)

// fakePluginDir returns a directory holding a freshly built infrena-plugin-fake, for
// passing to --plugin-dir.
//
// SKIPS rather than fails when the repository is absent: this suite's other reason to
// exist is being runnable, and a contributor who has not cloned the plugin should get a
// clear skip rather than a hundred confusing failures. It is not silent — the skip names
// the directory it looked in and the variable that overrides it.
func fakePluginDir(t *testing.T) string {
	t.Helper()
	pluginOnce.Do(buildFakePlugin)
	if pluginErr == nil {
		return pluginDir
	}
	message := fmt.Sprintf("infrena-plugin-fake is not available, so this suite cannot run: %v\n"+
		"Clone github.com/infrena/infrena-provider-fake beside this repository, or set %s.",
		pluginErr, pluginRepoEnv)
	if os.Getenv(requireEnv) != "" {
		t.Fatalf("%s\n%s is set, so this is a failure rather than a skip.", message, requireEnv)
	}
	t.Skip(message)
	return ""
}

func buildFakePlugin() {
	wd, err := os.Getwd()
	if err != nil {
		pluginErr = err
		return
	}
	// wd is <root>/tests/integration, so two levels up is this repository's root and
	// three is the directory holding it, whose sibling is the plugin. Getting this
	// wrong is how the first version of this looked inside the repo itself — which the
	// skip message named, and is why it names it.
	//
	// root is needed whether or not the repo path was overridden, because the
	// workspace below joins the plugin to THIS checkout either way.
	root := filepath.Dir(filepath.Dir(wd))

	repo := os.Getenv(pluginRepoEnv)
	if repo == "" {
		repo = filepath.Join(filepath.Dir(root), "infrena-provider-fake")
	}
	if _, err := os.Stat(filepath.Join(repo, "go.mod")); err != nil {
		pluginErr = fmt.Errorf("no Go module at %s", repo)
		return
	}

	out, err := os.MkdirTemp("", "infrena-plugin-*")
	if err != nil {
		pluginErr = err
		return
	}
	// The binary NAME is how the host finds it (§31.1), so this is not arbitrary.
	bin := filepath.Join(out, "infrena-plugin-fake")

	// A WORKSPACE JOINING THE PLUGIN TO THIS CHECKOUT, and it is load-bearing twice.
	//
	// FIRST, it is what makes these tests test THIS infrena. The plugin's go.mod
	// requires a released infrena, so a plain build here would link the plugin
	// against whatever version that names and the integration suite would exercise
	// the pairing of a working-tree host with a released SDK while reporting on the
	// host under test. A protocol change would then be tested against the protocol
	// it replaced.
	//
	// SECOND, it is what keeps this buildable without git credentials. infrena is a
	// private module (PLAN.md §31.1), so resolving that require means an
	// authenticated fetch; supplied by the workspace, it is never fetched at all.
	// Only ordinary public dependencies still go to the proxy.
	//
	// This used to come for free from `replace ... => ../infrena` in the plugin's own
	// go.mod, which is why CI checks the plugin out at a fixed path beside this repo.
	// That replace was removed on 2026-09-17 when the plugin moved to a gitignored
	// go.work of its own, and this build broke in CI the moment it merged — the
	// private fetch, with no credentials on that step. Declaring the pairing HERE is
	// the durable fix: this suite now states which infrena the plugin is built
	// against instead of inheriting it from another repository's go.mod, where a
	// change nobody here reviews can silently repoint it.
	work := filepath.Join(out, "go.work")
	if err := os.WriteFile(work, []byte(workspaceFor(root, repo)), 0o644); err != nil {
		pluginErr = err
		return
	}

	cmd := exec.Command("go", "build", "-o", bin, "./cmd/infrena-plugin-fake")
	// Built from the PLUGIN's module, not this one: it is a separate module with its own
	// go.mod, and `go build` of a path outside the main module is refused.
	cmd.Dir = repo
	// Explicit, so a go.work sitting in either checkout cannot change what is built.
	cmd.Env = append(os.Environ(), "GOWORK="+work)
	if combined, err := cmd.CombinedOutput(); err != nil {
		pluginErr = fmt.Errorf("building infrena-plugin-fake in %s: %v\n%s", repo, err, combined)
		return
	}
	pluginDir = out
}

// workspaceFor builds the go.work joining this checkout to the plugin's.
//
// The `go` directive is read from this repository's own go.mod rather than
// written as a literal: a workspace declaring a version below either module's
// is refused outright, so a hardcoded one turns the next toolchain bump into a
// failure in a file nobody would think to look in.
func workspaceFor(root, repo string) string {
	version := "1.27.0"
	if data, err := os.ReadFile(filepath.Join(root, "go.mod")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "go "); ok {
				version = strings.TrimSpace(rest)
				break
			}
		}
	}
	return "go " + version + "\n\nuse (\n\t" + repo + "\n\t" + root + "\n)\n"
}

// TestAShippedBuildCarriesNoProvider is the cutover's governing claim, and the only test
// that can make it: every in-process suite injects the fake double, so a build that
// accidentally regained a compiled-in provider would pass all of them.
//
// It runs the built binary WITHOUT --plugin-dir, which is what a user has before installing
// anything. Sabotage-checked: restoring the builtin makes this say "Configuration valid".
func TestAShippedBuildCarriesNoProvider(t *testing.T) {
	dir := t.TempDir()
	// The project is written here rather than scaffolded. This used to lean on `init`,
	// whose example declared a live fake.network — but the scaffold's example is now
	// COMMENTED OUT, precisely so that a fresh project validates on a machine with
	// nothing installed, so it can no longer be the thing that demands a plugin.
	// TestTheScaffoldValidatesWithNoProviderInstalled is that other half.
	body := []byte("project: shipped\nresources:\n  network:\n    type: fake.network\n    cidr: 10.0.0.0/16\n")
	if err := os.WriteFile(filepath.Join(dir, "infra.yml"), body, 0o644); err != nil {
		t.Fatal(err)
	}

	r := runWithoutPlugin(t, dir, "validate")
	if r.ExitCode == 0 {
		t.Fatalf("a shipped build validated a project with no provider installed, so it has "+
			"one compiled in:\n%s", r.combined())
	}
	// And the error is the one a user can act on: which plugin, where it looked, what to do.
	for _, want := range []string{"fake plugin is not available", "infrena-plugin-fake", "--plugin-dir"} {
		if !strings.Contains(r.combined(), want) {
			t.Errorf("the error does not mention %q:\n%s", want, r.combined())
		}
	}
}

// TestTheScaffoldValidatesWithNoProviderInstalled proves through the REAL binary what
// init_test.go proves in process: what `infrena init` writes must pass `infrena validate`
// on a machine that has installed nothing.
//
// An init whose output does not validate is worse than no init at all, because it teaches
// the language wrongly at the one moment a user has no way to tell — and this suite is the
// only one where "nothing installed" is the literal truth rather than an injected double.
func TestTheScaffoldValidatesWithNoProviderInstalled(t *testing.T) {
	dir := t.TempDir()
	if r := runWithoutPlugin(t, dir, "init"); r.ExitCode != 0 {
		t.Fatalf("init exit = %d:\n%s", r.ExitCode, r.combined())
	}

	// init scaffolds into ./infrena, and --chdir means what it says, so the project is
	// named directly rather than discovered.
	r := runWithoutPlugin(t, filepath.Join(dir, "infrena"), "validate")
	if r.ExitCode != 0 {
		t.Fatalf("a freshly scaffolded project does not validate with no provider installed:\n%s",
			r.combined())
	}
}

// runWithoutPlugin runs the binary with no --plugin-dir, as a user has before installing.
func runWithoutPlugin(t *testing.T, dir string, args ...string) result {
	t.Helper()
	cmd := exec.Command(binary(t), append([]string{"--chdir", dir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("running infrena %v: %v", args, err)
		}
		code = exitErr.ExitCode()
	}
	return result{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: code}
}
