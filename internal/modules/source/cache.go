package source

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/infrata/infrata/internal/diag"
)

const (
	// markerName is written INSIDE a checkout, last, before the checkout is
	// renamed into place. A directory without a valid marker was never
	// published by this code — an interrupted fetch, or something a user
	// put there — and is discarded rather than loaded as a module.
	markerName = ".infra-module"

	defaultLockTimeout = 2 * time.Minute
	lockPollInterval   = 100 * time.Millisecond
)

// Resolution is what a source resolved to.
type Resolution struct {
	// Dir is a directory on disk holding the module.
	Dir string
	// Commit is the commit a git source resolved to, and is empty for a
	// path source: a directory has no commit, and recording one in
	// modules.lock would record something nothing can check.
	Commit string
}

// marker is the JSON written into a published checkout.
type marker struct {
	Version   int       `json:"version"`
	Location  string    `json:"location"`
	Ref       string    `json:"ref"`
	Commit    string    `json:"commit"`
	FetchedAt time.Time `json:"fetched_at"`
}

// Cache turns a Source into a directory on disk, fetching a git source into
// <ProjectDir>/.infra/modules if it is not already there.
type Cache struct {
	// ProjectDir is the project root: the cache lives at
	// <ProjectDir>/.infra/modules and modules.lock beside infra.yml.
	ProjectDir string
	// LockTimeout bounds how long Resolve waits for another process to
	// finish fetching the same source. Zero means defaultLockTimeout.
	LockTimeout time.Duration
	// Git is the runner; the zero value is the real one.
	Git gitRunner
}

// NewCache returns a cache rooted at a project directory.
func NewCache(projectDir string) *Cache {
	return &Cache{ProjectDir: projectDir}
}

func (c *Cache) root() string { return filepath.Join(c.ProjectDir, ".infra", "modules") }

// checkoutDir is where a git source lives once fetched. It is content
// addressed on the LOCATION so that two sources naming one repository share a
// fetch, with the ref below it so that two refs of one repository coexist.
func (c *Cache) checkoutDir(s Source) string {
	return filepath.Join(c.root(), locationKey(s.Location), refSlug(s.Ref))
}

func (c *Cache) lockPath(s Source) string {
	return filepath.Join(c.root(), locationKey(s.Location), refSlug(s.Ref)+".lock")
}

func locationKey(location string) string {
	sum := sha256.Sum256([]byte(location))
	return hex.EncodeToString(sum[:])[:16]
}

// refSlug keeps the ref readable when it is already one safe path segment, and
// falls back to a hash otherwise: a ref may contain "/" (release/1.0), and a
// nested directory would put the checkout and its lock file at different depths.
func refSlug(ref string) string {
	ok := ref != "" && ref != "." && ref != ".."
	for _, r := range ref {
		switch {
		case 'a' <= r && r <= 'z', 'A' <= r && r <= 'Z', '0' <= r && r <= '9':
		case r == '.', r == '_', r == '-', r == '+':
		default:
			ok = false
		}
	}
	if ok {
		return ref
	}
	sum := sha256.Sum256([]byte(ref))
	return "ref-" + hex.EncodeToString(sum[:])[:16]
}

// Resolve returns a directory holding the module s names, fetching it if
// necessary. baseDir is the directory of the file that declared the source,
// which is what a relative path is relative to.
//
// It WRITES NOTHING outside the cache: no modules.lock, no record of what it
// resolved (Amendment 18). Only stage 5 sees every resolution, including the
// nested ones a fetched module's own module.yml declares, and only a complete
// set can be written in one shot — a lockfile built one entry per call here
// would be readable half-written by anything that looked.
func (c *Cache) Resolve(s Source, baseDir string) (Resolution, diag.Diagnostics) {
	if s.Kind == KindPath {
		return c.resolvePath(s, baseDir)
	}
	return c.resolveGit(s)
}

func (c *Cache) resolvePath(s Source, baseDir string) (Resolution, diag.Diagnostics) {
	dir := filepath.FromSlash(s.Location)
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(baseDir, dir)
	}

	info, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Resolution{}, one(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "module source " + strconv.Quote(s.Location) + " does not exist",
			Detail: "A path source is resolved relative to the file that declared it. " +
				strconv.Quote(s.Location) + " was looked for at " + dir + ", and there is nothing there.",
			Action: "Create the directory with a module.yml in it, or correct the path.",
			Origin: s.Origin,
		})
	case err != nil:
		return Resolution{}, one(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "module source " + strconv.Quote(s.Location) + " cannot be read",
			Detail:   err.Error(),
			Action:   "Check the permissions on " + dir + ".",
			Origin:   s.Origin,
		})
	case !info.IsDir():
		return Resolution{}, one(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "module source " + strconv.Quote(s.Location) + " is a file",
			Detail:   "A module is a DIRECTORY containing module.yml. " + dir + " is a file.",
			Action:   "Point the source at the directory: " + filepath.Dir(s.Location) + ".",
			Origin:   s.Origin,
		})
	}
	return Resolution{Dir: dir}, nil
}

func (c *Cache) resolveGit(s Source) (Resolution, diag.Diagnostics) {
	ctx := context.Background()

	// Checked before the lock is taken, because the common case is a cache
	// that is already good and no other process in sight.
	if res, ok := c.reusable(ctx, s); ok {
		return res, nil
	}

	release, ds := c.lock(s)
	if ds.HasErrors() {
		return Resolution{}, ds
	}
	defer release()

	// Checked AGAIN under the lock: the process that held it a moment ago
	// was very likely fetching exactly this, and without this second check
	// every queued caller re-fetches what it just waited for.
	if res, ok := c.reusable(ctx, s); ok {
		return res, nil
	}

	return c.fetch(ctx, s)
}

// reusable reports whether the cached checkout can be used as it stands.
//
// For a commit pin the answer never needs the network: a commit names an
// immutable object. For a TAG pin the remote is asked on every run, because a
// tag is mutable and a cache that never re-checks one is a cache that cannot
// see the change Amendment 10b exists to report. ls-remote transfers no
// objects, so the cost is one round trip, not a fetch.
func (c *Cache) reusable(ctx context.Context, s Source) (Resolution, bool) {
	dir := c.checkoutDir(s)
	m, ok := readMarker(dir)
	if !ok || m.Location != s.Location || m.Ref != s.Ref {
		return Resolution{}, false
	}

	if s.PinnedToHash() {
		if !strings.HasPrefix(m.Commit, s.Ref) {
			return Resolution{}, false
		}
		return Resolution{Dir: dir, Commit: m.Commit}, true
	}

	remote, err := c.Git.lsRemoteCommit(ctx, s.Location, s.Ref)
	if err != nil || remote != m.Commit {
		return Resolution{}, false
	}
	return Resolution{Dir: dir, Commit: m.Commit}, true
}

// fetch clones into a temp directory and publishes it with a rename.
//
// The rename is what makes a partially fetched tree impossible to observe:
// stage 5 reads a checkout without taking the lock, so a fetch that wrote into
// the final path directly would be visible half-done for as long as it ran.
func (c *Cache) fetch(ctx context.Context, s Source) (Resolution, diag.Diagnostics) {
	if err := os.MkdirAll(c.root(), 0o755); err != nil {
		return Resolution{}, one(c.ioDiag(s, err))
	}
	tmp, err := os.MkdirTemp(c.root(), ".tmp-")
	if err != nil {
		return Resolution{}, one(c.ioDiag(s, err))
	}
	defer os.RemoveAll(tmp) // a no-op once the rename has succeeded

	commit, err := c.Git.fetchCommit(ctx, tmp, s)
	if err != nil {
		return Resolution{}, one(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "module source " + strconv.Quote(s.String()) + " could not be fetched",
			Detail:   err.Error(),
			Action: "Check that the repository exists, that " + strconv.Quote(s.Ref) +
				" is a tag or commit in it, and that this machine can authenticate to it without a prompt (a credential helper for https, or an ssh key).",
			Origin: s.Origin,
		})
	}

	if err := writeMarker(tmp, marker{
		Version: 1, Location: s.Location, Ref: s.Ref, Commit: commit, FetchedAt: time.Now().UTC(),
	}); err != nil {
		return Resolution{}, one(c.ioDiag(s, err))
	}

	dir := c.checkoutDir(s)
	// Rename fails with ENOTEMPTY onto a non-empty directory, so the old
	// entry goes first. Both happen under the lock.
	if err := os.RemoveAll(dir); err != nil {
		return Resolution{}, one(c.ioDiag(s, err))
	}
	if err := os.Rename(tmp, dir); err != nil {
		// Another process published while we were fetching — only
		// reachable if locking failed. Its tree is as good as ours.
		if m, ok := readMarker(dir); ok && m.Commit == commit {
			return Resolution{Dir: dir, Commit: commit}, nil
		}
		return Resolution{}, one(c.ioDiag(s, err))
	}
	return Resolution{Dir: dir, Commit: commit}, nil
}

func (c *Cache) ioDiag(s Source, err error) diag.Diagnostic {
	return diag.Diagnostic{
		Severity: diag.SeverityError,
		Summary:  "the module cache for " + strconv.Quote(s.String()) + " could not be written",
		Detail:   err.Error(),
		Action:   "Check the permissions on " + c.root() + ", or remove it and re-run: it is a cache and can be rebuilt.",
		Origin:   s.Origin,
	}
}

// lock takes the fetch lock for one source, waiting for another process to
// finish rather than failing immediately: the holder is normally a few seconds
// from publishing exactly what this caller wants.
//
// The lock file is created by writing a temp file and hard-linking it into
// place. os.Link fails with EEXIST if the target exists, which is create-or-
// fail, which is exclusivity; os.Rename would OVERWRITE, so two processes would
// both believe they held the lock, both would remove the checkout directory,
// and one would delete the tree the other had just published while stage 5 was
// reading it. internal/state/lock.go makes the same choice for the same reason,
// and local.go's Put uses Rename for the opposite requirement.
//
// Unlike an environment lock, this one EXPIRES by timing out rather than
// waiting forever. An environment lock guards a mutation of the user's real
// infrastructure, where a wrong guess about staleness is unrecoverable; this one
// guards a directory that can be deleted and rebuilt.
func (c *Cache) lock(s Source) (release func(), ds diag.Diagnostics) {
	path := c.lockPath(s)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return func() {}, one(c.ioDiag(s, err))
	}

	timeout := c.LockTimeout
	if timeout <= 0 {
		timeout = defaultLockTimeout
	}
	deadline := time.Now().Add(timeout)

	for {
		err := linkLock(path, fmt.Sprintf(
			"{\"pid\":%d,\"host\":%q,\"location\":%q,\"ref\":%q,\"at\":%q}\n",
			os.Getpid(), hostname(), s.Location, s.Ref, time.Now().UTC().Format(time.RFC3339)))
		if err == nil {
			return func() { os.Remove(path) }, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return func() {}, one(c.ioDiag(s, err))
		}
		if time.Now().After(deadline) {
			held, _ := os.ReadFile(path)
			return func() {}, one(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "another process is fetching module source " + strconv.Quote(s.String()),
				Detail: "The fetch lock " + path + " was held for longer than " + timeout.String() +
					". It holds: " + strings.TrimSpace(string(held)),
				Action: "Wait for the other run to finish. If no other run is in progress, delete " + path + " — it is a cache lock, and deleting it cannot lose any state.",
				Origin: s.Origin,
			})
		}
		time.Sleep(lockPollInterval)
	}
}

func linkLock(path, content string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".lock-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Link(name, path)
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}

func readMarker(dir string) (marker, bool) {
	data, err := os.ReadFile(filepath.Join(dir, markerName))
	if err != nil {
		return marker{}, false
	}
	var m marker
	if err := json.Unmarshal(data, &m); err != nil || m.Version != 1 || m.Commit == "" {
		return marker{}, false
	}
	return m, true
}

func writeMarker(dir string, m marker) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, markerName), append(data, '\n'), 0o600)
}
