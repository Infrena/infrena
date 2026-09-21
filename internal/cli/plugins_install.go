package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/infrena/infrena/internal/backendhost"
	"github.com/infrena/infrena/internal/config"
	"github.com/infrena/infrena/internal/pluginhost"
	"github.com/infrena/infrena/internal/plugins"
	"github.com/infrena/infrena/internal/plugins/remote"
	"github.com/infrena/infrena/pkg/pluginmanifest"
)

// maxBinary bounds what is written to disk out of an archive. Real plugins are
// around nine megabytes unpacked, so this is far past anything plausible while
// still refusing an archive that expands until the disk is full. It is the same
// number remote caps a download at.
const maxBinary = 256 << 20

// newPluginsInstallCommand builds `infrena plugins install <name>[@version]`.
//
// The order of the steps is the design, and a reader changing this file needs
// it before they need any of the code:
//
//	search -> filter to what can run here -> refuse if nothing or more than one
//	-> check trust -> prompt only if a person is there -> fetch SHA256SUMS ->
//	download -> verify -> extract -> move into place -> write the lock
//
// Every step is a refusal rather than a repair, and a refusal at any of them
// leaves nothing on disk. Verification comes before extraction so a tampered
// archive is refused without being opened; the lock comes last because it
// records what is already there rather than what is intended.
//
// Two rules have no exceptions. A run with nobody at the terminal never
// approves a source, so a pipeline cannot quietly start trusting a new
// publisher of executables. And infrena never chooses between two sources
// answering one name, because the place code comes from is the part that
// matters.
func newPluginsInstallCommand(opts *GlobalOptions) *cobra.Command {
	var global bool
	var kind string

	cmd := &cobra.Command{
		Use:           "install [<name>[@version]]",
		Short:         "Download and install a plugin",
		Args:          cobra.MaximumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ro, closeRun, err := openRun(cmd, opts, "plugins install", "")
			if err != nil {
				return err
			}
			defer closeRun()

			// No name means the project's whole list, which is what a fresh
			// clone needs: there is no kind to resolve, so --kind has nothing
			// to say.
			if len(args) == 0 {
				if kind != "" {
					return fmt.Errorf("--kind has nothing to apply to: `infrena plugins install` with no name installs every plugin this project declares, and the project already says which kind each of them is.\n\nSuggested action:\n  Drop --kind, or name the one plugin you meant.")
				}
				return runInstallDeclared(cmd, opts, ro.Out(), global)
			}
			return runInstall(cmd, opts, ro.Out(), args[0], global, kind)
		},
	}

	cmd.Flags().BoolVar(&global, "global", false,
		"install for this user rather than into this project")
	cmd.Flags().StringVar(&kind, "kind", "",
		"which kind of plugin to install: provider or backend")
	return cmd
}

// runInstall is the whole sequence, in the order its doc comment sets out.
func runInstall(cmd *cobra.Command, opts *GlobalOptions, out io.Writer, arg string, global bool, kind string) error {
	name, wantVersion, err := parsePluginArg(arg)
	if err != nil {
		return err
	}

	// Read before a single request: answering a bad --kind after a search
	// would spend a rate limit to reach an error that was already true.
	var only []plugins.Role
	if kind != "" {
		role, err := parseRole(kind)
		if err != nil {
			return err
		}
		only = []plugins.Role{role}
	}

	// Where it will go is decided first, for the same reason: a download that
	// succeeds and then finds it has nowhere to put anything has spent a rate
	// limit to reach an error it could have reached instantly.
	dest, err := installDir(opts, global)
	if err != nil {
		return err
	}

	// 1. Search, across the sources the user trusts plus the one this project
	// names. Naming one is what gets it searched, not what gets it installed.
	in, err := newInstallation(cmd, opts, out, name, wantVersion, dest)
	if err != nil {
		return err
	}

	roles, err := in.roles(opts.Dir, only)
	if err != nil {
		return err
	}
	for _, role := range roles {
		if err := in.install(cmd, role); err != nil {
			return err
		}
	}
	return nil
}

// installation is one `plugins install` of one name: everything resolved before
// any kind of it is installed.
//
// One search, however many kinds come back. A project naming s3 as a provider
// and as a backend installs two binaries out of one set of answers; searching
// twice would spend two requests learning the same thing and could come back
// with two different answers.
type installation struct {
	opts    *GlobalOptions
	out     io.Writer
	name    string
	version string // the version asked for, empty for whatever is latest
	dest    string

	found     []plugins.Candidate
	untrusted map[string]bool
	// partial and authenticated are what an empty answer means: a source that
	// refused, and a search that could not see a private repository, are both
	// reported as what they are rather than as a missing plugin.
	partial       bool
	authenticated bool
}

// newInstallation searches, and reports what could not be searched.
func newInstallation(cmd *cobra.Command, opts *GlobalOptions, out io.Writer, name, version, dest string) (*installation, error) {
	sources, untrusted, err := searchSources(opts.Dir, name)
	if err != nil {
		return nil, err
	}
	fetcher := searchFetcher(false)
	found, problems := plugins.Search(cmd.Context(), fetcher, sources, name, searchEnvironment(opts.Dir, name))
	renderSearchWarnings(cmd.ErrOrStderr(), problems, found)

	return &installation{
		opts: opts, out: out, name: name, version: version, dest: dest,
		found: found, untrusted: untrusted,
		partial: len(problems) > 0, authenticated: fetcher.authenticated(),
	}, nil
}

// roles decides which kinds of this name to install, in the order the answer is
// cheapest and most certain:
//
//  1. --kind, if given. The user said so; nothing outranks that.
//  2. What the project declares. It already knows, and asking a user to
//     repeat something written in infrena.yml is asking them to keep two
//     places in step.
//  3. The search result, if only one kind answers the name.
//  4. Otherwise refuse, showing both and naming --kind. A kind is a candidate,
//     and infrena never auto-picks between candidates answering one name.
//
// Nothing found at all short-circuits the lot, and that refusal says whether
// infrena could not see rather than reporting the plugin as not there.
func (in *installation) roles(dir string, only []plugins.Role) ([]plugins.Role, error) {
	if len(in.found) == 0 {
		return nil, noSuchPlugin(in.name, in.partial, in.authenticated)
	}
	if len(only) > 0 {
		return only, nil
	}

	declared, err := declaredRoles(dir, in.name)
	if err != nil {
		return nil, err
	}
	if len(declared) > 0 {
		return declared, nil
	}

	published := rolesAmong(in.found)
	if len(published) == 1 {
		return published, nil
	}
	return nil, fmt.Errorf("both a provider and a backend are published under the name %q, and infrena does not choose between them.\n\n%s\nA provider serves resources; a backend stores state. There is no project here that names %s, so nothing says which you meant.\n\nSuggested action:\n  Pass --kind provider or --kind backend, or run this inside the project that needs it, where the `providers:` and `backend:` blocks already say.",
		in.name, describeCandidates(in.found), in.name)
}

// install is steps two to ten for one kind.
func (in *installation) install(cmd *cobra.Command, role plugins.Role) error {
	// 2. Filter, and 3. refuse if that leaves anything other than exactly one.
	// The kind filters first: a provider release is no answer to someone
	// installing a backend, and counting it would make two publishers out of
	// one.
	byRole := candidatesWithRole(in.found, role)
	if len(byRole) == 0 {
		if len(in.found) == 0 {
			return noSuchPlugin(in.name, in.partial, in.authenticated)
		}
		return fmt.Errorf("no %s named %q is published by any source that answered, so there is nothing of that kind to install.\n\nWhat is published under that name:\n%s\nSuggested action:\n  Install what exists, or check the spelling. A --kind is never silently swapped for the other one: they are different binaries doing different jobs.",
			role, in.name, describeCandidates(in.found))
	}
	chosen, err := soleCandidate(in.name, in.version, byRole, in.partial, in.authenticated)
	if err != nil {
		return err
	}

	// 4 and 5. Trust, and the prompt that is the only way to grant it.
	if err := ensureTrusted(cmd, in.opts, in.out, *chosen, in.untrusted); err != nil {
		return err
	}

	// 6 to 9. Checksums, the archive, the proof, and the one file taken out.
	client := installClient()
	stem, binary := releaseNames(role, in.name)
	if err := fetchAndInstall(cmd.Context(), client, *chosen, in.name, stem, binary, in.dest); err != nil {
		return err
	}

	// 10. The lock, written from what is now on disk rather than from what was
	// downloaded, so it records the file a later run will actually hash.
	path := filepath.Join(in.dest, binary)
	sum, err := plugins.FileChecksum(path)
	if err != nil {
		return err
	}
	if err := recordInstall(in.opts.Dir, cmd.ErrOrStderr(), lockKey(role, in.name), in.name, *chosen, sum); err != nil {
		return err
	}

	// The kind is always printed, because it is not always chosen by the
	// person reading this line: when the project resolved it, a choice was
	// made on their behalf and has to be visible.
	fmt.Fprintf(in.out, "Installed %s %s (%s) from %s\n  %s\n",
		in.name, chosen.Manifest.Version, role, chosen.Source, path)
	return nil
}

// candidatesWithRole keeps the releases of one kind.
func candidatesWithRole(found []plugins.Candidate, role plugins.Role) []plugins.Candidate {
	var out []plugins.Candidate
	for _, c := range found {
		if c.Role == role {
			out = append(out, c)
		}
	}
	return out
}

// releaseNames are the two names a release of one plugin has, and they differ
// by role: a provider ships infrena-plugin-<name>, a backend
// infrena-backend-<name>.
//
// The stem carries no extension and the binary does. A Windows release is
// infrena-backend-s3_1.0.0_windows_amd64.zip holding infrena-backend-s3.exe, so
// asking for the archive by the executable's name would ask for an asset that
// does not exist, and a 404 there reads as "no build for your machine".
func releaseNames(role plugins.Role, name string) (stem, binary string) {
	binary = pluginhost.BinaryName(name)
	if role == plugins.RoleBackend {
		binary = backendhost.BinaryName(name)
	}
	return strings.TrimSuffix(binary, ".exe"), binary
}

// lockKey is how this install is spelled in plugins.lock: a provider under its
// bare name, a backend under backendhost.LockKey, which is what `plugins list`
// reads a backend's version back from. One key for both would make a provider
// s3 and a backend s3 overwrite each other.
func lockKey(role plugins.Role, name string) string {
	if role == plugins.RoleBackend {
		return backendhost.LockKey(name)
	}
	return name
}

// runInstallDeclared is `infrena plugins install` with no name: everything this
// project declares, in one command.
//
// No kind resolution at all: the project says both the name and the kind for
// each one, so the ambiguity a named install has to resolve cannot arise here.
//
// One failure does not hide the others. Every plugin is attempted, each result
// is reported, and the command fails at the end if any of them did, the same
// rule discovery.Walk follows: a run that stopped at the first failure would
// leave a reader believing the plugins after it were fine.
func runInstallDeclared(cmd *cobra.Command, opts *GlobalOptions, out io.Writer, global bool) error {
	// The project is checked before the destination, because --global outside
	// a project has a perfectly good destination and still nothing to
	// enumerate.
	if !hasProject(opts.Dir) {
		return fmt.Errorf("no project in %s, so there is no list of plugins to install.\n\n`infrena plugins install` with no name installs every plugin a project's %s declares, and there is no %s here.\n\nSuggested action:\n  Run it inside a project, or name the plugin to install: `infrena plugins install <name>`.",
			opts.Dir, config.ProjectFileName, config.ProjectFileName)
	}

	declared, err := declaredPlugins(opts.Dir)
	if err != nil {
		return err
	}
	if len(declared) == 0 {
		// An answer, not a failure: a project that needs no plugins is a real
		// state, and `infrena init` produces one.
		fmt.Fprintf(out, "This project declares no plugins, so there is nothing to install.\n")
		return nil
	}

	dest, err := installDir(opts, global)
	if err != nil {
		return err
	}

	var failures []error
	for _, d := range declared {
		if path, ok := installedAlready(opts, d.role, d.name); ok {
			fmt.Fprintf(out, "%s (%s) is already installed\n  %s\n", d.name, d.role, path)
			continue
		}
		if err := installDeclared(cmd, opts, out, d, dest); err != nil {
			// Named here, because the error underneath is about one plugin
			// and the reader is looking at a list of several.
			failures = append(failures, fmt.Errorf("%s (%s): %w", d.name, d.role, err))
			fmt.Fprintf(cmd.ErrOrStderr(), "failed  %s (%s)\n", d.name, d.role)
		}
	}
	return errors.Join(failures...)
}

// installDeclared installs one entry off the project's list, down the same path
// a named install takes. A second way to install a plugin would be a second set
// of rules about what may be installed.
func installDeclared(cmd *cobra.Command, opts *GlobalOptions, out io.Writer, d declaredPlugin, dest string) error {
	in, err := newInstallation(cmd, opts, out, d.name, "", dest)
	if err != nil {
		return err
	}
	return in.install(cmd, d.role)
}

// installedAlready reports whether this plugin's binary is already somewhere the
// loader looks, and where.
//
// It looks at the binary on disk, not the lock and not the builtins: the lock
// records what an install wrote and would call a deleted binary present, and a
// builtin is served in process and is not a thing a project can be missing.
func installedAlready(opts *GlobalOptions, role plugins.Role, name string) (string, bool) {
	_, binary := releaseNames(role, name)
	// The same directories for both kinds; only the binary's name differs.
	for _, dir := range backendhost.Search(opts.Dir, opts.PluginDirs) {
		if dir == "" {
			continue
		}
		path := filepath.Join(dir, binary)
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, true
		}
	}
	return "", false
}

// parsePluginArg splits `name` or `name@version`.
func parsePluginArg(arg string) (name, version string, err error) {
	name, version, found := strings.Cut(arg, "@")
	if name == "" || (found && version == "") {
		return "", "", fmt.Errorf("%q is not a plugin to install: write a name, such as `aws`, or a name and a version, such as `aws@0.4.0`", arg)
	}
	return name, version, nil
}

// installDir is where the binary will be written.
//
// Both destinations are directories pluginhost.DefaultSearch already searches,
// so install puts plugins where the loader was already looking. Nothing is
// registered, and a hand-placed binary in a higher-precedence directory keeps
// winning, which is what keeps a plugin author's --plugin-dir working the day
// after an install.
//
// Without --global the project is the default, so a project's plugins travel
// with its lock file. A run outside a project refuses rather than writing an
// executable into the .infrena/plugins of whatever directory someone happened to
// be standing in.
func installDir(opts *GlobalOptions, global bool) (string, error) {
	if global {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot install for this user: %w; infrena has no home directory to install into, so run the command inside a project instead", err)
		}
		return filepath.Join(home, ".local", "share", "infrena", "plugins"), nil
	}
	if !hasProject(opts.Dir) {
		return "", fmt.Errorf("no project found in %s, so there is nowhere in it to install a plugin.\n\nSuggested action:\n  Run this inside a project, or pass --global to install for this user instead.", opts.Dir)
	}
	return filepath.Join(opts.Dir, ".infrena", "plugins"), nil
}

// hasProject reports whether dir is a project root. resolveProjectRoot has
// already pointed opts.Dir at one when there was one to find, so this is
// reading the result of that search rather than repeating it.
func hasProject(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, config.ProjectFileName))
	return err == nil && !info.IsDir()
}

// soleCandidate reduces a search to the one release that will be installed, or
// explains why there is no such thing.
//
// It never picks. Two sources answering one name end here, with both printed
// and neither installed, because choosing silently hands a user code from a
// place they did not choose. Being first in the search results is not being
// chosen: Search sorts for stable rendering only.
//
// Nothing found is not the same as nothing existing. A source that refused, and
// a search that ran without a token and so could not see a private repository,
// are both reported as what they are rather than as a missing plugin.
func soleCandidate(name, wantVersion string, found []plugins.Candidate, partial, authenticated bool) (*plugins.Candidate, error) {
	if len(found) == 0 {
		return nil, noSuchPlugin(name, partial, authenticated)
	}

	// A version is part of the question, not a choice between answers: it
	// narrows what was asked for before anything is selected, so asking for a
	// version nobody publishes is answered with what is published.
	if wantVersion != "" {
		var matching []plugins.Candidate
		for _, c := range found {
			if c.Manifest.Version.String() == wantVersion {
				matching = append(matching, c)
			}
		}
		if len(matching) == 0 {
			return nil, fmt.Errorf("no source publishes %s %s.\n\n%s\nA search reads each repository's latest release, so a version older than the latest one is not installable this way.\n\nSuggested action:\n  Install one of the versions above, or ask the publisher to release %s.",
				name, wantVersion, describeCandidates(found), wantVersion)
		}
		found = matching
	}

	var usable []plugins.Candidate
	for _, c := range found {
		if c.Usable {
			usable = append(usable, c)
		}
	}
	switch {
	case len(usable) == 0:
		return nil, fmt.Errorf("no release of %s can run here.\n\n%s\nSuggested action:\n  Ask the publisher for a build that matches, or upgrade infrena if the reason is a protocol or version floor.",
			name, describeCandidates(found))

	case len(usable) > 1:
		return nil, fmt.Errorf("%d sources publish a plugin named %q, and infrena does not choose between them.\n\n%s\nSuggested action:\n  Name the one you mean in this project's `plugins:` block, or remove the source you did not mean from your infrena plugins.yml, then install again.",
			countSources(usable), name, describeCandidates(usable))
	}
	return &usable[0], nil
}

// noSuchPlugin says nothing was found, and says it differently when infrena
// could not actually see — the same three-way distinction `plugins search`
// draws, because an install that reports a rate limit as a missing plugin sends
// a user to check a spelling that was right.
func noSuchPlugin(name string, partial, authenticated bool) error {
	switch {
	case partial:
		return fmt.Errorf("no plugin named %q in the sources that answered, and some sources could not be searched.\n\nThat is not the same as there being none; see the warnings above.\n\nSuggested action:\n  Fix whatever refused, then install again.", name)
	case !authenticated:
		return fmt.Errorf("no plugin named %q was visible to this search, which ran without a github token.\n\nA private repository is invisible to an unauthenticated search, so this is not the same as there being none.\n\nSuggested action:\n  Set %s (or %s) to a personal access token and install again, or check the spelling.",
			name, remote.TokenVar, remote.GitHubTokenVar)
	default:
		return fmt.Errorf("no plugin named %q in any source.\n\nSuggested action:\n  Check the spelling, or add the owner that publishes it to the `sources:` list in your infrena plugins.yml.", name)
	}
}

// describeCandidates lists what was found, one per line, with the kind of each
// and the reason a release cannot run here, which is what turns the refusals
// above into something a reader can act on.
//
// The kind is part of the identity, not decoration: two rows for one name from
// one owner are a provider and a backend, and without the word they read as the
// same thing published twice.
func describeCandidates(found []plugins.Candidate) string {
	var b strings.Builder
	for _, c := range found {
		fmt.Fprintf(&b, "  %s  %s  %s", c.Source, c.Role, c.Manifest.Version)
		if !c.Usable {
			fmt.Fprintf(&b, "  (%s)", c.Reason)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// ensureTrusted is the security boundary the whole command is built around.
//
// A project may name a source; only a user may trust one. infrena.yml travels
// with a `git clone`, so a source named there is searched and shown but is not
// a grant — otherwise cloning a repository would let it introduce a place
// infrena fetches executables from, and `infrena plan` would download one.
//
// A non-interactive run never approves, and both ways of having nobody there
// are handled: --output means a frontend is reading a file and no human is
// watching stdout, and end of input means nothing was there to answer. Either
// way the answer is an error naming the owner and how to approve it, never a
// default of yes and never a block on input that cannot come.
func ensureTrusted(cmd *cobra.Command, opts *GlobalOptions, out io.Writer, c plugins.Candidate, untrusted map[string]bool) error {
	if !untrusted[c.Source.String()] {
		return nil
	}

	// approvalUnobtainable is apply's own test for the same condition, reused
	// rather than restated so the two commands cannot drift into disagreeing
	// about when there is nobody to ask.
	if approvalUnobtainable(opts) {
		return untrustedError(c)
	}

	prompt := fmt.Sprintf(
		"\n%s publishes %s %s, and you have not approved that source.\n"+
			"Installing runs its code on this machine. Approving is remembered, and covers this source from now on.\n"+
			"Type yes to approve and install: ",
		c.Source, c.Manifest.Name, c.Manifest.Version)

	switch confirm(cmd.InOrStdin(), out, prompt, "yes") {
	case approvalNoInput:
		return untrustedError(c)
	case approvalDeclined:
		return fmt.Errorf("install cancelled: %s was not approved, and nothing has been downloaded", c.Source)
	}

	// Recorded in the user's own file, so it is asked once. A machine with no
	// config directory cannot record one, and Approve says so rather than
	// trusting the source for this run alone.
	home, err := os.UserConfigDir()
	if err != nil {
		home = ""
	}
	return plugins.Approve(home, c.Source)
}

// untrustedError is the refusal. It names the owner and the way to approve it,
// because a refusal that says neither leaves the reader stuck.
func untrustedError(c plugins.Candidate) error {
	return fmt.Errorf("%s %s is published by %s, which you have not approved.\n\nA project may name a source, but naming one is not permission to download an executable from it: project configuration travels with a `git clone`, so only you can trust a place binaries come from. A run with nobody at the terminal never approves one.\n\nSuggested action:\n  Run `infrena plugins install %s` in a terminal and approve %s when asked, or add %s to the `sources:` list in your infrena plugins.yml.",
		c.Manifest.Name, c.Manifest.Version, c.Source, c.Manifest.Name, c.Source.Owner, c.Source)
}

// installClient is the forge client install downloads through, pointed at the
// test seam when one is set, exactly as searchFetcher is.
func installClient() *remote.Client {
	client := remote.NewClient()
	if base := os.Getenv(githubAPIVar); base != "" {
		client.BaseURL = base
		client.RawURL = base
	}
	return client
}

// fetchAndInstall is steps six to nine: the checksums, the archive, the proof,
// and the single file taken out of it.
//
// Verify before extract, which is why this is one function rather than a
// download helper and an extract helper a future caller could put in the other
// order. A tampered archive is refused without being opened.
func fetchAndInstall(ctx context.Context, client *remote.Client, c plugins.Candidate, name, stem, binary, dest string) error {
	owner, repo, tag := c.Source.Owner, c.Repo, c.Tag

	// 6. The checksums come from the release, never the manifest, which is
	// committed before the archives exist. A release without a SHA256SUMS
	// cannot be installed, and Checksums says so.
	sums, err := client.Checksums(ctx, owner, repo, tag)
	if err != nil {
		return err
	}

	// The manifest's version names the asset, not the tag: a release strips
	// the leading v, so the tag is v0.4.0 while the archive is
	// infrena-plugin-aws_0.4.0_linux_amd64.tar.gz. Using the tag here asks for
	// an asset that does not exist, and a 404 on this path would read as "this
	// plugin publishes no build for your machine".
	//
	// The binary names it too, which is why the stem is passed in rather than
	// built from the plugin's name: a backend's archive is
	// infrena-backend-s3_1.0.0_linux_amd64.tar.gz, and no backend release
	// publishes an infrena-plugin- asset.
	asset := remote.AssetName(stem, c.Manifest.Version.String(), currentPlatform())
	want, ok := sums[asset]
	if !ok {
		return fmt.Errorf("the release %s of %s/%s publishes no checksum for %s.\n\nIts %s lists %s.\n\nAn archive nothing can vouch for is not installed.\n\nSuggested action:\n  Ask the publisher to include every archive in %s.",
			tag, owner, repo, asset, remote.ChecksumsName, strings.Join(sortedAssetNames(sums), ", "), remote.ChecksumsName)
	}

	// 7. Download. Bytes, held in memory, written nowhere yet.
	archive, err := client.ReleaseAsset(ctx, owner, repo, tag, asset)
	if err != nil {
		return err
	}

	// 8. Verify, before anything is opened. sha256sum writes a bare hex
	// digest, so this compares hex to hex; the lock's own prefixed form is a
	// different string for a different purpose.
	got := sha256.Sum256(archive)
	if !strings.EqualFold(hex.EncodeToString(got[:]), strings.TrimSpace(want)) {
		return fmt.Errorf("the checksum of %s does not match the one %s publishes.\n\nPublished: %s\nDownloaded: %s\n\nThe archive is not what its publisher released, so it has not been opened and nothing has been installed.\n\nSuggested action:\n  Try again in case the download was corrupted. If it fails again, report it to the publisher before installing anything from this release.",
			asset, remote.ChecksumsName, strings.TrimSpace(want), hex.EncodeToString(got[:]))
	}

	// 9 and 10. Extract and move into place, which ExtractBinary does as one
	// step: it writes to a temporary file beside the destination and renames
	// it only after the whole archive has been walked without a refusal, so a
	// refusal leaves nothing runnable behind.
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return fmt.Errorf("creating %s to install %s into: %w", dest, name, err)
	}
	return plugins.ExtractBinary(archive, binary, filepath.Join(dest, binary), maxBinary)
}

// currentPlatform is the machine asking. An install is for this machine; the
// lock records a checksum per platform because it is committed and read on
// others.
func currentPlatform() pluginmanifest.Platform {
	return pluginmanifest.Platform{OS: runtime.GOOS, Arch: runtime.GOARCH}
}

// recordInstall writes the lock entry for what is now on disk.
//
// Read, modify, write whole, so installing a second plugin does not drop the
// first.
//
// A global install outside a project records nothing and says so, rather than
// writing a plugins.lock into whatever directory the user was standing in: the
// lock is a project file, and there is no project here to own it.
//
// The key is not always the name. A backend is recorded under
// backendhost.LockKey, which is the repository's name, so a provider s3 and a
// backend s3 do not overwrite each other in a file keyed by one string.
func recordInstall(dir string, warn io.Writer, key, name string, c plugins.Candidate, sum string) error {
	if !hasProject(dir) {
		fmt.Fprintf(warn, "note: no project here, so %s records nothing for %s.\n", plugins.LockfileName, name)
		return nil
	}

	lock, err := plugins.ReadLockfile(dir)
	if err != nil {
		return err
	}
	if lock == nil {
		lock = &plugins.Lockfile{Version: plugins.LockVersion}
	}
	if lock.Plugins == nil {
		lock.Plugins = map[string]plugins.LockEntry{}
	}

	entry := lock.Plugins[key]
	if entry.Version != c.Manifest.Version.String() {
		// A different version invalidates the other platforms' checksums:
		// they were recorded for the version being replaced, and keeping them
		// would leave the lock vouching for binaries nobody installed.
		entry.Checksums = nil
	}
	if entry.Checksums == nil {
		entry.Checksums = map[string]string{}
	}
	entry.Version = c.Manifest.Version.String()
	entry.Source = c.Source.String()
	entry.Checksums[plugins.PlatformKey()] = sum
	lock.Plugins[key] = entry

	if err := lock.Write(dir); err != nil {
		return fmt.Errorf("%w\n\nThe binary is installed and verified, but %s does not record it, so it cannot be checked on a later run.\n\nSuggested action:\n  Fix whatever stopped the write and run `infrena plugins install %s` again.",
			err, plugins.LockfileName, name)
	}
	return nil
}

// findInstalled locates the binary one lock entry stands for, reading the kind
// off the key exactly as `plugins list` reads it off a filename.
func findInstalled(key string, search pluginhost.SearchOptions) (string, []string, error) {
	name, ok := strings.CutPrefix(key, plugins.BackendRepoPrefix)
	if !ok {
		return pluginhost.Find(key, search)
	}

	binary := backendhost.BinaryName(name)
	var searched []string
	for _, dir := range search.Dirs() {
		if dir == "" {
			continue
		}
		path := filepath.Join(dir, binary)
		searched = append(searched, path)
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, searched, nil
		}
	}
	return "", searched, &backendhost.NotFoundError{Backend: name, Binary: binary, Searched: searched}
}

// sortedAssetNames names what a checksums file does hold, so "no checksum for
// your archive" is followed by the list that makes it actionable.
func sortedAssetNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// newPluginsVerifyCommand builds `infrena plugins verify`: re-read the lock,
// re-hash what is on disk, and report anything that has changed since it was
// installed.
//
// Offline, deliberately. The lock is what makes a binary still trustworthy
// tomorrow, so checking it must work on a machine with no network and must
// never consult the publisher again: a publisher who can be asked is a
// publisher who can answer differently.
func newPluginsVerifyCommand(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:           "verify",
		Short:         "Check the installed plugins against plugins.lock",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ro, closeRun, err := openRun(cmd, opts, "plugins verify", "")
			if err != nil {
				return err
			}
			defer closeRun()

			return runVerify(opts, ro)
		},
	}
}

// runVerify checks every plugin the lock records.
//
// Every entry is checked before anything is reported failed: a user who has
// just been told one binary changed wants to know whether the rest did too.
func runVerify(opts *GlobalOptions, ro *runOutput) error {
	lock, err := plugins.ReadLockfile(opts.Dir)
	if err != nil {
		return err
	}
	if lock == nil || len(lock.Plugins) == 0 {
		// An answer, not a failure: a project that has installed nothing has
		// nothing to verify.
		fmt.Fprintf(ro.Out(), "No %s here, so there is nothing to verify.\n", plugins.LockfileName)
		return nil
	}

	names := make([]string, 0, len(lock.Plugins))
	for name := range lock.Plugins {
		names = append(names, name)
	}
	sort.Strings(names)

	search := pluginhost.DefaultSearch(opts.Dir, opts.PluginDirs)
	var failures []error
	for _, name := range names {
		if err := verifyOne(ro, lock, search, name); err != nil {
			failures = append(failures, err)
			continue
		}
		fmt.Fprintf(ro.Out(), "ok  %s %s\n", name, lock.Plugins[name].Version)
	}

	if len(failures) == 0 {
		return nil
	}
	return errors.Join(failures...)
}

// verifyOne finds one plugin's binary and hashes it against the lock.
//
// A binary the lock records and the machine does not have is a failure, not a
// skip: a verify that passed silently would report a project as verified when a
// plugin it needs is gone.
//
// The key says which kind it is. A backend is recorded under
// backendhost.LockKey, so an entry carrying that prefix is looked up as
// infrena-backend-<name> rather than as a provider of that whole string, which
// would look for infrena-plugin-infrena-backend-s3 and report a backend install
// had just written as missing.
func verifyOne(ro *runOutput, lock *plugins.Lockfile, search pluginhost.SearchOptions, name string) error {
	path, searched, err := findInstalled(name, search)
	if err != nil {
		return fmt.Errorf("plugin %s is recorded in %s but no binary was found.\n\nLooked in:\n  %s\n\nSuggested action:\n  Run `infrena plugins install %s` to fetch it again.",
			name, plugins.LockfileName, strings.Join(searched, "\n  "), name)
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
