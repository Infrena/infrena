package backendhost

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A backend whose stdout or stderr the host has stopped reading blocks on its
// next write and never exits, and the host waits for it to exit rather than
// kill it mid-Put. The provider host hung this way on a discover reply over the
// line limit; a backend is the same shape with a state in place of the reply.
// Both fail by timing out, because the failure being tested is a hang.

func TestAReplyOverTheLimitFailsTheCallAndStillLetsTheBackendExit(t *testing.T) {
	old := maxMessage
	maxMessage = 1024
	defer func() { maxMessage = old }()

	dir := buildFakeBackend(t)
	b, closeFn, err := Open(context.Background(), "huge-get", "", []string{dir}, nil)
	if err != nil {
		t.Fatal(err)
	}

	within(t, "the read", func() {
		if _, err := b.Get(context.Background(), "dev"); err == nil {
			t.Error("a reply over the limit was accepted")
		} else if !strings.Contains(err.Error(), "longer than the protocol allows") {
			t.Errorf("the error does not say the reply was too long: %v", err)
		}
	})
	within(t, "closing the backend", func() { _ = closeFn() })
}

func TestAnOverlongLogLineDoesNotBlockTheBackend(t *testing.T) {
	dir := buildFakeBackend(t)
	b, closeFn, err := Open(context.Background(), "noisy-get", "", []string{dir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	within(t, "the read", func() {
		if _, err := b.Get(context.Background(), "dev"); err != nil {
			t.Errorf("a long log line failed the read: %v", err)
		}
	})
	within(t, "closing the backend", func() { _ = closeFn() })
}

func within(t *testing.T, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); f() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s never finished: the backend is blocked on a pipe the host stopped reading", what)
	}
}
