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
