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
		Builtin: builtinsFor(opts.Dir),
		// Read here rather than passed in, because the loader must have them before
		// the first load and a load can happen from four different places. This does
		// NOT name any plugin — which is the thing buildRegistry deliberately does not
		// do; it only says which versions are acceptable if one is asked for.
		Constraints: pluginConstraints(opts.Dir),
	}
	reg := registry.New()
	reg.SetLoader(loader)
	return reg, loader
}

// builtinsFor supplies plugins served in process rather than as a binary.
//
// EMPTY IN A SHIPPED BUILD, since 2026-09-13: infrena carries no provider, and a project
// using the fake provider installs infrena-plugin-fake like any other. That is the whole
// point of §31.1 — a provider is a separate binary, including the official ones.
//
// It is a variable rather than a constant because it is the one seam the test suites use,
// for two different reasons:
//
//   - internal/cli's TestMain injects fakeDouble, because these tests run commands IN THIS
//     PROCESS, and §31.1's Testing section says the unit and fast suites register the fake
//     provider that way — over pluginhost.InProcess, which is the same handshake, protocol
//     and trust rules a subprocess gets.
//   - a discovery test injects TWO plugins, the shape that broke `discover`. A shipped
//     build now has none, so that shape is reachable no other way.
//
// tests/integration deliberately does NOT use this: it shells out to the built binary and
// builds the real infrena-plugin-fake, so the path a user actually runs is proved
// somewhere.
var builtinsFor = func(string) map[string]provider.Plugin { return nil }

// fakeDouble is the in-process fake provider, for suites that need a provider without a
// binary.
//
// Not a second implementation of anything shipped: it is the ENGINE'S TEST DOUBLE, and it
// shares an origin with infrena-plugin-fake only because that binary was ported from it.
// What keeps the two honest is that tests/integration runs the real one.
func fakeDouble(dir string) map[string]provider.Plugin {
	return map[string]provider.Plugin{"fake": test.NewPlugin(dir)}
}

// pluginConstraints reads the project's `plugins:` block (PLAN.md §31.1).
//
// Decoding errors are IGNORED, the same concession registerStateInstances makes: this
// runs before any diagnostic can be rendered properly, and reporting a malformed file
// twice gives a reader two problems to reconcile instead of one. A project whose
// configuration will not decode has a worse problem than an unchecked constraint, and
// the compile that follows reports it.
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

	// The variable scope, from stages 1-4 only. No compile, no modules, no
	// resources: resolving variables needs none of them, which is what makes this
	// available to a command that never compiles.
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
	// An UNSELECTED chain (no environment: `discover`, or a teardown of an
	// environment configuration no longer declares) leaves a per-environment
	// variable unknown rather than erroring — see compiler.VariableScope. An
	// unknown must not reach a plugin's Configure: it would be silently treated as
	// absent and the command would survey or mutate whichever account the plugin
	// defaults to, which is the one outcome nobody can see in the output.
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
// It can only happen without a selected environment, which is `discover` (it takes
// none) and a teardown of an environment configuration no longer declares. The value
// is genuinely unknowable there: it differs per environment and no environment was
// named. Refusing beats guessing, because the alternative is an account chosen by a
// plugin default while the output says nothing about it.
//
// The action is --var, and unlike the old message that promise is now kept: these
// commands accept it.
//
// usesDefaults SAYS WHETHER THIS COMMAND WILL READ `defaults:` AT ALL, and only a
// command that will is allowed to fail over one. An instance's configuration is a
// different matter and is always checked: it crosses to the plugin's Configure, so an
// unknown there picks an account silently.
//
// `defaults:` never reaches a plugin. It is read by the compiler when binding
// lifecycle rules and by import's generator when writing configuration, so `discover`
// — which does neither — was refusing to run over a value it would never have looked
// at. `defaults: {region: ${aws_region}}` with a per-environment value made
// `discover` impossible for no benefit at all, which is what this parameter fixes.
// Checking it everywhere was the safe-looking default, and safe-looking is how a
// check ends up guarding nothing while costing a command.
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
				// The empty case covers TWO commands and must describe both: `discover`,
				// which takes no environment, and a teardown of an environment
				// configuration no longer declares. "This command does not take one"
				// was written for the first and is false of the second — and was
				// briefly false of `import` too, which reached here with a hardcoded
				// empty environment while holding one on its command line.
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

// environmentProtections resolves §38's protections for an environment, for the
// commands that never compile.
//
// `destroy` is the reason it exists. It synthesises an EMPTY configuration —
// that is its premise, that nothing is configured — so it never reaches the
// compiler and would otherwise carry no protections at all, which is precisely
// backwards: the command that destroys everything in an environment is the one
// that most needs to know the environment refuses to be destroyed.
//
// Resolution goes through environments.Resolve, the same call the compiler
// makes, so a protection means one thing regardless of which command asked.
//
// No configuration at all yields no protections, and that is correct rather
// than lenient: a protection is something an environment DECLARES, and an
// environment whose files are gone has declared nothing. `destroy` is
// explicitly allowed to run against a project whose configuration was deleted
// (that is half of what it is for), so refusing there would make a wiped
// project permanently undestroyable.
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
// THE CLOSER IS NEVER NIL, so every call site can defer it without first asking
// which backend it got. For local it does nothing; for a plugin it shuts a
// child process down, and a process left behind after a failed apply is a
// process holding a lock.
//
// DECODED, NEVER COMPILED. State is read before anything is compiled — and
// `destroy`, `refresh`, `discover` and `import` never compile at all — so
// working out where state lives can only ever be a read of the file. That is
// also why `backend:` may not interpolate: there is no point at which a value
// there could be filled in.
// A MIGRATION THAT HAS NOT HAPPENED YET STOPS THE COMMAND HERE. See
// refuseWhileMigrationPending: between the configuration landing and somebody
// running `state migrate`, the backend this returns is EMPTY while the old one
// holds everything, and a command that proceeded would see every resource as
// unmanaged.
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
// THIS IS THE DANGEROUS HALF OF THE MIGRATION DESIGN (spec §7). A migration
// run from CI is necessarily two commits — one adding `migrate_from:`, one
// removing it — and between them the configuration names a NEW, EMPTY backend
// beside the old one that holds everything. Reading `backend:` alone, every
// resource looks unmanaged, and an apply CREATES ALL OF IT AGAIN: duplicate
// infrastructure, and two backends each claiming to record the same resources.
// In the flow this design exists for, that ordering is the likely one —
// configuration lands first and the pipeline runs before a human triggers
// anything.
//
// IT IS A GUARD AND NOT AN ACTION. A command never performs the migration:
// moving state as a side effect of a `plan` would be a worse surprise than the
// one being prevented. The only outcomes are "carry on" and "stop, and here is
// what to run".
//
// The DESTINATION IS CHECKED FIRST, which the command has opened anyway. State
// there means the migration is done, the block is inert, and the source is
// never touched — which matters, because opening it may start a plugin process
// and reach a network. Only an empty destination pays for a second open, and an
// empty destination is already the unusual case.
//
// An empty `Plugin` means NO BLOCK and no guard, which is every project. A
// block that was present and would not decode also lands here with an empty
// plugin; it is left alone deliberately, because `state migrate` reports that
// one properly and a second telling in a different shape gives the reader two
// problems to reconcile.
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
// An environment whose state records NO RESOURCES does not count. A backend
// keeps a file or an object per environment and one may be left behind by a
// `destroy`; treating that as "the migration is done" would wave through
// exactly the run this guard exists to stop, and it is the same emptiness test
// compareEnds makes when it decides the destination is waiting.
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
// SEPARATE FROM backendFor because a migration holds BOTH ENDS OPEN AT ONCE
// (spec §7): `backend:` and `migrate_from:` are the same shape, decoded by the
// same code, and they have to be opened by the same code too. A second way to
// turn a block into a backend is a second set of rules about what `plugin:`
// means, and a migration that read its source by different rules from its
// destination could move state somewhere nobody configured.
//
// `plugin: local` RESOLVES HERE, to the same in-process backend an absent block
// gets. Naming local has to be possible, because otherwise "migrate back to
// local" could only be said by leaving a block out, and leaving it out already
// means "no migration".
//
// An empty Plugin still means local, which is what backendFor's callers have
// always got from a project with no `backend:` block. The one exception is a
// block that was PRESENT and did not decode: it records an origin and no
// plugin, and treating that as local would write state to `.infra/` for a
// project that asked for it to live somewhere else. That refusal names
// `backend:` because that is the block whose failure moves state; a caller
// opening `migrate_from:` only does so once it has a plugin name to open.
func openBackend(
	ctx context.Context, opts *GlobalOptions, decl config.BackendDecl,
) (state.Backend, func() error, error) {
	if decl.Plugin == "" || decl.Plugin == localBackendName {
		// A BLOCK THAT DID NOT DECODE IS NOT AN ABSENT BLOCK. Config records
		// the origin of one it could not read, and treating that as "no
		// backend declared" would write state to `.infra/` for a project
		// that asked for it to live somewhere else entirely. A block that
		// named `local` decoded fine, so it never reaches this.
		if decl.Plugin == "" && decl.Origin.File != "" {
			return nil, nil, fmt.Errorf(
				"this project's `backend:` block could not be read, so infrena cannot tell where its state lives\n"+
					"  declared in: %s\n"+
					"Running anyway would write state to %s, which is not what the block asks for.\n"+
					"Run `infrena validate` to see what is wrong with it.",
				decl.Origin.File, filepath.Join(opts.Dir, StateDirName))
		}
		// The bootstrap: no block, no plugin, no process. It works before
		// anything is installed, which is what makes it the one backend
		// infrena can carry.
		return state.NewLocal(filepath.Join(opts.Dir, StateDirName)), func() error { return nil }, nil
	}

	return backendhost.Open(ctx, decl.Plugin, opts.Dir,
		backendhost.Search(opts.Dir, opts.PluginDirs), decl.Config)
}

// localBackendName is the plugin name the built-in backend answers to. It
// starts no process and needs nothing installed, which is what makes it the one
// backend infrena can carry.
const localBackendName = "local"

// backendDecls reads BOTH backend blocks out of a project's configuration in
// one decode: `backend:`, where state lives, and `migrate_from:`, where
// `state migrate` reads from.
//
// Decoding errors are IGNORED here, the same concession pluginConstraints and
// registerStateInstances make: this runs before any diagnostic can be rendered
// properly, and reporting a malformed file twice gives a reader two problems to
// reconcile instead of one. What is NOT ignored is either block's own failure
// to decode, because that one changes where state is written — see backendFor,
// which has the origin to tell it apart from a project that declared nothing.
//
// One decode rather than two because the two blocks are read together by every
// caller that wants the second one, and decoding twice is two chances to read
// two different versions of a file somebody is editing.
//
// An ABSENT `migrate_from:` is the zero value, and that is what nearly every
// project returns. A block that was present and did not decode records an
// origin and no plugin, the same distinction backendFor already relies on for
// `backend:` — so a caller must test the origin rather than assuming an empty
// Plugin means the block was never written.
func backendDecls(dir string) (backend, migrateFrom config.BackendDecl) {
	files, err := config.Load(dir)
	if err != nil {
		// No project here at all, which is local: `infra state list` outside
		// a project has nothing to read a backend out of.
		return config.BackendDecl{}, config.BackendDecl{}
	}
	decl, _ := config.Decode(files)
	return decl.Backend, decl.MigrateFrom
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
// recorded as `fake.network` can only be served by the plugin `test`, because a
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
// The environment is the caller's, and there are two callers with different answers:
// `import <env>` has one and must use it, `discover` has none and passes "". That
// distinction was missed once — import went through here with a hardcoded "" and so
// could not resolve a per-environment provider value despite being handed the
// environment on the command line.
func discoveryRegistry(
	opts *GlobalOptions, environment string, usesDefaults bool,
) (*registry.Registry, providers.Table, diag.Diagnostics, func()) {
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

	// `discover` passes "" here: it is the one command whose scope configuration does
	// not set, so everything a variable can supply without an environment is resolved
	// and anything that needs one is refused by name.
	table, instanceDS := registerStateInstances(reg, opts, environment, usesDefaults)
	ds.Extend(instanceDS)
	if len(table) == 0 {
		// ONE INSTANCE PER PLUGIN, which is EveryPlugin and deliberately not Implicit.
		//
		// This line used to call Implicit, whose job is to pick the single instance a
		// resource that names none belongs to — so it returns NOTHING when more than
		// one plugin is available, rather than guess an account for a resource. That
		// is right for binding and wrong here: with the builtin plus any installed
		// plugin, `discover` registered no instances at all and reported "Nothing
		// found." at exit 0, a silent empty survey. Found by infrena-provider-fake's
		// e2e suite.
		table = providers.EveryPlugin(reg)
		ds.Extend(providers.Register(table, reg))
	}
	return reg, table, ds, loader.Close
}
