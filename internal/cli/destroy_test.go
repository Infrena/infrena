package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"infra/internal/state"
	"infra/pkg/address"
	"infra/pkg/resource"
	"infra/pkg/value"
	testprovider "infra/providers/test"
)

func TestDestroyWithEmptyStateTakesNoLockAndDoesNotPrompt(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources: {}
`)
	opts := &GlobalOptions{Dir: dir, Parallelism: 4}
	cmd := newDestroyCommand(opts)
	cmd.SetArgs([]string{"dev"})
	cmd.SetIn(strings.NewReader(""))
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() = %v, want nil — nothing to destroy is success", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".infra", "state", "dev.lock")); !os.IsNotExist(err) {
		t.Error("destroy with nothing to destroy must not take the environment lock")
	}
}

func TestDestroyRequiresTheEnvironmentNameNotABareYes(t *testing.T) {
	dir := seedOneNetwork(t, "dev")

	opts := &GlobalOptions{Dir: dir, Parallelism: 4}
	cmd := newDestroyCommand(opts)
	cmd.SetArgs([]string{"dev"})
	cmd.SetIn(strings.NewReader("yes\n")) // a bare "yes" is not the environment name
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := cmd.Execute()
	if err == nil || errors.Is(err, errChanges) {
		t.Fatalf("Execute() = %v, want a plain error — \"yes\" must not satisfy destroy's confirmation", err)
	}
	st, gerr := backendFor(dir).Get(context.Background(), "dev")
	if gerr != nil {
		t.Fatalf("reading state: %v", gerr)
	}
	if _, ok := st.Get(address.Address{Name: "network"}); !ok {
		t.Error("network was destroyed despite confirmation being refused")
	}
}

func TestDestroyWithCorrectConfirmationRemovesEverything(t *testing.T) {
	dir := seedOneNetwork(t, "dev")

	opts := &GlobalOptions{Dir: dir, Parallelism: 4}
	cmd := newDestroyCommand(opts)
	cmd.SetArgs([]string{"dev"})
	cmd.SetIn(strings.NewReader("dev\n"))
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := cmd.Execute()
	if !errors.Is(err, errChanges) {
		t.Fatalf("Execute() = %v, want errChanges — a destroy with something to remove has changes", err)
	}
	// The prompt itself must actually warn: spec §13's typed confirmation is
	// only as good as what it makes the person read before they type
	// anything.
	if !strings.Contains(stdout.String(), "This cannot be undone") {
		t.Errorf("stdout does not carry the destructive warning:\n%s", stdout.String())
	}
	// The plan header must name the real project, taken from state.State.Project
	// (seedOneNetwork uses state.New("myapp", ...)) — destroy never calls
	// config.Load or compiler.Compile, so this is the only source for it, and
	// getting it wrong would mislead the person about to confirm a destroy
	// against the wrong project.
	if !strings.Contains(stdout.String(), `Plan for project "myapp"`) {
		t.Errorf("stdout does not name the project from state:\n%s", stdout.String())
	}
	// executor.Render (Task 12) is the one shared result renderer for both
	// apply and destroy — it still reports "Apply complete" regardless of
	// which command ran (destroy does not fork a second renderer to get
	// "Destroy complete!" wording — see this package's destroy.go doc
	// comment and apply.go's note on why two renderers for one Result is a
	// defect, not a style choice), but it now marks a removed resource "-"
	// rather than "+" (see the marker assertions below).
	if !strings.Contains(stdout.String(), "Apply complete: 1 applied, 0 failed, 0 skipped.") {
		t.Errorf("stdout does not report completion:\n%s", stdout.String())
	}
	// "- network", not "+": the summary marks a resource that was applied
	// by ceasing to exist with "-", matching the "-" the plan above used
	// to propose destroying it. A "+" here would tell someone who just
	// destroyed their environment that every resource had been created.
	if !strings.Contains(stdout.String(), "- network") {
		t.Errorf("stdout does not list the destroyed resource with a removal marker:\n%s", stdout.String())
	}
	if strings.Contains(stdout.String(), "+ network") {
		t.Errorf("stdout marks a DESTROYED resource as created:\n%s", stdout.String())
	}

	st, gerr := backendFor(dir).Get(context.Background(), "dev")
	if gerr != nil {
		t.Fatalf("reading state: %v", gerr)
	}
	if _, ok := st.Get(address.Address{Name: "network"}); ok {
		t.Error("network is still recorded in state after a confirmed destroy")
	}
	if _, err := os.Stat(filepath.Join(dir, ".infra", "state", "dev.lock")); !os.IsNotExist(err) {
		t.Error("destroy must release the lock when it finishes")
	}
}

func TestDestroyForgetsARetainedResourceWithoutDeletingIt(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources: {}
`)
	ctx := context.Background()
	cloudPath := filepath.Join(dir, testprovider.DefaultCloudPath)
	prov := testprovider.New(cloudPath)
	rs, err := prov.Create(ctx, &resource.DesiredResource{
		Address: address.Address{Name: "network"},
		Type:    "test.network",
		Attrs: map[string]value.Value{
			"cidr": value.String("10.20.0.0/16", value.SourceExplicit),
		},
		Lifecycle: resource.Lifecycle{Retain: true},
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
	rs.Lifecycle = resource.Lifecycle{Retain: true}
	st := state.New("myapp", "dev")
	st.Set(rs)
	seedState(t, dir, "dev", st)

	opts := &GlobalOptions{Dir: dir, Parallelism: 4, AutoApprove: true}
	cmd := newDestroyCommand(opts)
	cmd.SetArgs([]string{"dev"})
	cmd.SetIn(strings.NewReader(""))
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	if err := cmd.Execute(); !errors.Is(err, errChanges) {
		t.Fatalf("Execute() = %v, want errChanges", err)
	}
	// Forget's symbol is "=" (planner.OpKind.Symbol), not destroy's "-": a
	// plan that rendered a retained resource with "-" would be telling the
	// user it is about to call Delete on infrastructure retain exists to
	// protect.
	if !strings.Contains(stdout.String(), "= test.network.network") {
		t.Errorf("plan did not render the retained resource as forgotten:\n%s", stdout.String())
	}

	// The apply SUMMARY must agree with the plan. It marks removals by
	// finding the address absent from state, and a forgotten resource is
	// absent for exactly the same reason a destroyed one is — so this used
	// to print "- network", telling the user the resource retain exists to
	// protect had been deleted. Result.Forgotten is what lets the summary
	// say "=" here, matching the plan the user just approved.
	if !strings.Contains(stdout.String(), "= network") {
		t.Errorf("the apply summary did not mark the retained resource as forgotten:\n%s", stdout.String())
	}
	if strings.Contains(stdout.String(), "- network") {
		t.Errorf("the apply summary marked a RETAINED resource as deleted — the one claim retain exists to make false:\n%s", stdout.String())
	}

	afterSt, gerr := backendFor(dir).Get(ctx, "dev")
	if gerr != nil {
		t.Fatalf("reading state: %v", gerr)
	}
	if _, ok := afterSt.Get(address.Address{Name: "network"}); ok {
		t.Error("retained resource is still recorded in state after destroy")
	}

	// The whole point of retain: the real infrastructure must still exist.
	current, rerr := prov.Read(ctx, rs)
	if rerr != nil {
		t.Fatalf("reading back the fake cloud: %v", rerr)
	}
	if current == nil {
		t.Error("destroy called Delete on a retained resource — retain must remove it from state without touching the provider")
	}
}

// TestDestroyRefusesWhenAPreventDestroyResourceIsPresent is the regression
// test the fix-round-1 review found missing: the single most destructive
// command in the product had its most important protection unguarded by
// any committed test, proven only by hand. destroy.go itself contains no
// prevent_destroy logic at all (by design — see this file's package doc
// comment on destroy.go and `grep PreventDestroy internal/cli/destroy.go`,
// which returns nothing); the guard is entirely the planner's, reached
// through the same computePlan (Task 13) plan/apply already share. This
// pins the INTEGRATION of that delegation, not the guard's own logic
// (internal/planner already owns that — TestPreventDestroyIsAPlanTimeError).
//
// The fixture deliberately mixes a protected resource with a plain,
// otherwise-destroyable one: planner.Compute keeps computing operations for
// every OTHER resource even while refusing the protected one (confirmed
// directly: three seeded resources — one plain, one retain, one
// prevent_destroy — through Compute with an empty config produced
// "doomed -> destroy" and "keeper -> forget" alongside the prevent_destroy
// error, not in place of them). Since planDiags.HasErrors() makes
// computePlan return an error before RunE ever reaches confirmation or
// backend.Lock, that plan-time refusal must abort the ENTIRE destroy, not
// just skip the protected resource — so the plain one must survive too. A
// destroy that "helpfully" destroyed everything except the guarded resource
// would still be a destroy nobody approved.
func TestDestroyRefusesWhenAPreventDestroyResourceIsPresent(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources: {}
`)
	ctx := context.Background()
	prov := testprovider.New(filepath.Join(dir, testprovider.DefaultCloudPath))

	guarded, err := prov.Create(ctx, &resource.DesiredResource{
		Address: address.Address{Name: "guarded"},
		Type:    "test.network",
		Attrs: map[string]value.Value{
			"cidr": value.String("10.20.0.0/16", value.SourceExplicit),
		},
		Lifecycle: resource.Lifecycle{PreventDestroy: true},
	})
	if err != nil {
		t.Fatalf("seeding the guarded resource: %v", err)
	}
	plain, err := prov.Create(ctx, &resource.DesiredResource{
		Address: address.Address{Name: "plain"},
		Type:    "test.network",
		Attrs: map[string]value.Value{
			"cidr": value.String("10.30.0.0/16", value.SourceExplicit),
		},
	})
	if err != nil {
		t.Fatalf("seeding the plain resource: %v", err)
	}

	// The state record carries the guard explicitly. The fake provider's
	// Create no longer echoes DesiredResource.Lifecycle back onto the state
	// it returns: the executor stamps lifecycle onto state from the plan
	// (internal/executor/apply.go), so a provider that also set it would be
	// a second writer of a field it does not own. Seeding it here says out
	// loud what this fixture needs — a state record whose guard is set —
	// instead of borrowing it from a provider round trip.
	guarded.Lifecycle = resource.Lifecycle{PreventDestroy: true}
	st := state.New("myapp", "dev")
	st.Set(guarded)
	st.Set(plain)
	seedState(t, dir, "dev", st)

	opts := &GlobalOptions{Dir: dir, Parallelism: 4, AutoApprove: true}
	cmd := newDestroyCommand(opts)
	cmd.SetArgs([]string{"dev"})
	cmd.SetIn(strings.NewReader(""))
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err = cmd.Execute()
	if err == nil || errors.Is(err, errChanges) {
		t.Fatalf("Execute() = %v, want a plain error — prevent_destroy must fail the whole destroy "+
			"(exit 1), not report success or partial success", err)
	}
	if !strings.Contains(stderr.String(), "prevent_destroy") {
		t.Errorf("stderr does not name the guard:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "guarded") {
		t.Errorf("stderr does not name the protected resource:\n%s", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dir, ".infra", "state", "dev.lock")); !os.IsNotExist(err) {
		t.Error("a plan-time refusal must never reach backend.Lock")
	}

	afterSt, gerr := backendFor(dir).Get(ctx, "dev")
	if gerr != nil {
		t.Fatalf("reading state: %v", gerr)
	}
	if _, ok := afterSt.Get(address.Address{Name: "guarded"}); !ok {
		t.Error("the prevent_destroy resource was removed from state despite the guard")
	}
	if _, ok := afterSt.Get(address.Address{Name: "plain"}); !ok {
		t.Error("the plain resource was destroyed even though the whole plan should have been refused")
	}

	// The strongest possible proof retain/prevent_destroy exist for: both
	// resources must still be real, at the provider, not merely still
	// recorded in state.
	if current, rerr := prov.Read(ctx, guarded); rerr != nil || current == nil {
		t.Errorf("guarded no longer exists at the provider (Read = %v, %v)", current, rerr)
	}
	if current, rerr := prov.Read(ctx, plain); rerr != nil || current == nil {
		t.Errorf("plain no longer exists at the provider (Read = %v, %v)", current, rerr)
	}
}

// TestDestroyMixOfPlainAndRetainedResourcesInOneRun closes the trap this
// task's own brief warned about and fix-round-1 confirmed was still open:
// TestDestroyWithCorrectConfirmationRemovesEverything only ever destroys a
// single plain resource, and TestDestroyForgetsARetainedResourceWithoutDeletingIt
// only ever forgets a single retained one — neither proves that destroying
// ONE resource in a run leaves a RETAINED SIBLING in the same run alone. A
// bug that forgot the retain lifecycle whenever any other resource in the
// same plan was a genuine destroy would pass both of those tests and only
// show up here.
func TestDestroyMixOfPlainAndRetainedResourcesInOneRun(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources: {}
`)
	ctx := context.Background()
	prov := testprovider.New(filepath.Join(dir, testprovider.DefaultCloudPath))

	doomed, err := prov.Create(ctx, &resource.DesiredResource{
		Address: address.Address{Name: "doomed"},
		Type:    "test.network",
		Attrs: map[string]value.Value{
			"cidr": value.String("10.20.0.0/16", value.SourceExplicit),
		},
	})
	if err != nil {
		t.Fatalf("seeding the plain resource: %v", err)
	}
	keeper, err := prov.Create(ctx, &resource.DesiredResource{
		Address: address.Address{Name: "keeper"},
		Type:    "test.network",
		Attrs: map[string]value.Value{
			"cidr": value.String("10.30.0.0/16", value.SourceExplicit),
		},
		Lifecycle: resource.Lifecycle{Retain: true},
	})
	if err != nil {
		t.Fatalf("seeding the retained resource: %v", err)
	}

	// The state record carries the guard explicitly. The fake provider's
	// Create no longer echoes DesiredResource.Lifecycle back onto the state
	// it returns: the executor stamps lifecycle onto state from the plan
	// (internal/executor/apply.go), so a provider that also set it would be
	// a second writer of a field it does not own. Seeding it here says out
	// loud what this fixture needs — a state record whose guard is set —
	// instead of borrowing it from a provider round trip.
	keeper.Lifecycle = resource.Lifecycle{Retain: true}
	st := state.New("myapp", "dev")
	st.Set(doomed)
	st.Set(keeper)
	seedState(t, dir, "dev", st)

	opts := &GlobalOptions{Dir: dir, Parallelism: 4, AutoApprove: true}
	cmd := newDestroyCommand(opts)
	cmd.SetArgs([]string{"dev"})
	cmd.SetIn(strings.NewReader(""))
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	if err := cmd.Execute(); !errors.Is(err, errChanges) {
		t.Fatalf("Execute() = %v, want errChanges", err)
	}

	afterSt, gerr := backendFor(dir).Get(ctx, "dev")
	if gerr != nil {
		t.Fatalf("reading state: %v", gerr)
	}
	if _, ok := afterSt.Get(address.Address{Name: "doomed"}); ok {
		t.Error("doomed is still recorded in state after destroy")
	}
	if _, ok := afterSt.Get(address.Address{Name: "keeper"}); ok {
		t.Error("keeper is still recorded in state after destroy")
	}

	if current, rerr := prov.Read(ctx, doomed); rerr != nil {
		t.Fatalf("reading back doomed: %v", rerr)
	} else if current != nil {
		t.Error("doomed still exists at the provider — it should have been deleted, not forgotten")
	}
	if current, rerr := prov.Read(ctx, keeper); rerr != nil {
		t.Fatalf("reading back keeper: %v", rerr)
	} else if current == nil {
		t.Error("keeper no longer exists at the provider — retain must survive even when a sibling in the same run is genuinely destroyed")
	}
}

// TestDestroyLockConflictNamesTheHolder pins that destroy actually takes
// the environment lock, not merely that no lock file survives it. The
// other tests in this file only assert lock ABSENCE after destroy runs,
// which cannot tell "never locked" apart from "locked, then released" —
// exactly the trap this task's brief warned about. Pre-locking the
// environment as another holder, mirroring apply's
// TestApplyLockConflictNamesTheHolder, is what closes that gap: if destroy
// never called backend.Lock at all, this would proceed to execute (or
// crash) instead of failing on the contending holder.
func TestDestroyLockConflictNamesTheHolder(t *testing.T) {
	dir := seedOneNetwork(t, "dev")
	backend := backendFor(dir)
	lockCtx := state.WithOperation(context.Background(), "some-other-run")
	if _, err := backend.Lock(lockCtx, "dev"); err != nil {
		t.Fatalf("pre-locking dev: %v", err)
	}
	defer backend.ForceUnlock("dev")

	opts := &GlobalOptions{Dir: dir, Parallelism: 4, AutoApprove: true}
	cmd := newDestroyCommand(opts)
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

	st, gerr := backend.Get(context.Background(), "dev")
	if gerr != nil {
		t.Fatalf("reading state: %v", gerr)
	}
	if _, ok := st.Get(address.Address{Name: "network"}); !ok {
		t.Error("network was destroyed despite the environment being locked by another holder")
	}
}

// TestDestroyLockRecordsItsOwnOperationName pins the OTHER direction from
// TestDestroyLockConflictNamesTheHolder: that one proves destroy correctly
// REPORTS a lock a different process already stamped with its own
// operation name, but never that destroy stamps its own — an unrelated
// call, destroy.go's `state.WithOperation(ctx, "destroy")` immediately
// before backend.Lock. Deleting that call is invisible to every other test
// in this file, but it is user-facing: a second process hitting the lock
// while destroy holds it would be told the holder's operation is
// "unknown" rather than "destroy" (spec §9.2, §44 — an error must say what
// is actually happening).
//
// The fake provider's cloud-file latency (LatencyMS) is the established
// pattern for holding an operation in flight long enough for a concurrent
// look to observe it (see interrupt_test.go, refresh_test.go,
// executor/context_test.go) — here it holds destroy's in-lock refresh
// Read, giving a polling loop a real window to Inspect the lock while
// destroy still holds it, rather than guessing at a fixed sleep.
func TestDestroyLockRecordsItsOwnOperationName(t *testing.T) {
	dir := seedOneNetwork(t, "dev")
	cloudPath := filepath.Join(dir, testprovider.DefaultCloudPath)
	cloud, err := testprovider.LoadCloud(cloudPath)
	if err != nil {
		t.Fatalf("loading the fake cloud to add latency: %v", err)
	}
	cloud.LatencyMS = 300
	if err := cloud.Save(cloudPath); err != nil {
		t.Fatalf("saving the fake cloud with latency: %v", err)
	}

	opts := &GlobalOptions{Dir: dir, Parallelism: 4, AutoApprove: true}
	cmd := newDestroyCommand(opts)
	cmd.SetArgs([]string{"dev"})
	cmd.SetIn(strings.NewReader(""))
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	done := make(chan error, 1)
	go func() { done <- cmd.Execute() }()

	backend := backendFor(dir)
	deadline := time.Now().Add(5 * time.Second)
	var held bool
	var op string
	for time.Now().Before(deadline) {
		lock, ok, ierr := backend.Inspect("dev")
		if ierr != nil {
			t.Fatalf("Inspect: %v", ierr)
		}
		if ok {
			held, op = true, lock.Operation
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !held {
		t.Fatal("destroy never took the lock within the deadline")
	}
	if op != "destroy" {
		t.Errorf(`lock.Operation = %q, want "destroy"`, op)
	}

	select {
	case err := <-done:
		if !errors.Is(err, errChanges) {
			t.Fatalf("Execute() = %v, want errChanges", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("destroy did not finish")
	}
}

// TestDestroyReportsFailureAndReturnsPlainError pins that a failed delete
// during destroy is reported as a plain error, not silently treated as
// success and not conflated with errChanges (which means "completed with
// changes", not "failed"). Mirrors apply's
// TestApplyReportsFailureAndReturnsPlainError with an injected delete
// failure instead of a create failure.
func TestDestroyReportsFailureAndReturnsPlainError(t *testing.T) {
	dir := seedOneNetwork(t, "dev")

	cloudPath := filepath.Join(dir, testprovider.DefaultCloudPath)
	cloud, err := testprovider.LoadCloud(cloudPath)
	if err != nil {
		t.Fatalf("loading the fake cloud to add a failure rule: %v", err)
	}
	cloud.Failures = append(cloud.Failures, testprovider.FailureRule{
		Op: "delete", Address: "network", Nth: 1, Message: "injected failure",
	})
	if err := cloud.Save(cloudPath); err != nil {
		t.Fatalf("saving the fake cloud with a failure rule: %v", err)
	}

	opts := &GlobalOptions{Dir: dir, Parallelism: 4, AutoApprove: true}
	cmd := newDestroyCommand(opts)
	cmd.SetArgs([]string{"dev"})
	cmd.SetIn(strings.NewReader(""))
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	execErr := cmd.Execute()
	if execErr == nil || errors.Is(execErr, errChanges) {
		t.Fatalf("Execute() = %v, want a plain error — a failed delete must not report success", execErr)
	}
	if !strings.Contains(stdout.String(), "Apply complete: 0 applied, 1 failed, 0 skipped.") {
		t.Errorf("stdout does not report the failure:\n%s", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(dir, ".infra", "state", "dev.lock")); !os.IsNotExist(err) {
		t.Error("destroy must release the lock even when an operation fails")
	}
	st, gerr := backendFor(dir).Get(context.Background(), "dev")
	if gerr != nil {
		t.Fatalf("reading state: %v", gerr)
	}
	if _, ok := st.Get(address.Address{Name: "network"}); !ok {
		t.Error("network was dropped from state despite its delete failing")
	}
}

// TestDestroyPlanConvergesToNoChangesInsideTheLock reaches the one branch
// of destroy.go no other test in this file does: the post-lock
// "!p2.HasChanges()" path, printing "Nothing remained to destroy once the
// environment lock was acquired." Every other test keeps state and
// provider reality identical across destroy's two computePlan calls, so p2
// always agrees with p and this branch is never taken.
//
// mutatingReader (apply_test.go) stands in for a real concurrent actor: its
// before hook fires the moment confirm's bufio.Scanner reads the typed
// environment name — after the unlocked preview, before backend.Lock is
// taken — and simulates another process finishing the same destroy out of
// band: it deletes the resource from the fake cloud directly (prov.Delete)
// and overwrites the state file directly (bypassing state.Local.Put's
// lock check, which this test's own process does not yet hold at that
// point — precisely because the confirmation prompt runs before the lock
// is taken). The in-lock re-plan then observes nothing left in state and
// finds no operations.
func TestDestroyPlanConvergesToNoChangesInsideTheLock(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources: {}
`)
	ctx := context.Background()
	cloudPath := filepath.Join(dir, testprovider.DefaultCloudPath)
	prov := testprovider.New(cloudPath)
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

	statePath := filepath.Join(dir, StateDirName, "state", "dev.json")
	finishOutOfBand := func() {
		if err := prov.Delete(ctx, rs); err != nil {
			t.Fatalf("simulating an out-of-band delete: %v", err)
		}
		empty := state.New("myapp", "dev")
		data, err := empty.Encode()
		if err != nil {
			t.Fatalf("encoding the post-destroy state: %v", err)
		}
		if err := os.WriteFile(statePath, data, 0o600); err != nil {
			t.Fatalf("writing the post-destroy state directly: %v", err)
		}
	}

	opts := &GlobalOptions{Dir: dir, Parallelism: 4}
	cmd := newDestroyCommand(opts)
	cmd.SetArgs([]string{"dev"})
	cmd.SetIn(&mutatingReader{before: finishOutOfBand, body: strings.NewReader("dev\n")})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() = %v, want nil — nothing was left to destroy once the in-lock re-plan converged", err)
	}
	if !strings.Contains(stdout.String(), "Nothing remained to destroy once the environment lock was acquired.") {
		t.Errorf("stdout does not report the post-lock convergence:\n%s", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(dir, ".infra", "state", "dev.lock")); !os.IsNotExist(err) {
		t.Error("destroy must release the lock after the post-lock convergence path")
	}
}

// TestDestroyProjectNameComesFromAppliedState is fix-round-1's Important 3,
// proven against the real write path rather than a test shortcut. Every
// other test in this file that asserts a project name in destroy's plan
// header (TestDestroyWithCorrectConfirmationRemovesEverything included)
// seeds state directly with state.New("myapp", …), which passes even if
// nothing in production ever stamps the project — and, before this fix,
// nothing did (grep "state.New(" | grep -v _test.go found no production
// call site). That made those assertions green while guarding behaviour
// the binary did not have. This test seeds through a REAL `infra apply`
// (newApplyCommand, not a hand-built fixture) so the state destroy later
// reads is exactly what production would write, and would have failed
// before apply.go's computePlan started stamping st.Project = cfg.Project.
func TestDestroyProjectNameComesFromAppliedState(t *testing.T) {
	dir := applyRealNetwork(t, "dev")

	opts := &GlobalOptions{Dir: dir, Parallelism: 4}
	cmd := newDestroyCommand(opts)
	cmd.SetArgs([]string{"dev"})
	cmd.SetIn(strings.NewReader("dev\n"))
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	if err := cmd.Execute(); !errors.Is(err, errChanges) {
		t.Fatalf("Execute() = %v, want errChanges", err)
	}
	if !strings.Contains(stdout.String(), `Plan for project "myapp"`) {
		t.Errorf("stdout does not name the project from a REAL apply's state:\n%s", stdout.String())
	}
}

// applyRealNetwork runs a real `infra apply` end to end (through
// newApplyCommand, not a hand-seeded fixture) to create one test.network
// resource, and returns the project directory. See
// TestDestroyProjectNameComesFromAppliedState for why this, rather than
// seedOneNetwork, is what that test needs.
func applyRealNetwork(t *testing.T, environment string) string {
	t.Helper()
	dir := projectDir(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: 10.20.0.0/16
`)
	opts := &GlobalOptions{Dir: dir, Parallelism: 4, AutoApprove: true}
	cmd := newApplyCommand(opts)
	cmd.SetArgs([]string{environment})
	cmd.SetIn(strings.NewReader(""))
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	if err := cmd.Execute(); !errors.Is(err, errChanges) {
		t.Fatalf("seeding via a real apply: Execute() = %v, want errChanges", err)
	}
	return dir
}

// seedOneNetwork creates one test.network resource through the fake
// provider and records it in state for environment, returning the project
// directory. Shared by the confirmation tests above, which only care that
// something exists to destroy, not about its specific attributes.
func seedOneNetwork(t *testing.T, environment string) string {
	t.Helper()
	dir := projectDir(t, `
project: myapp
resources: {}
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
	st := state.New("myapp", environment)
	st.Set(rs)
	seedState(t, dir, environment, st)
	return dir
}

// TestApplyRecordsDependenciesInStateSoALaterDestroyCanOrderItself pins the
// one bookkeeping field the executor was still dropping.
//
// Spec §14 makes ResourceState.Dependencies the ONLY source of
// destroy-ordering edges once a resource leaves configuration: the planner's
// dependentsOf reads state, not config, for OpDestroy and OpForget, because
// by then neither the resource nor some of its dependents are in
// configuration at all. Nothing ever wrote the field. Every state file in
// existence carried an empty Dependencies, so invariant 4 held for
// everything still configured and silently did not for the one case where
// state is the only source — a teardown of resources already deleted from
// the YAML.
//
// The two halves are asserted together on purpose: that apply WRITES the
// field, and that destroy planning READS it back as a dependent warning.
// Either alone would pass with the other broken.
func TestApplyRecordsDependenciesInStateSoALaterDestroyCanOrderItself(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: 10.20.0.0/16
  db:
    type: test.database
    engine: postgres
    network: ${network.id}
`)
	applyOpts := &GlobalOptions{Dir: dir, Parallelism: 4, AutoApprove: true}
	applyCmd := newApplyCommand(applyOpts)
	applyCmd.SetArgs([]string{"dev"})
	applyCmd.SetIn(strings.NewReader(""))
	var applyOut, applyErr bytes.Buffer
	applyCmd.SetOut(&applyOut)
	applyCmd.SetErr(&applyErr)
	if err := applyCmd.Execute(); !errors.Is(err, errChanges) {
		t.Fatalf("apply = %v, want errChanges:\n%s%s", err, applyOut.String(), applyErr.String())
	}

	st, err := backendFor(dir).Get(context.Background(), "dev")
	if err != nil {
		t.Fatalf("reading state: %v", err)
	}
	db, ok := st.Get(address.Address{Name: "db"})
	if !ok {
		t.Fatal("db is not in state")
	}
	if len(db.Dependencies) != 1 || db.Dependencies[0].String() != "network" {
		t.Fatalf("db.Dependencies = %v, want [network] — configuration's dependency edges must survive into state, or a later destroy has no ordering at all", db.Dependencies)
	}
	if net, ok := st.Get(address.Address{Name: "network"}); !ok || len(net.Dependencies) != 0 {
		t.Errorf("network.Dependencies = %v, want empty — it depends on nothing", net.Dependencies)
	}

	// Now the consumer. Destroy plans from state alone (it compiles no
	// configuration), so the dependent warning on network can only have come
	// from db's recorded Dependencies.
	destroyOpts := &GlobalOptions{Dir: dir, Parallelism: 4}
	destroyCmd := newDestroyCommand(destroyOpts)
	destroyCmd.SetArgs([]string{"dev"})
	destroyCmd.SetIn(strings.NewReader("no\n"))
	var destroyOut, destroyErr bytes.Buffer
	destroyCmd.SetOut(&destroyOut)
	destroyCmd.SetErr(&destroyErr)
	_ = destroyCmd.Execute() // declined at the prompt; the plan is what matters

	out := destroyOut.String()
	if !strings.Contains(out, "This resource has 1 dependent resource.") {
		t.Errorf("the destroy plan does not warn that network has a dependent — state's Dependencies did not reach the planner:\n%s", out)
	}
}
