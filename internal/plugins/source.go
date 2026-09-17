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
// name, so a provider lives in `infrena-provider-<name>`, its manifest says
// `name: <name>`, and its binary is `infrena-plugin-<name>`; a state backend
// lives in `infrena-backend-<name>` and ships `infrena-backend-<name>`. That
// convention IS the registry: no index, no server, no publishing step, no
// account. The cost is that a plugin in a differently-named repository is
// findable only by naming the repository exactly, which is why the repository
// form of a source exists.
package plugins

import (
	"fmt"
	"strings"
)

// ProviderRepoPrefix and BackendRepoPrefix are the two prefixes a plugin
// repository may carry, one per Role. An owner search finds plugins by listing
// repositories and matching these, so a repository carrying neither is
// unreachable by name.
const (
	ProviderRepoPrefix = "infrena-provider-"
	BackendRepoPrefix  = "infrena-backend-"
)

// RepoPrefix is the old name for ProviderRepoPrefix, from before a repository
// could name a backend.
//
// Deprecated: say which role you mean, ProviderRepoPrefix or
// BackendRepoPrefix.
const RepoPrefix = ProviderRepoPrefix

// Role is what a plugin DOES: serve resources, or store state.
//
// NOT called Kind, because Kind is taken in this package and means something
// else entirely - whether a source names an owner or one repository. The
// user-facing flag is still `--kind`, since that is the word a person types.
type Role int

const (
	// RoleProvider serves resources: `infrena-provider-<name>`, shipping the
	// binary `infrena-plugin-<name>`.
	RoleProvider Role = iota
	// RoleBackend stores state: `infrena-backend-<name>`, shipping the binary
	// `infrena-backend-<name>`.
	RoleBackend
)

// String names the role for diagnostics, and is what the KIND column shows.
func (r Role) String() string {
	switch r {
	case RoleProvider:
		return "provider"
	case RoleBackend:
		return "backend"
	default:
		return fmt.Sprintf("Role(%d)", int(r))
	}
}

// RoleOf reads the role a repository name declares, and the `<name>` after the
// prefix. It reports false for a repository carrying neither prefix, which is
// one an owner search could never find.
//
// THE PREFIX DECIDES THE ROLE, THE MANIFEST DECIDES THE NAME. A repository
// called `infrena-backend-s3` whose manifest says `name: something-else` is a
// backend that is not called s3, and so is not a match for s3.
func RoleOf(repo string) (Role, string, bool) {
	if name, ok := strings.CutPrefix(repo, ProviderRepoPrefix); ok && name != "" {
		return RoleProvider, name, true
	}
	if name, ok := strings.CutPrefix(repo, BackendRepoPrefix); ok && name != "" {
		return RoleBackend, name, true
	}
	return 0, "", false
}

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
	// Role is what the repository name says the plugin does, for
	// KindRepository. It is MEANINGLESS for KindOwner and left at its zero
	// value there, because an owner publishes both roles and a source naming
	// one claims nothing about either.
	Role Role
}

// ParseSource reads the source syntax from PLAN.md §31.3.
//
// Two forms are accepted: `<host>/<owner>` to search an owner, and
// `<host>/<owner>/<repo>` for one exact repository. A repository that breaks
// the convention, carrying neither the provider nor the backend prefix, is
// refused here, when it is written, rather than silently never matching an
// owner search later.
func ParseSource(s string) (Source, error) {
	if strings.Contains(s, "://") {
		return Source{}, fmt.Errorf("plugin source %q includes a scheme: write it as %s/<owner> or %s/<owner>/%s<name>, with no https:// prefix", s, GitHubHost, GitHubHost, ProviderRepoPrefix)
	}

	parts := strings.Split(s, "/")
	for _, part := range parts {
		if part == "" {
			return Source{}, fmt.Errorf("plugin source %q has an empty segment: write it as %s/<owner> to search an owner, or %s/<owner>/%s<name> for one repository", s, GitHubHost, GitHubHost, ProviderRepoPrefix)
		}
	}

	switch len(parts) {
	case 2, 3:
	default:
		return Source{}, fmt.Errorf("plugin source %q is not a source: write it as %s/<owner> to search an owner, or %s/<owner>/%s<name> for one repository", s, GitHubHost, GitHubHost, ProviderRepoPrefix)
	}

	host := parts[0]
	if host != GitHubHost {
		return Source{}, fmt.Errorf("plugin source %q names the host %q, which infrena does not support yet: only %s is implemented, so write the source as %s/<owner> or %s/<owner>/%s<name>", s, host, GitHubHost, GitHubHost, GitHubHost, ProviderRepoPrefix)
	}

	src := Source{Host: host, Owner: parts[1], Kind: KindOwner}
	if len(parts) == 2 {
		return src, nil
	}

	repo := parts[2]
	role, _, ok := RoleOf(repo)
	if !ok {
		return Source{}, fmt.Errorf("plugin source %q names the repository %q, which follows neither the %s<name> nor the %s<name> convention: an owner search finds plugins by repository name, so a repository named otherwise could never be found; rename the repository, or check you have not written a binary name such as infrena-plugin-<name> by mistake", s, repo, ProviderRepoPrefix, BackendRepoPrefix)
	}

	src.Kind = KindRepository
	src.Repo = repo
	src.Role = role
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

// PluginName returns the `<name>` after whichever prefix the repository
// carries, and false for an owner source, which names no single plugin.
func (s Source) PluginName() (string, bool) {
	if s.Kind != KindRepository {
		return "", false
	}
	_, name, ok := RoleOf(s.Repo)
	return name, ok
}
