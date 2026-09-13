// Package pluginmanifest reads and validates `plugin.yaml`, the file a plugin
// repository publishes so a search can judge compatibility WITHOUT downloading a
// binary. PLAN.md §31.2.
//
// PUBLIC, for the same reason pkg/semver is: a plugin validates its own manifest in its
// own tests, and the alternative is a second implementation of this format that drifts
// from the one `infrata plugins install` checks it with. One parser, two callers.
//
// It is also the only practical way a plugin repository can validate the file at all.
// Reading YAML needs a YAML parser; infrata already depends on gopkg.in/yaml.v3 and a
// plugin author accepts that transitively by importing this package, where adding their
// own would be a third-party dependency in a module whose rule is the standard library
// plus infrata.
//
// NOTE ON THE ARCHITECTURE RULE: internal/config is "the only place in the engine
// permitted to touch yaml.Node", and this package does not touch it — it decodes into
// typed structs, so no untyped document flows anywhere. Project configuration and a
// plugin manifest are different documents with different readers, and each has exactly
// one door.
package pluginmanifest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/infrata/infrata/pkg/semver"
)

// Version is the manifest format version this build understands.
const Version = 1

// Supported lists every manifest format version this build can read.
var Supported = []int{1}

// Manifest is a parsed `plugin.yaml`.
type Manifest struct {
	// Format is the `manifest:` key: the version of the FILE FORMAT, not of the plugin.
	Format int
	// Name is the plugin's name: its binary is infrata-plugin-<Name>, Plugin.Name()
	// returns it, and every resource type it serves is prefixed with it.
	Name string
	// Version is the plugin's own version, which must equal the tag this manifest was
	// read at.
	Version semver.Version
	// Protocol is every plugin protocol version the plugin can speak. Non-empty.
	Protocol []int
	// Platforms is every published build.
	Platforms []Platform
	// Description is one line, for a search result.
	Description string
	// Infrata is the releases this plugin is known to work with. The ZERO value means
	// unconstrained, which is what an absent key means — see §31.2 on why absence is
	// preferred to `">= 0.0.0"`.
	Infrata semver.Constraint
	// Source is where the plugin lives, for a search result to link. Optional.
	Source string
}

// Platform is one GOOS/GOARCH pair.
type Platform struct {
	OS, Arch string
}

// String renders a platform as it is written.
func (p Platform) String() string { return p.OS + "/" + p.Arch }

// ParsePlatform reads `linux/amd64`.
//
// The halves are checked for SHAPE only, never against a list of known values: Go adds
// platforms, and a closed list here would refuse a manifest that is perfectly valid for
// a toolchain newer than this build.
func ParsePlatform(s string) (Platform, error) {
	os, arch, ok := strings.Cut(strings.TrimSpace(s), "/")
	if !ok {
		return Platform{}, fmt.Errorf("%q is not a platform: write GOOS/GOARCH, as in linux/amd64", s)
	}
	if os == "" || arch == "" || strings.Contains(arch, "/") {
		return Platform{}, fmt.Errorf("%q is not a platform: write GOOS/GOARCH, as in linux/amd64", s)
	}
	return Platform{OS: os, Arch: arch}, nil
}

// Supports reports whether the plugin publishes a build for a platform.
func (m *Manifest) Supports(p Platform) bool { return slices.Contains(m.Platforms, p) }

// SpeaksProtocol reports whether the plugin shares a protocol version with a host.
//
// Takes the host's whole SET, because the handshake negotiates: a build keeps talking to
// plugins written against a protocol it still supports.
func (m *Manifest) SpeaksProtocol(hostSupports []int) bool {
	for _, v := range m.Protocol {
		if slices.Contains(hostSupports, v) {
			return true
		}
	}
	return false
}

// AllowsInfrata reports whether the plugin accepts a running infrata version.
//
// A development build is EXEMPT, the same exemption §61.2 gives a project's own
// `infrata:` floor and for the same reason: a complaint about a developer's own build is
// not something they can act on. Anything parsing as 0.0.0 counts — a `go build` in a
// checkout with a VCS remote reports a pseudo-version that does.
func (m *Manifest) AllowsInfrata(infrataVersion string) bool {
	if m.Infrata.IsZero() {
		return true
	}
	v, err := semver.Parse(infrataVersion)
	if err != nil {
		return true
	}
	if v.Major == 0 && v.Minor == 0 && v.Patch == 0 {
		return true
	}
	return m.Infrata.Allows(v)
}

// manifestFile is the wire form. Written by hand rather than derived, so what is and is
// not part of the format is explicit — the same reason internal/planner writes its
// artifact's shape out longhand.
type manifestFile struct {
	Manifest    int      `yaml:"manifest"`
	Name        string   `yaml:"name"`
	Version     string   `yaml:"version"`
	Protocol    []int    `yaml:"protocol"`
	Platforms   []string `yaml:"platforms"`
	Description string   `yaml:"description"`
	Infrata     string   `yaml:"infrata"`
	Source      string   `yaml:"source"`
}

// Parse reads a manifest, returning any warnings alongside it.
//
// THE FORMAT VERSION IS READ FIRST, on its own, before anything else is decoded. A
// manifest from the future must report "this needs a newer infrata to describe itself"
// rather than an error about a key nobody recognises — which is the whole reason §31.2
// gives the format a version when the configuration language deliberately has none. The
// reader of a manifest cannot be upgraded in step with its writer.
//
// For a version this build KNOWS, unknown keys are refused: a typo in a manifest is a
// mistake its author wants to hear about. For a version it does not know, unknown keys
// are expected, so they are tolerated and the caller gets a warning instead.
func Parse(data []byte) (*Manifest, []string, error) {
	format, err := parseFormat(data)
	if err != nil {
		return nil, nil, err
	}

	var warnings []string
	known := slices.Contains(Supported, format)
	if !known {
		warnings = append(warnings, fmt.Sprintf(
			"this manifest is format version %d; this build of infrata understands %s, so "+
				"anything it adds is ignored", format, joinInts(Supported)))
	}

	var file manifestFile
	dec := yaml.NewDecoder(bytes.NewReader(data))
	// Unknown keys are an error only for a format this build claims to understand.
	dec.KnownFields(known)
	if err := dec.Decode(&file); err != nil && !errors.Is(err, io.EOF) {
		return nil, warnings, fmt.Errorf("plugin.yaml: %w", err)
	}

	m, err := fromFile(file)
	if err != nil {
		return nil, warnings, err
	}
	return m, warnings, nil
}

// parseFormat reads the `manifest:` key and nothing else.
//
// A separate, LENIENT pass: the document may hold keys this build has never heard of, and
// none of them may prevent it from answering "which format is this?".
func parseFormat(data []byte) (int, error) {
	var probe struct {
		Manifest *int `yaml:"manifest"`
	}
	if err := yaml.Unmarshal(data, &probe); err != nil {
		return 0, fmt.Errorf("plugin.yaml is not valid YAML: %w", err)
	}
	if probe.Manifest == nil {
		return 0, errors.New("plugin.yaml has no `manifest:` key, so its format is unknown; " +
			"the current format is `manifest: " + strconv.Itoa(Version) + "`")
	}
	if *probe.Manifest < 1 {
		return 0, fmt.Errorf("`manifest: %d` is not a format version; the current format is %d",
			*probe.Manifest, Version)
	}
	return *probe.Manifest, nil
}

// fromFile converts the decoded file into a Manifest, validating as it goes.
func fromFile(file manifestFile) (*Manifest, error) {
	m := &Manifest{
		Format:      file.Manifest,
		Name:        strings.TrimSpace(file.Name),
		Description: strings.TrimSpace(file.Description),
		Source:      strings.TrimSpace(file.Source),
		Protocol:    file.Protocol,
	}

	if file.Version != "" {
		v, err := semver.Parse(file.Version)
		if err != nil {
			return nil, fmt.Errorf("`version`: %w", err)
		}
		m.Version = v
	}
	if strings.TrimSpace(file.Infrata) != "" {
		c, err := semver.ParseConstraint(file.Infrata)
		if err != nil {
			return nil, fmt.Errorf("`infrata`: %w", err)
		}
		m.Infrata = c
	}
	for _, p := range file.Platforms {
		parsed, err := ParsePlatform(p)
		if err != nil {
			return nil, fmt.Errorf("`platforms`: %w", err)
		}
		m.Platforms = append(m.Platforms, parsed)
	}

	// hadVersion distinguishes "absent" from "0.0.0", which Validate must refuse
	// differently: one is a missing key and the other is a version nobody releases.
	if err := m.validate(strings.TrimSpace(file.Version) != ""); err != nil {
		return nil, err
	}
	return m, nil
}

// Validate checks a Manifest built in Go, for a plugin asserting its own manifest
// against its code without going through YAML.
func (m *Manifest) Validate() error { return m.validate(true) }

func (m *Manifest) validate(hadVersion bool) error {
	if m.Name == "" {
		return errors.New("`name` is required: it is the plugin's name, so the binary is " +
			"infrata-plugin-<name> and every resource type is prefixed with it")
	}
	// The name becomes a filename AND a resource-type prefix, so anything that would
	// break either is refused here rather than at load.
	if i := strings.IndexAny(m.Name, "./\\ \t:"); i >= 0 {
		return fmt.Errorf("`name` may not contain %q: it becomes both a filename "+
			"(infrata-plugin-%s) and a resource type prefix (%s.…)",
			string(m.Name[i]), m.Name, m.Name)
	}
	if !hadVersion {
		return errors.New("`version` is required, and must equal the tag this manifest is read at")
	}
	if len(m.Protocol) == 0 {
		return errors.New("`protocol` is required and must list at least one version: it is " +
			"every plugin protocol version this plugin can speak")
	}
	for _, v := range m.Protocol {
		if v < 1 {
			return fmt.Errorf("`protocol` contains %d, which is not a protocol version", v)
		}
	}
	if len(m.Platforms) == 0 {
		return errors.New("`platforms` is required and must list at least one GOOS/GOARCH: " +
			"without it nothing can tell whether a build exists for the machine asking")
	}
	if m.Description == "" {
		return errors.New("`description` is required: one line, for a search result to show")
	}
	return nil
}

func joinInts(ns []int) string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = strconv.Itoa(n)
	}
	return strings.Join(out, ", ")
}
