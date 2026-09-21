package integration

import (
	"bytes"
	"encoding/json"
	"errors"
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
		t.Fatalf("starting infrena %v: %v", args, err)
	}
	return cmd, &stdout, &stderr
}

// runStdin is run with stdin controlled, for the typed-confirmation tests.
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
			t.Fatalf("running infrena %v: %v", args, err)
		}
		code = exitErr.ExitCode()
	}
	return result{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: code}
}

// seedCloudLatency writes .infrena/fake-cloud.json with no resources and a
// simulated per-operation delay, so a subsequent apply's Create call is
// slow enough to overlap a second process's attempt to lock the
// environment.
func seedCloudLatency(t *testing.T, dir string, ms int) {
	t.Helper()
	cloudDir := filepath.Join(dir, ".infrena")
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
	data, err := os.ReadFile(filepath.Join(dir, ".infrena", "fake-cloud.json"))
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
	path := filepath.Join(dir, ".infrena", "fake-cloud.json")
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
	data, err := os.ReadFile(filepath.Join(dir, ".infrena", "state", environment+".json"))
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
	data, err := os.ReadFile(filepath.Join(dir, ".infrena", "state", environment+".json"))
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

// TestM3MVPRoundTrip walks the whole core workflow in one go: validate, plan,
// apply, re-plan clean, drift, refresh, remove, destroy. It starts from a
// hand-written infrena.yml rather than `infrena init`.
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
	if _, err := os.Stat(filepath.Join(dir, ".infrena", "state", "dev.json")); !os.IsNotExist(err) {
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

	// Externally mutate .infrena/fake-cloud.json by hand, standing in for a
	// person changing real infrastructure outside infrena entirely.
	id := cloudResourceID(t, dir, "network")
	mutateCloudAttribute(t, dir, id, "cidr", "10.99.0.0/16")

	p3 := run(t, dir, "plan", "dev")
	if p3.ExitCode != 2 {
		t.Fatalf("drift plan exit code %d, want 2\n%s", p3.ExitCode, p3.combined())
	}
	// cidr is ForceNew (providers/test/definitions.go), so drifting it
	// proposes a replacement rather than an in-place update.
	requireContains(t, p3.Stdout, "-/+ fake.network.network")
	requireContains(t, p3.Stdout, "replacement forced by: cidr")
	requireContains(t, p3.Stdout, `"10.99.0.0/16" -> "10.20.0.0/16"`)

	// plan never writes the drift down; refresh does.
	r1 := run(t, dir, "refresh", "dev")
	if r1.ExitCode != 0 {
		t.Fatalf("refresh exit code %d, want 0\n%s", r1.ExitCode, r1.combined())
	}
	if got := readStateAttribute(t, dir, "dev", "network", "cidr"); got != "10.99.0.0/16" {
		t.Errorf("refresh did not persist the observed drift: state cidr = %q, want %q", got, "10.99.0.0/16")
	}

	// Remove the resource from configuration.
	if err := os.WriteFile(filepath.Join(dir, "infrena.yml"), []byte("project: myapp\nresources: {}\n"), 0o644); err != nil {
		t.Fatalf("rewriting infrena.yml: %v", err)
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

// TestConcurrentApplyToOneEnvironmentSerializes pins that the environment lock
// holds against two genuinely overlapping OS processes, not two sequential
// invocations: the second apply must be refused, and the cloud must end up with
// one network rather than two.
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

// TestApplyCreatesDependencyBeforeDependent. zzz_network sorts AFTER database
// alphabetically — the opposite of the order the dependency requires — so a
// walker that fell back to declaration or alphabetical order instead of real
// dependency edges creates database first, which the cloud's globally
// increasing id counter reveals.
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
		t.Errorf("zzz_network (id %s) was not created before database (id %s) — a dependency must be created before the resource that references it", netID, dbID)
	}

	// The dependent's reference resolved to the real id rather than an
	// unresolved placeholder, so the executor genuinely deferred the expression
	// until zzz_network completed, rather than the two creates happening to
	// land in that order for an unrelated reason.
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

// TestDestroyRequiresTypedEnvironmentName, end to end against the real binary:
// a bare "y", or any word other than the environment's own name, must be
// refused, and the exact name must be accepted.
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

// TestApplyOnFirstInterruptFinishesInFlightWorkThenReleasesTheLock: a SIGINT
// stops the SCHEDULING of new work but does not abort the provider call already
// running.
//
// Two resources with NO dependency between them and parallelism forced to 1, so
// exactly one is in flight when the signal lands. Cancelling the operation's own
// context along with the run's aborts that Create and `first` is absent from the
// cloud; ignoring the signal creates BOTH.
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
	// High enough that the signal at 200ms lands well inside the first Create,
	// with margin on a loaded CI box.
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

// TestApplyOnSecondInterruptExitsImmediatelyAndLeavesTheLockStale is the other
// half: a user who interrupts twice is saying "stop NOW", and the tool obeys at
// the cost of leaving the lock behind — which the NEXT run must report as stale,
// naming who holds it and the command to clear it, rather than hanging or
// silently stealing it.
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
		// The in-flight Create had ~450ms left when the second signal
		// landed, so requiring exit well under that window is what tells
		// "exited immediately" from "finished normally". Exit code alone
		// cannot.
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

	lockPath := filepath.Join(dir, ".infrena", "state", "dev.lock")
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock file %s does not exist after a double interrupt, want it left behind stale: %v", lockPath, err)
	}

	next := run(t, dir, "apply", "dev", "--auto-approve")
	if next.ExitCode != 1 {
		t.Fatalf("subsequent apply against a stale lock exit code %d, want 1\n%s", next.ExitCode, next.combined())
	}
	requireContains(t, next.Stderr, "is locked")
	// Named specifically, not just "locked": the holder's actual pid — the
	// interrupted subprocess, not this test process — and the remedy.
	requireContains(t, next.Stderr, strconv.Itoa(cmd.Process.Pid))
	requireContains(t, next.Stderr, "infrena state unlock")
}

// TestSensitiveValueNeverAppearsInCommandOutput drives all four commands that
// can print a value — plan, apply, refresh and destroy — against the REAL
// BINARY. The shared redaction path pkg/value.Format is otherwise proven only
// at the unit level, and one shared renderer is worth nothing if nothing checks
// the commands that call it. Two redaction paths that had diverged is how a
// plaintext password shipped once already.
//
// The fixture also carries a deliberately VISIBLE sibling in the same composite
// attribute (tags.visible_marker). Format's contract is per-leaf redaction, not
// "hide everything near a secret", and asserting the sibling still renders is
// what tells those apart: a renderer that blanked the whole resource would pass
// a test that only checked the secret's absence.
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

	// BOTH streams, deliberately: the literal secret must never appear on
	// either one, so it does not matter which a leak would land on.
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
	// The sibling value in the SAME composite attribute must still render:
	// per-leaf redaction, not a blanket hide.
	requireContains(t, p.Stdout, marker)

	a := run(t, dir, "apply", "dev", "--auto-approve", "--verbose")
	if a.ExitCode != 2 {
		t.Fatalf("apply exit code %d, want 2\n%s", a.ExitCode, a.combined())
	}
	assertSecretAbsent(t, "apply", a)
	requireContains(t, a.Stdout, "<sensitive>")
	requireContains(t, a.Stdout, marker)

	// Plaintext state at rest, protected by mode 0600, is a deliberate
	// trade-off rather than an oversight — do not "fix" it by adding
	// state-file redaction. Asserting the MODE, and the plaintext below it,
	// is what stops a later change narrowing that either way in silence.
	statePath := filepath.Join(dir, ".infrena", "state", "dev.json")
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
		t.Error("state file does not contain the plaintext secret — plaintext state at rest is deliberate; if this changed, state-level encryption may have landed and this test (and its comment) need updating, not deleting")
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
// cloud, preserving the resources already in it. seedCloudLatency above writes
// a fresh, empty document instead, which is right for a test whose apply is
// about to create everything and wrong for one that must seed resources first
// and then slow their deletion down.
func setCloudLatency(t *testing.T, dir string, ms int) {
	t.Helper()
	path := filepath.Join(dir, ".infrena", "fake-cloud.json")
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

// waitForLock blocks until environment's lock file exists, so a test can signal
// a run at a point it KNOWS the lock is held rather than at a wall-clock guess.
// The lock existing is precisely the condition whose cleanup is under test.
func waitForLock(t *testing.T, dir, environment string) {
	t.Helper()
	path := filepath.Join(dir, ".infrena", "state", environment+".lock")
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
// counterpart of the apply interrupt test above, and nothing else covers it:
// take the interruptible wrapper off destroy's lock preamble and the rest of
// the suite still passes. Destroy is where a stranded lock is worst, because
// the environment is mid-teardown and the next destroy is refused.
//
// Without that wrapper ctx is never cancellable, SIGINT gets its default
// disposition, and the process dies where it stands with the lock file still on
// disk. So the discriminating assertion is the LOCK, not the exit code, and the
// signal is sent once the lock file is observed to exist rather than at a
// wall-clock guess.
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
	// Exit code 2 is "changes were applied", not a failure.
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

	lockPath := filepath.Join(dir, ".infrena", "state", "dev.lock")
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

// waitAsync blocks until a startAsync'd invocation finishes and returns what it
// produced, so a test starting two processes reads their results the same way
// run() reads one. Only the exit status is special-cased: a non-zero exit is an
// ExitError, which is a result here and not a harness failure.
func waitAsync(t *testing.T, label string, cmd *exec.Cmd, stdout, stderr *bytes.Buffer) result {
	t.Helper()
	if err := cmd.Wait(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("waiting for the %s apply: %v", label, err)
		}
	}
	return result{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: cmd.ProcessState.ExitCode()}
}

// TestConcurrentApplyToTwoEnvironmentsOverlaps is the other half of the
// concurrency pair: TestConcurrentApplyToOneEnvironmentSerializes proves two
// applies to ONE environment cannot overlap, and this proves two applies to
// DIFFERENT environments do — the lock is per environment, not global.
//
// Asserting only that both succeeded would be a test that cannot fail. Put a
// single global lock in front of the state backend and the two applies would
// serialise, both would still exit 2 with "Apply complete:", both state files
// would still be written, and this test would go on passing while the property
// it is named for had been destroyed.
//
// DO NOT REPLACE THIS WITH A WALL-CLOCK BOUND. A total duration cannot tell a
// serialised run on a fast machine from an overlapping run on a loaded CI
// runner, so such a bound fails ambiguously and cannot be read either way.
//
// It asserts OVERLAP directly instead. Each apply writes a report stream
// carrying an absolute timestamp on every event, so each run has a real
// interval — first `started` to last `succeeded` — and the two ran at the same
// time exactly when their intervals intersect. A global lock makes them
// disjoint however fast or slow the machine is.
//
// The overlap must be at least half the shorter interval, not merely non-zero:
// two runs sharing a single millisecond intersect while being serial in every
// way that matters, and a ratio stays machine-independent where a duration does
// not. latency_ms is 500 only to make the intervals comfortably wider than the
// clock's resolution.
func TestConcurrentApplyToTwoEnvironmentsOverlaps(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  network:
    type: fake.network
    cidr: 10.20.0.0/16
`)
	seedCloudLatency(t, dir, 500)

	// Build the CLI and the fake plugin BEFORE the applies start. Both are
	// sync.Once'd, so if this test runs first one apply is charged for the
	// compile and starts measurably after the other.
	binary(t)
	fakePluginDir(t)

	devReport := filepath.Join(t.TempDir(), "dev.ndjson")
	prodReport := filepath.Join(t.TempDir(), "production.ndjson")

	devCmd, devOut, devErr := startAsync(t, dir, "apply", "dev", "--auto-approve", "--output", devReport)
	prodCmd, prodOut, prodErr := startAsync(t, dir, "apply", "production", "--auto-approve", "--output", prodReport)

	dev := waitAsync(t, "dev", devCmd, devOut, devErr)
	production := waitAsync(t, "production", prodCmd, prodOut, prodErr)

	for _, c := range []struct {
		environment string
		res         result
	}{{"dev", dev}, {"production", production}} {
		if c.res.ExitCode != 2 {
			t.Fatalf("%s apply exit code %d, want 2 (changes applied)\n%s", c.environment, c.res.ExitCode, c.res.combined())
		}
		// NOT an assertion on stdout: `--output` sends the run to the file and
		// silences stdout entirely, which is the whole point of that flag.
	}

	devStart, devEnd := applyEventWindow(t, "dev", devReport)
	prodStart, prodEnd := applyEventWindow(t, "production", prodReport)

	// The intersection of two intervals: latest start to earliest end. Written
	// out because Go's min/max builtins are for ordered types and time.Time is
	// not one.
	latestStart, earliestEnd := devStart, devEnd
	if prodStart.After(latestStart) {
		latestStart = prodStart
	}
	if prodEnd.Before(earliestEnd) {
		earliestEnd = prodEnd
	}
	overlap := earliestEnd.Sub(latestStart)
	shorter := min(devEnd.Sub(devStart), prodEnd.Sub(prodStart))
	if overlap <= 0 {
		t.Errorf("the two applies did not overlap at all: dev ran %v-%v, production ran %v-%v — "+
			"they ran one after the other, so something is serialising environments that should be independent",
			devStart, devEnd, prodStart, prodEnd)
	} else if overlap*2 < shorter {
		t.Errorf("the two applies overlapped by only %v of a %v window (under half) — "+
			"they are barely concurrent, which is what partial serialisation looks like",
			overlap, shorter)
	}
	t.Logf("dev ran %v-%v, production ran %v-%v, overlapping by %v of a %v window",
		devStart, devEnd, prodStart, prodEnd, overlap, shorter)

	// Both environments have their own state, each holding its own copy of the
	// resource: neither apply wrote over the other's file, and neither saw the
	// other's resource. (The fake cloud file is one file shared by every
	// environment in a project, so it is a harness artifact under concurrent
	// writes and is deliberately not asserted on here — the per-environment
	// state files are what this property is about.)
	for _, environment := range []string{"dev", "production"} {
		st := readStateFile(t, dir, environment)
		if got, _ := st["environment"].(string); got != environment {
			t.Errorf("%s.json records environment %q", environment, got)
		}
		resources, ok := st["resources"].(map[string]any)
		if !ok {
			t.Fatalf("%s.json has no resources object: %#v", environment, st["resources"])
		}
		if _, ok := resources["network"]; !ok {
			t.Errorf("%s.json does not record the network it applied: %#v", environment, resources)
		}
	}
}
