package config

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ProjectFileName is the root configuration file. It is the only required one.
const ProjectFileName = "infrena.yml"

// VariablesFileName holds project-wide variable values: a flat mapping of name
// to value. Typed declarations — type, default, min, max — live under
// infrena.yml's `variables:` key instead. The two are different things and are
// decoded differently, which is why File carries a Kind rather than letting the
// decoder guess from the path.
const VariablesFileName = "variables.yml"

// EnvironmentsDirName holds one file per environment, each holding that
// environment's overrides.
const EnvironmentsDirName = "environments"

// ModuleFileName is the file a module's source directory must contain.
//
// Deliberately not infrena.yml. A module file is a different document shape —
// `inputs` and `outputs`, no `project` — and two names make the shapes
// distinguishable by construction, so neither decoder has to reject the other's
// keys case by case. It also stops a module directory looking like a project:
// under a shared name, `infrena plan dev` run inside modules/networking/ would
// find a valid infrena.yml and try, producing a pile of "variable not set"
// errors describing a situation that is not a mistake.
const ModuleFileName = "module.yml"

// ResourcesDirName holds resource declarations, globbed recursively. A file
// under it carries a `resources:` block and means exactly what the same block
// in infrena.yml means.
const ResourcesDirName = "resources"

// VarsDirName holds variable values, globbed recursively. At its top level the
// filename names the environment — default.yml for all of them, <env>.yml for
// one — and deeper files carry the environment inside.
const VarsDirName = "vars"

// DiscoveredDirName holds configuration written by `infrena import --generate`.
//
// It is loaded, not staged. Import adds a resource to state, and a resource in
// state that no configuration declares is scheduled for destruction — so a
// staging area would mean import followed by apply destroys what was just
// adopted.
const DiscoveredDirName = "discovered"

// ScopedVarsDirName is the directory inside a resources directory holding
// variables visible only to the resources declared there.
const ScopedVarsDirName = "vars"

// TemplatesDirName holds text blobs rendered into attributes, read on demand
// rather than loaded: `${template.NAME}` interpolates one in the same grammar
// as configuration, `${file.NAME}` reads it verbatim. Resolution lives in
// internal/cli, which is why nothing here reads it — the resources walk skips
// this directory so its contents are never mistaken for declarations.
const TemplatesDirName = "templates"

// FileKind says which shape a loaded file has. Loading does not inspect a
// file's contents, so this is derived entirely from the path.
type FileKind uint8

const (
	// FileProject is infrena.yml: `project`, `resources`, `variables`,
	// `environments`.
	FileProject FileKind = iota
	// FileVariables is variables.yml: a flat mapping of name to value.
	FileVariables
	// FileEnvironment is environments/<name>.yml: one environment's overrides.
	FileEnvironment
	// FileModule is a module's module.yml: `inputs`, `resources`, `modules`,
	// `outputs`.
	FileModule
	// FileResources is a file under resources/**: a `resources:` block, and
	// nothing else. Equivalent to infrena.yml's own `resources:` key, since the
	// directory is how a project is organised rather than a second way to mean
	// something.
	FileResources
	// FileVars is a file under vars/** or resources/<dir>/vars/**. Its shape
	// depends on where it sits: at the top of vars/ the filename names the
	// environment, and deeper the environments are named inside.
	FileVars
)

// String names a file kind for diagnostics. The default case is explicit so
// that an unrecognised kind cannot report as a real one.
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
	// ScopeDir is the resources directory a scoped vars file belongs to, as a
	// slash-separated path relative to the project — "database" for
	// resources/database/vars/sizes.yml. Empty for everything else, which is
	// what distinguishes a project-wide vars file from a scoped one.
	ScopeDir string
	// Dir is the resources directory a FileResources file was declared in, in
	// the same form, so a resource can be given the scope of the directory it
	// came from. Empty for infrena.yml and for discovered/.
	Dir  string
	Root *yaml.Node
}

// Load reads the project file, the optional variables file, and every
// environment file, in that order.
//
// The order is part of the contract. A duplicate diagnostic says where the
// first definition was, so "first" must not depend on the filesystem;
// infrena.yml must come first so the project name and origin come from the
// project file; and a plan is only deterministic if the same directory always
// produces the same slice.
//
// infrena.yml is required. The rest are optional and their absence is not a
// diagnostic: a project with no variables is a valid project, and environments
// may be declared inline under infrena.yml's `environments:` key instead.
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
	files = append(files, envs...)

	// The conventional directories, in a fixed order so that a duplicate
	// diagnostic names the same file first on every run.
	for _, c := range []struct {
		name string
		kind FileKind
	}{
		{ResourcesDirName, FileResources},
		{VarsDirName, FileVars},
		{DiscoveredDirName, FileResources},
	} {
		found, err := walkConventionalDir(filepath.Join(dir, c.name), c.kind)
		if err != nil {
			return nil, err
		}
		files = append(files, found...)
	}

	scoped, err := walkScopedVars(filepath.Join(dir, ResourcesDirName))
	if err != nil {
		return nil, err
	}
	return append(files, scoped...), nil
}

// walkScopedVars reads every resources/<dir>/vars/**.yml.
//
// A separate pass rather than a flag on walkConventionalDir, because these
// files are a different kind: the resources walk skips vars/ entirely and this
// one reads only vars/. Threading a mode through one walk would make each
// file's kind depend on where the walk happened to be.
//
// The scope is the resources directory that owns the vars directory, not the
// vars directory itself: resources/database/vars/sizes.yml is scoped to
// "database", the same string a resource file in that directory records as its
// own Dir.
func walkScopedVars(resourcesRoot string) ([]File, error) {
	var out []File
	err := filepath.WalkDir(resourcesRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() || filepath.Base(p) != ScopedVarsDirName || p == resourcesRoot {
			return nil
		}
		owner, err := filepath.Rel(resourcesRoot, filepath.Dir(p))
		if err != nil {
			return err
		}
		found, err := walkConventionalDir(p, FileVars)
		if err != nil {
			return err
		}
		for i := range found {
			// A scoped file's filename never names an environment: the
			// directory already says who it is for, and a second convention on
			// top would make resources/db/vars/production.yml ambiguous between
			// "production's values for db" and "a file called production".
			found[i].Environment = ""
			found[i].ScopeDir = filepath.ToSlash(owner)
			out = append(out, found[i])
		}
		return nil
	})
	if err != nil {
		if os.IsNotExist(err) || notADirectory(resourcesRoot) {
			// A resources/ that is a plain file is treated as absent here
			// rather than reported, because scoped variables are an optional
			// feature of an optional directory — walkConventionalDir reports
			// the same path properly when the resources walk reaches it.
			return nil, nil
		}
		return nil, err
	}
	return out, nil
}

// notADirectory reports whether path exists and is something other than a
// directory. It is how the callers here tell "you wrote a file where a
// directory belongs" from every other reason a read can fail.
//
// It asks the filesystem rather than testing the errno, because
// syscall.ENOTDIR is wrong on Windows in both directions: Windows reports
// ERROR_DIRECTORY when a file is read as a directory, which Go does not map to
// ENOTDIR, and Go defines syscall.ENOTDIR as ERROR_PATH_NOT_FOUND there, which
// is what a genuinely missing path returns.
//
// The extra syscall opens a TOCTOU window that does not matter: it runs only
// after a read has already failed, and the answer chooses the wording of a
// diagnostic rather than granting access to anything.
func notADirectory(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// walkConventionalDir reads every .yml file under root, recursively.
//
// Recursive, unlike loadEnvironmentDir: these directories are how a project is
// organised, so `resources/eu/west/net.yml` is an ordinary thing to write.
// Environments deliberately do not nest, which is why that walk stays separate
// rather than growing a flag.
//
// Sorted by path, once. The file order decides which of two duplicate
// declarations a diagnostic names first, and a project whose resource set
// changes between identical runs is not deterministic.
//
// A missing directory is not an error: most projects use none of these.
func walkConventionalDir(root string, kind FileKind) ([]File, error) {
	// Checked before the walk, because the walk does not fail on it:
	// filepath.WalkDir over a plain file visits that file as a single entry,
	// the .yml test below drops it, and the walk succeeds having read nothing.
	// A project with `resources` as a file would load cleanly with every
	// resource in it missing.
	if notADirectory(root) {
		return nil, fmt.Errorf(
			"%s exists but is not a directory; it must be a directory holding YAML files. "+
				"Remove the file or move it aside, then create %s as a directory.",
			root, root)
	}

	var paths []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// vars/ and templates/ inside a resources directory are not
			// resource files: without this the recursive walk would read
			// resources/db/vars/sizes.yml as a resources file and reject
			// every variable in it.
			if p != root {
				switch filepath.Base(p) {
				case ScopedVarsDirName, TemplatesDirName:
					return fs.SkipDir
				}
			}
			return nil
		}
		// .yml only. A README, a .gitkeep or an editor backup is an ordinary
		// thing to find in a real repository, and refusing to load a project
		// because someone left notes in it is hostile.
		if filepath.Ext(p) != ".yml" {
			return nil
		}
		paths = append(paths, p)
		return nil
	})
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	// filepath.WalkDir already walks in lexical order, so this is belt and
	// braces. It is kept because the order is load-bearing, and replacing
	// WalkDir with ReadDir would otherwise change it silently.
	sort.Strings(paths)

	out := make([]File, 0, len(paths))
	for _, p := range paths {
		// A vars file at the top level of vars/ names its environment with its
		// filename: default.yml for every environment, <env>.yml for one.
		// Deeper files carry the environment inside instead, so the candidate
		// is empty for them.
		//
		// Only a candidate: whether it names a real environment is decided at
		// decode, where the declared environments are known.
		env := ""
		if kind == FileVars && filepath.Dir(p) == root {
			env = strings.TrimSuffix(filepath.Base(p), ".yml")
		}
		f, found, err := loadOptionalFile(p, kind, env)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		if kind == FileResources {
			// The directory this resource was declared in, so it can later be
			// given that directory's scope. Relative to the resources root and
			// slash-separated, so it is the same string on every platform and
			// the same string a scoped vars file computes for itself.
			if rel, err := filepath.Rel(root, filepath.Dir(p)); err == nil && rel != "." {
				f.Dir = filepath.ToSlash(rel)
			}
		}
		out = append(out, f)
	}
	return out, nil
}

func loadProjectFile(dir string) (File, error) {
	path := filepath.Join(dir, ProjectFileName)
	f, found, err := loadOptionalFile(path, FileProject, "")
	if err != nil {
		return File{}, err
	}
	if !found {
		return File{}, fmt.Errorf("no %s found in %s; run `infrena init` to create one", ProjectFileName, dir)
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
		return File{}, false, fmt.Errorf("%s: %w%s", path, err, quotingHint(err, data))
	}
	return File{Path: path, Kind: kind, Environment: environment, Root: &root}, true, nil
}

// loadEnvironmentDir reads environments/, returning its files sorted by
// environment name.
//
// The directory is optional in its entirety, and so is its content: an empty
// environments/ says nothing, and a user who names an environment that does
// not exist gets a far better message later, from the code that knows which
// ones do.
func loadEnvironmentDir(dir string) ([]File, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		if notADirectory(dir) {
			// environments/ exists but is a plain file. Left as the raw OS
			// error this would name the path and neither the expectation
			// nor an action.
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

// LoadModule reads one module file, for a module's already resolved source
// directory.
//
// Separate from Load because a module directory is not a project: it has no
// infrena.yml, no variables.yml and no environments/, and no environment of its
// own for Load's file walk to discover. It is called once per module source,
// and its result goes to DecodeModule, so the caller never touches a yaml.Node.
//
// dir is already resolved. Turning a `source:` into a directory — joining a
// relative path against the file that named it, or fetching and caching a git
// remote — is internal/modules/source's job, because only it knows which file
// named the source and where the cache lives.
//
// Failures are errors rather than diagnostics, for Load's reason: a file that
// did not load has no node tree, so there is no Origin to point at. The three
// cases below are separated because all three otherwise read as a bare "no such
// file", which names the path and neither the expectation nor an action.
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
		if before, ok := strings.CutSuffix(base, ext); ok {
			name := before
			if name == "" {
				// A file literally named ".yml" names no environment.
				return "", false
			}
			return name, true
		}
	}
	return "", false
}

// quotingHint explains the one YAML error a reader cannot act on.
//
// An unquoted interpolation holding a map literal — `tags: ${merge(a, {b: c})}`
// — is rejected by YAML itself, because a plain scalar may not contain ": ".
// The message it gives is "mapping values are not allowed in this context",
// which says nothing about quoting and sends a reader looking for a mapping
// they did not write.
//
// Appended to the error rather than replacing it: the original names the line,
// and a reader who has seen this before should still recognise it.
func quotingHint(err error, data []byte) string {
	if !strings.Contains(err.Error(), "mapping values are not allowed") {
		return ""
	}
	if !strings.Contains(string(data), "${") {
		return ""
	}
	return "\n\nThis usually means an interpolation containing `: ` was left unquoted. " +
		"YAML ends a plain scalar at `: `, so wrap the whole value in quotes:\n" +
		"    tags: \"${merge(var.tags, {team: payments})}\""
}
