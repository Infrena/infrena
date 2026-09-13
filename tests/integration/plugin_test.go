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
// A shipped infrata carries no provider, so the binary this suite runs has none either —
// which is the whole point of the cutover. Every other suite in this repository registers
// the fake provider in process; this one builds `infrata-plugin-fake` from its own
// repository and puts it on the search path, so the path a USER takes is proved somewhere:
// a subprocess, a handshake, a cookie, stderr forwarding, and the host's trust rules over
// a real pipe.
//
// That is also the only place the SDK's `os.Stdout` redirect is observable at all — in
// process, Serve writes to the pipe it is given and a plugin's stray Println goes
// somewhere harmless.

// pluginRepoEnv lets a checkout elsewhere be named explicitly.
const pluginRepoEnv = "INFRATA_PLUGIN_FAKE_REPO"

// requireEnv turns the skip below into a FAILURE.
//
// CI sets it. A continuous integration run that silently skips its whole integration suite
// is worse than one with no integration suite at all: it reports green for a suite that
// never executed, and the day the plugin checkout breaks is the day nobody notices. A
// contributor without the plugin cloned still gets a skip, because that is a setup problem
// rather than a defect.
const requireEnv = "INFRATA_REQUIRE_PLUGIN"

var (
	pluginOnce sync.Once
	pluginDir  string
	pluginErr  error
)

// fakePluginDir returns a directory holding a freshly built infrata-plugin-fake, for
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
	message := fmt.Sprintf("infrata-plugin-fake is not available, so this suite cannot run: %v\n"+
		"Clone github.com/infrata/infrata-provider-fake beside this repository, or set %s.",
		pluginErr, pluginRepoEnv)
	if os.Getenv(requireEnv) != "" {
		t.Fatalf("%s\n%s is set, so this is a failure rather than a skip.", message, requireEnv)
	}
	t.Skip(message)
	return ""
}

func buildFakePlugin() {
	repo := os.Getenv(pluginRepoEnv)
	if repo == "" {
		wd, err := os.Getwd()
		if err != nil {
			pluginErr = err
			return
		}
		// wd is <root>/tests/integration, so three levels up is the directory holding
		// this repository, and the plugin is its sibling. Getting this wrong is how the
		// first version of this looked inside the repo itself — which the skip message
		// named, and is why it names it.
		root := filepath.Dir(filepath.Dir(wd))
		repo = filepath.Join(filepath.Dir(root), "infrata-provider-fake")
	}
	if _, err := os.Stat(filepath.Join(repo, "go.mod")); err != nil {
		pluginErr = fmt.Errorf("no Go module at %s", repo)
		return
	}

	out, err := os.MkdirTemp("", "infrata-plugin-*")
	if err != nil {
		pluginErr = err
		return
	}
	// The binary NAME is how the host finds it (§31.1), so this is not arbitrary.
	bin := filepath.Join(out, "infrata-plugin-fake")

	cmd := exec.Command("go", "build", "-o", bin, "./cmd/infrata-plugin-fake")
	// Built from the PLUGIN's module, not this one: it is a separate module with its own
	// go.mod, and `go build` of a path outside the main module is refused.
	cmd.Dir = repo
	if combined, err := cmd.CombinedOutput(); err != nil {
		pluginErr = fmt.Errorf("building infrata-plugin-fake in %s: %v\n%s", repo, err, combined)
		return
	}
	pluginDir = out
}

// TestAShippedBuildCarriesNoProvider is the cutover's governing claim, and the only test
// that can make it: every in-process suite injects the fake double, so a build that
// accidentally regained a compiled-in provider would pass all of them.
//
// It runs the built binary WITHOUT --plugin-dir, which is what a user has before installing
// anything. Sabotage-checked: restoring the builtin makes this say "Configuration valid".
func TestAShippedBuildCarriesNoProvider(t *testing.T) {
	dir := t.TempDir()
	// `init` scaffolds a project using the fake provider, so the scaffold itself is what
	// needs a plugin — which makes this also a test that `init` emits fake.* rather than
	// a type nothing serves.
	if r := runWithoutPlugin(t, dir, "init"); r.ExitCode != 0 {
		t.Fatalf("init exit = %d:\n%s", r.ExitCode, r.combined())
	}

	r := runWithoutPlugin(t, dir, "validate")
	if r.ExitCode == 0 {
		t.Fatalf("a shipped build validated a project with no provider installed, so it has "+
			"one compiled in:\n%s", r.combined())
	}
	// And the error is the one a user can act on: which plugin, where it looked, what to do.
	for _, want := range []string{"fake plugin is not available", "infrata-plugin-fake", "--plugin-dir"} {
		if !strings.Contains(r.combined(), want) {
			t.Errorf("the error does not mention %q:\n%s", want, r.combined())
		}
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
			t.Fatalf("running infrata %v: %v", args, err)
		}
		code = exitErr.ExitCode()
	}
	return result{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: code}
}
