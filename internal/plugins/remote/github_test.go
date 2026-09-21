package remote

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A rate limit and a missing repository come back from the same API call, and
// conflating them tells a user their plugin does not exist when what actually
// happened is that they searched four times in an hour. The two messages send a
// reader to completely different places: one to check the spelling of a name,
// the other to wait or set a token.
func TestARateLimitIsNeverReportedAsNotFound(t *testing.T) {
	reset := time.Now().Add(37 * time.Minute)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		// GitHub answers an exhausted limit with 403, and 404 for a repository
		// that is missing OR private. Both must be told apart from each other.
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"API rate limit exceeded"}`)
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL}

	_, err := c.Repositories(context.Background(), "mycorp")

	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("error is %T (%v), want *RateLimitError", err, err)
	}
	var nf *NotFoundError
	if errors.As(err, &nf) {
		t.Fatal("a rate limit also reported as not found")
	}
	if !strings.Contains(err.Error(), "rate limit") {
		t.Errorf("message does not say rate limit: %v", err)
	}
	// The message says when it resets and that a token raises it, because both
	// are actions the user can actually take.
	if !strings.Contains(err.Error(), "INFRENA_GITHUB_TOKEN") {
		t.Errorf("message does not name the token variable: %v", err)
	}
	if rl.Resets.Unix() != reset.Unix() {
		t.Errorf("Resets = %v, want %v", rl.Resets, reset)
	}
}

// The other side of the same coin: a genuine 404 must be a NotFoundError and
// must not be dressed up as a rate limit.
func TestAMissingRepositoryIsNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL}

	_, err := c.LatestTag(context.Background(), "someone", "infrena-provider-nope")

	var nf *NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("error is %T (%v), want *NotFoundError", err, err)
	}
	var rl *RateLimitError
	if errors.As(err, &rl) {
		t.Fatal("a missing repository also reported as a rate limit")
	}
}

// A 403 that is NOT a rate limit (a private repository, a bad token) must be
// neither of the two above, or the message sends the reader somewhere useless.
func TestAForbiddenThatIsNotARateLimitIsItsOwnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "4999")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL}

	_, err := c.Repositories(context.Background(), "mycorp")

	var rl *RateLimitError
	if errors.As(err, &rl) {
		t.Error("a plain forbidden reported as a rate limit")
	}
	var nf *NotFoundError
	if errors.As(err, &nf) {
		t.Error("a plain forbidden reported as not found")
	}
	if err == nil {
		t.Fatal("a forbidden response was accepted")
	}
}

func TestRepositoriesListsAnOwnersRepositories(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"name":"infrena-provider-aws"},{"name":"website"},{"name":"infrena-provider-hetzner"}]`)
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL}

	got, err := c.Repositories(context.Background(), "mycorp")
	if err != nil {
		t.Fatal(err)
	}
	// Unfiltered: the CALLER decides what the naming convention means, so this
	// stays a thin transport and the convention lives in one place.
	if len(got) != 3 {
		t.Errorf("Repositories = %v, want all three", got)
	}
}

// The manifest is read at the tag, never the default branch: the default branch
// describes unreleased code, so judging compatibility from it would report a
// plugin as compatible that nobody can install.
func TestFileAtTagRequestsTheTagAndNotTheDefaultBranch(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		fmt.Fprint(w, "manifest: 2\n")
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL}

	if _, err := c.FileAtTag(context.Background(), "infrena", "infrena-provider-aws", "v0.4.0", "plugin.yaml"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(path, "v0.4.0") {
		t.Errorf("request path %q does not name the tag", path)
	}
	for _, branch := range []string{"main", "master", "HEAD"} {
		if strings.Contains(path, branch) {
			t.Errorf("request path %q names a branch", path)
		}
	}
}

func TestATokenIsSentWhenOneIsSet(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		fmt.Fprint(w, `[]`)
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL, Token: "secret"}

	if _, err := c.Repositories(context.Background(), "mycorp"); err != nil {
		t.Fatal(err)
	}
	if auth == "" {
		t.Error("no Authorization header was sent")
	}
	if strings.Contains(auth, "secret") == false {
		t.Errorf("Authorization header does not carry the token")
	}
}

// Two variables, in a fixed order: the infrena-specific one wins so a user can
// give search its own token without disturbing whatever else on the machine
// reads GITHUB_TOKEN.
func TestNewClientPrefersTheInfrenaToken(t *testing.T) {
	t.Setenv("INFRENA_GITHUB_TOKEN", "mine")
	t.Setenv("GITHUB_TOKEN", "theirs")
	if got := NewClient().Token; got != "mine" {
		t.Errorf("Token = %q, want the infrena one", got)
	}

	t.Setenv("INFRENA_GITHUB_TOKEN", "")
	if got := NewClient().Token; got != "theirs" {
		t.Errorf("Token = %q, want the fallback", got)
	}
}

// A search with no deadline is a hung command, so the default client carries
// one rather than relying on every caller to pass a context with a timeout.
func TestNewClientSetsATimeout(t *testing.T) {
	if c := NewClient(); c.HTTP == nil || c.HTTP.Timeout == 0 {
		t.Error("the default http client has no timeout")
	}
}

// GitHub has no endpoint that covers both. `/users/{owner}/repos` answers for a
// user and `/orgs/{owner}/repos` for an organisation, and an owner is one or the
// other — so asking only one shape makes every owner of the other kind look
// empty, and its released plugins look like they do not exist. github.com/infrena
// is an organisation.
func TestRepositoriesFindsAnOrganisationsRepositories(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/orgs/"):
			if r.URL.Query().Get("page") == "1" {
				fmt.Fprint(w, `[{"name":"infrena-provider-aws"},{"name":"website"}]`)
				return
			}
			fmt.Fprint(w, `[]`)
		case strings.HasPrefix(r.URL.Path, "/users/"):
			// What GitHub really answers for an organisation: 200, and empty.
			fmt.Fprint(w, `[]`)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL}

	got, err := c.Repositories(context.Background(), "infrena")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("Repositories = %v, want the organisation's two repositories", got)
	}
}

// The other shape, which must keep working: an owner that IS a user has no
// `/orgs/{owner}` at all, and the 404 that comes back is not an error when the
// other shape answered.
func TestRepositoriesFindsAUsersRepositories(t *testing.T) {
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		switch {
		case strings.HasPrefix(r.URL.Path, "/orgs/"):
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
		case strings.HasPrefix(r.URL.Path, "/users/"):
			if r.URL.Query().Get("page") == "1" {
				fmt.Fprint(w, `[{"name":"infrena-provider-hetzner"}]`)
				return
			}
			fmt.Fprint(w, `[]`)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL}

	got, err := c.Repositories(context.Background(), "someone")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "infrena-provider-hetzner" {
		t.Errorf("Repositories = %v, want the user's one repository", got)
	}
	// Both shapes were tried: the organisation one first, and its 404 did not
	// stop the search.
	if !contains(asked, "/orgs/someone/repos") {
		t.Errorf("the organisation shape was never asked for: %v", asked)
	}
}

// Only BOTH shapes failing is a failure. An owner that is neither a user nor an
// organisation genuinely does not exist, and that is a NotFoundError naming the
// owner rather than an empty list a reader would take for "publishes nothing".
func TestRepositoriesFailsOnlyWhenNeitherOwnerShapeAnswers(t *testing.T) {
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found"}`)
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL}

	got, err := c.Repositories(context.Background(), "nobody")

	var nf *NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("error is %T (%v), want *NotFoundError", err, err)
	}
	if got != nil {
		t.Errorf("Repositories = %v, want nothing alongside the error", got)
	}
	if !strings.Contains(err.Error(), "nobody") {
		t.Errorf("message does not name the owner: %v", err)
	}
	// Failing before both shapes were tried is the bug, not the answer.
	for _, want := range []string{"/orgs/nobody/repos", "/users/nobody/repos"} {
		if !contains(asked, want) {
			t.Errorf("%s was never asked for: %v", want, asked)
		}
	}
}

// contains reports whether a path was among those the fake forge was asked for.
func contains(paths []string, want string) bool {
	for _, p := range paths {
		if p == want {
			return true
		}
	}
	return false
}

// A refusal on one shape must NEVER be swallowed by the other shape's empty
// list: that is the rate-limit mistake arriving through the second endpoint,
// and it would report a plugin as non-existent because nobody would answer.
func TestARefusalOnOneShapeIsNotAnEmptyListing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/orgs/") {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message":"API rate limit exceeded"}`)
			return
		}
		fmt.Fprint(w, `[]`)
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL}

	got, err := c.Repositories(context.Background(), "infrena")

	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("error is %T (%v), want *RateLimitError", err, err)
	}
	if got != nil {
		t.Errorf("Repositories = %v, want nothing alongside the error", got)
	}
}
