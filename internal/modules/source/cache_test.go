package source

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infrata/infrata/internal/diag"
	"github.com/infrata/infrata/pkg/value"
)

func TestResolveAPathSourceAgainstTheFileThatDeclaredIt(t *testing.T) {
	project := t.TempDir()
	nested := filepath.Join(project, "modules", "app")
	if err := os.MkdirAll(filepath.Join(nested, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	c := NewCache(project)

	// A source declared in modules/app/module.yml resolves against
	// modules/app, not against the project root — which is what baseDir is
	// for, and what makes "../shared" mean the same thing to a reader of that
	// file as it does to the loader.
	got, ds := c.Resolve(Source{Kind: KindPath, Location: "./sub"}, nested)
	if ds.HasErrors() {
		t.Fatalf("Resolve: %+v", ds)
	}
	if want := filepath.Join(nested, "sub"); got.Dir != want {
		t.Errorf("Dir = %q, want %q", got.Dir, want)
	}
	if got.Commit != "" {
		t.Errorf("Commit = %q, want empty: a directory has no commit, and recording one in modules.lock would record something nothing can check", got.Commit)
	}

	abs, ds := c.Resolve(Source{Kind: KindPath, Location: nested}, project)
	if ds.HasErrors() {
		t.Fatalf("Resolve(absolute): %+v", ds)
	}
	if abs.Dir != nested {
		t.Errorf("absolute Dir = %q, want %q", abs.Dir, nested)
	}
}

func TestResolveReportsAMissingPathSource(t *testing.T) {
	project := t.TempDir()
	origin := value.Origin{File: "infra.yml", Line: 3, Column: 5}

	_, ds := c13(t, project).Resolve(Source{Kind: KindPath, Location: "./modules/nope", Origin: origin}, project)
	if !ds.HasErrors() {
		t.Fatal("Resolve succeeded for a directory that does not exist")
	}
	var sb strings.Builder
	ds.Render(&sb)
	for _, want := range []string{"./modules/nope", "infra.yml:3:5", "Suggested action:"} {
		if !strings.Contains(sb.String(), want) {
			t.Errorf("diagnostic does not mention %q:\n%s", want, sb.String())
		}
	}
}

// TestResolveReportsAPathSourceThatIsAFile is a separate case because the
// failure a user hits is different: `source: ./modules/app/module.yml` is the
// natural mistake, and "not a directory" is the answer, not "does not exist".
func TestResolveReportsAPathSourceThatIsAFile(t *testing.T) {
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "module.yml"), []byte("inputs: {}\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, ds := c13(t, project).Resolve(Source{Kind: KindPath, Location: "./module.yml"}, project)
	if !ds.HasErrors() {
		t.Fatal("Resolve succeeded for a source naming a file")
	}
	var sb strings.Builder
	ds.Render(&sb)
	if !strings.Contains(sb.String(), "is a file") {
		t.Errorf("diagnostic does not say the source is a file:\n%s", sb.String())
	}
}

func c13(t *testing.T, project string) *Cache {
	t.Helper()
	c := NewCache(project)
	c.LockTimeout = 5 * time.Second
	c.Git = gitRunner{Timeout: 30 * time.Second}
	return c
}

func TestResolveFetchesAGitSourceIntoTheCache(t *testing.T) {
	repo := newTestRepo(t, map[string]string{"module.yml": "inputs: {}\n"})
	repo.Tag(t, "v1.0.0", false)
	project := t.TempDir()

	got, ds := c13(t, project).Resolve(gitSource(repo.Path, "v1.0.0"), project)
	if ds.HasErrors() {
		t.Fatalf("Resolve: %+v", ds)
	}
	if got.Commit != repo.Commit {
		t.Errorf("Commit = %q, want %q", got.Commit, repo.Commit)
	}
	if !strings.HasPrefix(got.Dir, filepath.Join(project, ".infra", "modules")) {
		t.Errorf("Dir = %q, want it under the project's .infra/modules", got.Dir)
	}
	if _, err := os.Stat(filepath.Join(got.Dir, "module.yml")); err != nil {
		t.Errorf("the checkout has no module.yml: %v", err)
	}
	if _, err := os.Stat(filepath.Join(got.Dir, markerName)); err != nil {
		t.Errorf("the checkout has no marker: %v — without one, an interrupted fetch is indistinguishable from a finished one", err)
	}
}

// TestResolveDoesNotFetchAHashPinThatIsAlreadyCached proves the skip by making
// the network impossible: after the first Resolve, the remote repository is
// DELETED. A second Resolve that reaches the network cannot succeed, so this
// fails the moment the skip rule is removed — and it cannot be satisfied by any
// amount of caching that still talks to the remote.
func TestResolveDoesNotFetchAHashPinThatIsAlreadyCached(t *testing.T) {
	repo := newTestRepo(t, map[string]string{"module.yml": "inputs: {}\n"})
	project := t.TempDir()
	c := c13(t, project)

	first, ds := c.Resolve(gitSource(repo.Path, repo.Commit), project)
	if ds.HasErrors() {
		t.Fatalf("first Resolve: %+v", ds)
	}
	if err := os.RemoveAll(repo.Path); err != nil {
		t.Fatalf("removing the remote: %v", err)
	}

	second, ds := c.Resolve(gitSource(repo.Path, repo.Commit), project)
	if ds.HasErrors() {
		t.Fatalf("second Resolve with the remote deleted: %+v — a commit pin that is already cached must not touch the network", ds)
	}
	if second.Dir != first.Dir || second.Commit != first.Commit {
		t.Errorf("second Resolve = %+v, want %+v", second, first)
	}
}

// TestResolveChecksATagPinAgainstTheRemoteEveryTime is the same test with the
// opposite expectation, and it is the one that keeps Amendment 10b possible. A
// tag is mutable, so a cached tag that is never re-checked is a plan that
// silently differs from yesterday's. Delete the ls-remote call for a cached tag
// and this passes — which is why the assertion is that it FAILS.
func TestResolveChecksATagPinAgainstTheRemoteEveryTime(t *testing.T) {
	repo := newTestRepo(t, map[string]string{"module.yml": "inputs: {}\n"})
	repo.Tag(t, "v1.0.0", false)
	project := t.TempDir()
	c := c13(t, project)

	if _, ds := c.Resolve(gitSource(repo.Path, "v1.0.0"), project); ds.HasErrors() {
		t.Fatalf("first Resolve: %+v", ds)
	}
	if err := os.RemoveAll(repo.Path); err != nil {
		t.Fatalf("removing the remote: %v", err)
	}
	if _, ds := c.Resolve(gitSource(repo.Path, "v1.0.0"), project); !ds.HasErrors() {
		t.Error("a cached TAG resolved with the remote gone; a tag must be re-checked on every run or a moved tag can never be detected")
	}
}

// TestResolveFollowsATagThatMoved covers the other half: when the remote's tag
// now points somewhere else, the cache must produce the new tree, not the old
// one. (Whether that CHANGE is reported to the user is Task 14's, at the
// modules.lock layer; here it is only that the checkout follows.)
func TestResolveFollowsATagThatMoved(t *testing.T) {
	repo := newTestRepo(t, map[string]string{"module.yml": "inputs: {}\n"})
	repo.Tag(t, "v1.0.0", false)
	project := t.TempDir()
	c := c13(t, project)

	first, ds := c.Resolve(gitSource(repo.Path, "v1.0.0"), project)
	if ds.HasErrors() {
		t.Fatalf("first Resolve: %+v", ds)
	}

	repo.CommitFiles(t, map[string]string{"module.yml": "inputs: {}\noutputs: {}\n"}, "second")
	repo.Tag(t, "v1.0.0", false)

	second, ds := c.Resolve(gitSource(repo.Path, "v1.0.0"), project)
	if ds.HasErrors() {
		t.Fatalf("second Resolve: %+v", ds)
	}
	if second.Commit == first.Commit {
		t.Fatalf("Commit is still %q after the tag moved", second.Commit)
	}
	body, err := os.ReadFile(filepath.Join(second.Dir, "module.yml"))
	if err != nil {
		t.Fatalf("reading the checkout: %v", err)
	}
	if !strings.Contains(string(body), "outputs:") {
		t.Errorf("the checkout is still the old tree: %q — a moved tag must re-publish, not just report", body)
	}
}

// TestResolveReplacesAnInterruptedCheckout builds the state an interrupted
// fetch leaves behind — a directory with some of the files and no marker — and
// requires Resolve to discard it. Delete the marker check and this fails by
// returning the junk directory, which stage 5 would then load as a module.
func TestResolveReplacesAnInterruptedCheckout(t *testing.T) {
	repo := newTestRepo(t, map[string]string{"module.yml": "inputs: {}\n"})
	repo.Tag(t, "v1.0.0", false)
	project := t.TempDir()
	c := c13(t, project)

	junk := c.checkoutDir(gitSource(repo.Path, "v1.0.0"))
	if err := os.MkdirAll(junk, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(junk, "half.txt"), []byte("truncated"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, ds := c.Resolve(gitSource(repo.Path, "v1.0.0"), project)
	if ds.HasErrors() {
		t.Fatalf("Resolve: %+v", ds)
	}
	if _, err := os.Stat(filepath.Join(got.Dir, "half.txt")); err == nil {
		t.Error("the interrupted checkout survived; a directory with no valid marker was never published by this code and must be discarded")
	}
	if _, err := os.Stat(filepath.Join(got.Dir, "module.yml")); err != nil {
		t.Errorf("the replacement checkout is missing module.yml: %v", err)
	}
}

// TestResolveReplacesACheckoutWhoseMarkerNamesAnotherCommit covers the hash-pin
// prefix check. A marker recording a different commit than the pin means the
// directory holds a different tree; trusting it would serve the wrong module
// from a pin that is supposed to be exact.
func TestResolveReplacesACheckoutWhoseMarkerNamesAnotherCommit(t *testing.T) {
	repo := newTestRepo(t, map[string]string{"module.yml": "inputs: {}\n"})
	project := t.TempDir()
	c := c13(t, project)

	got, ds := c.Resolve(gitSource(repo.Path, repo.Commit), project)
	if ds.HasErrors() {
		t.Fatalf("Resolve: %+v", ds)
	}
	bad := `{"version":1,"location":"` + repo.Path + `","ref":"` + repo.Commit + `","commit":"0000000000000000000000000000000000000000"}`
	if err := os.WriteFile(filepath.Join(got.Dir, markerName), []byte(bad), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	again, ds := c.Resolve(gitSource(repo.Path, repo.Commit), project)
	if ds.HasErrors() {
		t.Fatalf("Resolve after corrupting the marker: %+v", ds)
	}
	if again.Commit != repo.Commit {
		t.Errorf("Commit = %q, want %q: a marker disagreeing with the pin must be discarded, not believed", again.Commit, repo.Commit)
	}
}

// TestResolveWaitsForAnotherProcessAndGivesUpWithADiagnostic pins the lock
// itself. The lock file is created the way internal/state/lock.go creates one —
// os.Link, which fails if the target exists — so a lock that is already held is
// observable from here by creating the file.
//
// Delete the lock acquisition and this fails: Resolve returns a resolution
// instead of a diagnostic.
func TestResolveWaitsForAnotherProcessAndGivesUpWithADiagnostic(t *testing.T) {
	repo := newTestRepo(t, map[string]string{"module.yml": "inputs: {}\n"})
	repo.Tag(t, "v1.0.0", false)
	project := t.TempDir()
	c := c13(t, project)
	c.LockTimeout = 300 * time.Millisecond

	s := gitSource(repo.Path, "v1.0.0")
	lock := c.lockPath(s)
	if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(lock, []byte(`{"pid":1,"host":"elsewhere"}`), 0o600); err != nil {
		t.Fatalf("write lock: %v", err)
	}

	start := time.Now()
	_, ds := c.Resolve(s, project)
	if !ds.HasErrors() {
		t.Fatal("Resolve proceeded while another process held the fetch lock")
	}
	if elapsed := time.Since(start); elapsed < c.LockTimeout {
		t.Errorf("Resolve gave up after %s, before the %s timeout: it must wait, because the holder is normally about to finish", elapsed, c.LockTimeout)
	}
	var sb strings.Builder
	ds.Render(&sb)
	if !strings.Contains(sb.String(), lock) {
		t.Errorf("the diagnostic does not name the lock file, which is the only thing the user can act on:\n%s", sb.String())
	}
}

// TestResolveIsSafeWhenCallersRace runs eight concurrent Resolves of one source.
// What it catches is a torn publication: every caller must see a complete
// checkout, never a directory mid-fetch. It does NOT prove the fetch happened
// once — duplicate work is a cost, not a correctness failure, and no assertion
// here could tell the difference without counting invocations of git.
//
// Run with -race; the whole suite is run that way in 14.9 too.
func TestResolveIsSafeWhenCallersRace(t *testing.T) {
	repo := newTestRepo(t, map[string]string{"module.yml": "inputs: {}\n"})
	repo.Tag(t, "v1.0.0", false)
	project := t.TempDir()
	c := c13(t, project)
	c.LockTimeout = 60 * time.Second

	var wg sync.WaitGroup
	results := make([]Resolution, 8)
	errs := make([]diag.Diagnostics, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = c.Resolve(gitSource(repo.Path, "v1.0.0"), project)
		}(i)
	}
	wg.Wait()

	for i := range results {
		if errs[i].HasErrors() {
			t.Fatalf("caller %d: %+v", i, errs[i])
		}
		if results[i] != results[0] {
			t.Errorf("caller %d resolved to %+v, caller 0 to %+v", i, results[i], results[0])
		}
		if _, err := os.Stat(filepath.Join(results[i].Dir, "module.yml")); err != nil {
			t.Errorf("caller %d saw an incomplete checkout: %v", i, err)
		}
	}
}

// TestPrepopulateCacheProducesAnEntryResolveAccepts — add to cache_test.go.
//
// The location is unreachable on purpose. Resolve succeeding against
// example.invalid proves two things at once: the helper writes the entry where
// the cache actually looks, and a commit pin that is already cached skips the
// network entirely. Change the layout without changing the helper and this
// fails — which is the whole reason tests/integration is given a helper instead
// of the path.
func TestPrepopulateCacheProducesAnEntryResolveAccepts(t *testing.T) {
	project := t.TempDir()
	commit := strings.Repeat("a", 40)
	s, ds := Parse("https://example.invalid/repo:"+commit, value.Origin{})
	if ds.HasErrors() {
		t.Fatalf("Parse: %+v", ds)
	}
	if err := PrepopulateCache(project, s, commit, map[string]string{"module.yml": "inputs: {}\n"}); err != nil {
		t.Fatalf("PrepopulateCache: %v", err)
	}

	res, ds := c13(t, project).Resolve(s, project)
	if ds.HasErrors() {
		t.Fatalf("Resolve: %+v — a prepopulated commit pin must resolve with no network at all", ds)
	}
	if res.Commit != commit {
		t.Errorf("Commit = %q, want %q", res.Commit, commit)
	}
	if _, err := os.Stat(filepath.Join(res.Dir, "module.yml")); err != nil {
		t.Errorf("the prepopulated module.yml is not in the resolved directory: %v", err)
	}
}

// TestPrepopulateCacheRefusesWhatResolveWouldDiscard. Each of these produces an
// entry Resolve throws away, after which the test that used it fails with a
// network error and the author looks in the wrong place.
func TestPrepopulateCacheRefusesWhatResolveWouldDiscard(t *testing.T) {
	project := t.TempDir()
	commit := strings.Repeat("a", 40)

	tag, _ := Parse("https://example.invalid/repo:v1.0.0", value.Origin{})
	if err := PrepopulateCache(project, tag, commit, nil); err == nil {
		t.Error("a tag pin was accepted; a tag is re-checked against the remote on every resolve, so the entry would not have skipped the network")
	}

	hash, _ := Parse("https://example.invalid/repo:"+commit, value.Origin{})
	if err := PrepopulateCache(project, hash, strings.Repeat("b", 40), nil); err == nil {
		t.Error("a commit that does not match the pin was accepted; Resolve would discard the entry as another commit's")
	}
	if err := PrepopulateCache(project, hash, commit[:7], nil); err == nil {
		t.Error("an abbreviated commit was accepted")
	}
	if err := PrepopulateCache(project, Source{Kind: KindPath, Location: "./m"}, commit, nil); err == nil {
		t.Error("a path source was accepted")
	}
}
