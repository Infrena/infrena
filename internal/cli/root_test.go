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
