package plugins

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// LockfileName sits beside infra.yml and IS committed, exactly as modules.lock
// is. It records what install put on disk, so a binary replaced afterwards is
// caught at the next command rather than never.
const LockfileName = "plugins.lock"

// LockVersion is the format this build writes and the only one it reads. It is
// never omitted: a consumer that cannot tell version 1 from a later format can
// only guess, and guessing about the file that says which executables are
// trustworthy is the one place guessing is worst.
const LockVersion = 1

// LockEntry is one plugin's record: the version install resolved to, where it
// came from, and a checksum for each platform it was recorded on.
//
// A checksum PER PLATFORM, because a lockfile is committed and the next person
// to check the project out is not necessarily on the same machine. One
// checksum would mean either a file that only works for its author or a file
// that rewrites itself on every machine, and a lock that rewrites itself
// enforces nothing.
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
// A missing file is nil, nil. A project that has installed nothing has no lock
// and is not in an error state; it is the ordinary condition of a fresh
// checkout, and every caller has to handle "no lock" anyway.
//
// OFFLINE, and it must stay that way. This is read on the hot path before a
// plugin is launched, and the network is never on the hot path (section 31.3).
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
// Written in ONE call from the complete set, never entry by entry: a file built
// up incrementally is readable half-written by anything that looks, and a run
// that fails partway records half an install as though it were the whole.
//
// The bytes are stable across writes. encoding/json emits object keys in sorted
// order, and both maps here are keyed by strings, so the same lockfile marshals
// to the same bytes however Go happens to order the maps in memory this run.
// That matters because this file is committed: a diff that reorders itself
// between runs is a diff nobody can review.
func (l *Lockfile) Write(dir string) error {
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding %s: %w", LockfileName, err)
	}
	return writeLockAtomically(filepath.Join(dir, LockfileName), append(data, '\n'), dir)
}

// Check reports whether a binary is the one the lock recorded.
//
// TWO ABSENCES, TWO ANSWERS, and the difference is the whole design. A plugin
// the lock does not mention passes: a hand-placed binary keeps working (section
// 31.3), and the lock governs what INSTALL put on disk rather than claiming
// authority over everything. But a plugin the lock DOES mention, on a platform
// it does not record, FAILS: something installed this and recorded a checksum
// for another machine, so "no entry for yours" is a gap to see rather than
// permission to run whatever is there.
//
// A nil lockfile is the missing-file case ReadLockfile returns, and passes for
// the first of those reasons: a project with no lock has installed nothing, so
// there is nothing for this to have authority over.
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

// platformList names the platforms an entry does record, sorted, so the gap in
// the message above is a fact the user can act on rather than an assertion that
// something is missing.
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
// os.Rename, which OVERWRITES, and that is the requirement rather than a
// preference: replacing the lockfile with the new one is what writing it means.
// internal/modules/source does the same for modules.lock for the same reason.
//
// 0644, not the 0600 a plan artifact gets: this file is committed and read by
// everyone who checks the project out, and it holds no secrets.
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
