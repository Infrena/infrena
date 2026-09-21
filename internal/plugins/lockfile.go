package plugins

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// LockfileName sits beside infrena.yml and is committed, as modules.lock is. It
// records what install put on disk, so a binary replaced afterwards is caught at
// the next command.
const LockfileName = "plugins.lock"

// LockVersion is the format this build writes and the only one it reads. It is
// never omitted: a file that says which executables are trustworthy is the last
// place to guess at a format.
const LockVersion = 1

// PlatformKey is how the running machine is spelled in a lock entry's
// checksums. It lives here because the lock's format owns it: install writes a
// checksum under this key and the host reads one back under it before launching
// a binary, and two functions each building "GOOS/GOARCH" could drift apart
// silently — a lock that verifies nothing looks like one that verifies
// everything.
func PlatformKey() string {
	return runtime.GOOS + "/" + runtime.GOARCH
}

// LockEntry is one plugin's record: the version install resolved to, where it
// came from, and a checksum for each platform it was recorded on.
//
// A checksum per platform, because a lockfile is committed and the next person
// to check the project out is not necessarily on the same machine. A single
// checksum would have to rewrite itself on every machine, and a lock that
// rewrites itself enforces nothing.
type LockEntry struct {
	Version   string            `json:"version"`
	Source    string            `json:"source"`
	Checksums map[string]string `json:"checksums"`
}

// Lockfile is the shape of plugins.lock, keyed by the plugin name a project
// asks for.
type Lockfile struct {
	Version int                  `json:"version"`
	Plugins map[string]LockEntry `json:"plugins"`
}

// ReadLockfile reads plugins.lock from a project directory.
//
// A missing file is nil, nil: a project that has installed nothing has no lock
// and is not in an error state.
//
// This must stay offline. It is read before every plugin launch, and that path
// never touches the network.
func ReadLockfile(dir string) (*Lockfile, error) {
	path := filepath.Join(dir, LockfileName)

	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w; it records which plugin binaries this project trusts, so check the permissions on %s", LockfileName, err, path)
	}

	var lf Lockfile
	if err := json.Unmarshal(data, &lf); err != nil {
		return nil, fmt.Errorf("%s is not readable as version %d: %w; delete %s and re-run `infrena plugins install` to record the plugins this project uses, then commit the result", LockfileName, LockVersion, err, path)
	}
	if lf.Version != LockVersion {
		return nil, fmt.Errorf("%s is version %d and this build of infrena understands version %d; rather than guess at a file whose meaning is unknown - and then decide from it whether a binary may run - reading stops here. Upgrade infrena, or delete %s and re-run `infrena plugins install`",
			LockfileName, lf.Version, LockVersion, path)
	}
	if lf.Plugins == nil {
		lf.Plugins = map[string]LockEntry{}
	}
	return &lf, nil
}

// Write persists the lockfile into a project directory.
//
// Written in one call from the complete set, never entry by entry, so that a
// run failing partway cannot record half an install as though it were the whole.
//
// The bytes are stable across writes: encoding/json sorts object keys, and both
// maps here are string-keyed, so map iteration order cannot reorder the file.
// This file is committed, and a diff that reorders itself between runs is a diff
// nobody can review.
func (l *Lockfile) Write(dir string) error {
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding %s: %w", LockfileName, err)
	}
	return writeLockAtomically(filepath.Join(dir, LockfileName), append(data, '\n'), dir)
}

// Check reports whether a binary is the one the lock recorded.
//
// Two absences, two answers. A plugin the lock does not mention passes, so that
// a hand-placed binary keeps working: the lock governs what install put on disk
// rather than claiming authority over everything. A plugin the lock does
// mention, on a platform it does not record, fails: something installed this and
// recorded a checksum for another machine, so a missing entry for yours is a gap
// rather than permission to run whatever is there.
//
// A nil lockfile is the missing-file case ReadLockfile returns, and passes for
// the first reason.
func (l *Lockfile) Check(name, platform, sum string) error {
	if l == nil {
		return nil
	}

	entry, ok := l.Plugins[name]
	if !ok {
		return nil
	}

	want, ok := entry.Checksums[platform]
	if !ok {
		return fmt.Errorf("plugin %s is recorded in %s but has no checksum for %s.\n\nThe lock records %s, so this plugin was installed and pinned somewhere else. Nothing here can say whether the binary on this machine is the one that was approved.\n\nSuggested action:\n  Run `infrena plugins install %s` on this machine to fetch it and record %s, then commit %s.",
			name, LockfileName, platform, platformList(entry.Checksums), name, platform, LockfileName)
	}
	if want != sum {
		return fmt.Errorf("plugin %s does not match the checksum in %s for %s.\n\nRecorded: %s\nOn disk:  %s\n\nThe binary has been replaced since it was installed.\n\nSuggested action:\n  Run `infrena plugins install %s` to fetch it again, or restore the file the lock records.",
			name, LockfileName, platform, want, sum, name)
	}
	return nil
}

// platformList names the platforms an entry does record, sorted so the message
// above is stable.
func platformList(checksums map[string]string) string {
	names := make([]string, 0, len(checksums))
	for p := range checksums {
		names = append(names, p)
	}
	slices.Sort(names)

	if len(names) == 0 {
		return "no platforms at all"
	}
	return strings.Join(names, ", ")
}

// writeLockAtomically writes a temp file in the same directory and renames it
// over the target.
//
// The rename overwrites, which is the requirement: replacing the lockfile is
// what writing it means.
//
// 0644 rather than the 0600 a plan artifact gets, because this file is committed
// and read by everyone who checks the project out, and holds no secrets.
func writeLockAtomically(path string, data []byte, dir string) error {
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return fmt.Errorf("writing %s: %w; check the permissions on %s", LockfileName, err, dir)
	}
	name := tmp.Name()
	defer os.Remove(name) // a no-op once the rename has succeeded

	if err := tmp.Chmod(0o644); err == nil {
		_, err = tmp.Write(data)
	}
	if err != nil {
		tmp.Close()
		return fmt.Errorf("writing %s: %w; check the permissions on %s", LockfileName, err, dir)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing %s: %w; check the permissions on %s", LockfileName, err, dir)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("writing %s: %w; check the permissions on %s", LockfileName, err, dir)
	}
	return nil
}
