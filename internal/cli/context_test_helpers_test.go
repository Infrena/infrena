package cli

import (
	"testing"

	"github.com/infrena/infrena/internal/registry"
)

// mustRegistry is testRegistryFor with the plugin shutdown registered as cleanup, so
// a test cannot leak a plugin process by forgetting to close one.
func mustRegistry(t *testing.T, dir string) *registry.Registry {
	t.Helper()
	reg, closePlugins := testRegistryFor(dir)
	t.Cleanup(closePlugins)
	return reg
}
