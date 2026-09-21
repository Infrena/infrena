package source

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PrepopulateCache creates a cache entry that Resolve accepts without touching
// the network, and is the only supported way to build one from outside this
// package.
//
// It exists for tests in other packages, which cannot import a _test.go file and
// must not recompute the cache layout themselves. It takes no *testing.T and
// returns an error, so no production file imports "testing".
//
// The guards below are the point: every way of misusing this produces an entry
// Resolve silently discards, after which the source is fetched for real and the
// test fails with what looks like a network error.
func PrepopulateCache(projectDir string, s Source, commit string, files map[string]string) error {
	switch {
	case s.Kind != KindGit:
		return fmt.Errorf("prepopulate: %s is a path source and is never cached; point the test at the directory instead", s)

	case !s.PinnedToHash():
		// A tag is re-checked with ls-remote on every resolve, because
		// that check is the whole of the moved-tag detection. A
		// prepopulated tag entry therefore still needs a reachable
		// remote, which is what the caller was trying to avoid.
		return fmt.Errorf("prepopulate: %s is pinned to a tag, and a tag is re-checked against the remote on every resolve; only a commit pin skips the network", s)

	case len(commit) != 40:
		return fmt.Errorf("prepopulate: commit %q is not a full 40-character hash; the marker records the commit, not an abbreviation of it", commit)

	case !strings.HasPrefix(commit, s.Ref):
		return fmt.Errorf("prepopulate: commit %q does not start with the pinned ref %q, so Resolve would discard this entry as belonging to another commit", commit, s.Ref)
	}

	dir := NewCache(projectDir).checkoutDir(s)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for rel, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			return err
		}
	}

	// Written last, as fetch writes it: a directory without a valid marker
	// is one this code never published.
	return writeMarker(dir, marker{
		Version: 1, Location: s.Location, Ref: s.Ref, Commit: commit, FetchedAt: time.Now().UTC(),
	})
}
