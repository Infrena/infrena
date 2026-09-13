// Package version reports what this binary is and which formats it speaks.
// PLAN.md §61.
package version

import (
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/infrata/infrata/internal/semver"
)

// version is set at build time for a tagged release:
//
//	go build -ldflags "-X github.com/infrata/infrata/internal/version.version=0.4.1"
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
// information, which `go install github.com/infrata/infrata/cmd/infrata@v0.4.1`
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
// treated as one: it parses as 0.0.0, and a project stating `infrata: ">= 0.4"` would
// then refuse a developer's own build. Measured on this repository, which is how the
// case was found.
//
// The test is the MAJOR.MINOR.PATCH being all zero rather than the shape of the suffix.
// 0.0.0 is not a version anybody releases, so whatever produced it — a pseudo-version,
// a broken CI stamp — the honest answer is "this is not a release".
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
	// The PARSED form, so what is displayed is what a constraint compares: a tag of
	// `v0.4` would otherwise print as "0.4" while comparing as 0.4.0, and build
	// metadata would print but take no part in ordering.
	return parsed.String()
}

// Semver is Version parsed, for comparing against a constraint.
//
// A version that does not parse compares as 0.0.0 rather than failing: a development
// build must not be able to make `infrata: ">= 0.4"` fail with a message about the
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
// tidy: the compiler checks a project's `infrata:` floor, so this package is imported
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

// Info is everything `infrata version` reports.
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
// than tidy: the compiler checks a project's `infrata:` floor, so this package is
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
