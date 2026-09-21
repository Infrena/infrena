package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// cloudNames lists the addresses a fake cloud file holds, sorted.
//
// By PATH rather than by project directory, because that is the whole subject:
// which file an instance opens.
func cloudNames(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading fake cloud %s: %v", path, err)
	}
	var doc struct {
		Resources map[string]struct {
			Address string `json:"address"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decoding %s: %v", path, err)
	}
	out := make([]string, 0, len(doc.Resources))
	for _, r := range doc.Resources {
		out = append(out, r.Address)
	}
	sort.Strings(out)
	return out
}

// Provider instances through the built binary.

// TestAnInstanceConfiguredByAVariableReachesADifferentAccountPerEnvironment is why
// provider construction is split from provider use.
//
// The cycle it breaks: constructing a provider needs its configuration; that
// configuration interpolates a variable; resolving the variable needs a compile;
// and a compile needs the provider's schemas. Build the registry from raw
// declarations instead and `cloud: ${var.account_file}` stays a string nothing
// resolves, so the provider opens a file named after the expression.
//
// BOTH environments are applied, because one cannot tell "resolved" from "resolved
// correctly": a build that substituted a constant, or that ignored the key and used
// the default, would satisfy either half on its own.
func TestAnInstanceConfiguredByAVariableReachesADifferentAccountPerEnvironment(t *testing.T) {
	dir := project(t, `
project: MainApp
variables:
  account_file:
    type: string
environments:
  dev:
    account_file: dev-cloud.json
  production:
    account_file: prod-cloud.json
providers:
  - plugin: fake
    cloud: "${var.account_file}"
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
`)

	for _, env := range []string{"dev", "production"} {
		if a := run(t, dir, "apply", env, "--auto-approve"); a.ExitCode != 2 {
			t.Fatalf("apply %s exit = %d:\n%s", env, a.ExitCode, a.combined())
		}
	}

	dev := cloudNames(t, filepath.Join(dir, "dev-cloud.json"))
	prod := cloudNames(t, filepath.Join(dir, "prod-cloud.json"))
	if len(dev) != 1 || len(prod) != 1 {
		t.Fatalf("each environment's own cloud file should hold its one network; dev=%v prod=%v",
			dev, prod)
	}

	// And nothing was created in a file named after the unresolved expression.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "${") {
			t.Errorf("a cloud file was created at the literal expression %q, so the "+
				"interpolation never resolved", e.Name())
		}
	}
}

// TestAnInstanceNamingAnUnacceptedKeyIsRefused — the plugin fails closed.
//
// A misspelled key that is quietly ignored means an instance silently sharing
// another's account, and the first sign of it is a plan proposing to destroy
// resources somebody else owns. The refusal has to come from the PLUGIN, because the
// engine does not know what configuration a plugin accepts.
func TestAnInstanceNamingAnUnacceptedKeyIsRefused(t *testing.T) {
	dir := project(t, `
project: MainApp
environments:
  dev: {}
providers:
  - plugin: fake
    clowd: other.json
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
`)
	r := run(t, dir, "validate")
	if r.ExitCode == 0 {
		t.Fatalf("a key the plugin does not accept must be refused:\n%s", r.combined())
	}
	for _, want := range []string{"clowd", "cloud"} {
		if !strings.Contains(r.combined(), want) {
			t.Errorf("the diagnostic does not mention %q — a reader needs the key they wrote "+
				"and the one they meant:\n%s", want, r.combined())
		}
	}
}

// TestAResourceNamingAnUndeclaredInstanceIsRefusedAtValidate.
//
// The executor has its own guard, and it says "no provider instance offers this
// type" — true, and useless: the mistake is a NAME, the names available are in a
// file the reader can open, and by the time the executor speaks an apply is already
// running.
func TestAResourceNamingAnUndeclaredInstanceIsRefusedAtValidate(t *testing.T) {
	dir := project(t, `
project: MainApp
environments:
  dev: {}
providers:
  - plugin: fake
    name: main
  - plugin: fake
    name: acct2
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
    provider: acct3
`)
	r := run(t, dir, "validate")
	if r.ExitCode == 0 {
		t.Fatalf("a `provider:` naming no declared instance must be refused:\n%s", r.combined())
	}
	for _, want := range []string{"acct3", "main", "acct2"} {
		if !strings.Contains(r.combined(), want) {
			t.Errorf("the diagnostic does not mention %q:\n%s", want, r.combined())
		}
	}
}

// twoAccounts is two instances of one plugin with resources split between them, and
// DIFFERENT counts in each: a bug that split them evenly could otherwise satisfy a
// per-cloud assertion by accident.
//
// EACH ACCOUNT HOLDS ITS OWN NETWORK, which is not decoration. `fake.database`
// declares a requirement for a `fake.network`, and satisfaction is per instance
// (internal/compiler.checkRequirements) because another instance is another
// account. Give acct2 a database and no network and this fixture only validates
// against a check that counts types across the whole project.
const twoAccounts = `
project: MainApp
environments:
  dev: {}
providers:
  - plugin: fake
    name: main
  - plugin: fake
    name: acct2
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  here:
    type: fake.database
    engine: postgres
    network: ${net.id}
  also:
    type: fake.database
    engine: mysql
    network: ${net.id}
  othernet:
    type: fake.network
    cidr: 10.1.0.0/16
    provider: acct2
  there:
    type: fake.database
    engine: postgres
    network: ${othernet.id}
    provider: acct2
`

// TestTwoInstancesThatConfigureNothingAreStillTwoAccounts.
//
// Nothing else asserts it: collapse the fake provider's per-instance default cloud
// path to one shared file and the rest of the suite still passes, although the whole
// point of declaring two instances is that they hold different infrastructure.
//
// It is the plugin's own default, not the engine's: `cloud:` is the fake provider's
// stand-in for an account, and only the plugin knows that two instances sharing one
// is the same mistake as two AWS instances sharing one set of credentials.
func TestTwoInstancesThatConfigureNothingAreStillTwoAccounts(t *testing.T) {
	dir := project(t, twoAccounts)
	if a := run(t, dir, "apply", "dev", "--auto-approve"); a.ExitCode != 2 {
		t.Fatalf("apply exit = %d:\n%s", a.ExitCode, a.combined())
	}

	base := filepath.Join(dir, ".infrena")
	main := cloudNames(t, filepath.Join(base, "fake-cloud-main.json"))
	acct2 := cloudNames(t, filepath.Join(base, "fake-cloud-acct2.json"))
	if strings.Join(main, ",") != "also,here,net" {
		t.Errorf("main holds %v, want net, here and also", main)
	}
	// The half a shared file would fail: acct2's two resources must be there and
	// NOWHERE else.
	if strings.Join(acct2, ",") != "othernet,there" {
		t.Errorf("acct2 holds %v, want othernet and there", acct2)
	}
	// The counts DIFFER, so a symmetric bug that split resources evenly between two
	// files cannot satisfy both assertions above by accident.
	if len(main) == len(acct2) {
		t.Errorf("both clouds hold %d resources", len(main))
	}

	// And it converges, which means each instance was asked about its own resources
	// and found them.
	if again := run(t, dir, "plan", "dev"); again.ExitCode != 0 {
		t.Errorf("does not converge; re-plan exit = %d:\n%s", again.ExitCode, again.combined())
	}
}

// TestRemovingAResourceDestroysItFromItsOwnCloudOnly is what state records the
// provider instance FOR.
//
// A destroy has nothing but state: the configuration that named the account is the
// very thing the user deleted. Without the instance in state this either destroys
// from whichever account wins a lookup, or proposes nothing at all.
func TestRemovingAResourceDestroysItFromItsOwnCloudOnly(t *testing.T) {
	dir := project(t, twoAccounts)
	if a := run(t, dir, "apply", "dev", "--auto-approve"); a.ExitCode != 2 {
		t.Fatalf("apply exit = %d:\n%s", a.ExitCode, a.combined())
	}

	// Remove only acct2's resource.
	rewrite(t, dir, `
project: MainApp
environments:
  dev: {}
providers:
  - plugin: fake
    name: main
  - plugin: fake
    name: acct2
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  here:
    type: fake.database
    engine: postgres
    network: ${net.id}
  also:
    type: fake.database
    engine: mysql
    network: ${net.id}
  othernet:
    type: fake.network
    cidr: 10.1.0.0/16
    provider: acct2
`)
	p := run(t, dir, "plan", "dev")
	if p.ExitCode != 2 {
		t.Fatalf("removing a managed resource must propose a destroy; exit = %d:\n%s",
			p.ExitCode, p.combined())
	}
	if !strings.Contains(p.Stdout, "there") {
		t.Errorf("the destroy is not in the plan:\n%s", p.Stdout)
	}
	if a := run(t, dir, "apply", "dev", "--auto-approve"); a.ExitCode != 2 {
		t.Fatalf("apply exit = %d:\n%s", a.ExitCode, a.combined())
	}

	base := filepath.Join(dir, ".infrena")
	// acct2 keeps its own network — only the database was removed — so this asserts
	// the destroy was precise rather than that the account was emptied.
	if got := cloudNames(t, filepath.Join(base, "fake-cloud-acct2.json")); strings.Join(got, ",") != "othernet" {
		t.Errorf("acct2 holds %v, want just othernet", got)
	}
	// The other account UNTOUCHED, which is the half that fails if the destroy went to
	// whichever instance a type lookup happened to return.
	if got := cloudNames(t, filepath.Join(base, "fake-cloud-main.json")); strings.Join(got, ",") != "also,here,net" {
		t.Errorf("main holds %v — removing acct2's resource disturbed the other account", got)
	}
}

// TestTwoInstancesWithTheSameNameFailValidateNamingBothLines.
//
// An instance name is how a resource chooses its account, so two with one name would send
// resources to whichever happened to win — and the resource that lost would be created in
// the wrong place, successfully. Both LINES are named because the reader has to see the
// pair to know which one to rename.
func TestTwoInstancesWithTheSameNameFailValidateNamingBothLines(t *testing.T) {
	const body = `
project: MainApp
environments:
  dev: {}
providers:
  - plugin: fake
    name: main
  - plugin: fake
    name: main
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
`
	dir := project(t, body)
	r := run(t, dir, "validate")
	if r.ExitCode == 0 {
		t.Fatalf("two instances called `main` must fail validate:\n%s", r.combined())
	}
	if !strings.Contains(r.combined(), `"main"`) {
		t.Errorf("the diagnostic does not name the instance:\n%s", r.combined())
	}

	// BOTH declarations' lines, derived from the fixture rather than written down: a
	// hardcoded number is wrong the first time anyone edits a line above it.
	lines := linesContaining(t, body, "name: main")
	if len(lines) != 2 {
		t.Fatalf("the fixture has %d `name: main` lines, want 2", len(lines))
	}
	// In their CONTEXTUAL spellings, not as bare numbers. A bare digit matches the
	// temporary directory in the path, so a diagnostic that dropped one of the two
	// lines entirely would still pass.
	for _, want := range []string{
		fmt.Sprintf("infrena.yml:%d:", lines[1]), // where the second one is
		fmt.Sprintf("line %d of", lines[0]),      // and where to find the first
	} {
		if !strings.Contains(r.combined(), want) {
			t.Errorf("the diagnostic does not say %q, so a reader sees one of the pair and has "+
				"to find the other by hand:\n%s", want, r.combined())
		}
	}
}

// linesContaining returns the 1-based line numbers of body holding needle.
func linesContaining(t *testing.T, body, needle string) []int {
	t.Helper()
	var out []int
	for i, line := range strings.Split(body, "\n") {
		if strings.Contains(line, needle) {
			out = append(out, i+1)
		}
	}
	return out
}

// TestDestroyReachesEveryInstanceFromStateAlone is the sharpest form of why the
// instance is recorded in state: `destroy` never compiles. It reads state,
// synthesises an empty desired configuration, and has nothing else to go on — no
// `providers:` entry resolved against variables, no environment, only the name
// each resource carries.
//
// Nothing else covers that path: every other multi-instance test goes through
// `apply`, which compiles.
func TestDestroyReachesEveryInstanceFromStateAlone(t *testing.T) {
	dir := project(t, `
project: MainApp
environments:
  dev: {}
providers:
  - plugin: fake
    name: main
  - plugin: fake
    name: acct2
resources:
  here:
    type: fake.network
    cidr: 10.0.0.0/16
  there:
    type: fake.network
    cidr: 10.1.0.0/16
    provider: acct2
`)
	if a := run(t, dir, "apply", "dev", "--auto-approve"); a.ExitCode != 2 {
		t.Fatalf("apply exit = %d:\n%s", a.ExitCode, a.combined())
	}

	d := run(t, dir, "destroy", "dev", "--auto-approve")
	if d.ExitCode != 2 {
		t.Fatalf("destroy exit = %d:\n%s", d.ExitCode, d.combined())
	}

	base := filepath.Join(dir, ".infrena")
	if got := cloudNames(t, filepath.Join(base, "fake-cloud-main.json")); len(got) != 0 {
		t.Errorf("main still holds %v", got)
	}
	// The one that matters: an instance the destroy could only have found by
	// reading `providers:` for a name state gave it.
	if got := cloudNames(t, filepath.Join(base, "fake-cloud-acct2.json")); len(got) != 0 {
		t.Errorf("acct2 still holds %v — the destroy never reached the second account", got)
	}
}

// resourceBlock returns the plan's block for one resource: the line naming it through to
// the blank line that ends it.
//
// Per resource rather than whole-output matching, because "the plan mentions 200
// somewhere" is true whichever resource got the default.
func resourceBlock(t *testing.T, planOutput, name string) string {
	t.Helper()
	lines := strings.Split(planOutput, "\n")
	for i, line := range lines {
		if !strings.HasSuffix(strings.TrimSpace(line), "."+name) {
			continue
		}
		block := []string{line}
		for _, rest := range lines[i+1:] {
			if strings.TrimSpace(rest) == "" {
				break
			}
			block = append(block, rest)
		}
		return strings.Join(block, "\n")
	}
	t.Fatalf("no block for %q in:\n%s", name, planOutput)
	return ""
}

// TestDefaultsReachOneInstancesResourcesAndNotTheOthers.
//
// Two instances of one plugin, only one carrying `defaults:`. Both halves are asserted:
// a build that applied the block to everything and a build that applied it to nothing
// each satisfy one of them.
func TestDefaultsReachOneInstancesResourcesAndNotTheOthers(t *testing.T) {
	dir := project(t, `
project: MainApp
variables:
  tier:
    type: string
environments:
  dev:
    tier: shared
providers:
  - plugin: fake
    name: main
    defaults:
      size: 200
      tags:
        tier: ${var.tier}
  - plugin: fake
    name: acct2
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  here:
    type: fake.database
    engine: postgres
    network: ${net.id}
  othernet:
    type: fake.network
    cidr: 10.1.0.0/16
    provider: acct2
  there:
    type: fake.database
    engine: postgres
    network: ${othernet.id}
    provider: acct2
  own:
    type: fake.database
    engine: postgres
    network: ${net.id}
    size: 50
`)
	p := run(t, dir, "plan", "dev")
	if p.ExitCode != 2 {
		t.Fatalf("plan exit = %d:\n%s", p.ExitCode, p.combined())
	}

	// main's resource inherits both, and the plan says WHERE from — a reader sent to the
	// plugin's documentation for a value in their own file has been misled.
	here := resourceBlock(t, p.Stdout, "here")
	if !strings.Contains(here, "size: 200") {
		t.Errorf("`here` did not inherit main's size default:\n%s", here)
	}
	if !strings.Contains(here, "provider instance default") {
		t.Errorf("the plan does not credit the instance's `defaults:`:\n%s", here)
	}
	if !strings.Contains(here, "shared") {
		t.Errorf("`here` did not inherit main's tags with the variable resolved:\n%s", here)
	}

	// acct2's does not. 10 is the plugin's own schema default, which is what a resource
	// of an instance declaring no `defaults:` falls back to.
	there := resourceBlock(t, p.Stdout, "there")
	if !strings.Contains(there, "size: 10") {
		t.Errorf("`there` did not fall back to the plugin's schema default:\n%s", there)
	}
	if strings.Contains(there, "shared") {
		t.Errorf("`there` picked up main's tags although it belongs to acct2:\n%s", there)
	}

	// And what the resource writes itself still wins over its instance.
	if own := resourceBlock(t, p.Stdout, "own"); !strings.Contains(own, "size: 50") {
		t.Errorf("`own` lost its explicit size to the instance default:\n%s", own)
	}

	// It applies and converges, which is what proves the filled values are real rather
	// than only rendered.
	if a := run(t, dir, "apply", "dev", "--auto-approve"); a.ExitCode != 2 {
		t.Fatalf("apply exit = %d:\n%s", a.ExitCode, a.combined())
	}
	if again := run(t, dir, "plan", "dev"); again.ExitCode != 0 {
		t.Errorf("does not converge; re-plan exit = %d:\n%s", again.ExitCode, again.combined())
	}
}

// TestADefaultsKeyNothingAcceptsFailsValidate. `tag:` for `tags:` would otherwise apply
// to nothing, in every environment, forever, with no output in which its absence is
// visible.
func TestADefaultsKeyNothingAcceptsFailsValidate(t *testing.T) {
	dir := project(t, `
project: MainApp
environments:
  dev: {}
providers:
  - plugin: fake
    defaults:
      tag:
        team: payments
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
`)
	r := run(t, dir, "validate")
	if r.ExitCode == 0 {
		t.Fatalf("a `defaults:` key nothing accepts must fail validate:\n%s", r.combined())
	}
	for _, want := range []string{"tag", "tags"} {
		if !strings.Contains(r.combined(), want) {
			t.Errorf("the diagnostic does not mention %q:\n%s", want, r.combined())
		}
	}
}

// TestALifecycleDefaultProtectsEveryResourceOfItsInstance, and a resource may still
// opt out — the pairing this exists for: prevent_destroy across an account, off for
// the one thing you are replacing.
func TestALifecycleDefaultProtectsEveryResourceOfItsInstance(t *testing.T) {
	const providersBlock = `
project: MainApp
environments:
  dev: {}
providers:
  - plugin: fake
    defaults:
      prevent_destroy: true
`
	dir := project(t, providersBlock+`
resources:
  guarded:
    type: fake.network
    cidr: 10.0.0.0/16
  replaceable:
    type: fake.network
    cidr: 10.1.0.0/16
    lifecycle:
      prevent_destroy: false
`)
	if a := run(t, dir, "apply", "dev", "--auto-approve"); a.ExitCode != 2 {
		t.Fatalf("apply exit = %d:\n%s", a.ExitCode, a.combined())
	}

	// Remove ONLY the resource that opted out. A lifecycle default a resource cannot
	// escape is a trap with no way out, so this half comes first.
	rewrite(t, dir, providersBlock+`
resources:
  guarded:
    type: fake.network
    cidr: 10.0.0.0/16
`)
	p := run(t, dir, "plan", "dev")
	if p.ExitCode != 2 {
		t.Fatalf("removing the opted-out resource must propose a destroy; exit = %d:\n%s",
			p.ExitCode, p.combined())
	}
	if !strings.Contains(p.Stdout, "replaceable") {
		t.Errorf("the destroy of the opted-out resource is not in the plan:\n%s", p.Stdout)
	}

	// Now the guarded one, which the instance's `defaults:` protects and which says
	// nothing about lifecycle itself.
	rewrite(t, dir, providersBlock+`
resources:
  replaceable:
    type: fake.network
    cidr: 10.1.0.0/16
    lifecycle:
      prevent_destroy: false
`)
	r := run(t, dir, "plan", "dev")
	if r.ExitCode == 0 {
		t.Fatalf("removing a resource the instance protects must be refused:\n%s", r.combined())
	}
	if !strings.Contains(r.combined(), "guarded") {
		t.Errorf("the refusal does not name the resource:\n%s", r.combined())
	}
	if !strings.Contains(r.combined(), "prevent_destroy") {
		t.Errorf("nothing explains why, so a reader cannot find the `defaults:` block that "+
			"caused it:\n%s", r.combined())
	}
}

// rewrite replaces a project's infrena.yml.
func rewrite(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "infrena.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
