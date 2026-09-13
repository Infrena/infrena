package pluginmanifest_test

import (
	"strings"
	"testing"

	"github.com/infrata/infrata/pkg/pluginmanifest"
	"github.com/infrata/infrata/pkg/pluginproto"
)

// PLAN.md §31.2. An EXTERNAL test package, importing only what a plugin author can —
// this package exists so a plugin validates its own manifest with the same code
// `plugins install` will, so it must be usable exactly that way.

const valid = `
manifest: 1
name: fake
version: 0.1.0
protocol: [1]
platforms: [linux/amd64, linux/arm64, darwin/arm64, windows/amd64]
description: A fake provider for testing infrata without a cloud account.
infrata: ">= 0.2.0"
source: https://github.com/infrata/infrata-provider-fake
`

func parse(t *testing.T, body string) (*pluginmanifest.Manifest, []string) {
	t.Helper()
	m, warnings, err := pluginmanifest.Parse([]byte(body))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return m, warnings
}

func TestAValidManifestParses(t *testing.T) {
	m, warnings := parse(t, valid)
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
	if m.Name != "fake" || m.Version.String() != "0.1.0" {
		t.Errorf("name/version = %q/%s", m.Name, m.Version)
	}
	if !m.SpeaksProtocol(pluginproto.Supported) {
		t.Error("a manifest saying protocol [1] must speak this build's protocol")
	}
	if !m.Supports(pluginmanifest.Platform{OS: "darwin", Arch: "arm64"}) {
		t.Error("darwin/arm64 is listed and was not recognised")
	}
	if m.Supports(pluginmanifest.Platform{OS: "plan9", Arch: "386"}) {
		t.Error("a platform not listed was reported as supported")
	}
	if m.Description == "" || m.Source == "" {
		t.Errorf("description/source lost: %q / %q", m.Description, m.Source)
	}
}

// TestTheFormatVersionIsReadBeforeAnythingElse is §31.2's rule and the reason the format
// is versioned at all: a manifest from the future must say so, rather than failing with
// an error about a key nobody recognises.
func TestTheFormatVersionIsReadBeforeAnythingElse(t *testing.T) {
	m, warnings := parse(t, `
manifest: 99
name: futuristic
version: 2.0.0
protocol: [1]
platforms: [linux/amd64]
description: From a later infrata.
something_invented_later: {a: b}
`)
	if len(warnings) == 0 {
		t.Fatal("a manifest of an unknown format version must warn")
	}
	if !strings.Contains(warnings[0], "99") {
		t.Errorf("the warning does not name the version it found: %v", warnings)
	}
	// And it still parsed what this build DOES understand, which is what lets a search
	// say something useful about a plugin newer than itself.
	if m.Name != "futuristic" {
		t.Errorf("nothing was salvaged from a newer manifest: %+v", m)
	}
}

// TestAnUnknownKeyIsRefusedAtAKnownFormatVersion — the other side of the same rule. A
// typo is a mistake the author wants to hear about, and at a version this build claims to
// understand there is no other explanation for a key it does not know.
func TestAnUnknownKeyIsRefusedAtAKnownFormatVersion(t *testing.T) {
	_, _, err := pluginmanifest.Parse([]byte(valid + "platfroms: [linux/amd64]\n"))
	if err == nil {
		t.Fatal("an unknown key at a known format version must be refused")
	}
	if !strings.Contains(err.Error(), "platfroms") {
		t.Errorf("the error does not name the key: %v", err)
	}
}

// TestAMissingFormatVersionIsRefused, and says what to write.
func TestAMissingFormatVersionIsRefused(t *testing.T) {
	_, _, err := pluginmanifest.Parse([]byte("name: fake\nversion: 0.1.0\n"))
	if err == nil {
		t.Fatal("a manifest with no `manifest:` key must be refused")
	}
	for _, want := range []string{"manifest", "1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}

func TestEveryRequiredKeyIsRequired(t *testing.T) {
	for _, tc := range []struct{ drop, want string }{
		{"name: fake", "`name`"},
		{"version: 0.1.0", "`version`"},
		{"protocol: [1]", "`protocol`"},
		{"platforms:", "`platforms`"},
		{"description:", "`description`"},
	} {
		body := dropLineStarting(valid, tc.drop)
		if body == valid {
			t.Fatalf("the fixture has no line starting %q, so this case tests nothing", tc.drop)
		}
		_, _, err := pluginmanifest.Parse([]byte(body))
		if err == nil {
			t.Errorf("a manifest without %s must be refused", tc.want)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("dropping %q gave %v, which does not name %s", tc.drop, err, tc.want)
		}
	}
}

// TestInfrataAndSourceAreOptional. §31.2 makes `infrata` optional deliberately: absence
// means unconstrained, where `">= 0.0.0"` would be a value shaped like a constraint that
// constrains nothing.
func TestInfrataAndSourceAreOptional(t *testing.T) {
	m, _ := parse(t, `
manifest: 1
name: fake
version: 0.1.0
protocol: [1]
platforms: [linux/amd64]
description: No constraint stated.
`)
	if !m.Infrata.IsZero() {
		t.Errorf("an absent `infrata` must be the zero constraint, got %q", m.Infrata)
	}
	if !m.AllowsInfrata("9.9.9") {
		t.Error("an unconstrained manifest must allow any infrata")
	}
}

// TestAStatedConstraintIsEnforcedExceptForADevelopmentBuild.
//
// The exemption is §61.2's, for the same reason: a complaint about a developer's own
// build is not something they can act on. Anything parsing as 0.0.0 counts, because a
// `go build` in a checkout with a VCS remote reports a pseudo-version that does.
func TestAStatedConstraintIsEnforcedExceptForADevelopmentBuild(t *testing.T) {
	m, _ := parse(t, valid) // infrata: ">= 0.2.0"
	for _, tc := range []struct {
		version string
		want    bool
	}{
		{"0.3.0", true},
		{"0.2.0", true},
		{"0.1.9", false},
		{"0.0.0-dev", true},
		{"0.0.0-20260913163216-b7f0de604428+dirty", true},
		{"not-a-version", true},
	} {
		if got := m.AllowsInfrata(tc.version); got != tc.want {
			t.Errorf("AllowsInfrata(%q) = %v, want %v", tc.version, got, tc.want)
		}
	}
}

// TestAProtocolMismatchIsDetectable — rule 1 of §31.2's three, and the one a search
// answers without downloading anything.
func TestAProtocolMismatchIsDetectable(t *testing.T) {
	m, _ := parse(t, strings.Replace(valid, "protocol: [1]", "protocol: [7, 8]", 1))
	if m.SpeaksProtocol([]int{1}) {
		t.Error("a plugin speaking only 7 and 8 does not speak protocol 1")
	}
	// And the set is a SET on both sides: a host supporting 1 and 8 matches.
	if !m.SpeaksProtocol([]int{1, 8}) {
		t.Error("a host supporting 8 must match a plugin speaking 8")
	}
}

// TestANameThatWouldBreakAFilenameOrATypePrefixIsRefused. The name becomes both, so a dot
// or a slash in it is refused here rather than at load.
func TestANameThatWouldBreakAFilenameOrATypePrefixIsRefused(t *testing.T) {
	for _, bad := range []string{"fake.provider", "fake/provider", "fake provider", "a:b"} {
		body := strings.Replace(valid, "name: fake", "name: "+bad, 1)
		if _, _, err := pluginmanifest.Parse([]byte(body)); err == nil {
			t.Errorf("name %q must be refused", bad)
		}
	}
}

func TestPlatformsMustBeGOOSSlashGOARCH(t *testing.T) {
	for _, bad := range []string{"linux", "linux/", "/amd64", "linux/amd64/v3", ""} {
		body := strings.Replace(valid, "platforms: [linux/amd64, linux/arm64, darwin/arm64, windows/amd64]",
			"platforms: [\""+bad+"\"]", 1)
		if _, _, err := pluginmanifest.Parse([]byte(body)); err == nil {
			t.Errorf("platform %q must be refused", bad)
		}
	}
}

// dropLineStarting removes the first line beginning with prefix.
func dropLineStarting(body, prefix string) string {
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), prefix) {
			return strings.Join(append(lines[:i:i], lines[i+1:]...), "\n")
		}
	}
	return body
}
