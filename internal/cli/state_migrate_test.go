package cli

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

	"github.com/infrena/infrena/internal/backendhost"
	"github.com/infrena/infrena/internal/state"
	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/resource"
	"github.com/infrena/infrena/pkg/value"
	testprovider "github.com/infrena/infrena/providers/test"
)

func TestMigrateMovesEveryEnvironment(t *testing.T) {
	dir := newProjectMigratingLocalToFake(t) // migrate_from: local, backend: a fake remote
	seedLocalState(t, dir, "dev", "production")

	stdout, _, code := runCommand(t, dir, "state", "migrate")

	if code != ExitOK {
		t.Fatalf("exit = %d\n%s", code, stdout)
	}
	for _, env := range []string{"dev", "production"} {
		if !destinationHasState(t, dir, env) {
			t.Errorf("%s did not arrive at the destination", env)
		}
	}
}

// MIGRATION COPIES. The source is what a user reverts to when the new backend
// turns out to have a bug in it, so nothing is removed from it at all.
func TestMigrateLeavesTheSourceIntact(t *testing.T) {
	dir := newProjectMigratingLocalToFake(t)
	seedLocalState(t, dir, "dev")

	if _, stderr, code := runCommand(t, dir, "state", "migrate"); code != ExitOK {
		t.Fatalf("exit = %d\n%s", code, stderr)
	}

	if !sourceStillHasState(t, dir, "dev") {
		t.Error("migrate emptied the source")
	}
}

// THE CI CASE. A migration through a pipeline is two commits, and between
// them this block sits in committed configuration. If an ordinary command
// consulted it, a SUCCESSFUL migration would break every plan until somebody
// pushed the removal.
func TestMigrateFromDoesNotAffectOrdinaryCommands(t *testing.T) {
	dir := newProjectMigratingLocalToFake(t)
	seedLocalState(t, dir, "dev")
	if _, stderr, code := runCommand(t, dir, "state", "migrate"); code != ExitOK {
		t.Fatalf("setup migration failed: %s", stderr)
	}

	// migrate_from: is STILL in the config, and the destination now has state.
	for _, args := range [][]string{
		{"plan", "dev"}, {"validate"}, {"state", "list", "dev"},
	} {
		_, stderr, code := runCommand(t, dir, args...)
		if code == ExitError {
			t.Errorf("%v failed with migrate_from still present:\n%s", args, stderr)
		}
	}
}

// A re-run must be safe. CI re-runs happen, and the natural response to a red
// pipeline is to add --force to the workflow file, where it then sits on every
// future run. Succeeding as a no-op is what stops that.
func TestMigratingTwiceIsANoOpWhenBothEndsAgree(t *testing.T) {
	dir := newProjectMigratingLocalToFake(t)
	seedLocalState(t, dir, "dev")
	runCommand(t, dir, "state", "migrate")

	stdout, _, code := runCommand(t, dir, "state", "migrate")

	if code != ExitOK {
		t.Fatalf("a second migration failed: exit %d\n%s", code, stdout)
	}
	if !strings.Contains(stdout, "already migrated") {
		t.Errorf("output does not say it was already done:\n%s", stdout)
	}
}

// Differing state at both ends means somebody has been applying to one of
// them. Choosing a winner silently destroys real work.
func TestMigrateRefusesWhenBothEndsHoldDifferentState(t *testing.T) {
	dir := newProjectMigratingLocalToFake(t)
	seedLocalState(t, dir, "dev")
	seedDestinationState(t, dir, "dev", "something else entirely")

	_, stderr, code := runCommand(t, dir, "state", "migrate")

	if code == ExitOK {
		t.Fatal("migrate overwrote differing state without being asked")
	}
	if !strings.Contains(stderr, "dev") {
		t.Errorf("refusal does not name the environment:\n%s", stderr)
	}
	// --force must not be the first thing offered. It is the same hazard as a
	// lock: false escape hatch, and it arrives the same way: somebody adding a
	// flag to get past a red build.
	first := strings.SplitN(stderr, "\n", 2)[0]
	if strings.Contains(first, "--force") {
		t.Errorf("the refusal leads with --force:\n%s", stderr)
	}
}

// A refusal must not have written the half it was refusing to write.
func TestARefusedMigrationLeavesTheDestinationUntouched(t *testing.T) {
	dir := newProjectMigratingLocalToFake(t)
	seedLocalState(t, dir, "dev", "production")
	seedDestinationState(t, dir, "dev", "something else entirely")

	if _, _, code := runCommand(t, dir, "state", "migrate"); code == ExitOK {
		t.Fatal("migrate overwrote differing state without being asked")
	}

	// production was fine to copy, and must not have been copied anyway: the
	// decision is made about the whole migration before anything is written.
	if destinationHasState(t, dir, "production") {
		t.Error("a refused migration wrote an environment to the destination")
	}
}

func TestForceOverwritesDifferingState(t *testing.T) {
	dir := newProjectMigratingLocalToFake(t)
	seedLocalState(t, dir, "dev")
	seedDestinationState(t, dir, "dev", "something else entirely")

	_, stderr, code := runCommand(t, dir, "state", "migrate", "--force")

	if code != ExitOK {
		t.Fatalf("--force did not overwrite: exit %d\n%s", code, stderr)
	}
	if !destinationStateMentions(t, dir, "dev", sourceProviderID(t, dir, "dev")) {
		t.Error("--force did not replace the destination's state with the source's")
	}
}

// A failure must leave the source authoritative. The half left done has to be
// the harmless one.
func TestAFailedMigrationLeavesTheSourceIntact(t *testing.T) {
	dir := newProjectMigratingLocalToFailingDestination(t)
	seedLocalState(t, dir, "dev")

	_, _, code := runCommand(t, dir, "state", "migrate")

	if code == ExitOK {
		t.Fatal("a failing destination reported success")
	}
	if !sourceStillHasState(t, dir, "dev") {
		t.Fatal("the source lost state when the destination failed")
	}
}

// No migrate_from: is not a migration, and the error should say what to add
// rather than reporting an internal condition.
func TestMigrateWithNoMigrateFromSaysWhatToAdd(t *testing.T) {
	dir := newProjectFixture(t)

	_, stderr, code := runCommand(t, dir, "state", "migrate")

	if code == ExitOK {
		t.Fatal("migrate succeeded with nothing to migrate from")
	}
	if !strings.Contains(stderr, "migrate_from") {
		t.Errorf("error does not name the block to add:\n%s", stderr)
	}
}

// Both ends are locked for the whole operation, and a lock held elsewhere
// stops the migration rather than being worked around.
func TestMigrateRefusesWhileTheSourceIsLocked(t *testing.T) {
	dir := newProjectMigratingLocalToFake(t)
	seedLocalState(t, dir, "dev")

	ctx := state.WithOperation(context.Background(), "some-other-run")
	if _, err := localBackend(dir).Lock(ctx, "dev"); err != nil {
		t.Fatal(err)
	}
	defer localBackend(dir).ForceUnlock(ctx, "dev")

	_, stderr, code := runCommand(t, dir, "state", "migrate")

	if code == ExitOK {
		t.Fatal("migrate ran while the source was locked by another run")
	}
	if !strings.Contains(stderr, "locked") {
		t.Errorf("refusal does not say the environment is locked:\n%s", stderr)
	}
	if destinationHasState(t, dir, "dev") {
		t.Error("migrate wrote to the destination despite the source being locked")
	}
}

// A migration releases both locks, or the next command finds an environment
// nobody is using and cannot be used.
func TestMigrateReleasesBothLocks(t *testing.T) {
	dir := newProjectMigratingLocalToFake(t)
	seedLocalState(t, dir, "dev")

	if _, stderr, code := runCommand(t, dir, "state", "migrate"); code != ExitOK {
		t.Fatalf("exit = %d\n%s", code, stderr)
	}

	if lockHeld(t, dir, "dev") {
		t.Error("migrate left a lock behind")
	}
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// newProjectMigratingLocalToFake is a project whose state lives locally and
// whose `backend:` block names a remote that stores state in a directory.
//
// A REAL BACKEND SUBPROCESS, not an in-process double, because the whole
// question this command answers is what two independently opened ends hold,
// and a double shared between them could not get that wrong.
func newProjectMigratingLocalToFake(t *testing.T) string {
	t.Helper()
	return newProjectMigratingLocalTo(t, "store")
}

// newProjectMigratingLocalToFailingDestination names a backend that reads
// like any other and refuses every write, which is a destination that fails
// halfway rather than one that fails to open.
func newProjectMigratingLocalToFailingDestination(t *testing.T) string {
	t.Helper()
	return newProjectMigratingLocalTo(t, "brokenstore")
}

func newProjectMigratingLocalTo(t *testing.T, plugin string) string {
	t.Helper()
	dir := t.TempDir()
	body := fmt.Sprintf(`project: myapp
resources:
  network:
    type: fake.network
    cidr: 10.20.0.0/16
backend:
  plugin: %s
  dir: %s
migrate_from:
  plugin: local
`, plugin, remoteDir(dir))
	if err := os.WriteFile(filepath.Join(dir, "infrena.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	installTestBackend(t, dir)
	return dir
}

// newProjectMigratingCountedSourceToFake is a project whose SOURCE is a
// backend that records every time it is opened.
//
// Both ends are plugins here, unlike every other fixture in this file, and
// that is the whole point: the guard's cheapness is a claim about the source
// never being STARTED, and local starts nothing to begin with, so a local
// source could not tell a guard that opens it from one that does not.
func newProjectMigratingCountedSourceToFake(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	body := fmt.Sprintf(`project: myapp
resources:
  network:
    type: fake.network
    cidr: 10.20.0.0/16
backend:
  plugin: store
  dir: %s
migrate_from:
  plugin: countingstore
  dir: %s
`, remoteDir(dir), sourceDir(dir))
	if err := os.WriteFile(filepath.Join(dir, "infrena.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	installTestBackend(t, dir)
	return dir
}

// remoteDir is where the fake remote backend keeps its state, read directly by
// the assertions so they observe the destination rather than asking infrena
// what it thinks is there.
func remoteDir(projectDir string) string { return filepath.Join(projectDir, "remote") }

// sourceDir is where the counted source backend keeps its state, and its
// tally of opens alongside.
func sourceDir(projectDir string) string { return filepath.Join(projectDir, "source") }

// sourceOpenCount is how many times the source backend has been opened since
// the count was last reset.
//
// The tally is a byte per open appended by the plugin itself, so it counts
// PROCESSES STARTED rather than calls made -- which is the cost the guard
// claims not to pay.
func sourceOpenCount(t *testing.T, dir string) int {
	t.Helper()
	info, err := os.Stat(filepath.Join(sourceDir(dir), openTallyName))
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return int(info.Size())
}

func resetSourceOpenCount(t *testing.T, dir string) {
	t.Helper()
	if err := os.Remove(filepath.Join(sourceDir(dir), openTallyName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

// openTallyName is the file the counting backend appends to. It shares the
// directory with the state files and must never look like one: List reads
// `*.json`, so an environment called "opens" is not in reach.
const openTallyName = "opens"

// installTestBackend puts the built backend on the project's plugin search
// path under both names the tests launch it by.
func installTestBackend(t *testing.T, dir string) {
	t.Helper()
	built := buildTestBackend(t)
	plugins := filepath.Join(dir, StateDirName, "plugins")
	if err := os.MkdirAll(plugins, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"store", "brokenstore", "countingstore"} {
		if err := os.Symlink(built, filepath.Join(plugins, backendhost.BinaryName(name))); err != nil {
			t.Fatal(err)
		}
	}
}

// buildTestBackend compiles testdata/backendplugin once per test binary and
// returns the path to it. `go build` is by a wide margin the slowest thing
// these tests do, and every case wants the same binary.
func buildTestBackend(t *testing.T) string {
	t.Helper()
	testBackendOnce.Do(buildTestBackendOnce)
	if testBackendErr != nil {
		t.Fatalf("building the test backend: %v", testBackendErr)
	}
	return testBackendPath
}

var (
	testBackendOnce sync.Once
	testBackendPath string
	testBackendErr  error
)

func buildTestBackendOnce() {
	if runtime.GOOS == "windows" {
		testBackendErr = errors.New("the test backend is installed under a second name by symlink, which this test does not do on Windows")
		return
	}
	dir, err := os.MkdirTemp("", "infrena-cli-backend-*")
	if err != nil {
		testBackendErr = err
		return
	}
	testBackendPath = filepath.Join(dir, "backendplugin")
	cmd := exec.Command("go", "build", "-o", testBackendPath, "./testdata/backendplugin")
	if out, err := cmd.CombinedOutput(); err != nil {
		testBackendErr = fmt.Errorf("%v\n%s", err, out)
	}
}

// seedLocalState puts one real resource in each environment's LOCAL state,
// created through the fake provider so that an ordinary `plan` over the
// migrated state is clean rather than full of drift.
func seedLocalState(t *testing.T, dir string, environments ...string) {
	t.Helper()
	for _, env := range environments {
		seedState(t, dir, env, stateWithOneRealResource(t, dir, env))
	}
}

// seedSourceState is seedLocalState for a project whose source backend is a
// plugin storing state in a directory, written to that directory DIRECTLY
// rather than through the plugin, for the reason destinationHasState reads it
// directly: a fixture that went through infrena would be asserting on
// infrena's own belief about what it had stored.
func seedSourceState(t *testing.T, dir string, environments ...string) {
	t.Helper()
	for _, env := range environments {
		st := stateWithOneRealResource(t, dir, env)
		st.Serial = 1
		data, err := st.Encode()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(sourceDir(dir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sourceDir(dir), env+".json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// stateWithOneRealResource creates a resource in the fake cloud and returns
// the state recording it, so a `plan` over state seeded anywhere is clean
// rather than full of drift.
func stateWithOneRealResource(t *testing.T, dir, environment string) *state.State {
	t.Helper()
	prov := testprovider.New(filepath.Join(dir, testprovider.DefaultCloudPath))
	rs, err := prov.Create(context.Background(), &resource.DesiredResource{
		Address: address.Address{Name: "network"},
		Type:    "fake.network",
		Attrs: map[string]value.Value{
			"cidr": value.String("10.20.0.0/16", value.SourceExplicit),
		},
	})
	if err != nil {
		t.Fatalf("seeding the fake cloud: %v", err)
	}
	st := state.New("myapp", environment)
	st.Set(rs)
	return st
}

// seedDestinationState writes state to the destination directly, standing in
// for somebody having applied against the new backend already.
func seedDestinationState(t *testing.T, dir, environment, marker string) {
	t.Helper()
	st := state.New("myapp", environment)
	st.Serial = 1
	st.Set(&resource.ResourceState{
		Address:    address.Address{Name: "network"},
		Type:       "fake.network",
		Provider:   "fake",
		ProviderID: marker,
		Attributes: map[string]value.Value{
			"cidr": value.String("10.30.0.0/16", value.SourceExplicit),
		},
	})
	data, err := st.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(remoteDir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remoteDir(dir), environment+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// destinationHasState reads the destination's storage directly, so it reports
// what is really there rather than what infrena believes.
func destinationHasState(t *testing.T, dir, environment string) bool {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(remoteDir(dir), environment+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	st, err := state.Decode(data)
	if err != nil {
		t.Fatalf("the destination holds state infrena cannot read: %v", err)
	}
	return len(st.Resources) > 0
}

func destinationStateMentions(t *testing.T, dir, environment, want string) bool {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(remoteDir(dir), environment+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Contains(string(data), want)
}

// sourceProviderID is the provider id the source recorded, so a test can tell
// the source's state from the one seeded at the destination.
func sourceProviderID(t *testing.T, dir, environment string) string {
	t.Helper()
	st, err := localBackend(dir).Get(context.Background(), environment)
	if err != nil {
		t.Fatal(err)
	}
	r, ok := st.Get(address.Address{Name: "network"})
	if !ok {
		t.Fatalf("the source holds no network in %q", environment)
	}
	return r.ProviderID
}

func sourceStillHasState(t *testing.T, dir, environment string) bool {
	t.Helper()
	st, err := localBackend(dir).Get(context.Background(), environment)
	if err != nil {
		t.Fatal(err)
	}
	return len(st.Resources) > 0
}

// lockHeld reports a lock left behind at EITHER end, because a migration takes
// two and either one left behind blocks the next run.
func lockHeld(t *testing.T, dir, environment string) bool {
	t.Helper()
	if _, held, err := localBackend(dir).Inspect(context.Background(), environment); err != nil {
		t.Fatal(err)
	} else if held {
		return true
	}
	_, err := os.Stat(filepath.Join(remoteDir(dir), environment+".lock"))
	return err == nil
}

// ---------------------------------------------------------------------------
// --check, for pipelines
// ---------------------------------------------------------------------------

// The exit code IS the report. A pipeline branches on it without parsing
// anything, which is the whole point of the flag.
func TestCheckReportsEachSituationByExitCode(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T) string
		want  int
	}{
		{"no migrate_from block", func(t *testing.T) string { return newProjectFixture(t) }, ExitOK},
		{"migration pending", func(t *testing.T) string {
			dir := newProjectMigratingLocalToFake(t)
			seedLocalState(t, dir, "dev")
			return dir
		}, ExitMigrationPending},
		{"already complete", func(t *testing.T) string {
			dir := newProjectMigratingLocalToFake(t)
			seedLocalState(t, dir, "dev")
			runCommand(t, dir, "state", "migrate")
			return dir
		}, ExitMigrationComplete},
		{"ends differ", func(t *testing.T) string {
			dir := newProjectMigratingLocalToFake(t)
			seedLocalState(t, dir, "dev")
			seedDestinationState(t, dir, "dev", "something else entirely")
			return dir
		}, ExitMigrationConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := tc.setup(t)
			_, _, code := runCommand(t, dir, "state", "migrate", "--check")
			if code != tc.want {
				t.Errorf("exit = %d, want %d", code, tc.want)
			}
		})
	}
}

// --check must be genuinely read-only. A check that migrated would be the
// worst possible surprise in a pipeline: the thing you ran to find out
// whether to act would have acted.
func TestCheckWritesNothingAndTakesNoLock(t *testing.T) {
	dir := newProjectMigratingLocalToFake(t)
	seedLocalState(t, dir, "dev")

	if _, _, code := runCommand(t, dir, "state", "migrate", "--check"); code != ExitMigrationPending {
		t.Fatalf("exit = %d", code)
	}

	if destinationHasState(t, dir, "dev") {
		t.Error("--check wrote to the destination")
	}
	// A lock left behind would block the migration the check just recommended.
	if lockHeld(t, dir, "dev") {
		t.Error("--check left a lock behind")
	}
}

// The status crosses as a STRING, so a consumer never maps an exit code back
// to a meaning.
func TestCheckWritesTheStatusAsAStringInTheReport(t *testing.T) {
	dir := newProjectMigratingLocalToFake(t)
	seedLocalState(t, dir, "dev")
	out := filepath.Join(t.TempDir(), "check.ndjson")

	stdout, _, _ := runCommand(t, dir, "state", "migrate", "--check", "--output", out)

	if stdout != "" {
		t.Errorf("stdout = %q, want nothing", stdout)
	}
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"status":"pending"`) {
		t.Errorf("report does not carry the status as a string:\n%s", body)
	}
	if !strings.Contains(string(body), "dev") {
		t.Errorf("report does not name the environments:\n%s", body)
	}
}

// --check reads the SAME comparison the migration acts on. Two implementations
// of "are these two ends the same" is how a check comes to report one thing and
// the migration then does another.
func TestCheckAgreesWithWhatMigrateThenDoes(t *testing.T) {
	dir := newProjectMigratingLocalToFake(t)
	seedLocalState(t, dir, "dev", "production")

	if _, _, code := runCommand(t, dir, "state", "migrate", "--check"); code != ExitMigrationPending {
		t.Fatalf("check before migrating = %d, want pending", code)
	}
	if _, stderr, code := runCommand(t, dir, "state", "migrate"); code != ExitOK {
		t.Fatalf("the migration the check recommended failed: exit %d\n%s", code, stderr)
	}
	if _, _, code := runCommand(t, dir, "state", "migrate", "--check"); code != ExitMigrationComplete {
		t.Errorf("check after migrating = %d, want complete", code)
	}
}

// A check that cannot reach a backend is an ERROR, not one of the three
// situations: a pipeline that read 0 from an unreachable backend would take
// "nothing to do" from a question that was never answered.
func TestCheckReportsAnUnreadableBackendAsAnError(t *testing.T) {
	dir := newProjectMigratingLocalTo(t, "nosuchbackend")
	seedLocalState(t, dir, "dev")

	_, stderr, code := runCommand(t, dir, "state", "migrate", "--check")

	if code != ExitError {
		t.Errorf("exit = %d, want %d", code, ExitError)
	}
	if !strings.Contains(stderr, "nosuchbackend") {
		t.Errorf("the error does not name the backend:\n%s", stderr)
	}
}
