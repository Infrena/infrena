package plugins

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/infrena/infrena/internal/plugins/remote"
	"github.com/infrena/infrena/pkg/pluginmanifest"
)

// fakeFetcher answers the three calls Search makes, from maps.
//
// Search takes a Fetcher so that it depends on the shape of the forge calls and
// never on making one, which is what lets every test here run with no network
// at all and what keeps this package's dependency graph free of net/http.
type fakeFetcher struct {
	// repos answers Repositories, keyed by owner.
	repos map[string][]string
	// tags answers LatestTag, keyed "owner/repo".
	tags map[string]string
	// files answers FileAtTag, keyed "owner/repo@tag".
	files map[string][]byte
	// failOwner makes Repositories fail for an owner, so a source that cannot
	// be read can be told from one that answered nothing.
	failOwner map[string]error

	// repoCalls counts owner listings, so a test can assert that a repository
	// source never triggers one.
	repoCalls int
}

func (f *fakeFetcher) Repositories(_ context.Context, owner string) ([]string, error) {
	f.repoCalls++
	if err, ok := f.failOwner[owner]; ok {
		return nil, err
	}
	repos, ok := f.repos[owner]
	if !ok {
		return nil, &remote.NotFoundError{What: "owner " + owner}
	}
	return repos, nil
}

func (f *fakeFetcher) LatestTag(_ context.Context, owner, repo string) (string, error) {
	tag, ok := f.tags[owner+"/"+repo]
	if !ok {
		return "", &remote.NotFoundError{What: "a release of " + owner + "/" + repo}
	}
	return tag, nil
}

func (f *fakeFetcher) FileAtTag(_ context.Context, owner, repo, tag, path string) ([]byte, error) {
	if path != ManifestFile {
		return nil, fmt.Errorf("asked for %q, want %q", path, ManifestFile)
	}
	data, ok := f.files[owner+"/"+repo+"@"+tag]
	if !ok {
		return nil, &remote.NotFoundError{What: path + " in " + owner + "/" + repo + " at " + tag}
	}
	return data, nil
}

// manifestBytes writes a plugin.yaml that is usable in hereEnv, so a test that
// is not about compatibility does not have to think about it.
func manifestBytes(t *testing.T, name, version string) []byte {
	t.Helper()
	return []byte(fmt.Sprintf(`manifest: 2
name: %s
version: %s
protocol: [4]
platforms: [darwin/arm64, linux/amd64]
description: a plugin, for a test
`, name, version))
}

// hereEnv is a machine every manifestBytes manifest runs on. Fixed rather than
// read from runtime, so these tests answer the same on every developer's laptop
// and in CI.
func hereEnv() Environment {
	return Environment{
		Platform:  pluginmanifest.Platform{OS: "darwin", Arch: "arm64"},
		Protocols: []int{4, 3},
	}
}

func mustSource(t *testing.T, s string) Source {
	t.Helper()
	src, err := ParseSource(s)
	if err != nil {
		t.Fatalf("ParseSource(%q): %v", s, err)
	}
	return src
}

// Two owners can both answer one name, and infrena must present every match and
// never auto-pick - not the first alphabetically, not the higher version, not
// the official one. Choosing silently when two candidates both answer what the
// user asked gives them a thing they did not name.
func TestTwoOwnersPublishingOneNameBothSurvive(t *testing.T) {
	f := &fakeFetcher{
		repos: map[string][]string{
			"infrena": {"infrena-provider-hetzner"},
			"mycorp":  {"infrena-provider-hetzner", "website"},
		},
		tags: map[string]string{"infrena/infrena-provider-hetzner": "v1.0.0", "mycorp/infrena-provider-hetzner": "v2.0.0"},
		files: map[string][]byte{
			"infrena/infrena-provider-hetzner@v1.0.0": manifestBytes(t, "hetzner", "1.0.0"),
			"mycorp/infrena-provider-hetzner@v2.0.0":  manifestBytes(t, "hetzner", "2.0.0"),
		},
	}
	sources := []Source{mustSource(t, "github.com/infrena"), mustSource(t, "github.com/mycorp")}

	got, errs := Search(context.Background(), f, sources, "hetzner", hereEnv())
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}

	if len(got) != 2 {
		t.Fatalf("Search returned %d candidates, want both: %+v", len(got), got)
	}

	// The official owner does not win, and neither does the higher version:
	// both are present, and the caller is the one that has to ask.
	seen := map[string]string{}
	for _, c := range got {
		seen[c.Source.String()] = c.Manifest.Version.String()
	}
	if seen["github.com/infrena"] != "1.0.0" || seen["github.com/mycorp"] != "2.0.0" {
		t.Errorf("candidates = %v, want one from each source", seen)
	}
}

// A repository source names one repository exactly and must not trigger an
// owner listing.
func TestARepositorySourceIsNotSearchedAsAnOwner(t *testing.T) {
	f := &fakeFetcher{
		tags:  map[string]string{"someone/infrena-provider-hetzner": "v1.0.0"},
		files: map[string][]byte{"someone/infrena-provider-hetzner@v1.0.0": manifestBytes(t, "hetzner", "1.0.0")},
	}

	got, errs := Search(context.Background(), f,
		[]Source{mustSource(t, "github.com/someone/infrena-provider-hetzner")}, "hetzner", hereEnv())

	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1", len(got))
	}
	if f.repoCalls != 0 {
		t.Errorf("Repositories was called %d times for a repository source", f.repoCalls)
	}
}

// A rate limit from one source must not be silently swallowed, and must not be
// turned into "nothing found" for the whole search.
func TestARateLimitFromOneSourceIsReportedNotSwallowed(t *testing.T) {
	f := &fakeFetcher{
		repos:     map[string][]string{"mycorp": {"infrena-provider-hetzner"}},
		tags:      map[string]string{"mycorp/infrena-provider-hetzner": "v2.0.0"},
		files:     map[string][]byte{"mycorp/infrena-provider-hetzner@v2.0.0": manifestBytes(t, "hetzner", "2.0.0")},
		failOwner: map[string]error{"infrena": &remote.RateLimitError{Resets: time.Now().Add(time.Hour)}},
	}
	sources := []Source{mustSource(t, "github.com/infrena"), mustSource(t, "github.com/mycorp")}

	got, errs := Search(context.Background(), f, sources, "hetzner", hereEnv())

	if len(errs) == 0 {
		t.Fatal("a rate limit was swallowed")
	}
	// The other source still answered, so a partial result is still returned -
	// but the caller is told the answer is partial.
	if len(got) != 1 {
		t.Errorf("got %d candidates, want the one that succeeded", len(got))
	}
	// The reported error still says which source failed AND still reads as a
	// rate limit, because "not found" is the message this must never become.
	joined := fmt.Sprint(errs)
	if !strings.Contains(joined, "github.com/infrena") {
		t.Errorf("errors %v do not name the source that failed", errs)
	}
	if !strings.Contains(joined, "rate limit") {
		t.Errorf("errors %v no longer read as a rate limit", errs)
	}
}

// A manifest whose name does not match what was asked for is not a match, even
// though the repository name suggested it would be. The manifest is the truth.
func TestTheManifestNameDecidesTheMatch(t *testing.T) {
	f := &fakeFetcher{
		repos: map[string][]string{"mycorp": {"infrena-provider-hetzner"}},
		tags:  map[string]string{"mycorp/infrena-provider-hetzner": "v1.0.0"},
		files: map[string][]byte{"mycorp/infrena-provider-hetzner@v1.0.0": manifestBytes(t, "somethingelse", "1.0.0")},
	}

	got, _ := Search(context.Background(), f, []Source{mustSource(t, "github.com/mycorp")}, "hetzner", hereEnv())

	if len(got) != 0 {
		t.Errorf("got %+v, want no match when the manifest names a different plugin", got)
	}
}

// A candidate that cannot run here is still returned, with its reason: "no
// build for your machine" and "no such plugin" are different answers, and
// dropping the first would render it as the second.
func TestAnIncompatibleCandidateIsKeptWithItsReason(t *testing.T) {
	f := &fakeFetcher{
		repos: map[string][]string{"mycorp": {"infrena-provider-hetzner"}},
		tags:  map[string]string{"mycorp/infrena-provider-hetzner": "v1.0.0"},
		files: map[string][]byte{"mycorp/infrena-provider-hetzner@v1.0.0": []byte(`manifest: 2
name: hetzner
version: 1.0.0
protocol: [4]
platforms: [linux/amd64]
description: no build for the machine asking
`)},
	}

	got, errs := Search(context.Background(), f, []Source{mustSource(t, "github.com/mycorp")}, "hetzner", hereEnv())
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want the incompatible one kept", len(got))
	}
	if got[0].Usable {
		t.Fatal("a plugin with no build for this machine was called usable")
	}
	if !strings.Contains(got[0].Reason, "darwin/arm64") {
		t.Errorf("reason = %q, want the platform named", got[0].Reason)
	}
	if got[0].Tag != "v1.0.0" {
		t.Errorf("Tag = %q, want the tag the manifest was read at", got[0].Tag)
	}
}

// Results are ordered by source and then newest version first, so two runs of
// one search render identically. The ordering is presentation only: being first
// here is not being chosen, and nothing downstream may read it as a pick.
func TestResultsAreOrderedDeterministically(t *testing.T) {
	f := &fakeFetcher{
		repos: map[string][]string{
			"mycorp":  {"infrena-provider-hetzner"},
			"acme":    {"infrena-provider-hetzner"},
			"infrena": {"infrena-provider-hetzner"},
		},
		tags: map[string]string{
			"mycorp/infrena-provider-hetzner":  "v2.0.0",
			"acme/infrena-provider-hetzner":    "v3.0.0",
			"infrena/infrena-provider-hetzner": "v1.0.0",
		},
		files: map[string][]byte{
			"mycorp/infrena-provider-hetzner@v2.0.0":  manifestBytes(t, "hetzner", "2.0.0"),
			"acme/infrena-provider-hetzner@v3.0.0":    manifestBytes(t, "hetzner", "3.0.0"),
			"infrena/infrena-provider-hetzner@v1.0.0": manifestBytes(t, "hetzner", "1.0.0"),
		},
	}
	// Deliberately not in sorted order, so the ordering cannot come from the
	// order the sources were given in.
	sources := []Source{
		mustSource(t, "github.com/mycorp"),
		mustSource(t, "github.com/infrena"),
		mustSource(t, "github.com/acme"),
	}

	got, errs := Search(context.Background(), f, sources, "hetzner", hereEnv())
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}

	var order []string
	for _, c := range got {
		order = append(order, c.Source.String())
	}
	want := []string{"github.com/acme", "github.com/infrena", "github.com/mycorp"}
	if fmt.Sprint(order) != fmt.Sprint(want) {
		t.Errorf("order = %v, want %v", order, want)
	}
}

// A repository that has no release at all is REPORTED, not dropped. A user who
// named that repository as a source is asking about it specifically, and
// silence would read as "there is no such plugin".
func TestARepositorySourceWithNoReleaseIsReported(t *testing.T) {
	f := &fakeFetcher{}

	got, errs := Search(context.Background(), f,
		[]Source{mustSource(t, "github.com/someone/infrena-provider-hetzner")}, "hetzner", hereEnv())

	if len(got) != 0 {
		t.Errorf("got %+v, want nothing", got)
	}
	if len(errs) == 0 {
		t.Fatal("a source that could not be read was swallowed")
	}
	if !strings.Contains(fmt.Sprint(errs), "github.com/someone/infrena-provider-hetzner") {
		t.Errorf("errors %v do not name the source", errs)
	}
}

// A plugin repository in an owner listing that cannot be read must not cost the
// owner's other plugins. The unreadable one is still reported - it is an error,
// not a silence - but the owner's released plugin still comes back.
func TestARepositoryWithoutAReleaseDoesNotFailTheWholeOwner(t *testing.T) {
	f := &fakeFetcher{
		repos: map[string][]string{"mycorp": {"infrena-provider-hetzner", "infrena-provider-unreleased"}},
		tags:  map[string]string{"mycorp/infrena-provider-hetzner": "v2.0.0"},
		files: map[string][]byte{"mycorp/infrena-provider-hetzner@v2.0.0": manifestBytes(t, "hetzner", "2.0.0")},
	}

	got, _ := Search(context.Background(), f, []Source{mustSource(t, "github.com/mycorp")}, "hetzner", hereEnv())

	if len(got) != 1 {
		t.Fatalf("got %d candidates, want the released one: %+v", len(got), got)
	}
}

// An owner search must list BOTH prefixes, or a backend published by a
// trusted owner is invisible.
func TestSearchFindsABackendPublishedByAnOwner(t *testing.T) {
	f := &fakeFetcher{
		repos: map[string][]string{"mycorp": {"infrena-backend-s3", "infrena-provider-aws", "website"}},
		// The provider is released too, because a repository in an owner
		// listing with no release is a reported error here by design, and this
		// test is about what the listing KEEPS: both prefixes, and not
		// "website".
		tags: map[string]string{
			"mycorp/infrena-backend-s3":   "v1.0.0",
			"mycorp/infrena-provider-aws": "v1.0.0",
		},
		files: map[string][]byte{
			"mycorp/infrena-backend-s3@v1.0.0":   manifestBytes(t, "s3", "1.0.0"),
			"mycorp/infrena-provider-aws@v1.0.0": manifestBytes(t, "aws", "1.0.0"),
		},
	}

	got, errs := Search(context.Background(), f, []Source{mustSource(t, "github.com/mycorp")}, "s3", hereEnv())
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(got), got)
	}
	if got[0].Role != RoleBackend {
		t.Errorf("Role = %v, want RoleBackend", got[0].Role)
	}
}

// A provider and a backend of one name both survive: they are two different
// plugins, and neither may be dropped for the other.
func TestAProviderAndABackendOfOneNameBothSurvive(t *testing.T) {
	f := &fakeFetcher{
		repos: map[string][]string{"mycorp": {"infrena-provider-s3", "infrena-backend-s3"}},
		tags: map[string]string{
			"mycorp/infrena-provider-s3": "v2.0.0",
			"mycorp/infrena-backend-s3":  "v1.0.0",
		},
		files: map[string][]byte{
			"mycorp/infrena-provider-s3@v2.0.0": manifestBytes(t, "s3", "2.0.0"),
			"mycorp/infrena-backend-s3@v1.0.0":  manifestBytes(t, "s3", "1.0.0"),
		},
	}

	got, _ := Search(context.Background(), f, []Source{mustSource(t, "github.com/mycorp")}, "s3", hereEnv())

	if len(got) != 2 {
		t.Fatalf("got %d candidates, want both: %+v", len(got), got)
	}
	roles := map[Role]bool{}
	for _, c := range got {
		roles[c.Role] = true
	}
	if !roles[RoleProvider] || !roles[RoleBackend] {
		t.Errorf("roles found = %v, want both", roles)
	}
}
