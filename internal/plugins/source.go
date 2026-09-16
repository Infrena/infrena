// Package plugins knows where a provider plugin may come from, and which of
// those places the USER has trusted.
//
// IT MAKES NO NETWORK REQUESTS, and Unit 2 adds a client on top of it rather
// than inside it. That split is not tidiness: PLAN.md §31.3 makes "the network
// is never on the hot path" a hard rule, and a package that can reach the
// network is one a future caller on the hot path can reach it FROM. Keeping the
// reaching in one place above this one is what makes the rule checkable.
//
// THE NAMING CONVENTION IS LOAD-BEARING. An owner search works by repository
// name, so a plugin lives in `infrena-provider-<name>`, its manifest says
// `name: <name>`, and its binary is `infrena-plugin-<name>`. That convention IS
// the registry: no index, no server, no publishing step, no account. The cost is
// that a plugin in a differently-named repository is findable only by naming the
// repository exactly, which is why the repository form of a source exists.
package plugins

import (
	"fmt"
	"strings"
)

// RepoPrefix is the prefix every plugin repository carries. An owner search
// finds plugins by listing repositories and matching this, so a repository
// without it is unreachable by name.
const RepoPrefix = "infrena-provider-"

// GitHubHost is the only forge implemented today. The source syntax is
// host-prefixed precisely so a second one can be added later without changing
// anything a user has already written down.
const GitHubHost = "github.com"

// Kind distinguishes the two forms a source may take.
type Kind int

const (
	// KindOwner is `<host>/<owner>`: search every plugin repository the owner
	// publishes.
	KindOwner Kind = iota
	// KindRepository is `<host>/<owner>/<repo>`: one exact repository.
	KindRepository
)

// String names the kind for diagnostics.
func (k Kind) String() string {
	switch k {
	case KindOwner:
		return "owner"
	case KindRepository:
		return "repository"
	default:
		return fmt.Sprintf("Kind(%d)", int(k))
	}
}

// Source is a place a plugin may come from: an owner whose repositories are
// searched by name, or one exact repository.
//
// It is a parsed value and nothing more. Resolving a source to a release, and
// downloading anything, belongs above this package, because this one must stay
// unable to reach the network.
type Source struct {
	// Host is the forge, always GitHubHost today.
	Host string
	// Owner is the user or organisation that owns the repositories.
	Owner string
	// Repo is the repository name for KindRepository, empty for KindOwner.
	Repo string
	// Kind says which of the two forms this is.
	Kind Kind
}

// ParseSource reads the source syntax from PLAN.md §31.3.
//
// Two forms are accepted: `<host>/<owner>` to search an owner, and
// `<host>/<owner>/<repo>` for one exact repository. A repository that breaks
// the `infrena-provider-<name>` convention is refused here, when it is written,
// rather than silently never matching an owner search later.
func ParseSource(s string) (Source, error) {
	if strings.Contains(s, "://") {
		return Source{}, fmt.Errorf("plugin source %q includes a scheme: write it as %s/<owner> or %s/<owner>/%s<name>, with no https:// prefix", s, GitHubHost, GitHubHost, RepoPrefix)
	}

	parts := strings.Split(s, "/")
	for _, part := range parts {
		if part == "" {
			return Source{}, fmt.Errorf("plugin source %q has an empty segment: write it as %s/<owner> to search an owner, or %s/<owner>/%s<name> for one repository", s, GitHubHost, GitHubHost, RepoPrefix)
		}
	}

	switch len(parts) {
	case 2, 3:
	default:
		return Source{}, fmt.Errorf("plugin source %q is not a source: write it as %s/<owner> to search an owner, or %s/<owner>/%s<name> for one repository", s, GitHubHost, GitHubHost, RepoPrefix)
	}

	host := parts[0]
	if host != GitHubHost {
		return Source{}, fmt.Errorf("plugin source %q names the host %q, which infrena does not support yet: only %s is implemented, so write the source as %s/<owner> or %s/<owner>/%s<name>", s, host, GitHubHost, GitHubHost, GitHubHost, RepoPrefix)
	}

	src := Source{Host: host, Owner: parts[1], Kind: KindOwner}
	if len(parts) == 2 {
		return src, nil
	}

	repo := parts[2]
	name, ok := strings.CutPrefix(repo, RepoPrefix)
	if !ok || name == "" {
		return Source{}, fmt.Errorf("plugin source %q names the repository %q, which does not follow the %s<name> convention: an owner search finds plugins by repository name, so a repository named otherwise could never be found; rename the repository, or check you have not written the binary name infrena-plugin-<name> by mistake", s, repo, RepoPrefix)
	}

	src.Kind = KindRepository
	src.Repo = repo
	return src, nil
}

// String round-trips exactly what the user wrote, so a diagnostic quotes the
// source back in the form it was given in.
func (s Source) String() string {
	if s.Kind == KindRepository {
		return s.Host + "/" + s.Owner + "/" + s.Repo
	}
	return s.Host + "/" + s.Owner
}

// PluginName returns the `<name>` in `infrena-provider-<name>` for a repository
// source, and false for an owner source, which names no single plugin.
func (s Source) PluginName() (string, bool) {
	if s.Kind != KindRepository {
		return "", false
	}
	return strings.CutPrefix(s.Repo, RepoPrefix)
}
