// Package modules implements compiler stage 5: it loads module sources, finds
// each instantiation, evaluates its inputs in the CALLER's scope, instantiates
// the module's resources under module-qualified addresses, and collects its
// outputs (spec §7 stage 5; PLAN.md §11).
//
// After this stage nothing downstream knows modules exist (spec §7.2). The
// planner, graph, state and executor see a flat set of resources whose
// addresses happen to carry a module path. That simplification is paid for in
// two places, and both are deliberate: diagnostics depend on Origin.Module to
// say which instantiation a problem came from, and a resource's address is
// coupled to the module it lives in, so MOVING A RESOURCE BETWEEN MODULES
// RENAMES IT — which the planner reads as a destroy and a create, not a move.
// There is no `state mv` in Phase 1 (spec §5.2). Restructure modules before a
// resource holds data you cannot lose.
//
// This package never touches a raw YAML node. Stage 2 is the only stage permitted
// to (spec §7), so following a source means re-entering config.LoadModule and
// config.DecodeModule rather than reading a scalar out of a node here
// (Ruling 2).
//
// It never PARSES a source: stage 2 does, and owns the diagnostics for a
// missing pin, a refused scheme and `ext::` (Amendment 15b). Stage 5 receives a
// parsed source.Source and resolves it through the injected Resolver, handling
// only that resolution's own failures.
//
// Resolution happens mid-walk rather than upfront, because a nested module's own
// `modules:` list is not discoverable until that module is loaded. That makes
// this stage impure, against spec §7's "each stage is a pure function" — so the
// impurity is confined to one injected interface, which is what §7's purity was
// protecting (each stage tested in isolation) rather than the letter of it.
package modules

import (
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/infrata/infrata/internal/config"
	"github.com/infrata/infrata/internal/diag"
	"github.com/infrata/infrata/internal/modules/source"
	"github.com/infrata/infrata/internal/variables"
	"github.com/infrata/infrata/pkg/value"
)

// MaxDepth bounds module nesting at 32 instantiations (spec §7.2). The root
// project is depth 0; a module instantiated from the root is depth 1.
//
// IT COUNTS INSTANTIATIONS, NEVER LOADS (Amendment 12b) — how many module
// boundaries you cross to reach a resource, not how many modules a project has.
// §8e auto-loads every directory under the project root holding a module.yml, so
// a project with forty side-by-side modules has forty loaded and a depth of one.
// A bound that counted loads would turn a project's module COUNT into a compile
// failure: a ceiling no user could predict from their nesting. Enforced
// structurally rather than by care — w.path is pushed only in instantiate, and
// loadModules never touches it.
//
// The bound exists so that a pointlessly deep tree — a mistake the cycle check
// cannot see, because it is not a cycle — fails with a diagnostic rather than by
// exhausting the goroutine stack.
const MaxDepth = 32

// TypePrefix marks a resource as a module instantiation (PLAN.md §11, §8c).
//
// A resource type is already `<provider>.<resource>`, so `module.app_stack` is
// well-formed in the shape the language already has: a module behaves as a
// pseudo-provider named `module`. There is no second field and therefore no
// "both set"/"neither set" pair to validate, which is why this spelling was
// chosen over a separate `module:` key.
const TypePrefix = "module."

// Instance is one resource, flattened out of the module tree.
//
// Decl is a COPY owned by this instance, never stage 2's declaration: one
// source instantiated twice must not produce two instances sharing a struct
// that Task 6 then stamps with two different module paths. Task 6 makes the
// copy; until then the field holds what the walk found.
type Instance struct {
	Decl *config.ResourceDecl
	// Scope is the level this resource was instantiated at, shared by every
	// resource at that level. Compiler stage 6 evaluates the resource's
	// attributes against it.
	Scope *Scope
}

// Expansion is stage 5's output: a flat resource set, and nothing in it that
// knows modules exist.
type Expansion struct {
	// Project is the ROOT project name. It reaches ResolvedConfig.Project and
	// from there schema.DefaultContext.Project, which provider default
	// resolvers consume — so every resource inside a module needs it for its
	// defaults to resolve.
	//
	// Carried here, once per compilation, rather than passed as a parameter a
	// future caller could vary per instantiation: there is exactly one project
	// name and a module never has its own. config.ModuleFile must never gain a
	// `Project` field for the same reason `project:` in a module file is
	// rejected — the decoder has no case for it, so the field would be one
	// nothing ever fills.
	Project string
	// Resolutions is every remote module source this walk resolved, deduplicated
	// and sorted. It is what a command permitted to mutate the project directory
	// writes to modules.lock (Amendment 18) — stage 5 COLLECTS and never writes.
	//
	// Collected rather than written per Resolve call for the reason M4 already
	// paid for at ae1e309: a file created and populated in two steps is readable
	// in between, so a walk that then fails on a cycle, the depth bound or a
	// refused source would leave a lock recording half a resolution as though it
	// were complete. internal/state/lock.go fixed that shape once; a new
	// artifact must not reintroduce it.
	//
	// Stage 5 is the only component that CAN collect the full set: a nested
	// module's `modules:` list is not knowable until that module is loaded, so
	// nothing upstream of the walk has seen every source.
	Resolutions []Resolved
	Instances   []Instance
}

// Resolved is one module source and what it resolved to.
//
// The Source is carried alongside the Resolution because a Resolution alone
// ({Dir, Commit}) does not say WHICH source produced it, and modules.lock is
// keyed by source identity — not by the name a module was loaded under, which
// differs between levels and between projects.
type Resolved struct {
	Source     source.Source
	Resolution source.Resolution
}

// Resolver turns a parsed module source into a local directory — the one place
// internal/modules can reach the network.
//
// Injected rather than constructed so a compiler stage keeps no hidden network
// dependency and every test can substitute one (contract, Task 4 header).
type Resolver interface {
	Resolve(s source.Source, baseDir string) (source.Resolution, diag.Diagnostics)
}

// level is one place resources and modules are declared: the root project, or
// one module file.
//
// config.ProjectDecl and config.ModuleFile are deliberately different types —
// that is what makes a module's keys and a project's keys distinguishable by
// construction rather than by validation (Amendment 1) — and the walk over them
// is identical. Narrowing them to one shape HERE, at the two call sites that
// know which they hold, keeps the walk from asking which kind of file it is at
// every step.
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
	name   string // the INSTANTIATION's name — the resource, not the loaded module
	source string // `source:` exactly as written, for the diagnostic
	dir    string // the resolved absolute directory, the cycle key
}

// walker carries the state the recursion needs and the output it accumulates.
type walker struct {
	root string // the project directory, for rendering paths in diagnostics
	// resolve is the ONE place internal/modules can reach the network. See
	// Resolver's doc comment for why it is injected rather than constructed.
	resolve Resolver
	path    []frame // the CURRENT instantiation path; pushed and popped
	// resolved is keyed by source identity, so one source loaded under two
	// names is one entry.
	resolved  map[string]Resolved
	instances []Instance
	ds        *diag.Diagnostics
}

// Expand is compiler stage 5.
//
// CONTRACT, matching compiler.Compile's: the returned Expansion is meaningless
// if the returned diagnostics contain errors. It holds whatever the walk
// managed to collect, which for a broken module tree is a SUBSET of the
// project's resources — and a subset of desired state diffed against a full
// state file is exactly what invariant 1 reads as "removed from
// configuration". Check HasErrors() before touching it.
func Expand(project *config.ProjectDecl, scope variables.Scope, dir string, resolve Resolver) (*Expansion, diag.Diagnostics) {
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

	w := &walker{root: abs, resolve: resolve, ds: &ds}
	w.expand(rootLevel(project), &Scope{Vars: scope}, abs, nil)
	return &Expansion{
		Project:     project.Project,
		Resolutions: w.sortedResolutions(),
		Instances:   w.instances,
	}, ds
}

// expand walks one level: it records that level's plain resources, then
// instantiates each module call in turn.
//
// module is the instantiation path to this level, outermost first, empty at the
// root. It names the level in diagnostics here; Task 6 turns it into an address.
func (w *walker) expand(lv level, scope *Scope, dir string, module []string) {
	loaded := w.loadModules(lv, dir, module)

	// Stage 2 sorted Resources by name, so this walk is deterministic. It is
	// NOT the final order — Task 6 sorts the whole expansion by address, once,
	// at the end. Sorting here would be the twelfth redundant sort in this tree
	// and would be undone by the next append.
	for _, r := range lv.Resources {
		if strings.HasPrefix(r.Type, TypePrefix) {
			continue
		}
		w.instances = append(w.instances, Instance{Decl: r, Scope: scope})
	}

	// Task 7 replaces this with a topological order over sibling references,
	// which degenerates to name order when there are none.
	for _, r := range lv.Resources {
		if !strings.HasPrefix(r.Type, TypePrefix) {
			continue
		}
		w.instantiate(r, loaded, scope, dir, module)
	}
}

// loadedModule is one module made available under a name at one level.
type loadedModule struct {
	Name string
	Dir  string // resolved, absolute — the cycle key
	// Source is DISPLAY TEXT, not a source.Source: every use of it is a
	// diagnostic, and a discovered module has no written source at all. Keeping
	// the parsed type here as well would be two spellings of one concept.
	Source string
	Origin value.Origin
}

// loadModules resolves this level's `modules:` list into a name table.
//
// Names are derived by stage 2 (§8d) and read here; deriving them a second time
// would be two spellings of one rule. At the root, discovered modules are
// folded in too — see discover.go.
func (w *walker) loadModules(lv level, dir string, module []string) map[string]loadedModule {
	out := map[string]loadedModule{}

	if len(module) == 0 {
		// Names explicit here are passed to discover so it can stay silent
		// about them (§7, §8e): an explicit `modules:` entry beats a
		// discovered one of the same name, and that includes not reporting a
		// collision between two ON-DISK directories sharing that name when
		// neither one's discovery outcome matters — the explicit entry below
		// always wins the slot regardless of what discover found for it.
		explicit := make(map[string]bool, len(lv.Loads))
		for _, ld := range lv.Loads {
			explicit[ld.Name] = true
		}
		for _, m := range w.discover(dir, explicit) {
			out[m.Name] = m
		}
	}

	for _, ld := range lv.Loads {
		// Resolve, never Parse. Stage 2 parsed this and reported a missing pin,
		// a refused scheme or `ext::` with the line number (Amendment 15b);
		// re-parsing here would double-report every bad source. Only Resolve's
		// OWN failures — I/O, auth, a missing ref, a corrupt cache — belong to
		// stage 5.
		res, resolveDiags := w.resolve.Resolve(ld.Source, dir)
		w.ds.Extend(resolveDiags)
		if resolveDiags.HasErrors() {
			continue
		}
		// COLLECTED, never written (Amendment 18). See Expansion.Resolutions
		// for why writing per call is the M4 lock-file defect in a new file.
		w.record(ld.Source, res)
		// An explicit `modules:` entry beats a discovered one silently — §7's
		// "explicit config always wins over an implicit default", and §8e names
		// this as the one case where a name collision is NOT an error.
		out[ld.Name] = loadedModule{
			Name: ld.Name,
			Dir:  filepath.Clean(res.Dir),
			// Source.String() renders the parsed source in its canonical form
			// for display; the diagnostic wants what a reader recognises, not a
			// second spelling of the same thing.
			Source: ld.Source.String(),
			// ld.Source carries its own Origin, stamped from the line Parse
			// read it off (config.ModuleLoadDecl has no separate SourceOrigin
			// field — see its doc comment).
			Origin: ld.Source.Origin,
		}
	}
	return out
}

// instantiate expands one module call: it resolves the type to a loaded module,
// checks the two guards, re-enters stages 1 and 2 through the module loader,
// and recurses.
func (w *walker) instantiate(
	r *config.ResourceDecl, loaded map[string]loadedModule,
	caller *Scope, dir string, module []string,
) {
	lm, ok := w.resolveCall(r, loaded, module)
	if !ok {
		return
	}

	// ORDER IS LOAD-BEARING, but NOT for the reason it first appears. A cycle
	// is caught at its first repeat, so a simple a->b->a loop trips this at
	// depth 2 and is reported as a cycle whichever guard runs first. Swapping
	// the two leaves almost every test here green.
	//
	// The order is observable only when a cycle first repeats AT the bound —
	// a chain of MaxDepth distinct modules that closes back on itself, where
	// both conditions hold at once. Then it decides whether the user is told
	// "you have a loop, here it is" or "your nesting is too deep", and the
	// second sends them looking for nesting they do not have.
	// TestADeepCycleIsReportedAsACycleNotAsDepth is the only thing that pins
	// this; an earlier comment here claimed every cycle was at stake, which is
	// wrong and would have justified deleting that test as redundant.
	if at := w.onPath(lm.Dir); at >= 0 {
		w.ds.Add(w.cycleDiagnostic(at, r, lm))
		return
	}
	if len(w.path) >= MaxDepth {
		w.ds.Add(w.depthDiagnostic(r, lm))
		return
	}

	// LoadModule/DecodeModule, not config.Load/config.Decode: a module has no
	// variables.yml, no environments/ and no `project:`, and the project loader
	// would pull all three into a module (Ruling 2, refined by Amendment 1).
	// Stage 5 still never sees a raw YAML node.
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
		return
	}

	inner := make([]string, len(module)+1)
	copy(inner, module)
	// The INSTANTIATION's name, not the loaded module's: two calls to one
	// module must be tellable apart, and the resource name is what the user
	// chose per call.
	inner[len(module)] = r.Name

	child, decodeDiags := config.DecodeModule(file)
	// Stamped with the instantiation path before they are collected. A module
	// file's own diagnostics carry its file and line, which is not enough: one
	// file instantiated twice produces two identical messages, and Ruling 7
	// makes Origin.Module the only thing that tells them apart.
	w.ds.Extend(inModulePath(decodeDiags, inner))
	if decodeDiags.HasErrors() {
		// A module whose own file did not decode has no usable declarations.
		// Recursing would report every consequence of the syntax error as a
		// second, worse-told problem.
		return
	}

	w.path = append(w.path, frame{name: r.Name, source: lm.Source, dir: lm.Dir})
	// The path is the CURRENT branch, not everything visited: popping is what
	// lets one source be instantiated twice on two branches, an ordinary
	// diamond and not a cycle.
	defer func() { w.path = w.path[:len(w.path)-1] }()

	childLevel := moduleLevel(child)
	supplied := w.evaluateCall(r, caller)
	innerScope := w.moduleScope(r, childLevel, caller, supplied, inner)
	w.expand(childLevel, innerScope, lm.Dir, inner)
}

// resolveCall turns a `module.<name>` type into the module it names, applying
// §8c's two shape guards.
func (w *walker) resolveCall(
	r *config.ResourceDecl, loaded map[string]loadedModule, module []string,
) (loadedModule, bool) {
	// The two SHAPE guards Amendment 8c described — `module.` with nothing after
	// it, and a name containing a further `.` — are NOT here. Author A's
	// checkResourceType (stage 2) owns them, and that is the right place:
	// stage 2 holds the line number, and a malformed type is a property of the
	// text. A stage-5 copy would select `module.` by prefix, find an empty or
	// dotted name, and report that some module does not exist — a diagnostic
	// about a CONSEQUENCE, naming a module name the user never wrote
	// (Amendment 13b). Compile halts after decode errors, so a malformed type
	// never reaches here.
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
// KindPath sources are skipped: a local path has no revision to pin, which is
// the same fact 10a leans on when it requires a pin on remote sources and not
// on these. One fact, one consequence, in one place. If Author D wants paths in
// the file too, this condition is the only thing that changes.
//
// Keyed by source identity rather than by the name the module was loaded under:
// one source loaded under two names is one lock entry, and two names for one
// source must not produce two.
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
// Sorted because modules.lock is COMPARED against on every later run — an
// existing entry that differs is an error, never a silent update — so a file
// whose lines reorder between identical runs would report a change that did not
// happen. That is invariant 6 reaching an artifact outside the plan. The map it
// reads from is keyed by source identity and Go's map iteration is randomised,
// so this sort is load-bearing rather than defensive.
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
// included — `alpha (./a) -> beta (../b) -> gamma (../a)` — the shape
// internal/environments and graph.Layers use, so the three cycle errors in this
// system read alike (spec §7.4).
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

// depthDiagnostic is the OTHER failure, and says so in different words. A tree
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
// from — the same fact Ruling 7 requires, that a diagnostic from inside a
// module names the INSTANTIATION (the resource), not the loaded module.
//
// This does the same job internal/diag.Diagnostics.InModule (Task 9) will, but
// is written locally rather than adding that method: Task 9 has not landed yet
// as of this task, and internal/diag is outside the files this task is
// permitted to touch. It builds the same Origin.Module shape
// pkg/address.Address.InModule does — each enclosing name applied so the
// existing (deeper) module path ends up nested inside it — which for the single
// call site here (decodeDiags fresh off config.DecodeModule, Origin.Module
// always empty) simplifies to prepending the full path once. When Task 9 lands,
// this can be replaced with folding ds.InModule(module[i]) from the innermost
// element outward, per that task's contract; the two are equivalent for every
// diagnostic this package produces today.
func inModulePath(ds diag.Diagnostics, module []string) diag.Diagnostics {
	if len(module) == 0 || len(ds) == 0 {
		return ds
	}
	out := make(diag.Diagnostics, len(ds))
	for i, d := range ds {
		next := make([]string, 0, len(module)+len(d.Origin.Module))
		next = append(next, module...)
		next = append(next, d.Origin.Module...)
		d.Origin.Module = next
		out[i] = d
	}
	return out
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

// where names a level for a diagnostic. Ruling 7: an error message's only way
// to say which instantiation a problem came from is the module path.
func where(module []string) string {
	if len(module) == 0 {
		return "the project"
	}
	return "module " + strings.Join(module, ".")
}
