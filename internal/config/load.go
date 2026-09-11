package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

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
