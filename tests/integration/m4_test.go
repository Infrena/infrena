package integration

import (
	"strings"
	"testing"
)

// TestValidatePlanAndApplyResolveTheSameVariableLayers is Task 11's core
// property: validate, plan and apply must resolve ${cidr} to the same value.
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
    type: test.network
    cidr: ${cidr}
`)
		writeIn(t, dir, "overrides.yml", "cidr: 10.7.0.0/16\n")
		return dir
	}

	// With the file, every command resolves ${cidr}.
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
			t.Errorf("infra %v with no value for ${cidr}: exit = %d, want 1\n%s", cmd, r.ExitCode, r.combined())
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
    type: test.network
    cidr: ${cidr}
`)
	// Five environments set cidr; the sixth does not, so validate must fail
	// and must name the one that is broken.
	for _, env := range []string{"alpha", "bravo", "charlie", "delta", "echo"} {
		writeIn(t, dir, "environments/"+env+".yml", "cidr: 10.0.0.0/16\n")
	}
	writeIn(t, dir, "environments/foxtrot.yml", "unrelated: 1\n")

	r := run(t, dir, "validate")
	if r.ExitCode != 1 {
		t.Fatalf("validate exit = %d, want 1 — an environment with no value for ${cidr} is invalid\n%s",
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

// TestDestroyAndRefreshRefuseVariableFlags is the CLI-level counterpart to
// internal/cli's TestDestroyRejectsVarFlag/TestRefreshRejectsVarFlag: it
// drives the real built binary, so it also proves the refusal happens before
// the environment is touched at all.
func TestDestroyAndRefreshRefuseVariableFlags(t *testing.T) {
	dir := varProject(t, "cidr: 10.0.0.0/16\n")
	if r := run(t, dir, "apply", "dev", "--auto-approve"); r.ExitCode != 2 {
		t.Fatalf("apply exit = %d\n%s", r.ExitCode, r.combined())
	}

	for _, tc := range []struct {
		args []string
	}{
		{[]string{"destroy", "dev", "--auto-approve", "--var", "cidr=10.1.0.0/16"}},
		{[]string{"destroy", "dev", "--auto-approve", "--var-file", "variables.yml"}},
		{[]string{"refresh", "dev", "--var", "cidr=10.1.0.0/16"}},
		{[]string{"refresh", "dev", "--var-file", "variables.yml"}},
	} {
		r := run(t, dir, tc.args...)
		if r.ExitCode != 1 {
			t.Errorf("infra %v exit = %d, want 1 — a flag that cannot affect the outcome must be "+
				"refused, not ignored\n%s", tc.args, r.ExitCode, r.combined())
		}
		requireContains(t, r.combined(), "does not take --var")
	}

	// And the environment still exists: a refused command must not have run.
	if r := run(t, dir, "plan", "dev"); r.ExitCode != 0 {
		t.Errorf("plan after the refused commands exit = %d, want 0 (nothing should have changed)\n%s",
			r.ExitCode, r.combined())
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
    type: test.network
    cidr: ${a}
  b:
    type: test.network
    cidr: ${b}
  c:
    type: test.network
    cidr: ${c}
  d:
    type: test.network
    cidr: ${d}
  e:
    type: test.network
    cidr: ${e}
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
		{"c", `"env"`, "from environment variable"},
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
		line := attrLine(t, r.Stdout, "test.network."+tc.resource, "cidr")
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
	if line := attrLine(t, r.Stdout, "test.network.e", "cidr"); line != `cidr: "cli" [variable, from --var]` {
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
    type: test.network
    cidr: ${a}
  b:
    type: test.network
    cidr: ${b}
  c:
    type: test.network
    cidr: ${c}
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
		line := attrLine(t, r.Stdout, "test.network."+tc.resource, "cidr")
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
    type: test.network
    cidr: 10.0.0.0/16
  db:
    type: test.database
    engine: postgres
    network: ${net.id}
    size: ${size}
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
// without the declared type `size: ${size}` would reach an integer attribute
// as a string and fail schema binding with a kind mismatch the user cannot fix
// from YAML.
func TestATypedVariableKeepsItsTypeThroughTheChain(t *testing.T) {
	dir := typedProject(t)

	r := run(t, dir, "plan", "dev", "--var", "size=42")
	if r.ExitCode != 2 {
		t.Fatalf("plan exit = %d, want 2\n%s", r.ExitCode, r.combined())
	}
	// Unquoted: an integer. `size: "42"` would mean the string survived.
	if line := attrLine(t, r.Stdout, "test.database.db", "size"); line != `size: 42 [variable, from --var]` {
		t.Errorf("got %q, want %q", line, `size: 42 [variable, from --var]`)
	}

	// The declared default rung still renders as an integer and still carries
	// an annotation — the exact source word is Task 6's to fix if it differs,
	// but a declared default is never SourceExplicit and so is never bare.
	d := run(t, dir, "plan", "dev")
	line := attrLine(t, d.Stdout, "test.database.db", "size")
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
    type: test.network
    cidr: 10.0.0.0/16
  db:
    type: test.database
    engine: postgres
    network: ${net.id}
    password: ${dbpass}
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
			line := attrLine(t, r.Stdout, "test.database.db", "password")
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
