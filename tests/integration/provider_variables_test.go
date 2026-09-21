package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// varInProviderConfig configures an instance from a variable, which is how a
// project reaches a different account per environment, and the shape the AWS
// plugin's region default needs.
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

// varOnlyInTheEnvironment sets cloud_file with NO `default:`, so the value is
// obtainable only from the environment the command is given. Give it a default
// and every command resolves it without an environment, and the tests below pass
// against a command that never consults one.
const varOnlyInTheEnvironment = "variables:\n  cloud_file: .infrena/dev-account.json\n"

// TestEveryCommandResolvesAVariableInProviderConfiguration.
//
// `plan` and `apply` compile, so they resolve these for free. The four commands that
// work from state — refresh, destroy, import, discover — must resolve them too, and
// can: variable resolution needs only the early compile stages, no registry, no
// plugins, no resources, no modules, and three of the four are given an environment
// on the command line.
//
// What makes it matter: an AWS instance supplying `region` through
// `defaults: {region: ${var.aws_region}}` would otherwise be planned and applied and
// then never refreshed or destroyed. A project that cannot be torn down by the tool
// that built it is worse than one that cannot be built.
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
	if _, err := filepath.Glob(filepath.Join(dir, ".infrena", "dev-account.json")); err != nil {
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
			// produces; "could not be resolved" is what an unresolved instance
			// produces. Asserting only the first passes against a command that
			// still resolves without an environment.
			for _, unresolved := range []string{"undefined variable", "could not be resolved"} {
				if strings.Contains(got, unresolved) {
					t.Errorf("%s could not resolve the variable in `providers:` (%q):\n%s",
						tc.name, unresolved, got)
				}
			}
			// And the flag the diagnostic points at must not be refused.
			if strings.Contains(got, "does not take --var") {
				t.Errorf("%s still refuses --var:\n%s", tc.name, got)
			}
		})
	}
}

// TestDiscoverRefusesAValueItCannotKnowRatherThanGuessing.
//
// `discover` takes no environment, so a variable only an environment sets has no
// answer for it. It must NOT resolve to an unknown and hand that to the plugin,
// which would survey whichever account the plugin defaults to while the output
// said nothing about it. Refusing by name, with --var as the way out, is the
// honest answer — and --var must actually work, or the message is a promise the
// command breaks.
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
		"variables:\n  cloud_file: .infrena/dev-account.json\n")

	refused := run(t, dir, "discover")
	if refused.ExitCode == 0 {
		t.Errorf("discover used an unresolved provider value instead of refusing:\n%s", refused.combined())
	}
	for _, want := range []string{"cloud", "could not be resolved", "--var"} {
		requireContains(t, refused.combined(), want)
	}

	// The way out the message names has to exist.
	rescued := run(t, dir, "discover", "--var", "cloud_file=.infrena/dev-account.json")
	if rescued.ExitCode != 0 {
		t.Errorf("--var did not rescue discover, so the suggested action is a promise it breaks:\n%s", rescued.combined())
	}
}

// TestTheVersionFloorNowReachesTheStateOnlyCommands.
//
// The `infrena:` floor check rides on the same variable-resolution path the
// state-only commands now share. Put it in the compiler alone and a binary too
// old to understand the project refuses to `plan` it and then happily
// `refresh`es it.
//
// The floor is only enforced for a RELEASE build — a development build is exempt,
// and the test binary is one — so this stamps a version the way the release
// workflow does. Without the stamp it passes against a build that never checks.
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
// The reason the floor check cannot simply be moved earlier: tearing down a project
// whose files are gone is half of what `destroy` is for. Configuration that cannot
// be LOADED leaves the implicit instance and proceeds; only configuration that loads
// and states a floor this build cannot meet is refused.
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
	if err := os.Remove(filepath.Join(dir, "infrena.yml")); err != nil {
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
