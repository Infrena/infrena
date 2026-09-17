package integration

import (
	"strings"
	"testing"
)

// TestValidatePlanAndApplyResolveTheSameVariableLayers is Task 11's core
// property: validate, plan and apply must resolve ${var.cidr} to the same value.
// It asserts behaviour, not structure, because "they share a helper" is not
// the property; "they resolve the same value" is — a test asserting all
// three call compilerOptions would pass even if compilerOptions itself were
// wrong.
func TestValidatePlanAndApplyResolveTheSameVariableLayers(t *testing.T) {
	newDir := func(t *testing.T) string {
		dir := project(t, `
project: myapp
resources:
  net:
    type: fake.network
    cidr: ${var.cidr}
`)
		writeIn(t, dir, "overrides.yml", "cidr: 10.7.0.0/16\n")
		return dir
	}

	// With the file, every command resolves ${var.cidr}.
	for _, cmd := range [][]string{
		{"validate"},
		{"plan", "dev"},
		{"apply", "dev", "--auto-approve"},
	} {
		dir := newDir(t)
		r := run(t, dir, append(append([]string{}, cmd...), "--var-file", "overrides.yml")...)
		if r.ExitCode == 1 {
			t.Errorf("infra %v --var-file overrides.yml failed:\n%s", cmd, r.combined())
		}
		if strings.Contains(r.combined(), "undefined variable") {
			t.Errorf("infra %v did not consult --var-file:\n%s", cmd, r.combined())
		}
	}

	// Without it, every command fails the same way. The negative arm is what
	// makes the positive arm mean something: a command that ignored the
	// reference entirely would pass the first loop.
	for _, cmd := range [][]string{
		{"validate"},
		{"plan", "dev"},
		{"apply", "dev", "--auto-approve"},
	} {
		dir := newDir(t)
		r := run(t, dir, cmd...)
		if r.ExitCode != 1 {
			t.Errorf("infra %v with no value for ${var.cidr}: exit = %d, want 1\n%s", cmd, r.ExitCode, r.combined())
		}
		requireContains(t, r.combined(), "undefined variable")
	}
}

// TestValidateWithNoArgumentChecksEveryDeclaredEnvironment covers validate's
// optional environment argument. Six environments, not two: with six keys an
// implementation that iterates a Go map without sorting produces the sorted
// order by chance once in 720 runs, so this fails 719 times in 720 against an
// unsorted implementation. Two keys would pass roughly 88% of the time
// against broken code, which is not a test.
func TestValidateWithNoArgumentChecksEveryDeclaredEnvironment(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  net:
    type: fake.network
    cidr: ${var.cidr}
`)
	// Five environments set cidr; the sixth does not, so validate must fail
	// and must name the one that is broken.
	for _, env := range []string{"alpha", "bravo", "charlie", "delta", "echo"} {
		writeIn(t, dir, "environments/"+env+".yml", "cidr: 10.0.0.0/16\n")
	}
	writeIn(t, dir, "environments/foxtrot.yml", "unrelated: 1\n")

	r := run(t, dir, "validate")
	if r.ExitCode != 1 {
		t.Fatalf("validate exit = %d, want 1 — an environment with no value for ${var.cidr} is invalid\n%s",
			r.ExitCode, r.combined())
	}
	requireContains(t, r.combined(), "foxtrot")
	// Absence as well as presence: the five good environments must not be
	// blamed for the sixth's problem.
	for _, env := range []string{"alpha", "bravo", "charlie", "delta", "echo"} {
		if strings.Contains(r.combined(), "environment \""+env+"\"") {
			t.Errorf("validate blames %s for foxtrot's missing variable:\n%s", env, r.combined())
		}
	}

	// Naming an environment validates only that one.
	if g := run(t, dir, "validate", "alpha"); g.ExitCode != 0 {
		t.Errorf("validate alpha exit = %d, want 0\n%s", g.ExitCode, g.combined())
	}
	if b := run(t, dir, "validate", "foxtrot"); b.ExitCode != 1 {
		t.Errorf("validate foxtrot exit = %d, want 1\n%s", b.ExitCode, b.combined())
	}
}

// TestValidateReportsAnEnvironmentIndependentErrorOnce covers
// foldByEnvironment's other arm: a diagnostic identical across every
// validated environment (an unknown resource type does not become known in
// staging) is reported once, untagged — not once per environment.
func TestValidateReportsAnEnvironmentIndependentErrorOnce(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  net:
    type: test.nosuchtype
    cidr: 10.0.0.0/16
`)
	for _, env := range []string{"alpha", "bravo", "charlie"} {
		writeIn(t, dir, "environments/"+env+".yml", "unused: 1\n")
	}

	r := run(t, dir, "validate")
	if r.ExitCode != 1 {
		t.Fatalf("validate exit = %d, want 1\n%s", r.ExitCode, r.combined())
	}
	if n := strings.Count(r.combined(), "test.nosuchtype"); n != 1 {
		t.Errorf("an unknown resource type is environment-independent and must be reported once, got %d:\n%s",
			n, r.combined())
	}
}

// TestDestroyAndRefreshAcceptVariableFlagsWithoutChangingWhatStateSays.
//
// These two used to REFUSE --var and --var-file, and this test asserted the refusal.
// The refusal's premise — "a variable has nothing to interpolate into" — was true of
// resources and false of `providers:`, so it is gone (see §12.1, amended); an instance
// configured `cloud: ${var.cloud_file}` was otherwise refreshable and destroyable by
// nothing. tests/integration/provider_variables_test.go covers what the flags now
// reach.
//
// What this test keeps is the concern the refusal was protecting, which has not gone
// away: these commands work from RECORDED STATE, so a --var naming a resource's
// variable must not appear to change what they do. Accepting the flag is only correct
// while that stays true — if a --var could alter a destroy's resource values, the user
// would be editing a teardown with a flag, and the thing destroyed would not be the
// thing recorded.
func TestDestroyAndRefreshAcceptVariableFlagsWithoutChangingWhatStateSays(t *testing.T) {
	dir := varProject(t, "cidr: 10.0.0.0/16\n")
	if r := run(t, dir, "apply", "dev", "--auto-approve"); r.ExitCode != 2 {
		t.Fatalf("apply exit = %d\n%s", r.ExitCode, r.combined())
	}

	// cidr is a RESOURCE variable here, not a provider one. Overriding it must make
	// no difference to either command: what refresh reads and what destroy tears
	// down both come from state.
	for _, args := range [][]string{
		{"refresh", "dev", "--var", "cidr=10.9.0.0/16"},
		{"refresh", "dev", "--var-file", "variables.yml"},
	} {
		r := run(t, dir, args...)
		if r.ExitCode != 0 {
			t.Errorf("infra %v exit = %d, want 0\n%s", args, r.ExitCode, r.combined())
		}
		requireContains(t, r.combined(), "net: refreshed")
	}

	// State still describes what was actually built, not what the flag said.
	if r := run(t, dir, "plan", "dev"); r.ExitCode != 0 {
		t.Errorf("a --var on refresh changed what state records; plan exit = %d, want 0\n%s",
			r.ExitCode, r.combined())
	}

	destroy := run(t, dir, "destroy", "dev", "--auto-approve", "--var", "cidr=10.9.0.0/16")
	if destroy.ExitCode != 2 {
		t.Errorf("destroy exit = %d, want 2 (changes applied)\n%s", destroy.ExitCode, destroy.combined())
	}
	// The teardown came from state: the address recorded there, not a flag's value.
	requireContains(t, destroy.combined(), "net")
	if strings.Contains(destroy.combined(), "10.9.0.0/16") {
		t.Errorf("a --var reached the teardown; destroy must describe what state records:\n%s",
			destroy.combined())
	}
}

// attrLine returns the rendered plan line for one attribute of the operation
// whose header ends with `header`. Prose like "look for the cidr line" is not
// a test; this locates the value inside the right operation block, so a plan
// that printed the right value under the wrong resource fails.
func attrLine(t *testing.T, out, header, attr string) string {
	t.Helper()
	lines := strings.Split(out, "\n")
	for i, l := range lines {
		if !strings.HasSuffix(strings.TrimSpace(l), header) {
			continue
		}
		for _, next := range lines[i+1:] {
			trimmed := strings.TrimSpace(next)
			if trimmed == "" {
				break // end of this operation's block
			}
			if strings.HasPrefix(trimmed, attr+":") {
				return trimmed
			}
		}
		t.Fatalf("operation %q has no %q line:\n%s", header, attr, out)
	}
	t.Fatalf("no operation header ending in %q:\n%s", header, out)
	return ""
}

// TestPrecedenceChainEveryRungWins is spec §19's first M4 criterion.
//
// Five variables, each won by a DIFFERENT rung of PLAN.md §7's chain, all five
// defined at every rung below the one that wins. Testing only the top rung
// cannot distinguish a correct implementation from one that always returns the
// last layer it looked at; testing only the bottom cannot distinguish it from
// one that ignores overrides entirely. The middle three are the test.
//
// The module-defaults rung is deliberately absent: M5 populates it, and M4
// leaves the slot.
func TestPrecedenceChainEveryRungWins(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  a:
    type: fake.network
    cidr: ${var.a}
  b:
    type: fake.network
    cidr: ${var.b}
  c:
    type: fake.network
    cidr: ${var.c}
  d:
    type: fake.network
    cidr: ${var.d}
  e:
    type: fake.network
    cidr: ${var.e}
`)
	// rung 2: base configuration
	writeIn(t, dir, "variables.yml", "a: base\nb: base\nc: base\nd: base\ne: base\n")
	// rung 4: environment inheritance
	writeIn(t, dir, "environments/shared.yml", "b: inherited\nc: inherited\nd: inherited\ne: inherited\n")
	// rung 5: the named environment's own variables
	writeIn(t, dir, "environments/prod.yml", "extends: shared\nc: env\nd: env\ne: env\n")
	// rung 6a: --var-file
	writeIn(t, dir, "overrides.yml", "d: file\ne: file\n")

	// rung 6b: --var
	r := run(t, dir, "plan", "prod", "--var-file", "overrides.yml", "--var", "e=cli")
	if r.ExitCode != 2 {
		t.Fatalf("plan exit = %d, want 2\n%s", r.ExitCode, r.combined())
	}

	for _, tc := range []struct{ resource, want, scopeClause string }{
		{"a", `"base"`, "from base config"},
		{"b", `"inherited"`, "from environment inheritance"},
		{"c", `"env"`, "from environment config"},
		// d is supplied by --var-file, not --var: Amendment 6
		// (pkg/value/format.go's annotation) stamps SuppliedBy with the file
		// path exactly as typed on the command line — "--var" is reserved for
		// the literal flag, as TestScopeSurvivesTheSavedPlanArtifact already
		// pins. The brief's step 12.1 asserted "from --var" for this rung;
		// that contradicts Amendment 6 and this task's own fixture (the file
		// is passed as "overrides.yml"), so this expectation is corrected to
		// match the shipped, tested behavior rather than the brief's prose.
		{"d", `"file"`, "from overrides.yml"},
		{"e", `"cli"`, "from --var"},
	} {
		line := attrLine(t, r.Stdout, "fake.network."+tc.resource, "cidr")
		if !strings.HasPrefix(line, "cidr: "+tc.want) {
			t.Errorf("resource %s: %q, want the value %s", tc.resource, line, tc.want)
		}
		if !strings.Contains(line, tc.scopeClause) {
			t.Errorf("resource %s: %q does not name the winning scope (%q)", tc.resource, line, tc.scopeClause)
		}
	}

	// Absence as well as presence: each losing layer's value must appear
	// nowhere it did not win. "base" wins once (a) and loses four times.
	for _, tc := range []struct {
		text string
		want int
	}{
		{`"base"`, 1}, {`"inherited"`, 1}, {`"env"`, 1}, {`"file"`, 1}, {`"cli"`, 1},
	} {
		if n := strings.Count(r.Stdout, tc.text); n != tc.want {
			t.Errorf("%s appears %d times in the plan, want %d — a losing layer leaked into a "+
				"resource it did not win:\n%s", tc.text, n, tc.want, r.Stdout)
		}
	}

	// The user's fixed target shape, asserted literally once.
	if line := attrLine(t, r.Stdout, "fake.network.e", "cidr"); line != `cidr: "cli" [variable, from --var]` {
		t.Errorf("got %q, want %q", line, `cidr: "cli" [variable, from --var]`)
	}
}

// TestVarFileStackAppliesInFlagOrder is 12.2: multiple --var-file flags apply
// in flag order, so a later file wins over an earlier one for a key both set.
func TestVarFileStackAppliesInFlagOrder(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  a:
    type: fake.network
    cidr: ${var.a}
  b:
    type: fake.network
    cidr: ${var.b}
  c:
    type: fake.network
    cidr: ${var.c}
`)
	writeIn(t, dir, "one.yml", "a: one\nb: one\nc: one\n")
	writeIn(t, dir, "two.yml", "b: two\nc: two\n")
	writeIn(t, dir, "three.yml", "c: three\n")

	r := run(t, dir, "plan", "dev", "--var-file", "one.yml", "--var-file", "two.yml", "--var-file", "three.yml")
	if r.ExitCode != 2 {
		t.Fatalf("plan exit = %d, want 2\n%s", r.ExitCode, r.combined())
	}
	// The middle file's value must survive for b: an implementation that read
	// only the last file, or only the first, fails here and passes a
	// two-file test.
	for _, tc := range []struct{ resource, want string }{
		{"a", `"one"`}, {"b", `"two"`}, {"c", `"three"`},
	} {
		line := attrLine(t, r.Stdout, "fake.network."+tc.resource, "cidr")
		if !strings.HasPrefix(line, "cidr: "+tc.want) {
			t.Errorf("resource %s: %q, want %s", tc.resource, line, tc.want)
		}
	}
}

// TestMissingVarFileIsReportedNotIgnored is 12.2's second half: a --var-file
// that cannot be read is an error, never a silent no-op.
func TestMissingVarFileIsReportedNotIgnored(t *testing.T) {
	dir := varProject(t, "cidr: 10.0.0.0/16\n")
	r := run(t, dir, "plan", "dev", "--var-file", "nope.yml")
	if r.ExitCode != 1 {
		t.Errorf("plan exit = %d, want 1 — a --var-file that cannot be read is an error\n%s",
			r.ExitCode, r.combined())
	}
	requireContains(t, r.combined(), "nope.yml")
}

// typedProject is 12.3's fixture: a typed variable declared in infra.yml's
// `variables:` block, referenced by an attribute whose kind must match.
func typedProject(t *testing.T) string {
	t.Helper()
	return project(t, `
project: myapp
variables:
  size:
    type: integer
    default: 10
    min: 1
    max: 100
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  db:
    type: fake.database
    engine: postgres
    network: ${net.id}
    size: ${var.size}
`)
}

// TestTypedVariablesAreValidatedDuringValidate is PLAN.md §9's "validation must
// happen during infra validate", asserted on `validate` specifically and not
// on plan — §9 names the command.
func TestTypedVariablesAreValidatedDuringValidate(t *testing.T) {
	dir := typedProject(t)

	if r := run(t, dir, "validate"); r.ExitCode != 0 {
		t.Fatalf("validate exit = %d, want 0 — the declared default satisfies the schema\n%s",
			r.ExitCode, r.combined())
	}

	over := run(t, dir, "validate", "--var", "size=1000")
	if over.ExitCode != 1 {
		t.Errorf("validate --var size=1000 exit = %d, want 1\n%s", over.ExitCode, over.combined())
	}
	requireContains(t, over.combined(), "size")
	requireContains(t, over.combined(), "100") // the bound it violated (spec §44: what was expected)

	wrongType := run(t, dir, "validate", "--var", "size=abc")
	if wrongType.ExitCode != 1 {
		t.Errorf("validate --var size=abc exit = %d, want 1\n%s", wrongType.ExitCode, wrongType.combined())
	}
	requireContains(t, wrongType.combined(), "integer")
}

// TestATypedVariableKeepsItsTypeThroughTheChain: --var carries strings, so
// without the declared type `size: ${var.size}` would reach an integer attribute
// as a string and fail schema binding with a kind mismatch the user cannot fix
// from YAML.
func TestATypedVariableKeepsItsTypeThroughTheChain(t *testing.T) {
	dir := typedProject(t)

	r := run(t, dir, "plan", "dev", "--var", "size=42")
	if r.ExitCode != 2 {
		t.Fatalf("plan exit = %d, want 2\n%s", r.ExitCode, r.combined())
	}
	// Unquoted: an integer. `size: "42"` would mean the string survived.
	if line := attrLine(t, r.Stdout, "fake.database.db", "size"); line != `size: 42 [variable, from --var]` {
		t.Errorf("got %q, want %q", line, `size: 42 [variable, from --var]`)
	}

	// The declared default rung still renders as an integer and still carries
	// an annotation — the exact source word is Task 6's to fix if it differs,
	// but a declared default is never SourceExplicit and so is never bare.
	d := run(t, dir, "plan", "dev")
	line := attrLine(t, d.Stdout, "fake.database.db", "size")
	if !strings.HasPrefix(line, "size: 10 [") {
		t.Errorf("declared default rendered as %q, want `size: 10 [...]`", line)
	}
}

// TestASensitiveAttributeIsRedactedWhicheverLayerSuppliedIt is 12.4: a
// sensitive value stays redacted regardless of which precedence rung supplied
// it, while the provenance annotation — metadata, not data — still shows.
func TestASensitiveAttributeIsRedactedWhicheverLayerSuppliedIt(t *testing.T) {
	const secret = "hunter2-do-not-print"
	body := `
project: myapp
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  db:
    type: fake.database
    engine: postgres
    network: ${net.id}
    password: ${var.dbpass}
`
	for _, tc := range []struct {
		name  string
		args  []string
		setup func(t *testing.T, dir string)
	}{
		{
			name:  "from --var",
			args:  []string{"--var", "dbpass=" + secret},
			setup: func(t *testing.T, dir string) {},
		},
		{
			name:  "from --var-file",
			args:  []string{"--var-file", "secrets.yml"},
			setup: func(t *testing.T, dir string) { writeIn(t, dir, "secrets.yml", "dbpass: "+secret+"\n") },
		},
		{
			name:  "from variables.yml",
			args:  nil,
			setup: func(t *testing.T, dir string) { writeIn(t, dir, "variables.yml", "dbpass: "+secret+"\n") },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := project(t, body)
			tc.setup(t, dir)

			r := run(t, dir, append([]string{"plan", "dev"}, tc.args...)...)
			if r.ExitCode != 2 {
				t.Fatalf("plan exit = %d, want 2\n%s", r.ExitCode, r.combined())
			}
			if strings.Contains(r.combined(), secret) {
				t.Errorf("the secret reached the command's output:\n%s", r.combined())
			}
			line := attrLine(t, r.Stdout, "fake.database.db", "password")
			if !strings.HasPrefix(line, "password: <sensitive>") {
				t.Errorf("password rendered as %q, want a redacted value", line)
			}
			// The scope is still disclosed. A precedence level is metadata,
			// not data, so saying it leaks nothing — and a plan that redacted
			// the annotation too would be less useful for no gain.
			if !strings.Contains(line, "[") {
				t.Errorf("password line %q carries no provenance annotation", line)
			}
		})
	}
}

// TestApplySuccessStillShowsAVarFileWarning is the addition the team lead
// asked to fold in: Task 11's apply.go added an unconditional
// ds.Render(cmd.ErrOrStderr()) so a --var-file warning is not silently
// dropped on an otherwise-successful apply — unlike plan.go, apply has no
// later diagnostics pass that would carry the warning through. That render
// call was previously reachable by no test: deleting it broke nothing.
//
// The warning under test is internal/config/varfile.go's reserved
// block-name check: a --var-file with a top-level key matching one of
// infra.yml's own blocks ("resources", "project", "variables",
// "environments") is almost certainly a mistake, but only a warning — a
// variable may legitimately be named that — so apply must still succeed
// (exit 2, changes proposed) while printing it.
func TestApplySuccessStillShowsAVarFileWarning(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  net:
    type: fake.network
    cidr: ${var.cidr}
`)
	// "resources" here names a VARIABLE inside the flat var-file mapping, not
	// a configuration block — it triggers the warning precisely because a
	// var-file cannot declare a configuration block at all. It is unused by
	// the rest of the fixture, so it cannot itself affect whether apply
	// succeeds.
	writeIn(t, dir, "warn.yml", "cidr: 10.0.0.0/16\nresources: oops\n")

	r := run(t, dir, "apply", "dev", "--auto-approve", "--var-file", "warn.yml")
	if r.ExitCode != 2 {
		t.Fatalf("apply exit = %d, want 2 (success with changes) — this test is about the success "+
			"path, not the error path\n%s", r.ExitCode, r.combined())
	}
	// A fragment unique to this diagnostic: grepped against
	// internal/config's other diagnostic strings, exactly one produces it.
	requireContains(t, r.Stderr, `"resources" in warn.yml is a variable, not a configuration block`)
}

// TestAnUnsetVariableDoesNotHideTheRestOfTheFile.
//
// `validate` is the command run to find out what is wrong with a file, and it
// used to answer one problem at a time whenever one of them was an unset
// variable. Stage 4 halted the compile, so stages 4.5 through 6 never ran and
// a completely independent mistake in a resource stayed invisible until the
// variable was supplied and the command run again. A user migrating a project
// hits both at once and fixes them one round trip each.
//
// The two errors here are independent by construction: `size` is declared with
// no default and nothing sets it, and `engine: ${net}` names a resource where
// the schema declares no reference. Neither causes the other, and PLAN.md §7.4
// says diagnostics collect rather than choosing between themselves.
//
// The run still FAILS — an unset variable is an error, not a warning. What is
// asserted is how much of the file one run reports.
func TestAnUnsetVariableDoesNotHideTheRestOfTheFile(t *testing.T) {
	dir := project(t, `
project: myapp
environments:
  production: {}
variables:
  size:
    type: string
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  db:
    type: fake.database
    engine: ${net}
    size: ${var.size}
`)

	r := run(t, dir, "validate", "production")
	if r.ExitCode != 1 {
		t.Fatalf("validate exit = %d, want 1 — an unset variable is still an error\n%s",
			r.ExitCode, r.combined())
	}
	out := r.combined()
	if !strings.Contains(out, `variable "size" is not set`) {
		t.Errorf("the unset variable was not reported:\n%s", out)
	}
	if !strings.Contains(out, "declares no reference") {
		t.Errorf("the unset variable suppressed an independent error further down the file:\n%s", out)
	}
}
