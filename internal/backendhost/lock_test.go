package backendhost

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/infrena/infrena/internal/plugins"
)

// The host verifies against the lock on every launch once a lock exists. These
// are internal/pluginhost's lock cases for a backend binary, deliberately the
// same shape, because the two are the same rule. A backend reads and writes the
// whole of your state, every secret in it included, so an unverified backend
// binary is if anything worse than an unverified provider.
//
// The sentinel is the evidence, and it is why these tests plant a shell script
// rather than a backend. None of these binaries can complete the handshake, so
// every Open here fails; what separates the cases is whether the binary ran. A
// refusal that leaves no sentinel happened before anything was executed, which
// is the only property worth checking here. A test that only asserted "Open
// returned an error" would pass just as happily against a host that ran the
// binary first and checked afterwards.

// plantBackend writes an executable where a backend is looked for and returns
// the directory to search, plus the file it touches when it runs.
func plantBackend(t *testing.T, name string) (dir, sentinel string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the planted binary is a shell script")
	}

	dir = t.TempDir()
	sentinel = filepath.Join(dir, "ran-"+name)
	script := "#!/bin/sh\n: > '" + sentinel + "'\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, BinaryName(name)), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, sentinel
}

// writeBackendLock records one backend at one platform, which is all any of
// these cases needs.
func writeBackendLock(t *testing.T, projectDir, name, platform, sum string) {
	t.Helper()
	l := &plugins.Lockfile{Version: plugins.LockVersion, Plugins: map[string]plugins.LockEntry{
		LockKey(name): {
			Version:   "1.0.0",
			Source:    "github.com/infrena/infrena-backend-" + name,
			Checksums: map[string]string{platform: sum},
		},
	}}
	if err := l.Write(projectDir); err != nil {
		t.Fatal(err)
	}
}

func backendRan(t *testing.T, sentinel string) bool {
	t.Helper()
	_, err := os.Stat(sentinel)
	return err == nil
}

// A backend whose binary is not the one plugins.lock recorded is refused before
// it is launched, and the refusal names the backend.
func TestALockedBackendWhoseBinaryChangedIsRefusedBeforeItRuns(t *testing.T) {
	dir, sentinel := plantBackend(t, "s3")
	project := t.TempDir()
	writeBackendLock(t, project, "s3", plugins.PlatformKey(),
		"sha256:0000000000000000000000000000000000000000000000000000000000000000")

	_, _, err := Open(context.Background(), "s3", project, []string{dir}, nil)
	if err == nil {
		t.Fatal("a backend binary that does not match the lock was opened")
	}
	for _, want := range []string{"s3", plugins.LockfileName} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
	if backendRan(t, sentinel) {
		t.Error("the backend was executed before it was checked against the lock")
	}
}

// A backend the lock records for another machine only is refused too:
// something installed this and pinned it elsewhere, so "no entry for yours" is
// a gap to see rather than permission to run whatever is there.
func TestALockedBackendWithNoChecksumForThisPlatformIsRefused(t *testing.T) {
	dir, sentinel := plantBackend(t, "s3")
	project := t.TempDir()
	sum, err := plugins.FileChecksum(filepath.Join(dir, BinaryName("s3")))
	if err != nil {
		t.Fatal(err)
	}
	writeBackendLock(t, project, "s3", "plan9/mips", sum)

	if _, _, err := Open(context.Background(), "s3", project, []string{dir}, nil); err == nil {
		t.Fatal("a platform the lock does not record was accepted")
	}
	if backendRan(t, sentinel) {
		t.Error("the backend was executed before it was checked against the lock")
	}
}

// A hand-placed backend the lock does not mention keeps working: the lock
// governs what install put on disk rather than claiming authority over
// everything.
func TestAnUnlockedBackendStillLoads(t *testing.T) {
	dir, sentinel := plantBackend(t, "handplaced")
	project := t.TempDir()
	writeBackendLock(t, project, "s3", plugins.PlatformKey(), "sha256:aaaa")

	// It fails, because a shell script cannot speak the protocol. What matters
	// is that it got as far as being run.
	_, _, err := Open(context.Background(), "handplaced", project, []string{dir}, nil)
	if err != nil && strings.Contains(err.Error(), plugins.LockfileName) {
		t.Fatalf("a backend the lock does not mention was refused by the lock: %v", err)
	}
	if !backendRan(t, sentinel) {
		t.Error("a backend the lock does not mention was never launched")
	}
}

// And the ordinary case: a binary that is what the lock says it is runs.
func TestALockedBackendThatMatchesItsChecksumIsLaunched(t *testing.T) {
	dir, sentinel := plantBackend(t, "s3")
	project := t.TempDir()
	sum, err := plugins.FileChecksum(filepath.Join(dir, BinaryName("s3")))
	if err != nil {
		t.Fatal(err)
	}
	writeBackendLock(t, project, "s3", plugins.PlatformKey(), sum)

	_, _, err = Open(context.Background(), "s3", project, []string{dir}, nil)
	if err != nil && strings.Contains(err.Error(), plugins.LockfileName) {
		t.Fatalf("a binary matching the lock was refused: %v", err)
	}
	if !backendRan(t, sentinel) {
		t.Error("a binary matching the lock was never launched")
	}
}

// A lock nothing can read is a refusal, not a pass. The file is what says which
// executables are trustworthy, and a build that cannot read it cannot answer
// the question it was asked.
func TestAnUnreadableLockRefusesToOpenABackend(t *testing.T) {
	dir, sentinel := plantBackend(t, "s3")
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, plugins.LockfileName), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, _, err := Open(context.Background(), "s3", project, []string{dir}, nil); err == nil {
		t.Fatal("a backend was opened with an unreadable lock")
	}
	if backendRan(t, sentinel) {
		t.Error("the backend was executed despite the lock being unreadable")
	}
}

// A backend and a provider of the same name are different artifacts, built from
// separate repositories, and plugins.lock is one file, so they cannot share a
// key. A provider's entry must not be what a backend is
// checked against: the two binaries have different checksums by construction,
// so sharing a key would refuse a correctly installed pair on every command.
func TestABackendIsNotCheckedAgainstAProviderOfTheSameName(t *testing.T) {
	dir, sentinel := plantBackend(t, "s3")
	project := t.TempDir()

	l := &plugins.Lockfile{Version: plugins.LockVersion, Plugins: map[string]plugins.LockEntry{
		"s3": {
			Version:   "1.0.0",
			Source:    "github.com/infrena/infrena-provider-s3",
			Checksums: map[string]string{plugins.PlatformKey(): "sha256:not-the-backend"},
		},
	}}
	if err := l.Write(project); err != nil {
		t.Fatal(err)
	}

	_, _, err := Open(context.Background(), "s3", project, []string{dir}, nil)
	if err != nil && strings.Contains(err.Error(), plugins.LockfileName) {
		t.Fatalf("the backend was checked against the provider's entry: %v", err)
	}
	if !backendRan(t, sentinel) {
		t.Error("the backend was never launched")
	}
}
