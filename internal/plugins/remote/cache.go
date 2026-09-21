package remote

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// DefaultTTL is how long a search answer is reused. An hour matches GitHub's
// rate-limit window, so a user who searches repeatedly while deciding on a
// plugin spends their allowance once.
const DefaultTTL = time.Hour

// Cache is a disk cache with a TTL, sitting between the client and the forge so
// a repeated search costs nothing.
//
// Every failure on the read path is a miss rather than an error: a cache that
// can fail a command is worse than no cache, since the command would break for a
// reason unrelated to what the user asked.
type Cache struct {
	// Dir holds the entries. It is created on first write.
	Dir string
	// TTL is how long an entry stays fresh. Zero or negative means nothing is
	// ever fresh, which is the behaviour a --refresh flag wants.
	TTL time.Duration
	// Now reads the clock, so a test can move it. Nil means time.Now.
	Now func() time.Time
}

// entry is what is stored. The time lives with the payload rather than in the
// file's mtime, which ordinary tools (a backup restore, a copy) rewrite and
// would make a stale answer look fresh.
type entry struct {
	StoredAt time.Time `json:"stored_at"`
	Data     []byte    `json:"data"`
}

// Get returns a fresh entry, or reports a miss.
//
// A miss is returned for everything: no entry, an unreadable one, a corrupt
// one, a stale one, and one dated in the future by a clock that moved. None of
// those is worth failing a search over, and a miss simply costs one request.
func (c *Cache) Get(key string) ([]byte, bool) {
	raw, err := os.ReadFile(c.path(key))
	if err != nil {
		return nil, false
	}

	var e entry
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, false
	}

	age := c.now().Sub(e.StoredAt)
	if age < 0 || age > c.TTL {
		return nil, false
	}
	return e.Data, true
}

// Put stores a payload under a key.
//
// A failure is reported so a caller can mention it, but no caller should treat
// it as fatal: failing to write a cache entry has no effect on the answer the
// user asked for.
func (c *Cache) Put(key string, data []byte) error {
	if err := os.MkdirAll(c.Dir, 0o700); err != nil {
		return fmt.Errorf("creating the plugin cache directory %s: %w: check the permissions on it, or search with the cache disabled", c.Dir, err)
	}

	raw, err := json.Marshal(entry{StoredAt: c.now(), Data: data})
	if err != nil {
		return fmt.Errorf("encoding a cache entry for %q: %w: this is a bug, and the search still works without the cache", key, err)
	}

	// Written beside the target and renamed, so a reader never sees a half
	// written entry and a crash leaves no torn file behind.
	tmp, err := os.CreateTemp(c.Dir, "entry-*.tmp")
	if err != nil {
		return fmt.Errorf("creating a cache entry in %s: %w: check the permissions on it", c.Dir, err)
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("writing a cache entry in %s: %w: check there is space on the device", c.Dir, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing a cache entry in %s: %w: check there is space on the device", c.Dir, err)
	}
	if err := os.Rename(tmp.Name(), c.path(key)); err != nil {
		return fmt.Errorf("storing a cache entry in %s: %w: check the permissions on it", c.Dir, err)
	}
	return nil
}

// path names the file for a key.
//
// The key is hashed rather than sanitised. A key carries slashes and whatever an
// owner called themselves, and hashing removes the traversal question entirely:
// no sequence of characters can turn into a parent directory.
func (c *Cache) path(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(c.Dir, hex.EncodeToString(sum[:])+".json")
}

func (c *Cache) now() time.Time {
	if c.Now == nil {
		return time.Now()
	}
	return c.Now()
}

// DefaultCacheDir is where search answers live: <user cache dir>/infrena/plugins.
// It is not created here, because Put creates it and a search that never writes
// should leave nothing behind.
func DefaultCacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("finding the user cache directory: %w: set XDG_CACHE_HOME or HOME, or search with the cache disabled", err)
	}
	return filepath.Join(base, "infrena", "plugins"), nil
}
