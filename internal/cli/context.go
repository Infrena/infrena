package cli

import (
	"path/filepath"
	"sort"

	"github.com/infrata/infrata/internal/config"
	"github.com/infrata/infrata/internal/registry"
	"github.com/infrata/infrata/internal/state"
	"github.com/infrata/infrata/pkg/value"
	"github.com/infrata/infrata/providers/test"
)

// StateDirName is the per-project directory holding state, locks and, for the
// test provider, the fake cloud.
const StateDirName = ".infra"

// ImplicitInstance is the instance name a project with no `providers:` block gets.
//
// Every project written before §12.1 existed has no such block, and every one of
// them must keep working — so the absence of the block means one implicit instance
// named after the only plugin there is. That also makes a state file written before
// instances existed already correct: it records "test", which IS this instance.
const ImplicitInstance = "test"

// buildRegistry constructs the provider registry for a project directory, one
// provider per declared instance (PLAN.md §12.1).
//
// It reads `providers:` itself rather than taking a resolved table, and that is a
// KNOWN LIMITATION rather than a preference: constructing a provider needs its
// configuration, resolved configuration needs variables, and variables need a
// compile — which needs the registry for its schemas. The cycle is real. Today only
// LITERAL configuration reaches construction, so `cloud: ${path}` resolves (see
// internal/providers) but does not yet choose the file a provider opens.
//
// Breaking it properly means separating a plugin's SCHEMAS, which need no
// configuration, from its provider OBJECT, which does — a factory in the registry
// rather than a constructed provider. That is the next task's work and is recorded
// in the M11 plan.
func buildRegistry(dir string) *registry.Registry {
	reg := registry.New()
	for _, inst := range declaredInstances(dir) {
		if err := reg.Register(inst.name, test.New(inst.cloud)); err != nil {
			// A malformed built-in schema is a programming error. A DUPLICATE
			// instance name is not — config.decodeProviders reports that — so this
			// panic is unreachable through configuration.
			panic("registering provider instance " + inst.name + ": " + err.Error())
		}
	}
	return reg
}

// defaultInstance names the instance a resource with no `provider:` belongs to.
//
// From the same declarations buildRegistry reads, and there is one function doing
// the reading precisely so the two cannot come to disagree: a compiler that
// defaulted to one instance while the registry offered another would create a
// resource in one account and fail to find it in the other.
func defaultInstance(dir string) string {
	specs := declaredInstances(dir)
	for _, s := range specs {
		if s.isDefault {
			return s.name
		}
	}
	if len(specs) > 0 {
		return specs[0].name
	}
	return ImplicitInstance
}

type instanceSpec struct {
	name      string
	cloud     string
	isDefault bool
}

// declaredInstances reads `providers:` for the instances to construct, falling back
// to one implicit instance when the project declares none.
//
// Decoding errors are IGNORED here and reported by the caller's own compile. This
// function runs before any diagnostic can be rendered, and reporting the same
// malformed file twice — once as "cannot build the registry" and once properly —
// gives a reader two problems to reconcile instead of one.
func declaredInstances(dir string) []instanceSpec {
	defaultCloud := filepath.Join(dir, test.DefaultCloudPath)

	files, err := config.Load(dir)
	if err != nil {
		return []instanceSpec{{name: ImplicitInstance, cloud: defaultCloud, isDefault: true}}
	}
	decl, ds := config.Decode(files)
	if ds.HasErrors() || len(decl.Providers) == 0 {
		return []instanceSpec{{name: ImplicitInstance, cloud: defaultCloud, isDefault: true}}
	}

	out := make([]instanceSpec, 0, len(decl.Providers))
	for _, d := range decl.Providers {
		spec := instanceSpec{name: d.Name, cloud: defaultCloud, isDefault: d.Default}
		// `cloud:` is the fake provider's own configuration — where its JSON
		// "cloud" lives. Two instances pointing at one file would be two names for
		// one account, which is the opposite of what §12.1 is for, so each
		// instance gets its own by default.
		if c, ok := d.Config["cloud"]; ok {
			if text, ok := c.Value.AsString(); ok && text != "" {
				spec.cloud = filepath.Join(dir, text)
			}
		} else if d.Name != ImplicitInstance {
			spec.cloud = filepath.Join(dir, StateDirName, "fake-cloud-"+d.Name+".json")
		}
		out = append(out, spec)
	}
	return out
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
