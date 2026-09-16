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

	"github.com/infrena/infrena/internal/config"
	"github.com/infrena/infrena/internal/pluginhost"
	"github.com/infrena/infrena/internal/plugins"
	"github.com/infrena/infrena/internal/plugins/remote"
	"github.com/infrena/infrena/pkg/pluginmanifest"
)

// maxBinary bounds what is written to disk out of an archive.
//
// Real plugins are around nine megabytes unpacked. A quarter of a gigabyte is
// far past anything plausible while still refusing an archive that expands
// until the disk is full, which is a denial of service that survives a reboot.
// It is the same number remote caps a download at, for the same reason at the
// other end of the pipe.
const maxBinary = 256 << 20

// newPluginsInstallCommand builds `infrena plugins install <name>[@version]`
// (PLAN.md section 31.3).
//
// THE ORDER OF THE STEPS IS THE DESIGN, and it is written out here because a
// reader changing this file needs it before they need any of the code:
//
//	search -> filter to what can run here -> refuse if nothing or more than one
//	-> check trust -> prompt ONLY if a person is there -> fetch SHA256SUMS ->
//	download -> VERIFY -> extract -> move into place -> write the lock
//
// Every one of those is a refusal rather than a repair, and a refusal at any
// step leaves NOTHING on disk. Verification comes before extraction because a
// tampered archive must be refused without being opened; the lock comes last
// because it records what is already there rather than what is intended.
//
// TWO RULES HAVE NO EXCEPTIONS. A run with nobody at the terminal never
// approves a source: a pipeline that quietly starts trusting a new publisher of
// executables is the failure this whole subsection exists to prevent. And
// infrena never chooses between two sources answering one name, because the
// place code comes from is the part that matters.
func newPluginsInstallCommand(opts *GlobalOptions) *cobra.Command {
	var global bool

	cmd := &cobra.Command{
		Use:           "install <name>[@version]",
		Short:         "Download and install a provider plugin",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ro, closeRun, err := openRun(cmd, opts, "plugins install", "")
			if err != nil {
				return err
			}
			defer closeRun()

			return runInstall(cmd, opts, ro.Out(), args[0], global)
		},
	}

	cmd.Flags().BoolVar(&global, "global", false,
		"install for this user rather than into this project")
	return cmd
}

// runInstall is the whole sequence, in the order its doc comment sets out.
func runInstall(cmd *cobra.Command, opts *GlobalOptions, out io.Writer, arg string, global bool) error {
	name, wantVersion, err := parsePluginArg(arg)
	if err != nil {
		return err
	}

	// WHERE IT WILL GO IS DECIDED FIRST, before a single request. A download
	// that succeeds and then discovers it has nowhere to put anything has spent
	// a user's rate limit to reach an error it could have reached instantly.
	dest, err := installDir(opts, global)
	if err != nil {
		return err
	}

	// 1. SEARCH, across the sources the user trusts plus the one this project
	// names. Naming one is what gets it searched; it is not what gets it
	// installed.
	sources, untrusted, err := searchSources(opts.Dir, name)
	if err != nil {
		return err
	}
	fetcher := searchFetcher(false)
	found, problems := plugins.Search(cmd.Context(), fetcher, sources, name, searchEnvironment(opts.Dir, name))
	renderSearchWarnings(cmd.ErrOrStderr(), problems, found)

	// 2. FILTER, and 3. REFUSE if that leaves anything other than exactly one.
	chosen, err := soleCandidate(name, wantVersion, found, len(problems) > 0, fetcher.authenticated())
	if err != nil {
		return err
	}

	// 4 and 5. TRUST, and the prompt that is the only way to grant it.
	if err := ensureTrusted(cmd, opts, out, *chosen, untrusted); err != nil {
		return err
	}

	// 6 to 9. Checksums, the archive, the proof, and the one file taken out.
	client := installClient()
	binary := pluginhost.BinaryName(name)
	if err := fetchAndInstall(cmd.Context(), client, *chosen, name, binary, dest); err != nil {
		return err
	}

	// 10. THE LOCK, written from what is now on disk rather than from what was
	// downloaded, so it records the file a later run will actually hash.
	path := filepath.Join(dest, binary)
	sum, err := plugins.FileChecksum(path)
	if err != nil {
		return err
	}
	if err := recordInstall(opts.Dir, cmd.ErrOrStderr(), name, *chosen, sum); err != nil {
		return err
	}

	fmt.Fprintf(out, "Installed %s %s from %s\n  %s\n",
		name, chosen.Manifest.Version, chosen.Source, path)
	return nil
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
// BOTH ARE DIRECTORIES pluginhost.DefaultSearch ALREADY SEARCHES, so install
// puts plugins exactly where the loader was already looking. Nothing moves,
// nothing is registered, and a hand-placed binary in a higher-precedence
// directory keeps winning, which is what makes a plugin author's --plugin-dir
// still work the day after an install.
//
// Without --global the project is the default, so a project's plugins travel
// with its lock file. A run outside a project REFUSES rather than quietly
// installing somewhere the user did not name: writing an executable into
// ./.infra/plugins of whatever directory someone happened to be standing in is
// a surprise, and surprises about executables are the kind worth an error.
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
	return filepath.Join(opts.Dir, ".infra", "plugins"), nil
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
// IT NEVER PICKS. Two sources answering one name end here, with both printed
// and neither installed, because choosing silently hands a user code from a
// place they did not choose and the place is whose code runs on their machine.
// Being first in the search results is not being chosen: Search sorts for
// stable rendering and its own comment says so.
//
// NOTHING FOUND IS NOT THE SAME AS NOTHING EXISTING. A source that refused, and
// a search that ran without a token and therefore could not see a private
// repository, are both reported as what they are rather than as a missing
// plugin (section 31.3).
func soleCandidate(name, wantVersion string, found []plugins.Candidate, partial, authenticated bool) (*plugins.Candidate, error) {
	if len(found) == 0 {
		return nil, noSuchPlugin(name, partial, authenticated)
	}

	// A VERSION IS PART OF THE QUESTION, not a choice between answers. It
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
		// The plan's wording, and the rule it stands for: infrena does not
		// choose between sources.
		return nil, fmt.Errorf("%d sources publish a plugin named %q, and infrena does not choose between them.\n\n%s\nSuggested action:\n  Name the one you mean in this project's `plugins:` block, or remove the source you did not mean from your infrena plugins.yml, then install again.",
			countSources(usable), name, describeCandidates(usable))
	}
	return &usable[0], nil
}

// noSuchPlugin says nothing was found, and says it differently when infrena
// could not actually see - the same three-way distinction `plugins search`
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

// describeCandidates lists what was found, one per line, with the reason a
// release cannot run here. It is what turns every refusal above into something
// a reader can act on rather than a statement that they are out of luck.
func describeCandidates(found []plugins.Candidate) string {
	var b strings.Builder
	for _, c := range found {
		fmt.Fprintf(&b, "  %s  %s", c.Source, c.Manifest.Version)
		if !c.Usable {
			fmt.Fprintf(&b, "  (%s)", c.Reason)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// ensureTrusted is the security boundary the whole command is built around.
//
// A PROJECT MAY NAME A SOURCE; ONLY A USER MAY TRUST ONE. infra.yml travels
// with a `git clone`, so a source named there is searched and shown and is NOT
// a grant: otherwise cloning a repository would let it introduce a place
// infrena fetches executables from, and `infrena plan` would download one.
//
// NON-INTERACTIVE NEVER APPROVES, and the two ways of having nobody there are
// both handled. --output means a frontend is reading a file and no human is
// watching stdout; end of input means nothing was there to answer. Either way
// the answer is an error naming the owner and how to approve it, never a
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

	// The approval is recorded in the USER's own file, so it is asked once. A
	// machine with no config directory cannot record one, and Approve says so
	// rather than trusting the source for this run alone.
	home, err := os.UserConfigDir()
	if err != nil {
		home = ""
	}
	return plugins.Approve(home, c.Source)
}

// untrustedError is the refusal, which must name the owner and the way to
// approve it: a refusal that does not say who is asking and what to do next is
// the kind section 44 forbids.
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
// VERIFY BEFORE EXTRACT, which is why these are one function rather than a
// download helper and an extract helper a future caller could put in the other
// order. A tampered archive is refused without being opened.
func fetchAndInstall(ctx context.Context, client *remote.Client, c plugins.Candidate, name, binary, dest string) error {
	owner, repo, tag := c.Source.Owner, c.Repo, c.Tag

	// 6. THE CHECKSUMS COME FROM THE RELEASE, NEVER THE MANIFEST (section
	// 31.2): the manifest is committed before the archives exist. A release
	// without a SHA256SUMS cannot be installed, and Checksums says so.
	sums, err := client.Checksums(ctx, owner, repo, tag)
	if err != nil {
		return err
	}

	// THE MANIFEST'S VERSION NAMES THE ASSET, NOT THE TAG. release.yml strips
	// the leading v, so the tag is v0.4.0 while the archive is
	// infrena-plugin-aws_0.4.0_linux_amd64.tar.gz. Using the tag here asks for
	// an asset that does not exist, and a 404 on this path would read as "this
	// plugin publishes no build for your machine".
	asset := remote.AssetName(name, c.Manifest.Version.String(), currentPlatform())
	want, ok := sums[asset]
	if !ok {
		return fmt.Errorf("the release %s of %s/%s publishes no checksum for %s.\n\nIts %s lists %s.\n\nAn archive nothing can vouch for is not installed.\n\nSuggested action:\n  Ask the publisher to include every archive in %s.",
			tag, owner, repo, asset, remote.ChecksumsName, strings.Join(sortedAssetNames(sums), ", "), remote.ChecksumsName)
	}

	// 7. DOWNLOAD. Bytes, held in memory, written nowhere yet.
	archive, err := client.ReleaseAsset(ctx, owner, repo, tag, asset)
	if err != nil {
		return err
	}

	// 8. VERIFY, before anything is opened. sha256sum writes a bare hex digest,
	// so this compares hex to hex; the lock's own prefixed form is a different
	// string for a different purpose and the two must not be confused.
	got := sha256.Sum256(archive)
	if !strings.EqualFold(hex.EncodeToString(got[:]), strings.TrimSpace(want)) {
		return fmt.Errorf("the checksum of %s does not match the one %s publishes.\n\nPublished: %s\nDownloaded: %s\n\nThe archive is not what its publisher released, so it has not been opened and nothing has been installed.\n\nSuggested action:\n  Try again in case the download was corrupted. If it fails again, report it to the publisher before installing anything from this release.",
			asset, remote.ChecksumsName, strings.TrimSpace(want), hex.EncodeToString(got[:]))
	}

	// 9 and 10. EXTRACT AND MOVE INTO PLACE, which ExtractBinary does as one
	// step: it writes to a temporary file beside the destination and renames it
	// only after the whole archive has been walked without a refusal, so a
	// refusal leaves nothing runnable behind.
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return fmt.Errorf("creating %s to install %s into: %w", dest, name, err)
	}
	return plugins.ExtractBinary(archive, binary, filepath.Join(dest, binary), maxBinary)
}

// currentPlatform is the machine asking. Install is for THIS machine; the lock
// records a checksum per platform because it is committed and read on others.
func currentPlatform() pluginmanifest.Platform {
	return pluginmanifest.Platform{OS: runtime.GOOS, Arch: runtime.GOARCH}
}

// recordInstall writes the lock entry for what is now on disk.
//
// READ, MODIFY, WRITE WHOLE, so installing a second plugin does not drop the
// first. The file is committed, so the bytes are stable across runs by
// construction (see Lockfile.Write).
//
// A GLOBAL INSTALL OUTSIDE A PROJECT RECORDS NOTHING, and says so rather than
// writing a plugins.lock into whatever directory the user was standing in. The
// lock is a project file; there is no project here to own it.
func recordInstall(dir string, warn io.Writer, name string, c plugins.Candidate, sum string) error {
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

	entry := lock.Plugins[name]
	if entry.Version != c.Manifest.Version.String() {
		// A DIFFERENT VERSION INVALIDATES THE OTHER PLATFORMS' CHECKSUMS. They
		// were recorded for the version being replaced, and keeping them would
		// leave the lock vouching for binaries nobody installed.
		entry.Checksums = nil
	}
	if entry.Checksums == nil {
		entry.Checksums = map[string]string{}
	}
	entry.Version = c.Manifest.Version.String()
	entry.Source = c.Source.String()
	entry.Checksums[plugins.PlatformKey()] = sum
	lock.Plugins[name] = entry

	if err := lock.Write(dir); err != nil {
		return fmt.Errorf("%w\n\nThe binary is installed and verified, but %s does not record it, so it cannot be checked on a later run.\n\nSuggested action:\n  Fix whatever stopped the write and run `infrena plugins install %s` again.",
			err, plugins.LockfileName, name)
	}
	return nil
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
// OFFLINE, and that is the point rather than an optimisation. The lock is what
// makes a binary still trustworthy tomorrow, so checking it must work on a
// machine with no network and must never consult the publisher again - a
// publisher who can be asked is a publisher who can answer differently.
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
// EVERY ENTRY IS CHECKED BEFORE ANYTHING IS REPORTED FAILED. Stopping at the
// first bad one would hide the second, and a user who has just been told one
// binary changed wants to know whether the rest did too.
func runVerify(opts *GlobalOptions, ro *runOutput) error {
	lock, err := plugins.ReadLockfile(opts.Dir)
	if err != nil {
		return err
	}
	if lock == nil || len(lock.Plugins) == 0 {
		// AN ANSWER, NOT A FAILURE. A project that has installed nothing has
		// nothing to verify, and saying so is the truth rather than an error.
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
// A BINARY THE LOCK RECORDS AND THE MACHINE DOES NOT HAVE IS A FAILURE, not a
// skip. The lock says this project installed it; something removed it, and a
// verify that passed silently would report a project as verified when a plugin
// it needs is gone.
func verifyOne(ro *runOutput, lock *plugins.Lockfile, search pluginhost.SearchOptions, name string) error {
	path, searched, err := pluginhost.Find(name, search)
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
