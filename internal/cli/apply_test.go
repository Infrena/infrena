package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"infra/internal/state"
	"infra/pkg/address"
	"infra/pkg/resource"
	"infra/pkg/value"
	testprovider "infra/providers/test"
)

// seedState writes st directly to the backend, taking and releasing the
// environment lock around the write — state.Local.Put refuses to write
// without a held lock, and this helper keeps that contract rather than
// reaching around it.
func seedState(t *testing.T, dir, environment string, st *state.State) {
	t.Helper()
	backend := backendFor(dir)
	ctx := state.WithOperation(context.Background(), "test-seed")
	if _, err := backend.Lock(ctx, environment); err != nil {
		t.Fatalf("locking to seed state: %v", err)
	}
	defer func() {
		if err := backend.Unlock(ctx, environment); err != nil {
			t.Fatalf("unlocking after seeding state: %v", err)
		}
	}()
	if err := backend.Put(ctx, environment, st); err != nil {
		t.Fatalf("seeding state: %v", err)
	}
}

func TestApplyWithNoChangesTakesNoLockAndDoesNotPrompt(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: 10.20.0.0/16
`)
	ctx := context.Background()
	prov := testprovider.New(filepath.Join(dir, testprovider.DefaultCloudPath))
	rs, err := prov.Create(ctx, &resource.DesiredResource{
		Address: address.Address{Name: "network"},
		Type:    "test.network",
		Attrs: map[string]value.Value{
			"cidr": value.String("10.20.0.0/16", value.SourceExplicit),
		},
	})
	if err != nil {
		t.Fatalf("seeding the fake cloud: %v", err)
	}
	st := state.New("myapp", "dev")
	st.Set(rs)
	seedState(t, dir, "dev", st)

	opts := &GlobalOptions{Dir: dir, Parallelism: 4}
	cmd := newApplyCommand(opts)
	cmd.SetArgs([]string{"dev"})
	cmd.SetIn(strings.NewReader("")) // must never be read: no prompt should occur
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() = %v, want nil (no changes is success)", err)
	}
	if !strings.Contains(stdout.String(), "No changes") {
		t.Errorf("stdout does not report a clean plan:\n%s", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(dir, ".infra", "state", "dev.lock")); !os.IsNotExist(err) {
		t.Error("apply with no changes must not take the environment lock")
	}
}

func TestApplyWithAutoApproveCreatesResourcesAndReportsChanges(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: 10.20.0.0/16
`)
	opts := &GlobalOptions{Dir: dir, Parallelism: 4, AutoApprove: true}
	cmd := newApplyCommand(opts)
	cmd.SetArgs([]string{"dev"})
	cmd.SetIn(strings.NewReader(""))
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := cmd.Execute()
	if !errors.Is(err, errChanges) {
		t.Fatalf("Execute() = %v, want errChanges — a fresh apply always has changes", err)
	}
	// executor.Render's exact format (internal/executor/summary.go, pinned
	// by internal/executor/summary_test.go): "Apply complete: %d applied,
	// %d failed, %d skipped.", not "Apply complete!" / "N created" — this
	// is the one place this task's brief drifted from Task 12's landed
	// renderer, corrected here against the actual source rather than
	// against the brief's inline draft.
	if !strings.Contains(stdout.String(), "Apply complete: 1 applied, 0 failed, 0 skipped.") {
		t.Errorf("stdout does not report completion of the one create:\n%s", stdout.String())
	}

	st, err := backendFor(dir).Get(context.Background(), "dev")
	if err != nil {
		t.Fatalf("reading state after apply: %v", err)
	}
	if _, ok := st.Get(address.Address{Name: "network"}); !ok {
		t.Error("network was not recorded in state after a successful apply")
	}
	if _, err := os.Stat(filepath.Join(dir, ".infra", "state", "dev.lock")); !os.IsNotExist(err) {
		t.Error("apply must release the lock when it finishes")
	}
}

func TestApplyDeclinedApprovalAppliesNothing(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: 10.20.0.0/16
`)
	opts := &GlobalOptions{Dir: dir, Parallelism: 4}
	cmd := newApplyCommand(opts)
	cmd.SetArgs([]string{"dev"})
	cmd.SetIn(strings.NewReader("no\n"))
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := cmd.Execute()
	if err == nil || errors.Is(err, errChanges) {
		t.Fatalf("Execute() = %v, want a plain error — declining approval must not apply anything", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".infra", "state", "dev.json")); !os.IsNotExist(err) {
		t.Error("declining approval must not write state")
	}
	if _, err := os.Stat(filepath.Join(dir, ".infra", "state", "dev.lock")); !os.IsNotExist(err) {
		t.Error("declining approval must not leave the environment locked — the lock is taken after approval, not before")
	}
}

// TestApplyEOFOnStdinDeclinesRatherThanHanging pins the second design
// question this task's brief calls out explicitly: confirm treats
// bufio.Scanner.Scan returning false (EOF, or any other read error) as
// "not approved" — the CI-without---auto-approve case. Every other test in
// this file that supplies changes and declines either supplies a literal
// "no\n" or supplies AutoApprove: true; neither exercises Scan() itself
// returning false with real changes on the table, which is the exact
// branch confirm's own doc comment is about. An empty reader (as opposed
// to a reader containing "no\n") is what actually reaches EOF on the first
// Scan call.
func TestApplyEOFOnStdinDeclinesRatherThanHanging(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: 10.20.0.0/16
`)
	opts := &GlobalOptions{Dir: dir, Parallelism: 4}
	cmd := newApplyCommand(opts)
	cmd.SetArgs([]string{"dev"})
	cmd.SetIn(strings.NewReader("")) // EOF on the very first Scan
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := cmd.Execute()
	if err == nil || errors.Is(err, errChanges) {
		t.Fatalf("Execute() = %v, want a plain error — EOF on stdin must decline, not hang or apply", err)
	}
	if !strings.Contains(err.Error(), `type "yes"`) {
		t.Errorf("error does not explain how to approve: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".infra", "state", "dev.json")); !os.IsNotExist(err) {
		t.Error("EOF on stdin must not apply anything")
	}
}

// TestApplyOrdersDependentResourcesAndAppliesBoth exercises the seam this
// task's dependentsOf and computePlan feed into planner.BuildExecution and
// executor.Apply together, end to end, on a plan with a real dependency —
// every other test in this file uses a single resource, which cannot tell
// a correct BuildExecution(p, dependentsOf(p)) wiring apart from one that
// passed dependencies instead of dependents (execution would still
// "work" trivially with nothing to order). database.network references
// network.id, so this also exercises resolveAfter's cross-resource
// resolution (internal/executor/resolve.go) through the full CLI pipeline,
// not just resolve_test.go's unit-level fixtures.
func TestApplyOrdersDependentResourcesAndAppliesBoth(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: 10.20.0.0/16
  database:
    type: test.database
    engine: postgres
    network: ${network.id}
`)
	opts := &GlobalOptions{Dir: dir, Parallelism: 4, AutoApprove: true}
	cmd := newApplyCommand(opts)
	cmd.SetArgs([]string{"dev"})
	cmd.SetIn(strings.NewReader(""))
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := cmd.Execute()
	if !errors.Is(err, errChanges) {
		t.Fatalf("Execute() = %v, want errChanges — both resources are fresh creates", err)
	}
	if !strings.Contains(stdout.String(), "Apply complete: 2 applied, 0 failed, 0 skipped.") {
		t.Errorf("stdout does not report both creates:\n%s", stdout.String())
	}

	st, err := backendFor(dir).Get(context.Background(), "dev")
	if err != nil {
		t.Fatalf("reading state after apply: %v", err)
	}
	net, ok := st.Get(address.Address{Name: "network"})
	if !ok {
		t.Fatal("network was not recorded in state after a successful apply")
	}
	db, ok := st.Get(address.Address{Name: "database"})
	if !ok {
		t.Fatal("database was not recorded in state after a successful apply")
	}
	netID, _ := net.Attributes["id"].AsString()
	dbNetwork, _ := db.Attributes["network"].AsString()
	if netID == "" || dbNetwork != netID {
		t.Errorf("database.network = %q, want the applied network's id %q — dependency resolution did not run in the right order", dbNetwork, netID)
	}
}

// TestApplyReportsFailureAndReturnsPlainError exercises the render-then-
// exit-code seam on the one path none of the other tests in this file
// reach: a provider call that actually fails during execution.
// executor.Apply itself is exhaustively tested for this in
// internal/executor, but nothing else in this file checks that apply.go's
// own "if execDiags.HasErrors() || len(res.Failed) > 0" gate — and not
// errChanges — is what apply.go returns when that happens, nor that the
// lock is still released on a failed run.
func TestApplyReportsFailureAndReturnsPlainError(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: 10.20.0.0/16
`)
	cloud := testprovider.Cloud{
		Resources: map[string]*testprovider.CloudResource{},
		Failures: []testprovider.FailureRule{
			{Op: "create", Address: "network", Nth: 1, Message: "injected failure"},
		},
	}
	data, err := json.MarshalIndent(&cloud, "", "  ")
	if err != nil {
		t.Fatalf("marshaling fake cloud: %v", err)
	}
	cloudPath := filepath.Join(dir, testprovider.DefaultCloudPath)
	if err := os.MkdirAll(filepath.Dir(cloudPath), 0o755); err != nil {
		t.Fatalf("creating cloud dir: %v", err)
	}
	if err := os.WriteFile(cloudPath, data, 0o600); err != nil {
		t.Fatalf("seeding the fake cloud with a failure rule: %v", err)
	}

	opts := &GlobalOptions{Dir: dir, Parallelism: 4, AutoApprove: true}
	cmd := newApplyCommand(opts)
	cmd.SetArgs([]string{"dev"})
	cmd.SetIn(strings.NewReader(""))
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	execErr := cmd.Execute()
	if execErr == nil || errors.Is(execErr, errChanges) {
		t.Fatalf("Execute() = %v, want a plain error — a failed operation must not report success", execErr)
	}
	if !strings.Contains(stdout.String(), "Apply complete: 0 applied, 1 failed, 0 skipped.") {
		t.Errorf("stdout does not report the failure:\n%s", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(dir, ".infra", "state", "dev.lock")); !os.IsNotExist(err) {
		t.Error("apply must release the lock even when an operation fails")
	}
}

func TestApplyLockConflictNamesTheHolder(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: 10.20.0.0/16
`)
	backend := backendFor(dir)
	lockCtx := state.WithOperation(context.Background(), "some-other-run")
	if _, err := backend.Lock(lockCtx, "dev"); err != nil {
		t.Fatalf("pre-locking dev: %v", err)
	}
	defer backend.ForceUnlock("dev")

	opts := &GlobalOptions{Dir: dir, Parallelism: 4, AutoApprove: true}
	cmd := newApplyCommand(opts)
	cmd.SetArgs([]string{"dev"})
	cmd.SetIn(strings.NewReader(""))
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() = nil, want an error — the environment is already locked")
	}
	if !strings.Contains(err.Error(), "some-other-run") {
		t.Errorf("error does not name the holder's operation: %v", err)
	}
	if !strings.Contains(err.Error(), "pid") {
		t.Errorf("error does not name the holder's pid: %v", err)
	}
}
