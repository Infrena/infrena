package plugins

import (
	"context"
	"fmt"
	"sort"

	"github.com/infrena/infrena/pkg/pluginmanifest"
	"github.com/infrena/infrena/pkg/semver"
)

// ManifestFile is the file a plugin repository publishes so a search can judge
// compatibility without downloading a binary. It is read at a tag, never at a
// branch.
const ManifestFile = "plugin.yaml"

// Fetcher is the forge, reduced to the three questions a search asks it.
//
// This interface is why search can live in a package that makes no network
// requests: declaring it here and satisfying it from remote.Client means Search
// depends on the shape of the calls, never on the code that makes them. The CLI
// is the one place that knows about both and wires them together.
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
// It never picks. Two owners can publish one name, and both come back: choosing
// silently would hand the user code from a place they did not choose, and the
// place is whose code runs on their machine.
//
// Failures are returned rather than dropped, and never turn a partial answer
// into an empty one: "found nothing" and "could not look" are different answers,
// and a rate limit on one source must not present another source's plugin as the
// whole truth. Every error is collected with the source that produced it, every
// other source is still searched, and the caller gets both halves.
//
// A candidate that cannot run here is still a candidate, returned with Usable
// false and a Reason.
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
// A repository source names one exactly and must not trigger an owner listing:
// that listing can fail for reasons unrelated to the repository they named.
//
// An owner source is listed and filtered to either prefix; filtering to one role
// would make the other invisible to everyone who trusts the owner. Every
// prefixed repository is read, rather than only `<prefix><name>`, because the
// manifest decides what a repository publishes and the name is only a hint —
// which costs a request each, and is what the disk cache above this is for.
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
	// reposOf only ever yields a repository carrying a prefix, so a failure
	// here is a caller bug rather than a user one.
	role, _, ok := RoleOf(repo)
	if !ok {
		return nil, fmt.Errorf("repository %q carries neither %s nor %s", repo, ProviderRepoPrefix, BackendRepoPrefix)
	}

	tag, err := f.LatestTag(ctx, src.Owner, repo)
	if err != nil {
		return nil, err
	}

	// At the tag, never the default branch: the branch describes unreleased
	// code, so judging compatibility from it would report a plugin as
	// compatible that nobody can install.
	data, err := f.FileAtTag(ctx, src.Owner, repo, tag, ManifestFile)
	if err != nil {
		return nil, err
	}

	m, warnings, err := pluginmanifest.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s at %s: %w", ManifestFile, tag, err)
	}

	// The manifest's name decides, not the repository's: the manifest is what
	// the binary and resource types are named after, so a repository whose name
	// merely looks right is not a match.
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
// Presentation only. Being first here is not being chosen: the ordering exists
// so a user comparing two runs sees the same table, not so a caller can take
// element zero.
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
