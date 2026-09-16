package plugins

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// officialOwner is the organisation infrena's own plugins are published under.
//
// IT IS ALWAYS TRUSTED AND CANNOT BE REMOVED. This is deliberately not a
// configurable default: a default that can be configured away is one that gets
// configured away, usually by a half-edited file, and after that `plugin: aws`
// reports that nothing matches with no visible cause. Making it unremovable
// costs a user nothing - they can still add their own sources - and removes a
// whole class of unexplainable "no such plugin" reports.
const officialOwner = "github.com/infrena"

// trustedFile is the shape of `~/.config/infrena/plugins.yml`.
//
// This is the USER's file, not project configuration, so it is parsed into this
// typed struct directly. It never goes through internal/config and never sees a
// yaml.Node: that machinery exists to carry source origins into project
// diagnostics, and this file has no project to belong to.
type trustedFile struct {
	// Sources are the places the user trusts infrena to fetch plugins from,
	// in the order they wrote them.
	Sources []string `yaml:"sources"`
}

// OfficialOwner is the source infrena's own plugins are published under. It is
// always trusted, whatever the user's configuration says.
func OfficialOwner() Source {
	src, err := ParseSource(officialOwner)
	if err != nil {
		// officialOwner is a constant in this file; if it does not parse the
		// package is built wrong and no caller can do anything useful about it.
		panic(fmt.Sprintf("plugins: the official owner %q does not parse: %v", officialOwner, err))
	}
	return src
}

// TrustedPath is where the user's trusted sources are read from, given the
// value of os.UserConfigDir. Taking the config home as an argument is what lets
// a test point the whole thing at a temporary directory.
func TrustedPath(configHome string) string {
	return filepath.Join(configHome, "infrena", "plugins.yml")
}

// LoadTrusted reads the sources the user trusts, with the official owner first
// and duplicates removed.
//
// A missing file is not an error: having written no configuration is the
// ordinary state of a fresh machine, and such a user still gets the official
// owner. A file that exists but cannot be read or understood IS an error naming
// the file, never a quiet fallback to the official owner alone - this file is a
// security decision the user wrote down, and silently ignoring it would either
// hide a source they meant to add or leave them believing they had removed one.
func LoadTrusted(configHome string) ([]Source, error) {
	path := TrustedPath(configHome)

	user, err := readTrusted(path)
	if err != nil {
		return nil, err
	}

	sources := make([]Source, 0, len(user)+1)
	seen := make(map[string]bool, len(user)+1)
	for _, src := range append([]Source{OfficialOwner()}, user...) {
		key := src.String()
		if seen[key] {
			continue
		}
		seen[key] = true
		sources = append(sources, src)
	}
	return sources, nil
}

// readTrusted parses the user's file, returning nil for a file that is not
// there.
func readTrusted(path string) ([]Source, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the trusted plugin sources in %s: %w; either make the file readable or remove it, and infrena will trust %s alone", path, err, officialOwner)
	}

	var file trustedFile
	dec := yaml.NewDecoder(bytes.NewReader(data))
	// Fail closed on an unknown key. A typo in the one file that says where
	// infrena may fetch executables from is the worst possible place to be
	// lenient: `sourecs:` silently ignored reads, to the user, as a source they
	// added that never took effect.
	dec.KnownFields(true)
	if err := dec.Decode(&file); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("the trusted plugin sources in %s are not valid YAML: %w; expected a single `sources:` list of places to fetch plugins from, such as `sources:\n  - %s/<owner>`", path, err, GitHubHost)
	}

	sources := make([]Source, 0, len(file.Sources))
	for i, raw := range file.Sources {
		src, err := ParseSource(raw)
		if err != nil {
			return nil, fmt.Errorf("the trusted plugin sources in %s are not usable: sources[%d]: %w", path, i, err)
		}
		sources = append(sources, src)
	}
	return sources, nil
}
