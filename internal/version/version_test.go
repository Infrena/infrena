package version

import (
	"strings"
	"testing"
)

// TestAPseudoVersionIsNotARelease.
//
// `go build` in a checkout with a VCS remote makes debug.ReadBuildInfo report a
// pseudo-version, which parses as 0.0.0. Reporting one as this build's version made
// every project stating an `infrena:` floor refuse a developer's own build — measured
// on this repository, which is how the case was found.
func TestAPseudoVersionIsNotARelease(t *testing.T) {
	for _, in := range []string{
		"",
		"(devel)",
		"v0.0.0-20260913163216-b7f0de604428+dirty",
		"0.0.0",
		"0.0.0-dev",
		"not-a-version",
	} {
		if got := releaseVersion(in); got != "" {
			t.Errorf("releaseVersion(%q) = %q, want \"\" — that is not a release", in, got)
		}
	}
}

// TestARealTagIsAReleaseAndLosesItsV. `go install …@v0.4.1` records `v0.4.1`, and the
// `v` is Go's convention rather than something a user types or reads.
func TestARealTagIsAReleaseAndLosesItsV(t *testing.T) {
	for in, want := range map[string]string{
		"v0.4.1":       "0.4.1",
		"0.4.1":        "0.4.1",
		"v1.0.0":       "1.0.0",
		"v0.1.0-rc1":   "0.1.0-rc1",
		"v2.3.4+meta7": "2.3.4",
	} {
		if got := releaseVersion(in); got != want {
			t.Errorf("releaseVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestADevelopmentBuildSaysSo. A development build claiming to be a release is what
// turns a bug report into an afternoon.
func TestADevelopmentBuildSaysSo(t *testing.T) {
	// The test binary itself is one, which is why this can assert on Version()
	// directly: nothing sets the linker flag for `go test`.
	if v := Version(); v != DevelopmentVersion {
		t.Errorf("Version() = %q under `go test`, want %q", v, DevelopmentVersion)
	}
	if !strings.Contains(DevelopmentVersion, "dev") {
		t.Errorf("DevelopmentVersion = %q; it has to be recognisable as one at a glance",
			DevelopmentVersion)
	}
	// And it parses, so nothing downstream has to special-case the string to compare
	// it. It compares as 0.0.0, which is what makes every floor exempt it.
	if v := Semver(); v.Major != 0 || v.Minor != 0 || v.Patch != 0 {
		t.Errorf("Semver() = %v, want 0.0.0", v)
	}
}

// TestDescribeCarriesTheFormatsItIsGiven, and does not go looking for them itself —
// which is what keeps this package importable by the compiler without a cycle.
func TestDescribeCarriesTheFormatsItIsGiven(t *testing.T) {
	info := Describe([]Format{{Name: "state", Versions: []int{7}}})
	if len(info.Formats) != 1 || info.Formats[0].Versions[0] != 7 {
		t.Errorf("Formats = %+v, want what was passed in", info.Formats)
	}
	if info.Go == "" || info.Platform == "" {
		t.Errorf("incomplete: %+v", info)
	}
}

// TestAPseudoVersionFromATaggedRepositoryIsNotARelease is the case the
// all-zero test could not see, and it only became reachable when this
// repository grew its first tag.
//
// Go derives a pseudo-version from the NEAREST TAG, so once v0.11.1 existed, a
// build from any later commit reported v0.11.2-0.<stamp>-<rev>. That is not all
// zero, it parses cleanly, and it was answered as release 0.11.2 — a version
// that has never been published, reported by a binary built from a working
// tree, in exactly the string a bug report quotes.
func TestAPseudoVersionFromATaggedRepositoryIsNotARelease(t *testing.T) {
	for _, v := range []string{
		// After a release tag. The form that broke, and the reason for this test.
		"v0.11.2-0.20260918163216-b7f0de604428",
		// After a pre-release tag.
		"v0.12.0-rc1.0.20260918163216-b7f0de604428",
		// No earlier tag at all — the form the all-zero check already caught,
		// kept here so both routes stay covered by one table.
		"v0.0.0-20260913163216-b7f0de604428",
		// Uppercase v, as build info sometimes carries it.
		"0.11.2-0.20260918163216-b7f0de604428",
		// A DIRTY WORKING TREE, which is what a developer actually builds and the
		// case that caught this function lying while its own tests passed. The `+`
		// lands inside the pre-release rather than being dropped by the parser, so
		// the revision reads as 18 characters unless metadata is stripped first.
		"v0.11.2-0.20260918004204-25cb98f3acb3+dirty",
	} {
		if got := releaseVersion(v); got != "" {
			t.Errorf("releaseVersion(%q) = %q, want \"\" — a pseudo-version is a build from a "+
				"commit no tag names, and reporting one as a release names a version that does not exist", v, got)
		}
	}
}

// TestAGenuinePreReleaseIsStillARelease is the inverse, and the pair is what
// discriminates: a check that answered "" for everything would satisfy the test
// above perfectly while making `infrena version` useless on every release
// candidate anyone ever tags.
func TestAGenuinePreReleaseIsStillARelease(t *testing.T) {
	for v, want := range map[string]string{
		"v0.12.0-rc1":     "0.12.0-rc1",
		"v0.12.0-alpha.2": "0.12.0-alpha.2",
		// A dash inside the pre-release, which the tail test must not mistake
		// for a revision separator.
		"v0.12.0-beta-3": "0.12.0-beta-3",
		"v0.11.1":        "0.11.1",
	} {
		if got := releaseVersion(v); got != want {
			t.Errorf("releaseVersion(%q) = %q, want %q — this is a tag a person made on purpose", v, got, want)
		}
	}
}
