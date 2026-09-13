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

	"github.com/infrata/infrata/pkg/provider"
)

// Loader turns a plugin NAME into a running plugin.
//
// It exists because nothing should have to be told which plugins to load:
// configuration already says. A `providers:` entry naming `plugin: aws` is the
// instruction to go and find infrata-plugin-aws, and a resource of type
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

	// Builtin is a fallback for plugins that have no binary yet.
	//
	// TRANSITIONAL. The fake provider still lives inside this module while
	// infrata-provider-fake is built; registering it here keeps every existing
	// project working without a second code path, because a builtin is served over
	// pluginhost.InProcess and therefore goes through the same protocol, the same
	// handshake and the same trust rules a subprocess does. Delete this field, and
	// the entry in internal/cli, once the binary ships.
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
	if err != nil {
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
			fmt.Fprintf(l.Verbose, "[infrata] plugin %s loaded from %s\n", name, path)
		}
		return Launch(ctx, name, path, l.Dir, l.Verbose)
	}

	if builtin, ok := l.Builtin[name]; ok {
		if l.Verbose != nil {
			fmt.Fprintf(l.Verbose, "[infrata] plugin %s served in process (no binary found)\n", name)
		}
		return InProcess(ctx, builtin, l.Dir)
	}

	_ = searched
	return nil, findErr
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
	if env := os.Getenv("INFRATA_PLUGIN_PATH"); env != "" {
		opts.Explicit = append(opts.Explicit, splitPathList(env)...)
	}
	if home, err := os.UserHomeDir(); err == nil {
		opts.HomeDir = home
	}
	return opts
}

// splitPathList splits an INFRATA_PLUGIN_PATH the way the platform lists paths,
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
// `providers:` entry, or a resource type's prefix — but `infrata discover` asks what
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
	const prefix = "infrata-plugin-"
	name := strings.TrimSuffix(filename, ".exe")
	if !strings.HasPrefix(name, prefix) {
		return "", false
	}
	name = strings.TrimPrefix(name, prefix)
	return name, name != ""
}
