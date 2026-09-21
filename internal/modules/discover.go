package modules

import (
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/infrena/infrena/internal/config"
	"github.com/infrena/infrena/internal/diag"
)

// skipDirs are never descended into. `.git` holds thousands of files and no
// modules; `.infrena` holds state and the module cache, and descending into the
// cache would load every module a project has ever fetched under a name taken
// from a content hash.
var skipDirs = map[string]bool{".git": true, ".infrena": true}

// discover finds every directory under the project root holding a module.yml
// and makes it available under its directory name.
//
// Root only. A module's own dependencies come from its own `modules:` list, not
// from whatever directories happen to lie around the project that vendored it,
// or a module would work in one project and not the next. loadModules enforces
// this by calling discover only when the module path is empty.
//
// explicit is the set of names this level's `modules:` list already claims. A
// directory whose derived name is in explicit is skipped entirely — neither
// registered nor checked for a collision — because an explicit entry beats a
// discovered one, the one case where a name collision is not an error.
// Otherwise a vendored module sharing a basename with an unrelated top-level
// directory would trip the duplicate-name check below even though neither
// candidate is ever used.
func (w *walker) discover(root string, explicit map[string]bool) []loadedModule {
	byName := map[string]loadedModule{}
	var order []string

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if path != root && skipDirs[d.Name()] {
			return fs.SkipDir
		}
		// WalkDir does not follow symlinks, which is wanted here rather than an
		// accident to work around: a symlink out of the project root would load
		// a module the user never wrote down.
		if _, statErr := os.Stat(filepath.Join(path, config.ModuleFileName)); statErr != nil {
			return nil
		}
		name := moduleName(d.Name())
		if explicit[name] {
			return nil
		}
		if prior, dup := byName[name]; dup {
			w.ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "two directories both define a module named " + strconv.Quote(name),
				Detail: w.show(prior.Dir) + " and " + w.show(path) +
					" each hold a " + config.ModuleFileName + ", and both derive the same name.",
				Action: "Add one of them to `modules:` with an explicit `name:`, or rename a directory.",
			})
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		byName[name] = loadedModule{
			Name: name,
			Dir:  path,
			// A display string, not a source.Source: a discovered module
			// was never written down, so there is nothing to resolve.
			Source: "./" + filepath.ToSlash(rel),
		}
		order = append(order, name)
		return nil
	})
	if err != nil {
		w.ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "cannot scan the project for modules",
			Detail:   err.Error(),
			Action:   "Check the project directory is readable.",
		})
		return nil
	}

	// WalkDir visits lexically, so `order` is already deterministic and needs
	// no sort. The collision diagnostic above depends on that: which of two
	// colliding directories is called "prior" must not change between runs.
	out := make([]loadedModule, 0, len(order))
	for _, name := range order {
		out = append(out, byName[name])
	}
	return out
}

// moduleName normalises a directory name to an identifier: `-` becomes `_`.
// The `:ref` and `.git` trimming a source string needs is handled where sources
// are parsed; a directory name has neither.
func moduleName(base string) string {
	return strings.ReplaceAll(base, "-", "_")
}
