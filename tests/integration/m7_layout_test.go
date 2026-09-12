package integration

import (
	"strings"
	"testing"
)

// M7's conventional directories, through the built binary (PLAN.md §4.1).
//
// The unit tests prove each piece: config.Load walks the directories, stage 4
// resolves a directory's variables, stage 5 hands each resource the right
// scope. This file is here because every M7 defect so far lived in the joins
// between those pieces, not inside any of them — Task 1 shipped a walk that
// loaded files and decoded them with nothing, and `validate` cheerfully
// reported an empty resource set as valid.

const layoutModule = `
inputs:
  network:
    type: string
  size:
    type: integer
    default: 10
  password:
    type: string
resources:
  db:
    type: test.database
    engine: postgres
    network: ${network}
    size: ${size}
    password: ${password}
outputs:
  endpoint:
    value: ${db.endpoint}
`

// TestTheConventionalLayoutPlansAndApplies is the milestone in one run: four
// conventional directories at once, a reference ACROSS two resource
// directories, a scoped variable, and the whole thing reconciling.
//
// The cross-directory reference is the part worth stating. `${network.id}` is
// written in resources/app/ and names a resource declared in
// resources/network/ — an ordinary sibling reference, because a resource's
// address does not depend on the directory that declared it. A layout that
// scoped NAMES as well as variables would fail here with "no such resource",
// which reads like a missing resource rather than like the directories were
// walled off from each other.
func TestTheConventionalLayoutPlansAndApplies(t *testing.T) {
	dir := projectWithFiles(t, `
project: shop
variables:
  db_password:
    type: string
environments:
  dev:
    type: development
  production:
    type: production
modules:
  - ./modules/app-stack
`, map[string]string{
		"modules/app-stack/module.yml": layoutModule,
		"vars/default.yml":             "db_password: dev-secret\n",
		"vars/production.yml":          "db_password: prod-secret\n",
		"resources/network/net.yml": `
resources:
  network:
    type: test.network
    cidr: 10.0.0.0/16
`,
		"resources/app/vars/sizes.yml": "size: 50\n",
		"resources/app/app.yml": `
resources:
  stack:
    type: module.app_stack
    network: ${network.id}
    size: ${size}
    password: ${db_password}
  web:
    type: test.application
    image: nginx:1.27
    database_url: ${stack.endpoint}
`,
	})

	if r := run(t, dir, "validate"); r.ExitCode != 0 {
		t.Fatalf("the conventional layout does not validate: %s", r.combined())
	}

	p := run(t, dir, "plan", "dev")
	if p.ExitCode != 2 {
		t.Fatalf("plan exit = %d, want 2 (changes): %s", p.ExitCode, p.combined())
	}

	// The scoped variable reached the module call's input AND is labelled as
	// the rung it came from. A plan that resolved the value but called it base
	// configuration would still deploy correctly and would still be wrong: the
	// label is how a user finds the file to edit.
	size := attrLine(t, p.Stdout, modHeader("test.database", []string{"stack"}, "db"), "size")
	if !strings.HasPrefix(size, "size: 50 [") {
		t.Errorf("stack db size line = %q, want the directory's 50 (the module's own default is 10)", size)
	}
	if !strings.Contains(size, "directory vars") {
		t.Errorf("stack db size line = %q does not name the directory-vars rung", size)
	}

	// vars/default.yml supplied the password, and the provider's schema still
	// redacts it. A value arriving from a new kind of file must not arrive
	// through a new rendering path.
	pw := attrLine(t, p.Stdout, modHeader("test.database", []string{"stack"}, "db"), "password")
	if !strings.Contains(pw, "<sensitive>") {
		t.Errorf("password line = %q, want it redacted", pw)
	}
	if strings.Contains(p.combined(), "dev-secret") {
		t.Errorf("the secret from vars/default.yml is in the plan in clear:\n%s", p.combined())
	}

	if a := run(t, dir, "apply", "dev", "--auto-approve"); a.ExitCode != 2 {
		t.Fatalf("apply exit = %d, want 2: %s", a.ExitCode, a.combined())
	}
	// Invariant 2 over the whole layout: a second plan proposes nothing.
	if again := run(t, dir, "plan", "dev"); again.ExitCode != 0 {
		t.Fatalf("the layout does not converge; re-plan exit = %d:\n%s", again.ExitCode, again.combined())
	}

	// vars/production.yml names its environment, so production gets a
	// different password — and the value from default.yml must not survive
	// into it. Asserting only that production plans SOMETHING would pass
	// against a build that ignored the environment-named file entirely.
	prod := run(t, dir, "plan", "production")
	if prod.ExitCode != 2 {
		t.Fatalf("plan production exit = %d, want 2: %s", prod.ExitCode, prod.combined())
	}
	if strings.Contains(prod.combined(), "prod-secret") || strings.Contains(prod.combined(), "dev-secret") {
		t.Errorf("a password appears in clear in the production plan:\n%s", prod.combined())
	}
}

// TestTheDirectoryFormAndTheSingleFileFormAgree is the claim §4.1 makes: the
// layout is ORGANISATION, not semantics. Two projects with the same content
// laid out differently must plan identically, and any difference between them
// is a bug in the walk.
//
// Scoped variables are deliberately absent from this fixture. They are the one
// part of §4.1 that is not merely organisation — resources/<dir>/vars has no
// single-file spelling at all — so including them would make the two sides
// genuinely different projects and the comparison meaningless.
func TestTheDirectoryFormAndTheSingleFileFormAgree(t *testing.T) {
	const header = `
project: shop
variables:
  db_password:
    type: string
environments:
  dev:
    type: development
modules:
  - ./modules/app-stack
`
	const resources = `
resources:
  network:
    type: test.network
    cidr: 10.0.0.0/16
  stack:
    type: module.app_stack
    network: ${network.id}
    size: 50
    password: ${db_password}
`
	split := projectWithFiles(t, header, map[string]string{
		"modules/app-stack/module.yml": layoutModule,
		"vars/default.yml":             "db_password: dev-secret\n",
		"resources/net/net.yml": `
resources:
  network:
    type: test.network
    cidr: 10.0.0.0/16
`,
		"resources/stack/stack.yml": `
resources:
  stack:
    type: module.app_stack
    network: ${network.id}
    size: 50
    password: ${db_password}
`,
	})
	single := projectWithFiles(t, header+resources, map[string]string{
		"modules/app-stack/module.yml": layoutModule,
		"variables.yml":                "db_password: dev-secret\n",
	})

	a := run(t, split, "plan", "dev")
	b := run(t, single, "plan", "dev")
	if a.ExitCode != 2 || b.ExitCode != 2 {
		t.Fatalf("plan exits = %d and %d, want 2 and 2:\n--- split ---\n%s\n--- single ---\n%s",
			a.ExitCode, b.ExitCode, a.combined(), b.combined())
	}
	if a.Stdout != b.Stdout {
		t.Errorf("the two forms plan differently — the layout is organisation, not semantics\n"+
			"--- directories ---\n%s\n--- one file ---\n%s", a.Stdout, b.Stdout)
	}

	// The comparison is only worth anything if the plans SAY something. Two
	// empty strings are equal, and a walk that loaded nothing from either side
	// would produce exactly that.
	requireContains(t, a.Stdout, "module.stack.db")
	requireContains(t, a.Stdout, "size: 50")
}

// TestADuplicateAcrossTwoFilesNamesBothPaths — globbing makes accidental
// duplication easy in a way a single file does not, and a user who cannot see
// WHICH two files collided cannot fix it.
func TestADuplicateAcrossTwoFilesNamesBothPaths(t *testing.T) {
	const body = `
resources:
  store:
    type: test.database
    engine: postgres
`
	dir := projectWithFiles(t, "project: shop\n", map[string]string{
		"resources/a/store.yml": body,
		"resources/b/store.yml": body,
	})

	r := run(t, dir, "validate")
	if r.ExitCode == 0 {
		t.Fatalf("two files declaring `store` validated:\n%s", r.combined())
	}
	for _, want := range []string{"resources/a/store.yml", "resources/b/store.yml"} {
		if !strings.Contains(r.combined(), want) {
			t.Errorf("the diagnostic does not name %s — a user cannot fix a collision "+
				"they cannot locate:\n%s", want, r.combined())
		}
	}
}

// TestATemplatesDirectoryIsReservedAndUnread — §4.1 reserves the name without
// implementing it, so a project that has one must still validate.
//
// Both places it may appear, because they are separate code paths: the
// project-level templates/ is skipped by the loader's top-level walk, and the
// scoped resources/<dir>/templates/ by the resources walk — which is where this
// broke once already, reading resources/db/vars/sizes.yml as a resources file
// and rejecting every variable in it.
//
// The fixtures are `.yml` on purpose, and hold something a resources file would
// be REFUSED for. A `.tmpl` proves nothing — the walk reads only `.yml`, so a
// build that read templates/ in full would skip it anyway and the test would
// pass for the wrong reason. A .yml file that decoding would reject is the only
// fixture that can tell "not read" from "read and found nothing to say".
func TestATemplatesDirectoryIsReservedAndUnread(t *testing.T) {
	dir := projectWithFiles(t, `
project: shop
environments:
  dev:
    type: development
`, map[string]string{
		"templates/policy.yml": "role_name: ${project}-policy\n",
		"resources/net/net.yml": `
resources:
  net:
    type: test.network
    cidr: 10.0.0.0/16
`,
		"resources/db/db.yml": `
resources:
  store:
    type: test.database
    engine: postgres
    network: ${net.id}
`,
		"resources/db/templates/role.yml": "role_name: ${project}-db\n",
	})

	if r := run(t, dir, "validate"); r.ExitCode != 0 {
		t.Fatalf("a project with templates/ does not validate — the directory is reserved "+
			"and must not be read:\n%s", r.combined())
	}
	if r := run(t, dir, "plan", "dev"); r.ExitCode != 2 {
		t.Fatalf("plan exit = %d, want 2: %s", r.ExitCode, r.combined())
	}
}
