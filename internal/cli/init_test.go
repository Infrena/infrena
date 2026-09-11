package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInitWritesAProjectThatValidates is the load-bearing test, and the reason
// it lives here rather than in a docs check: an init whose output does not
// validate is worse than no init at all, because it teaches the language
// wrongly at the one moment a user has no way to tell.
//
// It runs the real validate command over the real scaffold — asserting the
// files merely EXIST would pass against a scaffold that cannot be compiled.
func TestInitWritesAProjectThatValidates(t *testing.T) {
	dir := t.TempDir()
	if _, err := scaffold(dir); err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	cmd := NewRootCommand()
	var sb strings.Builder
	cmd.SetOut(&sb)
	cmd.SetErr(&sb)
	cmd.SetArgs([]string{"--chdir", dir, "validate"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("the scaffold does not validate: %v\n%s", err, sb.String())
	}
}

// TestInitRefusesToOverwriteAndChangesNothing. init is the command someone runs
// when they are least sure what they are doing, so clobbering an existing
// infra.yml would destroy the one file they cannot regenerate.
//
// The second assertion is the one that matters: a refusal that had already
// written variables.yml would leave a directory that is neither the user's
// project nor a fresh one.
func TestInitRefusesToOverwriteAndChangesNothing(t *testing.T) {
	dir := t.TempDir()
	const mine = "project: mine\n"
	if err := os.WriteFile(filepath.Join(dir, "infra.yml"), []byte(mine), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := scaffold(dir); err == nil {
		t.Fatal("init overwrote an existing project")
	}

	got, err := os.ReadFile(filepath.Join(dir, "infra.yml"))
	if err != nil || string(got) != mine {
		t.Errorf("infra.yml = %q, want it untouched", string(got))
	}
	if _, err := os.Stat(filepath.Join(dir, "variables.yml")); err == nil {
		t.Error("a refused init still wrote variables.yml — the check must run before ANY write, " +
			"or a refusal leaves a directory that is neither the user's project nor a fresh one")
	}
}

// TestInitRefusesWhenAnyScaffoldFileExists, not just infra.yml. A directory
// holding a variables.yml someone wrote is not empty, whatever infra.yml says.
func TestInitRefusesWhenAnyScaffoldFileExists(t *testing.T) {
	dir := t.TempDir()
	// variables.yml deliberately, NOT infra.yml: the scaffold is written in
	// sorted order and infra.yml sorts FIRST, so a conflict there is hit before
	// anything has been written and cannot tell a check-first implementation
	// from a check-as-you-go one. variables.yml sorts LAST, so an interleaved
	// check leaves infra.yml behind and this catches it.
	if err := os.WriteFile(filepath.Join(dir, "variables.yml"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := scaffold(dir); err == nil {
		t.Fatal("init wrote over a directory that already held one of its files")
	}
	if _, err := os.Stat(filepath.Join(dir, "infra.yml")); err == nil {
		t.Error("a refused init left infra.yml behind: the existence check must run over EVERY " +
			"file before ANY is written, or a refusal half-scaffolds the directory")
	}
}

// TestInitIsDeterministic. The file list is reported to the user and the order
// must not come from Go's map iteration.
func TestInitIsDeterministic(t *testing.T) {
	var first []string
	for i := 0; i < 20; i++ {
		got, err := scaffold(t.TempDir())
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
