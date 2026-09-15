package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/infrena/infrena/pkg/provider"
)

// commandWithBuffers builds a bare command wired to buffers, so a test can
// read exactly what reached stdout. The commands in this package all take
// their writers from cobra (cmd.OutOrStdout()), which is the seam every
// other test in this package already uses; this only gives that seam a name
// for the tests that care about the writer rather than about a command.
func commandWithBuffers(t *testing.T) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	cmd := &cobra.Command{Use: "test"}
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	return cmd, &stdout
}

func TestOpenRunSilencesStdoutWhenOutputIsSet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "run.ndjson")
	cmd, stdout := commandWithBuffers(t)

	ro, closeRun, err := openRun(cmd, &GlobalOptions{Output: path}, "apply", "dev")
	if err != nil {
		t.Fatal(err)
	}
	defer closeRun()

	fmt.Fprint(ro.Out(), "this must not reach the terminal")
	if stdout.Len() != 0 {
		t.Errorf("stdout got %q, want nothing", stdout.String())
	}
	if ro.Report() == nil {
		t.Error("Report() is nil with --output set")
	}
}

func TestOpenRunWritesToStdoutWhenOutputIsAbsent(t *testing.T) {
	cmd, stdout := commandWithBuffers(t)

	ro, closeRun, err := openRun(cmd, &GlobalOptions{}, "apply", "dev")
	if err != nil {
		t.Fatal(err)
	}
	defer closeRun()

	fmt.Fprint(ro.Out(), "hello")
	if stdout.String() != "hello" {
		t.Errorf("stdout = %q, want hello", stdout.String())
	}
	if ro.Report() != nil {
		t.Error("Report() is non-nil with no --output")
	}
}

// newProjectFixture builds the same one-resource project against the
// in-process fake provider that apply_test.go's cases use — projectDir plus
// the double TestMain installs — rather than a third way of standing a
// project up.
func newProjectFixture(t *testing.T) string {
	t.Helper()
	return projectDir(t, `
project: myapp
resources:
  network:
    type: fake.network
    cidr: 10.20.0.0/16
`)
}

// runCommand runs one command through the real root, which is the only way
// to exercise a persistent flag such as --output, and maps its error to the
// exit code Execute would have produced. Nothing is written to the process's
// own stdout, so what the buffer holds is exactly what a terminal would have
// shown.
func runCommand(t *testing.T, dir string, args ...string) (string, string, int) {
	t.Helper()
	return runCommandWithStdin(t, dir, "", args...)
}

func runCommandWithStdin(t *testing.T, dir, stdin string, args ...string) (string, string, int) {
	t.Helper()
	root := NewRootCommand()
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(append([]string{"--chdir", dir}, args...))
	return executeRoot(t, root)
}

// runCommandIn runs a command as if the user were STANDING IN dir, which is not
// the same thing as passing --chdir: the flag answers the question
// findProjectRoot exists to ask, so a test that passed it could never exercise
// the search at all.
//
// The directory is written straight into the flag's value rather than through
// the flag set, because pflag's Set marks a flag Changed and that bit is
// precisely what decides whether the search runs.
func runCommandIn(t *testing.T, dir string, args ...string) (string, string, int) {
	t.Helper()
	root := NewRootCommand()
	if err := root.PersistentFlags().Lookup("chdir").Value.Set(dir); err != nil {
		t.Fatal(err)
	}
	root.SetIn(strings.NewReader(""))
	root.SetArgs(args)
	return executeRoot(t, root)
}

// runCommandInWithoutPlugins is runCommandIn on a machine with no provider
// installed, which is exactly where someone stands the first time they run
// init: a shipped infrena carries no provider (§31.1), and this package's
// TestMain injects one for every other test.
func runCommandInWithoutPlugins(t *testing.T, dir string, args ...string) (string, string, int) {
	t.Helper()
	previous := builtinsFor
	builtinsFor = func(string) map[string]provider.Plugin { return nil }
	t.Cleanup(func() { builtinsFor = previous })
	return runCommandIn(t, dir, args...)
}

// read returns a file's contents or fails the test, for assertions that are
// about what was written rather than about whether it could be read.
func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func executeRoot(t *testing.T, root *cobra.Command) (string, string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)

	code := ExitOK
	if err := root.Execute(); err != nil {
		// The same mapping Execute makes, kept here rather than calling
		// Execute itself, which writes to the process's own stdout and
		// would defeat the whole point of these tests. Every code Execute
		// distinguishes has to be distinguished here too, or a test asserting
		// on one of them silently reads ExitError instead.
		switch {
		case errors.Is(err, errChanges):
			code = ExitChanges
		case errors.Is(err, errNoApproval):
			fmt.Fprintf(&stderr, "Error: %v\n", err)
			code = ExitNoApproval
		default:
			fmt.Fprintf(&stderr, "Error: %v\n", err)
			code = ExitError
		}
	}
	return stdout.String(), stderr.String(), code
}

// withOutputPath substitutes a real temporary path for the "OUT" placeholder
// in a table-driven case's argument list, so a case can say --output without
// each one having to build a directory of its own.
func withOutputPath(t *testing.T, args []string) []string {
	t.Helper()
	out := append([]string(nil), args...)
	for i, a := range out {
		if a == "OUT" {
			out[i] = filepath.Join(t.TempDir(), "run.ndjson")
		}
	}
	return out
}

// stateExists reports whether a command got far enough to persist state for
// an environment. It reads the backend's file directly rather than through
// state.Local, because the question is "did anything reach the disk at all",
// and a loader that answers with an empty state for a missing file cannot
// tell that apart from a run that wrote one.
func stateExists(t *testing.T, dir, environment string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, ".infra", "state", environment+".json"))
	return err == nil
}

// The property, asserted directly rather than inferred from any one command's
// behaviour: with --output set, stdout is byte-empty. A frontend reading the
// file must not also have to strip a human-readable plan from stdout.
func TestOutputModeLeavesStdoutByteEmpty(t *testing.T) {
	for _, command := range []string{"validate", "plan", "apply", "refresh", "destroy"} {
		t.Run(command, func(t *testing.T) {
			dir := newProjectFixture(t)
			out := filepath.Join(t.TempDir(), "run.ndjson")

			stdout, _, _ := runCommand(t, dir, command, "dev", "--output", out, "--auto-approve")

			if stdout != "" {
				t.Errorf("%s --output wrote %q to stdout, want nothing", command, stdout)
			}
			if fi, err := os.Stat(out); err != nil || fi.Size() == 0 {
				t.Errorf("%s --output wrote no file: %v", command, err)
			}
		})
	}
}

// The other half: without --output, progress reaches stdout. A test asserting
// only the silence would pass against a command that prints nothing at all.
func TestWithoutOutputProgressReachesStdout(t *testing.T) {
	dir := newProjectFixture(t)

	stdout, _, _ := runCommand(t, dir, "apply", "dev", "--auto-approve")

	if !strings.Contains(stdout, "Creating ") {
		t.Errorf("no progress on stdout:\n%s", stdout)
	}
}
