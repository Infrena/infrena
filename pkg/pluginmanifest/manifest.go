// Package pluginmanifest reads and validates `plugin.yaml`, the file a plugin
// repository publishes so that compatibility can be judged without downloading
// a binary.
//
// It is public so that a plugin can validate its own manifest in its own
// tests, against the same parser `infrena plugins install` will use. One
// parser, two callers; the alternative is a second implementation of the
// format that drifts from this one.
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

	"github.com/infrena/infrena/pkg/semver"
)

// Version is the manifest format version this build writes. Reading is
// governed by Supported, which is wider.
const Version = 2

// Supported lists every manifest format version this build can read, newest
// first.
//
// Format 1 spelled the compatibility floor with the project's pre-rename name
// and is gone: nothing declares it. A manifest claiming it is refused rather
// than read leniently, because an absent floor means unconstrained and a
// plugin whose floor was quietly dropped would claim to run against anything.
var Supported = []int{2}

// Manifest is a parsed `plugin.yaml`.
type Manifest struct {
	// Format is the `manifest:` key: the version of the file format, not of
	// the plugin.
	Format int
	// Name is the plugin's name: its binary is infrena-plugin-<Name>,
	// Plugin.Name returns it, and every resource type it serves is prefixed
	// with it.
	Name string
	// Version is the plugin's own version, which must equal the tag this
	// manifest was read at.
	Version semver.Version
	// Protocol is every plugin protocol version the plugin can speak.
	// Non-empty.
	Protocol []int
	// Platforms is every published build.
	Platforms []Platform
	// Description is one line, for a search result.
	Description string
	// Infrena is the releases this plugin is known to work with. The zero
	// value means unconstrained, and is what an absent key gives: a plugin
	// that has made no claim is distinguishable from one claiming to work with
	// everything.
	Infrena semver.Constraint
	// Source is where the plugin lives, for a search result to link. Optional.
	Source string
}

// Platform is one GOOS/GOARCH pair.
type Platform struct {
	OS, Arch string
}

// String renders a platform the way it is written in a manifest, as
// `linux/amd64`.
func (p Platform) String() string { return p.OS + "/" + p.Arch }

// ParsePlatform reads a `GOOS/GOARCH` string such as `linux/amd64`.
//
// The halves are checked for shape only, never against a list of known values:
// Go adds platforms, and a closed list here would refuse a manifest that is
// perfectly valid for a newer toolchain.
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

// SpeaksProtocol reports whether the plugin shares a protocol version with a
// host.
//
// It takes the host's whole set, not just its newest version, because a host
// keeps talking to plugins written against any protocol it still supports.
func (m *Manifest) SpeaksProtocol(hostSupports []int) bool {
	for _, v := range m.Protocol {
		if slices.Contains(hostSupports, v) {
			return true
		}
	}
	return false
}

// AllowsInfrena reports whether the plugin accepts a running infrena version.
//
// A development build is exempt, as it is for a project's own `infrena:`
// floor: a complaint about a developer's own build is not something they can
// act on. Anything parsing as 0.0.0 counts as one, which includes the
// pseudo-version a `go build` in a checkout reports. An unparseable version is
// allowed for the same reason — a version this package cannot read is not
// evidence that the plugin is incompatible.
func (m *Manifest) AllowsInfrena(infrenaVersion string) bool {
	if m.Infrena.IsZero() {
		return true
	}
	v, err := semver.Parse(infrenaVersion)
	if err != nil {
		return true
	}
	if v.Major == 0 && v.Minor == 0 && v.Patch == 0 {
		return true
	}
	return m.Infrena.Allows(v)
}

// manifestFile is the file form. Written out by hand rather than derived from
// Manifest, so that what is and is not part of the published format is
// explicit.
type manifestFile struct {
	Manifest    int      `yaml:"manifest"`
	Name        string   `yaml:"name"`
	Version     string   `yaml:"version"`
	Protocol    []int    `yaml:"protocol"`
	Platforms   []string `yaml:"platforms"`
	Description string   `yaml:"description"`
	Infrena     string   `yaml:"infrena"`
	Source      string   `yaml:"source"`
}

// Parse reads a manifest, returning any warnings alongside it.
//
// The format version is read first, on its own, before anything else is
// decoded: a manifest from the future must report that it needs a newer
// infrena rather than an error about a key nobody recognises. A manifest's
// reader cannot be upgraded in step with its writer.
//
// For a version this build knows, unknown keys are refused — a typo is a
// mistake its author wants to hear about. For a version it does not know,
// unknown keys are expected, so they are tolerated and the caller gets a
// warning instead.
func Parse(data []byte) (*Manifest, []string, error) {
	format, err := parseFormat(data)
	if err != nil {
		return nil, nil, err
	}

	// A format OLDER than anything supported is refused, where a newer one is
	// only warned about. The asymmetry is the point: a newer format can only
	// ADD keys, and ignoring what this build does not know is safe, whereas an
	// older format may spell something this build no longer reads. Warning and
	// carrying on would drop that key's meaning silently, and for the
	// compatibility floor silence means unconstrained.
	if oldest := Supported[len(Supported)-1]; format < oldest {
		return nil, nil, fmt.Errorf(
			"plugin.yaml: this manifest is format version %d, which this build of infrena no "+
				"longer reads; write `manifest: %d`", format, oldest)
	}

	var warnings []string
	known := slices.Contains(Supported, format)
	if !known {
		warnings = append(warnings, fmt.Sprintf(
			"this manifest is format version %d; this build of infrena understands %s, so "+
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
// A separate, lenient pass: the document may hold keys this build has never
// heard of, and none of them may stop it answering which format this is.
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
	if strings.TrimSpace(file.Infrena) != "" {
		c, err := semver.ParseConstraint(file.Infrena)
		if err != nil {
			return nil, fmt.Errorf("`infrena`: %w", err)
		}
		m.Infrena = c
	}
	for _, p := range file.Platforms {
		parsed, err := ParsePlatform(p)
		if err != nil {
			return nil, fmt.Errorf("`platforms`: %w", err)
		}
		m.Platforms = append(m.Platforms, parsed)
	}

	// Passing whether `version:` was present distinguishes an absent key from a
	// declared 0.0.0, which need different messages.
	if err := m.validate(strings.TrimSpace(file.Version) != ""); err != nil {
		return nil, err
	}
	return m, nil
}

// Validate checks a Manifest built in Go, for a plugin asserting its own
// manifest against its code without going through YAML.
func (m *Manifest) Validate() error { return m.validate(true) }

func (m *Manifest) validate(hadVersion bool) error {
	if m.Name == "" {
		return errors.New("`name` is required: it is the plugin's name, so the binary is " +
			"infrena-plugin-<name> and every resource type is prefixed with it")
	}
	// The name becomes both a filename and a resource-type prefix, so anything
	// that would break either is refused here rather than at load.
	if i := strings.IndexAny(m.Name, "./\\ \t:"); i >= 0 {
		return fmt.Errorf("`name` may not contain %q: it becomes both a filename "+
			"(infrena-plugin-%s) and a resource type prefix (%s.…)",
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
