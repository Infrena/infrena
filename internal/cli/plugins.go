package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/infrena/infrena/internal/config"
	"github.com/infrena/infrena/internal/pluginhost"
	"github.com/infrena/infrena/internal/plugins"
	"github.com/infrena/infrena/internal/plugins/remote"
	"github.com/infrena/infrena/internal/version"
	"github.com/infrena/infrena/pkg/pluginmanifest"
	"github.com/infrena/infrena/pkg/pluginproto"
)

// newPluginsCommand builds `infrena plugins` (PLAN.md §31.3).
//
// `list` ANSWERS A QUESTION WITH NO NETWORK: what am I actually running. That
// is deliberate and is the rule the whole subsection is built on — searching
// happens in `plugins search` and `plugins install`, and in one interactive
// prompt, never anywhere a plan or an apply can reach.
//
// `search` is the one subcommand here that DOES reach the network, and it is
// where the rule is drawn: a user asking what exists somewhere else has asked
// for a request to be made, and nothing on the hot path asks that question.
func newPluginsCommand(opts *GlobalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plugins",
		Short: "Inspect the provider plugins on this machine",
	}
	cmd.AddCommand(newPluginsListCommand(opts), newPluginsSearchCommand(opts))
	return cmd
}

// newPluginsListCommand reports every plugin that could be loaded, the version
// it reports and where it was loaded from.
func newPluginsListCommand(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:           "list",
		Short:         "List the installed provider plugins",
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

			renderPluginList(cmd.Context(), ro, loader)
			return nil
		},
	}
}

// listLoader builds the loader `plugins list` reports through.
//
// NO CONSTRAINTS, unlike every other command's loader. The project's `plugins:`
// floor decides whether a version is ACCEPTABLE, and this command is asked what
// is INSTALLED. A plugin that violates the constraint is exactly the one whose
// version the user is trying to read, and refusing to load it would withhold
// the answer they came for.
func listLoader(opts *GlobalOptions) *pluginhost.Loader {
	return &pluginhost.Loader{
		Search:  pluginhost.DefaultSearch(opts.Dir, opts.PluginDirs),
		Dir:     opts.Dir,
		Verbose: verboseWriter(opts),
		Builtin: builtinsFor(opts.Dir),
	}
}

// renderPluginList prints the table, or the sentence that stands in for it.
func renderPluginList(ctx context.Context, ro *runOutput, loader *pluginhost.Loader) {
	names := loader.Available()
	if len(names) == 0 {
		// AN ANSWER, NOT A FAILURE, and exit 0 says so. A fresh machine has no
		// plugins, and telling someone in that position that something went
		// wrong is both untrue and unactionable.
		fmt.Fprintln(ro.Out(), "No plugins installed.")
		fmt.Fprintln(ro.Out(), "Run `infrena plugins install <name>` to install one.")
		return
	}

	w := tabwriter.NewWriter(ro.Out(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tVERSION\tPATH")
	for _, name := range names {
		version, path := pluginRow(ctx, loader, name)
		fmt.Fprintf(w, "%s\t%s\t%s\n", name, version, path)
	}
	_ = w.Flush()
}

// pluginRow loads a plugin and reads back what it turned out to be.
//
// LOADING IS HOW A VERSION IS LEARNED: it comes from the handshake, so there is
// no way to report it without starting the plugin. Still no network — starting
// a binary that is already on disk is not a request.
func pluginRow(ctx context.Context, loader *pluginhost.Loader, name string) (version, path string) {
	if _, err := loader.Load(ctx, name); err != nil {
		// Listed anyway. A plugin that will not start is the most important row
		// in this table, and omitting it would answer "what is installed" with
		// a list that does not mention the thing that is broken.
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
		// A builtin is served inside this process and has no binary. Saying so
		// is better than an empty cell a reader would take for a bug.
		path = "(in process)"
	}
	return version, path
}

// githubAPIVar redirects the forge client at another host.
//
// A TEST SEAM, NOT A FEATURE. It exists so this package's tests point at an
// httptest server instead of api.github.com, and it is deliberately not
// documented for users, not a flag, and not something to build a GitHub
// Enterprise story on: supporting another forge is a source-syntax question
// (§31.3 host-prefixes a source precisely so a second forge can be added), not
// an environment-variable one. It overrides the raw-file host too, so one test
// server answers both.
const githubAPIVar = "INFRENA_GITHUB_API"

// newPluginsSearchCommand builds `infrena plugins search <name>` (PLAN.md
// §31.3): every plugin of that name across every source the user trusts or the
// project names, which of them can run here, and why each of the others cannot.
//
// IT NEVER PICKS. Two owners can publish one name and both are printed, because
// choosing between them silently would hand the user code from a place they did
// not choose, and the place is the part that matters.
//
// NOTHING FOUND IS AN ANSWER AND EXITS 0. So is a plugin that exists but
// publishes no build for this machine, which is a different answer again and is
// printed with its reason rather than dropped.
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

			found, problems := plugins.Search(cmd.Context(), searchFetcher(refresh),
				sources, name, searchEnvironment(opts.Dir, name))

			// DIAGNOSTICS GO TO STDERR, which --output does not silence: a
			// source that could not be searched has to reach the operator
			// whether or not a frontend is reading the file.
			renderSearchWarnings(cmd.ErrOrStderr(), problems, found)
			renderSearch(ro.Out(), name, found, untrusted, len(problems) > 0)
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

// searchSources is every place this search looks: the sources the USER trusts,
// plus the one the PROJECT names for this plugin when it names one.
//
// A PROJECT-NAMED SOURCE IS A CANDIDATE, NOT A PERMISSION (§31.3). infra.yml is
// checked into git and travels to whoever clones it, so a source named there is
// searched and rendered as untrusted; Unit 3 is where installing from it takes
// an explicit confirmation. Returned alongside is the set of source strings that
// are untrusted, so the table can say which.
func searchSources(dir, name string) ([]plugins.Source, map[string]bool, error) {
	home, err := os.UserConfigDir()
	if err != nil {
		return nil, nil, fmt.Errorf("finding the user configuration directory: %w: set XDG_CONFIG_HOME or HOME, so infrena can read the plugin sources you trust", err)
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
// trusted OWNER covers every repository under it, which is the whole point of
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
// Decoding errors are IGNORED, the concession pluginConstraints already
// documents: a project whose configuration will not decode has a worse problem
// than an unsearched source, and reporting it from here would give the reader
// two problems to reconcile instead of one.
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
// Read from THIS process, because the question `plugins search` answers is "can
// I use this here" and here is this binary on this machine.
func searchEnvironment(dir, name string) plugins.Environment {
	env := plugins.Environment{
		Platform:       pluginmanifest.Platform{OS: runtime.GOOS, Arch: runtime.GOARCH},
		Protocols:      pluginproto.Supported,
		InfrenaVersion: version.Version(),
	}
	// The project's `plugins:` floor for THIS plugin only. A release the
	// project has already excluded is reported as excluded rather than as
	// usable, so the table answers the question the user is actually in the
	// middle of.
	if c, ok := pluginConstraints(dir)[name]; ok {
		env.Constraint = c
	}
	return env
}

// searchFetcher builds the thing Search asks its questions of: the GitHub
// client, behind the disk cache.
//
// A CACHE FAILURE IS NOT A SEARCH FAILURE. When the cache directory cannot even
// be named, the search runs without one rather than refusing: the user asked
// where a plugin is, and answering that with a message about a cache directory
// would break a command for a reason that has nothing to do with the question.
func searchFetcher(refresh bool) plugins.Fetcher {
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

// Repositories lists an owner's repositories, from the cache when it is fresh.
func (f *cachedFetcher) Repositories(ctx context.Context, owner string) ([]string, error) {
	key := f.key("repos", owner)
	if raw, ok := f.get(key); ok {
		var names []string
		if json.Unmarshal(raw, &names) == nil {
			return names, nil
		}
		// A cached entry that will not decode is a miss, never an error: see
		// remote.Cache on why nothing on this path may fail a command.
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

// FileAtTag reads one file at a tag.
//
// SAFE TO CACHE FOREVER in principle, because a tag is immutable in every
// workflow that matters, but it expires with everything else: a tag CAN be
// moved, and one TTL for the whole cache is a rule with no exceptions to get
// wrong.
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
// THE BASE URL IS PART OF IT. A test seam pointing at a local server must not
// be able to read, or leave behind, an entry written against GitHub itself, and
// including the host makes that impossible rather than unlikely. NUL separates
// the parts because no owner, repository, tag or path can contain one.
func (f *cachedFetcher) key(parts ...string) string {
	key := f.client.BaseURL
	for _, p := range parts {
		key += "\x00" + p
	}
	return key
}

// get reads a fresh entry, and misses when there is no cache at all.
func (f *cachedFetcher) get(key string) ([]byte, bool) {
	if f.cache == nil {
		return nil, false
	}
	return f.cache.Get(key)
}

// put stores an answer, best effort. A cache that cannot be written has no
// bearing on the answer the user asked for, so the failure is dropped rather
// than printed at someone who did not ask about a cache.
func (f *cachedFetcher) put(key string, data []byte) {
	if f.cache == nil {
		return
	}
	_ = f.cache.Put(key, data)
}

// renderSearchWarnings prints what went wrong beside what was found.
//
// A SOURCE THAT COULD NOT BE READ IS A WARNING, NEVER A SILENCE AND NEVER
// "NOTHING FOUND". A rate limit rendered as an empty result tells a user their
// plugin does not exist and sends them to check a spelling that was right. The
// errors arrive already naming the source or the repository they came from.
//
// Manifest-format warnings are printed too rather than swallowed: they are not
// a reason to reject a plugin, but they are the explanation for anything
// surprising about the row they belong to.
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
// ORDER IS PRESENTATION. Search sorts by source and then newest version so two
// runs render identically; the first row is not a recommendation and this
// function does not mark one.
func renderSearch(w io.Writer, name string, found []plugins.Candidate, untrusted map[string]bool, partial bool) {
	if len(found) == 0 {
		renderNothingFound(w, name, partial)
		return
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SOURCE\tVERSION\tPROTOCOL\tSTATUS")
	for _, c := range found {
		source := c.Source.String()
		if untrusted[c.Source.String()] {
			// Named by the project, not trusted by the user. Rendering it is
			// the whole reason it was searched at all.
			source += " (not trusted)"
		}
		status := "usable"
		if !c.Usable {
			status = c.Reason
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", source, c.Manifest.Version, joinInts(c.Manifest.Protocol), status)
	}
	_ = tw.Flush()

	if len(found) > 1 {
		fmt.Fprintf(w, "\n%d sources publish a plugin named %q. infrena does not choose between them.\n",
			countSources(found), name)
	}
}

// renderNothingFound says so, and says it differently when part of the search
// could not be done — because "nothing published this" and "nobody would tell
// me" send a reader to completely different places.
func renderNothingFound(w io.Writer, name string, partial bool) {
	if partial {
		fmt.Fprintf(w, "No plugin named %q in the sources that answered.\n", name)
		fmt.Fprintln(w, "Some sources could not be searched, so this is not the same as there being none; see the warnings above.")
		return
	}
	fmt.Fprintf(w, "No plugin named %q in any source.\n", name)
	fmt.Fprintln(w, "Check the spelling, or add the owner that publishes it to the `sources:` list in your infrena plugins.yml.")
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
