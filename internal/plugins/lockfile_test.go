package plugins

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLockfileRoundTrips(t *testing.T) {
	dir := t.TempDir()
	l := &Lockfile{Version: LockVersion, Plugins: map[string]LockEntry{
		"aws": {
			Version: "0.4.0",
			Source:  "github.com/infrena/infrena-provider-aws",
			Checksums: map[string]string{
				"linux/amd64":  "sha256:aaaa",
				"darwin/arm64": "sha256:bbbb",
			},
		},
	}}
	if err := l.Write(dir); err != nil {
		t.Fatal(err)
	}

	got, err := ReadLockfile(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, l) {
		t.Errorf("round trip changed the lockfile:\n got %+v\nwant %+v", got, l)
	}
}

// The lockfile is committed and read in a diff, so a rewrite that reorders keys
// is a diff nobody can review. Go randomises map order, so this is a real risk.
func TestWritingTwiceProducesIdenticalBytes(t *testing.T) {
	dir := t.TempDir()
	l := &Lockfile{Version: LockVersion, Plugins: map[string]LockEntry{
		"zeta":  {Version: "1.0.0", Source: "github.com/a/infrena-provider-zeta", Checksums: map[string]string{"linux/amd64": "sha256:1", "darwin/arm64": "sha256:2"}},
		"alpha": {Version: "1.0.0", Source: "github.com/a/infrena-provider-alpha", Checksums: map[string]string{"linux/arm64": "sha256:3", "linux/amd64": "sha256:4"}},
	}}
	for i := 0; i < 20; i++ {
		if err := l.Write(dir); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(dir, LockfileName))
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			t.Setenv("first", string(b))
			continue
		}
		if string(b) != os.Getenv("first") {
			t.Fatalf("write %d differs from the first", i)
		}
	}
}

// A missing lockfile is the ordinary case for a project that has not installed
// anything, and must not be an error.
func TestAMissingLockfileIsNotAnError(t *testing.T) {
	got, err := ReadLockfile(t.TempDir())
	if err != nil {
		t.Fatalf("ReadLockfile with no file: %v", err)
	}
	if got != nil {
		t.Errorf("ReadLockfile = %+v, want nil", got)
	}
}

// The whole point of the file: a binary swapped on disk after install is
// caught at the next command rather than never.
func TestCheckRefusesAChangedChecksumAndNamesThePlugin(t *testing.T) {
	l := &Lockfile{Version: LockVersion, Plugins: map[string]LockEntry{
		"aws": {Version: "0.4.0", Source: "github.com/infrena/infrena-provider-aws",
			Checksums: map[string]string{"linux/amd64": "sha256:aaaa"}},
	}}

	if err := l.Check("aws", "linux/amd64", "sha256:aaaa"); err != nil {
		t.Errorf("a matching checksum was refused: %v", err)
	}
	err := l.Check("aws", "linux/amd64", "sha256:dddd")
	if err == nil {
		t.Fatal("a changed checksum was accepted")
	}
	if !strings.Contains(err.Error(), "aws") {
		t.Errorf("error does not name the plugin: %v", err)
	}
}

// A platform the lock does not record is NOT a silent pass. The lock is what
// makes a binary trustworthy, and "no entry for your machine" must be visible
// rather than read as approval.
func TestCheckRefusesAPlatformTheLockDoesNotRecord(t *testing.T) {
	l := &Lockfile{Version: LockVersion, Plugins: map[string]LockEntry{
		"aws": {Version: "0.4.0", Checksums: map[string]string{"linux/amd64": "sha256:aaaa"}},
	}}
	if err := l.Check("aws", "darwin/arm64", "sha256:aaaa"); err == nil {
		t.Error("a platform with no recorded checksum was accepted")
	}
}

// A plugin with no entry at all is not an error: a hand-placed binary keeps
// working, and the lock governs what install put there.
func TestCheckIgnoresAPluginTheLockDoesNotMention(t *testing.T) {
	l := &Lockfile{Version: LockVersion, Plugins: map[string]LockEntry{}}
	if err := l.Check("handplaced", "linux/amd64", "sha256:whatever"); err != nil {
		t.Errorf("a plugin absent from the lock was refused: %v", err)
	}
}

// A file this version does not understand is an error naming the file and both
// versions, never a guess at what a future format meant.
func TestAFutureLockVersionIsRefusedAndNamesBothVersions(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, LockfileName), []byte(`{"version":99,"plugins":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := ReadLockfile(dir)
	if err == nil {
		t.Fatal("a lockfile from a future version was read as though it were understood")
	}
	for _, want := range []string{LockfileName, "99", "1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}
