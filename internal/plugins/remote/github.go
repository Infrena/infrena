// Package remote is the only place in infrena that talks to a plugin's forge.
//
// IT IS SEPARATE FROM internal/plugins DELIBERATELY. PLAN.md §31.3 makes "the
// network is never on the hot path" a hard rule, and internal/plugins carries a
// test asserting net/http is absent from its dependency tree. A package that
// CAN reach the network is one that a future caller on the hot path can reach it
// from; keeping the reaching one layer up is what makes the rule checkable
// rather than merely intended.
//
// It executes nothing and downloads no binaries. It reads text.
package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultBaseURL is GitHub's REST API.
	DefaultBaseURL = "https://api.github.com"
	// DefaultRawURL serves a file's bytes at a ref. It is a second host because
	// the REST API takes a ref as a query parameter, and a tag belongs in the
	// path where it is impossible to drop by accident.
	DefaultRawURL = "https://raw.githubusercontent.com"

	// defaultTimeout bounds every request. A search with no deadline is a hung
	// command, and a hung command with no output is indistinguishable from a
	// crash.
	defaultTimeout = 30 * time.Second

	// perPage is the largest page GitHub serves.
	perPage = 100
	// maxPages caps an owner listing. Ten pages is a thousand repositories,
	// well past any real owner, and the cap means a forge that keeps answering
	// "here is another full page" cannot spin this forever.
	maxPages = 10

	// maxBody bounds what is read from a response, so a wrong URL answering
	// with something enormous cannot exhaust memory.
	maxBody = 8 << 20
)

// Client reads a plugin's forge over HTTP. The zero value is usable once HTTP
// and BaseURL are set; NewClient fills in the real endpoints and a timeout.
type Client struct {
	// HTTP performs the requests. Nil means a default client with a timeout.
	HTTP *http.Client
	// BaseURL is the REST API root. IT EXISTS SO TESTS POINT AT httptest and
	// never at api.github.com.
	BaseURL string
	// RawURL is the root for file bytes. Empty means BaseURL, which is what
	// lets one test server answer both.
	RawURL string
	// Token authenticates the requests. It is used FOR SEARCH ONLY: nothing in
	// this package downloads or runs anything.
	Token string
}

// NewClient returns a client pointed at GitHub, with a request timeout and a
// token from INFRENA_GITHUB_TOKEN, falling back to GITHUB_TOKEN.
func NewClient() *Client {
	token := os.Getenv(TokenVar)
	if token == "" {
		token = os.Getenv(GitHubTokenVar)
	}
	return &Client{
		HTTP:    &http.Client{Timeout: defaultTimeout},
		BaseURL: DefaultBaseURL,
		RawURL:  DefaultRawURL,
		Token:   token,
	}
}

// ownerShapes are the two ways GitHub will list an owner's repositories.
//
// THERE IS NO SINGLE ENDPOINT FOR BOTH. An owner is either a user or an
// organisation, `/orgs/{owner}/repos` answers for one and `/users/{owner}/repos`
// for the other, and asking only one shape makes every owner of the other kind
// look empty. github.com/infrena is an organisation, so `/users/infrena/repos`
// answers 200 with an empty array and a released plugin comes back as "no
// plugin named aws in any source" - the exact failure this package exists to
// make impossible.
//
// The organisation shape is asked first because that is where infrena's own
// plugins live, and a hit there costs the cheaper of the two orders.
var ownerShapes = []string{"orgs", "users"}

// Repositories lists every repository an owner publishes, UNFILTERED: the
// caller decides what the naming convention means, so the convention lives in
// one place rather than in every transport.
//
// BOTH SHAPES ARE TRIED AND THEIR ANSWERS UNIONED. A 404 from one is not a
// failure when the other answered: an organisation has no user listing and a
// user has no organisation listing, so exactly one 404 is the ordinary case.
// Only both failing means the owner could not be read, and that is reported as
// one NotFoundError naming the owner.
//
// ANY OTHER REFUSAL STOPS THE SEARCH RATHER THAN BEING UNIONED AWAY. A rate
// limit or a forbidden on one shape, combined with an empty list from the
// other, would produce an empty result that reads as "this owner publishes
// nothing" - which is §31.3's forbidden "could not see" rendered as "does not
// exist", arriving through the second endpoint.
func (c *Client) Repositories(ctx context.Context, owner string) ([]string, error) {
	var names []string
	seen := map[string]bool{}
	answered := false

	for _, shape := range ownerShapes {
		found, err := c.repositoriesUnder(ctx, shape, owner)
		if err != nil {
			var missing *NotFoundError
			if errors.As(err, &missing) {
				// The owner is not of this kind. The other shape is the answer.
				continue
			}
			return nil, err
		}
		answered = true
		for _, name := range found {
			if !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
	}

	if !answered {
		return nil, &NotFoundError{What: fmt.Sprintf("repositories of %q", owner)}
	}
	return names, nil
}

// repositoriesUnder pages through one of the two owner shapes.
func (c *Client) repositoriesUnder(ctx context.Context, shape, owner string) ([]string, error) {
	what := fmt.Sprintf("repositories of %q", owner)

	var names []string
	for page := 1; page <= maxPages; page++ {
		endpoint := fmt.Sprintf("%s/%s/%s/repos?per_page=%d&page=%d",
			strings.TrimSuffix(c.base(), "/"), shape, url.PathEscape(owner), perPage, page)

		body, err := c.get(ctx, endpoint, what, "application/vnd.github+json")
		if err != nil {
			return nil, err
		}

		var repos []struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(body, &repos); err != nil {
			return nil, fmt.Errorf("reading %s from %s: %w: expected a JSON array of repositories", what, endpoint, err)
		}
		for _, r := range repos {
			if r.Name != "" {
				names = append(names, r.Name)
			}
		}
		// A short page is the last page. Asking again would cost a request
		// from an allowance of sixty an hour for nothing.
		if len(repos) < perPage {
			break
		}
	}
	return names, nil
}

// LatestTag resolves the tag of a repository's latest release.
//
// A TAG, NOT A BRANCH (§31.2). The default branch describes unreleased code, so
// judging compatibility from it would report a plugin as compatible that nobody
// can install.
func (c *Client) LatestTag(ctx context.Context, owner, repo string) (string, error) {
	what := fmt.Sprintf("latest release of %s/%s", owner, repo)
	endpoint := fmt.Sprintf("%s/repos/%s/%s/releases/latest",
		strings.TrimSuffix(c.base(), "/"), url.PathEscape(owner), url.PathEscape(repo))

	body, err := c.get(ctx, endpoint, what, "application/vnd.github+json")
	if err != nil {
		return "", err
	}

	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := json.Unmarshal(body, &release); err != nil {
		return "", fmt.Errorf("reading %s from %s: %w: expected a JSON release", what, endpoint, err)
	}
	if release.TagName == "" {
		return "", &NotFoundError{What: fmt.Sprintf("a release tag for %s/%s", owner, repo)}
	}
	return release.TagName, nil
}

// FileAtTag reads one file's bytes at a git TAG.
//
// The tag goes in the PATH, not in a query parameter, so a request that lost it
// cannot quietly fall back to the default branch: it would ask for a path that
// does not exist and be told so.
func (c *Client) FileAtTag(ctx context.Context, owner, repo, tag, path string) ([]byte, error) {
	what := fmt.Sprintf("%s in %s/%s at %s", path, owner, repo, tag)
	endpoint := fmt.Sprintf("%s/%s/%s/%s/%s",
		strings.TrimSuffix(c.raw(), "/"),
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(tag), escapePath(path))

	return c.get(ctx, endpoint, what, "text/plain")
}

// get performs one request and returns the body, or the error the caller must
// be able to tell apart.
func (c *Client) get(ctx context.Context, endpoint, what, accept string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("building a request for %s: %w: %q is not a usable URL", what, err, endpoint)
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w: check the network connection, or try again", what, err)
	}
	defer resp.Body.Close()

	if err := c.classify(resp, what); err != nil {
		return nil, err
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("reading the body of %s: %w: the response ended early, so try again", what, err)
	}
	return body, nil
}

// classify turns a response into the error the caller must be able to tell
// apart, and the rate-limit case is why this function exists at all.
//
// GitHub answers an exhausted limit with 403 and a missing-or-private
// repository with 404, and unauthenticated callers get 60 requests an hour —
// which one owner search with a few candidates can exhaust in a single
// invocation. Reporting that as "not found" tells a user their plugin does not
// exist, sends them to check a spelling that was right, and hides a condition
// that fixes itself in under an hour. The two messages must never collapse into
// one.
//
// what names the resource, because §44 wants a message that says where.
func (c *Client) classify(resp *http.Response, what string) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	switch resp.StatusCode {
	case http.StatusNotFound:
		return &NotFoundError{What: what}

	case http.StatusForbidden, http.StatusTooManyRequests:
		// A 403 is a rate limit ONLY when the forge says the allowance is
		// spent. Anything else with the same code is a private repository or a
		// token problem, and saying "rate limit" would have the reader wait an
		// hour for a condition that will not change.
		if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			return &RateLimitError{
				Resets:        resetTime(resp.Header.Get("X-RateLimit-Reset")),
				Authenticated: c.Token != "",
			}
		}
		return &ForbiddenError{What: what, Message: apiMessage(resp)}

	default:
		return &APIError{What: what, Status: resp.StatusCode, Message: apiMessage(resp)}
	}
}

// resetTime reads X-RateLimit-Reset, which is unix seconds. A header that is
// missing or unreadable yields the zero time, and the message simply omits the
// reset rather than inventing one.
func resetTime(header string) time.Time {
	secs, err := strconv.ParseInt(header, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(secs, 0)
}

// apiMessage pulls GitHub's own explanation out of an error body, when there is
// one. A body that is not the expected JSON is simply not quoted.
func apiMessage(resp *http.Response) string {
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil || len(body) == 0 {
		return ""
	}
	var payload struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	return payload.Message
}

// escapePath escapes each segment of a file path, keeping the separators.
func escapePath(path string) string {
	segments := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	return strings.Join(segments, "/")
}

func (c *Client) base() string {
	if c.BaseURL == "" {
		return DefaultBaseURL
	}
	return c.BaseURL
}

// raw falls back to BaseURL so a single test server can answer both the API and
// the file reads.
func (c *Client) raw() string {
	if c.RawURL == "" {
		return c.base()
	}
	return c.RawURL
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP == nil {
		return &http.Client{Timeout: defaultTimeout}
	}
	return c.HTTP
}
