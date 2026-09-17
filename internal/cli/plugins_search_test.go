package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/infrena/infrena/internal/plugins/remote"
	"github.com/infrena/infrena/pkg/pluginproto"
)

// PLAN.md §31.3's `infrena plugins search`: every match across every source,
// and for each one that cannot run here, why.

func TestSearchPrintsUsableAndRejectedWithReasons(t *testing.T) {
	dir := newProjectFixture(t)
	trustSources(t, "github.com/mycorp")
	srv := fakeGitHub(t) // serves one usable and one wrong-platform plugin
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	stdout, stderr, code := runCommand(t, dir, "plugins", "search", "hetzner")

	if code != ExitOK {
		t.Fatalf("exit = %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "2.0.0") {
		t.Errorf("usable candidate missing:\n%s", stdout)
	}
	// The rejected one must still appear, with why.
	if !strings.Contains(stdout, "no build for") {
		t.Errorf("rejected candidate or its reason missing:\n%s", stdout)
	}
	// NEITHER SOURCE IS PICKED. Two owners answering one name are both shown,
	// which is the rule the whole command exists to obey.
	for _, want := range []string{"github.com/infrena", "github.com/mycorp", "1.0.0"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output missing %q:\n%s", want, stdout)
		}
	}
}

// github.com/infrena is an ORGANISATION, and an organisation's repositories do
// not appear in GitHub's user listing at all - so a search that asks only
// /users/{owner}/repos reports a released, official plugin as non-existent.
func TestSearchFindsAPluginAnOrganisationPublishes(t *testing.T) {
	dir := newProjectFixture(t)
	trustSources(t)
	srv := fakeGitHubOrg(t)
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	stdout, stderr, code := runCommand(t, dir, "plugins", "search", "aws")

	if code != ExitOK {
		t.Fatalf("exit = %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "github.com/infrena") || !strings.Contains(stdout, "3.0.0") {
		t.Errorf("the organisation's plugin was not found:\n%s", stdout)
	}
}

// Nothing found is an answer and exits 0. It is also the moment a clear
// message matters most, because the user is deciding whether they typed the
// name wrong.
func TestSearchFindingNothingSaysSoAndExitsZero(t *testing.T) {
	dir := newProjectFixture(t)
	trustSources(t)
	srv := fakeGitHubEmpty(t)
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	stdout, _, code := runCommand(t, dir, "plugins", "search", "nope")

	if code != ExitOK {
		t.Errorf("exit = %d, want %d", code, ExitOK)
	}
	if !strings.Contains(stdout, "No plugin named") {
		t.Errorf("output does not say nothing was found:\n%s", stdout)
	}
}

// AN UNAUTHENTICATED SEARCH DOES NOT KNOW THAT NOTHING EXISTS, so it must not
// say so. A private repository answers an unauthenticated listing with 200 and
// an empty array - indistinguishable, from here, from an owner who publishes
// nothing - and "No plugin named %q in any source. Check the spelling" sends a
// user to check a spelling that was right. Same defect as reporting a rate
// limit as not-found (PLAN.md §31.3), through a different door.
func TestSearchWithoutATokenDoesNotClaimNothingExists(t *testing.T) {
	dir := newProjectFixture(t)
	trustSources(t)
	srv := fakeGitHubEmpty(t)
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	stdout, _, code := runCommand(t, dir, "plugins", "search", "aws")

	if code != ExitOK {
		t.Errorf("exit = %d, want %d", code, ExitOK)
	}
	// The definitive wording belongs to a search that could actually see.
	if strings.Contains(stdout, "in any source") {
		t.Errorf("an unauthenticated search claimed nothing exists anywhere:\n%s", stdout)
	}
	// §44: a suggested action the user can take.
	for _, want := range []string{"INFRENA_GITHUB_TOKEN", "GITHUB_TOKEN", "private"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output does not mention %q:\n%s", want, stdout)
		}
	}
	// The spelling hint stays, just not as the only explanation.
	if !strings.Contains(stdout, "spelling") {
		t.Errorf("output dropped the spelling hint:\n%s", stdout)
	}
}

// With a token, infrena really did look, so the definitive wording is right and
// telling the reader to set a token they already set would be noise.
func TestSearchWithATokenSaysNothingExistsPlainly(t *testing.T) {
	dir := newProjectFixture(t)
	trustSources(t)
	srv := fakeGitHubEmpty(t)
	t.Setenv("INFRENA_GITHUB_API", srv.URL)
	t.Setenv("INFRENA_GITHUB_TOKEN", "a-token")

	stdout, _, code := runCommand(t, dir, "plugins", "search", "aws")

	if code != ExitOK {
		t.Errorf("exit = %d, want %d", code, ExitOK)
	}
	if !strings.Contains(stdout, "in any source") {
		t.Errorf("an authenticated search did not give the definitive answer:\n%s", stdout)
	}
	if strings.Contains(stdout, "INFRENA_GITHUB_TOKEN") {
		t.Errorf("output tells a reader to set the token they already set:\n%s", stdout)
	}
}

// NO HOME MUST NOT MEAN NO SEARCH. `os.UserConfigDir` fails outright on a
// machine with neither HOME nor XDG_CONFIG_HOME - a minimal container, some CI
// runners - and refusing the whole command there denies a user the official
// owner, which needs no configuration file to be trusted. A missing config
// DIRECTORY degrades exactly like a missing config FILE.
func TestSearchWithoutAConfigDirectoryStillSearchesTheOfficialOwner(t *testing.T) {
	dir := newProjectFixture(t)
	trustSources(t)
	// After trustSources, so these win: no config home of any kind.
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	srv := fakeGitHubOrg(t)
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	stdout, stderr, code := runCommand(t, dir, "plugins", "search", "aws")

	if code != ExitOK {
		t.Fatalf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, ExitOK, stdout, stderr)
	}
	if strings.Contains(stderr, "user configuration directory") {
		t.Errorf("a missing config directory failed the search:\n%s", stderr)
	}
	if !strings.Contains(stdout, "github.com/infrena") || !strings.Contains(stdout, "3.0.0") {
		t.Errorf("the official owner was not searched:\n%s", stdout)
	}
}

// A SOURCE THAT COULD NOT BE READ IS A WARNING, NEVER A SILENCE. A rate limit
// rendered as "nothing found" sends a user to check a spelling that was right.
func TestSearchReportsASourceThatCouldNotBeReadRatherThanSayingNothingExists(t *testing.T) {
	dir := newProjectFixture(t)
	trustSources(t)
	srv := fakeGitHubRateLimited(t)
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	stdout, stderr, code := runCommand(t, dir, "plugins", "search", "hetzner")

	if code != ExitOK {
		t.Errorf("exit = %d, want %d", code, ExitOK)
	}
	if !strings.Contains(stderr, "rate limit") {
		t.Errorf("stderr does not report the rate limit:\n%s", stderr)
	}
	if !strings.Contains(stderr, "github.com/infrena") {
		t.Errorf("stderr does not name the source that failed:\n%s", stderr)
	}
	// "in any source" is the wording for a search that actually completed.
	// A search that could not read a source must never reach for it.
	if strings.Contains(stdout, "in any source") {
		t.Errorf("a source that could not be read was rendered as nothing existing:\n%s", stdout)
	}
	if !strings.Contains(stdout, "could not be searched") {
		t.Errorf("stdout does not say the answer is incomplete:\n%s", stdout)
	}
}

// Unit A's rule.
func TestSearchOutputModeLeavesStdoutByteEmpty(t *testing.T) {
	dir := newProjectFixture(t)
	trustSources(t, "github.com/mycorp")
	srv := fakeGitHub(t)
	t.Setenv("INFRENA_GITHUB_API", srv.URL)
	out := filepath.Join(t.TempDir(), "run.ndjson")

	stdout, _, _ := runCommand(t, dir, "plugins", "search", "hetzner", "--output", out)

	if stdout != "" {
		t.Errorf("stdout = %q, want nothing", stdout)
	}
}

// THE HARD RULE, asserted at the command boundary: no command on the hot path
// may reach the network. This is the guard that catches a future change wiring
// search into one of them.
//
// EACH CASE IS ARGUED THE WAY A USER WOULD ARGUE IT, and the exit code is
// asserted alongside the count. A command that failed on its arguments makes
// no network request either, so a test that only counted attempts would keep
// passing after `graph` stopped working — it would be checking that a usage
// error is cheap rather than that the hot path is offline.
func TestHotPathCommandsMakeNoNetworkRequest(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		want     int
		produces string
	}{
		{"validate", []string{"validate", "dev"}, ExitOK, "Configuration valid"},
		// The fixture declares a resource nothing has created, so a plan has
		// changes and §16 says that is exit 2.
		{"plan", []string{"plan", "dev"}, ExitChanges, "1 to create"},
		{"graph", []string{"graph", "dev"}, ExitOK, "fake.network.network"},
		// explain takes a RESOURCE TYPE, not an environment.
		{"explain", []string{"explain", "fake.network"}, ExitOK, "Required:"},
		// state is a command group; `state dev` would be an unknown subcommand
		// and would exercise nothing.
		{"state", []string{"state", "list", "dev"}, ExitOK, "No resources are managed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := newProjectFixture(t)
			blocked := blockNetwork(t)

			stdout, stderr, code := runCommand(t, dir, tc.args...)

			if code != tc.want {
				t.Fatalf("exit = %d, want %d; the command did not run\nstdout:\n%s\nstderr:\n%s",
					code, tc.want, stdout, stderr)
			}
			// The command's own product, so this cannot pass on a command
			// that exited cleanly without doing anything.
			if !strings.Contains(stdout, tc.produces) {
				t.Fatalf("output does not contain %q, so the command did not do its work:\n%s",
					tc.produces, stdout)
			}
			if n := blocked.Attempts(); n != 0 {
				t.Errorf("%s made %d network attempts, want 0", tc.name, n)
			}
		})
	}
}

// trustSources writes the user's trusted-source file into a config home of its
// own, so a search in a test reads the sources the test named and never the
// developer's own. The cache home moves with it for the same reason.
func trustSources(t *testing.T, sources ...string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	// Whether a token is set changes what an empty result MEANS, so no test
	// inherits the developer's own. A test about the authenticated wording sets
	// one itself, after this.
	t.Setenv("INFRENA_GITHUB_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")

	if len(sources) == 0 {
		return
	}
	dir := filepath.Join(home, "config", "infrena")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "sources:\n"
	for _, s := range sources {
		body += "  - " + s + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "plugins.yml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fakeGitHub answers the three calls a search makes, for two owners publishing
// one name: the official owner with a build for a machine nobody is sitting at,
// and mycorp with a build for this one.
func fakeGitHub(t *testing.T) *httptest.Server {
	t.Helper()
	here := fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH)
	return fakeForge(t, forge{
		// infrena is an ORGANISATION, as it is in reality, and mycorp a user:
		// one fixture covering both owner shapes.
		orgs: map[string]bool{"infrena": true},
		repos: map[string][]string{
			"infrena": {"infrena-provider-hetzner", "website"},
			"mycorp":  {"infrena-provider-hetzner"},
		},
		tags: map[string]string{
			"infrena/infrena-provider-hetzner": "v1.0.0",
			"mycorp/infrena-provider-hetzner":  "v2.0.0",
		},
		files: map[string]string{
			"infrena/infrena-provider-hetzner@v1.0.0": manifestYAML("hetzner", "1.0.0", "windows/amd64"),
			"mycorp/infrena-provider-hetzner@v2.0.0":  manifestYAML("hetzner", "2.0.0", here),
		},
	})
}

// fakeGitHubOrg is the official owner as it really is: an organisation, whose
// repositories GitHub serves under /orgs and not under /users.
func fakeGitHubOrg(t *testing.T) *httptest.Server {
	t.Helper()
	here := fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH)
	return fakeForge(t, forge{
		orgs:  map[string]bool{"infrena": true},
		repos: map[string][]string{"infrena": {"infrena-provider-aws"}},
		tags:  map[string]string{"infrena/infrena-provider-aws": "v3.0.0"},
		files: map[string]string{
			"infrena/infrena-provider-aws@v3.0.0": manifestYAML("aws", "3.0.0", here),
		},
	})
}

// fakeGitHubEmpty is an owner who publishes nothing.
func fakeGitHubEmpty(t *testing.T) *httptest.Server {
	t.Helper()
	return fakeForge(t, forge{})
}

// fakeGitHubRateLimited is the forge refusing to answer at all, in the way that
// must never be reported as "not found".
func fakeGitHubRateLimited(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"API rate limit exceeded"}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// forge is what the fake serves, keyed the way the client asks for it.
type forge struct {
	repos map[string][]string
	tags  map[string]string
	files map[string]string
	// orgs are the owners that are ORGANISATIONS rather than users. GitHub
	// lists those under /orgs/{owner}/repos, answers /users/{owner}/repos with
	// an empty array, and has no /orgs listing at all for a user - so an owner
	// search that asks only one shape sees half the world.
	orgs map[string]bool
	// visibleTo is the token that can see any of this. Empty means everything
	// is public. A caller without it is served the empty world GitHub serves
	// for a private repository, which is the whole point: an empty answer here
	// means "you could not see", not "it is not there".
	visibleTo string
	// releases are the assets a release publishes, keyed "owner/repo@tag" and
	// then by asset name. `plugins install` reads these; `plugins search` never
	// asks for one, which is itself worth keeping true.
	//
	// GitHub HAS NO ENDPOINT THAT TAKES AN ASSET NAME, so the fake models the
	// two requests the real one forces: the release is read at
	// /repos/{o}/{r}/releases/tags/{tag} for the ids, and each asset is fetched
	// at /repos/{o}/{r}/releases/assets/{id}. A fake that served an asset by
	// name would answer whatever the client asked and prove nothing about the
	// path a user takes.
	releases map[string]map[string][]byte
}

func manifestYAML(name, version, platform string) string {
	return fmt.Sprintf(`manifest: 2
name: %s
version: %s
protocol: [%d]
platforms: [%s]
description: a plugin, for a test
`, name, version, pluginproto.Supported[0], platform)
}

// fakeForge serves the REST API and the raw file host from one server, which is
// what remote.Client's RawURL fallback is for.
func fakeForge(t *testing.T, f forge) *httptest.Server {
	t.Helper()

	// Asset ids, assigned once and deterministically, so the listing and the
	// download agree without the handler having to search.
	assetIDs := map[string]int64{}
	assetBodies := map[string][]byte{}
	next := int64(1000)
	for _, release := range sortedReleaseKeys(f.releases) {
		for _, name := range sortedAssetKeys(f.releases[release]) {
			next++
			assetIDs[release+"/"+name] = next
			assetBodies[fmt.Sprint(next)] = f.releases[release][name]
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if f.visibleTo != "" && r.Header.Get("Authorization") != "Bearer "+f.visibleTo {
			// The empty world: a listing with nothing in it, and nothing else
			// findable. Exactly what an unauthorised caller sees on GitHub.
			if len(parts) == 3 && parts[2] == "repos" {
				_ = json.NewEncoder(w).Encode([]map[string]string{})
				return
			}
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch {
		case len(parts) == 3 && (parts[0] == "users" || parts[0] == "orgs") && parts[2] == "repos":
			owner, isOrg := parts[1], f.orgs[parts[1]]
			if parts[0] == "orgs" && !isOrg {
				// A user has no organisation listing. The 404 is ordinary, not
				// a failure, whenever the other shape answers.
				w.WriteHeader(http.StatusNotFound)
				return
			}
			names := []map[string]string{}
			// Page 2 onwards is empty; the client stops on a short page. An
			// organisation's repositories are invisible to the user listing,
			// which is how a published plugin came back as "no plugin named".
			if r.URL.Query().Get("page") == "1" && !(parts[0] == "users" && isOrg) {
				for _, name := range f.repos[owner] {
					names = append(names, map[string]string{"name": name})
				}
			}
			_ = json.NewEncoder(w).Encode(names)

		case len(parts) == 5 && parts[0] == "repos" && parts[3] == "releases" && parts[4] == "latest":
			tag, ok := f.tags[parts[1]+"/"+parts[2]]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": tag})

		case len(parts) == 5 && parts[0] == "repos" && parts[3] == "releases" && parts[4] == "tags":
			// Never reached: the tag is a sixth segment. Kept out of the
			// default arm so a malformed request is still an error.
			w.WriteHeader(http.StatusNotFound)

		case len(parts) == 6 && parts[0] == "repos" && parts[3] == "releases" && parts[4] == "tags":
			assets, ok := f.releases[parts[1]+"/"+parts[2]+"@"+parts[5]]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			var listed []map[string]any
			for _, name := range sortedAssetKeys(assets) {
				listed = append(listed, map[string]any{
					"name": name,
					"id":   assetIDs[parts[1]+"/"+parts[2]+"@"+parts[5]+"/"+name],
					// The real API returns a browser_download_url too. It is
					// served here so a client that followed it instead of
					// building its own URL would be caught by the assertion in
					// the default arm rather than silently working.
					"browser_download_url": "http://elsewhere.invalid/" + name,
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": parts[5], "assets": listed})

		case len(parts) == 5 && parts[0] == "repos" && parts[3] == "releases" && parts[4] == "assets":
			w.WriteHeader(http.StatusNotFound)

		case len(parts) == 6 && parts[0] == "repos" && parts[3] == "releases" && parts[4] == "assets":
			body, ok := assetBodies[parts[5]]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			// The real endpoint serves the bytes only for this Accept; with
			// the JSON one it describes the asset instead.
			if r.Header.Get("Accept") != "application/octet-stream" {
				t.Errorf("asset fetched with Accept %q, want application/octet-stream", r.Header.Get("Accept"))
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(body)

		case len(parts) == 4 && parts[3] == "plugin.yaml":
			body, ok := f.files[parts[0]+"/"+parts[1]+"@"+parts[2]]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			fmt.Fprint(w, body)

		default:
			t.Errorf("fake forge asked for an unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// THE CACHE KEY MUST INCLUDE WHO IS ASKING. This is the sequence a user
// actually follows: search, be told to set a token, set it, search again. An
// unauthenticated "I could not see it" persisted under a key that ignores the
// credential is replayed for the whole TTL as if it were "it does not exist" -
// which is the same defect as reporting a rate limit as not-found, and it
// defeats the remedy infrena itself suggested.
func TestSearchAfterSettingATokenIsNotServedTheUnauthenticatedEmptyResult(t *testing.T) {
	dir := newProjectFixture(t)
	trustSources(t)
	srv := fakeGitHubPrivateOrg(t, "a-token")
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	// Nothing is visible without a token, and that answer goes into the cache.
	stdout, stderr, code := runCommand(t, dir, "plugins", "search", "aws")
	if code != ExitOK {
		t.Fatalf("unauthenticated search: exit = %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if strings.Contains(stdout, "3.0.0") {
		t.Fatalf("the fixture is wrong: an unauthenticated search saw the plugin:\n%s", stdout)
	}

	// The user does exactly what they were told to do, and does NOT pass
	// --refresh, because nothing told them to.
	t.Setenv("INFRENA_GITHUB_TOKEN", "a-token")

	stdout, stderr, code = runCommand(t, dir, "plugins", "search", "aws")
	if code != ExitOK {
		t.Fatalf("authenticated search: exit = %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "github.com/infrena") || !strings.Contains(stdout, "3.0.0") {
		t.Errorf("the authenticated search was served the unauthenticated empty result:\n%s", stdout)
	}
}

// TWO TOKENS ARE TWO DIFFERENT VIEWS OF THE FORGE. A repository one user can
// read is invisible to another, so one user's answer must never be served to
// the next - which on a shared machine, or under one account with two tokens,
// is the same replayed "could not see" wearing a different hat.
func TestSearchWithADifferentTokenIsNotServedTheOtherTokensResult(t *testing.T) {
	dir := newProjectFixture(t)
	trustSources(t)
	srv := fakeGitHubPrivateOrg(t, "the-token-that-can-see")
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	t.Setenv("INFRENA_GITHUB_TOKEN", "a-token-that-cannot")
	stdout, stderr, code := runCommand(t, dir, "plugins", "search", "aws")
	if code != ExitOK {
		t.Fatalf("first search: exit = %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if strings.Contains(stdout, "3.0.0") {
		t.Fatalf("the fixture is wrong: the wrong token saw the plugin:\n%s", stdout)
	}

	t.Setenv("INFRENA_GITHUB_TOKEN", "the-token-that-can-see")

	stdout, stderr, code = runCommand(t, dir, "plugins", "search", "aws")
	if code != ExitOK {
		t.Fatalf("second search: exit = %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "3.0.0") {
		t.Errorf("one token's answer was served to another token:\n%s", stdout)
	}
}

// A KEY BECOMES A FILENAME, so the credential's identity may be in the key only
// as a hash. A token written into a path leaks it to anything that lists a
// directory - a backup, a crash report, a shoulder - and a cache file is the
// last place a secret should be legible.
func TestSearchNeverWritesTheTokenIntoACacheFilename(t *testing.T) {
	dir := newProjectFixture(t)
	trustSources(t)
	srv := fakeGitHubPrivateOrg(t, "sekrit-token-value")
	t.Setenv("INFRENA_GITHUB_API", srv.URL)
	t.Setenv("INFRENA_GITHUB_TOKEN", "sekrit-token-value")

	stdout, stderr, code := runCommand(t, dir, "plugins", "search", "aws")
	if code != ExitOK {
		t.Fatalf("exit = %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "3.0.0") {
		t.Fatalf("the search found nothing, so it may not have written a cache entry:\n%s", stdout)
	}

	cacheDir := filepath.Join(os.Getenv("XDG_CACHE_HOME"), "infrena", "plugins")
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		t.Fatalf("reading the cache directory %s: %v", cacheDir, err)
	}
	if len(entries) == 0 {
		t.Fatalf("no cache entries in %s, so this test asserts nothing", cacheDir)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "sekrit-token-value") {
			t.Errorf("cache filename %q contains the token", e.Name())
		}
	}

	// The filename is a hash of the key, so it would hide a token in the key by
	// accident. The key itself is what must not carry one, since that is the
	// thing a future change could hand to something that does not hash.
	f := &cachedFetcher{client: &remote.Client{BaseURL: srv.URL, Token: "sekrit-token-value"}}
	key := f.key("repos", "infrena")
	if strings.Contains(key, "sekrit-token-value") {
		t.Errorf("the cache key carries the token: %q", key)
	}
	if key == (&cachedFetcher{client: &remote.Client{BaseURL: srv.URL}}).key("repos", "infrena") {
		t.Error("a token and no token produce the same cache key")
	}
}

// fakeGitHubPrivateOrg is the official owner publishing a repository only a
// caller bearing token can see. Without it the forge answers exactly as GitHub
// does for a private repository: 200 and an empty array, indistinguishable from
// an owner who publishes nothing.
func fakeGitHubPrivateOrg(t *testing.T, token string) *httptest.Server {
	t.Helper()
	here := fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH)
	return fakeForge(t, forge{
		visibleTo: token,
		orgs:      map[string]bool{"infrena": true},
		repos:     map[string][]string{"infrena": {"infrena-provider-aws"}},
		tags:      map[string]string{"infrena/infrena-provider-aws": "v3.0.0"},
		files: map[string]string{
			"infrena/infrena-provider-aws@v3.0.0": manifestYAML("aws", "3.0.0", here),
		},
	})
}

// sortedReleaseKeys and sortedAssetKeys keep the fake's asset ids stable across
// runs, so a failure is reproducible rather than depending on map order.
func sortedReleaseKeys(m map[string]map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedAssetKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Search is exploratory. If you ask what is called s3 and there is a
// provider and a backend, both is the honest answer, and the column is what
// makes it readable rather than confusing.
func TestSearchShowsTheKindOfEachCandidate(t *testing.T) {
	dir := newProjectFixture(t)
	trustSources(t)
	srv := fakeGitHubServingBothKinds(t, "s3")
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	stdout, _, code := runCommand(t, dir, "plugins", "search", "s3")

	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stdout, "KIND") {
		t.Errorf("no kind column:\n%s", stdout)
	}
	for _, want := range []string{"provider", "backend"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output does not show %q:\n%s", want, stdout)
		}
	}
}
