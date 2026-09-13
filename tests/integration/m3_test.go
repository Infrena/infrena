package integration

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// startAsync starts the CLI without waiting for it, so the caller can start
// a second invocation while the first is still running — the genuine
// overlap a concurrency test needs. Unlike run, it does not block on
// cmd.Wait(); the caller does that explicitly once it needs the result.
func startAsync(t *testing.T, dir string, args ...string) (*exec.Cmd, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	// --plugin-dir, as run() does: the binary under test carries no provider.
	cmd := exec.Command(binary(t), append([]string{"--chdir", dir, "--plugin-dir", fakePluginDir(t)}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting infra %v: %v", args, err)
	}
	return cmd, &stdout, &stderr
}

// runStdin is run with stdin controlled, for the typed-confirmation tests.
// run (helpers_test.go) has no way to set stdin, and is left untouched —
// this is additive, not a duplicate of it.
func runStdin(t *testing.T, dir, stdin string, args ...string) result {
	t.Helper()
	// --plugin-dir, as run() does: the binary under test carries no provider.
	cmd := exec.Command(binary(t), append([]string{"--chdir", dir, "--plugin-dir", fakePluginDir(t)}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if err2, ok := err.(*exec.ExitError); ok {
			exitErr = err2
		}
		if exitErr == nil {
			t.Fatalf("running infra %v: %v", args, err)
		}
		code = exitErr.ExitCode()
	}
	return result{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: code}
}

// seedCloudLatency writes .infra/fake-cloud.json with no resources and a
// simulated per-operation delay, so a subsequent apply's Create call is
// slow enough to overlap a second process's attempt to lock the
// environment.
func seedCloudLatency(t *testing.T, dir string, ms int) {
	t.Helper()
	cloudDir := filepath.Join(dir, ".infra")
	if err := os.MkdirAll(cloudDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	doc := map[string]any{"resources": map[string]any{}, "latency_ms": ms}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal fake cloud: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cloudDir, "fake-cloud.json"), data, 0o600); err != nil {
		t.Fatalf("write fake cloud: %v", err)
	}
}

func readFakeCloudResources(t *testing.T, dir string) map[string]map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, ".infra", "fake-cloud.json"))
	if err != nil {
		t.Fatalf("reading fake cloud: %v", err)
	}
	var doc struct {
		Resources map[string]map[string]any `json:"resources"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decoding fake cloud: %v", err)
	}
	return doc.Resources
}

// cloudResourceID returns the single provider id whose recorded address
// matches name, failing loudly on anything other than exactly one match —
// silently picking "the first" on ambiguity would make a future
// multi-resource fixture pass or fail for the wrong reason.
func cloudResourceID(t *testing.T, dir, name string) string {
	t.Helper()
	var found []string
	for id, r := range readFakeCloudResources(t, dir) {
		if addr, _ := r["address"].(string); addr == name {
			found = append(found, id)
		}
	}
	if len(found) != 1 {
		t.Fatalf("expected exactly one cloud resource with address %q, found %d", name, len(found))
	}
	return found[0]
}

func mutateCloudAttribute(t *testing.T, dir, id, key string, value any) {
	t.Helper()
	path := filepath.Join(dir, ".infra", "fake-cloud.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading fake cloud: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decoding fake cloud: %v", err)
	}
	resources := doc["resources"].(map[string]any)
	resource := resources[id].(map[string]any)
	attrs := resource["attributes"].(map[string]any)
	attrs[key] = value
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal fake cloud: %v", err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatalf("writing fake cloud: %v", err)
	}
}

func readStateFile(t *testing.T, dir, environment string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, ".infra", "state", environment+".json"))
	if err != nil {
		t.Fatalf("reading state: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decoding state: %v", err)
	}
	return doc
}

func readStateAttribute(t *testing.T, dir, environment, name, attr string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, ".infra", "state", environment+".json"))
	if err != nil {
		t.Fatalf("reading state: %v", err)
	}
	var doc struct {
		Resources map[string]struct {
			Attributes map[string]struct {
				Raw any `json:"raw"`
			} `json:"attributes"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decoding state: %v", err)
	}
	r, ok := doc.Resources[name]
	if !ok {
		t.Fatalf("resource %q not found in state", name)
	}
	a, ok := r.Attributes[attr]
	if !ok {
		t.Fatalf("attribute %q not found on %q", attr, name)
	}
	s, ok := a.Raw.(string)
	if !ok {
		t.Fatalf("attribute %q on %q is not a string: %#v", attr, name, a.Raw)
	}
	return s
}

// TestM3MVPRoundTrip drives §48's script as far as M3's own scope goes: no
// `infra init` (see this task's doc comment — it is M7, not M3), starting
// instead from a hand-written infra.yml exactly like M2's suite already
// does.
func TestM3MVPRoundTrip(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  network:
    type: fake.network
    cidr: 10.20.0.0/16
`)

	if v := run(t, dir, "validate"); v.ExitCode != 0 {
		t.Fatalf("validate exit code %d, want 0\n%s", v.ExitCode, v.combined())
	}

	p1 := run(t, dir, "plan", "dev")
	if p1.ExitCode != 2 {
		t.Fatalf("plan exit code %d, want 2\n%s", p1.ExitCode, p1.combined())
	}
	requireContains(t, p1.Stdout, "+ fake.network.network")
	if _, err := os.Stat(filepath.Join(dir, ".infra", "state", "dev.json")); !os.IsNotExist(err) {
		t.Fatal("plan must not have written state")
	}

	a1 := run(t, dir, "apply", "dev", "--auto-approve")
	if a1.ExitCode != 2 {
		t.Fatalf("apply exit code %d, want 2\n%s", a1.ExitCode, a1.combined())
	}
	requireContains(t, a1.Stdout, "Apply complete:")

	p2 := run(t, dir, "plan", "dev")
	if p2.ExitCode != 0 {
		t.Fatalf("re-plan exit code %d, want 0 (no changes)\n%s", p2.ExitCode, p2.combined())
	}
	requireContains(t, p2.Stdout, "No changes")
	if strings.Contains(p2.Stdout, "Usage:") || strings.Contains(p2.Stdout, "Error:") {
		t.Errorf("re-plan output is polluted:\n%s", p2.Stdout)
	}

	// Externally mutate .infra/fake-cloud.json by hand, standing in for a
	// person changing real infrastructure outside infra entirely.
	id := cloudResourceID(t, dir, "network")
	mutateCloudAttribute(t, dir, id, "cidr", "10.99.0.0/16")

	p3 := run(t, dir, "plan", "dev")
	if p3.ExitCode != 2 {
		t.Fatalf("drift plan exit code %d, want 2\n%s", p3.ExitCode, p3.combined())
	}
	// cidr is ForceNew (providers/test/definitions.go), so drifting it
	// proposes a replacement, not an in-place update — confirmed against
	// the team lead's own hand-verified anchor for this exact scenario.
	requireContains(t, p3.Stdout, "-/+ fake.network.network")
	requireContains(t, p3.Stdout, "replacement forced by: cidr")
	requireContains(t, p3.Stdout, `"10.99.0.0/16" -> "10.20.0.0/16"`)

	// plan never wrote the drift down (spec §10) — refresh does.
	r1 := run(t, dir, "refresh", "dev")
	if r1.ExitCode != 0 {
		t.Fatalf("refresh exit code %d, want 0\n%s", r1.ExitCode, r1.combined())
	}
	if got := readStateAttribute(t, dir, "dev", "network", "cidr"); got != "10.99.0.0/16" {
		t.Errorf("refresh did not persist the observed drift: state cidr = %q, want %q", got, "10.99.0.0/16")
	}

	// Remove the resource from configuration.
	if err := os.WriteFile(filepath.Join(dir, "infra.yml"), []byte("project: myapp\nresources: {}\n"), 0o644); err != nil {
		t.Fatalf("rewriting infra.yml: %v", err)
	}

	p4 := run(t, dir, "plan", "dev")
	if p4.ExitCode != 2 {
		t.Fatalf("removal plan exit code %d, want 2\n%s", p4.ExitCode, p4.combined())
	}
	requireContains(t, p4.Stdout, "- fake.network.network")

	a2 := run(t, dir, "apply", "dev", "--auto-approve")
	if a2.ExitCode != 2 {
		t.Fatalf("teardown apply exit code %d, want 2\n%s", a2.ExitCode, a2.combined())
	}
	requireContains(t, a2.Stdout, "Apply complete:")

	st := readStateFile(t, dir, "dev")
	if _, ok := st["resources"].(map[string]any)["network"]; ok {
		t.Error("network is still recorded in state after its destroy was applied")
	}
}

// TestConcurrentApplyToOneEnvironmentSerializes proves invariant 5 with two
// genuinely overlapping OS processes, not two sequential invocations. See
// this task's doc comment for exactly what a broken implementation looks
// like under this test.
func TestConcurrentApplyToOneEnvironmentSerializes(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  network:
    type: fake.network
    cidr: 10.20.0.0/16
`)
	seedCloudLatency(t, dir, 500)

	first, firstOut, firstErr := startAsync(t, dir, "apply", "dev", "--auto-approve")
	time.Sleep(150 * time.Millisecond) // give the first apply time to lock and enter its slow Create

	second := run(t, dir, "apply", "dev", "--auto-approve")

	if err := first.Wait(); err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			t.Fatalf("waiting for the first apply: %v", err)
		}
	}
	firstCode := first.ProcessState.ExitCode()
	if firstCode != 2 {
		t.Fatalf("first apply exit code %d, want 2 (it should have won the lock and applied)\n%s%s",
			firstCode, firstOut.String(), firstErr.String())
	}
	requireContains(t, firstOut.String(), "Apply complete:")

	if second.ExitCode != 1 {
		t.Fatalf("second apply exit code %d, want 1 (the environment was already locked)\n%s", second.ExitCode, second.combined())
	}
	requireContains(t, second.Stderr, "is locked")
	requireContains(t, second.Stderr, "apply")

	count := 0
	for _, r := range readFakeCloudResources(t, dir) {
		if addr, _ := r["address"].(string); addr == "network" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("fake cloud has %d resources at address \"network\", want exactly 1 — "+
			"a lock that failed to serialize the two applies let both create it", count)
	}
}

// TestApplyCreatesDependencyBeforeDependent proves invariant 4. zzz_network
// sorts AFTER database alphabetically — the opposite of the order
// dependency resolution requires — so a walker that silently fell back to
// declaration or alphabetical order instead of real dependency edges would
// create database first, and this test would catch it via the cloud's
// globally increasing id counter.
func TestApplyCreatesDependencyBeforeDependent(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  database:
    type: fake.database
    engine: postgres
    network: ${zzz_network.id}
  zzz_network:
    type: fake.network
    cidr: 10.20.0.0/16
`)

	res := run(t, dir, "apply", "dev", "--auto-approve")
	if res.ExitCode != 2 {
		t.Fatalf("apply exit code %d, want 2\n%s", res.ExitCode, res.combined())
	}

	netID := cloudResourceID(t, dir, "zzz_network")
	dbID := cloudResourceID(t, dir, "database")
	netN := trailingNumber(t, netID)
	dbN := trailingNumber(t, dbID)
	if !(netN < dbN) {
		t.Errorf("zzz_network (id %s) was not created before database (id %s) — invariant 4 violated", netID, dbID)
	}

	// The dependent's reference resolved to the real id, not an unresolved
	// placeholder — proof the executor deferred the expression until
	// zzz_network actually completed (spec §15), not just that the two
	// creates happened to land in some order for an unrelated reason.
	if got := readStateAttribute(t, dir, "dev", "database", "network"); got != netID {
		t.Errorf("database.network = %q, want %q (zzz_network's real id)", got, netID)
	}
}

func trailingNumber(t *testing.T, id string) int {
	t.Helper()
	i := strings.LastIndex(id, "-")
	if i < 0 {
		t.Fatalf("cloud id %q has no numeric suffix", id)
	}
	n, err := strconv.Atoi(id[i+1:])
	if err != nil {
		t.Fatalf("cloud id %q has a non-numeric suffix: %v", id, err)
	}
	return n
}

// TestDestroyRequiresTypedEnvironmentName exercises task 13's confirmation
// design end to end against the real binary: a bare "y" (or any word other
// than the environment's own name) must be refused, and the exact name must
// be accepted.
func TestDestroyRequiresTypedEnvironmentName(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  network:
    type: fake.network
    cidr: 10.20.0.0/16
`)
	if res := run(t, dir, "apply", "dev", "--auto-approve"); res.ExitCode != 2 {
		t.Fatalf("setup apply failed: exit %d\n%s", res.ExitCode, res.combined())
	}

	bare := runStdin(t, dir, "y\n", "destroy", "dev")
	if bare.ExitCode != 1 {
		t.Fatalf("destroy with a bare y exit code %d, want 1\n%s", bare.ExitCode, bare.combined())
	}
	stillThere := readStateFile(t, dir, "dev")
	if _, ok := stillThere["resources"].(map[string]any)["network"]; !ok {
		t.Error("network was removed from state despite typed confirmation being refused")
	}

	confirmed := runStdin(t, dir, "dev\n", "destroy", "dev")
	if confirmed.ExitCode != 2 {
		t.Fatalf("destroy with the correct typed confirmation exit code %d, want 2\n%s", confirmed.ExitCode, confirmed.combined())
	}
	st := readStateFile(t, dir, "dev")
	if _, ok := st["resources"].(map[string]any)["network"]; ok {
		t.Error("network is still recorded in state after a confirmed destroy")
	}
}

// TestApplyOnFirstInterruptFinishesInFlightWorkThenReleasesTheLock proves the
// first half of §15's contract: a SIGINT stops the SCHEDULING of new work but
// does not abort the provider call already running. The fixture has two
// resources with NO dependency between them and parallelism forced to 1, so
// exactly one is in flight when the signal lands and the other has not
// started. What this catches: an implementation that cancels the operation's
// own context along with the run's would abort the in-flight Create, and
// "first" would be absent from the fake cloud; one that ignores the signal
// entirely would create BOTH.
func TestApplyOnFirstInterruptFinishesInFlightWorkThenReleasesTheLock(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  first:
    type: fake.network
    cidr: 10.20.0.0/16
  second:
    type: fake.network
    cidr: 10.21.0.0/16
`)
	// seedCloudLatency high enough (800ms) that the signal at 200ms lands
	// well inside the first Create, with margin on a loaded CI box.
	seedCloudLatency(t, dir, 800)

	cmd, stdout, stderr := startAsync(t, dir, "apply", "dev", "--auto-approve", "--parallelism", "1")
	time.Sleep(200 * time.Millisecond)
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("sending SIGINT: %v", err)
	}

	waitErr := cmd.Wait()
	if waitErr == nil {
		t.Fatalf("process exited 0, want non-zero (interrupted is not success)\n%s%s", stdout.String(), stderr.String())
	}
	if _, ok := waitErr.(*exec.ExitError); !ok {
		t.Fatalf("waiting for the interrupted apply: %v", waitErr)
	}
	code := cmd.ProcessState.ExitCode()
	if code == 0 {
		t.Fatalf("interrupted apply exited 0, want non-zero\n%s%s", stdout.String(), stderr.String())
	}

	cloud := readFakeCloudResources(t, dir)
	var addrs []string
	for _, r := range cloud {
		if addr, ok := r["address"].(string); ok {
			addrs = append(addrs, addr)
		}
	}
	if len(addrs) != 1 || addrs[0] != "first" {
		t.Fatalf("fake cloud holds %v, want exactly [\"first\"] (the resource in flight when SIGINT landed)\n%s%s",
			addrs, stdout.String(), stderr.String())
	}

	st := readStateFile(t, dir, "dev")
	resources, _ := st["resources"].(map[string]any)
	if _, ok := resources["first"]; !ok {
		t.Errorf("state does not record %q after the interrupted apply persisted it", "first")
	}
	if _, ok := resources["second"]; ok {
		t.Errorf("state records %q, which should never have started", "second")
	}

	// The lock must be released: a subsequent apply must not be refused.
	next := run(t, dir, "apply", "dev", "--auto-approve")
	if strings.Contains(next.Stderr, "is locked") {
		t.Fatalf("subsequent apply refused as locked — the interrupted run left the lock held:\n%s", next.combined())
	}
}

// TestApplyOnSecondInterruptExitsImmediatelyAndLeavesTheLockStale proves the
// second half: a user who interrupts twice is saying "stop NOW", and the tool
// obeys at the cost of leaving the lock behind — which the NEXT run must
// report as stale, naming who holds it and the command to clear it, rather
// than hanging or silently stealing it.
func TestApplyOnSecondInterruptExitsImmediatelyAndLeavesTheLockStale(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  first:
    type: fake.network
    cidr: 10.20.0.0/16
  second:
    type: fake.network
    cidr: 10.21.0.0/16
`)
	seedCloudLatency(t, dir, 800)

	cmd, stdout, stderr := startAsync(t, dir, "apply", "dev", "--auto-approve", "--parallelism", "1")
	time.Sleep(200 * time.Millisecond)
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("sending first SIGINT: %v", err)
	}
	time.Sleep(150 * time.Millisecond) // total 350ms, still inside the 800ms Create
	before := time.Now()
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("sending second SIGINT: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case waitErr := <-done:
		elapsed := time.Since(before)
		// The in-flight Create had ~450ms left (800ms total, 350ms
		// elapsed) when the second signal landed. Requiring exit well
		// under that window is what distinguishes "exited immediately"
		// from "finished normally" — an assertion on exit code alone
		// cannot tell the two apart.
		if elapsed > 300*time.Millisecond {
			t.Errorf("process took %v to exit after the second SIGINT, want well under 300ms (it should not have waited for the in-flight Create)", elapsed)
		}
		if waitErr == nil {
			t.Fatalf("process exited 0, want non-zero\n%s%s", stdout.String(), stderr.String())
		}
		if _, ok := waitErr.(*exec.ExitError); !ok {
			t.Fatalf("waiting for the twice-interrupted apply: %v", waitErr)
		}
	case <-time.After(600 * time.Millisecond):
		t.Fatal("process did not exit within 600ms of the second SIGINT — it waited out the in-flight Create instead of exiting immediately")
	}

	lockPath := filepath.Join(dir, ".infra", "state", "dev.lock")
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock file %s does not exist after a double interrupt, want it left behind stale: %v", lockPath, err)
	}

	next := run(t, dir, "apply", "dev", "--auto-approve")
	if next.ExitCode != 1 {
		t.Fatalf("subsequent apply against a stale lock exit code %d, want 1\n%s", next.ExitCode, next.combined())
	}
	requireContains(t, next.Stderr, "is locked")
	// Named specifically, not just "locked": the holder's actual pid (the
	// interrupted subprocess, not this test process) must appear, and the
	// message must name the remedy.
	requireContains(t, next.Stderr, strconv.Itoa(cmd.Process.Pid))
	requireContains(t, next.Stderr, "infra state unlock")
}

// TestSensitiveValueNeverAppearsInCommandOutput closes the M3 DoD line this
// task's own first report flagged as unmet: "a sensitive value never
// appears in apply output, summary output, or a progress event, at any
// nesting depth" — checked against the REAL BINARY, across plan, apply,
// refresh AND destroy. Nothing before this drove all four: M2's
// TestPlanNeverLeaksASecret (m2_test.go) only drives plan, and the shared
// redaction path pkg/value.Format is otherwise proven only at the unit
// level, through executor.Render (TestRenderRedactsSensitiveAttributes) —
// never through a real subprocess for apply, refresh or destroy. §36 is a
// core product requirement specifically because M2 shipped a plaintext
// password once already, from two redaction paths that had diverged; the
// whole point of one shared renderer is undermined if nothing checks the
// actual commands that call it.
//
// NOTE on nesting depth. The request behind this test asked for a
// sensitive value nested inside a map inside a list, to probe deeper than
// M2's top-level secret did. That fixture cannot be built through
// configuration as M3 actually ships, confirmed against the real binary
// while writing this test:
//
//	Error: interpolation inside a map is not supported
//	  Expressions may appear in string values only.
//
// (internal/config/decode.go's decodeScalar only recognises "${" at a bare
// scalar node — decodeValue's Sequence/MappingNode branches recurse into
// composites but never call it on the composite itself, so a reference
// inside a list or map is rejected at compile time, before any value ever
// exists to leak). No schema attribute in providers/test declares a
// composite with its own sensitive leaf either — schema.go's markSensitive
// marks a whole attribute's root Value, never a leaf inside one. The
// deepest sensitivity this engine can construct today is a schema-declared
// Sensitive SCALAR attribute (fake.database.password) — the same depth
// M2's own test already used. What this test adds over M2's is coverage of
// apply/refresh/destroy (M2 only had plan), plus a sibling, deliberately
// VISIBLE value in the same composite (tags.visible_marker) alongside the
// secret: pkg/value.Format's actual contract is per-leaf redaction, not
// "hide everything near a secret," and asserting the sibling still renders
// is what tells those two apart — a renderer that blanked the whole
// resource near any sensitive field would also pass a test that only
// checked the secret's absence.
func TestSensitiveValueNeverAppearsInCommandOutput(t *testing.T) {
	const secret = "correct-horse-battery-staple"
	const marker = "not-a-secret-marker"
	dir := project(t, `
project: myapp
resources:
  network:
    type: fake.network
    cidr: 10.20.0.0/16
  database:
    type: fake.database
    engine: postgres
    password: `+secret+`
    network: ${network.id}
    tags:
      visible_marker: `+marker+`
`)

	// assertSecretAbsent checks BOTH streams, deliberately: the point is
	// that the literal secret must never appear on either one, so it does
	// not matter which stream a leak would land on (same reasoning as M2's
	// TestPlanNeverLeaksASecret).
	assertSecretAbsent := func(t *testing.T, label string, res result) {
		t.Helper()
		if strings.Contains(res.Stdout, secret) {
			t.Errorf("%s: secret leaked on stdout:\n%s", label, res.Stdout)
		}
		if strings.Contains(res.Stderr, secret) {
			t.Errorf("%s: secret leaked on stderr:\n%s", label, res.Stderr)
		}
	}

	p := run(t, dir, "plan", "dev")
	if p.ExitCode != 2 {
		t.Fatalf("plan exit code %d, want 2\n%s", p.ExitCode, p.combined())
	}
	assertSecretAbsent(t, "plan", p)
	requireContains(t, p.Stdout, "<sensitive>")
	// The sibling value in the SAME composite attribute must still render —
	// proof this is selective, per-leaf redaction, not a blanket hide.
	requireContains(t, p.Stdout, marker)

	a := run(t, dir, "apply", "dev", "--auto-approve", "--verbose")
	if a.ExitCode != 2 {
		t.Fatalf("apply exit code %d, want 2\n%s", a.ExitCode, a.combined())
	}
	assertSecretAbsent(t, "apply", a)
	requireContains(t, a.Stdout, "<sensitive>")
	requireContains(t, a.Stdout, marker)

	// §12.2 permits plaintext state at rest, at 0600 — a documented §53
	// trade-off, not an oversight, and not something a future reader should
	// "fix" by adding state-file redaction. This asserts the mode instead
	// of the mode's absence of plaintext, precisely to document that
	// distinction rather than let a later change silently narrow it either
	// way.
	statePath := filepath.Join(dir, ".infra", "state", "dev.json")
	info, err := os.Stat(statePath)
	if err != nil {
		t.Fatalf("stat state file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("state file mode = %v, want 0600 (plaintext-at-rest is only acceptable under this permission bit)", info.Mode().Perm())
	}
	stateBytes, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("reading state file: %v", err)
	}
	if !strings.Contains(string(stateBytes), secret) {
		t.Error("state file does not contain the plaintext secret — expected under §12.2; if this changed, state-level encryption may have landed and this test (and its comment) need updating, not deleting")
	}

	r := run(t, dir, "refresh", "dev", "--verbose")
	if r.ExitCode != 0 {
		t.Fatalf("refresh exit code %d, want 0\n%s", r.ExitCode, r.combined())
	}
	assertSecretAbsent(t, "refresh", r)

	d := runStdin(t, dir, "dev\n", "destroy", "dev")
	if d.ExitCode != 2 {
		t.Fatalf("destroy exit code %d, want 2\n%s", d.ExitCode, d.combined())
	}
	assertSecretAbsent(t, "destroy", d)
	requireContains(t, d.Stdout, "<sensitive>")
	requireContains(t, d.Stdout, marker)
}

// setCloudLatency adds a simulated per-operation delay to an EXISTING fake
// cloud, preserving the resources already in it. seedCloudLatency above
// writes a fresh, empty document, which is right for a test whose apply is
// about to create everything and wrong for one that must first seed
// resources and then slow their deletion down.
func setCloudLatency(t *testing.T, dir string, ms int) {
	t.Helper()
	path := filepath.Join(dir, ".infra", "fake-cloud.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading fake cloud: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parsing fake cloud: %v", err)
	}
	doc["latency_ms"] = ms
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal fake cloud: %v", err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatalf("write fake cloud: %v", err)
	}
}

// waitForLock blocks until environment's lock file exists, so a test can
// signal a run at a point it KNOWS the lock is held rather than at a
// wall-clock guess. Polling a file the run creates is the only ordering
// signal available across a process boundary, and it is exact: the lock
// existing is precisely the condition whose cleanup is under test.
func waitForLock(t *testing.T, dir, environment string) {
	t.Helper()
	path := filepath.Join(dir, ".infra", "state", environment+".lock")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s to be locked", environment)
}

// TestDestroyOnInterruptExitsCleanlyAndReleasesTheLock is the destroy-side
// counterpart of the apply interrupt test above.
//
// It exists because measurement inverted an assumption: removing
// runInterruptible from apply's copy of the lock preamble was caught here,
// and from refresh's copy was caught by internal/cli, but removing it from
// destroy's copy failed NOTHING in the whole suite. Destroy is the command
// where a stranded lock is worst — the environment is mid-teardown, and the
// next destroy is refused — and it was the one nothing pinned.
//
// Without the wrapper, ctx is never cancellable and SIGINT gets its default
// disposition: the process dies where it stands, with the lock file still
// on disk. With it, the run unwinds through its deferred release. So the
// discriminating assertion is the lock, not the exit code — and the signal
// is sent once the lock file is observed to exist, which makes the test
// exact rather than a wall-clock guess.
//
// The structural fix (withLockedEnvironment, internal/cli/interrupt.go) is
// what makes the omission unrepresentable going forward; this pins the
// behaviour for the one command that had no evidence of it at all.
func TestDestroyOnInterruptExitsCleanlyAndReleasesTheLock(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  first:
    type: fake.network
    cidr: 10.20.0.0/16
  second:
    type: fake.network
    cidr: 10.21.0.0/16
`)
	// Exit code 2 is "changes were applied" (root.go's errChanges arm), not
	// a failure — the same code the MVP round trip asserts for a first apply.
	if res := run(t, dir, "apply", "dev", "--auto-approve"); res.ExitCode != 2 {
		t.Fatalf("seeding apply exit code %d, want 2:\n%s", res.ExitCode, res.combined())
	}
	// Slow enough that the run is still inside the lock when the signal
	// lands, however loaded the machine is.
	setCloudLatency(t, dir, 1500)

	cmd, stdout, stderr := startAsync(t, dir, "destroy", "dev", "--auto-approve", "--parallelism", "1")
	waitForLock(t, dir, "dev")
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("sending SIGINT: %v", err)
	}

	waitErr := cmd.Wait()
	if waitErr == nil {
		t.Fatalf("process exited 0, want non-zero (interrupted is not success)\n%s%s", stdout.String(), stderr.String())
	}
	if _, ok := waitErr.(*exec.ExitError); !ok {
		t.Fatalf("waiting for the interrupted destroy: %v", waitErr)
	}

	lockPath := filepath.Join(dir, ".infra", "state", "dev.lock")
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("the lock on dev is still held after an interrupted destroy (Stat: %v)\n%s%s",
			err, stdout.String(), stderr.String())
	}

	// The lock file being gone is the mechanism; this is the consequence a
	// user actually meets.
	next := run(t, dir, "destroy", "dev", "--auto-approve")
	if strings.Contains(next.Stderr, "is locked") {
		t.Fatalf("subsequent destroy refused as locked — the interrupted run left the lock held:\n%s", next.combined())
	}
}
