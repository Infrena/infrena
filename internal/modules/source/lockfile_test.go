package source

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infrata/infrata/pkg/value"
)

func writeLockfileFixture(t *testing.T, project string, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(project, "modules.lock"), []byte(body), 0o644); err != nil {
		t.Fatalf("write modules.lock: %v", err)
	}
}

func readLockfile(t *testing.T, project string) Lockfile {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(project, "modules.lock"))
	if err != nil {
		t.Fatalf("reading modules.lock: %v", err)
	}
	var lf Lockfile
	if err := json.Unmarshal(data, &lf); err != nil {
		t.Fatalf("modules.lock is not valid JSON: %v\n%s", err, data)
	}
	return lf
}

// TestCheckIsAPureRead is the assertion Amendment 18 turns on. `infra validate`
// answers a question about a project and must not mutate it, and the only thing
// standing between those two is that Check writes nothing at all. A comparison
// that "helpfully" records what it saw would make validate a writer.
//
// Delete the pure-read property — have Check persist an absent entry — and this
// fails.
func TestCheckIsAPureRead(t *testing.T) {
	project := t.TempDir()
	lf, ds := LoadLockfile(project)
	if ds.HasErrors() {
		t.Fatalf("LoadLockfile on a project with no lockfile: %+v", ds)
	}

	rec := Record{Source: "https://example.invalid/repo", Ref: "v1.0.0", Commit: strings.Repeat("a", 40)}
	if ds := lf.Check(rec, value.Origin{}); ds.HasErrors() {
		t.Fatalf("Check on an absent entry reported %+v; an entry nobody has recorded yet is not a mismatch", ds)
	}

	entries, err := os.ReadDir(project)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		t.Errorf("Check created %q; it must write nothing, or `infra validate` becomes a command that mutates the project it is reporting on", e.Name())
	}
}

func TestCheckIsSilentWhenTheEntryMatches(t *testing.T) {
	project := t.TempDir()
	commit := strings.Repeat("a", 40)
	writeLockfileFixture(t, project, `{"version":1,"modules":[{"source":"https://example.invalid/repo","ref":"v1.0.0","commit":"`+commit+`"}]}`)

	lf, ds := LoadLockfile(project)
	if ds.HasErrors() {
		t.Fatalf("LoadLockfile: %+v", ds)
	}
	if ds := lf.Check(Record{Source: "https://example.invalid/repo", Ref: "v1.0.0", Commit: commit}, value.Origin{}); ds.HasErrors() {
		t.Errorf("Check reported %+v for an entry that matches", ds)
	}
}

// TestLoadLockfileRefusesAVersionItDoesNotUnderstand: guessing at a file this
// build cannot read would mean comparing against something whose meaning is
// unknown, which is worse than stopping.
func TestLoadLockfileRefusesAVersionItDoesNotUnderstand(t *testing.T) {
	project := t.TempDir()
	writeLockfileFixture(t, project, `{"version":2,"modules":[]}`)
	if _, ds := LoadLockfile(project); !ds.HasErrors() {
		t.Error("LoadLockfile accepted a version 2 file")
	}
}

// TestCheckReportsAMovedTagAndNeverAdoptsIt is Amendment 10b's whole point and
// Amendment 18's third semantic. A lockfile that silently adopts whatever it
// finds records history and enforces nothing — the appearance of pinning
// without the property, inside the feature built to prevent exactly that.
//
// Two assertions, and the second is the one that would be missed: the
// diagnostic, and that the file on disk is UNCHANGED afterwards.
func TestCheckReportsAMovedTagAndNeverAdoptsIt(t *testing.T) {
	project := t.TempDir()
	was := strings.Repeat("a", 40)
	now := strings.Repeat("b", 40)
	body := `{"version":1,"modules":[{"source":"https://example.invalid/repo","ref":"v1.0.0","commit":"` + was + `"}]}`
	writeLockfileFixture(t, project, body)

	lf, ds := LoadLockfile(project)
	if ds.HasErrors() {
		t.Fatalf("LoadLockfile: %+v", ds)
	}

	origin := value.Origin{File: "infra.yml", Line: 3, Column: 5}
	ds = lf.Check(Record{Source: "https://example.invalid/repo", Ref: "v1.0.0", Commit: now}, origin)
	if !ds.HasErrors() {
		t.Fatal("a tag that moved was accepted silently")
	}

	var sb strings.Builder
	ds.Render(&sb)
	got := sb.String()
	for _, want := range []string{"v1.0.0", was, now, "modules.lock", "infra.yml:3:5", "Suggested action:"} {
		if !strings.Contains(got, want) {
			t.Errorf("diagnostic does not mention %q — a report that does not name both commits cannot be acted on:\n%s", want, got)
		}
	}
	// The action must work with the commands this milestone ships. There is
	// no `infra modules update` and no --upgrade flag in M5.
	if strings.Contains(got, "infra modules") || strings.Contains(got, "--upgrade") {
		t.Errorf("the action names a surface this milestone does not have:\n%s", got)
	}

	after, err := os.ReadFile(filepath.Join(project, "modules.lock"))
	if err != nil {
		t.Fatalf("reading modules.lock: %v", err)
	}
	if string(after) != body {
		t.Errorf("modules.lock changed during a comparison:\n%s\nwant it byte-identical: a lockfile that adopts what it finds enforces nothing", after)
	}
}

// TestTheDocumentedRemedyWorks: the action tells the user to delete the entry
// and re-run. If that does not actually clear the error, the diagnostic is
// giving instructions that fail.
func TestTheDocumentedRemedyWorks(t *testing.T) {
	project := t.TempDir()
	now := strings.Repeat("b", 40)
	writeLockfileFixture(t, project, `{"version":1,"modules":[]}`)

	lf, ds := LoadLockfile(project)
	if ds.HasErrors() {
		t.Fatalf("LoadLockfile: %+v", ds)
	}
	if ds := lf.Check(Record{Source: "https://example.invalid/repo", Ref: "v1.0.0", Commit: now}, value.Origin{}); ds.HasErrors() {
		t.Errorf("with the entry removed, the comparison still fails: %+v", ds)
	}
}

// TestBumpingAPinIsANewEntryNotAMismatch is Amendment 20c, and it is the case
// that separates a lockfile from an obstruction.
//
// A user editing `:v1.0` to `:v2.0` has made a deliberate change and gets a
// commit that differs from what the lockfile records. Keyed on the source
// alone, that reads as a moved tag and the tool tells them to delete an entry
// to permit an edit they just made. Keyed on (source, ref) — which is what the
// record already carries — it is simply a pin this project has not seen before.
//
// The sabotage is one line: key on rec.Source instead of rec.Source + ref. It
// is the plausible mistake, because "one entry per module source" reads
// perfectly natural, and nothing else in the suite catches it.
func TestBumpingAPinIsANewEntryNotAMismatch(t *testing.T) {
	project := t.TempDir()
	const src = "https://example.invalid/repo"
	oldCommit := strings.Repeat("a", 40)
	newCommit := strings.Repeat("b", 40)
	writeLockfileFixture(t, project, `{"version":1,"modules":[{"source":"`+src+`","ref":"v1.0","commit":"`+oldCommit+`"}]}`)

	lf, ds := LoadLockfile(project)
	if ds.HasErrors() {
		t.Fatalf("LoadLockfile: %+v", ds)
	}

	bumped := Record{Source: src, Ref: "v2.0", Commit: newCommit}
	if ds := lf.Check(bumped, value.Origin{}); ds.HasErrors() {
		var sb strings.Builder
		ds.Render(&sb)
		t.Fatalf("editing the pin from v1.0 to v2.0 was reported as a problem:\n%s", sb.String())
	}

	// And the walk that follows replaces the file with what the
	// configuration now says: the v1.0 entry is gone because it was not
	// resolved, not because anything deleted it.
	if ds := WriteLockfile(project, []Record{bumped}); ds.HasErrors() {
		t.Fatalf("WriteLockfile: %+v", ds)
	}
	after := readLockfile(t, project)
	if len(after.Modules) != 1 || after.Modules[0] != bumped {
		t.Errorf("modules = %+v, want only %+v", after.Modules, bumped)
	}
}

// TestAHashPinnedSourceIsRecordedAndChecked. A commit cannot move, so this
// entry can never fire on its own — which is an argument for writing it, not
// against: a tampered cache or an abbreviated hash that resolved differently is
// otherwise unreported, and the mechanism would be untestable without a live
// remote.
func TestAHashPinnedSourceIsRecordedAndChecked(t *testing.T) {
	project := t.TempDir()
	const src = "https://example.invalid/repo"
	commit := strings.Repeat("a", 40)

	rec, ok := Pin(Source{Kind: KindGit, Location: src, Ref: commit}, Resolution{Dir: "/tmp/x", Commit: commit})
	if !ok {
		t.Fatal("Pin skipped a hash-pinned source; every KindGit source gets an entry")
	}
	if ds := WriteLockfile(project, []Record{rec}); ds.HasErrors() {
		t.Fatalf("WriteLockfile: %+v", ds)
	}

	lf, ds := LoadLockfile(project)
	if ds.HasErrors() {
		t.Fatalf("LoadLockfile: %+v", ds)
	}
	tampered := Record{Source: src, Ref: commit, Commit: strings.Repeat("b", 40)}
	if ds := lf.Check(tampered, value.Origin{}); !ds.HasErrors() {
		t.Error("a hash pin resolving to a different commit was accepted; that is a tampered cache or an abbreviated hash that moved, and it is exactly what the record is for")
	}
}

// TestWriteLockfileWritesTheWholeSetSorted. Amendment 18: the write is one
// call with every resolution the walk produced, never one call per Resolve. A
// lockfile built up entry by entry is readable half-written by anything that
// looks — the shape of the M4 lock-file bug (ae1e309), fixed once in
// internal/state/lock.go and not to be reintroduced in a new artifact.
func TestWriteLockfileWritesTheWholeSetSorted(t *testing.T) {
	project := t.TempDir()
	recs := []Record{
		{Source: "https://example.invalid/z", Ref: "v1", Commit: strings.Repeat("c", 40)},
		{Source: "https://example.invalid/a", Ref: "v2", Commit: strings.Repeat("b", 40)},
		{Source: "https://example.invalid/a", Ref: "v1", Commit: strings.Repeat("a", 40)},
	}
	if ds := WriteLockfile(project, recs); ds.HasErrors() {
		t.Fatalf("WriteLockfile: %+v", ds)
	}

	lf := readLockfile(t, project)
	if lf.Version != 1 {
		t.Errorf("version = %d, want 1: the wire format must be able to change without breaking a consumer", lf.Version)
	}
	want := []Record{
		{Source: "https://example.invalid/a", Ref: "v1", Commit: strings.Repeat("a", 40)},
		{Source: "https://example.invalid/a", Ref: "v2", Commit: strings.Repeat("b", 40)},
		{Source: "https://example.invalid/z", Ref: "v1", Commit: strings.Repeat("c", 40)},
	}
	if len(lf.Modules) != len(want) {
		t.Fatalf("modules = %+v, want %+v", lf.Modules, want)
	}
	for i := range want {
		if lf.Modules[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v — this file is committed, and a diff that reorders itself between runs is a diff nobody can read", i, lf.Modules[i], want[i])
		}
	}

	info, err := os.Stat(filepath.Join(project, "modules.lock"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("mode = %v, want 0644: this file is committed and read by everyone who checks the project out, unlike a plan artifact or a report", perm)
	}
}

// TestWriteLockfileWritesNothingForAProjectWithNoRemotes matches the
// integration assertion Author C already makes: a lock file listing nothing
// suggests pinning is happening.
func TestWriteLockfileWritesNothingForAProjectWithNoRemotes(t *testing.T) {
	project := t.TempDir()
	if ds := WriteLockfile(project, nil); ds.HasErrors() {
		t.Fatalf("WriteLockfile: %+v", ds)
	}
	if _, err := os.Stat(filepath.Join(project, "modules.lock")); !os.IsNotExist(err) {
		t.Errorf("modules.lock exists for a project that resolved no remote sources (stat err = %v)", err)
	}
}

// TestPinSkipsAPathSource keeps "what belongs in the lockfile" in this package.
// A directory has no commit, and an entry recording one would record something
// nothing can check.
func TestPinSkipsAPathSource(t *testing.T) {
	if _, ok := Pin(Source{Kind: KindPath, Location: "./modules/app"}, Resolution{Dir: "/tmp/x"}); ok {
		t.Error("Pin produced a lockfile entry for a path source")
	}
	rec, ok := Pin(Source{Kind: KindGit, Location: "https://example.invalid/r", Ref: "v1"}, Resolution{Dir: "/tmp/x", Commit: strings.Repeat("a", 40)})
	if !ok {
		t.Fatal("Pin refused a git source")
	}
	if rec.Source != "https://example.invalid/r" || rec.Ref != "v1" {
		t.Errorf("Pin = %+v", rec)
	}
}

// TestWriteLockfileOverwritesAnExistingLockfile pins the os.Rename half of the
// Rename-versus-Link distinction, which nothing else in this suite reaches.
//
// Every other write test calls WriteLockfile once, against a project that has
// no lockfile yet — and os.Link succeeds when the target does not exist. So
// swapping Rename for Link, which is the mistake this repository has made in
// each direction once, leaves the whole suite green while making the SECOND
// write of any project fail with a phantom "file exists" conflict. That is a
// regression a user hits on their second plan, not their first.
//
// The second write also changes the set, so this fails if Rename is swapped for
// Link (EEXIST) AND if the write is made additive rather than replacing.
func TestWriteLockfileOverwritesAnExistingLockfile(t *testing.T) {
	project := t.TempDir()

	first := []Record{
		{Source: "https://example.invalid/a", Ref: "v1", Commit: strings.Repeat("a", 40)},
		{Source: "https://example.invalid/gone", Ref: "v9", Commit: strings.Repeat("9", 40)},
	}
	if ds := WriteLockfile(project, first); ds.HasErrors() {
		t.Fatalf("first WriteLockfile: %+v", ds)
	}

	second := []Record{
		{Source: "https://example.invalid/a", Ref: "v2", Commit: strings.Repeat("b", 40)},
	}
	if ds := WriteLockfile(project, second); ds.HasErrors() {
		t.Fatalf("second WriteLockfile: %+v — a lockfile that cannot be rewritten is a lockfile that only works once", ds)
	}

	lf := readLockfile(t, project)
	if len(lf.Modules) != 1 {
		t.Fatalf("modules = %+v, want exactly the second set: writing the complete set IS the prune, so a source the configuration no longer names must be gone", lf.Modules)
	}
	if lf.Modules[0] != second[0] {
		t.Errorf("entry = %+v, want %+v", lf.Modules[0], second[0])
	}
}
