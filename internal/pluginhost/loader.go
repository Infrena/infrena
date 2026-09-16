package pluginhost

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

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
