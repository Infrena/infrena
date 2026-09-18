package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// varInProviderConfig configures an instance from a variable, which is the shape
// PLAN.md §12.1 offers for reaching a different account per environment, and the
// shape the AWS plugin's region default needs.
const varInProviderConfig = `
project: varprov
environments:
  dev: {}
variables:
  cloud_file:
    type: string
providers:
  - plugin: fake
    cloud: ${var.cloud_file}
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
`

// varOnlyInTheEnvironment is the half the first version of this fixture got wrong. It
// gave cloud_file a `default:`, which every command can resolve WITHOUT an environment
// — so the test passed for `import` while import was still resolving with no
// environment at all, and the bug it was written to catch went out in the commit that
// claimed to fix it. A fixture has to contradict its expected output: the value must be
// obtainable ONLY from the environment the command is given.
const varOnlyInTheEnvironment = "variables:\n  cloud_file: .infra/dev-account.json\n"

// TestEveryCommandResolvesAVariableInProviderConfiguration.
//
// `plan` and `apply` compile, so they always resolved these. The four commands that
// work from state — refresh, destroy, import, discover — did not: they read
// `providers:` with an EMPTY scope and reported `undefined variable` for any `${...}`
// in it. §12.1 called that "the honest cost" of having no environment, and it was
// wrong about the cost being necessary: resolving variables needs stages 1-4 only —
// no registry, no plugins, no resources, no modules — and three of those four
// commands are given an environment on the command line.
//
// What made it matter rather than merely inelegant: an AWS instance supplying
// `region` through `defaults: {region: ${var.aws_region}}` could be planned and applied
// and then never refreshed or destroyed. A project that cannot be torn down by the
// tool that built it is worse than one that cannot be built.
//
// The assertion is per command and each runs against the SAME project, so a
// regression in any one of them is named rather than hidden behind the first.
func TestEveryCommandResolvesAVariableInProviderConfiguration(t *testing.T) {
	dir := project(t, varInProviderConfig)
	// Set ONLY in the environment, so a command that resolves without one cannot get
	// it. See varOnlyInTheEnvironment.
	if err := os.MkdirAll(filepath.Join(dir, "environments"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "environments", "dev.yml"), varOnlyInTheEnvironment)

	if a := run(t, dir, "apply", "dev", "--auto-approve"); a.ExitCode != 2 {
		t.Fatalf("apply: %d\n%s", a.ExitCode, a.combined())
	}
	// The variable really did choose the file: the default cloud path would be
	// fake-cloud.json, so this name existing is what proves it was interpolated.
	if _, err := filepath.Glob(filepath.Join(dir, ".infra", "dev-account.json")); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"refresh", []string{"refresh", "dev"}},
		{"import", []string{"import", "dev", "fake.network.net-1"}},
		// destroy last: it removes what the others look at.
		{"destroy", []string{"destroy", "dev", "--auto-approve"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := run(t, dir, tc.args...).combined()
			// BOTH failure texts. "undefined variable" is what an empty scope
			// produced; "could not be resolved" is what an unresolved instance
			// produces now. A test asserting only the first passed while `import`
			// was still resolving without an environment — the bug went out in the
			// commit that claimed to fix it.
			for _, unresolved := range []string{"undefined variable", "could not be resolved"} {
				if strings.Contains(got, unresolved) {
					t.Errorf("%s could not resolve the variable in `providers:` (%q):\n%s",
						tc.name, unresolved, got)
				}
			}
			// The old failure mode, kept explicit: these two refused the flag that
			// the diagnostic told users to reach for.
			if strings.Contains(got, "does not take --var") {
				t.Errorf("%s still refuses --var:\n%s", tc.name, got)
			}
		})
	}
}

// TestDiscoverRefusesAValueItCannotKnowRatherThanGuessing.
//
// `discover` takes no environment, so a variable only an environment sets has no
// answer for it. The old behaviour was to resolve nothing at all; the new one must
// not overcorrect into resolving it to an unknown and handing that to the plugin,
// which would survey whichever account the plugin defaults to while the output said
// nothing about it. Refusing by name, with --var as the way out, is the only honest
// answer — and --var must actually work, or the message is a promise the command
// breaks.
func TestDiscoverRefusesAValueItCannotKnowRatherThanGuessing(t *testing.T) {
	dir := project(t, `
project: varprov
environments:
  dev: {}
variables:
  cloud_file:
    type: string
providers:
  - plugin: fake
    cloud: ${var.cloud_file}
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
`)
	if err := os.MkdirAll(filepath.Join(dir, "environments"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "environments", "dev.yml"),
		"variables:\n  cloud_file: .infra/dev-account.json\n")

	refused := run(t, dir, "discover")
	if refused.ExitCode == 0 {
		t.Errorf("discover used an unresolved provider value instead of refusing:\n%s", refused.combined())
	}
	for _, want := range []string{"cloud", "could not be resolved", "--var"} {
		requireContains(t, refused.combined(), want)
	}

	// The way out the message names has to exist.
	rescued := run(t, dir, "discover", "--var", "cloud_file=.infra/dev-account.json")
	if rescued.ExitCode != 0 {
		t.Errorf("--var did not rescue discover, so the suggested action is a promise it breaks:\n%s", rescued.combined())
	}
}

// TestTheVersionFloorNowReachesTheStateOnlyCommands.
//
// A consequence of sharing stages 1-4 rather than a goal of it, and worth a test
// precisely because nobody set out to get it. §61.2 recorded the gap: the `infrena:`
// floor check lived in the compiler, so the four commands that never compile did not
// apply it — a binary too old to understand the project would refuse to `plan` it and
// then happily `refresh` it. VariableScope runs the check, so they apply it now.
//
// The floor is only enforced for a RELEASE build (a development build is exempt, and
// the test binary is one), so this stamps a version the way the release workflow does.
// Without the stamp the test would pass against a build that never checks anything.
func TestTheVersionFloorNowReachesTheStateOnlyCommands(t *testing.T) {
	stamped := stampedBinary(t, "9.9.9")

	dir := project(t, `
project: floor
infrena: ">= 99.0"
environments:
  dev: {}
providers:
  - plugin: fake
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
`)

	for _, args := range [][]string{
		{"refresh", "dev"},
		{"destroy", "dev", "--auto-approve"},
		{"discover"},
		{"import", "dev"},
	} {
		got := runBinary(t, stamped, dir, args...)
		if got.ExitCode == 0 {
			t.Errorf("infrena %v ran against a project whose floor it cannot meet:\n%s",
				args, got.combined())
		}
		requireContains(t, got.combined(), "requires infrena >= 99.0")
	}
}

// TestDestroyStillWorksWithNoConfigurationAtAll.
//
// The property the change above must not have broken, and the reason the floor check
// cannot simply be moved earlier: tearing down a project whose files are gone is half
// of what `destroy` is for (§6.1). Configuration that cannot be LOADED leaves the
// implicit instance and proceeds; only configuration that loads and states a floor
// this build cannot meet is refused.
func TestDestroyStillWorksWithNoConfigurationAtAll(t *testing.T) {
	dir := project(t, `
project: gone
environments:
  dev: {}
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
`)
	if a := run(t, dir, "apply", "dev", "--auto-approve"); a.ExitCode != 2 {
		t.Fatalf("apply: %d\n%s", a.ExitCode, a.combined())
	}
	if err := os.Remove(filepath.Join(dir, "infra.yml")); err != nil {
		t.Fatal(err)
	}

	d := run(t, dir, "destroy", "dev", "--auto-approve")
	if d.ExitCode != 2 {
		t.Fatalf("destroy with no configuration exit = %d, want 2\n%s", d.ExitCode, d.combined())
	}
	requireContains(t, d.combined(), "net")
}

// TestDiscoverIgnoresAnUnresolvedDefaultItWillNeverRead is the other side of
// TestDiscoverRefusesAValueItCannotKnowRatherThanGuessing, and the distinction
// between them is the whole point.
//
// An instance's CONFIGURATION crosses to the plugin's Configure, so an unknown
// there picks an account silently and must be refused. Its `defaults:` never
// reach a plugin at all — they are read by the compiler when binding lifecycle
// rules and by import's generator when writing configuration, and `discover`
// does neither. So refusing over one cost a command and bought nothing:
// `defaults: {region: ${var.aws_region}}` with a per-environment value made
// `discover` impossible on a project that was otherwise fine.
//
// Both halves asserted against ONE project, because the risk in narrowing a
// safety check is narrowing it too far. The same file has an unresolved
// `defaults` entry and a resolvable configuration, so this proves discover ran
// over the first without proving it would tolerate the second.
func TestDiscoverIgnoresAnUnresolvedDefaultItWillNeverRead(t *testing.T) {
	dir := project(t, `
project: varprov
environments:
  dev: {}
variables:
  engine:
    type: string
providers:
  - plugin: fake
    defaults:
      engine: ${var.engine}
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
`)
	if err := os.MkdirAll(filepath.Join(dir, "environments"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "environments", "dev.yml"),
		"variables:\n  engine: postgres\n")

	// discover never reads `defaults:`, so an unresolved one must not stop it.
	if r := run(t, dir, "discover"); r.ExitCode != 0 {
		t.Errorf("discover refused over a `defaults:` value it never reads:\n%s", r.combined())
	}

	// And the check it kept still bites: an unresolved CONFIGURATION value does
	// reach the plugin, so it is still refused. Without this, "stop checking
	// defaults" and "stop checking anything" look identical from the outside.
	dir2 := project(t, `
project: varprov
environments:
  dev: {}
variables:
  cloud_file:
    type: string
providers:
  - plugin: fake
    cloud: ${var.cloud_file}
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
`)
	if r := run(t, dir2, "discover"); r.ExitCode == 0 {
		t.Errorf("discover no longer refuses an unresolved CONFIGURATION value, "+
			"so the check was narrowed too far:\n%s", r.combined())
	}
}
