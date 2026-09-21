package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/infrena/infrena/internal/backendhost"
	"github.com/infrena/infrena/internal/config"
	"github.com/infrena/infrena/internal/pluginhost"
	"github.com/infrena/infrena/internal/plugins"
	"github.com/infrena/infrena/internal/plugins/remote"
	"github.com/infrena/infrena/internal/version"
	"github.com/infrena/infrena/pkg/pluginmanifest"
	"github.com/infrena/infrena/pkg/pluginproto"
)

// newPluginsCommand builds `infrena plugins`.
//
// `list` answers "what am I actually running" with no network at all. Reaching
// out happens only where a user asked for it: `plugins search`, `plugins
// install`, and one interactive prompt — never anywhere a plan or an apply can
// reach.
func newPluginsCommand(opts *GlobalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plugins",
		Short: "Inspect the plugins on this machine",
	}
	cmd.AddCommand(
		newPluginsListCommand(opts),
		newPluginsSearchCommand(opts),
		newPluginsInstallCommand(opts),
		newPluginsVerifyCommand(opts),
	)
	return cmd
}

// newPluginsListCommand reports every plugin that could be loaded, the version
// it reports and where it was loaded from.
func newPluginsListCommand(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:           "list",
		Short:         "List the installed plugins",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ro, closeRun, err := openRun(cmd, opts, "plugins list", "")
			if err != nil {
				return err
			}
			defer closeRun()

			loader := listLoader(opts)
			defer loader.Close()

			renderPluginList(cmd.Context(), ro, loader,
				opts.Dir, backendhost.Search(opts.Dir, opts.PluginDirs))
			return nil
		},
	}
}

// listLoader builds the loader `plugins list` reports through.
//
// No constraints, unlike every other command's loader. The project's `plugins:`
// floor decides whether a version is acceptable; this command is asked what is
// installed. A plugin that violates the constraint is exactly the one whose
// version the user is trying to read.
func listLoader(opts *GlobalOptions) *pluginhost.Loader {
	return &pluginhost.Loader{
		Search:  pluginhost.DefaultSearch(opts.Dir, opts.PluginDirs),
		Dir:     opts.Dir,
		Verbose: verboseWriter(opts),
		Builtin: builtinsFor(opts.Dir),
	}
}

// renderPluginList prints the table, or the sentence that stands in for it.
//
// Both kinds are listed, since both are installable. Which is which is read off
// the binary's name — `infrena-plugin-` is a provider, `infrena-backend-` is a
// backend — so no new state records it and nothing has to be started to find
// out.
func renderPluginList(ctx context.Context, ro *runOutput, loader *pluginhost.Loader, dir string, backendDirs []string) {
	names := loader.Available()
	backends := installedBackends(dir, backendDirs)
	if len(names) == 0 && len(backends) == 0 {
		// An answer, not a failure, and exit 0 says so: a fresh machine has
		// no plugins.
		fmt.Fprintln(ro.Out(), "No plugins installed.")
		fmt.Fprintln(ro.Out(), "Run `infrena plugins install <name>` to install one.")
		return
	}

	w := tabwriter.NewWriter(ro.Out(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tKIND\tVERSION\tPATH")
	for _, name := range names {
		version, path := pluginRow(ctx, loader, name)
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", name, plugins.RoleProvider, version, path)
	}
	for _, b := range backends {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", b.name, plugins.RoleBackend, b.version, b.path)
	}
	_ = w.Flush()
}

// backendRow is one installed state backend, as list reports it.
type backendRow struct{ name, version, path string }

// installedBackends finds the backend binaries on the same search path a
// provider is looked for on, first directory wins, exactly as backendhost.find
// resolves one.
//
// The version comes from plugins.lock rather than from a handshake, which is
// the one place this row differs from a provider's: a backend's handshake is
// behind a Configure call carrying the project's `backend:` block, so starting
// one just to read a version would start it without its configuration and
// report a working backend as broken. A backend put there by hand has no lock
// entry and says so.
func installedBackends(dir string, dirs []string) []backendRow {
	versions := map[string]string{}
	if lf, err := plugins.ReadLockfile(dir); err == nil && lf != nil {
		for name, entry := range lf.Plugins {
			versions[name] = entry.Version
		}
	}

	seen := map[string]bool{}
	var out []backendRow
	for _, d := range dirs {
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name, ok := backendNameOf(e.Name())
			if !ok || seen[name] {
				continue
			}
			seen[name] = true
			version, ok := versions[backendhost.LockKey(name)]
			if !ok {
				// Installed by hand, or by a build that did not write a lock.
				// Saying so beats printing a version nothing vouches for.
				version = "unknown"
			}
			out = append(out, backendRow{name: name, version: version, path: filepath.Join(d, e.Name())})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// backendNameOf recovers a backend's name from its binary's filename, the
// mirror of pluginhost's pluginNameOf.
func backendNameOf(filename string) (string, bool) {
	name := strings.TrimSuffix(filename, ".exe")
	name, ok := strings.CutPrefix(name, "infrena-backend-")
	return name, ok && name != ""
}

// pluginRow loads a plugin and reads back what it turned out to be.
//
// A version comes from the handshake, so there is no way to report it without
// starting the plugin. Still no network — starting a binary that is already on
// disk is not a request.
func pluginRow(ctx context.Context, loader *pluginhost.Loader, name string) (version, path string) {
	if _, err := loader.Load(ctx, name); err != nil {
		// Listed anyway: a plugin that will not start is the most important
		// row in this table.
		return "unavailable", "-"
	}
	path, version, ok := loader.Resolved(name)
	if !ok {
		return "unavailable", "-"
	}
	if version == "" {
		// The SDK's answer for a plugin that does not implement Version().
		version = "0.0.0"
	}
	if path == "" {
		// A builtin is served inside this process and has no binary. Saying
		// so beats an empty cell a reader would take for a bug.
		path = "(in process)"
	}
	return version, path
}

// githubAPIVar redirects the forge client at another host.
//
// A test seam, not a feature: it points the tests at an httptest server instead
// of api.github.com. It is deliberately undocumented and not a flag, because
// supporting another forge is a source-syntax question — a source is
// host-prefixed precisely so a second forge can be added — not an
// environment-variable one. It overrides the raw-file host too, so one test
// server answers both.
const githubAPIVar = "INFRENA_GITHUB_API"

// newPluginsSearchCommand builds `infrena plugins search <name>`: every plugin
// of that name across every source the user trusts or the project names, which
// of them can run here, and why each of the others cannot.
//
// It never picks. Two owners can publish one name and both are printed, because
// choosing silently would hand the user code from a place they did not choose.
//
// Nothing found is an answer and exits 0. So is a plugin that exists but
// publishes no build for this machine, which is printed with its reason rather
// than dropped.
func newPluginsSearchCommand(opts *GlobalOptions) *cobra.Command {
	var refresh bool

	cmd := &cobra.Command{
		Use:           "search <name>",
		Short:         "Find a plugin by name across every source",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ro, closeRun, err := openRun(cmd, opts, "plugins search", "")
			if err != nil {
				return err
			}
			defer closeRun()

			name := args[0]
			sources, untrusted, err := searchSources(opts.Dir, name)
			if err != nil {
				return err
			}

			fetcher := searchFetcher(refresh)
			found, problems := plugins.Search(cmd.Context(), fetcher,
				sources, name, searchEnvironment(opts.Dir, name))

			// Diagnostics go to stderr, which --output does not silence: a
			// source that could not be searched has to reach the operator
			// whether or not a frontend is reading the file.
			renderSearchWarnings(cmd.ErrOrStderr(), problems, found)
			renderSearch(ro.Out(), name, found, untrusted,
				len(problems) > 0, fetcher.authenticated())
			return nil
		},
	}

	// --refresh is spelled as a cache TTL of zero rather than as a flag the
	// cache knows about: an entry that is never fresh is exactly what "ask
	// again" means, and it keeps the staleness rule in one place.
	cmd.Flags().BoolVar(&refresh, "refresh", false,
		"ignore cached answers and ask the forge again")
	return cmd
}

// searchSources is every place this search looks: the sources the user trusts,
// plus the one the project names for this plugin when it names one. It also
// returns the set of source strings that are untrusted, so the table can say
// which.
//
// A project-named source is a candidate, not a permission. infrena.yml is
// checked into git and travels to whoever clones it, so a source named there is
// searched and rendered as untrusted, and installing from it takes an explicit
// confirmation.
func searchSources(dir, name string) ([]plugins.Source, map[string]bool, error) {
	// No config directory is not a failed search. os.UserConfigDir fails
	// outright with neither HOME nor XDG_CONFIG_HOME, which is an ordinary
	// minimal container or CI runner, and refusing there would deny the user
	// even the official owner — a source trusted whatever the file says,
	// precisely so it cannot be configured away. A missing directory therefore
	// degrades like a missing file; a malformed file is still an error.
	home, err := os.UserConfigDir()
	if err != nil {
		home = ""
	}
	trusted, err := plugins.LoadTrusted(home)
	if err != nil {
		return nil, nil, err
	}

	untrusted := map[string]bool{}
	named, ok := projectPluginSource(dir, name)
	if !ok {
		return trusted, untrusted, nil
	}

	src, err := plugins.ParseSource(named)
	if err != nil {
		return nil, nil, fmt.Errorf("this project names the source %q for the plugin %q, which is not usable: %w", named, name, err)
	}
	for _, t := range trusted {
		if coversSource(t, src) {
			return trusted, untrusted, nil
		}
	}
	untrusted[src.String()] = true
	return append(trusted, src), untrusted, nil
}

// coversSource reports whether a trusted source already covers another. A
// trusted owner covers every repository under it, which is the point of
// trusting an owner rather than a list of repositories.
func coversSource(trusted, src plugins.Source) bool {
	if trusted.String() == src.String() {
		return true
	}
	return trusted.Kind == plugins.KindOwner &&
		trusted.Host == src.Host && trusted.Owner == src.Owner
}

// projectPluginSource reads the source the project names for one plugin.
//
// Decoding errors are ignored, as pluginConstraints already documents: a
// project whose configuration will not decode has a worse problem than an
// unsearched source, and reporting it from here would give the reader two
// problems to reconcile instead of one.
func projectPluginSource(dir, name string) (string, bool) {
	files, err := config.Load(dir)
	if err != nil {
		return "", false
	}
	decl, ds := config.Decode(files)
	if ds.HasErrors() {
		return "", false
	}
	entry, ok := decl.Plugins[name]
	if !ok || entry.Source == "" {
		return "", false
	}
	return entry.Source, true
}

// searchEnvironment describes the machine and the build that is asking, plus
// whatever the project already said about acceptable versions of this plugin.
//
// Read from this process, because the question `plugins search` answers is "can
// I use this here", and here is this binary on this machine.
func searchEnvironment(dir, name string) plugins.Environment {
	env := plugins.Environment{
		Platform:       pluginmanifest.Platform{OS: runtime.GOOS, Arch: runtime.GOARCH},
		Protocols:      pluginproto.Supported,
		InfrenaVersion: version.Version(),
	}
	// The project's `plugins:` floor for this plugin only, so a release the
	// project has already excluded is reported as excluded rather than as
	// usable.
	if c, ok := pluginConstraints(dir)[name]; ok {
		env.Constraint = c
	}
	return env
}

// searchFetcher builds the thing Search asks its questions of: the GitHub
// client, behind the disk cache.
//
// A cache failure is not a search failure: when the cache directory cannot even
// be named, the search runs without one rather than refusing.
func searchFetcher(refresh bool) *cachedFetcher {
	client := remote.NewClient()
	if base := os.Getenv(githubAPIVar); base != "" {
		client.BaseURL = base
		client.RawURL = base
	}

	f := &cachedFetcher{client: client}
	if dir, err := remote.DefaultCacheDir(); err == nil {
		ttl := remote.DefaultTTL
		if refresh {
			// Nothing is ever fresh, so every answer is asked for again and
			// the fresh one is stored on the way back.
			ttl = 0
		}
		f.cache = &remote.Cache{Dir: dir, TTL: ttl}
	}
	return f
}

// cachedFetcher serves a search from disk when it can, so a repeated search
// costs nothing from an allowance of sixty requests an hour.
//
// It is the only place the two halves meet: remote.Client makes requests and
// remote.Cache stores bytes, and neither knows about the other.
type cachedFetcher struct {
	client *remote.Client
	// cache is nil when there is nowhere to put one, and every method still
	// works.
	cache *remote.Cache
}

// authenticated reports whether a forge token was found, which decides what an
// empty result means: without one, a private repository answers a listing with
// an empty array, so nothing found is not the same as nothing existing.
func (f *cachedFetcher) authenticated() bool {
	return f.client.Token != ""
}

// Repositories lists an owner's repositories, from the cache when it is fresh.
func (f *cachedFetcher) Repositories(ctx context.Context, owner string) ([]string, error) {
	key := f.key("repos", owner)
	if raw, ok := f.get(key); ok {
		var names []string
		if json.Unmarshal(raw, &names) == nil {
			return names, nil
		}
		// A cached entry that will not decode is a miss, never an error:
		// nothing on this path may fail a command.
	}

	names, err := f.client.Repositories(ctx, owner)
	if err != nil {
		return nil, err
	}
	if raw, err := json.Marshal(names); err == nil {
		f.put(key, raw)
	}
	return names, nil
}

// LatestTag resolves a repository's newest release tag.
func (f *cachedFetcher) LatestTag(ctx context.Context, owner, repo string) (string, error) {
	key := f.key("tag", owner, repo)
	if raw, ok := f.get(key); ok {
		return string(raw), nil
	}

	tag, err := f.client.LatestTag(ctx, owner, repo)
	if err != nil {
		return "", err
	}
	f.put(key, []byte(tag))
	return tag, nil
}

// FileAtTag reads one file at a tag. The answer expires with everything else
// rather than being kept forever: a tag can be moved, and one TTL for the whole
// cache is a rule with no exceptions to get wrong.
func (f *cachedFetcher) FileAtTag(ctx context.Context, owner, repo, tag, path string) ([]byte, error) {
	key := f.key("file", owner, repo, tag, path)
	if raw, ok := f.get(key); ok {
		return raw, nil
	}

	data, err := f.client.FileAtTag(ctx, owner, repo, tag, path)
	if err != nil {
		return nil, err
	}
	f.put(key, data)
	return data, nil
}

// key names a cached answer.
//
// The base URL is part of it, so a test seam pointing at a local server cannot
// read, or leave behind, an entry written against GitHub itself. NUL separates
// the parts because no owner, repository, tag or path can contain one.
//
// So is the credential. Without it, the empty listing an unauthenticated search
// got back — a private repository is invisible rather than absent — is replayed
// for the whole TTL to a search that can see, including the one the user runs
// immediately after setting the token infrena just told them to set. It also
// keeps one token's view of the forge out of another's.
func (f *cachedFetcher) key(parts ...string) string {
	key := f.client.BaseURL + "\x00" + credentialID(f.client.Token)
	for _, p := range parts {
		key += "\x00" + p
	}
	return key
}

// credentialID identifies the credential an answer was read with, without
// carrying it. A key becomes a filename on disk, so a token in a key is a token
// in a path, legible to anything that lists a directory and copied into every
// backup. A SHA-256 tells two tokens apart just as well and says nothing about
// either.
//
// No token gets its own marker rather than an empty string, and no hash can
// collide with it: a hash is hex, and this is not.
func credentialID(token string) string {
	if token == "" {
		return "anonymous"
	}
	sum := sha256.Sum256([]byte(token))
	return "token-" + hex.EncodeToString(sum[:])
}

// get reads a fresh entry, and misses when there is no cache at all.
func (f *cachedFetcher) get(key string) ([]byte, bool) {
	if f.cache == nil {
		return nil, false
	}
	return f.cache.Get(key)
}

// put stores an answer, best effort. A cache that cannot be written has no
// bearing on the answer the user asked for, so the failure is dropped.
func (f *cachedFetcher) put(key string, data []byte) {
	if f.cache == nil {
		return
	}
	_ = f.cache.Put(key, data)
}

// renderSearchWarnings prints what went wrong beside what was found.
//
// A source that could not be read is a warning, never a silence and never
// "nothing found": a rate limit rendered as an empty result tells a user their
// plugin does not exist and sends them to check a spelling that was right.
//
// Manifest-format warnings are printed too. They are not a reason to reject a
// plugin, but they explain anything surprising about the row they belong to.
func renderSearchWarnings(w io.Writer, problems []error, found []plugins.Candidate) {
	for _, err := range problems {
		fmt.Fprintf(w, "warning: %v\n", err)
	}
	for _, c := range found {
		for _, warning := range c.Warnings {
			fmt.Fprintf(w, "warning: %s/%s at %s: %s\n",
				c.Source.Host+"/"+c.Source.Owner, c.Repo, c.Tag, warning)
		}
	}
	if len(problems) > 0 {
		fmt.Fprintln(w, "Some sources could not be searched, so this answer may be incomplete.")
	}
}

// renderSearch prints the table, or the sentence that stands in for it.
//
// Order is presentation only. Search sorts by source and then newest version so
// two runs render identically; the first row is not a recommendation, and this
// function does not mark one.
func renderSearch(w io.Writer, name string, found []plugins.Candidate, untrusted map[string]bool, partial, authenticated bool) {
	if len(found) == 0 {
		renderNothingFound(w, name, partial, authenticated)
		return
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SOURCE\tKIND\tVERSION\tPROTOCOL\tSTATUS")
	for _, c := range found {
		source := c.Source.String()
		if untrusted[c.Source.String()] {
			// Named by the project, not trusted by the user.
			source += " (not trusted)"
		}
		status := "usable"
		if !c.Usable {
			status = c.Reason
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", source, c.Role, c.Manifest.Version, joinInts(c.Manifest.Protocol), status)
	}
	_ = tw.Flush()

	if len(found) > 1 {
		fmt.Fprintf(w, "\n%d sources publish a plugin named %q. infrena does not choose between them.\n",
			countSources(found), name)
	}
}

// renderNothingFound says a search found nothing, and says it differently when
// infrena could not actually see: "nothing published this" and "nobody would
// tell me" send a reader to completely different places.
//
// There are two ways of not seeing. A source that refused is the loud one, and
// it arrives as a warning. The quiet one is a search that ran without a token:
// a private repository answers an unauthenticated listing with 200 and an empty
// array, indistinguishable from an owner who publishes nothing. The standing
// rule is that infrena never reports what it could not see as what does not
// exist.
func renderNothingFound(w io.Writer, name string, partial, authenticated bool) {
	switch {
	case partial:
		fmt.Fprintf(w, "No plugin named %q in the sources that answered.\n", name)
		fmt.Fprintln(w, "Some sources could not be searched, so this is not the same as there being none; see the warnings above.")

	case !authenticated:
		fmt.Fprintf(w, "No plugin named %q was visible to this search, which ran without a github token.\n", name)
		fmt.Fprintln(w, "A private repository is invisible to an unauthenticated search, so this is not the same as there being none.")
		fmt.Fprintf(w, "Set %s (or %s) to a personal access token and search again.\n",
			remote.TokenVar, remote.GitHubTokenVar)
		fmt.Fprintln(w, "If it should be public, check the spelling, or add the owner that publishes it to the `sources:` list in your infrena plugins.yml.")

	default:
		fmt.Fprintf(w, "No plugin named %q in any source.\n", name)
		fmt.Fprintln(w, "Check the spelling, or add the owner that publishes it to the `sources:` list in your infrena plugins.yml.")
	}
}

// countSources is how many distinct places answered, which is the number worth
// quoting: one owner publishing two releases is not a choice between sources.
func countSources(found []plugins.Candidate) int {
	seen := map[string]bool{}
	for _, c := range found {
		seen[c.Source.String()] = true
	}
	return len(seen)
}
