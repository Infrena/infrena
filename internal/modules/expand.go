// Package modules flattens the module tree: it loads module sources, finds each
// instantiation, evaluates its inputs in the caller's scope, instantiates the
// module's resources under module-qualified addresses, and collects its outputs.
//
// Afterwards nothing downstream knows modules exist. The planner, graph, state
// and executor see a flat set of resources whose addresses happen to carry a
// module path. That is paid for in two places, both deliberate: diagnostics
// depend on Origin.Module to say which instantiation a problem came from, and a
// resource's address is coupled to the module it lives in, so moving a resource
// between modules renames it — which the planner reads as a destroy and a
// create, not a move. There is no `state mv`, so restructure modules before a
// resource holds data you cannot lose.
//
// This package never touches a raw YAML node: following a source means
// re-entering config.LoadModule and config.DecodeModule rather than reading a
// scalar out of a node here. It never parses a source either — config does, and
// owns the diagnostics for a missing pin, a refused scheme and `ext::`. This
// package receives a parsed source.Source and resolves it through the injected
// Resolver, handling only that resolution's own failures.
//
// Resolution happens mid-walk rather than upfront, because a nested module's own
// `modules:` list is not discoverable until that module is loaded. The resulting
// impurity is confined to one injected interface, so the stage is still testable
// in isolation.
package modules

import (
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/infrena/infrena/internal/config"
	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/internal/modules/source"
	"github.com/infrena/infrena/internal/variables"
	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/value"
)

// MaxDepth bounds module nesting at 32 instantiations. The root project is
// depth 0; a module instantiated from the root is depth 1.
//
// It counts instantiations, never loads: how many module boundaries you cross to
// reach a resource, not how many modules a project has. Every directory under
// the project root holding a module.yml is loaded automatically, so a project
// with forty side-by-side modules has forty loaded and a depth of one, and a
// bound that counted loads would turn a project's module count into a compile
// failure nobody could predict. That is enforced structurally: w.path is pushed
// only in instantiate, and loadModules never touches it.
//
// The bound exists so that a pointlessly deep tree — which the cycle check
// cannot see, because it is not a cycle — fails with a diagnostic rather than by
// exhausting the goroutine stack.
const MaxDepth = 32

// TypePrefix marks a resource as a module instantiation.
//
// A resource type is already `<provider>.<resource>`, so `module.app_stack`
// fits the shape the language already has: a module behaves as a pseudo-provider
// named `module`. There is no second field and therefore no "both set"/"neither
// set" pair to validate, which is why this was chosen over a separate `module:`
// key.
const TypePrefix = "module."

// Instance is one resource, flattened out of the module tree.
type Instance struct {
	// Address is module-qualified: a resource `db` inside an instantiation
	// `net` is `module.net.db`. A root resource keeps an empty Module.
	//
	// The module path is embedded permanently. Moving a resource from one
	// module to another renames it, and a rename is a destroy plus a create.
	Address address.Address
	// Decl is a copy owned by this instance, never the shared decoded
	// declaration: one source instantiated twice must not produce two
	// instances sharing a struct stamped with two different module paths.
	Decl *config.ResourceDecl
	// Scope is the level this resource was instantiated at, shared by every
	// resource at that level. Attributes are evaluated against it through
	// Scope.In(Decl.Dir), because at the root level the variables visible to
	// a resource depend on which resources directory declared it, while the
	// bindings do not.
	Scope *Scope
	// ProviderInstance is the provider instance this resource belongs to, or
	// "" when nothing named one, which later resolves to the default.
	//
	// Resolved here because inheritance needs the module path: a call's
	// `provider:` passes to everything it expands into, so deploying one
	// stack into two accounts is two calls differing by one line. An inner
	// resource naming its own still wins.
	//
	// Left empty rather than defaulted, so this package needs no knowledge of
	// which provider instances exist.
	ProviderInstance string
	// Skipped marks a resource excluded from this environment.
	//
	// Marked rather than omitted, and dropped at exactly one point: after
	// reference binding, before the planner. A resource that vanished here
	// would make a reference to it report "no such resource", sending a
	// reader hunting for a typo in a name that is right there in the file.
	Skipped bool
	// SkipOrigin is where the `skip`/`only` that excluded it was written.
	SkipOrigin value.Origin
	// ExtraDeps are edges that could not be written as a bare name, because
	// the name they came from expanded away. Separate from Decl.DependsOn,
	// which holds bare names resolved later against the level's own scope:
	// one slice holding both kinds would make every consumer ask which it
	// had.
	ExtraDeps []address.Address
}

// Expansion is the flattened result: a resource set with nothing in it that
// knows modules exist.
type Expansion struct {
	// Project is the root project name, which provider default resolvers
	// consume, so every resource inside a module needs it for its defaults to
	// resolve.
	//
	// Carried once per compilation rather than passed per instantiation:
	// there is exactly one project name and a module never has its own.
	Project string
	// Resolutions is every remote module source this walk resolved,
	// deduplicated and sorted. It is what a command permitted to mutate the
	// project directory writes to modules.lock; expansion only collects.
	//
	// Collected rather than written per Resolve call, because a lock file
	// created and populated in two steps is readable in between: a walk that
	// then failed on a cycle, the depth bound or a refused source would leave
	// a lock recording half a resolution as though it were complete.
	//
	// Only this walk can collect the full set, because a nested module's
	// `modules:` list is not knowable until that module is loaded.
	Resolutions []Resolved
	// Instances is every resource the walk produced, sorted by address.
	Instances []Instance
}

// Resolved is one module source and what it resolved to.
//
// The Source is carried alongside the Resolution because a Resolution alone
// does not say which source produced it, and modules.lock is keyed by source
// identity — not by the name a module was loaded under, which differs between
// levels and between projects.
type Resolved struct {
	Source     source.Source
	Resolution source.Resolution
}

// Resolver turns a parsed module source into a local directory — the one place
// this package can reach the network.
//
// Injected rather than constructed, so the stage keeps no hidden network
// dependency and a test can substitute one.
type Resolver interface {
	Resolve(s source.Source, baseDir string) (source.Resolution, diag.Diagnostics)
}

// level is one place resources and modules are declared: the root project, or
// one module file.
//
// config.ProjectDecl and config.ModuleFile are deliberately different types, so
// that a module's keys and a project's keys are distinguishable by construction
// rather than by validation, but the walk over them is identical. Narrowing them
// to one shape here, at the two call sites that know which they hold, keeps the
// walk from asking which kind of file it is at every step.
type level struct {
	Inputs    []config.VariableDecl // empty at the root
	Loads     []config.ModuleLoadDecl
	Resources []*config.ResourceDecl
	Outputs   []config.OutputDecl // empty at the root
	Origin    value.Origin
}

func rootLevel(p *config.ProjectDecl) level {
	return level{Loads: p.Modules, Resources: p.Resources, Origin: p.Origin}
}

func moduleLevel(m *config.ModuleFile) level {
	return level{
		Inputs:    m.Inputs,
		Loads:     m.Modules,
		Resources: m.Resources,
		Outputs:   m.Outputs,
		Origin:    m.Origin,
	}
}

// frame is one entry on the current instantiation path.
type frame struct {
	name   string // the instantiation's name — the resource, not the loaded module
	source string // `source:` exactly as written, for the diagnostic
	dir    string // the resolved absolute directory, the cycle key
}

// walker carries the state the recursion needs and the output it accumulates.
type walker struct {
	root string // the project directory, for rendering paths in diagnostics
	// resolve is the one place this package can reach the network.
	resolve Resolver
	path    []frame // the current instantiation path; pushed and popped
	// resolved is keyed by source identity, so one source loaded under two
	// names is one entry.
	resolved  map[string]Resolved
	env       Env
	instances []Instance
	ds        *diag.Diagnostics
}

// Expand flattens a project's module tree.
//
// The returned Expansion is meaningless if the returned diagnostics contain
// errors: it holds whatever the walk managed to collect, which for a broken
// module tree is a subset of the project's resources — and a subset of desired
// state diffed against a full state file reads as "removed from configuration".
// Check HasErrors before touching it.
//
// dirScopes is the per-directory variable scopes, keyed by
// config.ResourceDecl.Dir, and may be nil. It applies to the root level only;
// see Scope.dirVars.
func Expand(
	project *config.ProjectDecl, scope variables.Scope,
	dirScopes map[string]variables.Scope, env Env, dir string, resolve Resolver,
	secrets func(name string) (value.Value, bool), secretsErr func() error,
	templates func(dir, name string) (string, value.Origin, bool),
) (*Expansion, diag.Diagnostics) {
	var ds diag.Diagnostics

	abs, err := filepath.Abs(dir)
	if err != nil {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "cannot resolve the project directory " + strconv.Quote(dir),
			Detail:   err.Error(),
			Action:   "Run the command from a directory that exists.",
		})
		return &Expansion{}, ds
	}

	w := &walker{root: abs, resolve: resolve, env: env, ds: &ds}
	w.expand(rootLevel(project), &Scope{Vars: scope, Secrets: secrets, SecretsErr: secretsErr, Templates: templates, dirVars: dirScopes, names: map[string]Binding{}, skipped: map[string]value.Origin{}}, abs, nil, "")

	// One sort, after everything is collected. The walk is already
	// deterministic, but a deterministic walk is not address order: a level's
	// own resources interleave with its modules'. Sorting any earlier would be
	// undone by the next append.
	sort.Slice(w.instances, func(i, j int) bool {
		return w.instances[i].Address.String() < w.instances[j].Address.String()
	})
	return &Expansion{
		Project:     project.Project,
		Resolutions: w.sortedResolutions(),
		Instances:   w.instances,
	}, ds
}

// expand walks one level: it records that level's plain resources, then
// instantiates each module call in turn — in an order where every call a
// sibling's attributes read has already been expanded — then fans depends_on
// edges that name, or are written on, a module call out to what that call
// produced.
//
// module is the instantiation path to this level, outermost first, empty at the
// root. It returns the addresses this level produced, so a caller instantiating
// a module can fan its own depends_on out to them. inherited is the provider
// instance passed down from the module call that entered this level, or "" at
// the root.
func (w *walker) expand(lv level, scope *Scope, dir string, module []string, inherited string) []address.Address {
	loaded := w.loadModules(lv, dir, module)

	var produced []address.Address
	// calls maps a module call's resource name to the addresses it produced.
	// Nothing is addressed with that name after expansion, so this table is
	// the only way an edge naming it can be resolved.
	calls := map[string][]address.Address{}

	// Resources arrive sorted by name, so this walk is deterministic. It is
	// not the final order: Expand sorts the whole expansion by address once, at
	// the end.
	for _, r := range lv.Resources {
		if strings.HasPrefix(r.Type, TypePrefix) {
			continue
		}
		addr := addressIn(module, r.Name)
		skipped, skipOrigin := w.excluded(r, scope.In(r.Dir), w.env)

		// for_each after skip, deliberately: a resource excluded from this
		// environment is not here at all, so evaluating how many of it to make
		// would report errors about a resource nobody asked for.
		if !skipped && r.ForEach.Name != "" {
			entries, ok := w.forEachInstances(r, scope.In(r.Dir))
			if !ok {
				continue
			}
			for _, entry := range entries {
				keyed := addr.WithKey(entry.Key)
				w.instances = append(w.instances, Instance{
					Address:          keyed,
					Decl:             instantiateDecl(r, module),
					Scope:            eachScope(scope, entry),
					ProviderInstance: providerFor(r, inherited),
				})
				produced = append(produced, keyed)
			}
			// Bound under the declared name, carrying every instance it
			// made, so a reference can name the set and one instance can be
			// picked out of it. Binding each key separately would make
			// `${subnet}` resolve to nothing.
			scope.bind(r.Name, Binding{Kind: BindsResource, Address: addr, Instances: produced[len(produced)-len(entries):]})
			continue
		}

		w.instances = append(w.instances, Instance{
			Address:          addr,
			Decl:             instantiateDecl(r, module),
			Scope:            scope,
			ProviderInstance: providerFor(r, inherited),
			Skipped:          skipped,
			SkipOrigin:       skipOrigin,
		})
		if skipped {
			// Not bound as a resource: a reference to it must not produce an
			// edge to something that is not being created. Recorded as
			// skipped so a later diagnostic can say why the name does not
			// resolve.
			scope.markSkipped(r.Name, skipOrigin)
			continue
		}
		produced = append(produced, addr)
		// Bound before any call is expanded, because a call's attributes may
		// read a sibling resource, and resources need no ordering pass: they
		// are not expanded, only named.
		scope.bind(r.Name, Binding{Kind: BindsResource, Address: addr})
	}

	// Attributes are parsed once: the ordering pass below reads the
	// references and evaluateCall reads the trees.
	exprs := map[string]map[string]*value.Expr{}
	for _, r := range lv.Resources {
		if strings.HasPrefix(r.Type, TypePrefix) {
			exprs[r.Name] = w.parseCall(r)
		}
	}

	for _, r := range w.orderCalls(lv, exprs, module) {
		// A skipped call is never entered. Its resources have no addresses
		// to mark, so unlike a plain resource there is nothing to put in the
		// expansion — only the name, recorded so a reference can be reported.
		if skipped, skipOrigin := w.excluded(r, scope.In(r.Dir), w.env); skipped {
			scope.markSkipped(r.Name, skipOrigin)
			continue
		}
		// A call with for_each expands once per entry, exactly as a resource
		// does, and every resource inside lands under the keyed call:
		// module.store["orders"].db.
		//
		// The inputs are evaluated once per entry, because `each.key` and
		// `each.value` are the whole point: a call that could not vary its
		// inputs per instance would be a loop producing N copies of one thing.
		if r.ForEach.Name != "" {
			entries, ok := w.forEachInstances(r, scope.In(r.Dir))
			if !ok {
				continue
			}
			var all []address.Address
			keys := make([]string, 0, len(entries))
			keyedOutputs := make(map[string]map[string]value.Value, len(entries))
			keyedAddresses := make(map[string][]address.Address, len(entries))
			for _, entry := range entries {
				each := eachScope(scope, entry)
				// The call's name carries the key, so addressIn builds the
				// keyed module path with no change to addressing: a keyed
				// instantiation is a module level whose name happens to be
				// bracketed. A Key on every level of address.Address would
				// instead change the state key of every resource in every
				// module.
				keyed := *r
				keyed.Name = keyedCallName(r.Name, entry.Key)

				supplied := w.evaluateCall(r, each.In(r.Dir), exprs[r.Name])
				inner, outputs := w.instantiate(&keyed, loaded, each, supplied, dir, module, inherited)

				keys = append(keys, entry.Key)
				keyedOutputs[entry.Key] = outputs
				keyedAddresses[entry.Key] = inner
				// Per entry, so `network: ${net.id}` on the call is a
				// dependency of that entry's resources. Evaluated in the
				// entry's own scope for the same reason the inputs are.
				w.attachEdges(inner, w.callReferences(exprs[r.Name], each))
				all = append(all, inner...)
			}
			calls[r.Name] = all
			produced = append(produced, all...)
			scope.bind(r.Name, Binding{
				Kind:           BindsModule,
				Addresses:      all,
				Keys:           keys,
				KeyedOutputs:   keyedOutputs,
				KeyedAddresses: keyedAddresses,
			})
			continue
		}

		supplied := w.evaluateCall(r, scope.In(r.Dir), exprs[r.Name])
		inner, outputs := w.instantiate(r, loaded, scope, supplied, dir, module, inherited)
		calls[r.Name] = inner
		// A reference in the call's own attributes — `network: ${net.id}` — is
		// a dependency of everything the call expanded into, and nothing else
		// records it. The call is expanded away, so nothing downstream sees its
		// attributes, and inside the module that value arrives as an input:
		// a bare `${var.network}` with no dot, which the edge walk skips.
		// Without this the plan looks clean and the apply fails with "network
		// is still unknown after its dependencies were applied", after the
		// outer resource has already been created.
		w.attachEdges(inner, w.callReferences(exprs[r.Name], scope))
		produced = append(produced, inner...)
		// Bound after expansion, because the outputs do not exist until then.
		// That is exactly why orderCalls exists.
		scope.bind(r.Name, Binding{Kind: BindsModule, Addresses: inner, Outputs: outputs})
	}

	w.fanOut(lv, calls, module)
	return produced
}

// callReferences collects what a module call's own attributes refer to, as
// addresses, deduplicated and sorted.
//
// Resolved through the scope rather than by qualifying the expression, for the
// same reason fanOut resolves a depends_on name that way: a reference may name
// an outer resource or another module call, and a call has many addresses.
// Qualify would also fold a resolved output into a literal, leaving no
// reference to see, so the edge to another module would go missing.
//
// Coarse on purpose: every resource the call produced gets every edge, rather
// than only those whose own attributes use the input. A superset is correct
// here — the module cannot begin before its inputs exist — and per-input
// tracking would buy nothing the executor can use.
func (w *walker) callReferences(exprs map[string]*value.Expr, scope *Scope) []address.Address {
	var out []address.Address
	seen := map[string]bool{}
	add := func(a address.Address) {
		if k := a.String(); !seen[k] {
			seen[k] = true
			out = append(out, a)
		}
	}

	names := make([]string, 0, len(exprs))
	for name := range exprs {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		if exprs[name] == nil {
			continue
		}
		for _, ref := range exprs[name].References() {
			// Scope-relative: `${net.id}` names `net` at this level.
			b, ok := scope.Lookup(ref.Target.Name)
			if !ok {
				// An unbound name is a diagnostic elsewhere, not an edge.
				continue
			}
			switch b.Kind {
			case BindsResource:
				add(b.Address)
			case BindsModule:
				for _, a := range b.Addresses {
					add(a)
				}
			}
		}
	}
	return sortAddresses(out)
}

// fanOut rewrites every depends_on edge that names, or is written on, a module
// call — the two cases created by making an instantiation a resource whose name
// does not survive expansion.
func (w *walker) fanOut(lv level, calls map[string][]address.Address, module []string) {
	for _, r := range lv.Resources {
		isCall := strings.HasPrefix(r.Type, TypePrefix)

		var edges []address.Address
		for _, target := range r.DependsOn {
			produced, ok := calls[target]
			if !ok {
				// A plain resource keeps its bare name, resolved later
				// against the level's scope. Nothing to do here.
				continue
			}
			edges = append(edges, produced...)
		}
		edges = sortAddresses(edges)

		if isCall {
			// The call's own depends_on belongs to everything it expanded into.
			// Its non-call targets are bare names that no longer have a
			// resource to sit on, so they are resolved here too.
			w.attachEdges(calls[r.Name], append(edges, w.plainTargets(r, calls, module)...))
			continue
		}
		w.attachEdges([]address.Address{addressIn(module, r.Name)}, edges)
	}
}

// plainTargets resolves a module call's depends_on entries that name ordinary
// resources at the same level. A plain resource keeps its bare names; a module
// call cannot, because the resources that inherit the edge are in a different
// scope and the name would be resolved there.
func (w *walker) plainTargets(
	r *config.ResourceDecl, calls map[string][]address.Address, module []string,
) []address.Address {
	var out []address.Address
	for _, target := range r.DependsOn {
		if _, isCall := calls[target]; isCall {
			continue
		}
		out = append(out, addressIn(module, target))
	}
	return sortAddresses(out)
}

// loadedModule is one module made available under a name at one level.
type loadedModule struct {
	Name string
	Dir  string // resolved, absolute — the cycle key
	// Source is display text, not a source.Source: every use of it is a
	// diagnostic, and a discovered module has no written source at all.
	Source string
	Origin value.Origin
}

// loadModules resolves this level's `modules:` list into a name table.
//
// Names are derived during decoding and read here; deriving them a second time
// would be two spellings of one rule. At the root, discovered modules are folded
// in too — see discover.go.
func (w *walker) loadModules(lv level, dir string, module []string) map[string]loadedModule {
	out := map[string]loadedModule{}

	if len(module) == 0 {
		// Explicit names are passed to discover so it can stay silent about
		// them: an explicit `modules:` entry beats a discovered one, which
		// includes not reporting a collision between two on-disk directories
		// sharing that name, since neither would ever be used.
		explicit := make(map[string]bool, len(lv.Loads))
		for _, ld := range lv.Loads {
			explicit[ld.Name] = true
		}
		for _, m := range w.discover(dir, explicit) {
			out[m.Name] = m
		}
	}

	for _, ld := range lv.Loads {
		// Resolve, never Parse. Decoding already parsed this and reported a
		// missing pin, a refused scheme or `ext::` with the line number, so
		// re-parsing here would double-report every bad source. Only Resolve's
		// own failures — I/O, auth, a missing ref, a corrupt cache — belong
		// here.
		res, resolveDiags := w.resolve.Resolve(ld.Source, dir)
		w.ds.Extend(resolveDiags)
		if resolveDiags.HasErrors() {
			continue
		}
		// Collected, never written; see Expansion.Resolutions.
		w.record(ld.Source, res)
		// An explicit `modules:` entry silently overwrites a discovered one of
		// the same name: the one case where a name collision is not an error.
		out[ld.Name] = loadedModule{
			Name: ld.Name,
			Dir:  filepath.Clean(res.Dir),
			// The canonical rendering of the parsed source, which is what a
			// reader recognises in a diagnostic.
			Source: ld.Source.String(),
			// ld.Source carries its own Origin, stamped from the line it was
			// parsed off.
			Origin: ld.Source.Origin,
		}
	}
	return out
}

// instantiate expands one module call: it resolves the type to a loaded module,
// checks the two guards, re-enters loading and decoding for the module file, and
// recurses. supplied is the call's attributes, already evaluated in the caller's
// scope by evaluateCall. It returns the addresses the module produced and the
// outputs it collected — nil, nil on every early return, because a module that
// failed one of these guards produced nothing to bind.
func (w *walker) instantiate(
	r *config.ResourceDecl, loaded map[string]loadedModule,
	caller *Scope, supplied map[string]value.Value, dir string, module []string,
	inherited string,
) ([]address.Address, map[string]value.Value) {
	lm, ok := w.resolveCall(r, loaded, module)
	if !ok {
		return nil, nil
	}

	// The cycle check must come before the depth check, and it matters only
	// in one case: a chain of MaxDepth distinct modules that closes back on
	// itself, where both conditions hold at once. Then this decides whether
	// the user is told "you have a loop, here it is" or "your nesting is too
	// deep", and the second sends them looking for nesting they do not have.
	if at := w.onPath(lm.Dir); at >= 0 {
		w.ds.Add(w.cycleDiagnostic(at, r, lm))
		return nil, nil
	}
	if len(w.path) >= MaxDepth {
		w.ds.Add(w.depthDiagnostic(r, lm))
		return nil, nil
	}

	// LoadModule/DecodeModule, not config.Load/config.Decode: a module has no
	// variables.yml, no environments/ and no `project:`, and the project
	// loader would pull all three into a module.
	file, err := config.LoadModule(lm.Dir)
	if err != nil {
		w.ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "cannot load module " + strconv.Quote(lm.Name),
			Detail: "`" + lm.Source + "` resolves to " + w.show(lm.Dir) + ", and " + err.Error() +
				".\n\nA module source is a directory holding " + config.ModuleFileName + ".",
			Action: "Correct the `modules:` entry, or create " +
				filepath.ToSlash(filepath.Join(lm.Source, config.ModuleFileName)) + ".",
			Origin: lm.Origin,
		})
		return nil, nil
	}

	inner := make([]string, len(module)+1)
	copy(inner, module)
	// The instantiation's name, not the loaded module's: two calls to one
	// module must be tellable apart, and the resource name is what the user
	// chose per call.
	inner[len(module)] = r.Name

	child, decodeDiags := config.DecodeModule(file)
	// Stamped with the instantiation path before they are collected. A module
	// file's own diagnostics carry its file and line, which is not enough: one
	// file instantiated twice produces two identical messages, and
	// Origin.Module is the only thing that tells them apart.
	w.ds.Extend(inModulePath(decodeDiags, inner))
	if decodeDiags.HasErrors() {
		// A module whose own file did not decode has no usable declarations.
		// Recursing would report every consequence of the syntax error as a
		// second, worse-told problem.
		return nil, nil
	}

	w.path = append(w.path, frame{name: r.Name, source: lm.Source, dir: lm.Dir})
	// The path is the current branch, not everything visited: popping is what
	// lets one source be instantiated twice on two branches, an ordinary
	// diamond and not a cycle.
	defer func() { w.path = w.path[:len(w.path)-1] }()

	childLevel := moduleLevel(child)
	innerScope := w.moduleScope(r, childLevel, caller, supplied, inner)
	// The call's provider is inherited by everything inside. An inner resource
	// naming its own still wins, and a call that names none passes down
	// whatever it inherited, so a module three levels deep still lands in the
	// account the outermost call chose.
	addrs := w.expand(childLevel, innerScope, lm.Dir, inner, providerFor(r, inherited))
	// Collected after expanding, so the module's own names are all bound and
	// an output reading ${service.endpoint} resolves to module.<call>.service.
	return addrs, w.collectOutputs(childLevel, innerScope)
}

// resolveCall turns a `module.<name>` type into the module it names.
func (w *walker) resolveCall(
	r *config.ResourceDecl, loaded map[string]loadedModule, module []string,
) (loadedModule, bool) {
	// The two shape guards — `module.` with nothing after it, and a name
	// containing a further `.` — are deliberately not here. Decoding owns
	// them, because it holds the line number and a malformed type is a
	// property of the text. A copy here would find an empty or dotted name and
	// report that some module does not exist, naming a module the user never
	// wrote. Compilation halts after decode errors, so a malformed type never
	// reaches here.
	name := strings.TrimPrefix(r.Type, TypePrefix)

	lm, ok := loaded[name]
	if !ok {
		w.ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "no module named " + strconv.Quote(name) + " is loaded",
			Detail: "Resource " + strconv.Quote(r.Name) + " in " + where(module) +
				" instantiates it.\n\nLoaded here:\n  " + strings.Join(loadedNames(loaded), "\n  "),
			Action: "Add it under `modules:`, or correct the name. A module in a directory holding " +
				config.ModuleFileName + " under the project root is loaded automatically.",
			Origin: r.Origin,
		})
		return loadedModule{}, false
	}
	return lm, true
}

// record keeps one resolution for modules.lock.
//
// KindPath sources are skipped: a local path has no revision to pin, the same
// fact that makes a pin required on remote sources and not on these.
//
// Keyed by source identity rather than by the name the module was loaded under,
// so one source loaded under two names is one lock entry.
func (w *walker) record(s source.Source, res source.Resolution) {
	if s.Kind == source.KindPath {
		return
	}
	if w.resolved == nil {
		w.resolved = map[string]Resolved{}
	}
	w.resolved[s.String()] = Resolved{Source: s, Resolution: res}
}

// sortedResolutions returns the collected resolutions in a stable order.
//
// Sorted because modules.lock is compared against on every later run — an
// existing entry that differs is an error, never a silent update — so a file
// whose lines reordered between identical runs would report a change that did
// not happen. The map it reads from has randomised iteration order, so this sort
// is load-bearing rather than defensive.
func (w *walker) sortedResolutions() []Resolved {
	if len(w.resolved) == 0 {
		return nil
	}
	out := make([]Resolved, 0, len(w.resolved))
	for _, r := range w.resolved {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Source.String() < out[j].Source.String()
	})
	return out
}

// onPath returns the index of dir on the current instantiation path, or -1.
func (w *walker) onPath(dir string) int {
	for i, f := range w.path {
		if f.dir == dir {
			return i
		}
	}
	return -1
}

// show renders an absolute directory relative to the project, so a diagnostic
// prints modules/net rather than /tmp/TestX123/modules/net.
func (w *walker) show(dir string) string {
	rel, err := filepath.Rel(w.root, dir)
	if err != nil {
		return dir
	}
	return filepath.ToSlash(rel)
}

// cycleDiagnostic renders the cycle in participation order with the wrap
// included — `alpha (./a) -> beta (../b) -> gamma (../a)` — the same shape
// internal/environments and graph.Layers use, so every cycle error in this
// system reads alike.
//
// Each step is `instantiation (source as written)` rather than a bare path: the
// same directory is spelled differently from different levels, so a chain of
// raw sources would end in a string the reader cannot match against its start.
// The sentence beneath names the repeated directory once.
func (w *walker) cycleDiagnostic(at int, r *config.ResourceDecl, lm loadedModule) diag.Diagnostic {
	steps := make([]string, 0, len(w.path)-at+1)
	for _, f := range w.path[at:] {
		steps = append(steps, f.name+" ("+f.source+")")
	}
	steps = append(steps, r.Name+" ("+lm.Source+")")

	return diag.Diagnostic{
		Severity: diag.SeverityError,
		Summary:  "module instantiation forms a cycle",
		Detail: strings.Join(steps, " -> ") + "\n\n" + strconv.Quote(r.Name) + " instantiates " +
			w.show(lm.Dir) + ", which is already being expanded, so expanding it never terminates.",
		Action: "Remove one instantiation so the chain ends at a module that instantiates nothing.",
		Origin: r.Origin,
	}
}

// depthDiagnostic is the other failure, and says so in different words. A tree
// that is merely deep is not one that never terminates, and telling a user with
// a 40-deep tree to look for a cycle sends them where there is nothing to find.
func (w *walker) depthDiagnostic(r *config.ResourceDecl, lm loadedModule) diag.Diagnostic {
	var from string
	if len(w.path) > 0 {
		from = ", instantiated from " + strconv.Quote(w.path[len(w.path)-1].name)
	}
	return diag.Diagnostic{
		Severity: diag.SeverityError,
		Summary:  "module nesting is deeper than " + strconv.Itoa(MaxDepth) + " instantiations",
		Detail: "Expanding " + strconv.Quote(r.Name) + from + " would be instantiation number " +
			strconv.Itoa(len(w.path)+1) + ". Nesting is bounded so that a mistake in a `modules:` " +
			"entry fails here rather than by exhausting the stack.\n\nThis is not a cycle: no " +
			"module on the path instantiates itself.",
		Action: "Flatten the module tree, or inline the innermost modules into their callers.",
		Origin: r.Origin,
	}
}

// inModulePath stamps every diagnostic with the full instantiation path it came
// from, so that a diagnostic from inside a module names the instantiation — the
// resource — rather than the loaded module.
func inModulePath(ds diag.Diagnostics, module []string) diag.Diagnostics {
	// Outermost-last, for the same reason as originInPath: Diagnostics.InModule
	// prepends one name at a time.
	for _, m := range slices.Backward(module) {
		ds = ds.InModule(m)
	}
	return ds
}

// loadedNames lists what is loaded at a level, sorted, for a diagnostic. Go's
// map iteration is randomised and such a list must not reorder itself between
// runs of the same configuration.
func loadedNames(loaded map[string]loadedModule) []string {
	out := make([]string, 0, len(loaded))
	for name := range loaded {
		out = append(out, name)
	}
	if len(out) == 0 {
		return []string{"(none)"}
	}
	sort.Strings(out)
	return out
}

// where names a level for a diagnostic. The module path is a message's only
// way to say which instantiation a problem came from.
func where(module []string) string {
	if len(module) == 0 {
		return "the project"
	}
	return "module " + strings.Join(module, ".")
}

// providerFor resolves which instance a resource belongs to, at this level.
//
// The resource's own `provider:` wins; otherwise it inherits from the module
// call that entered this level; otherwise "", and the default is supplied later.
// Written as one expression so the precedence cannot drift between the resource
// case and the call case.
func providerFor(r *config.ResourceDecl, inherited string) string {
	if name, ok := r.Provider.Value.AsString(); ok && name != "" {
		return name
	}
	return inherited
}
