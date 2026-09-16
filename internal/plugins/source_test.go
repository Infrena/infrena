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

// Section 31.3 says the syntax is host-prefixed PRECISELY so another forge can
// be added later without changing what a user wrote. So an unknown host is
// refused with a message that says it is not supported YET, not that the input
// is malformed - those send a reader to different places.
func TestAnUnsupportedHostSaysSoRatherThanCallingItMalformed(t *testing.T) {
	_, err := ParseSource("gitlab.com/mycorp")
	if err == nil {
		t.Fatal("gitlab.com was accepted")
	}
	if !strings.Contains(err.Error(), "gitlab.com") || !strings.Contains(err.Error(), "github.com") {
		t.Errorf("error should name the host given and the one supported: %v", err)
	}
}
