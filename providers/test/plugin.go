package test

import (
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/infrata/infrata/pkg/provider"
	"github.com/infrata/infrata/pkg/schema"
	"github.com/infrata/infrata/pkg/value"
)

// Plugin is the fake provider as a PLUGIN: its schemas, which need no
// configuration, and a constructor for one configured instance of it.
//
// The split is what lets `cloud: ${path}` work at all. A provider object needs its
// cloud file; the path may interpolate a variable; resolving that variable needs a
// compile; and a compile needs the schemas. Handing the schemas over separately
// breaks the cycle — see provider.Plugin.
type Plugin struct {
	// dir is the project directory. A `cloud:` path in configuration is relative
	// to the project, not to the process's working directory, which --chdir moves
	// out from under us.
	dir string
}

// NewPlugin returns the fake provider's plugin, resolving cloud paths inside dir.
func NewPlugin(dir string) *Plugin { return &Plugin{dir: dir} }

var _ provider.Plugin = (*Plugin)(nil)

// Name returns the plugin name, which is what `plugin:` names.
func (pl *Plugin) Name() string { return "test" }

// Definitions returns the resource types the fake provider offers. Identical for
// every instance, and available before any of them is configured.
func (pl *Plugin) Definitions() []*schema.ResourceDefinition { return definitions() }

// cloudKey is the fake provider's only configuration: which JSON file holds this
// instance's world.
const cloudKey = "cloud"

// New constructs one instance.
//
// `cloud:` is this provider's stand-in for an account: two instances pointing at
// one file would be two names for one account, which is the opposite of what
// instances are for. So an instance that names no file gets its OWN, named after
// itself — except the implicit instance, which keeps the historical path so a
// project that predates `providers:` finds the cloud it already has.
func (pl *Plugin) New(instance string, config map[string]value.Value) (provider.Provider, error) {
	if err := rejectUnknownKeys(config); err != nil {
		return nil, err
	}

	path := defaultCloudPath(instance)
	if v, declared := config[cloudKey]; declared {
		text, ok := v.AsString()
		if !ok {
			return nil, fmt.Errorf("`cloud` must be a path to a JSON file, got %s", v.Kind)
		}
		if text == "" {
			return nil, fmt.Errorf("`cloud` is empty: give the path to the JSON file holding "+
				"this instance's fake infrastructure, or omit the key to get %s", defaultCloudPath(instance))
		}
		path = text
	}
	return New(filepath.Join(pl.dir, path)), nil
}

// defaultCloudPath is where an instance's world lives when it names no file.
func defaultCloudPath(instance string) string {
	if instance == "" || instance == "test" {
		return DefaultCloudPath
	}
	return filepath.Join(filepath.Dir(DefaultCloudPath), "fake-cloud-"+instance+".json")
}

// rejectUnknownKeys refuses configuration this plugin does not understand.
//
// Fail closed. A misspelled `clowd:` that is quietly ignored means an instance
// silently sharing another's account — and the first sign of it is a plan
// proposing to destroy resources somebody else owns.
func rejectUnknownKeys(config map[string]value.Value) error {
	var unknown []string
	for k := range config {
		if k != cloudKey {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return fmt.Errorf("unknown configuration %s; the test provider accepts only `cloud`",
		strings.Join(quoteAll(unknown), ", "))
}

func quoteAll(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = strconv.Quote(n)
	}
	return out
}
