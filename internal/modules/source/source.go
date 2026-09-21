// Package source turns a module source string into a directory on disk.
//
// A source is either a filesystem path — absolute, or relative to the file that
// declared it — or a git repository pinned to a tag or a commit. The pin is
// required: an unpinned remote means the same configuration plans differently on
// different days, silently and at a distance.
//
// Everything Parse classifies as KindGit is eventually handed to the git binary
// as an argument, so Parse is a trust boundary and not a convenience. It accepts
// three spellings and refuses everything else by name.
//
// A path may contain a colon: "../mod:v1" has no scheme and no user@host:
// prefix, so it is a filesystem path whose name happens to contain a colon,
// not a pin. This language has no way to spell "a local repository at a tag" —
// the module is the directory.
package source

import (
	"strconv"
	"strings"

	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/pkg/value"
)

// Kind is how a module source names its content.
type Kind int

const (
	// KindPath is a directory on this filesystem.
	KindPath Kind = iota
	// KindGit is a git repository, pinned to a tag or a commit.
	KindGit
)

// String names a kind for diagnostics. It is a switch with an explicit default
// rather than a two-armed if, for the reason diag.Severity.String gives: an
// unrecognised value must read as corruption, not as one of the real kinds.
func (k Kind) String() string {
	switch k {
	case KindPath:
		return "path"
	case KindGit:
		return "git"
	default:
		return "Kind(" + strconv.Itoa(int(k)) + ")"
	}
}

// Source is a parsed module source.
type Source struct {
	// Kind is whether this is a filesystem path or a git repository.
	Kind Kind
	// Location is the repository URL with the ":ref" suffix removed for
	// KindGit, or the path exactly as written for KindPath.
	Location string
	// Ref is the tag or commit a git source is pinned to. It is empty for a
	// path: a path names a directory, and this language has no spelling for
	// a local repository at a tag, so "../mod:v1" is a path whose name
	// contains a colon.
	Ref string
	// Origin is where the source was declared.
	Origin value.Origin
}

// PinnedToHash reports whether Ref names a commit rather than a tag. The cache
// may skip the network for a commit, which names an immutable object, and may
// never skip it for a tag, which does not.
//
// Seven characters is git's own minimum abbreviation. A tag literally named
// like a short hash would be misclassified, costing the absence of a network
// round trip; fetchCommit resolves such a ref through the route that tries refs
// before object prefixes.
func (s Source) PinnedToHash() bool {
	if s.Kind != KindGit || len(s.Ref) < 7 || len(s.Ref) > 40 {
		return false
	}
	for _, r := range s.Ref {
		if !('0' <= r && r <= '9') && !('a' <= r && r <= 'f') {
			return false
		}
	}
	return true
}

// String renders the source in the spelling it was written in, and is an exact
// round trip for anything Parse accepted: Parse(raw).String() == raw, byte for
// byte. It holds because Parse only splits — it refuses surrounding whitespace
// rather than trimming it, and never canonicalises a host, a trailing slash or a
// ".git" suffix. `infrena export` regenerates a `modules:` entry from this, so
// normalising here would rewrite the user's own file under them.
func (s Source) String() string {
	if s.Ref == "" {
		return s.Location
	}
	return s.Location + ":" + s.Ref
}

// Parse classifies raw and validates it. origin is where the source was
// declared, and every diagnostic carries it.
func Parse(raw string, origin value.Origin) (Source, diag.Diagnostics) {
	p := parser{origin: origin}

	switch {
	case raw == "":
		return p.refuse("module source is empty",
			"A `modules:` entry must name a directory or a git repository.",
			"Write a path such as ./modules/networking, or a pinned remote such as https://github.com/acme/infra-networking:v1.0.0.")

	case strings.TrimSpace(raw) != raw:
		return p.refuse("module source "+strconv.Quote(raw)+" has surrounding whitespace",
			"The source is used verbatim as a path or a URL, so leading or trailing whitespace is never part of what was meant.",
			"Remove the whitespace around the value.")

	// Refused before anything else is decided, because a classification
	// step that runs first could send it down the path branch, where a
	// leading "-" is merely an odd directory name and no refusal would fire.
	// git parses an argument beginning with "-" as an option unless it is
	// separated by "--".
	case strings.HasPrefix(raw, "-"):
		return p.refuse("module source "+strconv.Quote(raw)+" begins with \"-\"",
			"A source beginning with \"-\" is parsed by git as a command-line option rather than as a repository.",
			"If this is a directory, write it as ./"+strings.TrimPrefix(raw, "-")+" or as an absolute path.")
	}

	if helper, rest, ok := splitTransportHelper(raw); ok {
		return p.refuseHelper(helper, rest)
	}

	if scheme, rest, ok := splitScheme(raw); ok {
		switch scheme {
		case "https", "ssh":
			return p.parseURL(raw, scheme, rest)
		case "file":
			return p.refuse("module source "+strconv.Quote(raw)+" uses the file:// scheme",
				"A module on this filesystem is spelled as a path, not as a URL. A second spelling for the same thing only widens what the scheme allowlist has to reason about.",
				"Write it as a path: /"+strings.TrimPrefix(rest, "/")+".")
		default:
			return p.refuse("module source "+strconv.Quote(raw)+" uses the unsupported scheme "+strconv.Quote(scheme),
				"A git module source is https://, ssh://, or the git@host:path shorthand. Nothing else is fetched.",
				"Use https:// or ssh:// for a remote module, or a filesystem path for a local one.")
		}
	}

	if authority, path, ok := splitSCP(raw); ok {
		return p.parseSCP(raw, authority, path)
	}

	return Source{Kind: KindPath, Location: raw, Origin: origin}, nil
}

type parser struct {
	origin value.Origin
	ds     diag.Diagnostics
}

func (p *parser) refuse(summary, detail, action string) (Source, diag.Diagnostics) {
	p.ds.Add(diag.Diagnostic{
		Severity: diag.SeverityError,
		Summary:  summary,
		Detail:   detail,
		Action:   action,
		Origin:   p.origin,
	})
	return Source{}, p.ds
}

// refuseHelper refuses git's `<transport>::<address>` form. ext:: is named
// explicitly because it is not merely unsupported: git's ext transport runs its
// address as a shell command, so `ext::sh -c ...` in a module source is remote
// code execution at `infrena plan` time when a user's gitconfig sets
// protocol.ext.allow.
func (p *parser) refuseHelper(helper, rest string) (Source, diag.Diagnostics) {
	if helper == "ext" {
		return p.refuse("module source uses git's ext:: transport",
			"ext:: runs a shell command and speaks git's protocol over its standard input and output, so an ext:: module source executes "+strconv.Quote(rest)+" on this machine during planning. It is refused whatever it contains.",
			"If you meant a repository, write it as https://host/path:tag or ssh://host/path:tag.")
	}
	return p.refuse("module source uses the "+helper+":: transport helper",
		"A git module source is written as a plain URL. "+helper+":: selects a transport helper program, which is a wider surface than this language admits.",
		"Write the URL without the "+helper+":: prefix: "+rest+".")
}

// parseURL handles https:// and ssh://. rest is everything after "://":
// authority, then "/", then the repository path, which carries the ":ref".
func (p *parser) parseURL(raw, scheme, rest string) (Source, diag.Diagnostics) {
	slash := strings.Index(rest, "/")
	if slash < 0 || slash == len(rest)-1 {
		return p.refuse("module source "+strconv.Quote(raw)+" has no repository path",
			"A git source names a host and then a repository: "+scheme+"://host/owner/repo:tag.",
			"Add the repository path and the tag or commit it is pinned to.")
	}
	authority, path := rest[:slash], rest[slash+1:]
	if d, bad := p.checkUserinfo(authority); bad {
		return d, p.ds
	}
	// The ref colon is looked for in the path only. Scanning the whole
	// string would find the scheme's colon here, and the host separator in
	// the scp form below; a port lives in the authority and is excluded for
	// free.
	return p.finish(raw, path)
}

// parseSCP handles git's user@host:path shorthand.
func (p *parser) parseSCP(raw, authority, path string) (Source, diag.Diagnostics) {
	if d, bad := p.checkUserinfo(authority); bad {
		return d, p.ds
	}
	return p.finish(raw, path)
}

// checkUserinfo refuses a URL carrying credentials. A password or token in a
// module source would be written into modules.lock, printed by every diagnostic
// that names the source, and rendered in plan output — none of which goes
// through pkg/value.Format, the single redaction path. Refusing the spelling is
// how this package avoids needing a second one.
func (p *parser) checkUserinfo(authority string) (Source, bool) {
	at := strings.LastIndex(authority, "@")
	if at < 0 || !strings.Contains(authority[:at], ":") {
		return Source{}, false // no userinfo, or plain "git@host", which is the normal case
	}
	// The source is deliberately not echoed: it contains the secret, and
	// pkg/value.Format is the product's only redaction path.
	s, _ := p.refuse("module source carries credentials in its URL",
		"A password or token written into a module source is recorded in modules.lock, printed by every "+
			"diagnostic that names the source, and rendered in plan output, none of which redacts. The value "+
			"is not repeated here for that reason.",
		"Remove the credentials from the URL and let git supply them: a credential helper for https://, or a key for ssh://.")
	return s, true
}

// finish splits the required ":ref" off the repository path and validates it.
func (p *parser) finish(raw, path string) (Source, diag.Diagnostics) {
	colon := strings.LastIndex(path, ":")
	if colon < 0 {
		return p.refuse("module source "+strconv.Quote(raw)+" is not pinned to a tag or commit",
			"A remote module must name exactly what it is: an unpinned source plans differently on different days, because the branch it follows moves. The repository path here is "+strconv.Quote(path)+" with no \":tag-or-hash\" suffix.",
			"Add the version you want: "+raw+":v1.2.0, or "+raw+":9f3c1ab for a commit.")
	}
	ref := path[colon+1:]
	if ref == "" {
		return p.refuse("module source "+strconv.Quote(raw)+" has an empty ref",
			"The \":\" that pins a module source must be followed by a tag or a commit hash.",
			"Write "+strings.TrimSuffix(raw, ":")+":v1.2.0.")
	}
	if why, ok := validRef(ref); !ok {
		return p.refuse("module source ref "+strconv.Quote(ref)+" is not a valid tag or commit",
			"A ref is a git tag name or a commit hash: "+why+". Refs also reach git as arguments, so this is checked rather than passed through.",
			"Pin the source to a tag such as v1.2.0 or to a commit such as 9f3c1ab.")
	}
	return Source{
		Kind:     KindGit,
		Location: raw[:len(raw)-len(ref)-1],
		Ref:      ref,
		Origin:   p.origin,
	}, nil
}

// splitTransportHelper detects git's "<transport>::<address>" form.
func splitTransportHelper(raw string) (helper, rest string, ok bool) {
	i := strings.Index(raw, "::")
	if i <= 0 {
		return "", "", false
	}
	for _, r := range raw[:i] {
		if !isSchemeRune(r) {
			return "", "", false
		}
	}
	return raw[:i], raw[i+2:], true
}

// splitScheme detects "<scheme>://".
func splitScheme(raw string) (scheme, rest string, ok bool) {
	i := strings.Index(raw, "://")
	if i <= 0 {
		return "", "", false
	}
	scheme = strings.ToLower(raw[:i])
	if !('a' <= scheme[0] && scheme[0] <= 'z') {
		return "", "", false
	}
	for _, r := range scheme {
		if !isSchemeRune(r) {
			return "", "", false
		}
	}
	return scheme, raw[i+3:], true
}

// splitSCP detects git's user@host:path shorthand, which has no scheme. A "/"
// before the "@" means a filesystem path containing an "@" (./mods/a@b/net),
// not a remote.
func splitSCP(raw string) (authority, path string, ok bool) {
	at := strings.Index(raw, "@")
	if at <= 0 || strings.Contains(raw[:at], "/") {
		return "", "", false
	}
	colon := strings.Index(raw[at:], ":")
	if colon < 0 {
		return "", "", false
	}
	colon += at
	return raw[:colon], raw[colon+1:], true
}

func isSchemeRune(r rune) bool {
	return ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z') ||
		('0' <= r && r <= '9') || r == '+' || r == '-' || r == '.'
}

// validRef checks a tag name or commit hash. It is deliberately narrower than
// git-check-ref-format: this is an allowlist of what a module pin may be, and
// the ref is passed to git as part of a refspec.
func validRef(ref string) (why string, ok bool) {
	switch {
	case len(ref) > 128:
		return "it is longer than 128 characters", false
	case strings.HasPrefix(ref, "-"):
		return "it begins with \"-\", which git parses as an option", false
	case strings.HasPrefix(ref, "."), strings.HasSuffix(ref, "."):
		return "it begins or ends with \".\"", false
	case strings.Contains(ref, ".."):
		return "it contains \"..\"", false
	case strings.HasSuffix(ref, ".lock"):
		return "it ends with \".lock\", which git reserves", false
	case strings.HasPrefix(ref, "/"), strings.HasSuffix(ref, "/"), strings.Contains(ref, "//"):
		return "it begins, ends with, or doubles \"/\"", false
	}
	for _, r := range ref {
		switch {
		case 'a' <= r && r <= 'z', 'A' <= r && r <= 'Z', '0' <= r && r <= '9':
		case r == '.', r == '_', r == '-', r == '+', r == '/':
		default:
			return "it contains " + strconv.QuoteRune(r), false
		}
	}
	return "", true
}

// DeriveName derives the identifier a scalar `modules:` entry is loaded under:
// the last path segment of the repository or directory, ".git" removed, "-"
// normalised to "_".
//
// It reads Source.Location, never raw text, which makes the strip order
// structural rather than a rule to remember: Parse has already taken the ":ref"
// suffix off, whereas deriving from raw text would turn
// "https://github.com/acme/infra.git:v1.2.0" into "infra.git".
func DeriveName(s Source) (string, diag.Diagnostics) {
	path := strings.TrimRight(repoPath(s), "/")
	if i := strings.LastIndex(path, "/"); i >= 0 {
		path = path[i+1:]
	}
	name := strings.ReplaceAll(strings.TrimSuffix(path, ".git"), "-", "_")

	if !isIdentifier(name) {
		return "", one(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "no module name can be derived from " + strconv.Quote(s.Location),
			Detail: "A `modules:` entry written as a bare source is loaded under a name taken from its last path " +
				"segment, and that name is used as a resource type: `type: module.<name>`. " +
				strconv.Quote(name) + " is not a usable identifier.",
			Action: "Write the entry in its mapping form and name it yourself:\n" +
				"  - name: my_module\n    source: " + s.Location + refSuffix(s),
			Origin: s.Origin,
		})
	}
	return name, nil
}

// repoPath is the part of a location that names the repository or directory,
// with any authority removed. It uses the same two splitters Parse uses, which
// is the point: git's scp shorthand can carry a repository with no "/" in it at
// all ("git@github.com:infra-db"), and a naive last-"/" split derives the host.
func repoPath(s Source) string {
	if s.Kind != KindGit {
		return s.Location
	}
	if _, rest, ok := splitScheme(s.Location); ok {
		if _, after, ok := strings.Cut(rest, "/"); ok {
			return after
		}
		return ""
	}
	if _, path, ok := splitSCP(s.Location); ok {
		return path
	}
	return s.Location
}

func refSuffix(s Source) string {
	if s.Ref == "" {
		return ""
	}
	return ":" + s.Ref
}

func isIdentifier(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case 'a' <= r && r <= 'z', 'A' <= r && r <= 'Z', r == '_':
		case '0' <= r && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// one wraps a single diagnostic in a Diagnostics collection.
func one(d diag.Diagnostic) diag.Diagnostics {
	return diag.Diagnostics{d}
}
