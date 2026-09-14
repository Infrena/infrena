package source

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/pkg/value"
)

// lockfileName sits beside infra.yml and IS committed — unlike .infra/, which
// is derived data. It records what each remote source resolved to, so that a
// tag moving underneath a project is a reported change rather than a plan that
// quietly differs from yesterday's.
const lockfileName = "modules.lock"

// Record is one entry of modules.lock.
type Record struct {
	Source string `json:"source"`
	Ref    string `json:"ref"`
	Commit string `json:"commit"`
}

// Lockfile is the file's shape. Version exists so the format can change without
// breaking a consumer; it is never omitted.
type Lockfile struct {
	Version int      `json:"version"`
	Modules []Record `json:"modules"`

	// byKey indexes Modules for Check. Unexported so that a Lockfile
	// decoded straight from JSON by a test still works through Check,
	// which rebuilds it lazily.
	byKey map[string]Record
}

// Pin is the lockfile entry a resolution produces, and reports whether there is
// one. A path source has no commit: a directory is not a version, and an entry
// recording one would record something nothing can check.
//
// Every git source gets one, hash pins included. A commit cannot move, so that
// entry can never fire by itself — but excluding it would leave a tampered
// cache, or an abbreviated hash that resolved to something else, unreported,
// and would make the comparison untestable without a live remote.
func Pin(s Source, res Resolution) (Record, bool) {
	if s.Kind != KindGit || res.Commit == "" {
		return Record{}, false
	}
	return Record{Source: s.Location, Ref: s.Ref, Commit: res.Commit}, true
}

// LoadLockfile reads modules.lock. A missing file is not an error: the first
// run of a project that uses a remote module creates it.
func LoadLockfile(projectDir string) (Lockfile, diag.Diagnostics) {
	path := filepath.Join(projectDir, lockfileName)

	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Lockfile{Version: 1}, nil
	}
	if err != nil {
		return Lockfile{}, one(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  lockfileName + " cannot be read",
			Detail:   err.Error(),
			Action:   "Check the permissions on " + path + ".",
		})
	}

	var lf Lockfile
	if err := json.Unmarshal(data, &lf); err != nil || lf.Version != 1 {
		return Lockfile{}, one(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  lockfileName + " is not readable as version 1",
			Detail: "It records what each remote module source resolved to. Rather than guess at a file this " +
				"version does not understand — and then compare against something whose meaning is unknown — " +
				"planning stops here.",
			Action: "Delete " + path + " and re-run to record the current commits, then commit the result.",
		})
	}
	return lf, nil
}

// Check compares one resolution against what the lockfile records. It is a PURE
// READ: it writes nothing, ever.
//
// That is what lets `infra validate` report a moved tag without mutating the
// project it is answering a question about. It is also the third of Amendment
// 18's semantics: an entry that differs is an ERROR and is never adopted. A
// lockfile that silently updates itself to whatever it finds records history
// and enforces nothing — the appearance of pinning without the property, inside
// the feature built to prevent that.
//
// An entry that is absent is not a mismatch. It is a source this project has
// not pinned yet, and WriteLockfile records it after a successful walk.
//
// The key is (Source, Ref), not Source (Amendment 20c). Keyed on the source
// alone, a user editing ":v1.0" to ":v2.0" resolves to a commit that differs
// from the record and is told their own deliberate edit is a moved tag. Under
// this key that edit is simply a pin nobody has recorded yet, and the old
// entry disappears because the next successful walk writes the complete set
// without it.
//
// There is no "the source is recorded under a different ref" branch below, and
// there must not be: that is the same mistake spelled a second way. A changed
// ref is a changed pin, and a changed pin is not a conflict to report — it is
// the user telling this project what they now want.
func (lf *Lockfile) Check(rec Record, origin value.Origin) diag.Diagnostics {
	if lf.byKey == nil {
		lf.byKey = make(map[string]Record, len(lf.Modules))
		for _, r := range lf.Modules {
			lf.byKey[r.Source+"\x00"+r.Ref] = r
		}
	}

	prev, ok := lf.byKey[rec.Source+"\x00"+rec.Ref]
	if !ok || prev.Commit == rec.Commit {
		return nil
	}
	return one(diag.Diagnostic{
		Severity: diag.SeverityError,
		Summary:  "module source " + strconv.Quote(rec.Source+":"+rec.Ref) + " no longer resolves to the recorded commit",
		Detail: lockfileName + " records " + prev.Commit + " for " + strconv.Quote(rec.Ref) +
			", and it now resolves to " + rec.Commit + ". A tag can be moved, so this is a change to what this " +
			"project builds even though nothing in the configuration changed.",
		Action: "If the move is intended, delete that entry from " + lockfileName +
			" (or the whole file) and re-run to record " + rec.Commit + ". If it is not, pin the source to the " +
			"commit instead: " + rec.Source + ":" + prev.Commit + ".",
		Origin: origin,
	})
}

// WriteLockfile persists every resolution a walk produced, in ONE call.
//
// One call, not one per resolution: a file built up entry by entry is readable
// half-written by anything that looks, and if the walk then fails on a cycle or
// a refused source it records half a resolution as though it were complete.
// That is the shape of the M4 lock-file bug (ae1e309), fixed once in
// internal/state/lock.go, and it must not reappear in a new artifact.
//
// The caller decides WHEN: after a successful walk, from a mutating command.
// `infra validate` never calls this — Check is the whole of what it needs.
//
// Because the set is complete, writing it IS the prune: a source the
// configuration no longer names is simply absent from what is written.
func WriteLockfile(projectDir string, records []Record) diag.Diagnostics {
	if len(records) == 0 {
		// Nothing to pin. An existing file is left alone rather than
		// deleted: it is the user's, and it is committed.
		return nil
	}

	dedup := make(map[string]Record, len(records))
	for _, r := range records {
		dedup[r.Source+"\x00"+r.Ref] = r
	}
	lf := Lockfile{Version: 1, Modules: make([]Record, 0, len(dedup))}
	for _, r := range dedup {
		lf.Modules = append(lf.Modules, r)
	}
	// Sorted once, where it is produced: this file is committed, and a diff
	// that reorders itself between runs is a diff nobody can read.
	sort.Slice(lf.Modules, func(i, j int) bool {
		if lf.Modules[i].Source != lf.Modules[j].Source {
			return lf.Modules[i].Source < lf.Modules[j].Source
		}
		return lf.Modules[i].Ref < lf.Modules[j].Ref
	})

	data, err := json.MarshalIndent(lf, "", "  ")
	if err != nil {
		return one(lockfileWriteDiag(projectDir, err))
	}

	return writeFileAtomically(filepath.Join(projectDir, lockfileName), append(data, '\n'), 0o644, projectDir)
}

// writeFileAtomically writes a temp file in the same directory and renames it
// over the target.
//
// os.Rename, NOT the os.Link that the cache's fetch lock uses (13.6), and the
// difference is the requirement rather than a preference. Rename OVERWRITES,
// which is exactly the job here: replacing the lockfile with the new one is
// what writing it means, and internal/state/local.go's Put does the same for
// the same reason. Link fails if the target exists, which is what makes it
// right for a lock that must be held by one process and wrong for a file that
// is rewritten on every successful run.
//
// This project has had that call wrong in each direction once. Getting it
// backwards here would mean a second run of any project with a remote module
// fails to write its lockfile and reports a lock conflict that does not exist.
func writeFileAtomically(path string, data []byte, perm os.FileMode, dir string) diag.Diagnostics {
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return one(lockfileWriteDiag(dir, err))
	}
	name := tmp.Name()
	defer os.Remove(name) // a no-op once the rename has succeeded

	// 0644, not the 0600 a plan artifact or a report gets: this file is
	// committed and read by everyone who checks the project out, and it
	// holds no secrets — Parse refuses a source carrying credentials.
	if err := tmp.Chmod(perm); err == nil {
		_, err = tmp.Write(data)
	}
	if err != nil {
		tmp.Close()
		return one(lockfileWriteDiag(dir, err))
	}
	if err := tmp.Close(); err != nil {
		return one(lockfileWriteDiag(dir, err))
	}
	if err := os.Rename(name, path); err != nil {
		return one(lockfileWriteDiag(dir, err))
	}
	return nil
}

func lockfileWriteDiag(dir string, err error) diag.Diagnostic {
	return diag.Diagnostic{
		Severity: diag.SeverityError,
		Summary:  lockfileName + " could not be written",
		Detail:   err.Error(),
		Action:   "Check the permissions on " + dir + ".",
	}
}
