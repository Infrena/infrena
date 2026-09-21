package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/infrena/infrena/internal/backendhost"
	"github.com/infrena/infrena/internal/compiler"
	"github.com/infrena/infrena/internal/config"
	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/internal/environments"
	"github.com/infrena/infrena/internal/pluginhost"
	"github.com/infrena/infrena/internal/providers"
	"github.com/infrena/infrena/internal/registry"
	"github.com/infrena/infrena/internal/state"
	"github.com/infrena/infrena/pkg/provider"
	"github.com/infrena/infrena/pkg/semver"
	"github.com/infrena/infrena/pkg/value"
	"github.com/infrena/infrena/providers/test"
)

// StateDirName is the per-project directory holding state, locks and, for the
// test provider, the fake cloud.
const StateDirName = ".infrena"

// buildRegistry constructs the provider registry for a project directory.
//
// It names no plugins. Which providers a project uses is a fact about that
// project's configuration, so the registry is given a loader and the compile
// loads whatever the configuration turns out to ask for.
//
// The returned close function shuts down every plugin that was started. Callers
// must defer it, or a command leaves child processes behind.
func buildRegistry(opts *GlobalOptions) (*registry.Registry, func()) {
	reg, loader := buildRegistryWithLoader(opts)
	return reg, loader.Close
}

// buildRegistryWithLoader is buildRegistry for the callers that need the loader
// itself: `discover` asks it what plugins are available, because its scope is not
// set by configuration.
func buildRegistryWithLoader(opts *GlobalOptions) (*registry.Registry, *pluginhost.Loader) {
	loader := &pluginhost.Loader{
		Search:  pluginhost.DefaultSearch(opts.Dir, opts.PluginDirs),
		Dir:     opts.Dir,
		Verbose: verboseWriter(opts),
		Builtin: builtinsFor(opts.Dir),
		// Read here rather than passed in: the loader must have them before the
		// first load, and a load can start from several places. This names no
		// plugin, only which versions are acceptable if one is asked for.
		Constraints: pluginConstraints(opts.Dir),
	}
	reg := registry.New()
	reg.SetLoader(loader)
	return reg, loader
}

// builtinsFor supplies plugins served in process rather than as a binary.
//
// Empty in a shipped build: a provider is a separate binary, including the official
// ones, so a project using the fake provider installs infrena-plugin-fake like any
// other.
//
// It is a variable because it is the one seam the in-process test suites use — they
// run commands in this process and register the fake provider here, over the same
// handshake, protocol and trust rules a subprocess gets, and a discovery test injects
// two plugins to reach a shape a shipped build no longer has. tests/integration
// deliberately does not use it: it shells out to the built binary and builds the real
// infrena-plugin-fake, so the path a user actually runs is proved somewhere.
var builtinsFor = func(string) map[string]provider.Plugin { return nil }

// fakeDouble is the in-process fake provider, for suites that need a provider without a
// binary. infrena-plugin-fake was ported from it; what keeps the two honest is that
// tests/integration runs the real one.
func fakeDouble(dir string) map[string]provider.Plugin {
	return map[string]provider.Plugin{"fake": test.NewPlugin(dir)}
}

// pluginConstraints reads the project's `plugins:` block.
//
// Decoding errors are ignored, the same concession registerStateInstances makes: this
// runs before any diagnostic can be rendered properly, and the compile that follows
// reports a malformed file once, in the shape a reader can act on.
func pluginConstraints(dir string) map[string]semver.Constraint {
	files, err := config.Load(dir)
	if err != nil {
		return nil
	}
	decl, ds := config.Decode(files)
	if ds.HasErrors() || len(decl.Plugins) == 0 {
		return nil
	}
	out := make(map[string]semver.Constraint, len(decl.Plugins))
	for name, c := range decl.Plugins {
		out[name] = c.Constraint
	}
	return out
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
func stateOnlyRegistry(
	opts *GlobalOptions, environment string,
) (*registry.Registry, providers.Table, diag.Diagnostics, func()) {
	reg, closePlugins := buildRegistry(opts)
	// usesDefaults false: `destroy` and `refresh` never compile, and an
	// instance's `defaults:` are read only by the compiler's binding and by
	// import's generator. Neither runs here.
	table, ds := registerStateInstances(reg, opts, environment, false)
	return reg, table, ds, closePlugins
}

// registerStateInstances builds the provider instances a state-only command needs.
//
// `destroy` and `refresh` never compile — one synthesises an empty desired state,
// the other reads state and calls Provider.Read — so neither has a variable scope to
// resolve an instance's configuration against. This reads `providers:` and takes
// only literal configuration, reporting an instance whose configuration interpolates
// rather than guessing at it.
//
// A command that works from state alone has no environment, and an instance
// configured per environment has no single answer for it. What saves it is that the
// instance name is recorded in state, so a resource still reaches the right account
// whenever that account's configuration does not itself depend on an environment.
func registerStateInstances(
	reg *registry.Registry, opts *GlobalOptions, environment string, usesDefaults bool,
) (providers.Table, diag.Diagnostics) {
	var ds diag.Diagnostics

	dir := opts.Dir
	files, err := config.Load(dir)
	if err != nil {
		// No readable configuration at all. `destroy` is explicitly allowed to run
		// against a project whose files are gone (that is half of what it is for),
		// so this is not an error — it leaves the implicit instance below.
		return registerImplicit(reg, ds)
	}
	decl, decodeDS := config.Decode(files)
	if decodeDS.HasErrors() {
		// Not reported here on purpose: a state-only command is not the place a
		// user learns their configuration is malformed, and the next `validate`
		// says so properly.
		return registerImplicit(reg, ds)
	}
	// The plugins this project uses, by the same rule compilation applies. A
	// state-only command loads them here because nothing else will.
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

	// The variable scope alone: no modules and no resources, because resolving
	// variables needs neither, which is what makes it available to a command that
	// never compiles.
	copts, optDS := compilerOptions(opts, environment)
	ds.Extend(optDS)
	if optDS.HasErrors() {
		return nil, ds
	}
	stage, stageDS := compiler.VariableScope(files, copts)
	ds.Extend(stageDS)
	if stageDS.HasErrors() {
		return nil, ds
	}

	table, resolveDS := providers.Resolve(decl.Providers, stage.Scope)
	ds.Extend(resolveDS)
	if resolveDS.HasErrors() {
		return nil, ds
	}
	// Without a selected environment a per-environment variable stays unknown
	// rather than erroring — see compiler.VariableScope. An unknown must not reach
	// a plugin's Configure: it would be treated as absent, and the command would
	// survey or mutate whichever account the plugin defaults to.
	ds.Extend(refuseUnresolvedInstances(table, environment, usesDefaults))
	if ds.HasErrors() {
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

// refuseUnresolvedInstances reports an instance whose configuration still holds an
// unknown after resolution.
//
// It can only happen without a selected environment: `discover`, which takes none,
// and a teardown of an environment configuration no longer declares. The value is
// genuinely unknowable there, and refusing beats guessing — the alternative is an
// account chosen by a plugin default while the output says nothing about it.
//
// usesDefaults says whether this command will read `defaults:` at all, and only a
// command that will is allowed to fail over one. `defaults:` never reaches a plugin:
// the compiler reads it when binding lifecycle rules and import's generator when
// writing configuration, so checking it in `discover` refused runs over a value that
// command would never have looked at. An instance's own configuration is always
// checked, because it does cross to the plugin's Configure.
func refuseUnresolvedInstances(table providers.Table, environment string, usesDefaults bool) diag.Diagnostics {
	var ds diag.Diagnostics

	for _, name := range table.Names() {
		inst := table[name]
		parts := []struct {
			what   string
			values map[string]value.Value
		}{
			{"configuration", inst.Config},
		}
		if usesDefaults {
			parts = append(parts, struct {
				what   string
				values map[string]value.Value
			}{"`defaults`", inst.Defaults})
		}
		for _, part := range parts {
			for _, key := range sortedAttributeKeys(part.values) {
				if part.values[key].Known {
					continue
				}
				// The empty case covers two commands and has to describe both:
				// `discover`, which takes no environment, and a teardown of an
				// environment configuration no longer declares.
				detail := "No environment was resolved, so a value that differs per environment " +
					"cannot be determined — either this command takes no environment, or the " +
					"one named is no longer declared in configuration."
				if environment != "" {
					detail = "Its value is not set for environment " + strconv.Quote(environment) + "."
				}
				ds.Add(diag.Diagnostic{
					Severity: diag.SeverityError,
					Summary: "provider " + strconv.Quote(name) + "'s " + part.what + " key " +
						strconv.Quote(key) + " could not be resolved",
					Detail: detail + " Running anyway would use whatever the plugin defaults to, " +
						"which may be a different account than the one you mean.",
					Action: "Pass the value with --var, or use a literal here.",
					Origin: inst.Origin,
				})
			}
		}
	}
	return ds
}

// environmentProtections resolves an environment's protections for the commands
// that never compile.
//
// `destroy` is the reason it exists. Its premise is that nothing is configured, so
// it never reaches the compiler and would otherwise carry no protections at all —
// backwards, for the command that most needs to know an environment refuses to be
// destroyed. Resolution goes through environments.Resolve, the same call the
// compiler makes, so a protection means one thing regardless of which command asked.
//
// No configuration at all yields no protections. A protection is something an
// environment declares, and `destroy` is explicitly allowed to run against a project
// whose configuration was deleted, so refusing there would make a wiped project
// permanently undestroyable.
func environmentProtections(opts *GlobalOptions, environment string) compiler.Protections {
	files, err := config.Load(opts.Dir)
	if err != nil {
		return compiler.Protections{}
	}
	decl, decodeDS := config.Decode(files)
	if decodeDS.HasErrors() {
		// Reported by the caller's own decode. A protection read out of a
		// document that did not parse is not one to act on.
		return compiler.Protections{}
	}
	chain, chainDS := environments.Resolve(decl.Environments, environment)
	if chainDS.HasErrors() {
		return compiler.Protections{}
	}
	return compiler.Protections{
		RequireApproval:     chain.RequireApproval,
		RequireApprovalFrom: chain.RequireApprovalFrom,
		PreventDestroy:      chain.PreventDestroy,
		PreventDestroyFrom:  chain.PreventDestroyFrom,
	}
}

// backendFor opens the state backend a project asks for, and the closer that
// releases it.
//
// The closer is never nil, so every call site can defer it without first asking
// which backend it got. For local it does nothing; for a plugin it shuts a child
// process down, and a process left behind after a failed apply is a process holding
// a lock.
//
// Decoded, never compiled: state is read before anything is compiled, and `destroy`,
// `refresh`, `discover` and `import` never compile at all. That is also why
// `backend:` may not interpolate — there is no point at which a value there could be
// filled in.
//
// A migration that has not happened yet stops the command here; see
// refuseWhileMigrationPending.
func backendFor(ctx context.Context, opts *GlobalOptions) (state.Backend, func() error, error) {
	destinationDecl, sourceDecl := backendDecls(opts.Dir)
	destination, closeDestination, err := openBackend(ctx, opts, destinationDecl)
	if err != nil {
		return nil, nil, err
	}
	if err := refuseWhileMigrationPending(ctx, opts, destination, destinationDecl, sourceDecl); err != nil {
		// Closed here rather than left to the caller, which never got the
		// backend and so has nothing to defer. A plugin process left running
		// behind a refusal is a process holding whatever it locked.
		_ = closeDestination()
		return nil, nil, err
	}
	return destination, closeDestination, nil
}

// refuseWhileMigrationPending stops an ordinary command running against a
// backend that is still waiting for its state.
//
// A migration run from CI is necessarily two commits — one adding
// `migrate_from:`, one removing it — and between them the configuration names a
// new, empty backend beside the old one that holds everything. Reading `backend:`
// alone, every resource looks unmanaged, and an apply creates all of it again:
// duplicate infrastructure, and two backends each claiming to record the same
// resources.
//
// It is a guard and not an action. Moving state as a side effect of a `plan` would
// be a worse surprise than the one being prevented, so the only outcomes are "carry
// on" and "stop, and here is what to run".
//
// The destination is checked first, which the command has opened anyway. State there
// means the migration is done and the source is never touched — which matters,
// because opening it may start a plugin process and reach a network.
//
// An empty `Plugin` means no block and no guard, which is every project. A block that
// was present and would not decode also lands here with an empty plugin, and is left
// alone deliberately: `state migrate` reports that one properly.
func refuseWhileMigrationPending(
	ctx context.Context,
	opts *GlobalOptions,
	destination state.Backend,
	destinationDecl, sourceDecl config.BackendDecl,
) error {
	if sourceDecl.Plugin == "" {
		return nil
	}

	held, err := environmentsHoldingState(ctx, destination)
	if err != nil {
		// Not this guard's error to report. The command is about to use this
		// backend for its real work and will fail there with a message about
		// what it was actually doing, which is the one a reader can act on.
		return nil
	}
	if len(held) > 0 {
		return nil
	}

	source, closeSource, err := openBackend(ctx, opts, sourceDecl)
	if err != nil {
		return fmt.Errorf(
			"this project's `backend:` block names %s, which holds no state, and the backend "+
				"`migrate_from:` names could not be opened to find out whether a migration is pending\n"+
				"  %v\n"+
				"Running anyway could treat every resource as unmanaged and create it all a second time.\n"+
				"Make %s reachable and run `infrena state migrate`, or remove the `migrate_from:` block\n"+
				"if the migration is already done.",
			backendName(destinationDecl), err, backendName(sourceDecl))
	}
	defer closeSource()

	pending, err := environmentsHoldingState(ctx, source)
	if err != nil {
		return fmt.Errorf(
			"the backend `migrate_from:` names could not be read, so infrena cannot tell whether a "+
				"migration is pending\n"+
				"  %v\n"+
				"`backend:` names %s, which holds no state, so running anyway could treat every\n"+
				"resource as unmanaged and create it all a second time.\n"+
				"Run `infrena state migrate` once %s can be read, or remove the `migrate_from:` block\n"+
				"if the migration is already done.",
			err, backendName(destinationDecl), backendName(sourceDecl))
	}
	if len(pending) == 0 {
		// Both ends empty. A new project that happens to carry both blocks is
		// not a pending migration, and refusing here would block a legitimate
		// first apply.
		return nil
	}

	return fmt.Errorf(
		"this project is waiting for a state migration that has not been run\n"+
			"  `backend:` names %s, which holds no state\n"+
			"  `migrate_from:` names %s, which holds state for %s\n"+
			"Running now would find every resource unmanaged, and an apply would create all of it a\n"+
			"second time alongside what already exists.\n"+
			"Run `infrena state migrate` to copy the state across, or remove the `migrate_from:` block\n"+
			"if this project is not migrating after all.",
		backendName(destinationDecl), backendName(sourceDecl), joinEnvironments(pending))
}

// environmentsHoldingState names the environments a backend holds real state
// for, in sorted order.
//
// An environment whose state records no resources does not count. A backend keeps a
// file or an object per environment and one may be left behind by a `destroy`, and
// treating that as "the migration is done" would wave through exactly the run this
// guard exists to stop. It is the same emptiness test compareEnds makes.
func environmentsHoldingState(ctx context.Context, b state.Backend) ([]string, error) {
	environments, err := b.List(ctx)
	if err != nil {
		return nil, err
	}
	sort.Strings(environments)

	var held []string
	for _, environment := range environments {
		st, err := b.Get(ctx, environment)
		if err != nil {
			return nil, err
		}
		if st != nil && len(st.Resources) > 0 {
			held = append(held, environment)
		}
	}
	return held, nil
}

// openBackend opens the backend a decoded block asks for, and the closer that
// releases it.
//
// Separate from backendFor because a migration holds both ends open at once:
// `backend:` and `migrate_from:` are the same shape, decoded by the same code, and
// they have to be opened by the same code too. A migration that read its source by
// different rules from its destination could move state somewhere nobody configured.
//
// `plugin: local` resolves here, to the same in-process backend an absent block gets.
// Naming local has to be possible, because otherwise "migrate back to local" could
// only be said by leaving a block out, and leaving it out already means "no
// migration".
//
// An empty Plugin still means local. The one exception is a block that was present
// and did not decode: it records an origin and no plugin, and treating that as local
// would write state into the project directory for a project that asked for it to
// live somewhere else. That refusal names `backend:`, because that is the block whose
// failure moves state; a caller opening `migrate_from:` only does so once it has a
// plugin name.
func openBackend(
	ctx context.Context, opts *GlobalOptions, decl config.BackendDecl,
) (state.Backend, func() error, error) {
	if decl.Plugin == "" || decl.Plugin == localBackendName {
		// A block that did not decode is not an absent block: config records the
		// origin of one it could not read. A block naming `local` decoded fine, so
		// it never reaches this.
		if decl.Plugin == "" && decl.Origin.File != "" {
			return nil, nil, fmt.Errorf(
				"this project's `backend:` block could not be read, so infrena cannot tell where its state lives\n"+
					"  declared in: %s\n"+
					"Running anyway would write state to %s, which is not what the block asks for.\n"+
					"Run `infrena validate` to see what is wrong with it.",
				decl.Origin.File, filepath.Join(opts.Dir, StateDirName))
		}
		return state.NewLocal(filepath.Join(opts.Dir, StateDirName)), func() error { return nil }, nil
	}

	return backendhost.Open(ctx, decl.Plugin, opts.Dir,
		backendhost.Search(opts.Dir, opts.PluginDirs), decl.Config)
}

// localBackendName is the plugin name the built-in backend answers to. It
// starts no process and needs nothing installed, which is what makes it the one
// backend infrena can carry.
const localBackendName = "local"

// backendDecls reads both backend blocks out of a project's configuration in one
// decode: `backend:`, where state lives, and `migrate_from:`, where `state migrate`
// reads from.
//
// One decode rather than two, because decoding twice is two chances to read two
// different versions of a file somebody is editing. Decoding errors are ignored as
// elsewhere in this file; what is not ignored is either block's own failure to
// decode, because that changes where state is written — see backendFor, which has
// the origin to tell it apart from a project that declared nothing.
//
// An absent `migrate_from:` is the zero value. A block that was present and did not
// decode records an origin and no plugin, so a caller must test the origin rather
// than assume an empty Plugin means the block was never written.
func backendDecls(dir string) (backend, migrateFrom config.BackendDecl) {
	files, err := config.Load(dir)
	if err != nil {
		// No project here at all, which is local: `infrena state list` outside
		// a project has nothing to read a backend out of.
		return config.BackendDecl{}, config.BackendDecl{}
	}
	decl, _ := config.Decode(files)
	return decl.Backend, decl.MigrateFrom
}

// sortedAttributeKeys lists an attribute map's keys in sorted order, so output is
// deterministic across runs.
//
// It is the only thing holding `infrena state show`'s attribute order stable, and
// nothing pins it: the fixtures print resources with too few attributes for a
// randomised map order to differ from a sorted one.
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

// ensureStateProviders loads the plugins the recorded resources need, and registers
// the implicit instance if the project declares none.
//
// "Declares none" is read from configuration, never inferred from what the registry
// happens to hold yet. On the plan and apply paths the declared instances are built
// by the compile that runs after this, so asking the registry would register an
// implicit, config-less instance first, and providers.Register would then skip the
// real one as a name already taken — sending every provider call in the run to
// whatever account the plugin defaults to, with nothing in the output to say so.
//
// Only state triggers it: with nothing recorded, no plugin is loaded before the
// compile, Implicit can derive nothing, and the declared instance registers normally.
//
// files may be nil, and is from `destroy` and `refresh`. Both are state-only by
// premise and have already been through registerStateInstances. For them state is the
// only thing that says which plugins are involved, since a plugin serves `<name>.*`
// and nothing else.
//
// Called after state is read, which is why it is separate from
// registerStateInstances.
func ensureStateProviders(reg *registry.Registry, st *state.State, files []config.File) diag.Diagnostics {
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

	// A project that declares `providers:` gets its instances from that block, and
	// nothing may stand in for them: an implicit instance registered here would be
	// the one that wins, configuration and all. Decoding errors are ignored on the
	// same terms as pluginConstraints.
	if decl, _ := config.Decode(files); len(decl.Providers) > 0 {
		return ds
	}

	// Now that the plugins are here, the implicit instance can be derived.
	if len(reg.InstanceNames()) == 0 {
		ds.Extend(providers.Register(providers.Implicit(reg), reg))
	}
	return ds
}

// loadConfiguredPlugins loads every plugin a project's configuration names.
//
// Shared by the commands that need schemas without compiling — `explain`, and the
// state-only commands' first pass. Decoding errors are ignored on the same terms as
// pluginConstraints.
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
// Every other command knows which plugins it needs because the project says so.
// Discovery asks what exists — including resources no configuration mentions, which
// is the whole point — so its scope is every plugin available to ask, and a project's
// own `providers:` only decides how those are configured.
//
// The environment is the caller's, and the two callers differ: `import <env>` has one
// and must pass it, `discover` has none and passes "". An import that passed ""
// cannot resolve a per-environment provider value it was handed the environment for.
func discoveryRegistry(
	opts *GlobalOptions, environment string, usesDefaults bool,
) (*registry.Registry, providers.Table, diag.Diagnostics, func()) {
	reg, loader := buildRegistryWithLoader(opts)

	var ds diag.Diagnostics
	for _, name := range loader.Available() {
		if err := reg.EnsurePlugin(context.Background(), name); err != nil {
			// A plugin that will not load is reported and skipped rather than
			// failing the command: discovery is a survey, and a partial answer the
			// user is told is partial beats no answer at all.
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityWarning,
				Summary:  "the " + name + " plugin could not be loaded, so nothing it manages is listed",
				Detail:   err.Error(),
			})
		}
	}

	// `discover` passes "" here: it is the one command whose scope configuration does
	// not set, so everything a variable can supply without an environment is resolved
	// and anything that needs one is refused by name.
	table, instanceDS := registerStateInstances(reg, opts, environment, usesDefaults)
	ds.Extend(instanceDS)
	if len(table) == 0 {
		// One instance per plugin, which is EveryPlugin and deliberately not
		// Implicit. Implicit picks the single instance a resource that names none
		// belongs to, so it returns nothing when more than one plugin is available
		// rather than guess an account. That is right for binding and wrong here:
		// `discover` would register no instances at all and report "Nothing found."
		// at exit 0, a silent empty survey.
		table = providers.EveryPlugin(reg)
		ds.Extend(providers.Register(table, reg))
	}
	return reg, table, ds, loader.Close
}
