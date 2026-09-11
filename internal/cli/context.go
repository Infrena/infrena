package cli

import (
	"path/filepath"
	"sort"

	"github.com/infrata/infrata/internal/registry"
	"github.com/infrata/infrata/internal/state"
	"github.com/infrata/infrata/pkg/value"
	"github.com/infrata/infrata/providers/test"
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
//
// Redundancy note (measured): removing this sort fails nothing, and unlike
// most of its siblings it has no upstream protector at all — it is the ONLY
// thing making `infra state show`'s attribute order stable. What hides it is
// fixture size: the tests that exercise state show print resources with too
// few attributes for a randomised map order to differ from a sorted one. The
// property is the same one TestRenderIsDeterministicAcrossRepeatedCalls
// pins for plan output; this is its `state show` counterpart, and it is
// untested rather than redundant.
func sortedAttributeKeys(attrs map[string]value.Value) []string {
	names := make([]string, 0, len(attrs))
	for name := range attrs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
