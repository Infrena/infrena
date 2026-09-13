package compiler

import (
	"strings"
	"testing"

	"github.com/infrata/infrata/internal/config"
	"github.com/infrata/infrata/pkg/semver"
)

// PLAN.md §61.2: the optional `infrata:` floor.

const versionedProject = `
project: p
infrata: "%s"
environments:
  dev: {}
resources:
  net:
    type: test.network
    cidr: 10.0.0.0/16
`

// TestAnUnsatisfiedFloorIsReportedOnceAndStopsThere.
//
// The HALT is the point. A binary that cannot understand a project would otherwise
// report every unknown key it met — twenty diagnostics that are all the same problem
// said badly, with the one that explains them buried among them.
func TestAnUnsatisfiedFloorIsReportedOnceAndStopsThere(t *testing.T) {
	ds := checkRequiredVersion(projectWithFloor(t, ">= 9.0"), "0.4.1")
	if !ds.HasErrors() {
		t.Fatal("a floor this build does not meet must be an error")
	}
	if len(ds) != 1 {
		t.Errorf("got %d diagnostics, want exactly one: %v", len(ds), ds)
	}
	out := rendered(ds)
	// Both numbers, because a reader needs to know what they have as well as what
	// is wanted — "requires >= 9.0" alone sends them to check their own version.
	for _, want := range []string{">= 9.0", "0.4.1"} {
		if !strings.Contains(out, want) {
			t.Errorf("the diagnostic does not mention %q:\n%s", want, out)
		}
	}
}

func TestASatisfiedFloorPasses(t *testing.T) {
	for _, constraint := range []string{">= 0.4", ">= 0.4, < 1.0", "0.4.1", "!= 0.5.0"} {
		if ds := checkRequiredVersion(projectWithFloor(t, constraint), "0.4.1"); ds.HasErrors() {
			t.Errorf("%q should allow 0.4.1:\n%s", constraint, rendered(ds))
		}
	}
}

// TestAProjectWithNoFloorIsUnconstrained — every project written before the key
// existed, which is all of them.
func TestAProjectWithNoFloorIsUnconstrained(t *testing.T) {
	if ds := checkRequiredVersion(&config.ProjectDecl{Project: "p"}, "0.4.1"); ds.HasErrors() {
		t.Errorf("a project stating no floor must be unconstrained:\n%s", rendered(ds))
	}
}

// TestADevelopmentBuildIsExemptFromEveryFloor.
//
// A development build reports 0.0.0-dev, which satisfies no floor at all — so without
// this exemption every `go build` from a checkout would refuse every project that
// states one, including this repository's own fixtures. The constraint exists to stop
// a RELEASED binary quietly misreading a project written for a later one; a developer
// running their own build has not made that mistake.
func TestADevelopmentBuildIsExemptFromEveryFloor(t *testing.T) {
	p := projectWithFloor(t, ">= 99.0")
	if ds := checkRequiredVersion(p, developmentVersion); ds.HasErrors() {
		t.Errorf("a development build must not be refused by a project's floor:\n%s", rendered(ds))
	}
}

// projectWithFloor builds a decl stating the given constraint.
func projectWithFloor(t *testing.T, constraint string) *config.ProjectDecl {
	t.Helper()
	c, err := semver.ParseConstraint(constraint)
	if err != nil {
		t.Fatalf("ParseConstraint(%q): %v", constraint, err)
	}
	return &config.ProjectDecl{Project: "p", RequiredVersion: c}
}

// TestAnUnparseableBuildVersionSatisfiesEveryFloor — the same reasoning as the
// development build above, for the case where the linker flag was set to something
// that is not a version at all. A complaint about the binary's own version string is
// not something the user can act on.
func TestAnUnparseableBuildVersionSatisfiesEveryFloor(t *testing.T) {
	if ds := checkRequiredVersion(projectWithFloor(t, ">= 99.0"), "nightly-2026-09-13"); ds.HasErrors() {
		t.Errorf("an unparseable build version must not be refused by a floor:\n%s", rendered(ds))
	}
}

// TestCompileChecksTheFloorAndStopsBeforeAnythingElse is the WIRING, and it was
// missing: every test above calls checkRequiredVersion directly, so deleting its call
// from Compile broke nothing at all.
//
// The fixture's type is deliberately nonsense as well as the floor being unmeetable. A
// build that skipped the version check would report the unknown type instead — so this
// asserts both that the floor is reported AND that nothing after it ran, which is the
// halt's whole purpose: one error, not twenty symptoms of it.
func TestCompileChecksTheFloorAndStopsBeforeAnythingElse(t *testing.T) {
	files := loadFiles(t, `
project: p
infrata: ">= 9.0"
environments:
  dev: {}
resources:
  a:
    type: nonsense.thing
`)
	_, ds := Compile(files, testRegistry(t), Options{Environment: "dev", Version: "0.4.1"})
	if !ds.HasErrors() {
		t.Fatal("Compile must refuse a project whose floor this build does not meet")
	}
	out := rendered(ds)
	if !strings.Contains(out, ">= 9.0") {
		t.Errorf("Compile did not check the floor:\n%s", out)
	}
	if strings.Contains(out, "nonsense") {
		t.Errorf("compilation continued past the version check, so a user gets the floor "+
			"error buried among symptoms of it:\n%s", out)
	}
}

// TestCompileWithNoVersionSuppliedChecksNothing. Options.Version empty is a
// development build, and every floor exempts one — otherwise this repository's own
// fixtures would be refused by any project stating a floor.
func TestCompileWithNoVersionSuppliedChecksNothing(t *testing.T) {
	files := loadFiles(t, `
project: p
infrata: ">= 9.0"
environments:
  dev: {}
resources:
  net:
    type: test.network
    cidr: 10.0.0.0/16
`)
	if _, ds := Compile(files, testRegistry(t), Options{Environment: "dev"}); ds.HasErrors() {
		t.Errorf("a build reporting no version must not be refused by a floor:\n%s", rendered(ds))
	}
}

// TestAZeroVersionSatisfiesEveryFloor.
//
// Found by running `make build` on this repository: with a VCS remote present,
// debug.ReadBuildInfo reports a PSEUDO-VERSION —
// `0.0.0-20260913163216-b7f0de604428+dirty` — which parses as 0.0.0, so every project
// stating a floor refused a developer's own build. internal/version no longer returns
// one, and this is the second line of defence, because a broken CI stamping "0.0.0"
// would arrive by a different route.
//
// 0.0.0 is not a version anybody releases, so whatever produced it, the honest answer
// is that this is not a release.
func TestAZeroVersionSatisfiesEveryFloor(t *testing.T) {
	for _, current := range []string{
		"0.0.0",
		"0.0.0-20260913163216-b7f0de604428+dirty",
	} {
		if ds := checkRequiredVersion(projectWithFloor(t, ">= 0.4"), current); ds.HasErrors() {
			t.Errorf("%q is not a release and must not be refused by a floor:\n%s",
				current, rendered(ds))
		}
	}
}
