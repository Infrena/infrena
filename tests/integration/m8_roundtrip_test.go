package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Invariant 3, the last of §47's six to get a test (spec §29):
//
//	existing infrastructure → discover → import → generate minimal YAML → plan
//	→ no unexpected changes
//
// "Unexpected" is doing real work in that sentence, and this file is where it
// gets pinned down. See TestTheRoundTripLeavesExactlyTheOmittedSecret.

// preexistingCloud is infrastructure this project never created: no `address`
// field on anything, which is what the fake provider writes when IT created a
// resource.
const preexistingCloud = `{"resources":{
  "vpc-0a1b":{"type":"test.network","attributes":{
    "cidr":"10.0.0.0/16","id":"vpc-0a1b"}},
  "db-9":{"type":"test.database","attributes":{
    "engine":"postgres","size":10,"network":"vpc-0a1b","endpoint":"db-9.internal"}},
  "db-77":{"type":"test.database","attributes":{
    "engine":"mysql","size":500,"network":"vpc-0a1b","endpoint":"db-77.internal"}}
}}`

const emptyProject = `
project: adopted
environments:
  dev: {}
`

// roundTripProject writes a project with no resources at all and a cloud full
// of them.
func roundTripProject(t *testing.T, cloud string) string {
	t.Helper()
	dir := projectWithFiles(t, emptyProject, map[string]string{
		".infra/fake-cloud.json": cloud,
	})
	return dir
}

// TestTheImportRoundTripPlansClean is invariant 3.
//
// Nothing is declared to begin with, so every resource in the plan came from
// discovery. A clean plan afterwards means the generated configuration
// describes the infrastructure accurately enough that the engine proposes
// nothing — which is the whole claim of §29.
func TestTheImportRoundTripPlansClean(t *testing.T) {
	dir := roundTripProject(t, preexistingCloud)

	// 1. Discover. Read-only, and it finds things this project never created.
	d := run(t, dir, "discover")
	if d.ExitCode != 0 {
		t.Fatalf("discover exit = %d: %s", d.ExitCode, d.combined())
	}
	for _, want := range []string{"vpc-0a1b", "db-9", "db-77"} {
		requireContains(t, d.Stdout, want)
	}
	// Nothing was adopted by looking.
	if _, err := os.Stat(filepath.Join(dir, "discovered")); !os.IsNotExist(err) {
		t.Error("discover wrote configuration; it is read-only")
	}

	// 2. Import, generating the configuration that declares what was adopted.
	i := run(t, dir, "import", "dev", "--generate")
	if i.ExitCode != 0 {
		t.Fatalf("import exit = %d: %s", i.ExitCode, i.combined())
	}

	// 3. Plan. ZERO operations.
	p := run(t, dir, "plan", "dev")
	if p.ExitCode != 0 {
		t.Fatalf("the round trip is not clean; plan exit = %d (0 means no changes):\n%s\n\n%s",
			p.ExitCode, p.combined(), showGenerated(t, dir))
	}
	requireContains(t, p.Stdout, "0 to create, 0 to update, 0 to replace, 0 to destroy")
}

// TestTheRoundTripGeneratesMinimalConfiguration is the assertion the round trip
// itself cannot make, and the plan for this milestone said so before the code
// existed: a generator that emitted EVERY attribute would also plan clean.
//
// Minimality is therefore tested against the bytes, separately, or §27 is
// unverified no matter how green invariant 3 is.
func TestTheRoundTripGeneratesMinimalConfiguration(t *testing.T) {
	dir := roundTripProject(t, preexistingCloud)
	if r := run(t, dir, "import", "dev", "--generate"); r.ExitCode != 0 {
		t.Fatalf("import exit = %d: %s", r.ExitCode, r.combined())
	}

	databases := readGenerated(t, dir, "databases.yml")

	// db-9's size is 10, which IS the development default: omitted.
	// db-77's size is 500, which is not: emitted. The fixture holds both so
	// that "omits everything" and "omits nothing" both fail.
	if strings.Contains(databases, "size: 10") {
		t.Errorf("size 10 is the provider default for this environment and must be omitted:\n%s", databases)
	}
	if !strings.Contains(databases, "size: 500") {
		t.Errorf("size 500 is not a default and must be emitted:\n%s", databases)
	}

	// Computed attributes are absent — not for tidiness, but because setting
	// one is a hard error and the file would not load.
	if strings.Contains(databases, "endpoint:") {
		t.Errorf("a computed attribute was written; the file should not even load:\n%s", databases)
	}

	// And the file is organised the way §27.1 asks: logical names, not one
	// dump.
	if _, err := os.Stat(filepath.Join(dir, "discovered", "networks.yml")); err != nil {
		t.Errorf("networks were not written to their own file: %v", err)
	}
}

// TestTheRoundTripLeavesExactlyTheOmittedSecret is the honest statement of
// invariant 3, and it exists because the naive one is not true.
//
// §27 forbids writing a discovered secret to a file destined for version
// control. State must still hold it, or drift detection on that attribute
// stops working. So configuration and state genuinely disagree about exactly
// one attribute, and the plan says so — that is a CHANGE, and invariant 3 says
// "no UNEXPECTED changes".
//
// The alternatives are worse in both directions: writing the secret to disk is
// the failure the milestone exists to avoid, and dropping it from state would
// hide a real difference between configuration and reality.
//
// So the test pins the shape rather than the count: exactly one resource
// changes, exactly one attribute of it, it is the sensitive one, and the
// generated file told the reader to supply it.
func TestTheRoundTripLeavesExactlyTheOmittedSecret(t *testing.T) {
	const secret = "hunter2-correct-horse-battery"
	dir := roundTripProject(t, `{"resources":{
	  "vpc-0a1b":{"type":"test.network","attributes":{"cidr":"10.0.0.0/16","id":"vpc-0a1b"}},
	  "db-9":{"type":"test.database","attributes":{
	    "engine":"postgres","size":10,"network":"vpc-0a1b","password":"`+secret+`"}}
	}}`)

	if r := run(t, dir, "import", "dev", "--generate"); r.ExitCode != 0 {
		t.Fatalf("import exit = %d: %s", r.ExitCode, r.combined())
	}

	// The secret is nowhere in anything that would be committed.
	generated := showGenerated(t, dir)
	if strings.Contains(generated, secret) {
		t.Fatalf("the discovered secret was written to disk:\n%s", generated)
	}
	// And the file names what must be supplied.
	requireContains(t, generated, "password")

	p := run(t, dir, "plan", "dev")
	if p.ExitCode != 2 {
		t.Fatalf("plan exit = %d, want 2: a secret omitted from configuration but present in "+
			"state IS a difference, and hiding it would mean hiding real drift too:\n%s",
			p.ExitCode, p.combined())
	}
	// Exactly one resource, exactly one attribute, and it is the secret.
	requireContains(t, p.Stdout, "0 to create, 1 to update, 0 to replace, 0 to destroy")
	requireContains(t, p.Stdout, "password")
	if strings.Contains(p.Stdout, secret) {
		t.Errorf("the plan printed the secret in clear:\n%s", p.Stdout)
	}
	for _, mustNotChange := range []string{"engine:", "size:", "network:", "cidr:"} {
		if strings.Contains(p.Stdout, mustNotChange) {
			t.Errorf("%s also changed; only the omitted secret should:\n%s", mustNotChange, p.Stdout)
		}
	}

	// Supplying it closes the gap — which is what makes the TODO in the
	// generated file an instruction that actually works.
	path := filepath.Join(dir, "discovered", "databases.yml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fixed := strings.Replace(string(body), "    engine: postgres",
		"    engine: postgres\n    password: "+secret, 1)
	if fixed == string(body) {
		t.Fatalf("could not patch the generated file:\n%s", body)
	}
	if err := os.WriteFile(path, []byte(fixed), 0o644); err != nil {
		t.Fatal(err)
	}
	if after := run(t, dir, "plan", "dev"); after.ExitCode != 0 {
		t.Errorf("supplying the omitted secret did not make the plan clean; exit = %d:\n%s",
			after.ExitCode, after.combined())
	}
}

// TestImportingTwiceIsSafe. Import is the command people run when they are
// unsure what happened the first time, so running it again must not duplicate
// configuration, re-adopt anything, or dirty the plan.
func TestImportingTwiceIsSafe(t *testing.T) {
	dir := roundTripProject(t, preexistingCloud)

	if r := run(t, dir, "import", "dev", "--generate"); r.ExitCode != 0 {
		t.Fatalf("first import exit = %d: %s", r.ExitCode, r.combined())
	}
	first := readGenerated(t, dir, "databases.yml")

	// The second import finds everything already in state and must refuse
	// rather than overwrite — importing again would replace the recorded
	// provider IDs with whatever discovery happened to name this time.
	second := run(t, dir, "import", "dev", "--generate")
	if second.ExitCode == 0 {
		t.Errorf("a second import of resources already in state succeeded; it must refuse:\n%s",
			second.combined())
	}
	requireContains(t, second.combined(), "already in the state")

	// Nothing was duplicated in the file, and the plan is still clean.
	if got := readGenerated(t, dir, "databases.yml"); got != first {
		t.Errorf("the second import rewrote the generated file:\n--- before ---\n%s\n--- after ---\n%s",
			first, got)
	}
	if p := run(t, dir, "plan", "dev"); p.ExitCode != 0 {
		t.Errorf("the plan is no longer clean after a second import; exit = %d:\n%s",
			p.ExitCode, p.combined())
	}
}

// TestImportingANewResourceAppendsToTheExistingFile — the account grew. The new
// resource joins the file, the existing blocks are untouched, and the plan is
// clean again.
func TestImportingANewResourceAppendsToTheExistingFile(t *testing.T) {
	dir := roundTripProject(t, preexistingCloud)
	if r := run(t, dir, "import", "dev", "--generate"); r.ExitCode != 0 {
		t.Fatalf("first import exit = %d: %s", r.ExitCode, r.combined())
	}

	// Someone creates a database outside this tool, the way they did before.
	//
	// The cloud is rewritten WHOLE rather than patched. The provider
	// re-serialises the file on every operation, so a string replacement
	// against the original text silently matches nothing after the first
	// import — which is how the first version of this test "passed" its setup
	// and then failed on a resource that was never added.
	cloudPath := filepath.Join(dir, ".infra", "fake-cloud.json")
	grown := strings.Replace(preexistingCloud, `"resources":{`, `"resources":{
	  "db-500":{"type":"test.database","attributes":{
	    "engine":"postgres","size":42,"network":"vpc-0a1b"}},`, 1)
	if grown == preexistingCloud {
		t.Fatal("the fixture did not grow; the replacement matched nothing")
	}
	if err := os.WriteFile(cloudPath, []byte(grown), 0o644); err != nil {
		t.Fatal(err)
	}

	// Import just that one, by selector — the others are already in state.
	r := run(t, dir, "import", "dev", "test.database.db-500", "--generate")
	if r.ExitCode != 0 {
		t.Fatalf("importing one new resource exit = %d: %s", r.ExitCode, r.combined())
	}

	databases := readGenerated(t, dir, "databases.yml")
	if !strings.Contains(databases, "db-500") {
		t.Errorf("the new resource was not appended:\n%s", databases)
	}
	// The existing ones survive exactly once each.
	for _, existing := range []string{"db-9", "db-77"} {
		if n := strings.Count(databases, "\n  "+existing+":"); n != 1 {
			t.Errorf("%s appears %d times after appending:\n%s", existing, n, databases)
		}
	}
	if p := run(t, dir, "plan", "dev"); p.ExitCode != 0 {
		t.Errorf("the plan is not clean after adopting a new resource; exit = %d:\n%s",
			p.ExitCode, p.combined())
	}
}

func readGenerated(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "discovered", name))
	if err != nil {
		t.Fatalf("reading generated %s: %v", name, err)
	}
	return string(b)
}

func showGenerated(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "discovered"))
	if err != nil {
		return "(no discovered/ directory)"
	}
	var sb strings.Builder
	for _, e := range entries {
		sb.WriteString("--- discovered/" + e.Name() + " ---\n")
		b, err := os.ReadFile(filepath.Join(dir, "discovered", e.Name()))
		if err != nil {
			sb.WriteString("(unreadable: " + err.Error() + ")\n")
			continue
		}
		sb.Write(b)
	}
	return sb.String()
}
