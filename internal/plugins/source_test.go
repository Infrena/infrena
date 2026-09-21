package plugins

import (
	"strings"
	"testing"
)

func TestParseSourceTellsAnOwnerFromARepository(t *testing.T) {
	for _, tc := range []struct {
		in    string
		kind  Kind
		owner string
		repo  string
		name  string
	}{
		{"github.com/mycorp", KindOwner, "mycorp", "", ""},
		{"github.com/infrena", KindOwner, "infrena", "", ""},
		{"github.com/someone/infrena-provider-hetzner", KindRepository, "someone", "infrena-provider-hetzner", "hetzner"},
		{"github.com/infrena/infrena-provider-aws", KindRepository, "infrena", "infrena-provider-aws", "aws"},
	} {
		got, err := ParseSource(tc.in)
		if err != nil {
			t.Fatalf("ParseSource(%q): %v", tc.in, err)
		}
		if got.Kind != tc.kind || got.Owner != tc.owner || got.Repo != tc.repo {
			t.Errorf("ParseSource(%q) = %+v, want kind %v owner %q repo %q", tc.in, got, tc.kind, tc.owner, tc.repo)
		}
		if got.String() != tc.in {
			t.Errorf("String() = %q, want %q", got.String(), tc.in)
		}
		name, ok := got.PluginName()
		if tc.name == "" {
			if ok {
				t.Errorf("PluginName() = %q for an owner source, want none", name)
			}
		} else if !ok || name != tc.name {
			t.Errorf("PluginName() = %q, %v; want %q", name, ok, tc.name)
		}
	}
}

// The naming convention is load-bearing: an owner search works by repository
// name, so a repository that does not follow it cannot be found by name and
// must be refused HERE rather than silently never matching.
func TestParseSourceRefusesARepositoryThatBreaksTheConvention(t *testing.T) {
	for _, in := range []string{
		"github.com/someone/hetzner",                // no infrena-provider- prefix
		"github.com/someone/infrena-provider-",      // empty name
		"github.com/someone/infrena-plugin-hetzner", // that is the BINARY name, not the repo name
	} {
		if _, err := ParseSource(in); err == nil {
			t.Errorf("ParseSource(%q) was accepted", in)
		}
	}
}

func TestParseSourceRefusesMalformedInput(t *testing.T) {
	for _, in := range []string{
		"",
		"mycorp",     // no host
		"github.com", // host only
		"github.com/someone/infrena-provider-x/extra", // too deep
		"https://github.com/mycorp",                   // a scheme is not the syntax
		"gitlab.com/mycorp",                           // only GitHub is built; see below
	} {
		if _, err := ParseSource(in); err == nil {
			t.Errorf("ParseSource(%q) was accepted", in)
		}
	}
}

// The syntax is host-prefixed precisely so another forge can be added later
// without changing what a user wrote. So an unknown host is refused with a
// message saying it is not supported yet, not that the input is malformed -
// those send a reader to different places.
func TestAnUnsupportedHostSaysSoRatherThanCallingItMalformed(t *testing.T) {
	_, err := ParseSource("gitlab.com/mycorp")
	if err == nil {
		t.Fatal("gitlab.com was accepted")
	}
	if !strings.Contains(err.Error(), "gitlab.com") || !strings.Contains(err.Error(), "github.com") {
		t.Errorf("error should name the host given and the one supported: %v", err)
	}
}

// A backend repository is a source in its own right: it parses, it carries the
// backend role, and its plugin name is what follows the prefix.
func TestABackendRepositoryParses(t *testing.T) {
	got, err := ParseSource("github.com/infrena/infrena-backend-s3")
	if err != nil {
		t.Fatalf("ParseSource refused a backend repository: %v", err)
	}
	if got.Kind != KindRepository {
		t.Errorf("Kind = %v, want KindRepository", got.Kind)
	}
	if got.Role != RoleBackend {
		t.Errorf("Role = %v, want RoleBackend", got.Role)
	}
	name, ok := got.PluginName()
	if !ok || name != "s3" {
		t.Errorf("PluginName = %q, %v; want s3", name, ok)
	}
	if got.String() != "github.com/infrena/infrena-backend-s3" {
		t.Errorf("String = %q", got.String())
	}
}

func TestAProviderRepositoryStillParsesAndIsAProvider(t *testing.T) {
	got, err := ParseSource("github.com/infrena/infrena-provider-aws")
	if err != nil {
		t.Fatal(err)
	}
	if got.Role != RoleProvider {
		t.Errorf("Role = %v, want RoleProvider", got.Role)
	}
	name, _ := got.PluginName()
	if name != "aws" {
		t.Errorf("PluginName = %q, want aws", name)
	}
}

// The convention is still load-bearing: a repository matching NEITHER prefix
// can never be found by an owner search, so it is refused when written. The
// message must now offer both shapes rather than only the provider one.
func TestARepositoryMatchingNeitherPrefixIsStillRefused(t *testing.T) {
	_, err := ParseSource("github.com/someone/hetzner")
	if err == nil {
		t.Fatal("a repository matching neither prefix was accepted")
	}
	for _, want := range []string{"infrena-provider-", "infrena-backend-"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not offer %q: %v", want, err)
		}
	}
}

// An owner publishes both kinds, so an owner source cannot carry a role and
// must not pretend to.
func TestAnOwnerSourceCarriesNoRole(t *testing.T) {
	got, err := ParseSource("github.com/mycorp")
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != KindOwner {
		t.Fatalf("Kind = %v", got.Kind)
	}
	// PluginName is meaningless for an owner, and says so rather than guessing.
	if _, ok := got.PluginName(); ok {
		t.Error("an owner source named a plugin")
	}
}
