package cli

import (
	"errors"
	"path/filepath"
	"sort"

	"github.com/infrata/infrata/internal/config"
	"github.com/infrata/infrata/internal/diag"
	"github.com/infrata/infrata/internal/providers"
	"github.com/infrata/infrata/internal/registry"
	"github.com/infrata/infrata/internal/state"
	"github.com/infrata/infrata/internal/variables"
	"github.com/infrata/infrata/pkg/value"
	"github.com/infrata/infrata/providers/test"
)

// StateDirName is the per-project directory holding state, locks and, for the
// test provider, the fake cloud.
const StateDirName = ".infra"

// buildRegistry constructs the provider registry for a project directory.
//
// SCHEMAS ONLY. No provider object exists yet, and cannot: an instance's
// configuration may interpolate a variable, resolving that variable needs a
// compile, and a compile needs these schemas. compiler.Compile supplies the other
// half — internal/providers.Prepare constructs each instance from the values it has
// by then resolved and registers it here (PLAN.md §12.1).
//
// So a command that compiles gets a registry that dispatches; a command that does
// not — and there are two, `destroy` and `refresh`, which work from state alone —
// must build its instances some other way. That is registerStateInstances.
func buildRegistry(dir string) *registry.Registry {
	reg := registry.New()
	if err := reg.RegisterPlugin(test.NewPlugin(dir)); err != nil {
		// A malformed built-in schema is a programming error, caught by the
		// provider's own tests long before here. Nothing a user writes reaches it:
		// this call reads no configuration at all.
		panic("registering the test plugin: " + err.Error())
	}
	return reg
}

// stateOnlyRegistry is buildRegistry plus the instances a command that never
// compiles still has to dispatch to.
func stateOnlyRegistry(dir string) (*registry.Registry, providers.Table, diag.Diagnostics) {
	reg := buildRegistry(dir)
	table, ds := registerStateInstances(reg, dir)
	return reg, table, ds
}

// registerStateInstances builds the provider instances a state-only command needs.
//
// `destroy` and `refresh` never compile: one synthesises an empty desired state and
// the other reads state and calls Provider.Read. Neither can therefore go through
// Prepare, and neither has a variable scope to resolve an instance's configuration
// against — so this reads `providers:` and takes only LITERAL configuration,
// reporting an instance whose configuration interpolates anything rather than
// guessing at it.
//
// The asymmetry is real and is the honest shape of the problem: a command that
// works from state alone has no environment, and an instance configured per
// environment has no single answer for it. What saves it is that the instance NAME
// is recorded in state, so the resource still reaches the right account whenever
// that account's configuration does not itself depend on an environment.
func registerStateInstances(reg *registry.Registry, dir string) (providers.Table, diag.Diagnostics) {
	var ds diag.Diagnostics

	files, err := config.Load(dir)
	if err != nil {
		// No readable configuration at all. `destroy` is explicitly allowed to run
		// against a project whose files are gone (that is half of what it is for),
		// so this is not an error — it leaves the implicit instance below.
		return registerImplicit(reg, ds)
	}
	decl, decodeDS := config.Decode(files)
	if decodeDS.HasErrors() {
		// Reported by nothing here on purpose: a state-only command is not the
		// place a user learns their configuration is malformed, and saying so
		// twice — once as "cannot build the registry", once properly on the next
		// `validate` — gives a reader two problems to reconcile instead of one.
		return registerImplicit(reg, ds)
	}
	if len(decl.Providers) == 0 {
		return registerImplicit(reg, ds)
	}

	table, resolveDS := providers.Resolve(decl.Providers, literalOnlyScope())
	ds.Extend(resolveDS)
	if resolveDS.HasErrors() {
		return nil, ds
	}
	ds.Extend(providers.Register(table, reg))
	return table, ds
}

// registerImplicit registers the single implicit instance of a project that
// declares no `providers:` block.
func registerImplicit(reg *registry.Registry, ds diag.Diagnostics) (providers.Table, diag.Diagnostics) {
	table := providers.Implicit(reg)
	ds.Extend(providers.Register(table, reg))
	return table, ds
}

// literalOnlyScope is an empty variable scope.
//
// An instance configured with `${...}` is therefore reported as an undefined
// variable rather than silently resolved to something. That diagnostic is the point
// — see registerStateInstances.
func literalOnlyScope() variables.Scope { return variables.Scope{} }

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

// errProviderInstances ends a command whose provider instances could not be built.
//
// The diagnostics have already been rendered, so this carries no detail of its own:
// a second telling of the same problem, in a different shape, is what makes a reader
// go looking for two.
var errProviderInstances = errors.New("provider instances could not be configured")

// defaultsByInstance projects a provider table down to just the `defaults:` blocks,
// keyed by instance name — what internal/generator needs and no more.
//
// A whole providers.Table in generator.Options would put every instance's credentials
// within reach of a package whose job is writing files a user reads.
func defaultsByInstance(table providers.Table) map[string]map[string]value.Value {
	if len(table) == 0 {
		return nil
	}
	out := make(map[string]map[string]value.Value, len(table))
	for name, inst := range table {
		if len(inst.Defaults) > 0 {
			out[name] = inst.Defaults
		}
	}
	return out
}
