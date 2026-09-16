package pluginhost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/infrena/infrena/internal/plugins"
	"github.com/infrena/infrena/pkg/provider"
	"github.com/infrena/infrena/pkg/semver"
)

// Loader turns a plugin NAME into a running plugin.
//
// It exists because nothing should have to be told which plugins to load:
// configuration already says. A `providers:` entry naming `plugin: aws` is the
// instruction to go and find infrena-plugin-aws, and a resource of type
// `aws.instance` is the same instruction said another way. The CLI holds no list.
//
// ONE PROCESS PER PLUGIN PER COMMAND, which is what the cache is for: two accounts
// of one cloud means one process holding two configured clients, not two processes.
type Loader struct {
	// Search is where to look for a binary.
	Search SearchOptions
	// Dir is the project directory, handed to every plugin it loads.
	Dir string
	// Verbose, when set, receives each plugin's stderr and a line naming the path
	// each was loaded from.
	Verbose io.Writer

	// Constraints are the version ranges a project accepts, keyed by plugin name
	// (PLAN.md §31.1). Absent means unconstrained.
	//
	// Enforced HERE rather than at each call site, because four different paths load
	// plugins — a compile, the state-only commands, `explain`, and discovery's
	// load-everything — and a constraint checked in three of them is a constraint
	// nobody can rely on.
	Constraints map[string]semver.Constraint

	// Builtin serves a plugin from inside this process instead of a binary.
	//
	// A SHIPPED BUILD REGISTERS NOTHING HERE. This was transitional while
	// infrena-provider-fake was being built; the binary shipped, the fallback went
	// with it, and the field survives as the seam the engine's own tests inject the
	// fake double through (internal/cli's TestMain). It is not dead code and it is
	// not a second code path: a builtin is served over pluginhost.InProcess, so it
	// goes through the same protocol, handshake and trust rules a subprocess does,
	// which is what makes testing through it worth anything.
	//
	// The loader still prefers a binary on the search path, so a builtin cannot
	// shadow an installed plugin.
	Builtin map[string]provider.Plugin

	mu      sync.Mutex
	loaded  map[string]*Plugin
	failed  map[string]error
	started bool
	// order is the names that failed, in the order they were asked for, so a
	// report about them reads in the order the run met them rather than in Go
	// map order, which changes between runs.
	order []string

	// lock is plugins.lock, read from Dir the first time a binary is about to be
	// launched and remembered for the rest of the command.
	//
	// READ HERE RATHER THAN PASSED IN, and that is the whole reason this is a
	// field the loader fills rather than one a caller sets. Four paths load
	// plugins; a lock handed in by three of them is a lock nobody can rely on,
	// and the failure mode of forgetting is silence - everything keeps working,
	// and nothing is checked.
	lock     *plugins.Lockfile
	lockErr  error
	lockRead bool
}

// Load returns the named plugin, launching it the first time it is asked for.
//
// A failure is CACHED as well as a success. A project naming one missing plugin on
// twenty resources should say so once.
func (l *Loader) Load(ctx context.Context, name string) (provider.Plugin, error) {
	p, err := l.load(ctx, name)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// load is Load with the concrete type, for callers inside this package.
func (l *Loader) load(ctx context.Context, name string) (*Plugin, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.started {
		l.loaded = map[string]*Plugin{}
		l.failed = map[string]error{}
		l.order = nil
		l.started = true
	}
	if p, ok := l.loaded[name]; ok {
		return p, nil
	}
	if err, ok := l.failed[name]; ok {
		return nil, err
	}

	p, err := l.open(ctx, name)
	if err == nil {
		err = l.checkVersion(name, p)
	}
	if err != nil {
		// A plugin that loaded but does not satisfy its constraint is shut down
		// rather than left running: the command is going to fail, and leaving a
		// child process behind to be reaped at exit is how a refusal becomes a
		// hang on a plugin that ignores stdin closing.
		if p != nil {
			_ = p.Close()
		}
		l.failed[name] = err
		l.order = append(l.order, name)
		return nil, err
	}
	l.loaded[name] = p
	return p, nil
}

// open finds and connects one plugin: a binary if there is one, otherwise a
// builtin, otherwise an error saying everywhere it looked.
func (l *Loader) open(ctx context.Context, name string) (*Plugin, error) {
	path, searched, findErr := Find(name, l.Search)
	if findErr == nil {
		// BEFORE THE PROCESS STARTS, because after it has started is after its
		// code has run. Section 31.3: the host verifies on every launch once a
		// lock exists.
		if err := l.checkLock(name, path); err != nil {
			return nil, err
		}
		if l.Verbose != nil {
			fmt.Fprintf(l.Verbose, "[infrena] plugin %s loaded from %s\n", name, path)
		}
		return Launch(ctx, name, path, l.Dir, l.Verbose)
	}

	if builtin, ok := l.Builtin[name]; ok {
		if l.Verbose != nil {
			fmt.Fprintf(l.Verbose, "[infrena] plugin %s served in process (no binary found)\n", name)
		}
		return InProcess(ctx, builtin, l.Dir)
	}

	_ = searched
	return nil, findErr
}

// checkLock refuses a binary that is not the one plugins.lock recorded.
//
// NO NETWORK AND ALMOST NO COST. The lock is read once per command, and the
// only work per plugin is hashing a file that is about to be read and executed
// anyway - and only when the lock has something to say about that plugin.
//
// A BUILTIN IS NOT CHECKED, because there is no file: it is served from inside
// this process, so nothing was fetched and nothing can have been replaced. The
// check belongs on the path where a binary came from somewhere else.
func (l *Loader) checkLock(name, path string) error {
	lock, err := l.lockfile()
	if err != nil {
		// A LOCK NOBODY CAN READ REFUSES, rather than being treated as absent.
		// This file is what says which executables are trustworthy, and reading
		// it wrong in the permissive direction turns a corrupt file into a
		// silent loss of the only check there is.
		return err
	}
	if lock == nil {
		return nil
	}

	// Lockfile.Check is the authority on what an absent entry means, and it
	// says a plugin the lock does not mention passes - a hand-placed binary
	// keeps working. Asking first is only so a project that installed one
	// plugin does not hash the other nine on every command; the two answers
	// must stay the same, and this is the line to change if Check's ever does.
	if _, recorded := lock.Plugins[name]; !recorded {
		return nil
	}

	sum, err := plugins.FileChecksum(path)
	if err != nil {
		return err
	}
	if err := lock.Check(name, plugins.PlatformKey(), sum); err != nil {
		return fmt.Errorf("%w\n\nThe binary checked was %s.", err, path)
	}
	return nil
}

// lockfile reads plugins.lock once and remembers the answer, error included.
//
// Called with mu held, from load. A command that loads four plugins reads the
// file once; a project with no lock at all still reads it once, which is one
// stat of a file that is not there.
func (l *Loader) lockfile() (*plugins.Lockfile, error) {
	if !l.lockRead {
		l.lock, l.lockErr = plugins.ReadLockfile(l.Dir)
		l.lockRead = true
	}
	return l.lock, l.lockErr
}

// checkVersion refuses a plugin outside the range the project accepts.
//
// A plugin that reports no version at all reports 0.0.0 — the SDK's answer when a
// plugin does not implement Version() — and cannot satisfy any constraint above it.
// That is the right answer and needs its own message: "0.0.0 does not satisfy >= 0.3.0"
// would send an author looking for a version they never set.
//
// Note the ASYMMETRY with infrena's own `infrena:` floor (§61.2), which EXEMPTS a
// 0.0.0 build. There the unversioned binary is the user's own development build and a
// complaint about it is not something they can act on. Here it is a third-party plugin
// they chose to install, and they can act: install a versioned build, or drop the
// constraint.
func (l *Loader) checkVersion(name string, p *Plugin) error {
	constraint, ok := l.Constraints[name]
	if !ok || constraint.IsZero() {
		return nil
	}
	reported := p.Version()
	got, err := semver.Parse(reported)
	if err != nil {
		return &VersionError{Plugin: name, Reported: reported, Constraint: constraint, Path: p.client.Path()}
	}
	if constraint.Allows(got) {
		return nil
	}
	return &VersionError{
		Plugin:      name,
		Reported:    reported,
		Unversioned: got.Major == 0 && got.Minor == 0 && got.Patch == 0 && reported == "0.0.0",
		Constraint:  constraint,
		Path:        p.client.Path(),
	}
}

// Resolved reports where a plugin was loaded from and the version it reported,
// for a plugin this loader has already loaded.
//
// For `infrena plugins list`, which answers "what am I actually running". It
// reads what was OPENED rather than searching again, because a fresh search
// answers a subtly different question — where a binary would be found now — and
// two implementations of the same answer drift.
//
// path is empty for a builtin, which is served in this process and has no
// binary. ok is false for a plugin that has not been loaded, or whose load
// failed.
func (l *Loader) Resolved(name string) (path, version string, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	p, loaded := l.loaded[name]
	if !loaded {
		return "", "", false
	}
	return p.client.Path(), p.Version(), true
}

// Missing names every plugin this loader was asked for and could not find, in
// the order it was asked.
//
// For section 31.3's offer to install one. It reports ONLY the plugins whose
// binary was nowhere - a plugin that was found and failed its version
// constraint, or that crashed on startup, is a different problem with a
// different answer, and offering to install one that is already installed would
// be advice the user cannot act on.
//
// It reads what was ASKED FOR rather than searching again: the question is
// which plugins this command needed and did not get, and configuration is what
// decides that.
func (l *Loader) Missing() []string {
	l.mu.Lock()
	defer l.mu.Unlock()

	var out []string
	for _, name := range l.order {
		if err, failed := l.failed[name]; failed {
			if _, ok := errors.AsType[*NotFoundError](err); ok {
				out = append(out, name)
			}
		}
	}
	return out
}

// Close shuts down every plugin this loader started.
//
// Best effort and never fails the command: the work is already done by the time
// this runs, and a plugin that will not exit cleanly is not a reason to report an
// apply as failed.
func (l *Loader) Close() {
	l.mu.Lock()
	loaded := l.loaded
	l.loaded = map[string]*Plugin{}
	l.mu.Unlock()

	names := make([]string, 0, len(loaded))
	for name := range loaded {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		_ = loaded[name].Close()
	}
}

// DefaultSearch is where plugins are looked for, given a project directory.
func DefaultSearch(dir string, explicit []string) SearchOptions {
	opts := SearchOptions{ProjectDir: dir}
	opts.Explicit = append(opts.Explicit, explicit...)
	if env := os.Getenv("INFRENA_PLUGIN_PATH"); env != "" {
		opts.Explicit = append(opts.Explicit, splitPathList(env)...)
	}
	if home, err := os.UserHomeDir(); err == nil {
		opts.HomeDir = home
	}
	return opts
}

// splitPathList splits an INFRENA_PLUGIN_PATH the way the platform lists paths,
// dropping empty entries so a trailing separator is not a search of "".
func splitPathList(s string) []string {
	var out []string
	for _, part := range filepath.SplitList(s) {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// Available names every plugin that could be loaded: the binaries on the search
// path, plus the builtins.
//
// For DISCOVERY, which is the one command whose scope configuration does not set.
// Every other command knows which plugins it needs because the project says so — a
// `providers:` entry, or a resource type's prefix — but `infrena discover` asks what
// EXISTS, including resources no configuration mentions, so the only sensible scope
// is everything available to ask.
func (l *Loader) Available() []string {
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}

	for _, dir := range l.Search.dirs() {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if name, ok := pluginNameOf(e.Name()); ok {
				add(name)
			}
		}
	}
	for name := range l.Builtin {
		add(name)
	}
	sort.Strings(out)
	return out
}

// pluginNameOf recovers a plugin's name from its binary's filename.
func pluginNameOf(filename string) (string, bool) {
	const prefix = "infrena-plugin-"
	name := strings.TrimSuffix(filename, ".exe")
	if !strings.HasPrefix(name, prefix) {
		return "", false
	}
	name = strings.TrimPrefix(name, prefix)
	return name, name != ""
}
