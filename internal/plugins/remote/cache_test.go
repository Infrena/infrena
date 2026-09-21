package remote

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAFreshEntryIsReturnedAndAStaleOneIsNot(t *testing.T) {
	now := time.Unix(1000, 0)
	c := &Cache{Dir: t.TempDir(), TTL: time.Hour, Now: func() time.Time { return now }}
	if err := c.Put("mycorp/repos", []byte("payload")); err != nil {
		t.Fatal(err)
	}

	if got, ok := c.Get("mycorp/repos"); !ok || string(got) != "payload" {
		t.Errorf("Get = %q, %v; want the payload", got, ok)
	}

	now = now.Add(2 * time.Hour)
	if _, ok := c.Get("mycorp/repos"); ok {
		t.Error("a stale entry was returned")
	}
}

// A key contains a slash and whatever an owner is called. It must not be able
// to escape the cache directory or collide with another key.
func TestAKeyCannotEscapeTheCacheDirectory(t *testing.T) {
	dir := t.TempDir()
	c := &Cache{Dir: dir, TTL: time.Hour, Now: time.Now}

	if err := c.Put("../../etc/passwd", []byte("x")); err != nil {
		t.Fatal(err)
	}

	var outside []string
	filepath.WalkDir(filepath.Dir(dir), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && !strings.HasPrefix(p, dir) && strings.Contains(p, "passwd") {
			outside = append(outside, p)
		}
		return nil
	})
	if len(outside) > 0 {
		t.Errorf("cache wrote outside its directory: %v", outside)
	}
}

// A corrupt or unreadable cache entry is a MISS, never an error: a cache is an
// optimisation, and one that can fail a command is worse than no cache.
func TestACorruptEntryIsAMissRatherThanAnError(t *testing.T) {
	dir := t.TempDir()
	c := &Cache{Dir: dir, TTL: time.Hour, Now: time.Now}
	if err := c.Put("k", []byte("payload")); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) == 0 {
		t.Fatal("nothing was written")
	}
	if err := os.WriteFile(filepath.Join(dir, entries[0].Name()), []byte("\x00 not valid"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, ok := c.Get("k"); ok {
		t.Error("a corrupt entry was returned as a hit")
	}
}

// Two keys that differ only in a character a filesystem might fold or a
// sanitiser might strip must not share one entry, or a search for one owner
// answers with another owner's repositories.
func TestDifferentKeysDoNotCollide(t *testing.T) {
	c := &Cache{Dir: t.TempDir(), TTL: time.Hour, Now: time.Now}
	if err := c.Put("mycorp/repos", []byte("mine")); err != nil {
		t.Fatal(err)
	}
	if err := c.Put("mycorp/repos/", []byte("theirs")); err != nil {
		t.Fatal(err)
	}

	got, ok := c.Get("mycorp/repos")
	if !ok || string(got) != "mine" {
		t.Errorf("Get = %q, %v; want the first payload back", got, ok)
	}
}

// A miss on a cache that was never written is a miss, not an error: the first
// search on a new machine must not be the one that fails.
func TestAnAbsentEntryIsAPlainMiss(t *testing.T) {
	c := &Cache{Dir: filepath.Join(t.TempDir(), "never-created"), TTL: time.Hour, Now: time.Now}
	if _, ok := c.Get("anything"); ok {
		t.Error("an entry that was never written was reported as a hit")
	}
}

func TestDefaultCacheDirIsUnderTheUsersCacheDirectory(t *testing.T) {
	dir, err := DefaultCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(filepath.ToSlash(dir), "infrena/plugins") {
		t.Errorf("DefaultCacheDir = %q, want it to end in infrena/plugins", dir)
	}
}
