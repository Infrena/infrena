package modules

import (
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/infrata/infrata/internal/config"
	"github.com/infrata/infrata/internal/diag"
)

// skipDirs are never descended into. `.git` holds thousands of files and no
// modules; `.infra` holds state and the module cache, and descending into the
// cache would load every module a project has ever fetched under a name taken
// from a content hash.
var skipDirs = map[string]bool{".git": true, ".infra": true}

// discover finds every directory under the project root holding a module.yml
// and makes it available under its directory name (PLAN.md §11, §8e).
//
// ROOT ONLY. A module's own dependencies come from its own `modules:` list, not
// from whatever directories happen to lie around the project that vendored it —
// otherwise a module works in one project and not the next, which is the
// opposite of what a module is for. loadModules enforces this by calling
// discover only when the module path is empty.
//
// explicit is the set of names this level's `modules:` list already claims.
// Any directory whose derived name is in explicit is skipped entirely —
// neither registered nor checked for a collision against another directory of
// the same name — because the explicit entry always wins that name's slot
// regardless of what discovery would have found (§7, §8e: "an explicit
// `modules:` entry beats a discovered one of the same name" is the one case a
// name collision is not an error). Without this, a vendored module directory
// that happens to share a basename with an unrelated top-level module
// directory would trip the duplicate-name check below even though neither
// discovered candidate is ever used.
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
		// Symlinks are not followed: WalkDir does not follow them, and that is
		// the behaviour wanted here rather than an accident to work around. A
		// symlink out of the project root would load a module from outside the
		// repository, which is a source the user never wrote down.
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
			// A display string, not a source.Source: a discovered module was
			// never written down, so there is no source to parse and nothing
			// for Resolve to do. loadedModule.Source is what diagnostics print,
			// which is why it is a string here and at the explicit sites alike.
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
	// no sort — the collision diagnostic above depends on that: which of two
	// colliding directories is called "prior" must not change between runs.
	out := make([]loadedModule, 0, len(order))
	for _, name := range order {
		out = append(out, byName[name])
	}
	return out
}

// moduleName normalises a directory name to an identifier (§8d): `-` becomes
// `_`. The `:ref` and `.git` trimming §8d also describes belongs to a SOURCE
// string, which stage 2 handles; a directory name has neither.
func moduleName(base string) string {
	return strings.ReplaceAll(base, "-", "_")
}
