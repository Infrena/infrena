package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// M9 §6.1 — an environment is reachable if it is DECLARED or it HAS STATE.
//
// The hazard this closes: before M9, removing an environment from configuration
// left `destroy` working and `plan`/`refresh` refusing with "unknown
// environment". The only reachable command was the destructive one, run blind,
// which inverts plan-then-apply.

const twoEnvProject = `
project: MainApp
environments:
  dev: {}
  sandbox: {}
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  db:
    type: fake.database
    engine: postgres
    network: ${net.id}
`

// removeSandbox rewrites infra.yml without the sandbox environment.
func removeSandbox(t *testing.T, dir string) {
	t.Helper()
	p := filepath.Join(dir, "infra.yml")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	out := strings.Replace(string(b), "  sandbox: {}\n", "", 1)
	if out == string(b) {
		t.Fatal("the fixture did not change; the sandbox declaration was not found")
	}
	if err := os.WriteFile(p, []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestPlanOnARemovedEnvironmentProposesDestroyingEverything — §6.1.
//
// Note what this must NOT do. `resources:` is declared globally, not per
// environment, so COMPILING an undeclared environment yields every resource and
// would plan a full CREATE against an environment already holding them. The
// empty desired configuration has to be supplied deliberately; the teardown is
// not what falls out of compiling.
func TestPlanOnARemovedEnvironmentProposesDestroyingEverything(t *testing.T) {
	dir := project(t, twoEnvProject)
	if r := run(t, dir, "apply", "sandbox", "--auto-approve"); r.ExitCode != 2 {
		t.Fatalf("apply sandbox exit = %d: %s", r.ExitCode, r.combined())
	}
	removeSandbox(t, dir)

	p := run(t, dir, "plan", "sandbox")
	if p.ExitCode != 2 {
		t.Fatalf("plan on a removed environment exit = %d, want 2 (changes):\n%s",
			p.ExitCode, p.combined())
	}
	// Everything in state, destroyed. Not created.
	requireContains(t, p.Stdout, "0 to create, 0 to update, 0 to replace, 2 to destroy")

	// And it SAYS WHY. A total destruction that reads like an ordinary plan is
	// the most alarming output this tool can produce, and the reason is not
	// visible from the diff itself — the resources are still in the file.
	low := strings.ToLower(p.combined())
	if !strings.Contains(low, "sandbox") {
		t.Errorf("the output does not name the environment:\n%s", p.combined())
	}
	if !strings.Contains(low, "no longer declared") && !strings.Contains(low, "not declared") {
		t.Errorf("the output does not explain that the environment is undeclared, so a reader "+
			"cannot tell this from the tool proposing to delete their infrastructure for no "+
			"reason:\n%s", p.combined())
	}
}

// TestAnUnknownStatelessEnvironmentIsStillAnError is the half that keeps §6.1 a
// disjunction rather than "anything goes".
//
// Without it `plan devv` plans nothing and exits 0, so a typo becomes a silent
// no-op instead of a diagnostic — and the diagnostic is the only thing that
// tells a user why their apply did nothing.
func TestAnUnknownStatelessEnvironmentIsStillAnError(t *testing.T) {
	dir := project(t, twoEnvProject)

	r := run(t, dir, "plan", "devv")
	if r.ExitCode != 1 {
		t.Fatalf("plan on a typo'd environment exit = %d, want 1 (error):\n%s", r.ExitCode, r.combined())
	}
	requireContains(t, r.combined(), "devv")
	// The declared names, so the user can see what they meant.
	for _, want := range []string{"dev", "sandbox"} {
		if !strings.Contains(r.combined(), want) {
			t.Errorf("the diagnostic does not list %q among the declared environments:\n%s",
				want, r.combined())
		}
	}
}

// TestApplyingARemovedEnvironmentTearsItDownAndStopsBeingReachable.
//
// The teardown has to complete: once state lists nothing, §6.1's "has state"
// arm no longer holds, so the environment falls back to the typo case. Asserting
// only that the apply succeeded would pass against one that destroyed the
// resources and left state claiming they exist.
func TestApplyingARemovedEnvironmentTearsItDownAndStopsBeingReachable(t *testing.T) {
	dir := project(t, twoEnvProject)
	if r := run(t, dir, "apply", "sandbox", "--auto-approve"); r.ExitCode != 2 {
		t.Fatalf("apply sandbox exit = %d: %s", r.ExitCode, r.combined())
	}
	statePath := filepath.Join(dir, ".infra", "state", "sandbox.json")
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("no state after the first apply: %v", err)
	}
	removeSandbox(t, dir)

	if r := run(t, dir, "apply", "sandbox", "--auto-approve"); r.ExitCode != 2 {
		t.Fatalf("apply on a removed environment exit = %d, want 2:\n%s", r.ExitCode, r.combined())
	}

	// The environment must stop being reachable, which is the OBSERVABLE rule.
	// An earlier version of this test asserted the state FILE was deleted — it is
	// not, deliberately: reachability keys on whether state lists resources, so
	// an emptied environment is already unreachable, and removing the file would
	// discard the serial that detects stale plans.
	after := run(t, dir, "plan", "sandbox")
	if after.ExitCode != 1 {
		t.Errorf("sandbox is still reachable after its teardown; plan exit = %d, want 1 "+
			"(unknown environment):\n%s", after.ExitCode, after.combined())
	}
	requireContains(t, after.combined(), "sandbox")
	if n := strings.Count(after.combined(), "to destroy"); n != 0 {
		t.Errorf("planning a torn-down environment still proposes destroying things:\n%s",
			after.combined())
	}
	// dev is untouched — the teardown is scoped to one environment.
	if r := run(t, dir, "plan", "dev"); r.ExitCode != 2 {
		t.Errorf("dev was affected by sandbox's teardown; plan dev exit = %d:\n%s",
			r.ExitCode, r.combined())
	}
}

// TestRefreshReachesARemovedEnvironment. Drift on something you are about to
// destroy is exactly what you want to see first, and refresh refused for the
// same reason plan did.
func TestRefreshReachesARemovedEnvironment(t *testing.T) {
	dir := project(t, twoEnvProject)
	if r := run(t, dir, "apply", "sandbox", "--auto-approve"); r.ExitCode != 2 {
		t.Fatalf("apply sandbox exit = %d: %s", r.ExitCode, r.combined())
	}
	removeSandbox(t, dir)

	if r := run(t, dir, "refresh", "sandbox"); r.ExitCode != 0 && r.ExitCode != 2 {
		t.Fatalf("refresh on a removed environment exit = %d, want 0 or 2:\n%s",
			r.ExitCode, r.combined())
	}
}

// TestAStatelessButDeclaredEnvironmentIsNormal — the other side of the
// disjunction, and the case that must not regress: a brand-new environment has
// no state and must plan CREATES, not a teardown.
func TestAStatelessButDeclaredEnvironmentIsNormal(t *testing.T) {
	dir := project(t, twoEnvProject)
	p := run(t, dir, "plan", "sandbox")
	if p.ExitCode != 2 {
		t.Fatalf("plan on a fresh declared environment exit = %d:\n%s", p.ExitCode, p.combined())
	}
	requireContains(t, p.Stdout, "2 to create")
	if strings.Contains(p.Stdout, "to destroy") && !strings.Contains(p.Stdout, "0 to destroy") {
		t.Errorf("a fresh environment proposed a destroy:\n%s", p.Stdout)
	}
}

// §6.2 through the built binary: one configuration, four environments, four
// different resource sets, no variables involved beyond the filters themselves.

const filteredModule = `
inputs:
  network:
    type: string
  replica_in:
    type: list
    default: []
resources:
  primary:
    type: fake.database
    engine: postgres
    network: ${var.network}
  replica:
    type: fake.database
    engine: postgres
    network: ${var.network}
    only: ${var.replica_in}
`

const fourEnvProject = `
project: MainApp
environments:
  dev: {}
  staging: {}
  sandbox: {}
  production: {}
modules:
  - ./modules/stack
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  debug_box:
    type: fake.application
    image: debug:1
    database_url: fixed
    only: [dev, sandbox]
  audit:
    type: fake.application
    image: audit:1
    database_url: fixed
    skip: [dev]
  stack:
    type: module.stack
    network: ${net.id}
    replica_in: [production]
`

// TestOneConfigurationFourEnvironments is the milestone through the binary.
//
// Every environment gets `net` and `module.stack.primary`. Beyond that: dev has
// debug_box, staging has audit, sandbox has both filters' opposite halves, and
// production alone has the module's replica. The counts differ per environment,
// which is the whole claim.
func TestOneConfigurationFourEnvironments(t *testing.T) {
	dir := projectWithFiles(t, fourEnvProject, map[string]string{
		"modules/stack/module.yml": filteredModule,
	})

	want := map[string][]string{
		// present, absent
		"dev":        {"fake.application.debug_box", "fake.application.audit"},
		"staging":    {"fake.application.audit", "fake.application.debug_box"},
		"sandbox":    {"fake.application.debug_box", ""},
		"production": {"fake.database.module.stack.replica", "fake.application.debug_box"},
	}

	for _, env := range []string{"dev", "staging", "sandbox", "production"} {
		p := run(t, dir, "plan", env)
		if p.ExitCode != 2 {
			t.Fatalf("plan %s exit = %d:\n%s", env, p.ExitCode, p.combined())
		}
		if present := want[env][0]; present != "" && !strings.Contains(p.Stdout, present) {
			t.Errorf("%s is missing %s:\n%s", env, present, p.Stdout)
		}
		// The absence half, per environment. Presence alone passes against a
		// build that ignores the filters.
		if absent := want[env][1]; absent != "" && strings.Contains(p.Stdout, absent) {
			t.Errorf("%s still has %s, which is filtered out of it:\n%s", env, absent, p.Stdout)
		}
		// The module's own filter, driven by an input the caller supplied — the
		// case §6.2 exists for.
		hasReplica := strings.Contains(p.Stdout, "module.stack.replica")
		if (env == "production") != hasReplica {
			t.Errorf("%s: module replica present = %v, want %v", env, hasReplica, env == "production")
		}
		// Every environment keeps the unfiltered resources, or the filters are
		// removing more than they were asked to.
		for _, always := range []string{"fake.network.net", "module.stack.primary"} {
			if !strings.Contains(p.Stdout, always) {
				t.Errorf("%s lost %s, which no filter mentions:\n%s", env, always, p.Stdout)
			}
		}

		if a := run(t, dir, "apply", env, "--auto-approve"); a.ExitCode != 2 {
			t.Fatalf("apply %s exit = %d:\n%s", env, a.ExitCode, a.combined())
		}
		if again := run(t, dir, "plan", env); again.ExitCode != 0 {
			t.Errorf("%s does not converge; re-plan exit = %d:\n%s", env, again.ExitCode, again.combined())
		}
	}

	// Four independent state files, holding different sets.
	counts := map[string]int{}
	for _, env := range []string{"dev", "staging", "sandbox", "production"} {
		b, err := os.ReadFile(filepath.Join(dir, ".infra", "state", env+".json"))
		if err != nil {
			t.Fatalf("no state for %s: %v", env, err)
		}
		counts[env] = strings.Count(string(b), `"address"`)
	}
	if counts["dev"] == counts["production"] {
		t.Errorf("dev and production hold the same number of resources (%d); the filters did "+
			"nothing", counts["dev"])
	}
}

// TestAddingSkipToAnAppliedResourceProposesDestroyingIt — the ruling, through
// the binary. Editing a filter is a destructive act, deliberately.
func TestAddingSkipToAnAppliedResourceProposesDestroyingIt(t *testing.T) {
	const body = `
project: MainApp
environments:
  dev: {}
  staging: {}
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  extra:
    type: fake.network
    cidr: 10.1.0.0/16
`
	dir := project(t, body)
	for _, env := range []string{"dev", "staging"} {
		if r := run(t, dir, "apply", env, "--auto-approve"); r.ExitCode != 2 {
			t.Fatalf("apply %s: %s", env, r.combined())
		}
	}

	// Now retire `extra` from dev only.
	patched := strings.Replace(body, `  extra:
    type: fake.network
    cidr: 10.1.0.0/16
`, `  extra:
    type: fake.network
    cidr: 10.1.0.0/16
    skip: [dev]
`, 1)
	if patched == body {
		t.Fatal("the fixture did not change")
	}
	if err := os.WriteFile(filepath.Join(dir, "infra.yml"), []byte(patched), 0o644); err != nil {
		t.Fatal(err)
	}

	devPlan := run(t, dir, "plan", "dev")
	if devPlan.ExitCode != 2 {
		t.Fatalf("plan dev exit = %d, want 2 (a destroy):\n%s", devPlan.ExitCode, devPlan.combined())
	}
	requireContains(t, devPlan.Stdout, "1 to destroy")
	requireContains(t, devPlan.Stdout, "fake.network.extra")

	// And staging is untouched — the filter named one environment.
	if stagingPlan := run(t, dir, "plan", "staging"); stagingPlan.ExitCode != 0 {
		t.Errorf("staging changed too; plan exit = %d, want 0:\n%s",
			stagingPlan.ExitCode, stagingPlan.combined())
	}
}

// TestAnEnvironmentTypeKeyFailsValidate — §13, withdrawn, through the binary.
func TestAnEnvironmentTypeKeyFailsValidate(t *testing.T) {
	dir := project(t, `
project: MainApp
environments:
  production:
    type: production
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
`)
	r := run(t, dir, "validate")
	if r.ExitCode == 0 {
		t.Fatalf("`type:` in an environment must fail validate:\n%s", r.combined())
	}
	// It has to say what to use instead, or someone copying an older example is
	// stuck (§44).
	for _, want := range []string{"type", "variables"} {
		if !strings.Contains(r.combined(), want) {
			t.Errorf("the diagnostic does not mention %q:\n%s", want, r.combined())
		}
	}
}
