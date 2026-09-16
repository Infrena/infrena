package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		switch {
		case len(parts) == 3 && parts[0] == "users" && parts[2] == "repos":
			names := []map[string]string{}
			// Page 2 onwards is empty; the client stops on a short page.
			if r.URL.Query().Get("page") == "1" {
				for _, name := range f.repos[parts[1]] {
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
