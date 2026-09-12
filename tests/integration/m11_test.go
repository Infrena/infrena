package integration

import (
	"encoding/json"
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

// M11 through the built binary: provider instances (PLAN.md §12.1).

// TestAnInstanceConfiguredByAVariableReachesADifferentAccountPerEnvironment is the
// factory split's whole reason for existing.
//
// The cycle it breaks: constructing a provider needs its configuration; this
// configuration interpolates a variable; resolving that variable needs a compile;
// and a compile needs the provider's schemas. Before the split the registry was
// built from raw declarations, so `cloud: ${account_file}` resolved to a string
// nothing read — the provider opened a file named after the expression, or the
// default, depending on the day.
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
  - plugin: test
    cloud: "${account_file}"
resources:
  net:
    type: test.network
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

	// And nothing was created in a file named after the unresolved expression,
	// which is what the old build did.
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
  - plugin: test
    clowd: other.json
resources:
  net:
    type: test.network
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
  - plugin: test
    name: main
  - plugin: test
    name: acct2
resources:
  net:
    type: test.network
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

// TestTwoInstancesThatConfigureNothingAreStillTwoAccounts.
//
// Found by a sabotage that collapsed the fake provider's per-instance default cloud
// path to one shared file and broke NOTHING in the suite. The manual check that
// established this behaviour in the first place was never written down, so the whole
// point of declaring two instances — that they hold different infrastructure — rested
// on a default nothing asserted.
//
// It is the plugin's own default, not the engine's: `cloud:` is the fake provider's
// stand-in for an account, and only the plugin knows that two instances sharing one
// is the same mistake as two AWS instances sharing one set of credentials.
func TestTwoInstancesThatConfigureNothingAreStillTwoAccounts(t *testing.T) {
	dir := project(t, `
project: MainApp
environments:
  dev: {}
providers:
  - plugin: test
    name: main
  - plugin: test
    name: acct2
resources:
  here:
    type: test.network
    cidr: 10.0.0.0/16
  there:
    type: test.network
    cidr: 10.1.0.0/16
    provider: acct2
`)
	if a := run(t, dir, "apply", "dev", "--auto-approve"); a.ExitCode != 2 {
		t.Fatalf("apply exit = %d:\n%s", a.ExitCode, a.combined())
	}

	base := filepath.Join(dir, ".infra")
	main := cloudNames(t, filepath.Join(base, "fake-cloud-main.json"))
	acct2 := cloudNames(t, filepath.Join(base, "fake-cloud-acct2.json"))
	if len(main) != 1 || main[0] != "here" {
		t.Errorf("main holds %v, want just `here`", main)
	}
	// The half a shared file would fail: `there` must be in acct2 and NOWHERE else.
	if len(acct2) != 1 || acct2[0] != "there" {
		t.Errorf("acct2 holds %v, want just `there`", acct2)
	}
}

// TestDestroyReachesEveryInstanceFromStateAlone is invariant 1 for a project with
// more than one account, and it is the sharpest form of §12.1's reason for recording
// the instance in state: `destroy` never compiles. It reads state, synthesises an
// empty desired configuration, and has nothing else to go on — no `providers:` entry
// resolved against variables, no environment, only the name each resource carries.
//
// Found by a sabotage: making the state-only path ignore `providers:` entirely and
// fall back to the implicit instance broke NOTHING in the suite, because every
// multi-instance test until now went through `apply`, which compiles.
func TestDestroyReachesEveryInstanceFromStateAlone(t *testing.T) {
	dir := project(t, `
project: MainApp
environments:
  dev: {}
providers:
  - plugin: test
    name: main
  - plugin: test
    name: acct2
resources:
  here:
    type: test.network
    cidr: 10.0.0.0/16
  there:
    type: test.network
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

	base := filepath.Join(dir, ".infra")
	if got := cloudNames(t, filepath.Join(base, "fake-cloud-main.json")); len(got) != 0 {
		t.Errorf("main still holds %v", got)
	}
	// The one that matters: an instance the destroy could only have found by
	// reading `providers:` for a name state gave it.
	if got := cloudNames(t, filepath.Join(base, "fake-cloud-acct2.json")); len(got) != 0 {
		t.Errorf("acct2 still holds %v — the destroy never reached the second account", got)
	}
}
