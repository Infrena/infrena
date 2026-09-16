package plugins

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTrusted puts body at TrustedPath(home), creating the directory it lives in.
func writeTrusted(t *testing.T, home, body string) {
	t.Helper()
	path := TrustedPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// A user with no config file is the ordinary case and must not be an error.
// They still get the official owner, or `plugin: aws` would report that
// nothing matches, on a fresh machine, with no way to tell why.
func TestNoConfigFileStillTrustsTheOfficialOwner(t *testing.T) {
	got, err := LoadTrusted(t.TempDir())
	if err != nil {
		t.Fatalf("LoadTrusted with no file: %v", err)
	}
	if len(got) != 1 || got[0].String() != "github.com/infrena" {
		t.Errorf("LoadTrusted = %v, want just the official owner", got)
	}
}

// A MISSING CONFIG DIRECTORY MUST DEGRADE EXACTLY LIKE A MISSING CONFIG FILE.
// A minimal container or a CI runner can have no HOME at all, and os.UserConfigDir
// then has nothing to return; a user in that position still gets the official
// owner, because being unable to name a config directory says nothing about
// where infrena's own plugins live.
//
// The empty config home must also not be joined into a RELATIVE path: reading
// `infrena/plugins.yml` out of whatever directory the command happens to be run
// from would make a file in a checkout silently decide what infrena trusts.
func TestNoConfigDirectoryStillTrustsTheOfficialOwner(t *testing.T) {
	cwd := t.TempDir()
	writeTrusted(t, cwd, "sources:\n  - github.com/from-the-working-directory\n")
	t.Chdir(cwd)

	got, err := LoadTrusted("")
	if err != nil {
		t.Fatalf("LoadTrusted with no config directory: %v", err)
	}
	if len(got) != 1 || got[0].String() != "github.com/infrena" {
		t.Errorf("LoadTrusted = %v, want just the official owner", got)
	}
}

func TestUserSourcesAreAddedAfterTheOfficialOwner(t *testing.T) {
	home := t.TempDir()
	writeTrusted(t, home, "sources:\n  - github.com/mycorp\n  - github.com/someone/infrena-provider-hetzner\n")

	got, err := LoadTrusted(home)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"github.com/infrena", "github.com/mycorp", "github.com/someone/infrena-provider-hetzner"}
	if len(got) != len(want) {
		t.Fatalf("LoadTrusted = %v, want %v", got, want)
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i].String(), want[i])
		}
	}
}

// Section 31.3: the official owner is always searched and CANNOT be removed.
// It is not a configurable default, because a configurable default is a thing
// that gets misconfigured into an empty list, after which `plugin: aws` reports
// that nothing matches and the cause is invisible.
func TestTheOfficialOwnerCannotBeRemoved(t *testing.T) {
	home := t.TempDir()
	// A user who lists other sources and not the official one, and a user who
	// writes an explicitly empty list, must both still get it.
	for _, body := range []string{"sources:\n  - github.com/mycorp\n", "sources: []\n", "sources:\n"} {
		writeTrusted(t, home, body)
		got, err := LoadTrusted(home)
		if err != nil {
			t.Fatalf("body %q: %v", body, err)
		}
		if len(got) == 0 || got[0].String() != "github.com/infrena" {
			t.Errorf("body %q: LoadTrusted = %v, official owner missing", body, got)
		}
	}
}

// Listing it explicitly must not list it twice.
func TestListingTheOfficialOwnerDoesNotDuplicateIt(t *testing.T) {
	home := t.TempDir()
	writeTrusted(t, home, "sources:\n  - github.com/infrena\n  - github.com/mycorp\n")

	got, err := LoadTrusted(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("LoadTrusted = %v, want 2 entries", got)
	}
}

// A malformed trust file is an ERROR, never a silent fallback to the official
// owner alone. This file is a security decision the user wrote down; quietly
// ignoring it would mean a plugin install that should have been possible
// reports that nothing was found, or worse, that a source they revoked is
// silently still absent from a list they think they edited.
func TestAMalformedTrustFileIsAnErrorNamingTheFile(t *testing.T) {
	home := t.TempDir()
	writeTrusted(t, home, "sources:\n  - github.com/someone/not-a-plugin-repo\n")

	_, err := LoadTrusted(home)
	if err == nil {
		t.Fatal("a malformed source was accepted")
	}
	if !strings.Contains(err.Error(), TrustedPath(home)) {
		t.Errorf("error does not name the file: %v", err)
	}
}

// An unknown key is a typo in a security file, so it is refused rather than
// ignored, the way every other file this project reads behaves.
func TestAnUnknownKeyInTheTrustFileIsAnError(t *testing.T) {
	home := t.TempDir()
	writeTrusted(t, home, "source:\n  - github.com/mycorp\n")

	if _, err := LoadTrusted(home); err == nil {
		t.Fatal("an unknown key was accepted")
	}
}

func TestOfficialOwnerIsTheInfrenaOrganisation(t *testing.T) {
	got := OfficialOwner()
	if got.String() != "github.com/infrena" || got.Kind != KindOwner {
		t.Errorf("OfficialOwner() = %+v", got)
	}
}
