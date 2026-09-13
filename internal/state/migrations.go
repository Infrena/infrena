package state

import (
	"fmt"
	"strings"
)

// The registered migrations, newest step last. PLAN.md §21.
//
// THIS IS THE MECHANISM'S FIRST USE. Migration, RegisterMigration and Decode's chain
// have existed since M1 with nothing registered, which is exactly why §21.1 argued for
// exercising them on the fake provider's rename — a state file describing imaginary
// infrastructure — rather than first on something a user cannot recreate.
func init() { RegisterMigration(fakeProviderRename) }

// oldFakePlugin and newFakePlugin are the fake provider's names either side of its move
// out of this repository (PLAN.md §31.1, §31.2).
const (
	oldFakePlugin = "test"
	newFakePlugin = "fake"
)

// fakeProviderRename is version 1 → 2: the fake provider became a separately distributed
// plugin called `fake`, so the types it serves moved from `test.*` to `fake.*`.
//
// Without this, every project that ever used the fake provider stops loading the moment
// the builtin is deleted: state records `fake.network`, no installed plugin serves that
// type, and the resource cannot be read, planned or destroyed. The user's real
// infrastructure is fine and their state file is the only thing standing between them and
// it, which is the worst position to leave someone in.
var fakeProviderRename = Migration{
	From:  1,
	To:    2,
	Apply: renameFakeProvider,
}

// renameFakeProvider rewrites the type prefix on every resource, and the recorded
// instance name only where it is the IMPLICIT one.
//
// The asymmetry is the whole care in this function. A type belongs to the plugin, so every
// `test.*` becomes `fake.*` unconditionally. An INSTANCE NAME belongs to the user: it is
// `test` only when they declared no `providers:` block and the implicit instance took the
// plugin's name. Someone who wrote `providers: [{plugin: test, name: main}]` has state
// recording `main`, and after the rename their configuration says `plugin: fake` while the
// instance is still `main` — rewriting every provider name to `fake` would break exactly
// those projects, silently, by pointing their resources at an instance that does not exist.
//
// NUMBERS ARRIVE AS json.Number here and nothing in this function touches one, which is
// how it avoids the rounding that made this whole mechanism unusable until Decode started
// using UseNumber. Rewriting only strings is not a coincidence; it is the cheapest
// possible migration and worth noticing before writing a more ambitious one.
func renameFakeProvider(raw map[string]any) error {
	resources, ok := raw["resources"]
	if !ok || resources == nil {
		// A state file with no resources at all — an environment applied and then
		// emptied. Nothing to rewrite, and not an error.
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
