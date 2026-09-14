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
