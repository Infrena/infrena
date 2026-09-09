package cli

import (
	"path/filepath"
	"sort"

	"infra/internal/registry"
	"infra/internal/state"
	"infra/pkg/value"
	"infra/providers/test"
)

// StateDirName is the per-project directory holding state, locks and, for the
// test provider, the fake cloud.
const StateDirName = ".infra"

// buildRegistry constructs the provider registry for a project directory.
// M1 registers only the test provider; the AWS provider joins it in Phase 3.
func buildRegistry(dir string) *registry.Registry {
	reg := registry.New()
	cloudPath := filepath.Join(dir, test.DefaultCloudPath)
	if err := reg.Register(test.New(cloudPath)); err != nil {
		// A malformed built-in schema is a programming error, not user error.
		panic("registering the test provider: " + err.Error())
	}
	return reg
}

// backendFor constructs the state backend for a project directory.
func backendFor(dir string) *state.Local {
	return state.NewLocal(filepath.Join(dir, StateDirName))
}

// sortedAttributeKeys lists an attribute map's keys in sorted order, so
// output is deterministic across runs.
func sortedAttributeKeys(attrs map[string]value.Value) []string {
	names := make([]string, 0, len(attrs))
	for name := range attrs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
