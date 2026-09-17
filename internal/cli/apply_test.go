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
	"sync"
	"testing"

	"github.com/infrena/infrena/internal/compiler"
	"github.com/infrena/infrena/internal/config"
	"github.com/infrena/infrena/internal/state"
	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/resource"
	"github.com/infrena/infrena/pkg/value"
	testprovider "github.com/infrena/infrena/providers/test"
)

// seedState writes st directly to the backend, taking and releasing the
// environment lock around the write — state.Local.Put refuses to write
// without a held lock, and this helper keeps that contract rather than
// reaching around it.
func seedState(t *testing.T, dir, environment string, st *state.State) {
	t.Helper()
	backend := localBackend(dir)
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
    type: fake.network
    cidr: 10.20.0.0/16
`)
	ctx := context.Background()
	prov := testprovider.New(filepath.Join(dir, testprovider.DefaultCloudPath))
	rs, err := prov.Create(ctx, &resource.DesiredResource{
		Address: address.Address{Name: "network"},
		Type:    "fake.network",
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

	// Pre-lock dev as another holder before running the no-op apply, and
	// leave it held for the whole test. Checking dev.lock's absence AFTER
	// Execute returns — the only check this test used to make — cannot
	// distinguish "apply never locked" from "apply locked, then
	// releaseLock released it": both end with no lock file on disk.
	// Reviewer-proven directly: a mutation that adds a lock+release inside
	// the !p.HasChanges() branch left this test (and all six other
	// TestApply* tests) green. Holding the lock as someone else for the
	// duration instead is what actually pins the decision this test is
	// named for: if apply's no-changes path ever called backend.Lock, that
	// call would fail immediately against this existing holder (Lock's
	// O_EXCL create), and Execute would return a non-nil error.
	backend := localBackend(dir)
	lockCtx := state.WithOperation(context.Background(), "some-other-run")
	if _, err := backend.Lock(lockCtx, "dev"); err != nil {
		t.Fatalf("pre-locking dev: %v", err)
	}
	defer backend.ForceUnlock(context.Background(), "dev")

	opts := &GlobalOptions{Dir: dir, Parallelism: 4}
	cmd := newApplyCommand(opts)
	cmd.SetArgs([]string{"dev"})
	cmd.SetIn(strings.NewReader("")) // must never be read: no prompt should occur
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() = %v, want nil — a no-op apply must succeed even though another process holds the environment lock; it never needed the lock at all", err)
	}
	if !strings.Contains(stdout.String(), "No changes") {
		t.Errorf("stdout does not report a clean plan:\n%s", stdout.String())
	}
	lock, held, err := backend.Inspect(context.Background(), "dev")
	if err != nil {
		t.Fatalf("inspecting the lock after apply: %v", err)
	}
	if !held || lock.Operation != "some-other-run" {
		t.Errorf("lock after apply = (held=%v, operation=%q), want the pre-existing lock untouched", held, lock.Operation)
	}
}

func TestApplyWithAutoApproveCreatesResourcesAndReportsChanges(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  network:
    type: fake.network
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

	st, err := localBackend(dir).Get(context.Background(), "dev")
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
    type: fake.network
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

// TestApplyEOFOnStdinRefusesRatherThanHanging pins the CI-without-
// --auto-approve case. It used to assert that confirm read EOF and declined
// with "you must type yes", which was advice nobody in that pipeline could
// take; the run now refuses BEFORE the prompt, with errNoApproval and exit
// 77, naming an escape that works. An empty reader (as opposed to a reader
// containing "no\n") is what actually reaches EOF on the first read.
func TestApplyEOFOnStdinRefusesRatherThanHanging(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  network:
    type: fake.network
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
	if !errors.Is(err, errNoApproval) {
		t.Fatalf("Execute() = %v, want errNoApproval — EOF on stdin must refuse, not hang or apply", err)
	}
	if !strings.Contains(err.Error(), "--auto-approve") {
		t.Errorf("error does not name an escape this caller can take: %v", err)
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
    type: fake.network
    cidr: 10.20.0.0/16
  database:
    type: fake.database
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

	st, err := localBackend(dir).Get(context.Background(), "dev")
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
    type: fake.network
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

// TestApplyRePlansInsideTheLockNotJustBeforeIt pins this task's core design
// requirement: apply computes the plan twice — once unlocked, to show it
// and take approval, and once again immediately after the lock is
// acquired, so execution never runs against state or provider reality read
// before the lock was held (this task's own doc comment on computePlan and
// newApplyCommand's RunE). A version of apply that collapsed the two into
// one computePlan call — reusing the pre-lock plan and state rather than
// recomputing — would still pass every other test in this file, because
// nothing else here changes anything between the two calls.
//
// The fake provider's own failure-injection is the seam: refresh.Refresh
// calls provider.Read exactly once per resource already in state
// (internal/refresh/refresh.go's readOne, with no internal retry), and
// computePlan calls refresh.Refresh once per invocation. network is
// already in state and matches the fake cloud exactly (so it proposes no
// change on its own), while database is a fresh create — giving a plan
// with changes on both passes without ever calling Read on network more
// than once per computePlan call. A FailureRule with Nth: 2 therefore
// fires on exactly the SECOND read of network across the whole apply run
// — the in-lock re-plan — and never on the first, unlocked preview.
func TestApplyRePlansInsideTheLockNotJustBeforeIt(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  network:
    type: fake.network
    cidr: 10.20.0.0/16
  database:
    type: fake.database
    engine: postgres
    network: ${network.id}
`)
	ctx := context.Background()
	cloudPath := filepath.Join(dir, testprovider.DefaultCloudPath)
	prov := testprovider.New(cloudPath)
	rs, err := prov.Create(ctx, &resource.DesiredResource{
		Address: address.Address{Name: "network"},
		Type:    "fake.network",
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

	cloud, err := testprovider.LoadCloud(cloudPath)
	if err != nil {
		t.Fatalf("loading the fake cloud to add a failure rule: %v", err)
	}
	cloud.Failures = append(cloud.Failures, testprovider.FailureRule{
		Op: "read", Address: "network", Nth: 2, Message: "second-refresh-marker",
	})
	if err := cloud.Save(cloudPath); err != nil {
		t.Fatalf("saving the fake cloud with a failure rule: %v", err)
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
		t.Fatalf("Execute() = %v, want a plain error from the second, in-lock refresh failing", execErr)
	}
	if !strings.Contains(stderr.String(), "second-refresh-marker") {
		t.Errorf("stderr does not show the injected second-read failure — the in-lock re-plan did not run refresh a second time:\n%s", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dir, ".infra", "state", "dev.lock")); !os.IsNotExist(err) {
		t.Error("apply must release the lock even when the in-lock re-plan fails")
	}
}

// mutatingReader wraps body but runs before, once, on its very first Read
// call — before returning any of body's bytes — then behaves exactly like
// body from then on. It stands in for a real concurrent actor: a custom
// io.Reader is a genuine seam here, not a workaround, because
// bufio.Scanner.Scan (confirm, apply.go) calls Read at the exact moment a
// human's approval keystroke would arrive — after the unlocked preview,
// before backend.Lock — which is precisely where a real race with another
// process would land.
type mutatingReader struct {
	before func()
	body   io.Reader
	done   bool
}

func (r *mutatingReader) Read(p []byte) (int, error) {
	if !r.done {
		r.before()
		r.done = true
	}
	return r.body.Read(p)
}

// TestApplyPlanConvergesToNoChangesInsideTheLock reaches the one branch of
// apply.go no other test in this file does: the post-lock
// "!p2.HasChanges()" path, printing "No changes remained once the
// environment lock was acquired". Every other test keeps provider reality
// identical across apply's two computePlan calls, so p2 always agrees with
// p and this branch is never taken. This constructs, deterministically,
// the exact race re-planning inside the lock exists to guard against
// (this task's own doc comment on computePlan): real infrastructure
// changing in the window between the unlocked preview a human approved and
// the lock being granted. mutatingReader fixes the fake cloud's drift —
// the very drift the first, unlocked plan proposed to correct via a
// replace — the moment "yes" is read, standing in for another process
// having already applied the same fix out of band. The second,
// in-lock computePlan then observes reality already matching configuration
// and finds nothing left to do.
func TestApplyPlanConvergesToNoChangesInsideTheLock(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  network:
    type: fake.network
    cidr: 10.0.0.0/16
`)
	ctx := context.Background()
	cloudPath := filepath.Join(dir, testprovider.DefaultCloudPath)
	prov := testprovider.New(cloudPath)
	// Seeded drifted from configuration (10.1.0.0/16 vs the desired
	// 10.0.0.0/16, a ForceNew attribute) so the unlocked preview proposes a
	// replace and apply reaches the approval prompt at all.
	rs, err := prov.Create(ctx, &resource.DesiredResource{
		Address: address.Address{Name: "network"},
		Type:    "fake.network",
		Attrs: map[string]value.Value{
			"cidr": value.String("10.1.0.0/16", value.SourceExplicit),
		},
	})
	if err != nil {
		t.Fatalf("seeding the fake cloud: %v", err)
	}
	st := state.New("myapp", "dev")
	st.Set(rs)
	seedState(t, dir, "dev", st)

	fixDrift := func() {
		cloud, err := testprovider.LoadCloud(cloudPath)
		if err != nil {
			t.Fatalf("loading the fake cloud to fix drift: %v", err)
		}
		res, ok := cloud.Resources[rs.ProviderID]
		if !ok {
			t.Fatalf("network (%s) not found in the fake cloud", rs.ProviderID)
		}
		res.Attributes["cidr"] = "10.0.0.0/16"
		if err := cloud.Save(cloudPath); err != nil {
			t.Fatalf("saving the fixed fake cloud: %v", err)
		}
	}

	opts := &GlobalOptions{Dir: dir, Parallelism: 4}
	cmd := newApplyCommand(opts)
	cmd.SetArgs([]string{"dev"})
	cmd.SetIn(&mutatingReader{before: fixDrift, body: strings.NewReader("yes\n")})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() = %v, want nil — nothing was left to apply once the in-lock re-plan converged", err)
	}
	if !strings.Contains(stdout.String(), "No changes remained once the environment lock was acquired") {
		t.Errorf("stdout does not report the post-lock convergence:\n%s", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(dir, ".infra", "state", "dev.lock")); !os.IsNotExist(err) {
		t.Error("apply must release the lock after the post-lock convergence path")
	}
}

func TestApplyLockConflictNamesTheHolder(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  network:
    type: fake.network
    cidr: 10.20.0.0/16
`)
	backend := localBackend(dir)
	lockCtx := state.WithOperation(context.Background(), "some-other-run")
	if _, err := backend.Lock(lockCtx, "dev"); err != nil {
		t.Fatalf("pre-locking dev: %v", err)
	}
	defer backend.ForceUnlock(context.Background(), "dev")

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

// TestApplyReplaceWhoseNewValueReferencesAResourceCreatedInTheSameRun pins
// needsDesired (internal/executor/apply.go).
//
// needsDesired excludes a replace's DESTROY phase from resolving desired
// values. Forcing it to `return true` leaves the whole suite green, and an
// ordinary destroy or replace behaves identically under that mutation,
// because their references resolve against state. The shape that breaks is
// the one below: a replace whose NEW attributes point at a resource that
// does not exist yet and is created later in the same run. The destroy phase
// runs first, so resolving its operation's After finds ${network2.id} still
// unknown, and the run dies with "network is still unknown after its
// dependencies were applied" — taking the create phase and network2 with it
// as skips. The guard is what stops a destroy from demanding values only the
// create half needs.
//
// Seeded through two real applies rather than a hand-built plan: what is
// under test is that the PLANNER produces this node shape and the executor
// survives it, and a fixture that assembled the operation itself would prove
// only that the author knew which fields to set.
func TestApplyReplaceWhoseNewValueReferencesAResourceCreatedInTheSameRun(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  network:
    type: fake.network
    cidr: 10.20.0.0/16
  db:
    type: fake.database
    engine: postgres
    network: ${network.id}
`)
	apply := func(t *testing.T) (string, error) {
		t.Helper()
		opts := &GlobalOptions{Dir: dir, Parallelism: 4, AutoApprove: true}
		cmd := newApplyCommand(opts)
		cmd.SetArgs([]string{"dev"})
		cmd.SetIn(strings.NewReader(""))
		var stdout, stderr bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		err := cmd.Execute()
		return stdout.String() + stderr.String(), err
	}

	if out, err := apply(t); !errors.Is(err, errChanges) {
		t.Fatalf("first apply = %v, want errChanges:\n%s", err, out)
	}

	// engine is ForceNew, so db becomes a replace; network2 does not exist
	// yet, so the replace's new network value is unresolvable until it does.
	if err := os.WriteFile(filepath.Join(dir, "infra.yml"), []byte(`
project: myapp
resources:
  network:
    type: fake.network
    cidr: 10.20.0.0/16
  network2:
    type: fake.network
    cidr: 10.50.0.0/16
  db:
    type: fake.database
    engine: mysql
    network: ${network2.id}
`), 0o644); err != nil {
		t.Fatalf("rewriting configuration: %v", err)
	}

	out, err := apply(t)
	if !errors.Is(err, errChanges) {
		t.Fatalf("second apply = %v, want errChanges — the replace and the new network are both real work:\n%s", err, out)
	}
	if !strings.Contains(out, "Apply complete: 2 applied, 0 failed, 0 skipped.") {
		t.Fatalf("the replace did not complete cleanly:\n%s", out)
	}

	st, err := localBackend(dir).Get(context.Background(), "dev")
	if err != nil {
		t.Fatalf("reading state after apply: %v", err)
	}
	net2, ok := st.Get(address.Address{Name: "network2"})
	if !ok {
		t.Fatal("network2 was not recorded in state")
	}
	db, ok := st.Get(address.Address{Name: "db"})
	if !ok {
		t.Fatal("db was not recorded in state")
	}
	net2ID, _ := net2.Attributes["id"].AsString()
	dbNetwork, _ := db.Attributes["network"].AsString()
	if net2ID == "" || dbNetwork != net2ID {
		t.Errorf("db.network = %q, want network2's id %q — the replace's create phase did not resolve against the newly created network", dbNetwork, net2ID)
	}
	if engine, _ := db.Attributes["engine"].AsString(); engine != "mysql" {
		t.Errorf("db.engine = %q, want mysql — the replacement did not take effect", engine)
	}
}

// TestExecutorOptionsDoesNotConflateTheTwoConcurrencyBounds pins spec
// §15/§34's second bound at the seam where it was lost.
//
// The mechanism is tested in internal/executor
// (TestApplyBoundsPerProviderIndependentlyOfGlobalParallelism); what was
// broken is the WIRING. Both commands passed PerProvider: opts.Parallelism,
// which can never bind — the global semaphore admits at most Parallelism
// operations, so the per-provider check is unreachable before the global
// one has already deferred the node. A bound that cannot fire is not a
// bound.
//
// This asserts against executorOptions, the function production actually
// calls, rather than reading the constant back: the constant being 8 is not
// the property, the two bounds being independent is. An earlier bounds test
// in internal/executor set PerProvider == Parallelism and therefore could
// not have distinguished them either — hence the explicit inequality here.
func TestExecutorOptionsDoesNotConflateTheTwoConcurrencyBounds(t *testing.T) {
	// The property is independence, not any particular number: PerProvider
	// must not TRACK Parallelism. (Asserting inequality at a single value
	// would fail the moment the constant coincided with that value, which
	// is a coincidence, not a conflation.)
	var seen int
	for i, parallelism := range []int{1, 8, 20, 100} {
		got := executorOptions(&GlobalOptions{Parallelism: parallelism}, nil, nil, "dev")
		if got.Parallelism != parallelism {
			t.Errorf("Parallelism = %d, want %d — --parallelism must reach the executor unchanged", got.Parallelism, parallelism)
		}
		if got.PerProvider < 1 {
			t.Errorf("PerProvider = %d, want at least 1", got.PerProvider)
		}
		if i == 0 {
			seen = got.PerProvider
			continue
		}
		if got.PerProvider != seen {
			t.Fatalf("PerProvider = %d at --parallelism %d but %d at --parallelism 1: the per-provider bound tracks the global one, so it can never fire",
				got.PerProvider, parallelism, seen)
		}
	}

	if got := executorOptions(&GlobalOptions{Parallelism: 10}, nil, nil, "dev"); got.PerProvider >= 10 {
		t.Errorf("PerProvider = %d at the default --parallelism of 10 — the per-provider bound is inert out of the box", got.PerProvider)
	}
}

// TestApplyOnADependsOnOnlyChangeUpdatesRecordedDependencies is the other
// half of TestApplyRecordsDependenciesInStateSoALaterDestroyCanOrderItself
// (destroy_test.go): recording dependencies on CREATE alone left invariant 4
// working for resources created afterwards and not for resources whose
// dependencies later change.
//
// The reproduction it pins: `db` is applied depending on `net`; a new
// resource `other` is added and `db` is made to depend on it too. Before the
// planner diffed dependency edges, that planned as "1 to create" — only
// `other` — `db` was never updated, and its recorded Dependencies stayed
// [net]. The destroy edge for `other` did not exist, so a later destroy could
// delete `other` first and strand `db`.
//
// It is fixed the same way a lifecycle-only change was: configuration's
// metadata must be able to catch up with what state records, or the recorded
// copy is authoritative in a direction nobody chose.
func TestApplyOnADependsOnOnlyChangeUpdatesRecordedDependencies(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  net:
    type: fake.network
    cidr: 10.20.0.0/16
  db:
    type: fake.database
    engine: postgres
    network: ${net.id}
`)
	apply := func(t *testing.T) (string, error) {
		t.Helper()
		opts := &GlobalOptions{Dir: dir, Parallelism: 4, AutoApprove: true}
		cmd := newApplyCommand(opts)
		cmd.SetArgs([]string{"dev"})
		cmd.SetIn(strings.NewReader(""))
		var stdout, stderr bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		err := cmd.Execute()
		return stdout.String() + stderr.String(), err
	}

	if out, err := apply(t); !errors.Is(err, errChanges) {
		t.Fatalf("first apply = %v, want errChanges:\n%s", err, out)
	}
	st, err := localBackend(dir).Get(context.Background(), "dev")
	if err != nil {
		t.Fatalf("reading state: %v", err)
	}
	db, _ := st.Get(address.Address{Name: "db"})
	if len(db.Dependencies) != 1 || db.Dependencies[0].String() != "net" {
		t.Fatalf("after the first apply db.Dependencies = %v, want [net]", db.Dependencies)
	}

	// Only the dependency edges change: db's attributes are untouched.
	if err := os.WriteFile(filepath.Join(dir, "infra.yml"), []byte(`
project: myapp
resources:
  net:
    type: fake.network
    cidr: 10.20.0.0/16
  other:
    type: fake.network
    cidr: 10.50.0.0/16
  db:
    type: fake.database
    engine: postgres
    network: ${net.id}
    depends_on: [other]
`), 0o644); err != nil {
		t.Fatalf("rewriting configuration: %v", err)
	}

	out, err := apply(t)
	if !errors.Is(err, errChanges) {
		t.Fatalf("second apply = %v, want errChanges:\n%s", err, out)
	}
	// The user must be told what the update is. An update whose only change
	// is metadata renders as a bare header without renderMetadataLines.
	if !strings.Contains(out, "depends_on: [net] -> [net, other]") {
		t.Errorf("the plan does not show the dependency change it is asking approval for:\n%s", out)
	}

	st, err = localBackend(dir).Get(context.Background(), "dev")
	if err != nil {
		t.Fatalf("reading state after the second apply: %v", err)
	}
	db, ok := st.Get(address.Address{Name: "db"})
	if !ok {
		t.Fatal("db is not in state")
	}
	got := make([]string, 0, len(db.Dependencies))
	for _, d := range db.Dependencies {
		got = append(got, d.String())
	}
	if len(got) != 2 || got[0] != "net" || got[1] != "other" {
		t.Fatalf("db.Dependencies = %v, want [net other] — a depends_on-only change must reach state, or the new destroy edge does not exist", got)
	}

	// And it must have converged: a third plan proposes nothing.
	planCmd := newPlanCommand(&GlobalOptions{Dir: dir, Parallelism: 4})
	planCmd.SetArgs([]string{"dev"})
	var planOut, planErr bytes.Buffer
	planCmd.SetOut(&planOut)
	planCmd.SetErr(&planErr)
	if err := planCmd.Execute(); err != nil {
		t.Fatalf("re-plan = %v, want no changes:\n%s%s", err, planOut.String(), planErr.String())
	}
	if !strings.Contains(planOut.String(), "No changes.") {
		t.Errorf("invariant 2: the dependency update did not converge:\n%s", planOut.String())
	}
}

func TestApplyRefusesWhenApprovalCannotBeObtained(t *testing.T) {
	cases := []struct {
		name  string
		args  []string
		stdin string
	}{
		// --output means nobody is reading stdout, so nobody can see a prompt.
		{"output mode", []string{"apply", "dev", "--output", "OUT"}, ""},
		// stdin at EOF means nobody is there to type.
		{"stdin at eof", []string{"apply", "dev"}, ""},
		// destroy asks for more than "yes", which does not make the answer
		// any more obtainable.
		{"destroy in output mode", []string{"destroy", "dev", "--output", "OUT"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := newProjectFixture(t)
			args := withOutputPath(t, tc.args)

			// destroy has nothing to destroy in a fresh fixture, so it needs
			// a state to plan against — applied here, before the run under
			// test, which also proves the refusal below is about approval
			// rather than about an empty plan.
			if args[0] == "destroy" {
				if _, _, code := runCommand(t, dir, "apply", "dev", "--auto-approve"); code != ExitChanges {
					t.Fatalf("seeding apply exit = %d, want %d", code, ExitChanges)
				}
			}

			_, stderr, code := runCommandWithStdin(t, dir, tc.stdin, args...)

			if code != ExitNoApproval {
				t.Errorf("exit = %d, want %d", code, ExitNoApproval)
			}
			if !strings.Contains(stderr, "--auto-approve") {
				t.Errorf("stderr does not name the fix:\n%s", stderr)
			}
			// The whole point of refusing before the lock: nothing was touched.
			if args[0] == "apply" && stateExists(t, dir, "dev") {
				t.Error("apply wrote state despite refusing for want of approval")
			}
			if _, err := os.Stat(filepath.Join(dir, ".infra", "state", "dev.lock")); !os.IsNotExist(err) {
				t.Error("the environment was locked despite refusing for want of approval")
			}
		})
	}
}

// --auto-approve and --plan are both escapes, and a clean plan never needed
// approval at all, so none of the three may hit the new code path.
func TestApprovalRuleDoesNotFireWhenItShouldNot(t *testing.T) {
	dir := newProjectFixture(t)
	out := filepath.Join(t.TempDir(), "run.ndjson")

	if _, _, code := runCommand(t, dir, "apply", "dev", "--output", out, "--auto-approve"); code == ExitNoApproval {
		t.Error("--auto-approve still hit the approval refusal")
	}
	// Applied once, so the second run has no changes and must exit 0.
	if _, _, code := runCommand(t, dir, "apply", "dev", "--output", out); code != ExitOK {
		t.Errorf("clean apply exit = %d, want %d", code, ExitOK)
	}
}

// One reader for stdin, read exactly once: whatever the user types must
// arrive at confirm intact, with no earlier read having consumed part of it.
func TestApprovalPeekDoesNotEatTheAnswer(t *testing.T) {
	dir := newProjectFixture(t)

	_, _, code := runCommandWithStdin(t, dir, "yes\n", "apply", "dev")

	if code != ExitChanges {
		t.Fatalf("exit = %d, want %d — a typed \"yes\" must still approve", code, ExitChanges)
	}
	if !stateExists(t, dir, "dev") {
		t.Error("nothing was applied despite an approved apply")
	}
}

// lockedBuffer serializes writes so watchedStdin below can read what has
// already been printed while the command is still running, without racing
// the progress renderer's own writes under -race.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// watchedStdin answers like an ordinary reader but records what stdout held
// at the moment it was FIRST read, which is the only way to observe the
// ordering from outside: on a terminal a read blocks, so anything printed
// after the first read is printed after the user has already typed.
type watchedStdin struct {
	out     *lockedBuffer
	answer  io.Reader
	read    bool
	printed string
}

func (s *watchedStdin) Read(p []byte) (int, error) {
	if !s.read {
		s.read = true
		s.printed = s.out.String()
	}
	return s.answer.Read(p)
}

// TestApprovalPromptIsPrintedBeforeStdinIsRead pins the regression that
// detecting end of input by peeking a byte off stdin introduced. That peek
// ran before the prompt was written, and on a terminal it blocks until the
// user types — so the user saw a blank screen with no indication that the
// single most important prompt in the tool was waiting on them, and the
// prompt appeared only after their keystrokes. Nothing may read stdin until
// the question has reached stdout.
func TestApprovalPromptIsPrintedBeforeStdinIsRead(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  network:
    type: fake.network
    cidr: 10.20.0.0/16
`)
	out := &lockedBuffer{}
	stdin := &watchedStdin{out: out, answer: strings.NewReader("yes\n")}

	opts := &GlobalOptions{Dir: dir, Parallelism: 4}
	cmd := newApplyCommand(opts)
	cmd.SetArgs([]string{"dev"})
	cmd.SetIn(stdin)
	cmd.SetOut(out)
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)

	if err := cmd.Execute(); err != nil && !errors.Is(err, errChanges) {
		t.Fatalf("Execute() = %v, want a successful apply: %s", err, stderr.String())
	}
	if !stdin.read {
		t.Fatal("stdin was never read, so the test proves nothing about the order")
	}
	if !strings.Contains(stdin.printed, "Enter a value:") {
		t.Errorf("stdin was read before the approval prompt reached stdout — a user would face a blank screen.\nprinted before the first read:\n%s", stdin.printed)
	}
}

// TestComputePlanNeverFoldsObservationsIntoState guards the one refactor
// that would turn this milestone's `current`-is-stale bug into an invisible
// one.
//
// The tidy-looking move, once the executor started needing observations, is
// to write them into state the moment refresh returns — "one current truth,
// everything downstream reads it." It is wrong, and quietly so. st is two
// things at once here: the left-hand side of the planner's diff, and the
// document the executor PERSISTS as it applies. Folding observations in
// before planner.Compute corrupts both. The planner's Lifecycle and
// Dependencies diffs read st deliberately — no provider owns those fields,
// so the record is their only source of truth — and would start comparing
// configuration against whatever a provider's Read happened to return,
// proposing spurious changes or, worse, recording an empty prevent_destroy
// guard. And the executor would then persist drifted attribute values as
// though they had been applied, with no provider call ever having been made
// to make them true.
//
// So this pins the separation directly: after computePlan, the plan must
// have been computed against what was OBSERVED, and st must still hold what
// was last PERSISTED. The merge between the two belongs after planning,
// inside the executor — see executor.currentFor.
func TestComputePlanNeverFoldsObservationsIntoState(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  net:
    type: fake.vpc
    tags:
      owner: platform
`)
	ctx := context.Background()
	cloudPath := filepath.Join(dir, testprovider.DefaultCloudPath)
	prov := testprovider.New(cloudPath)
	rs, err := prov.Create(ctx, &resource.DesiredResource{
		Address: address.Address{Name: "net"},
		Type:    "fake.vpc",
		Attrs: map[string]value.Value{
			"tags": value.Map(map[string]value.Value{
				"owner": value.String("platform", value.SourceExplicit),
			}, value.SourceExplicit),
		},
	})
	if err != nil {
		t.Fatalf("seeding the fake cloud: %v", err)
	}
	st := state.New("myapp", "dev")
	st.Set(rs)
	seedState(t, dir, "dev", st)

	// Somebody retags it outside infrena. State still says "platform"; the
	// provider now says "intruder".
	cloud, err := testprovider.LoadCloud(cloudPath)
	if err != nil {
		t.Fatalf("loading the fake cloud: %v", err)
	}
	cloud.Resources[rs.ProviderID].Attributes["tags"] = map[string]any{"owner": "intruder"}
	if err := cloud.Save(cloudPath); err != nil {
		t.Fatalf("saving the drifted fake cloud: %v", err)
	}

	opts := &GlobalOptions{Dir: dir, Parallelism: 4}
	cmd := newApplyCommand(opts)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	copts, cds := compilerOptions(opts, "dev")
	if cds.HasErrors() {
		t.Fatalf("compiler options: %+v", cds)
	}
	files, err := config.Load(dir)
	if err != nil {
		t.Fatalf("loading configuration: %v", err)
	}
	reg, closePlugins := buildRegistry(opts)
	defer closePlugins()
	cfg, compileDiags := compiler.Compile(files, reg, copts)
	if compileDiags.HasErrors() {
		t.Fatalf("compiling: %+v", compileDiags)
	}
	ro, closeRun, err := openRun(cmd, opts, "apply", "dev")
	if err != nil {
		t.Fatalf("openRun: %v", err)
	}
	defer closeRun()

	p, planState, obs, err := computePlan(ctx, cmd, localBackend(dir), reg, cfg, "dev", opts, ro)
	if err != nil {
		t.Fatalf("computePlan: %v", err)
	}

	// The observation reached the planner: the drift is what the plan
	// proposes to correct. A merge-before-Compute refactor would not
	// necessarily break this half, which is exactly why the second half
	// below has to be asserted separately.
	if !p.HasChanges() {
		t.Fatal("plan proposes nothing against a drifted resource — the observation never reached the planner")
	}
	if got := ownerTag(t, p.Operations[0].Before); got != "intruder" {
		t.Errorf("plan Before owner = %q, want the observed %q", got, "intruder")
	}
	if got := ownerTag(t, p.Operations[0].After); got != "platform" {
		t.Errorf("plan After owner = %q, want the configured %q", got, "platform")
	}
	if seen, ok := obs["net"]; !ok || seen.State == nil {
		t.Fatalf("computePlan returned no observation for net: %+v", obs)
	} else if got := ownerTag(t, seen.State.Attributes); got != "intruder" {
		t.Errorf("observed owner = %q, want %q", got, "intruder")
	}

	// THE GUARD. st is the last state persisted, and computePlan must hand
	// it back exactly that way.
	stored, ok := planState.Get(address.Address{Name: "net"})
	if !ok {
		t.Fatal("net is missing from the state computePlan returned")
	}
	if got := ownerTag(t, stored.Attributes); got != "platform" {
		t.Errorf("state owner = %q after computePlan, want the last persisted %q — observations must NEVER be written into state before planner.Compute: st is both the left-hand side of the planner's diff and the document the executor persists, and folding reality into it collapses the first and falsifies the second", got, "platform")
	}
}

// ownerTag reads attrs["tags"]["owner"] as a string, failing the test if the
// shape is not what the fixture above builds.
func ownerTag(t *testing.T, attrs map[string]value.Value) string {
	t.Helper()
	tags, ok := attrs["tags"].Raw.(map[string]value.Value)
	if !ok {
		t.Fatalf("tags is not a map: %#v", attrs["tags"])
	}
	owner, ok := tags["owner"].AsString()
	if !ok {
		t.Fatalf("tags.owner is not a string: %#v", tags["owner"])
	}
	return owner
}
