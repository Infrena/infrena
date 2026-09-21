package plugins

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Trusting an owner trusts the repositories under it: the approval is a
// judgement about a publisher, which is the unit a person can actually reason
// about. One mechanism, asked once.
func TestTrustingAnOwnerTrustsItsRepositories(t *testing.T) {
	trusted := []Source{mustSource(t, "github.com/mycorp")}

	if !IsTrusted(trusted, mustSource(t, "github.com/mycorp/infrena-provider-hetzner")) {
		t.Error("a repository under a trusted owner was not trusted")
	}
	if IsTrusted(trusted, mustSource(t, "github.com/someone/infrena-provider-hetzner")) {
		t.Error("a repository under an untrusted owner was trusted")
	}
}

// Trusting ONE repository does not trust its owner. The narrower approval must
// not widen by itself.
func TestTrustingOneRepositoryDoesNotTrustTheOwner(t *testing.T) {
	trusted := []Source{mustSource(t, "github.com/someone/infrena-provider-hetzner")}

	if IsTrusted(trusted, mustSource(t, "github.com/someone/infrena-provider-other")) {
		t.Error("trusting one repository trusted a sibling")
	}
	if IsTrusted(trusted, mustSource(t, "github.com/someone")) {
		t.Error("trusting one repository trusted the whole owner")
	}
	if !IsTrusted(trusted, mustSource(t, "github.com/someone/infrena-provider-hetzner")) {
		t.Error("the exact repository that was trusted was not trusted")
	}
}

// Approving records it in the USER's own file, so it is asked once and is
// visible afterwards.
func TestApproveRecordsTheOwnerAndSurvivesAReload(t *testing.T) {
	home := t.TempDir()

	if err := Approve(home, mustSource(t, "github.com/mycorp")); err != nil {
		t.Fatal(err)
	}

	got, err := LoadTrusted(home)
	if err != nil {
		t.Fatal(err)
	}
	if !IsTrusted(got, mustSource(t, "github.com/mycorp/infrena-provider-hetzner")) {
		t.Error("an approved owner was not trusted after reloading")
	}
}

// Approving twice must not write it twice.
func TestApproveIsIdempotent(t *testing.T) {
	home := t.TempDir()
	s := mustSource(t, "github.com/mycorp")
	for i := 0; i < 3; i++ {
		if err := Approve(home, s); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := LoadTrusted(home)
	n := 0
	for _, x := range got {
		if x.String() == "github.com/mycorp" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("approved owner appears %d times, want 1", n)
	}
}

// The file is the user's. A comment they wrote next to a source is the only
// record of why they trusted it, and losing it to a machine-written rewrite
// would teach them not to annotate the one file worth annotating.
func TestApproveKeepsTheCommentsAUserWrote(t *testing.T) {
	home := t.TempDir()
	path := TrustedPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	original := "# the places I fetch plugins from\nsources:\n  # approved by security, ticket 412\n  - github.com/acme\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Approve(home, mustSource(t, "github.com/mycorp")); err != nil {
		t.Fatal(err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"# the places I fetch plugins from",
		"# approved by security, ticket 412",
		"github.com/acme",
		"github.com/mycorp",
	} {
		if !strings.Contains(string(after), want) {
			t.Errorf("Approve lost %q:\n%s", want, after)
		}
	}
}

// Approving one thing must not quietly drop another. The file is a list of
// decisions, and a rewrite that keeps only the newest is a decision the user
// never made.
func TestApproveKeepsTheSourcesAlreadyThere(t *testing.T) {
	home := t.TempDir()
	if err := Approve(home, mustSource(t, "github.com/acme")); err != nil {
		t.Fatal(err)
	}
	if err := Approve(home, mustSource(t, "github.com/mycorp/infrena-provider-hetzner")); err != nil {
		t.Fatal(err)
	}

	got, err := LoadTrusted(home)
	if err != nil {
		t.Fatal(err)
	}
	if !IsTrusted(got, mustSource(t, "github.com/acme/infrena-provider-anything")) {
		t.Error("the first approval was lost by the second")
	}
	if !IsTrusted(got, mustSource(t, "github.com/mycorp/infrena-provider-hetzner")) {
		t.Error("the second approval was not recorded")
	}
}

// A file that cannot be understood is NOT overwritten. Rewriting it would
// destroy whatever the user meant to say and silently change what infrena
// trusts, which is the one file where a silent change is least acceptable.
func TestApproveRefusesToOverwriteAFileItCannotRead(t *testing.T) {
	home := t.TempDir()
	path := TrustedPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	broken := "sourecs:\n  - github.com/acme\n"
	if err := os.WriteFile(path, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}

	err := Approve(home, mustSource(t, "github.com/mycorp"))
	if err == nil {
		t.Fatal("a file that does not parse was rewritten")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error does not name the file: %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != broken {
		t.Errorf("the unreadable file was modified:\n%s", after)
	}
}

// The official owner is trusted by LoadTrusted whatever the file says, so
// writing it down adds nothing and only makes the user's own list longer.
func TestApprovingTheOfficialOwnerWritesNothing(t *testing.T) {
	home := t.TempDir()

	if err := Approve(home, OfficialOwner()); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(TrustedPath(home)); !os.IsNotExist(err) {
		data, _ := os.ReadFile(TrustedPath(home))
		t.Errorf("approving the always-trusted owner created a file:\n%s", data)
	}
}

// Approving with nowhere to record it must fail rather than appear to work. A
// container with no HOME cannot remember an approval, and pretending otherwise
// would ask the same question on every run while reporting success on each.
func TestApproveWithNoConfigDirectoryIsAnError(t *testing.T) {
	if err := Approve("", mustSource(t, "github.com/mycorp")); err == nil {
		t.Fatal("an approval with nowhere to be written reported success")
	}
}
