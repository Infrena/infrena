package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infrena/infrena/internal/state"
	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/resource"
	"github.com/infrena/infrena/pkg/value"
	testprovider "github.com/infrena/infrena/providers/test"
)

func TestPlanProposesCreatesOnFreshProject(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  network:
    type: fake.network
    cidr: 10.20.0.0/16
`)
	opts := &GlobalOptions{Dir: dir, Parallelism: 4}
	cmd := newPlanCommand(opts)
	cmd.SetArgs([]string{"dev"})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := cmd.Execute()
	if !errors.Is(err, errChanges) {
		t.Fatalf("Execute() = %v, want errChanges — a fresh project always has changes", err)
	}
	// An exact match, not a substring check. "Contains" cannot fail on the
	// exact defect that hid in this command until the SilenceUsage/
	// SilenceErrors fix: cobra printing "Usage: ...\n" to stdout alongside
	// the plan on this very success-with-changes path (errChanges is a
	// non-nil error, which is what triggers cobra's post-RunE usage print
	// when a command's own Silence* fields are unset). Comparing the whole
	// rendered text is the only assertion that would have caught it.
	want := "Plan for project \"myapp\", environment \"dev\":\n" +
		"\n" +
		"  + fake.network.network\n" +
		"      cidr: \"10.20.0.0/16\"\n" +
		"      id: (known after apply)\n" +
		"\n" +
		"Plan: 1 to create, 0 to update, 0 to replace, 0 to destroy, 0 to forget.\n"
	if stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
}

func TestPlanRendersDiagnosticsToStderrOnInvalidConfig(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  database:
    type: aws.rds
    engine: postgres
`)
	opts := &GlobalOptions{Dir: dir, Parallelism: 4}
	cmd := newPlanCommand(opts)
	cmd.SetArgs([]string{"dev"})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := cmd.Execute()
	if err == nil || errors.Is(err, errChanges) {
		t.Fatalf("Execute() = %v, want a plain error for invalid configuration", err)
	}
	if !strings.Contains(stderr.String(), "aws.rds") {
		t.Errorf("stderr does not name the offending type:\n%s", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("no plan should reach stdout when configuration fails to compile, got:\n%s", stdout.String())
	}
}

func TestPlanNeverWritesStateOrTakesTheLock(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  network:
    type: fake.network
    cidr: 10.20.0.0/16
`)
	opts := &GlobalOptions{Dir: dir, Parallelism: 4}
	cmd := newPlanCommand(opts)
	cmd.SetArgs([]string{"dev"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	_ = cmd.Execute()

	if _, err := os.Stat(filepath.Join(dir, ".infra", "state", "dev.json")); !os.IsNotExist(err) {
		t.Error("plan must never write state")
	}
	if _, err := os.Stat(filepath.Join(dir, ".infra", "state", "dev.lock")); !os.IsNotExist(err) {
		t.Error("plan must never take the environment lock")
	}
}

// The file is a report stream now, not a bare artifact — one format for
// every command (spec 2.4) — but it is still 0600, because the plan line it
// carries holds the cleartext values apply --plan reads back (spec §12.2).
func TestPlanOutputWritesA0600ReportStreamCarryingThePlan(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  network:
    type: fake.network
    cidr: 10.20.0.0/16
`)
	outPath := filepath.Join(t.TempDir(), "plan.ndjson")
	opts := &GlobalOptions{Dir: dir, Parallelism: 4, Output: outPath}
	cmd := newPlanCommand(opts)
	cmd.SetArgs([]string{"dev"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	_ = cmd.Execute()

	info, err := os.Stat(outPath)
	if err != nil {
		t.Fatalf("--output did not write a file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("plan file mode = %v, want 0600", info.Mode().Perm())
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading plan file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 2 {
		t.Fatalf("--output is not a stream:\n%s", data)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &meta); err != nil {
		t.Fatalf("first line is not valid JSON: %v", err)
	}
	if meta["type"] != "meta" || meta["command"] != "plan" {
		t.Errorf("first line is not plan's meta line: %s", lines[0])
	}

	// Read back through the same door apply --plan uses, which is the only
	// assertion that says the artifact survived the envelope.
	p, err := readSavedPlan(outPath)
	if err != nil {
		t.Fatalf("readSavedPlan on plan --output: %v", err)
	}
	if !p.HasChanges() {
		t.Error("the plan line carries no operations")
	}
}

// TestPlanFailsOnPreventDestroy guards the plan-time-error integration point:
// a resource refused by prevent_destroy at plan time must fail the command
// (exit 1) rather than render an approvable plan (exit 2). Without
// `ds.Extend(planDiags)` in plan.go, Compute's diagnostic never reaches the
// command's own working set and the prevent_destroy refusal would be
// invisible — RunE would see zero operations (operationFor returns a nil
// operation for a resource it refused) and report success with no changes.
// That is proven RED below.
//
// It does NOT also require extending ds with p.Diagnostics. Reading
// planner.Compute shows every return path copies the exact ds it is about to
// return into p.Diagnostics, so the two are always identical in content —
// confirmed by internal/planner's own TestPreventDestroyIsAPlanTimeError,
// which asserts against Compute's returned diagnostics directly and never
// touches p.Diagnostics. plan.go used to extend ds with p.Diagnostics too;
// that duplicated every plan-time diagnostic on stderr (verified: the
// prevent_destroy message printed twice), so it was removed. The count
// assertion below guards against that regressing.
//
// It seeds a resource directly through the fake provider (rather than
// hand-writing a cloud file) so refresh.Refresh genuinely observes it as
// present — a resource refresh cannot find at all is read by the planner as
// "already gone from the provider" and produces a silent forget, never the
// prevent_destroy refusal this test exists to catch.
func TestPlanFailsOnPreventDestroy(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources: {}
`)
	ctx := context.Background()

	// Seed the fake cloud with a resource, then record it in state with
	// prevent_destroy set — and never declare it in configuration, so plan
	// sees a resource in state but not in configuration: invariant 1's
	// removal case, the one prevent_destroy exists to refuse.
	prov := testprovider.New(filepath.Join(dir, testprovider.DefaultCloudPath))
	rs, err := prov.Create(ctx, &resource.DesiredResource{
		Address: address.Address{Name: "network"},
		Type:    "fake.network",
		Attrs: map[string]value.Value{
			"cidr": value.String("10.20.0.0/16", value.SourceExplicit),
		},
		Lifecycle: resource.Lifecycle{PreventDestroy: true},
	})
	if err != nil {
		t.Fatalf("seeding the fake cloud: %v", err)
	}

	// The state record carries the guard explicitly. The fake provider's
	// Create no longer echoes DesiredResource.Lifecycle back onto the state
	// it returns: the executor stamps lifecycle onto state from the plan
	// (internal/executor/apply.go), so a provider that also set it would be
	// a second writer of a field it does not own. Seeding it here says out
	// loud what this fixture needs — a state record whose guard is set —
	// instead of borrowing it from a provider round trip.
	rs.Lifecycle = resource.Lifecycle{PreventDestroy: true}
	st := state.New("myapp", "dev")
	st.Set(rs)
	b := backendFor(dir)
	if _, err := b.Lock(ctx, "dev"); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if err := b.Put(ctx, "dev", st); err != nil {
		t.Fatalf("seeding state: %v", err)
	}
	if err := b.Unlock(ctx, "dev"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	opts := &GlobalOptions{Dir: dir, Parallelism: 4}
	cmd := newPlanCommand(opts)
	cmd.SetArgs([]string{"dev"})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err = cmd.Execute()
	if err == nil || errors.Is(err, errChanges) {
		t.Fatalf("Execute() = %v, want a plain error — prevent_destroy must fail planning (exit 1), "+
			"not report success with changes (exit 2)", err)
	}
	if !strings.Contains(stderr.String(), "prevent_destroy") {
		t.Errorf("stderr does not name the guard:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "network") {
		t.Errorf("stderr does not name the protected resource:\n%s", stderr.String())
	}
	// "prevent_destroy" itself appears three times within a single rendered
	// diagnostic (summary, detail, and suggested action), so it cannot tell
	// one diagnostic block from two. "Suggested action:" is printed once per
	// diagnostic by diag.Render, so its count is the number of diagnostic
	// blocks on stderr.
	if n := strings.Count(stderr.String(), "Suggested action:"); n != 1 {
		t.Errorf("prevent_destroy diagnostic appears %d times on stderr, want exactly 1 — "+
			"planDiags and p.Diagnostics carry the same content, so extending ds with both duplicates it:\n%s", n, stderr.String())
	}
	// Progress now reaches stdout while the refresh runs ("Reading ...
	// done"), so an empty-stdout assertion would be asserting the absence of
	// that rather than what this test is named for. The property is
	// unchanged: no PLAN is rendered when planning fails.
	if strings.Contains(stdout.String(), "Plan:") {
		t.Errorf("no plan should reach stdout when a plan-time error occurs, got:\n%s", stdout.String())
	}
}
