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

// THE MOST IMPORTANT TEST IN THIS UNIT. A rate limit and a missing repository
// come back from the SAME API call, and conflating them tells a user their
// plugin does not exist when what actually happened is that they searched four
// times in an hour. Those two messages send a reader to completely different
// places: one to check the spelling of a name, the other to wait or set a token.
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
	// Section 44: the message says when it resets and that a token raises it,
	// because both are actions the user can actually take.
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

// The manifest is read at the TAG, never the default branch (section 31.2):
// the default branch describes unreleased code, so judging compatibility from
// it would report a plugin as compatible that nobody can install.
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
