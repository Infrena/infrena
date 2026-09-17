package plugins

import (
	"context"
	"fmt"
	"sort"

	"github.com/infrena/infrena/pkg/pluginmanifest"
	"github.com/infrena/infrena/pkg/semver"
)

// ManifestFile is the file a plugin repository publishes so a search can judge
// compatibility WITHOUT downloading a binary (§31.2). It is read at a tag,
// never at a branch.
const ManifestFile = "plugin.yaml"

// Fetcher is the forge, reduced to the three questions a search asks it.
//
// THIS INTERFACE IS WHY SEARCH LIVES IN THE NETWORK-FREE PACKAGE. PLAN.md
// §31.3 makes "the network is never on the hot path" a hard rule, and
// network_test.go checks it by reading this package's dependency graph. An
// interface declared HERE and satisfied by remote.Client up there means Search
// depends on the SHAPE of the calls and never on the code that makes them, so
// importing remote from this file — which would drag net/http in and fail that
// test — is not merely discouraged, it is unnecessary. The CLI is the one place
// that knows about both, and it wires the two together.
//
// It is also what makes Search testable: every test here runs against a fake,
// with no server and no network at all.
type Fetcher interface {
	// Repositories lists every repository an owner has, unfiltered. The
	// convention is applied by the caller, not the transport.
	Repositories(ctx context.Context, owner string) ([]string, error)
	// LatestTag resolves the newest release tag of a repository.
	LatestTag(ctx context.Context, owner, repo string) (string, error)
	// FileAtTag reads one file at a tag.
	FileAtTag(ctx context.Context, owner, repo, tag, path string) ([]byte, error)
}

// Search finds every release named name across every source, and says of each
// whether it can run here.
//
// IT NEVER PICKS (§31.3). Two owners can publish one name, and both come back:
// not the first alphabetically, not the higher version, not the official one.
// Choosing silently when two candidates both answer what the user asked hands
// them a thing they did not name, from a place they did not choose, and the
// place is the part that matters — it is whose code runs on their machine.
//
// FAILURES ARE RETURNED, NOT THROWN AWAY, AND NEVER TURN A PARTIAL ANSWER INTO
// AN EMPTY ONE. This is discovery.Walk's rule, for the same reason: "found
// nothing" and "could not look" are different answers, and a partial answer a
// reader mistakes for a complete one is the failure worth avoiding. A rate
// limit on one source must not be able to report the other source's plugin as
// the whole truth, and must not be able to report nothing at all. So every
// error is collected with the source that produced it, every other source is
// still searched, and the caller gets both halves.
//
// A candidate that cannot run here is still a candidate: it comes back with
// Usable false and a Reason, because a plugin that exists but publishes no
// build for this machine is a different answer from no plugin at all.
func Search(ctx context.Context, f Fetcher, sources []Source, name string, env Environment) ([]Candidate, []error) {
	var out []Candidate
	var problems []error

	for _, src := range sources {
		repos, err := reposOf(ctx, f, src)
		if err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", src, err))
			continue
		}

		for _, repo := range repos {
			c, err := candidate(ctx, f, src, repo, name, env)
			if err != nil {
				problems = append(problems, fmt.Errorf("%s/%s: %w", src.Host+"/"+src.Owner, repo, err))
				continue
			}
			if c == nil {
				continue
			}
			out = append(out, *c)
		}
	}

	sortCandidates(out)
	return out, problems
}

// reposOf turns a source into the repositories worth reading a manifest from.
//
// A repository source names one exactly and must NOT trigger an owner listing:
// the user already said which repository they mean, and listing an owner to
// rediscover it costs a request and can fail for reasons that have nothing to
// do with the repository they named.
//
// An owner source is listed and filtered to EITHER prefix, which is the whole
// registry (source.go): no index, no server, no publishing step. Both are kept
// because an owner publishes providers and backends alike, and filtering to one
// would make the other invisible to everyone who trusts the owner. Everything
// with a prefix is then read, rather than only `<prefix><name>`, because the
// MANIFEST decides what a repository publishes and the repository name is only
// a hint. That costs a request per plugin repository the owner has, which is
// what the disk cache above this package is for.
func reposOf(ctx context.Context, f Fetcher, src Source) ([]string, error) {
	if src.Kind == KindRepository {
		return []string{src.Repo}, nil
	}

	all, err := f.Repositories(ctx, src.Owner)
	if err != nil {
		return nil, err
	}

	var repos []string
	for _, repo := range all {
		if _, _, ok := RoleOf(repo); ok {
			repos = append(repos, repo)
		}
	}
	return repos, nil
}

// candidate reads one repository's manifest at its latest tag and judges it.
//
// A nil candidate with a nil error means "this repository publishes something
// else": a match, not a failure, that simply is not the thing asked for.
func candidate(ctx context.Context, f Fetcher, src Source, repo, name string, env Environment) (*Candidate, error) {
	// THE PREFIX DECIDES THE ROLE. reposOf only ever yields a repository
	// carrying one, so a failure here is a caller bug rather than a user one.
	role, _, ok := RoleOf(repo)
	if !ok {
		return nil, fmt.Errorf("repository %q carries neither %s nor %s", repo, ProviderRepoPrefix, BackendRepoPrefix)
	}

	tag, err := f.LatestTag(ctx, src.Owner, repo)
	if err != nil {
		return nil, err
	}

	// AT THE TAG, NEVER THE DEFAULT BRANCH (§31.2). The default branch
	// describes unreleased code, so judging compatibility from it would report
	// a plugin as compatible that nobody can install.
	data, err := f.FileAtTag(ctx, src.Owner, repo, tag, ManifestFile)
	if err != nil {
		return nil, err
	}

	m, warnings, err := pluginmanifest.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s at %s: %w", ManifestFile, tag, err)
	}

	// THE MANIFEST'S NAME DECIDES, not the repository's. The convention puts
	// `<name>` in the repository name, but the manifest is what the plugin says
	// about itself and what its binary and resource types are named after, so a
	// repository whose name merely looks right is not a match.
	if m.Name != name {
		return nil, nil
	}

	usable, reason := Check(m, env)
	return &Candidate{
		Source:   src,
		Repo:     repo,
		Role:     role,
		Manifest: m,
		Tag:      tag,
		Usable:   usable,
		Reason:   reason,
		Warnings: warnings,
	}, nil
}

// sortCandidates orders results so repeated searches render identically: by
// source, then newest version first.
//
// PRESENTATION ONLY. Being first here is not being chosen, and nothing
// downstream may read it as a pick — the ordering exists so a user comparing
// two runs sees the same table, not so a caller can take element zero.
func sortCandidates(cs []Candidate) {
	sort.SliceStable(cs, func(i, j int) bool {
		a, b := cs[i], cs[j]
		if as, bs := a.Source.String(), b.Source.String(); as != bs {
			return as < bs
		}
		if c := semver.Compare(a.Manifest.Version, b.Manifest.Version); c != 0 {
			return c > 0
		}
		return a.Tag < b.Tag
	})
}
