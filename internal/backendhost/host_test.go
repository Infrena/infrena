package backendhost

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/infrena/infrena/internal/state"
)

// The host presents a plugin as an ordinary Backend, so nothing upstream
// learns that state became remote.
func TestOpenPresentsAPluginAsAnOrdinaryBackend(t *testing.T) {
	dir := buildFakeBackend(t)

	b, closeFn, err := Open(context.Background(), "memory", "", []string{dir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()

	ctx := context.Background()
	if _, err := b.Lock(ctx, "dev"); err != nil {
		t.Fatal(err)
	}
	st := state.New("p", "dev")
	if err := b.Put(ctx, "dev", st); err != nil {
		t.Fatal(err)
	}
	got, err := b.Get(ctx, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if got.Environment != "dev" {
		t.Errorf("Environment = %q, want dev", got.Environment)
	}
}

// A lock conflict must survive the wire as ErrLocked, or every caller that
// tests for it silently stops working when state goes remote.
func TestALockConflictArrivesAsErrLocked(t *testing.T) {
	dir := buildFakeBackend(t)
	b, closeFn, err := Open(context.Background(), "memory", "", []string{dir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	ctx := context.Background()

	if _, err := b.Lock(ctx, "dev"); err != nil {
		t.Fatal(err)
	}
	_, err = b.Lock(ctx, "dev")
	if !errors.Is(err, state.ErrLocked) {
		t.Fatalf("second Lock returned %v, want ErrLocked", err)
	}
	// The message is what a user reads, and a conflict that does not say who
	// holds the environment is one nobody can act on.
	if !strings.Contains(err.Error(), "locked") {
		t.Errorf("the conflict lost the backend's own message: %v", err)
	}
}

// A plugin that dies mid-run must produce an error naming the backend, not a
// nil dereference. The executor writes state once per operation, so this is
// the failure mode that matters most.
func TestABackendThatDiesMidRunErrorsClearly(t *testing.T) {
	dir := buildFakeBackend(t)
	b, closeFn, err := Open(context.Background(), "crash-on-put", "", []string{dir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()

	err = b.Put(context.Background(), "dev", state.New("p", "dev"))
	if err == nil {
		t.Fatal("a dead backend accepted a write")
	}
	if !strings.Contains(err.Error(), "crash-on-put") {
		t.Errorf("error does not name the backend: %v", err)
	}
	// Its last words explain the exit; the sentence alone only reports that
	// something ended.
	if !strings.Contains(err.Error(), "on purpose") {
		t.Errorf("error does not quote what the backend said before it died: %v", err)
	}
	// AND EVERY LATER CALL, rather than one call failing and the next
	// blocking forever on a pipe nobody is reading.
	if _, err := b.Get(context.Background(), "dev"); err == nil {
		t.Error("a dead backend answered a read")
	}
}

// THE HOST FILLS THE HOLDER. A backend plugin is a child of this process, so a
// lock it stamped itself would name a PID that stops existing when the run
// ends — and `infra state unlock` prints that PID for a user to go and check.
// The operation is not knowable inside the plugin at all.
func TestTheHostStampsTheLockWithItsOwnIdentity(t *testing.T) {
	dir := buildFakeBackend(t)
	b, closeFn, err := Open(context.Background(), "memory", "", []string{dir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()

	ctx := state.WithOperation(context.Background(), "apply")
	held, err := b.Lock(ctx, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if held.PID != os.Getpid() {
		t.Errorf("PID = %d, want this process (%d): the plugin stamped its own", held.PID, os.Getpid())
	}
	if held.Operation != "apply" {
		t.Errorf("Operation = %q, want apply", held.Operation)
	}
	if held.Environment != "dev" {
		t.Errorf("Environment = %q, want dev", held.Environment)
	}
	// Inspect reads back what Lock recorded, over the wire, so the holder a
	// conflict would print is the holder that was stored.
	seen, locked, err := b.Inspect(ctx, "dev")
	if err != nil || !locked {
		t.Fatalf("Inspect = %v, %v, %v", seen, locked, err)
	}
	if seen.PID != held.PID || seen.Operation != held.Operation {
		t.Errorf("Inspect returned %+v, want %+v", seen, held)
	}
}

// The three methods the command line calls and nothing else does. They reach
// the plugin like the rest, and ForceUnlock reports a lock that was not there
// rather than a silent success.
func TestListAndForceUnlockCrossTheWire(t *testing.T) {
	dir := buildFakeBackend(t)
	b, closeFn, err := Open(context.Background(), "memory", "", []string{dir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	ctx := context.Background()

	envs, err := b.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(envs) != 0 {
		t.Errorf("List = %v, want nothing for a backend holding no state", envs)
	}

	if _, err := b.Lock(ctx, "dev"); err != nil {
		t.Fatal(err)
	}
	if err := b.Put(ctx, "dev", state.New("p", "dev")); err != nil {
		t.Fatal(err)
	}
	if envs, err = b.List(ctx); err != nil || len(envs) != 1 || envs[0] != "dev" {
		t.Errorf("List = %v, %v, want [dev]", envs, err)
	}

	if err := b.ForceUnlock(ctx, "dev"); err != nil {
		t.Fatal(err)
	}
	if err := b.ForceUnlock(ctx, "dev"); err == nil {
		t.Error("force-unlocking an environment that was not locked looked like it worked")
	}
}

// An empty Get is a project that has never applied anything, which is the
// ordinary case for the commands that ask. It becomes an empty state here
// rather than a nil the caller has to remember to check, exactly as the local
// backend already answers.
func TestAnEmptyGetBecomesAnEmptyState(t *testing.T) {
	dir := buildFakeBackend(t)
	b, closeFn, err := Open(context.Background(), "memory", "", []string{dir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()

	got, err := b.Get(context.Background(), "never-applied")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("Get returned no state and no error")
	}
	if got.Environment != "never-applied" {
		t.Errorf("Environment = %q, want never-applied", got.Environment)
	}
	if len(got.Resources) != 0 {
		t.Errorf("Resources = %v, want none", got.Resources)
	}
}

// A backend that is named but not installed says so, names where it looked,
// and says how to get it. Never a fallback to local: that would write state
// somewhere the user did not ask for.
func TestAMissingBackendSaysWhereItLookedAndHowToInstallIt(t *testing.T) {
	_, _, err := Open(context.Background(), "s3", "", []string{t.TempDir()}, nil)
	if err == nil {
		t.Fatal("a backend that is not installed opened")
	}
	for _, want := range []string{"s3", "infrena-backend-s3", "plugins install"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// buildFakeBackend compiles testdata/backendplugin and returns a directory
// holding it under both names the tests launch it by.
//
// Built ONCE per test binary: `go build` is the slowest thing here by a wide
// margin, and every case wants the same two binaries. The second name is a
// hard link to the first, because the program decides what to do from the name
// it was launched under, so one build serves both.
func buildFakeBackend(t *testing.T) string {
	t.Helper()
	fakeOnce.Do(buildFake)
	if fakeErr != nil {
		t.Fatalf("building the test backend: %v", fakeErr)
	}
	return fakeDir
}

var (
	fakeOnce sync.Once
	fakeDir  string
	fakeErr  error
)

func buildFake() {
	if runtime.GOOS == "windows" {
		fakeErr = errors.New("the test backend is linked under a second name, which this test does not do on Windows")
		return
	}
	dir, err := os.MkdirTemp("", "infrena-backend-*")
	if err != nil {
		fakeErr = err
		return
	}

	// The binary NAME is how the host finds it, so this is not arbitrary.
	first := filepath.Join(dir, BinaryName("memory"))
	cmd := exec.Command("go", "build", "-o", first, "./testdata/backendplugin")
	if out, err := cmd.CombinedOutput(); err != nil {
		fakeErr = fmt.Errorf("%v\n%s", err, out)
		return
	}
	if err := os.Link(first, filepath.Join(dir, BinaryName("crash-on-put"))); err != nil {
		fakeErr = err
		return
	}
	fakeDir = dir
}
