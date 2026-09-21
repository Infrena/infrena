package plugins

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/infrena/infrena/pkg/pluginmanifest"
	"github.com/infrena/infrena/pkg/semver"
)

// Environment is everything about the asking side that decides whether a
// plugin can run here: this machine, this build, and what the project asked
// for.
//
// It is a value passed in rather than read from the process, so that Check
// stays pure logic over a parsed manifest and can be tested for platforms other
// than the one the test runs on.
type Environment struct {
	// Platform is the GOOS/GOARCH asking, normally runtime.GOOS and
	// runtime.GOARCH.
	Platform pluginmanifest.Platform
	// Protocols is every plugin protocol version this build of infrena speaks.
	// The whole set, because the handshake negotiates.
	Protocols []int
	// InfrenaVersion is this build's version, checked against the manifest's
	// `infrena:` floor. Empty, or a development build, is exempt — see
	// Manifest.AllowsInfrena.
	InfrenaVersion string
	// Constraint is the range the project asked for in `plugins:`, when it
	// named one. The zero value means the project said nothing.
	Constraint semver.Constraint
}

// Candidate is one plugin release found by a search, together with the verdict
// on whether it can run here.
//
// A rejected candidate is still a candidate: it is kept, with its reason,
// because "this plugin exists but publishes no build for your machine" and "no
// plugin by that name exists anywhere" are different answers, and dropping the
// first would render it as the second.
type Candidate struct {
	// Source is where this release was found, as the user wrote it: an owner
	// source stays an owner source. Two sources may answer one name, and both
	// are kept.
	Source Source
	// Repo is the repository the manifest was read from. Kept because an owner
	// source does not name one and the manifest's name is what identifies a
	// plugin, so the repository is otherwise unrecoverable.
	Repo string
	// Role is what the repository name says this plugin does, provider or
	// backend. The manifest still decides the name: a repository called
	// `infrena-backend-s3` whose manifest says something else is a backend
	// that is not called s3.
	Role Role
	// Manifest is the plugin.yaml read at Tag.
	Manifest *pluginmanifest.Manifest
	// Tag is the git tag the manifest was read at, never a branch.
	Tag string
	// Usable says whether this release can run in this Environment.
	Usable bool
	// Reason says why it cannot, in a form a user can act on. Empty when
	// Usable.
	Reason string
	// Warnings is whatever the manifest parser said about the file itself, such
	// as a format version newer than this build describes. Carried rather than
	// dropped: not a reason to reject the plugin, but the explanation for
	// anything surprising about what is shown.
	Warnings []string
}

// Check reports whether a plugin release can run in this environment, and when
// it cannot, why.
//
// A manifest can fail several checks at once, and only the first failure is
// reported. The order is fixed here, so one manifest always produces the same
// reason:
//
//  1. The constraint the project wrote. A release the project has already
//     excluded is not one whose missing build is worth chasing.
//  2. A build for this machine. Nothing else can be asked about a binary that
//     does not exist.
//  3. A shared protocol version.
//  4. The plugin's own `infrena:` floor. Last because it is the only one whose
//     fix is to upgrade infrena rather than to choose a different release.
//
// Every reason names the evidence on both sides, so the message says what was
// expected rather than only that something was refused. The predicates are
// pluginmanifest's own; reimplementing them here would let search and install
// disagree about the same plugin.
func Check(m *pluginmanifest.Manifest, env Environment) (bool, string) {
	if !env.Constraint.IsZero() && !env.Constraint.Allows(m.Version) {
		return false, fmt.Sprintf("version %s does not satisfy the constraint %s this project asks for",
			m.Version, env.Constraint)
	}

	if !m.Supports(env.Platform) {
		return false, fmt.Sprintf("no build for %s; published builds are %s",
			env.Platform, joinPlatforms(m.Platforms))
	}

	if !m.SpeaksProtocol(env.Protocols) {
		return false, fmt.Sprintf("speaks plugin protocol %s; this build of infrena speaks %s",
			joinInts(m.Protocol), joinInts(env.Protocols))
	}

	if !m.AllowsInfrena(env.InfrenaVersion) {
		return false, fmt.Sprintf("requires infrena %s; this is infrena %s",
			m.Infrena, env.InfrenaVersion)
	}

	return true, ""
}

func joinPlatforms(ps []pluginmanifest.Platform) string {
	if len(ps) == 0 {
		return "none"
	}
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.String()
	}
	return strings.Join(out, ", ")
}

func joinInts(ns []int) string {
	if len(ns) == 0 {
		return "none"
	}
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = strconv.Itoa(n)
	}
	return strings.Join(out, ", ")
}
