// Package version reports what this binary is and which formats it speaks.
package version

import (
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/infrena/infrena/pkg/semver"
)

// version is set at build time for a tagged release:
//
//	go build -ldflags "-X github.com/infrena/infrena/internal/version.version=0.4.1"
//
// Left empty by default, on purpose: a hand-maintained version constant is
// wrong by the second commit after a release, and a development build claiming
// to be a release sends every bug report about it to the wrong place.
var version string

// DevelopmentVersion is what a build with no version information calls itself.
const DevelopmentVersion = "0.0.0-dev"

// Version returns this build's version.
//
// From the linker flag if a release set one; otherwise from the module's own build
// information, which `go install github.com/infrena/infrena/cmd/infrena@v0.4.1`
// records without anyone having to remember. Only when neither exists — a plain
// `go build` from a checkout — does it admit to being a development build.
func Version() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := releaseVersion(info.Main.Version); v != "" {
			return v
		}
	}
	return DevelopmentVersion
}

// releaseVersion returns a module version if it names a release, and ""
// otherwise. A pseudo-version must not be treated as one: a project stating
// `infrena: ">= 0.4"` would then either refuse a developer's own build or
// accept it as a release that never existed.
//
// Two tests, and both are needed. An all-zero version is not something anybody
// releases, whatever produced it. But Go derives a pseudo-version from the
// nearest tag, so once a repository has one a build reports something like
// `v0.11.2-0.20260918163216-b7f0de604428`, which is not all zero and parses
// cleanly — hence the pre-release suffix is read as well.
func releaseVersion(v string) string {
	if v == "" || v == "(devel)" {
		return ""
	}
	v = strings.TrimPrefix(v, "v")
	parsed, err := semver.Parse(v)
	if err != nil {
		return ""
	}
	if parsed.Major == 0 && parsed.Minor == 0 && parsed.Patch == 0 {
		return ""
	}
	if isPseudoVersion(parsed.Pre) {
		return ""
	}
	// The parsed form, so what is displayed is what a constraint compares: a
	// tag of `v0.4` would otherwise print as "0.4" while comparing as 0.4.0,
	// and build metadata would print but take no part in ordering.
	return parsed.String()
}

// Semver is Version parsed, for comparing against a constraint.
//
// A version that does not parse compares as 0.0.0 rather than failing: a
// development build must not make `infrena: ">= 0.4"` fail with a message about
// the binary's own version string, which the user cannot act on. Being treated
// as older than everything is the honest reading.
func Semver() semver.Version {
	v, err := semver.Parse(Version())
	if err != nil {
		return semver.Version{}
	}
	return v
}

// Revision returns the VCS commit this was built from, or "" if unknown.
func Revision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			if len(s.Value) > 7 {
				return s.Value[:7]
			}
			return s.Value
		}
	}
	return ""
}

// Format is one versioned wire format this build speaks.
type Format struct {
	// Name identifies the format, as `infrena version` prints it.
	Name string `json:"name"`
	// Versions is every version understood, not merely the one written: the
	// plugin protocol negotiates over a set.
	Versions []int `json:"versions"`
}

// Info is everything `infrena version` reports.
type Info struct {
	Version  string   `json:"version"`
	Revision string   `json:"revision,omitempty"`
	Go       string   `json:"go"`
	Platform string   `json:"platform"`
	Formats  []Format `json:"formats"`
}

// Describe returns this build, given the formats its caller knows about.
//
// The formats are passed in rather than read here, and this package imports
// nothing engine-specific, because the compiler imports it to check a project's
// `infrena:` floor: reading a format constant from the planner here is an
// import cycle. The CLI assembles the list, since it already imports every
// package that owns a format.
func Describe(formats []Format) Info {
	return Info{
		Version:  Version(),
		Revision: Revision(),
		Go:       runtime.Version(),
		Platform: runtime.GOOS + "/" + runtime.GOARCH,
		Formats:  formats,
	}
}

// isPseudoVersion reports whether a pre-release suffix is the one Go synthesises
// for a commit that no tag names.
//
// The three forms Go produces differ in what precedes the timestamp and agree
// on what follows it:
//
//	vX.0.0-20260918163216-b7f0de604428          no earlier tag
//	vX.Y.Z-pre.0.20260918163216-b7f0de604428    earlier tag was a pre-release
//	vX.Y.Z+1-0.20260918163216-b7f0de604428      earlier tag was a release
//
// So the test is the tail: a 14-digit UTC timestamp, a dash, then exactly 12
// lower-case hex characters. Matching the tail rather than enumerating the
// three prefixes keeps a fourth form from silently reading as a release.
//
// It is deliberately narrow in the other direction too: `0.12.0-rc1`,
// `-alpha.2` and `-beta-3` each fail at least one part of the test, so a
// genuine pre-release keeps being reported as the release it is.
func isPseudoVersion(pre string) bool {
	// Build metadata first. semver.Parse drops a `+meta` suffix only when it
	// comes before the pre-release separator, and a dirty working tree produces
	// `v0.11.2-0.20260918004204-25cb98f3acb3+dirty`, where the `+` falls inside
	// the pre-release — so without this the revision reads as 18 characters
	// rather than 12 and the build reports itself as a release.
	if plus := strings.IndexByte(pre, '+'); plus >= 0 {
		pre = pre[:plus]
	}

	dash := strings.LastIndex(pre, "-")
	if dash < 0 {
		return false
	}
	revision := pre[dash+1:]
	if len(revision) != 12 {
		return false
	}
	for _, c := range revision {
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}

	// The timestamp is the last dot-separated field before the revision, which is
	// what makes the `pre.0.` and `0.` forms fall out without special-casing them.
	stamp := pre[:dash]
	if dot := strings.LastIndex(stamp, "."); dot >= 0 {
		stamp = stamp[dot+1:]
	}
	if len(stamp) != 14 {
		return false
	}
	for _, c := range stamp {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
