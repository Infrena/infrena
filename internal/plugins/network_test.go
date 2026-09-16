package plugins

import (
	"os/exec"
	"strings"
	"testing"
)

// The network is never on the hot path (PLAN.md section 31.3), and this is
// the check that keeps it true rather than merely intended. Unit 2 puts an
// HTTP client ABOVE this package; the day something puts one inside it,
// this fails and names the import.
//
// A DEPENDENCY CHECK, not a behaviour one. internal/cli's proxy test catches a
// call made through something already imported; this one catches the import
// itself, which is the thing that makes such a call possible at all. Neither
// subsumes the other.
func TestPluginsPackageCannotReachTheNetwork(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		// The toolchain is what answers this question; without it there is
		// nothing to ask. Skipped rather than guessed, because a guess here
		// would report an architectural rule as held when nothing checked it.
		t.Skip("no go toolchain on PATH, so the dependency graph cannot be read")
	}

	// `.` rather than the import path: go test runs with this package's
	// directory as the working directory, so the two name the same package and
	// this needs no idea of where the module root is.
	out, err := exec.Command("go", "list", "-deps", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, out)
	}

	for _, dep := range strings.Fields(string(out)) {
		if dep == "net/http" || dep == "net" {
			t.Errorf("internal/plugins depends on %s, which it must never do: "+
				"the network belongs in a package ABOVE this one, or a caller on "+
				"the hot path can reach it from here", dep)
		}
	}
}
