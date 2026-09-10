package cli

import (
	"bytes"
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

// TestUnsupportedFlagsErrorRatherThanBeingIgnored covers the class, not the
// instance. --var-file was registered, advertised in --help, and read by no
// code path. That crossed from harmless to misleading once --var began
// working end to end, because a user has every reason to assume its sibling
// does too. A flag that silently does nothing is worse than an absent one.
func TestUnsupportedFlagsErrorRatherThanBeingIgnored(t *testing.T) {
	root := NewRootCommand()
	root.SetArgs([]string{"--var-file", "vars.yml", "validate"})
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)

	err := root.Execute()
	if err == nil {
		t.Fatal("--var-file is not wired to anything; using it must be an error, not a silent no-op")
	}
	if !strings.Contains(err.Error(), "--var-file") {
		t.Errorf("the error must name the flag: %v", err)
	}
	if !strings.Contains(err.Error(), "--var") {
		t.Errorf("the error should point at what does work: %v", err)
	}
}

// TestSupportedFlagsAreNotRefused guards the other direction — the check must
// not reject a flag that works.
func TestSupportedFlagsAreNotRefused(t *testing.T) {
	if err := checkUnsupportedFlags(&GlobalOptions{Vars: []string{"a=b"}, Verbose: true}); err != nil {
		t.Errorf("supported flags must pass: %v", err)
	}
}
