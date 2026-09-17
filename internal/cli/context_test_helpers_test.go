package cli

import (
	"path/filepath"
	"testing"

	"github.com/infrena/infrena/internal/registry"
	"github.com/infrena/infrena/internal/state"
)

// mustRegistry is testRegistryFor with the plugin shutdown registered as cleanup, so
// a test cannot leak a plugin process by forgetting to close one.
func mustRegistry(t *testing.T, dir string) *registry.Registry {
	t.Helper()
	reg, closePlugins := testRegistryFor(dir)
	t.Cleanup(closePlugins)
	return reg
}

// localBackend is the local state backend for a project directory, for the
// tests that seed state or read it back.
//
// NOT backendFor, deliberately. These cases are about what a command wrote to
// state, not about which backend a project routes to; going through backendFor
// would make every one of them decode configuration and handle a closer to
// answer a question none of them is asking. internal/cli/backend_test.go is
// where the routing itself is pinned.
func localBackend(dir string) *state.Local {
	return state.NewLocal(filepath.Join(dir, StateDirName))
}
