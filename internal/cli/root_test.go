package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRootCommandHasExpectedSubcommands(t *testing.T) {
	root := NewRootCommand()
	want := []string{"validate", "state"}
	have := map[string]bool{}
	for _, c := range root.Commands() {
		have[c.Name()] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("root command is missing subcommand %q", w)
		}
	}
}

func TestRootCommandGlobalFlags(t *testing.T) {
	root := NewRootCommand()
	for _, name := range []string{"var", "var-file", "verbose", "output"} {
		if root.PersistentFlags().Lookup(name) == nil {
			t.Errorf("missing global flag --%s", name)
		}
	}
}

func TestRootCommandPrintsUsage(t *testing.T) {
	root := NewRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("--help returned error: %v", err)
	}
	if !strings.Contains(out.String(), "infra") {
		t.Errorf("usage output did not mention the tool name: %q", out.String())
	}
}

// TestVarFileReportsAMissingFileRatherThanIgnoringIt replaces
// TestUnsupportedFlagsErrorRatherThanBeingIgnored now that --var-file works:
// checkUnsupportedFlags no longer turns any use of the flag into an error,
// because the variable system has arrived and the flag is wired. Deleting the
// old guarantee outright would be how it disappears with no one noticing (M3's
// lesson), so this pins the one thing still true of a --var-file with nothing
// behind it: naming a file that does not exist must fail loudly rather than
// silently contributing nothing to the run.
//
// It targets `plan`, not `validate`: this task wires --var-file into
// compiler.Options for `plan` only (internal/cli/plan.go) — the brief this
// test was drafted from named `validate`, but `infra validate` does not read
// opts.VarFiles at all yet (that per-command wiring is task 11's), so a
// --var-file there is STILL silently ignored today and the test as originally
// drafted cannot pass. Using `plan dev` instead exercises the path this task
// actually wired.
func TestVarFileReportsAMissingFileRatherThanIgnoringIt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "infra.yml"), []byte("project: myapp\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := NewRootCommand()
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"--chdir", dir, "--var-file", "vars.yml", "plan", "dev"})

	err := root.Execute()
	if err == nil {
		t.Fatal("--var-file naming a file that does not exist must fail, not silently contribute nothing")
	}
	if !strings.Contains(err.Error(), "not valid") {
		t.Errorf("expected the configuration-not-valid error path, got: %v", err)
	}
}

// TestSupportedFlagsAreNotRefused guards the other direction — the check must
// not reject a flag that works.
func TestSupportedFlagsAreNotRefused(t *testing.T) {
	if err := checkUnsupportedFlags(&GlobalOptions{Vars: []string{"a=b"}, Verbose: true}); err != nil {
		t.Errorf("supported flags must pass: %v", err)
	}
}
