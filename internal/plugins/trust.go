package plugins

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// IsTrusted reports whether the user has trusted a source infrena is about to
// download an executable from.
//
// TRUST WIDENS DOWNWARDS AND NEVER UPWARDS, and that asymmetry is the whole
// rule:
//
//   - An approved OWNER covers every repository under it. "I trust this
//     publisher" is the unit a person can actually reason about, and it is
//     asked once rather than again for each repository the publisher ships.
//   - An approved REPOSITORY covers that repository and nothing else. It does
//     not reach a sibling, and it does not reach the owner. A user who deliberately
//     narrowed their approval to one repository said the narrow thing on purpose;
//     widening it for them would hand the rest of an owner's repositories a trust
//     nobody granted, and the whole point of PLAN.md section 31.3 is that only a
//     user may trust a place binaries come from.
//
// Both directions matter because they fail differently. Failing to widen
// downwards is an annoyance - one more prompt. Widening upwards is a security
// hole that nothing in the interface would ever show.
func IsTrusted(trusted []Source, s Source) bool {
	for _, t := range trusted {
		if t.Host != s.Host || t.Owner != s.Owner {
			continue
		}
		switch t.Kind {
		case KindOwner:
			// The owner is approved, so anything under it is too, including
			// the owner source itself.
			return true
		case KindRepository:
			if s.Kind == KindRepository && s.Repo == t.Repo {
				return true
			}
		}
	}
	return false
}

// Approve records a source in the USER's own trusted-sources file, creating it
// if it is not there, so the question is asked once rather than on every run.
//
// IT IS IDEMPOTENT AND IT PRESERVES THE FILE. A source already trusted - by
// name, or because the always-trusted official owner covers it - writes
// nothing at all, so the file is not even touched. When a source IS added, the
// existing document is edited through a yaml.Node rather than re-serialised
// from a struct: the comments a user wrote beside a source are the only record
// of WHY they trusted it, and this is the last file in infrena where a machine
// should quietly delete a human's reasoning.
//
// A FILE THAT DOES NOT PARSE IS NOT OVERWRITTEN. It is reported, naming the
// path, and left exactly as it was. Replacing it would destroy what the user
// meant to say and silently change what infrena trusts - the two worst
// outcomes available on this path - and the fix is one the user must make by
// hand anyway.
//
// The write is atomic: a temporary file in the same directory, renamed over
// the original, so an interrupted approval leaves the old file rather than half
// a new one.
func Approve(configHome string, s Source) error {
	if configHome == "" {
		return fmt.Errorf("cannot approve %s: infrena has no configuration directory to record it in.\n\nAn approval that is not written down would be asked for again on every run.\n\nSuggested action:\n  Set HOME (or XDG_CONFIG_HOME) so there is somewhere to keep %s.",
			s, filepath.Join("infrena", "plugins.yml"))
	}

	path := TrustedPath(configHome)

	// Read through the same parser LoadTrusted uses, so a file this refuses to
	// understand is refused here too rather than being rewritten into
	// something it does understand.
	existing, err := readTrusted(path)
	if err != nil {
		return fmt.Errorf("cannot approve %s: %w.\n\nSuggested action:\n  Fix %s by hand, then approve again. It has not been modified.", s, err, path)
	}

	// The official owner is trusted by LoadTrusted whatever the file says, so
	// recording it would only make the user's own list longer without changing
	// anything.
	if IsTrusted(append([]Source{OfficialOwner()}, existing...), s) {
		return nil
	}

	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("reading %s before approving %s: %w", path, s, err)
	}

	updated, err := appendSource(data, s)
	if err != nil {
		return fmt.Errorf("cannot approve %s: %w.\n\nSuggested action:\n  Fix %s by hand, then approve again. It has not been modified.", s, err, path)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating the directory for %s: %w", path, err)
	}
	return writeAtomic(path, updated)
}

// appendSource adds one entry to the `sources:` list of a plugins.yml, keeping
// everything else in the document - including comments - where it was.
//
// An empty or missing document gets a fresh one with a header saying what the
// file is for, because the first time this runs is the first time the user sees
// it, and a bare list of hostnames explains nothing about why it decides what
// infrena may download.
func appendSource(data []byte, s Source) ([]byte, error) {
	entry := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s.String()}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("the existing file is not valid YAML: %w", err)
	}

	if doc.Kind == 0 || len(doc.Content) == 0 {
		return marshalDoc(freshDocument(entry))
	}

	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("the existing file is not a mapping: expected a single `sources:` list")
	}

	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "sources" {
			continue
		}
		list := root.Content[i+1]
		switch list.Kind {
		case yaml.SequenceNode:
			list.Content = append(list.Content, entry)
		case yaml.ScalarNode:
			if list.Tag != "!!null" {
				return nil, fmt.Errorf("`sources` is %s rather than a list", list.Tag)
			}
			// `sources:` with nothing under it. Replace the null in place, so
			// any comment attached to the key survives.
			*list = yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{entry}}
		default:
			return nil, fmt.Errorf("`sources` is not a list")
		}
		return marshalDoc(&doc)
	}

	// A file with other content but no sources key at all: add one, leaving
	// whatever else is there untouched.
	root.Content = append(root.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "sources"},
		&yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{entry}},
	)
	return marshalDoc(&doc)
}

// freshDocument builds the file a user sees the first time they approve
// anything.
func freshDocument(entry *yaml.Node) *yaml.Node {
	key := &yaml.Node{
		Kind:        yaml.ScalarNode,
		Tag:         "!!str",
		Value:       "sources",
		HeadComment: "Places infrena may download plugins from, approved by you.\nA line naming an owner covers every repository under it.\nProject configuration can name a source but can never add one here.",
	}
	return &yaml.Node{
		Kind: yaml.DocumentNode,
		Content: []*yaml.Node{{
			Kind: yaml.MappingNode,
			Tag:  "!!map",
			Content: []*yaml.Node{key, {
				Kind:    yaml.SequenceNode,
				Tag:     "!!seq",
				Content: []*yaml.Node{entry},
			}},
		}},
	}
}

// marshalDoc renders a document at the two-space indent the rest of infrena's
// YAML uses.
func marshalDoc(doc *yaml.Node) ([]byte, error) {
	var buf []byte
	w := &byteWriter{buf: &buf}
	enc := yaml.NewEncoder(w)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("writing the trusted sources: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("writing the trusted sources: %w", err)
	}
	return buf, nil
}

// byteWriter collects an encoder's output without pulling in bytes.Buffer for
// one use.
type byteWriter struct{ buf *[]byte }

func (w *byteWriter) Write(p []byte) (int, error) {
	*w.buf = append(*w.buf, p...)
	return len(p), nil
}

// writeAtomic replaces a file in one step, so an interrupted write leaves the
// previous contents rather than a truncated file that decides what infrena is
// allowed to download.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("creating a temporary file in %s: %w", dir, err)
	}
	name := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(name)
	}()

	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("writing %s: %w", name, err)
	}
	// 0644 rather than the 0600 a temporary file is created with: this is
	// ordinary configuration a user edits, not a secret.
	if err := tmp.Chmod(0o644); err != nil {
		return fmt.Errorf("setting the mode of %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", name, err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("moving the approved sources into %s: %w", path, err)
	}
	return nil
}
