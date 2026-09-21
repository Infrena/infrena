package state

import (
	"fmt"
	"strings"
)

// The registered migrations, newest step last.
func init() { RegisterMigration(fakeProviderRename) }

// oldFakePlugin and newFakePlugin are the fake provider's names either side of
// its move out of this repository.
const (
	oldFakePlugin = "test"
	newFakePlugin = "fake"
)

// fakeProviderRename is version 1 to 2: the fake provider became a separately
// distributed plugin called `fake`, so the types it serves moved from `test.*`
// to `fake.*`.
//
// Without this, a project that used the fake provider stops loading the moment
// the builtin is deleted: state records `fake.network`, no installed plugin
// serves that type, and the resource cannot be read, planned or destroyed.
var fakeProviderRename = Migration{
	From:  1,
	To:    2,
	Apply: renameFakeProvider,
}

// renameFakeProvider rewrites the type prefix on every resource, and the
// recorded instance name only where it is the implicit one.
//
// The asymmetry is the whole care in this function. A type belongs to the
// plugin, so every `test.*` becomes `fake.*` unconditionally. An instance name
// belongs to the user: it is `test` only when they declared no `providers:`
// block and the implicit instance took the plugin's name. Someone who wrote
// `providers: [{plugin: test, name: main}]` has state recording `main`, and
// rewriting every provider name to `fake` would silently point their resources
// at an instance that does not exist.
//
// Numbers arrive as json.Number and nothing here touches one, which is how it
// avoids rounding them. Rewriting only strings is the cheapest possible
// migration, and worth noticing before writing a more ambitious one.
func renameFakeProvider(raw map[string]any) error {
	resources, ok := raw["resources"]
	if !ok || resources == nil {
		// An environment applied and then emptied. Nothing to rewrite, and
		// not an error.
		return nil
	}
	entries, ok := resources.(map[string]any)
	if !ok {
		return fmt.Errorf("`resources` is %T, not an object", resources)
	}

	oldPrefix := oldFakePlugin + "."
	for name, entry := range entries {
		r, ok := entry.(map[string]any)
		if !ok {
			return fmt.Errorf("resource %q is %T, not an object", name, entry)
		}
		if t, ok := r["type"].(string); ok && strings.HasPrefix(t, oldPrefix) {
			r["type"] = newFakePlugin + "." + strings.TrimPrefix(t, oldPrefix)
		}
		if p, ok := r["provider"].(string); ok && p == oldFakePlugin {
			r["provider"] = newFakePlugin
		}
	}
	return nil
}
