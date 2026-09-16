package plugins

import (
	"strings"
	"testing"

	"github.com/infrena/infrena/pkg/pluginmanifest"
	"github.com/infrena/infrena/pkg/semver"
)

// manifest builds a real pluginmanifest.Manifest, validated the same way a
// parsed one is, so a test can never assert against a shape the parser would
// have refused.
func manifest(t *testing.T, name, version string, protocols []int, platforms []string) *pluginmanifest.Manifest {
	t.Helper()

	v, err := semver.Parse(version)
	if err != nil {
		t.Fatalf("version %q: %v", version, err)
	}

	m := &pluginmanifest.Manifest{
		Format:      pluginmanifest.Version,
		Name:        name,
		Version:     v,
		Protocol:    protocols,
		Description: "a plugin, for a test",
	}
	for _, p := range platforms {
		parsed, err := pluginmanifest.ParsePlatform(p)
		if err != nil {
			t.Fatalf("platform %q: %v", p, err)
		}
		m.Platforms = append(m.Platforms, parsed)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("test manifest is not valid: %v", err)
	}
	return m
}

func constraint(t *testing.T, s string) semver.Constraint {
	t.Helper()
	c, err := semver.ParseConstraint(s)
	if err != nil {
		t.Fatalf("constraint %q: %v", s, err)
	}
	return c
}

// Section 31.3: "Every filtered-out candidate is still worth mentioning, with
// the reason." A user whose plugin exists but has no darwin/arm64 build must be
// TOLD that, not told nothing was found — those send a reader to completely
// different places.
func TestEveryRejectionCarriesAReasonThatNamesTheEvidence(t *testing.T) {
	here := pluginmanifest.Platform{OS: "darwin", Arch: "arm64"}
	for _, tc := range []struct {
		name string
		m    *pluginmanifest.Manifest
		env  Environment
		want string
	}{
		{
			name: "no build for this platform",
			m:    manifest(t, "hetzner", "2.0.0", []int{4}, []string{"linux/amd64"}),
			env:  Environment{Platform: here, Protocols: []int{4, 3}},
			want: "darwin/arm64",
		},
		{
			name: "protocol does not intersect",
			m:    manifest(t, "hetzner", "2.0.0", []int{9}, []string{"darwin/arm64"}),
			env:  Environment{Platform: here, Protocols: []int{4, 3}},
			want: "protocol",
		},
		{
			name: "version constraint not satisfied",
			m:    manifest(t, "hetzner", "1.0.0", []int{4}, []string{"darwin/arm64"}),
			env:  Environment{Platform: here, Protocols: []int{4}, Constraint: constraint(t, ">= 2.0.0")},
			want: "2.0.0",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			usable, reason := Check(tc.m, tc.env)
			if usable {
				t.Fatal("candidate was accepted")
			}
			if !strings.Contains(reason, tc.want) {
				t.Errorf("reason %q does not name %q", reason, tc.want)
			}
		})
	}
}

func TestAUsableCandidateHasNoReason(t *testing.T) {
	m := manifest(t, "hetzner", "2.0.0", []int{4}, []string{"darwin/arm64"})
	usable, reason := Check(m, Environment{
		Platform:  pluginmanifest.Platform{OS: "darwin", Arch: "arm64"},
		Protocols: []int{4, 3},
	})
	if !usable {
		t.Fatalf("a compatible plugin was rejected: %s", reason)
	}
	if reason != "" {
		t.Errorf("reason = %q, want empty", reason)
	}
}

// A manifest failing two checks reports ONE reason, and always the same one, so
// two runs of the same search never disagree about why a plugin was rejected.
// The order is fixed by Check and asserted here rather than left to whichever
// predicate happens to be evaluated first.
func TestTheFirstFailureInTheFixedOrderIsTheOneReported(t *testing.T) {
	here := pluginmanifest.Platform{OS: "darwin", Arch: "arm64"}

	// Out of the range the project asked for AND built for nothing this machine
	// runs: the constraint is reported, because a release the project has
	// already excluded is not one whose missing build is worth chasing.
	m := manifest(t, "hetzner", "1.0.0", []int{4}, []string{"linux/amd64"})
	_, reason := Check(m, Environment{
		Platform:   here,
		Protocols:  []int{4},
		Constraint: constraint(t, ">= 2.0.0"),
	})
	if !strings.Contains(reason, ">= 2.0.0") {
		t.Errorf("reason = %q, want the constraint, which is checked first", reason)
	}

	// In range, but neither built for this machine nor speaking a protocol this
	// build knows: the missing build is reported, because there is no binary to
	// hold a protocol conversation with.
	m = manifest(t, "hetzner", "2.0.0", []int{9}, []string{"linux/amd64"})
	_, reason = Check(m, Environment{Platform: here, Protocols: []int{4}})
	if !strings.Contains(reason, "darwin/arm64") {
		t.Errorf("reason = %q, want the platform, which is checked before the protocol", reason)
	}
}

// A plugin that names a floor this build is under is rejected with the floor
// and this build's version both in the message, because the action is to
// upgrade and the reader needs to know to what.
func TestAnInfrenaFloorAboveThisBuildIsRejectedWithBothVersions(t *testing.T) {
	m := manifest(t, "hetzner", "2.0.0", []int{4}, []string{"darwin/arm64"})
	m.Infrena = constraint(t, ">= 0.9.0")

	usable, reason := Check(m, Environment{
		Platform:       pluginmanifest.Platform{OS: "darwin", Arch: "arm64"},
		Protocols:      []int{4},
		InfrenaVersion: "0.4.1",
	})
	if usable {
		t.Fatal("a plugin requiring a newer infrena was accepted")
	}
	if !strings.Contains(reason, "0.9.0") || !strings.Contains(reason, "0.4.1") {
		t.Errorf("reason = %q, want both the floor and this build's version", reason)
	}
}
