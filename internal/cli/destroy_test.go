package cli

import (
	"bytes"
	"context"
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
	// The plan header must name the real project, taken from state.State.Project
	// (seedOneNetwork uses state.New("myapp", ...)) — destroy never calls
	// config.Load or compiler.Compile, so this is the only source for it, and
	// getting it wrong would mislead the person about to confirm a destroy
	// against the wrong project.
	if !strings.Contains(stdout.String(), `Plan for project "myapp"`) {
		t.Errorf("stdout does not name the project from state:\n%s", stdout.String())
	}
	// executor.Render (Task 12) is the one shared result renderer for both
	// apply and destroy — it reports "Apply complete" and marks every
	// successfully-applied operation with "+" regardless of whether the
	// underlying operation was a create or a destroy (see its own doc
	// comment: "a resource can be Applied with nothing in st when it was
	// destroyed or forgotten"). destroy does not fork a second renderer to
	// get "Destroy complete!" wording — see this package's destroy.go doc
	// comment and apply.go's note on why two renderers for one Result is a
	// defect, not a style choice.
	if !strings.Contains(stdout.String(), "Apply complete: 1 applied, 0 failed, 0 skipped.") {
		t.Errorf("stdout does not report completion:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "+ network") {
		t.Errorf("stdout does not list the destroyed resource as applied:\n%s", stdout.String())
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
