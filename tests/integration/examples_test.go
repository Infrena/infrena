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
		// Provenance, all three kinds the README points at.
		"[variable, from base config]", "[variable, from directory vars]",
		"[default, from provider default]",
		// resources/app/vars/sizes.yml beating the module's own default of 10.
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

// TestTheShopExampleTeachesTheLayout runs the two edits the README's "Try
// breaking it" section tells a reader to make, and checks they produce the
// errors it prints.
//
// The README is the only place those errors are written down, so nothing else
// can notice when they drift — and one of them was already wrong before this
// test existed: it told a reader to delete a module's input declaration and
// showed the resulting "undefined variable", when following those steps
// actually reports "has no input" first, from the call the reader was not told
// to change.
func TestTheShopExampleTeachesTheLayout(t *testing.T) {
	src, err := filepath.Abs(filepath.Join("..", "..", "examples", "shop"))
	if err != nil {
		t.Fatal(err)
	}

	// "Scope." Moving the scoped vars file to another resources directory must
	// take `size` out of scope where it is used. This is the assertion that a
	// build which made every scoped variable global would fail — the plan above
	// would pass against one, because the right value would still appear.
	moved := t.TempDir()
	copyTree(t, src, moved)
	if err := os.MkdirAll(filepath.Join(moved, "resources", "network", "vars"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(
		filepath.Join(moved, "resources", "app", "vars", "sizes.yml"),
		filepath.Join(moved, "resources", "network", "vars", "sizes.yml"),
	); err != nil {
		t.Fatal(err)
	}
	r := run(t, moved, "plan", "dev")
	if r.ExitCode == 0 || r.ExitCode == 2 {
		t.Fatalf("`size` stayed in scope after its vars file moved to another directory; "+
			"exit = %d:\n%s", r.ExitCode, r.combined())
	}
	requireContains(t, r.combined(), `undefined variable "size"`)
	requireContains(t, r.combined(), "resources/app/app.yml:7:5")

	// "What the module boundary refuses." Both halves, because the README now
	// claims both: a caller cannot pass what the module does not declare, and a
	// module cannot reach a caller's variable to get around that.
	both := t.TempDir()
	copyTree(t, src, both)
	modPath := filepath.Join(both, "modules", "app-stack", "module.yml")
	callPath := filepath.Join(both, "resources", "app", "app.yml")
	const inputDecl = "  password:\n    type: string\n"

	mod := readFile(t, modPath)
	if !strings.Contains(mod, inputDecl) {
		t.Fatalf("the module no longer declares `password` as the README describes:\n%s", mod)
	}
	writeFile(t, modPath, strings.Replace(mod, inputDecl, "", 1))

	half := run(t, both, "plan", "dev")
	requireContains(t, half.combined(), `has no input "password"`)

	// Now remove the call's line too and have the module reach for the project
	// variable directly. It cannot see it: a module depends only on what it
	// declares, which is what makes the same module reusable elsewhere.
	writeFile(t, modPath, strings.Replace(
		strings.Replace(mod, inputDecl, "", 1),
		"password: ${password}", "password: ${db_password}", 1))
	call := readFile(t, callPath)
	writeFile(t, callPath, strings.Replace(call, "    password: ${db_password}\n", "", 1))

	routed := run(t, both, "plan", "dev")
	requireContains(t, routed.combined(), `undefined variable "db_password"`)
	requireContains(t, routed.combined(), "modules/app-stack/module.yml:16:5")
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
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
		// Skip anything the example is not: .infra/ is state and a fake cloud,
		// left behind by anyone who ran the example in place. Copying it made
		// this test inherit a developer's local run — it failed on
		// "replicas: 3 -> 1" from a state file that is gitignored and was never
		// part of the example at all.
		if strings.HasPrefix(filepath.Base(p), ".") {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
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
