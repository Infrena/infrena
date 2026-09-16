package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infrena/infrena/pkg/provider"
)

// PLAN.md section 31.3's interactive offer: when a plugin a project needs is
// missing, and a person is at the terminal, say what is published and offer to
// install it - then STOP.
//
// THE STOP IS THE PART WITH TEETH. Installing mid-`plan` would mean the first
// half of a run happened under different conditions from the second, so every
// test here that installs also asserts the command did not go on to do its job.

// atATerminal makes the offer's one environmental question answer yes.
//
// The predicate is replaced rather than the terminal faked, because there is no
// terminal to fake inside `go test` without a pty library, and the third-party
// budget is two packages. What it stands in for is tested separately, below.
func atATerminal(t *testing.T) {
	t.Helper()
	previous := stdinIsTerminal
	stdinIsTerminal = func(io.Reader) bool { return true }
	t.Cleanup(func() { stdinIsTerminal = previous })
}

// runMissingPlugin runs a command on a machine with no provider installed,
// which is what a fresh checkout of a project looks like.
func runMissingPlugin(t *testing.T, dir, stdin string, args ...string) (string, string, int) {
	t.Helper()
	previous := builtinsFor
	builtinsFor = func(string) map[string]provider.Plugin { return nil }
	t.Cleanup(func() { builtinsFor = previous })
	return runCommandWithStdin(t, dir, stdin, args...)
}

func TestAMissingPluginIsOfferedAtATerminalAndTheCommandStops(t *testing.T) {
	dir := newProjectFixture(t)
	trustSources(t)
	srv := fakeGitHubServing(t, "infrena", "fake", "1.0.0")
	t.Setenv("INFRENA_GITHUB_API", srv.URL)
	atATerminal(t)

	stdout, _, code := runMissingPlugin(t, dir, "yes\n", "validate")

	if code == ExitOK {
		t.Fatalf("the command continued after installing:\n%s", stdout)
	}
	// Every survivor is shown before the question, with its version.
	for _, want := range []string{"github.com/infrena", "1.0.0", "Installed"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the offer does not mention %q:\n%s", want, stdout)
		}
	}
	if n := installedBinaries(t, dir); n != 1 {
		t.Errorf("%d binaries installed, want 1", n)
	}
	// AND IT STOPPED. A validate that went on to report the project valid would
	// have done the second half of its work under conditions the first half
	// never saw.
	if strings.Contains(stdout, "Configuration valid") {
		t.Errorf("the command carried on after installing:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Re-run") {
		t.Errorf("the offer does not say the command stopped:\n%s", stdout)
	}
}

// NO TERMINAL, NO QUESTION, AND NO SEARCH. Nothing may block on input that
// cannot come, and nothing may reach the network for a command that never asked
// to - so this asserts zero network attempts as well as zero prompts.
func TestWithNoTerminalNothingIsAskedAndTheInstallCommandIsNamed(t *testing.T) {
	dir := newProjectFixture(t)
	trustSources(t)
	blocked := blockNetwork(t)

	stdout, stderr, code := runMissingPlugin(t, dir, "yes\n", "validate")

	if code == ExitOK {
		t.Fatal("a missing plugin was not an error")
	}
	if n := blocked.Attempts(); n != 0 {
		t.Errorf("a run with no terminal made %d network attempts, want 0", n)
	}
	if strings.Contains(stdout, "Type yes") {
		t.Errorf("a run with no terminal was asked a question:\n%s", stdout)
	}
	// The existing error, plus the one line that fixes it.
	if !strings.Contains(stderr, "infrena plugins install fake") {
		t.Errorf("the failure does not name the command that installs it:\n%s", stderr)
	}
	if n := installedBinaries(t, dir); n != 0 {
		t.Errorf("%d binaries installed with nobody to approve it", n)
	}
}

func TestDecliningTheOfferInstallsNothing(t *testing.T) {
	dir := newProjectFixture(t)
	trustSources(t)
	srv := fakeGitHubServing(t, "infrena", "fake", "1.0.0")
	t.Setenv("INFRENA_GITHUB_API", srv.URL)
	atATerminal(t)

	stdout, stderr, code := runMissingPlugin(t, dir, "no\n", "validate")

	if code == ExitOK {
		t.Fatal("a declined offer left the command succeeding")
	}
	if n := installedBinaries(t, dir); n != 0 {
		t.Errorf("%d binaries installed after declining", n)
	}
	if strings.Contains(stdout, "Installed") {
		t.Errorf("a declined offer reported an install:\n%s", stdout)
	}
	// And the way to do it by hand is still there, since they may have declined
	// only because they wanted to read it first.
	if !strings.Contains(stderr, "infrena plugins install fake") {
		t.Errorf("declining left no way to install it:\n%s", stderr)
	}
}

// SEARCHING CAN BE TURNED OFF, and then nothing is searched: not a search whose
// result is discarded, no request at all.
func TestTheOfferIsNotMadeWhenSearchingIsDisabled(t *testing.T) {
	dir := newProjectFixture(t)
	trustSources(t)
	atATerminal(t)
	t.Setenv(pluginSearchOffVar, "1")
	blocked := blockNetwork(t)

	stdout, stderr, code := runMissingPlugin(t, dir, "yes\n", "validate")

	if code == ExitOK {
		t.Fatal("a missing plugin was not an error")
	}
	if n := blocked.Attempts(); n != 0 {
		t.Errorf("%d network attempts with searching disabled, want 0", n)
	}
	if strings.Contains(stdout, "Type yes") {
		t.Errorf("a question was asked with searching disabled:\n%s", stdout)
	}
	if !strings.Contains(stderr, "infrena plugins install fake") {
		t.Errorf("the failure does not name the command that installs it:\n%s", stderr)
	}
}

// --output means a frontend is reading a file and nobody is watching stdout, so
// there is nobody to ask however much of a terminal stdin is.
func TestTheOfferIsNotMadeInMachineMode(t *testing.T) {
	dir := newProjectFixture(t)
	trustSources(t)
	atATerminal(t)
	blocked := blockNetwork(t)

	_, _, code := runMissingPlugin(t, dir, "yes\n", "validate",
		"--output", filepath.Join(t.TempDir(), "r.ndjson"))

	if code == ExitOK {
		t.Fatal("a missing plugin was not an error")
	}
	if n := blocked.Attempts(); n != 0 {
		t.Errorf("%d network attempts under --output, want 0", n)
	}
	if n := installedBinaries(t, dir); n != 0 {
		t.Errorf("%d binaries installed under --output", n)
	}
}

// What the replaced predicate actually does. A terminal cannot be stood up in
// `go test` without a pty, but everything this must say NO to can be.
func TestNothingButACharacterDeviceLooksLikeATerminal(t *testing.T) {
	if stdinIsTerminal(strings.NewReader("yes\n")) {
		t.Error("a buffer was taken for a terminal")
	}

	path := filepath.Join(t.TempDir(), "in")
	if err := os.WriteFile(path, []byte("yes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if stdinIsTerminal(f) {
		t.Error("a redirected file was taken for a terminal")
	}

	// THE ONE THAT BIT. /dev/null is a character device, and is what `go test`,
	// cron and most CI runners hand a process as stdin. Taking it for a
	// terminal made a failing `plan` reach api.github.com.
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	if stdinIsTerminal(null) {
		t.Error("/dev/null was taken for a terminal")
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if stdinIsTerminal(r) {
		t.Error("a pipe was taken for a terminal")
	}
}
