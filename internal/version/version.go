// Package version reports what this binary is and which formats it speaks.
// PLAN.md §61.
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
// Left EMPTY by default, on purpose. A hand-maintained version constant is wrong by
// the second commit after a release, and a development build claiming to be 0.4.1 is
// worse than one admitting it is a development build — it is what turns a bug report
// into an afternoon.
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

// releaseVersion returns a module version if it names a RELEASE, and "" otherwise.
//
// `go build` in a checkout with a VCS remote reports a pseudo-version —
// `0.0.0-20260913163216-b7f0de604428+dirty` — which is not a release and must not be
// treated as one: it parses as 0.0.0, and a project stating `infrena: ">= 0.4"` would
// then refuse a developer's own build. Measured on this repository, which is how the
// case was found.
//
// TWO TESTS, because the first one stopped being enough the day this repository
// grew a tag.
//
// The all-zero test came first: 0.0.0 is not a version anybody releases, so whatever
// produced it — a pseudo-version from an untagged repository, a broken CI stamp — the
// honest answer is "not a release". That was complete while `go build` could only
// produce `0.0.0-20260913163216-b7f0de604428`.
//
// It is not complete now. Go derives a pseudo-version from the NEAREST TAG, so a build
// from a commit after v0.11.1 reports `v0.11.2-0.20260918163216-b7f0de604428` — which
// is not all zero, parses cleanly, and was being reported as release 0.11.2: a version
// that has never existed, from a working tree, in the string a bug report quotes.
//
// So the suffix is now read as well. Go's three pseudo-version forms all end their
// pre-release with a 14-digit UTC timestamp and a 12-character revision, which nothing
// a human tags looks like — `0.12.0-rc1` has neither.
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
	// The PARSED form, so what is displayed is what a constraint compares: a tag of
	// `v0.4` would otherwise print as "0.4" while comparing as 0.4.0, and build
	// metadata would print but take no part in ordering.
	return parsed.String()
}

// Semver is Version parsed, for comparing against a constraint.
//
// A version that does not parse compares as 0.0.0 rather than failing: a development
// build must not be able to make `infrena: ">= 0.4"` fail with a message about the
// binary's own version string, which is a problem the user cannot act on. Being
// treated as older than everything is the honest reading — it is not a release.
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

// NOTHING ENGINE-SPECIFIC IS IMPORTED HERE, and that is load-bearing rather than
// tidy: the compiler checks a project's `infrena:` floor, so this package is imported
// by the compiler — and a Formats() living here that read planner.PlanVersion made an
// import cycle immediately. The list of formats is assembled by internal/cli, which
// already imports every package that owns one. See versionCommand.

// Format is one versioned wire format this build speaks.
type Format struct {
	Name string `json:"name"`
	// Versions is every version understood, not merely the one written. The plugin
	// protocol negotiates over a SET, and the others will when they need to.
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
// The formats are PASSED IN rather than read here, and that is load-bearing rather
// than tidy: the compiler checks a project's `infrena:` floor, so this package is
// imported by the compiler — and reading planner.PlanVersion here made an import cycle
// immediately. internal/cli assembles the list, since it already imports every package
// that owns a format.
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
// The three forms Go produces (cmd/go's pseudo.go) differ in what precedes the
// timestamp and agree on what follows it:
//
//	vX.0.0-20260918163216-b7f0de604428          no earlier tag
//	vX.Y.Z-pre.0.20260918163216-b7f0de604428    earlier tag was a pre-release
//	vX.Y.Z+1-0.20260918163216-b7f0de604428      earlier tag was a release
//
// So the test is the TAIL: a 14-digit UTC timestamp, then a dash, then exactly 12
// lower-case hex characters. Matching on the tail rather than enumerating the three
// prefixes is what keeps a fourth form from silently reading as a release.
//
// Deliberately narrow in the other direction too. `0.12.0-rc1`, `-alpha.2` and
// `-beta-3` are versions a person tags on purpose, and each fails at least one part
// of the test, so a genuine pre-release keeps being reported as the release it is.
func isPseudoVersion(pre string) bool {
	// BUILD METADATA FIRST, and this is not defensive tidying — it is the case that
	// made the real binary disagree with this function's own unit tests.
	//
	// semver.Parse drops a `+meta` suffix only when it comes before the pre-release
	// separator. A dirty working tree produces
	// `v0.11.2-0.20260918004204-25cb98f3acb3+dirty`, where the `+` falls INSIDE the
	// pre-release, so Pre arrives here ending `...acb3+dirty` and the revision reads
	// as 18 characters rather than 12. The tests passed; `infrena version` still
	// printed 0.11.2 for a working-tree build, which is the only place it mattered.
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
