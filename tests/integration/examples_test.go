package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTheShopExampleStillWorks runs examples/shop through the real binary.
//
// An example in a repository rots silently: it is the one configuration nobody
// runs, so a language change breaks it and the first person to find out is
// someone reading the documentation. Running it here means a change that breaks
// it breaks the build instead.
//
// It asserts the OUTPUT, not just the exit code. An example whose value is that
// it demonstrates provenance and redaction has stopped being that example the
// day it renders neither, and it would still exit 2.
func TestTheShopExampleStillWorks(t *testing.T) {
	src, err := filepath.Abs(filepath.Join("..", "..", "examples", "shop"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	copyTree(t, src, dir)

	if r := run(t, dir, "validate"); r.ExitCode != 0 {
		t.Fatalf("the example does not validate: %s", r.combined())
	}

	p := run(t, dir, "plan", "dev")
	if p.ExitCode != 2 {
		t.Fatalf("plan exit = %d, want 2 (changes): %s", p.ExitCode, p.combined())
	}
	for _, want := range []string{
		// The module expanded, under a module-qualified address.
		"module.stack.db",
		// Redaction, from the provider's schema rather than the configuration.
		"<sensitive>",
		// Provenance, both kinds the README points at.
		"[variable,", "[default,",
		// The caller's override beating the module's own default of 10.
		"size: 50",
	} {
		if !strings.Contains(p.combined(), want) {
			t.Errorf("the example no longer demonstrates %q:\n%s", want, p.combined())
		}
	}

	if a := run(t, dir, "apply", "dev", "--auto-approve"); a.ExitCode != 2 {
		t.Fatalf("apply exit = %d, want 2: %s", a.ExitCode, a.combined())
	}
	if again := run(t, dir, "plan", "dev"); again.ExitCode != 0 {
		t.Fatalf("the example does not converge; re-plan exit = %d:\n%s", again.ExitCode, again.combined())
	}

	// The README tells a reader production differs. If it stops differing, the
	// README is wrong and nothing else would notice.
	if r := run(t, dir, "plan", "production"); r.ExitCode != 2 {
		t.Fatalf("plan production exit = %d, want 2: %s", r.ExitCode, r.combined())
	}
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}
