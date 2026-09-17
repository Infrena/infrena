package cli

import (
	"context"
	"strings"
	"testing"

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
	dir := newProjectWithBackend(t, "backend:\n  plugin: s3\n  bucket: acme-state\n")

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
	dir := newProjectWithBackend(t, "backend:\n  bucket: acme-state\n")

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

// newProjectWithBackend writes the one-resource fixture with a `backend:`
// block appended.
func newProjectWithBackend(t *testing.T, block string) string {
	t.Helper()
	return projectDir(t, `
project: myapp
resources:
  network:
    type: fake.network
    cidr: 10.20.0.0/16
`+block)
}
