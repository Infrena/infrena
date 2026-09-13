package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/infrata/infrata/internal/config"
	"github.com/infrata/infrata/internal/diag"
	"github.com/infrata/infrata/internal/pluginhost"
	"github.com/infrata/infrata/internal/providers"
	"github.com/infrata/infrata/internal/registry"
	"github.com/infrata/infrata/internal/state"
	"github.com/infrata/infrata/internal/variables"
	"github.com/infrata/infrata/pkg/provider"
	"github.com/infrata/infrata/pkg/value"
	"github.com/infrata/infrata/providers/test"
)

// StateDirName is the per-project directory holding state, locks and, for the
// test provider, the fake cloud.
const StateDirName = ".infra"

// buildRegistry constructs the provider registry for a project directory.
//
// IT NAMES NO PLUGINS. Which providers a project uses is a fact about that
// project's configuration — a `providers:` entry saying `plugin: aws`, or a
// resource of type `aws.instance` — so the registry is given a LOADER and the
// plugins follow from what the configuration turns out to say. Compiler stage 4.5
// is where that happens.
//
// The returned close function shuts down every plugin that was started. Callers
// must defer it: without it a command leaves child processes behind.
func buildRegistry(opts *GlobalOptions) (*registry.Registry, func()) {
	reg, loader := buildRegistryWithLoader(opts)
	return reg, loader.Close
}

// buildRegistryWithLoader is buildRegistry for the one caller that needs the loader
// itself: `discover` asks what plugins are available, because its scope is not set by
// configuration.
func buildRegistryWithLoader(opts *GlobalOptions) (*registry.Registry, *pluginhost.Loader) {
	loader := &pluginhost.Loader{
		Search:  pluginhost.DefaultSearch(opts.Dir, opts.PluginDirs),
		Dir:     opts.Dir,
		Verbose: verboseWriter(opts),
		Builtin: builtinPlugins(opts.Dir),
	}
	reg := registry.New()
	reg.SetLoader(loader)
	return reg, loader
}

// builtinPlugins are the plugins served in process because no binary exists yet.
//
// TRANSITIONAL, and the only entry is the fake provider, which is being moved to
// its own repository as infrata-plugin-fake. A builtin is not a second code path:
// pluginhost.InProcess runs the SDK over an in-memory pipe, so it goes through the
// same handshake, the same protocol and the same trust rules a subprocess does.
// A real binary on the search path wins over this, so the cutover is a matter of
// installing one.
//
// Delete this function, and pluginhost.Loader.Builtin, once that binary ships.
func builtinPlugins(dir string) map[string]provider.Plugin {
	return map[string]provider.Plugin{"test": test.NewPlugin(dir)}
}

// verboseWriter is where plugin logs go, or nil when --verbose is off.
func verboseWriter(opts *GlobalOptions) io.Writer {
	if !opts.Verbose {
		return nil
	}
	return os.Stderr
}

// stateOnlyRegistry is buildRegistry plus the instances a command that never
// compiles still has to dispatch to.
//
// The returned close function shuts down every plugin started; callers must defer it.
func stateOnlyRegistry(opts *GlobalOptions) (*registry.Registry, providers.Table, diag.Diagnostics, func()) {
	reg, closePlugins := buildRegistry(opts)
	table, ds := registerStateInstances(reg, opts.Dir)
	return reg, table, ds, closePlugins
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
	// The plugins this project uses, from the same rule compilation applies. A
	// state-only command loads them here because nothing else will: it never reaches
	// stage 4.5.
	for _, name := range decl.NeededPlugins() {
		if err := reg.EnsurePlugin(context.Background(), name); err != nil {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "the " + name + " plugin could not be loaded",
				Detail:   err.Error(),
				Action: "Install the plugin, or correct the `plugin:` name. `--verbose` reports " +
					"where each plugin was loaded from.",
			})
		}
	}
	if ds.HasErrors() {
		return nil, ds
	}

	if len(decl.Providers) == 0 {
		// Resources but no `providers:` block: the plugins are loaded, so the
		// implicit instance can be derived the same way a compile derives it.
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

// testRegistryFor builds a registry the way a command does, for tests that need one
// without running a command.
//
// It exists so a test cannot accidentally build a registry a different way from the
// commands it is testing — the difference that would hide is "which plugins does
// this project actually load", which is the whole subject.
func testRegistryFor(dir string) (*registry.Registry, func()) {
	return buildRegistry(&GlobalOptions{Dir: dir})
}

// ensureStateProviders loads the plugins the RECORDED resources need, and registers
// the implicit instance if the project declares none.
//
// Configuration cannot answer this for a state-only command. `destroy`'s whole
// premise is that nothing is configured — `resources: {}`, or no file at all — so
// the only thing that says which plugins are involved is the state: a resource
// recorded as `test.network` can only be served by the plugin `test`, because a
// plugin serves `<name>.*` and nothing else (PLAN.md §31.1).
//
// Called AFTER state is read, which is why it is separate from
// registerStateInstances: at the time that runs, state has not been opened yet.
func ensureStateProviders(reg *registry.Registry, st *state.State) diag.Diagnostics {
	var ds diag.Diagnostics
	if st == nil {
		return ds
	}

	seen := map[string]bool{}
	var names []string
	for _, addr := range st.Addresses() {
		rs, ok := st.Get(addr)
		if !ok {
			continue
		}
		prefix, _, found := strings.Cut(rs.Type, ".")
		if !found || prefix == "" || seen[prefix] {
			continue
		}
		seen[prefix] = true
		names = append(names, prefix)
	}
	sort.Strings(names)

	for _, name := range names {
		if err := reg.EnsurePlugin(context.Background(), name); err != nil {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "the " + name + " plugin could not be loaded",
				Detail: err.Error() + "\nIt is needed because state records resources of type " +
					name + ".*, and only that plugin can report on them.",
				Action: "Install the plugin. Until it is available, the resources it manages " +
					"cannot be read, changed or destroyed.",
			})
		}
	}
	if ds.HasErrors() {
		return ds
	}

	// Now that the plugins are here, the implicit instance can be derived. A project
	// that DOES declare `providers:` already had its instances registered from
	// configuration, and Register leaves those alone.
	if len(reg.InstanceNames()) == 0 {
		ds.Extend(providers.Register(providers.Implicit(reg), reg))
	}
	return ds
}

// loadConfiguredPlugins loads every plugin a project's configuration names.
//
// Shared by the commands that need SCHEMAS without compiling — `explain`, and the
// state-only commands' first pass. Decoding errors are ignored here on purpose: this
// runs before any diagnostic can be rendered properly, and reporting a malformed file
// twice gives a reader two problems to reconcile instead of one.
func loadConfiguredPlugins(reg *registry.Registry, dir string) diag.Diagnostics {
	var ds diag.Diagnostics
	files, err := config.Load(dir)
	if err != nil {
		return ds
	}
	decl, decodeDS := config.Decode(files)
	if decodeDS.HasErrors() {
		return ds
	}
	for _, name := range decl.NeededPlugins() {
		if err := reg.EnsurePlugin(context.Background(), name); err != nil {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "the " + name + " plugin could not be loaded",
				Detail:   err.Error(),
				Action: "Install the plugin, or correct the `plugin:` name. `--verbose` reports " +
					"where each plugin was loaded from.",
			})
		}
	}
	return ds
}

// discoveryRegistry builds the registry `discover` uses.
//
// Every OTHER command knows which plugins it needs because the project says so.
// Discovery asks what EXISTS — including resources no configuration mentions, which
// is the whole point — so its scope is every plugin available to ask, and a project's
// own `providers:` only decides how those are configured.
func discoveryRegistry(opts *GlobalOptions) (*registry.Registry, providers.Table, diag.Diagnostics, func()) {
	reg, loader := buildRegistryWithLoader(opts)

	var ds diag.Diagnostics
	for _, name := range loader.Available() {
		if err := reg.EnsurePlugin(context.Background(), name); err != nil {
			// A plugin that will not load is reported and skipped rather than
			// failing the command: discovery is a survey, and a partial answer the
			// user is TOLD is partial beats no answer at all.
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityWarning,
				Summary:  "the " + name + " plugin could not be loaded, so nothing it manages is listed",
				Detail:   err.Error(),
			})
		}
	}

	table, instanceDS := registerStateInstances(reg, opts.Dir)
	ds.Extend(instanceDS)
	if len(table) == 0 {
		// A project that configures no instances still has one per plugin: discovery
		// asks each about the account its own defaults point at.
		table = providers.Implicit(reg)
		ds.Extend(providers.Register(table, reg))
	}
	return reg, table, ds, loader.Close
}
