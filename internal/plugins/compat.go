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
// A VALUE, passed in rather than read from the process. Check is pure logic
// over a parsed manifest, and a function that read runtime.GOOS itself could
// only be tested on the platform the test happened to run on — which is exactly
// the case that matters least, since the interesting answer is the one for a
// machine the developer is not sitting at.
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
	// named one. The zero value means the project said nothing, which is not
	// the same as ">= 0.0.0" only in that it is never quoted at the user.
	Constraint semver.Constraint
}

// Candidate is one plugin release found by a search, together with the verdict
// on whether it can run here.
//
// A REJECTED CANDIDATE IS STILL A CANDIDATE (§31.3). It is kept, with its
// reason, because "this plugin exists but publishes no build for your machine"
// and "no plugin by that name exists anywhere" are different answers that send
// a reader to completely different places. Dropping the first would render it
// as the second.
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
	// backend. The MANIFEST still decides the name: a repository called
	// `infrena-backend-s3` whose manifest says something else is a backend
	// that is not called s3.
	Role Role
	// Manifest is the plugin.yaml read at Tag.
	Manifest *pluginmanifest.Manifest
	// Tag is the git tag the manifest was read at, never a branch (§31.2).
	Tag string
	// Usable says whether this release can run in this Environment.
	Usable bool
	// Reason says why it cannot, in a form a user can act on. Empty when
	// Usable.
	Reason string
	// Warnings is whatever the manifest parser said about the FILE, such as it
	// being a format version newer than this build describes. Carried rather
	// than dropped: it is not a reason to reject the plugin, but it is the
	// explanation for anything surprising about what is shown.
	Warnings []string
}

// Check reports whether a plugin release can run in this environment, and when
// it cannot, why.
//
// ONE REASON, NOT ALL OF THEM, AND ALWAYS THE SAME ONE. A manifest can fail
// several checks at once, and listing every failure buries the one that
// matters; picking whichever check ran first would make the message depend on
// the order of an unordered map. So the order is fixed here, narrowest question
// first, and the FIRST failure is what the user is told:
//
//  1. The constraint the project wrote. A release the project has already
//     excluded is not one whose missing build is worth chasing — reporting
//     "no darwin/arm64 build" for a version the user ruled out sends them to
//     ask an author for a build of something they do not want.
//  2. A build for this machine. There is no binary to ask anything else about
//     until one exists for the platform asking.
//  3. A shared protocol version. This is a question about a binary that does
//     exist, so it comes after the one about whether it exists.
//  4. The plugin's own `infrena:` floor. Last because it is the only one whose
//     fix is to upgrade infrena rather than to choose a different release, and
//     a reader who has a different release available should hear about that
//     first.
//
// Every reason names the concrete evidence on BOTH sides — this platform and
// the published ones, the protocols each side speaks, the version and the
// constraint — because §44 wants a message that says what was expected, not one
// that says no.
//
// The predicates themselves are pluginmanifest's own (Supports,
// SpeaksProtocol, AllowsInfrena). This function adds the one thing they do not
// have: a sentence. Reimplementing them here would put a second answer to
// "is this compatible?" in the tree, and the day the two disagree, a search and
// an install disagree about the same plugin.
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

// joinPlatforms lists published builds for a message, so the reader sees what
// the plugin DOES have next to what they asked for.
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

// joinInts lists protocol versions for a message.
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
