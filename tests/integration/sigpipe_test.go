package integration

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// A reader that goes away must not change what a run reports.
//
// Go deliberately lets SIGPIPE kill a process that writes to a broken fd 1 or
// 2, which is the right default for a cat-like filter and the wrong one here:
// infrena's exit codes are a product API (PLAN.md §37.1) that pipelines branch
// on, and 141 is not one of them. `infrena plan dev | head -1` has to still
// say 2 when there are changes. `apply` matters more again, because a signal
// kill walks straight past the durability the executor goes out of its way to
// keep — state written under context.WithoutCancel, the environment lock
// released on the way out.
//
// These tests run the BUILT BINARY with fd 1 on a real pipe. A unit test over
// a fake writer that returns an error cannot prove any of this: the bug is
// that the process is killed by a signal before the error is ever returned.

// wideProject declares n resources, so a run has plenty left to print after
// the reader has gone away, and plenty left to do after the lock is taken.
func wideProject(n int) string {
	var b strings.Builder
	b.WriteString("project: myapp\nresources:\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "  net%03d:\n    type: fake.network\n    cidr: 10.%d.%d.0/24\n", i, i/256, i%256)
	}
	return b.String()
}

// runWithStdoutClosed runs the CLI with stdout on a pipe whose read end is
// closed early, and returns the exit code and stderr.
//
// readUntil chooses which of the two real shapes this is:
//
//   - "" closes the read end BEFORE the process starts, so the very first
//     write to stdout finds a broken pipe. There is no race to lose, which is
//     what makes this the assertion about exit codes. It is what
//     `infrena plan dev | grep -q something` looks like once grep has its
//     answer, or a pager the operator quit.
//   - a marker reads until that text has been printed and closes then, which
//     is `| head -1` on a run whose output outlives the first line: the
//     process is mid-flight, holding the environment lock, when the reader
//     disappears.
func runWithStdoutClosed(t *testing.T, dir, readUntil string, args ...string) (int, string) {
	t.Helper()
	full := append([]string{"--chdir", dir, "--plugin-dir", fakePluginDir(t)}, args...)
	cmd := exec.Command(binary(t), full...)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	cmd.Stdout = w
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if readUntil == "" {
		r.Close()
	}
	if err := cmd.Start(); err != nil {
		w.Close()
		r.Close()
		t.Fatalf("starting infrena %v: %v", args, err)
	}
	// The child holds the only write end from here, so an EOF the reader sees
	// is the child's and not ours.
	w.Close()

	if readUntil != "" {
		readThrough(t, r, readUntil)
		r.Close()
	}

	code := 0
	if err := cmd.Wait(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("waiting for infrena %v: %v", args, err)
		}
		// 128+signal, the way a shell reports a signalled child and the way
		// the pipeline that hit this bug saw it. os/exec reports -1 for a
		// signal death, which would make the failure read as "exited -1" —
		// true, and useless for recognising SIGPIPE.
		code = exitErr.ExitCode()
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			code = 128 + int(ws.Signal())
		}
	}
	return code, stderr.String()
}

// readThrough reads from r in small bites until marker has been seen, leaving
// the rest of the run's output unread on purpose.
func readThrough(t *testing.T, r *os.File, marker string) {
	t.Helper()
	var seen bytes.Buffer
	buf := make([]byte, 128)
	for !strings.Contains(seen.String(), marker) {
		n, err := r.Read(buf)
		seen.Write(buf[:n])
		if err != nil {
			r.Close()
			t.Fatalf("the run ended without ever printing %q:\n%s", marker, seen.String())
		}
	}
}

// requireNotSignalled fails with the diagnosis rather than a bare number,
// because 141 read as "exit code 141" tells the next reader nothing.
func requireNotSignalled(t *testing.T, code, want int, what, stderr string) {
	t.Helper()
	if code == 141 {
		t.Fatalf("%s was killed by SIGPIPE (exit 141) when its reader went away; want %d.\nstderr:\n%s",
			what, want, stderr)
	}
	if code != want {
		t.Fatalf("%s exited %d, want %d.\nstderr:\n%s", what, code, want, stderr)
	}
}

// requireQuietAboutThePipe: the reader closing the pipe is its own business,
// not a failure infrena reports. Only a write to an --output FILE is a real
// error, and this run has no --output.
func requireQuietAboutThePipe(t *testing.T, stderr string) {
	t.Helper()
	for _, unwanted := range []string{"broken pipe", "Error:", "EPIPE"} {
		if strings.Contains(stderr, unwanted) {
			t.Errorf("a closed stdout was reported as an error (%q):\n%s", unwanted, stderr)
		}
	}
}

func TestPlanKeepsItsExitCodeWhenStdoutIsClosed(t *testing.T) {
	dir := project(t, wideProject(40))

	code, stderr := runWithStdoutClosed(t, dir, "", "plan", "dev")
	requireNotSignalled(t, code, 2, "plan with a closed stdout", stderr)
	requireQuietAboutThePipe(t, stderr)
}

func TestApplyKeepsItsExitCodeWhenStdoutIsClosed(t *testing.T) {
	dir := project(t, wideProject(40))

	code, stderr := runWithStdoutClosed(t, dir, "", "apply", "dev", "--auto-approve")
	requireNotSignalled(t, code, 2, "apply with a closed stdout", stderr)
	requireQuietAboutThePipe(t, stderr)
	requireAppliedCleanly(t, dir, "dev")
}

// TestApplyFinishesWhenTheReaderLeavesMidRun is the case that actually costs
// something. The reader leaves after the first operation has completed, so
// the process is inside withLockedEnvironment with work still in flight —
// exactly where a signal kill skips the state write and strands the lock.
func TestApplyFinishesWhenTheReaderLeavesMidRun(t *testing.T) {
	dir := project(t, wideProject(40))

	code, stderr := runWithStdoutClosed(t, dir, "done (", "apply", "dev", "--auto-approve")
	requireNotSignalled(t, code, 2, "apply whose reader left mid-run", stderr)
	requireQuietAboutThePipe(t, stderr)
	requireAppliedCleanly(t, dir, "dev")

	// And the environment is usable again: a second run is not blocked by a
	// lock the first one never released.
	if again := run(t, dir, "plan", "dev"); again.ExitCode != 0 {
		t.Fatalf("re-plan after a pipe-interrupted apply exited %d, want 0 (converged):\n%s",
			again.ExitCode, again.combined())
	}
}

// requireAppliedCleanly checks the two things a signal kill destroys: the
// state write, and the lock release.
func requireAppliedCleanly(t *testing.T, dir, environment string) {
	t.Helper()
	stateDir := filepath.Join(dir, ".infra", "state")

	body, err := os.ReadFile(filepath.Join(stateDir, environment+".json"))
	if err != nil {
		t.Fatalf("state for %s was not written: %v", environment, err)
	}
	if !bytes.Contains(body, []byte("fake.network")) {
		t.Errorf("state for %s records no applied resources:\n%s", environment, body)
	}

	lock := filepath.Join(stateDir, environment+".lock")
	if _, err := os.Stat(lock); err == nil {
		held, _ := os.ReadFile(lock)
		t.Errorf("the environment lock was left behind at %s:\n%s", lock, held)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat %s: %v", lock, err)
	}
}
