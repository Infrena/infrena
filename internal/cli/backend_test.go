package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/infrena/infrena/internal/config"
	"github.com/infrena/infrena/internal/state"
)

// No backend block: local, exactly as today, with nothing installed and no
// process started. This is the bootstrap and it must never need a plugin.
func TestAProjectWithNoBackendBlockUsesLocalAndStartsNothing(t *testing.T) {
	dir := newProjectFixture(t)
	blocked := blockNetwork(t)

	b, closeFn, err := backendFor(context.Background(), &GlobalOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()

	if _, ok := b.(*state.Local); !ok {
		t.Errorf("backendFor returned %T, want *state.Local", b)
	}
	if blocked.Attempts() != 0 {
		t.Error("the local backend reached the network")
	}
}

// A backend named but not installed is an error naming the backend and how to
// get it, not a silent fallback to local. Falling back would write state
// somewhere the user did not ask for, which is the worst available outcome.
func TestAMissingBackendPluginIsAnErrorAndNeverFallsBackToLocal(t *testing.T) {
	dir := newProjectWithBackendBlock(t, "backend:\n  plugin: s3\n  bucket: acme-state\n")

	_, _, err := backendFor(context.Background(), &GlobalOptions{Dir: dir})
	if err == nil {
		t.Fatal("a missing backend silently fell back")
	}
	for _, want := range []string{"s3", "plugins install"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

// A `backend:` block that did not decode is not a project with no backend
// block. The two are a keystroke apart in a file and a continent apart in
// consequence: one means local, the other means state belongs somewhere else
// and infrena could not work out where.
func TestABackendBlockThatDidNotDecodeDoesNotFallBackToLocal(t *testing.T) {
	dir := newProjectWithBackendBlock(t, "backend:\n  bucket: acme-state\n")

	_, _, err := backendFor(context.Background(), &GlobalOptions{Dir: dir})
	if err == nil {
		t.Fatal("a backend block that could not be read fell back to local")
	}
	if !strings.Contains(err.Error(), "backend") {
		t.Errorf("error does not name the block that is wrong: %v", err)
	}
}

// The closer is safe to call for local, because every call site defers it
// without asking which backend it got. A nil closer would make eight commands
// panic on the ordinary path.
func TestTheCloserIsAlwaysSafeToCall(t *testing.T) {
	dir := newProjectFixture(t)

	_, closeFn, err := backendFor(context.Background(), &GlobalOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if closeFn == nil {
		t.Fatal("backendFor returned no closer")
	}
	if err := closeFn(); err != nil {
		t.Errorf("closing a local backend failed: %v", err)
	}
}

// newProjectWithBackendBlock writes the one-resource fixture with a `backend:`
// block appended.
//
// The parameter is the BLOCK, YAML and all, which the old name did not say:
// plugins_install_test.go's newProjectWithBackend takes a backend's NAME, and
// two helpers a keystroke apart taking different things is how a caller passes
// "s3" to the one that wanted four lines of YAML.
func newProjectWithBackendBlock(t *testing.T, block string) string {
	t.Helper()
	return projectDir(t, `
project: myapp
resources:
  network:
    type: fake.network
    cidr: 10.20.0.0/16
`+block)
}

// `plugin: local` is how a migration names the built-in backend. Without it,
// "migrate to local" could only be expressed by ABSENCE, and absence already
// means "no migration".
func TestOpenBackendResolvesLocalByName(t *testing.T) {
	dir := newProjectFixture(t)

	b, closeFn, err := openBackend(context.Background(), &GlobalOptions{Dir: dir},
		config.BackendDecl{Plugin: "local"})
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()

	if _, ok := b.(*state.Local); !ok {
		t.Errorf("openBackend returned %T, want *state.Local", b)
	}
}

// Two ends open independently, so a migration holds both at once.
func TestBothEndsCanBeOpenAtTheSameTime(t *testing.T) {
	dir := newProjectFixture(t)
	opts := &GlobalOptions{Dir: dir}

	a, closeA, err := openBackend(context.Background(), opts, config.BackendDecl{Plugin: "local"})
	if err != nil {
		t.Fatal(err)
	}
	defer closeA()
	b, closeB, err := openBackend(context.Background(), opts, config.BackendDecl{Plugin: "local"})
	if err != nil {
		t.Fatal(err)
	}
	defer closeB()

	if a == nil || b == nil {
		t.Fatal("one end failed to open")
	}
}

// A backend named but not installed is an error naming it and how to get it,
// exactly as backendFor already refuses. Never a silent fall back to local:
// migrating INTO local when the user asked for a bucket would put state
// somewhere they did not ask for, which is the worst available outcome.
func TestAMissingBackendPluginInAMigrationIsAnError(t *testing.T) {
	dir := newProjectFixture(t)

	_, _, err := openBackend(context.Background(), &GlobalOptions{Dir: dir},
		config.BackendDecl{Plugin: "nosuch"})
	if err == nil {
		t.Fatal("a missing backend opened silently")
	}
	if !strings.Contains(err.Error(), "nosuch") {
		t.Errorf("error does not name the backend: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The guard: a backend that is waiting for a migration
// ---------------------------------------------------------------------------

// THE HAZARD. Configuration lands with a new empty backend and a
// migrate_from: naming the old one that holds everything. Nobody has run the
// migration yet. Reading only `backend:`, every resource looks unmanaged and
// apply would CREATE ALL OF IT AGAIN.
//
// In the CI flow this design exists for, that ordering is the LIKELY one:
// config lands first, the pipeline runs before a human triggers anything.
func TestCommandsRefuseWhileAMigrationIsPending(t *testing.T) {
	dir := newProjectMigratingLocalToFake(t)
	seedLocalState(t, dir, "dev") // the SOURCE holds state; the destination is empty

	for _, args := range [][]string{{"plan", "dev"}, {"apply", "dev", "--auto-approve"}, {"refresh", "dev"}} {
		_, stderr, code := runCommand(t, dir, args...)

		if code == ExitOK || code == ExitChanges {
			t.Errorf("%v proceeded into a backend awaiting migration", args)
		}
		if !strings.Contains(stderr, "state migrate") {
			t.Errorf("%v: refusal does not name the fix:\n%s", args, stderr)
		}
	}
}

// The refusal names the environments at risk, because the reader's next
// question is what is in the balance and a message that made them go and find
// out would be answered by running the very command being refused.
func TestTheRefusalNamesTheEnvironmentsAtRisk(t *testing.T) {
	dir := newProjectMigratingLocalToFake(t)
	seedLocalState(t, dir, "dev", "production")

	_, stderr, _ := runCommand(t, dir, "plan", "dev")

	for _, want := range []string{"dev", "production"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("refusal does not name %q:\n%s", want, stderr)
		}
	}
}

// A COMMAND NEVER MIGRATES. The guard stops a run; it does not quietly do the
// thing it is guarding against being skipped. A plan that migrated would move
// state as a side effect of a read-only command, which is a worse surprise
// than the one this guard exists to prevent.
func TestTheGuardRefusesRatherThanMigrating(t *testing.T) {
	dir := newProjectMigratingLocalToFake(t)
	seedLocalState(t, dir, "dev")

	runCommand(t, dir, "plan", "dev")

	if destinationHasState(t, dir, "dev") {
		t.Error("an ordinary command performed the migration")
	}
}

// Once migrated, the block is harmless and everything proceeds. This is the
// property the whole inertness rule exists to protect: a SUCCESSFUL migration
// must not break the pipeline while the removal commit is pending.
func TestCommandsProceedOnceTheMigrationIsDone(t *testing.T) {
	dir := newProjectMigratingLocalToFake(t)
	seedLocalState(t, dir, "dev")
	if _, _, code := runCommand(t, dir, "state", "migrate"); code != ExitOK {
		t.Fatal("setup migration failed")
	}

	// migrate_from: is STILL present and must now be inert.
	if _, stderr, code := runCommand(t, dir, "plan", "dev"); code == ExitError {
		t.Errorf("plan refused after a completed migration:\n%s", stderr)
	}
}

// A new project that has both blocks but nothing anywhere is not a pending
// migration. Refusing here would block a legitimate first apply.
func TestAnEmptySourceIsNotAPendingMigration(t *testing.T) {
	dir := newProjectMigratingLocalToFake(t) // nothing seeded anywhere

	if _, stderr, code := runCommand(t, dir, "plan", "dev"); code == ExitError {
		t.Errorf("plan refused with nothing to migrate:\n%s", stderr)
	}
}

// A project with no `migrate_from:` block is every project, and the guard must
// be invisible to it -- including on a machine where nothing is installed and
// no process may be started.
func TestTheGuardIsInvisibleWithoutAMigrateFromBlock(t *testing.T) {
	dir := newProjectFixture(t)
	blocked := blockNetwork(t)

	if _, stderr, code := runCommand(t, dir, "plan", "dev"); code == ExitError {
		t.Errorf("plan refused with no migrate_from block at all:\n%s", stderr)
	}
	if blocked.Attempts() != 0 {
		t.Error("the guard reached the network for a project with no migrate_from block")
	}
}

// The guard must not cost anything in the ordinary case. With state at the
// destination the source is never opened, which matters when opening it means
// starting a plugin process and talking to a network.
func TestTheSourceIsNotOpenedWhenTheDestinationHasState(t *testing.T) {
	dir := newProjectMigratingCountedSourceToFake(t) // counts opens of the SOURCE
	seedSourceState(t, dir, "dev")
	if _, stderr, code := runCommand(t, dir, "state", "migrate"); code != ExitOK {
		t.Fatalf("setup migration failed: exit %d\n%s", code, stderr)
	}

	resetSourceOpenCount(t, dir)
	runCommand(t, dir, "plan", "dev")

	if n := sourceOpenCount(t, dir); n != 0 {
		t.Errorf("the source backend was opened %d times when the destination already had state", n)
	}
}

// The other half of the same fixture, so the count above is known to be able
// to move. A guard that never opened the source at all would pass the test
// above while failing to protect anything.
func TestTheSourceIsOpenedWhenTheDestinationIsEmpty(t *testing.T) {
	dir := newProjectMigratingCountedSourceToFake(t)
	seedSourceState(t, dir, "dev")

	resetSourceOpenCount(t, dir)
	_, stderr, code := runCommand(t, dir, "plan", "dev")

	if code != ExitError {
		t.Errorf("plan proceeded into an empty backend while the source held state:\n%s", stderr)
	}
	if n := sourceOpenCount(t, dir); n == 0 {
		t.Error("the source backend was never opened, so nothing checked whether a migration was pending")
	}
}
