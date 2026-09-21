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
	// "infrena", not "infra": the old name is a substring of "infrastructure",
	// which the description contains, so the looser check passed no matter what
	// the command was called.
	if !strings.Contains(out.String(), "infrena") {
		t.Errorf("usage output did not mention the tool name: %q", out.String())
	}
}

// TestVarFileReportsAMissingFileRatherThanIgnoringIt took over from the
// blanket refusal checkUnsupportedFlags used to apply to --var-file. Dropping
// that refusal without putting anything in its place is how a guarantee
// disappears unnoticed, so this pins the one thing still true of a --var-file
// with nothing behind it: naming a file that does not exist must fail loudly
// rather than silently contributing nothing to the run.
//
// It targets `plan` because that is where --var-file is threaded into
// compiler.Options.
func TestVarFileReportsAMissingFileRatherThanIgnoringIt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "infrena.yml"), []byte("project: myapp\n"), 0o644); err != nil {
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
