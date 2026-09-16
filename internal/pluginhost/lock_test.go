package pluginhost

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/infrena/infrena/internal/plugins"
)

// PLAN.md section 31.3: "The host verifies on every launch once a lock exists."
// A binary replaced on disk after it was installed is caught at the next
// command rather than never.
//
// THE SENTINEL IS THE EVIDENCE, and it is why these tests plant a shell script
// rather than a plugin. None of these binaries can complete the handshake, so
// every Load here fails; what separates the cases is WHETHER THE BINARY RAN. A
// refusal that leaves no sentinel happened before anything was executed, which
// is the only thing a check like this is worth anything for. A test that only
// asserted "Load returned an error" would pass just as happily against a loader
// that ran the binary first and checked afterwards.

// plantBinary writes an executable into a project's plugin directory and
// returns the path it can be caught at, plus the file it touches when it runs.
func plantBinary(t *testing.T, dir, name string) (path, sentinel string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the planted binary is a shell script")
	}

	pluginDir := filepath.Join(dir, ".infra", "plugins")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel = filepath.Join(dir, "ran-"+name)
	path = filepath.Join(pluginDir, BinaryName(name))

	script := "#!/bin/sh\n: > '" + sentinel + "'\nexit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, sentinel
}

// writeLock records one plugin at one platform, which is all any of these
// cases needs.
func writeLock(t *testing.T, dir, name, platform, sum string) {
	t.Helper()
	l := &plugins.Lockfile{Version: plugins.LockVersion, Plugins: map[string]plugins.LockEntry{
		name: {
			Version:   "1.0.0",
			Source:    "github.com/infrena/infrena-provider-" + name,
			Checksums: map[string]string{platform: sum},
		},
	}}
	if err := l.Write(dir); err != nil {
		t.Fatal(err)
	}
}

func ranAt(t *testing.T, sentinel string) bool {
	t.Helper()
	_, err := os.Stat(sentinel)
	return err == nil
}

func TestALockedPluginWhoseBinaryChangedIsRefusedAtLaunch(t *testing.T) {
	dir := t.TempDir()
	_, sentinel := plantBinary(t, dir, "demo")
	writeLock(t, dir, "demo", plugins.PlatformKey(), "sha256:0000000000000000000000000000000000000000000000000000000000000000")

	l := &Loader{Search: SearchOptions{ProjectDir: dir}, Dir: dir}
	t.Cleanup(l.Close)

	_, err := l.Load(context.Background(), "demo")
	if err == nil {
		t.Fatal("a binary that does not match the lock was loaded")
	}
	for _, want := range []string{"demo", plugins.LockfileName} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
	if ranAt(t, sentinel) {
		t.Error("the binary was executed before it was checked against the lock")
	}
}

// A plugin the lock records for another machine only is refused too: something
// installed this and pinned it elsewhere, so "no entry for yours" is a gap to
// see rather than permission to run whatever is there.
func TestALockedPluginWithNoChecksumForThisPlatformIsRefusedAtLaunch(t *testing.T) {
	dir := t.TempDir()
	path, sentinel := plantBinary(t, dir, "demo")
	sum, err := plugins.FileChecksum(path)
	if err != nil {
		t.Fatal(err)
	}
	writeLock(t, dir, "demo", "plan9/mips", sum)

	l := &Loader{Search: SearchOptions{ProjectDir: dir}, Dir: dir}
	t.Cleanup(l.Close)

	if _, err := l.Load(context.Background(), "demo"); err == nil {
		t.Fatal("a platform the lock does not record was accepted")
	}
	if ranAt(t, sentinel) {
		t.Error("the binary was executed before it was checked against the lock")
	}
}

// A hand-placed binary the lock does not mention keeps working (section 31.3):
// the lock governs what INSTALL put on disk rather than claiming authority over
// everything.
func TestAnUnlockedPluginStillLoads(t *testing.T) {
	dir := t.TempDir()
	_, sentinel := plantBinary(t, dir, "handplaced")
	writeLock(t, dir, "demo", plugins.PlatformKey(), "sha256:aaaa")

	l := &Loader{Search: SearchOptions{ProjectDir: dir}, Dir: dir}
	t.Cleanup(l.Close)

	// It fails, because a shell script cannot speak the protocol. What matters
	// is that it got as far as being run.
	_, err := l.Load(context.Background(), "handplaced")
	if err != nil && strings.Contains(err.Error(), plugins.LockfileName) {
		t.Fatalf("a plugin the lock does not mention was refused by the lock: %v", err)
	}
	if !ranAt(t, sentinel) {
		t.Error("a plugin the lock does not mention was never launched")
	}
}

// And the ordinary case: a binary that is what the lock says it is runs.
func TestALockedPluginThatMatchesItsChecksumIsLaunched(t *testing.T) {
	dir := t.TempDir()
	path, sentinel := plantBinary(t, dir, "demo")
	sum, err := plugins.FileChecksum(path)
	if err != nil {
		t.Fatal(err)
	}
	writeLock(t, dir, "demo", plugins.PlatformKey(), sum)

	l := &Loader{Search: SearchOptions{ProjectDir: dir}, Dir: dir}
	t.Cleanup(l.Close)

	if _, err := l.Load(context.Background(), "demo"); err != nil && strings.Contains(err.Error(), plugins.LockfileName) {
		t.Fatalf("a binary matching the lock was refused: %v", err)
	}
	if !ranAt(t, sentinel) {
		t.Error("a binary matching the lock was never launched")
	}
}

// A lock nothing can read is a REFUSAL, not a pass. The file is what says which
// executables are trustworthy, and a build that cannot read it cannot answer
// the question it was asked.
func TestAnUnreadableLockRefusesToLaunchAnything(t *testing.T) {
	dir := t.TempDir()
	_, sentinel := plantBinary(t, dir, "demo")
	if err := os.WriteFile(filepath.Join(dir, plugins.LockfileName), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	l := &Loader{Search: SearchOptions{ProjectDir: dir}, Dir: dir}
	t.Cleanup(l.Close)

	if _, err := l.Load(context.Background(), "demo"); err == nil {
		t.Fatal("a plugin was loaded with an unreadable lock")
	}
	if ranAt(t, sentinel) {
		t.Error("the binary was executed despite the lock being unreadable")
	}
}
