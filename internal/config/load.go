package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"gopkg.in/yaml.v3"
)

// ProjectFileName is the root configuration file. It is the only required one.
const ProjectFileName = "infra.yml"

// VariablesFileName holds project-wide variable VALUES (PLAN.md §8): a flat
// mapping of name to value. Typed DECLARATIONS — type, default, min, max —
// live under infra.yml's `variables:` key instead (PLAN.md §9). The two are
// different things and stage 2 decodes them differently, which is why File
// carries a Kind rather than letting stage 2 guess from the path.
const VariablesFileName = "variables.yml"

// EnvironmentsDirName holds one file per environment (PLAN.md §8). Each file
// is that environment's overrides.
const EnvironmentsDirName = "environments"

// ModuleFileName is the file a module's source directory must contain
// (PLAN.md §11.3).
//
// Deliberately NOT infra.yml, confirmed as a product-API decision (contract
// Amendment 1). A module file is a different document shape — `inputs` and
// `outputs`, no `project` — and two names make the shapes distinguishable by
// construction: neither decoder accepts the other's keys, so four bespoke
// rejection rules never have to exist. It also stops a module directory looking
// like a project: under a shared name, `infrata plan dev` run inside
// modules/networking/ would find a valid infra.yml and TRY, producing a pile of
// "variable not set" errors describing a situation that is not a mistake.
const ModuleFileName = "module.yml"

// FileKind says which of the three shapes a loaded file has. Stage 1 does not
// read a file's contents — it is not permitted to touch yaml.Node — so this is
// derived entirely from the path.
type FileKind uint8

const (
	// FileProject is infra.yml: `project`, `resources`, `variables`,
	// `environments`.
	FileProject FileKind = iota
	// FileVariables is variables.yml: a flat mapping of name to value.
	FileVariables
	// FileEnvironment is environments/<name>.yml: one environment's overrides.
	FileEnvironment
	// FileModule is a module's module.yml: `inputs`, `resources`, `modules`,
	// `outputs`.
	FileModule
)

// String names a file kind for diagnostics. Explicit default, for the reason
// diag.Severity.String() gives: an unrecognised kind must not report as a real
// one.
func (k FileKind) String() string {
	switch k {
	case FileProject:
		return "project file"
	case FileVariables:
		return "variables file"
	case FileEnvironment:
		return "environment file"
	case FileModule:
		return "module file"
	default:
		return fmt.Sprintf("FileKind(%d)", uint8(k))
	}
}

// File is a parsed source file. The node tree keeps line and column
// information, which every diagnostic depends on.
type File struct {
	Path string
	Kind FileKind
	// Environment is the environment a FileEnvironment file configures, taken
	// from its base name with the extension removed. Empty for other kinds.
	Environment string
	Root        *yaml.Node
}

// Load reads the project file, the optional variables file, and every
// environment file, in that order (spec §7, stage 1).
//
// THE ORDER IS PART OF THE CONTRACT. Stage 2's duplicate diagnostics say where
// the FIRST definition was, so "first" must not depend on the filesystem;
// infra.yml must come first so the project name and origin come from the
// project file; and invariant 6 (plan determinism) means the same directory
// must always produce the same slice. os.ReadDir happens to sort its results,
// but that is a property of a function this code does not own.
//
// infra.yml is required. The other two are optional and their absence is not a
// diagnostic: a project with no variables is a valid project, and environments
// may be declared inline under infra.yml's `environments:` key instead.
//
// A YAML syntax error is returned as an error rather than collected as a
// diagnostic, because a file that did not parse has no node tree and therefore
// no Origin for a diagnostic to point at.
func Load(dir string) ([]File, error) {
	project, err := loadProjectFile(dir)
	if err != nil {
		return nil, err
	}
	files := []File{project}

	vars, found, err := loadOptionalFile(filepath.Join(dir, VariablesFileName), FileVariables, "")
	if err != nil {
		return nil, err
	}
	if found {
		files = append(files, vars)
	}

	envs, err := loadEnvironmentDir(filepath.Join(dir, EnvironmentsDirName))
	if err != nil {
		return nil, err
	}
	return append(files, envs...), nil
}

func loadProjectFile(dir string) (File, error) {
	path := filepath.Join(dir, ProjectFileName)
	f, found, err := loadOptionalFile(path, FileProject, "")
	if err != nil {
		return File{}, err
	}
	if !found {
		return File{}, fmt.Errorf("no %s found in %s; run `infra init` to create one", ProjectFileName, dir)
	}
	return f, nil
}

// loadOptionalFile reads one file, reporting absence as (File{}, false, nil)
// rather than as an error, so that callers decide whether absence matters.
func loadOptionalFile(path string, kind FileKind, environment string) (File, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return File{}, false, nil
		}
		return File{}, false, err
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return File{}, false, fmt.Errorf("%s: %w", path, err)
	}
	return File{Path: path, Kind: kind, Environment: environment, Root: &root}, true, nil
}

// loadEnvironmentDir reads environments/, returning its files sorted by
// environment name.
//
// The directory is optional in its entirety, and so is its content: an empty
// environments/ says nothing, and a user who names an environment that does
// not exist gets a far better message from stage 3, which knows which ones do.
func loadEnvironmentDir(dir string) ([]File, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		if errors.Is(err, syscall.ENOTDIR) {
			// environments/ exists but is a plain file: a typo, a `touch`
			// where `mkdir` was meant, or a bad merge. Left as the raw
			// ENOTDIR error, this would name the path but neither the
			// expectation nor an action — exactly the shape spec §44
			// forbids, and the duplicate-spelling error below already
			// gets right.
			return nil, fmt.Errorf(
				"%s exists but is not a directory; it must be a directory holding one YAML file per environment. "+
					"Remove the file or move it aside, then create %s as a directory.",
				dir, dir)
		}
		return nil, err
	}

	paths := map[string]string{} // environment name -> path
	var names []string
	for _, e := range entries {
		// Environments do not nest. Recursing would invent a structure
		// nothing else in the engine understands, and a directory called
		// `old/` is a perfectly ordinary thing to find in a real repository.
		if e.IsDir() {
			continue
		}
		name, ok := environmentNameFor(e.Name())
		if !ok {
			// Not YAML. README.md, .gitkeep and editor droppings live in real
			// directories; refusing them would make the tool unusable.
			continue
		}
		path := filepath.Join(dir, e.Name())
		if other, dup := paths[name]; dup {
			// Both spellings of the same environment. Picking one would drop
			// the other file's overrides from every plan with nothing
			// printed — the exact silent-loss shape this engine refuses.
			first, second := other, path
			if second < first {
				first, second = second, first
			}
			return nil, fmt.Errorf(
				"environment %q is defined by both %s and %s; one would silently shadow the other. "+
					"Delete or rename one of them.", name, first, second)
		}
		paths[name] = path
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]File, 0, len(names))
	for _, name := range names {
		f, found, err := loadOptionalFile(paths[name], FileEnvironment, name)
		if err != nil {
			return nil, err
		}
		if !found {
			// Deleted between ReadDir and ReadFile. An environment that is no
			// longer there is the same as one that never was.
			continue
		}
		out = append(out, f)
	}
	return out, nil
}

// LoadModule reads one module file (compiler stage 1), for a module's already
// resolved source directory.
//
// Separate from Load because a module directory is not a project: it has no
// infra.yml, no variables.yml and no environments/, and it has no environment of
// its own for Load's file walk to discover. Stage 5 calls this once per module
// SOURCE and hands the result to DecodeModule, which is how stage 5 loads module
// sources while never touching a yaml.Node itself (contract Ruling 2, as refined
// by Amendment 1).
//
// dir is already resolved. Turning a `source:` into a directory — joining a
// relative path against the file that named it, or fetching and caching a git
// remote — is internal/modules/source's job (Amendment 10), because only it knows
// which file named the source and where the cache lives.
//
// Failures are errors rather than diagnostics, for Load's reason: a file that did
// not load has no node tree, so there is no Origin for a diagnostic to point at.
// Each of the three cases below reads identically as a bare "no such file", which
// names the path and neither the expectation nor an action — exactly the shape
// §44 forbids.
func LoadModule(dir string) (File, error) {
	path := filepath.Join(dir, ModuleFileName)
	f, found, err := loadOptionalFile(path, FileModule, "")
	if err != nil {
		return File{}, err
	}
	if found {
		return f, nil
	}

	info, statErr := os.Stat(dir)
	switch {
	case statErr != nil && os.IsNotExist(statErr):
		return File{}, fmt.Errorf(
			"module source %s does not exist; a module `source` names a directory containing %s. "+
				"Create the directory, or correct the `source`.", dir, ModuleFileName)
	case statErr != nil:
		return File{}, statErr
	case !info.IsDir():
		return File{}, fmt.Errorf(
			"module source %s is a file, not a directory; a module `source` names a directory containing %s. "+
				"Point `source` at the directory instead.", dir, ModuleFileName)
	}

	if _, err := os.Stat(filepath.Join(dir, ProjectFileName)); err == nil {
		return File{}, fmt.Errorf(
			"module source %s contains %s but no %s; a module declares `inputs`, `resources` and `outputs` and no `project`. "+
				"Rename %s to %s.", dir, ProjectFileName, ModuleFileName, ProjectFileName, ModuleFileName)
	}

	return File{}, fmt.Errorf(
		"module source %s contains no %s. Create it with an `inputs`, `resources` and `outputs` block.",
		dir, ModuleFileName)
}

// environmentNameFor returns the environment a file in environments/
// configures, and whether the file is a YAML file at all.
//
// Both .yml and .yaml are accepted. Ignoring one of them would produce an
// environment that exists in the repository and not in the tool, which the
// user discovers as "unknown environment" while looking straight at the file.
func environmentNameFor(base string) (string, bool) {
	for _, ext := range []string{".yml", ".yaml"} {
		if strings.HasSuffix(base, ext) {
			name := strings.TrimSuffix(base, ext)
			if name == "" {
				// A file literally named ".yml" names no environment.
				return "", false
			}
			return name, true
		}
	}
	return "", false
}
