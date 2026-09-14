package compiler

import (
	"maps"
	"sort"
	"strconv"

	"github.com/infrena/infrena/internal/config"
	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/internal/environments"
	"github.com/infrena/infrena/internal/modules"
	"github.com/infrena/infrena/internal/modules/source"
	"github.com/infrena/infrena/internal/providers"
	"github.com/infrena/infrena/internal/registry"
	"github.com/infrena/infrena/internal/variables"
	"github.com/infrena/infrena/pkg/semver"
	"github.com/infrena/infrena/pkg/value"
)

// Compile runs the full compiler pipeline — decode, reference binding,
// schema binding, and whole-graph validation — accumulating diagnostics from
// whichever stages run.
//
// Diagnostics still collect within a stage: one bad resource does not stop
// that stage from reporting its siblings' problems too. But a stage that
// could not build a usable ResolvedConfig must not hand it to the next one —
// a stage-7 kind check against a resource whose type was never resolved
// produces noise, not signal, and the same is true of stage 8 reasoning about
// a graph that stage 6 or 7 could not finish. Compile therefore stops at the
// first stage boundary where HasErrors() is true, returning everything
// collected up to and including that stage.
//
// CONTRACT: the returned ResolvedConfig is meaningless if the returned
// diagnostics contain errors. Check HasErrors() before touching it. On an
// early return it holds whatever the last completed stage built — some
// resources fully resolved, others never given defaults or checked for
// required attributes — and nothing in it distinguishes the two. Nothing
// panics; it simply looks valid. `cfg, _ := Compile(...)` is always a bug.
//
// From stage 6 onward the partial config is returned deliberately rather
// than a zero value, because an empty ResolvedConfig is the more dangerous
// shape: zero desired resources diffed against a populated state file is
// exactly what invariant 1 reads as "removed from configuration", so it
// would describe a plan destroying every managed resource. A partial config
// degrades one resource at a time; an empty one degrades the whole
// environment at once.
//
// Decode is the exception, and it cannot be otherwise: it fails before any
// ResolvedConfig exists, so there is nothing partial to return and the zero
// value is all there is. That is precisely the syntax-error case, so the
// paragraph above buys NO defence in depth there — the caller's HasErrors()
// check is the only thing standing between a malformed file and a
// destroy-everything plan. Both callers make it. A third must too.
//
// Stages 3 (environments.Resolve) and 4 (variables.Resolve) are two more such
// exceptions, and for the same reason as Decode: neither builds a
// ResolvedConfig, so a failure there has nothing partial to hand forward
// either.
//
// Stage 3 failing means the environment chain itself could not be
// established — extending an unknown environment or a cycle in `extends`.
// Stage 4 still RUNS in that case (a failed Chain carries Selected: false,
// and stage 4 already treats an unselected chain the same way it treats
// `infra validate` having no environment argument at all: an unset
// environment-scoped variable becomes an unknown rather than an error, so a
// broken chain by itself adds no further diagnostics). What the halt
// actually guards is stage 4's CHAIN-INDEPENDENT checking — a malformed
// variable declaration (variables.Schemas) or a malformed --var (ParseText)
// — which runs unconditionally and would otherwise be reported ALONGSIDE
// the chain failure. That is a deliberate stage-boundary policy choice, in
// the same spirit as Decode's own halt and in tension with §7.4's
// "diagnostics collect, don't fail fast": a user who has both a cycle in
// `extends` and a malformed variable default fixes the cycle, re-runs, and
// only then learns about the default. Two independent problems reported one
// stage-boundary at a time, rather than both at once, is judged the lesser
// confusion here because a diagnostic ABOUT a chain that never resolved
// (the malformed default's context is "resolving environment X", and X's
// own chain is broken) risks reading as caused by the very failure that
// already explains itself.
//
// Stage 4 failing outright means one or more declared variables could not be
// resolved to a value; stage 6 would otherwise report `undefined variable`
// again at every use site, the same problem told worse.
func Compile(files []config.File, reg *registry.Registry, opts Options) (ResolvedConfig, diag.Diagnostics) {
	var ds diag.Diagnostics

	stage, stageDS := VariableScope(files, opts)
	ds.Extend(stageDS)
	if stageDS.HasErrors() {
		return ResolvedConfig{}, ds
	}
	project, chain, scope := stage.Project, stage.Chain, stage.Scope
	fileValues, varDiags := stage.FileValues, stage.VarDiags

	// Stage 4.5: the provider instances. AFTER variables, because an instance's
	// configuration interpolates them — `cloud: ${var.path}`, `iam-role: ${var.role}` —
	// and BEFORE stage 5, because expansion needs to know which instance a
	// resource belongs to in order to inherit it down a module call.
	//
	// This is also where the registry stops being schemas-only: Prepare constructs
	// each instance's provider object from the values just resolved, which is the
	// whole reason the plugin/provider split exists (PLAN.md §12.1).
	table, provDiags := providers.Prepare(opts.Context(), project, scope, reg)
	ds.Extend(provDiags)
	if provDiags.HasErrors() {
		// An instance that could not be configured cannot be dispatched to, and
		// every resource belonging to it would report the same thing again. Worse,
		// a resource whose instance silently vanished is one stage 6 would hand a
		// different instance — the default — and a resource created in the wrong
		// account is not a diagnostic anybody gets to read.
		return ResolvedConfig{}, ds
	}

	dirScopes, dirDiags := directoryScopes(project, chain, opts, fileValues, varDiags)
	ds.Extend(dirDiags)
	if dirDiags.HasErrors() {
		// Same reason as the project-wide run above: a directory whose own
		// variable is out of bounds would otherwise be reported once here and
		// again at every use site inside that directory.
		return ResolvedConfig{}, ds
	}

	// The environment, and every environment declared, so stage 5 can resolve
	// `skip`/`only` and refuse a name nothing declares (PLAN.md §6.2).
	env := modules.Env{Name: opts.Environment, Declared: declaredEnvironments(project)}
	expansion, moduleDiags := modules.Expand(project, scope, dirScopes, env, opts.Dir, source.NewCache(opts.Dir))
	ds.Extend(moduleDiags)
	if moduleDiags.HasErrors() {
		// This halt suppresses ALL of stage 6, including diagnostics with
		// nothing to do with modules: a root resource referring to a
		// nonexistent root resource is reported only on the next run. That is
		// deliberate and is the same trade stages 3 and 4 make. After a failed
		// expansion the resource set stage 6 would walk is not the user's
		// configuration — every reference into the module that failed to expand
		// reports "no such resource", one diagnostic per reference, noise
		// proportional to the size of the module rather than to the size of the
		// mistake. TestCompileStopsAfterModuleErrors pins the suppression so it
		// stays a decision rather than an accident.
		return ResolvedConfig{}, ds
	}

	// The lockfile comparison is a PURE READ, which is what lets `validate`
	// report a moved tag without writing anything (Amendment 18b). Stage 5
	// COLLECTS resolutions; nothing here records them — a command permitted to
	// mutate the project directory writes modules.lock after a successful walk.
	//
	// Wired here rather than inside stage 5 because internal/modules must not
	// read the project directory for anything but modules, and because a
	// comparison that ran during the walk could not see the complete set.
	if lf, lockDiags := source.LoadLockfile(opts.Dir); !lockDiags.HasErrors() {
		for _, r := range expansion.Resolutions {
			rec, ok := source.Pin(r.Source, r.Resolution)
			if !ok {
				continue // a path source has no revision to pin
			}
			ds.Extend(lf.Check(rec, r.Source.Origin))
		}
	} else {
		ds.Extend(lockDiags)
	}
	if ds.HasErrors() {
		return ResolvedConfig{}, ds
	}

	if opts.RecordLocks {
		// The COMPLETE set, in one call, after the walk succeeded. Writing per
		// resolution would leave a file recording half a walk as though it were
		// whole, which is the shape ae1e309 fixed once already. Writing the
		// whole set IS the prune: a source the configuration no longer names is
		// simply absent from what is written.
		recs := make([]source.Record, 0, len(expansion.Resolutions))
		for _, r := range expansion.Resolutions {
			if rec, ok := source.Pin(r.Source, r.Resolution); ok {
				recs = append(recs, rec)
			}
		}
		ds.Extend(source.WriteLockfile(opts.Dir, recs))
		if ds.HasErrors() {
			return ResolvedConfig{}, ds
		}
	}

	cfg, bindDiags := bindReferences(expansion, opts, reg, table)
	ds.Extend(bindDiags)
	if bindDiags.HasErrors() {
		return cfg, ds
	}

	schemaDiags := bindSchemas(&cfg, reg, opts, table)
	ds.Extend(schemaDiags)
	if schemaDiags.HasErrors() {
		return cfg, ds
	}

	validateDiags := validateGraph(&cfg, reg)
	ds.Extend(validateDiags)

	return cfg, ds
}

// checkRequiredVersion enforces the optional `infrena:` floor a project states.
//
// A DEVELOPMENT BUILD IS EXEMPT, deliberately. It reports 0.0.0-dev, which satisfies
// no floor at all, so every `go build` from a checkout would refuse every project that
// states one — including this repository's own fixtures. The constraint exists to stop
// a RELEASED binary quietly misreading a project written for a later one; a developer
// running their own build has not made that mistake.
//
// notARelease reports whether v cannot be a version anyone shipped: 0.0.0, or a
// version carrying a pre-release/build suffix.
//
// The zero check alone USED TO BE ENOUGH: before this repository had any release tag,
// `go build` in a checkout with a VCS remote reported a pseudo-version that parsed as
// 0.0.0, so checking for zero caught every development build. It stopped being enough
// the moment the first tag (v0.1.0) was pushed — a pseudo-version bases itself on the
// nearest reachable tag plus one patch, so the same untagged checkout now reports
// something like `0.4.1-0.20260914210715-b41497c99237+dirty`, which is not zero and
// so slipped past this check entirely, letting a project's floor refuse a developer's
// own build (found via `infrena: ">= 0.5"` in a freshly scaffolded project, 2026-09-14).
// A suffix is what still marks it: the release workflow stamps a bare MAJOR.MINOR.PATCH
// with `-ldflags -X`, so anything carrying a `-` (semver.Parse folds a trailing `+meta`
// into the same field) is, whatever numbers it landed on, not that.
func notARelease(v semver.Version) bool {
	return (v.Major == 0 && v.Minor == 0 && v.Patch == 0) || v.Pre != ""
}

// developmentVersion is what a build with no version information calls itself. Stated
// here rather than imported so that internal/compiler does not depend on
// internal/version at all — the value is a string on the wire between them, and the
// one place it is produced (internal/version) has the test that pins the spelling.
const developmentVersion = "0.0.0-dev"

// current is passed in rather than read from internal/version, so this is a pure
// function of its inputs — which is what lets a test pin a build version without a
// test-only export on a package the compiler depends on.
func checkRequiredVersion(project *config.ProjectDecl, current string) diag.Diagnostics {
	var ds diag.Diagnostics
	if project.RequiredVersion.IsZero() {
		return ds
	}
	if current == "" || current == developmentVersion {
		return ds
	}

	running, err := semver.Parse(current)
	if err != nil || notARelease(running) {
		// Neither an unparseable version nor a non-release one is something a user can
		// act on a complaint about — see notARelease for why zero alone is not enough
		// to catch it.
		return ds
	}
	if project.RequiredVersion.Allows(running) {
		return ds
	}
	ds.Add(diag.Diagnostic{
		Severity: diag.SeverityError,
		Summary: "this project requires infrena " + project.RequiredVersion.String() +
			", and this is " + current,
		Detail: "`infrena:` in the configuration states which versions of the tool the " +
			"project is known to work with. Running an older one risks misreading syntax it " +
			"does not have; running a newer one may be fine, and the constraint can be " +
			"widened to say so.",
		Action: "Upgrade infrena, or widen `infrena:` once you have confirmed this version works.",
		Origin: project.RequiredVersionOrigin,
	})
	return ds
}

// fileVars merges --var-file entries over variables.yml's.
//
// The two arrive as separate fields because they are separate things — one is
// a decoded project file, the other a command-line input — and they merge only
// here, at the one rung of the precedence chain they share. The file named on
// the command line is the more specific of the two, so it wins.
// VariableStage is what stages 1-4 produce: the decoded project, the environment
// chain, and the variable scope everything later interpolates against.
type VariableStage struct {
	Project *config.ProjectDecl
	Chain   environments.Chain
	Scope   variables.Scope

	// FileValues is §7's file rung: variables.yml and any --var-file, merged. Kept
	// because directoryScopes re-runs stage 4 per resources directory and must put
	// the same rungs back underneath that directory's own values.
	FileValues map[string]value.Value

	// VarDiags is what the project-wide stage 4 reported, and it is the SUBTRACTION
	// BASE for each directory's re-run: every rung but the directory's own is
	// identical between them, so without this a single project-wide mistake is
	// reported once per directory. See directoryScopes.
	VarDiags diag.Diagnostics
}

// VariableScope runs stages 1 to 4 and stops there: decode, the `infrena:` floor,
// the environment chain, and the variables.
//
// EXPORTED FOR THE COMMANDS THAT NEVER COMPILE. `discover`, `import`, `refresh` and
// `destroy` still have to resolve `providers:`, whose configuration interpolates
// variables — and resolving variables needs none of the rest of a compile. It needs
// no registry, no plugins, no resources and no modules, which is why this can be
// lifted out and why it was wrong to conclude those commands had no scope available
// (PLAN.md §12.1, amended).
//
// Shared with Compile rather than reimplemented: the precedence ladder IS the
// product's rule (§7), and a second copy of it would drift from the first in exactly
// the way a user could not predict.
//
// An empty opts.Environment is valid and deliberate. It yields an unselected Chain,
// so a variable only an environment sets becomes an UNKNOWN rather than an error —
// which is what lets `discover`, which has no environment, still resolve everything
// that does not depend on one. A caller that cannot use an unknown must say so
// itself; this function does not know which of its callers those are.
func VariableScope(files []config.File, opts Options) (VariableStage, diag.Diagnostics) {
	var ds diag.Diagnostics

	project, decodeDiags := config.Decode(files)
	ds.Extend(decodeDiags)
	if decodeDiags.HasErrors() {
		return VariableStage{}, ds
	}

	// BEFORE ANYTHING ELSE RUNS. A binary that cannot understand this project must
	// say so once, rather than reporting twenty unknown-key diagnostics that are all
	// the same problem said badly (PLAN.md §61.2).
	if versionDiags := checkRequiredVersion(project, opts.Version); versionDiags.HasErrors() {
		ds.Extend(versionDiags)
		return VariableStage{}, ds
	}

	chain, envDiags := environments.Resolve(project.Environments, opts.Environment)
	ds.Extend(envDiags)
	if envDiags.HasErrors() {
		// A broken chain does not by itself make stage 4 noisier — an
		// unselected Chain already resolves an unset environment-scoped
		// variable to an unknown rather than an error (see Compile's doc
		// comment). What this halt actually suppresses is stage 4's
		// chain-independent checking (a malformed variable declaration or
		// a malformed --var), which would otherwise be reported alongside
		// a diagnostic already explaining why the chain itself failed.
		return VariableStage{Project: project}, ds
	}

	fileValues := fileVars(project, opts)
	scope, varDiags := variables.Resolve(project.Variables, chain, fileValues, nil, opts.Vars)
	ds.Extend(varDiags)
	seedProcessVariables(&scope, project.Project, opts)
	if varDiags.HasErrors() {
		// Stage 6 would report `undefined variable` for each of the same
		// names at each use site — the same problem told twice, with the
		// second telling less informative than the first.
		return VariableStage{Project: project, Chain: chain}, ds
	}

	return VariableStage{
		Project: project, Chain: chain, Scope: scope,
		FileValues: fileValues, VarDiags: varDiags,
	}, ds
}

func fileVars(project *config.ProjectDecl, opts Options) map[string]value.Value {
	out := make(map[string]value.Value, len(project.VariableValues)+len(opts.FileVars))
	maps.Copy(out, project.VariableValues)
	maps.Copy(out, opts.FileVars)
	return out
}

// directoryScopes runs stage 4 once more for each resources directory that
// declares its own vars/ (PLAN.md §4.1), with that directory's values on §7's
// directory rung.
//
// ONE RUN PER DIRECTORY rather than one ladder that branches, because the
// ladder IS the precedence rule and there must be exactly one of it (spec
// §7.1). A directory's scope differs from the project's only in what sits on
// rung 2.5, but every rung ABOVE it has to be re-applied on top: without that,
// a --var would stop winning inside any directory that happened to set the
// same name, which is the one rung PLAN.md §7 says no file outranks.
//
// baseDiags is what the project-wide run already reported, and it is
// SUBTRACTED from each directory's run. Every rung but the directory's own is
// identical between the runs — same declarations, same variables.yml, same
// environment chain, same command line — so a single project-wide mistake
// would otherwise be reported once per directory that declares any variable at
// all. What survives the subtraction is exactly what this directory's own files
// caused: an identical message is the same mistake told again, and a message
// that differs (the bound violated by a different value, from a different file)
// is a different one.
//
// Returns nil when no directory declares variables, which is the common case
// and keeps the whole feature off the path of a project that does not use it.
func directoryScopes(
	project *config.ProjectDecl, chain environments.Chain, opts Options,
	files map[string]value.Value, baseDiags diag.Diagnostics,
) (map[string]variables.Scope, diag.Diagnostics) {
	if len(project.ScopedValues) == 0 {
		return nil, nil
	}

	reported := make(map[string]bool, len(baseDiags))
	for _, d := range baseDiags {
		reported[diagKey(d)] = true
	}

	var ds diag.Diagnostics
	out := make(map[string]variables.Scope, len(project.ScopedValues))
	// Sorted: the diagnostics below are emitted in this order, and Go's map
	// iteration is randomised. Two runs of one configuration must report the
	// same problems in the same order (invariant 6).
	for _, dir := range sortedDirs(project.ScopedValues) {
		scope, dirDiags := variables.Resolve(project.Variables, chain, files, project.ScopedValues[dir], opts.Vars)
		for _, d := range dirDiags {
			if !reported[diagKey(d)] {
				reported[diagKey(d)] = true
				ds.Add(d)
			}
		}
		// The three process variables are authoritative in every scope, not
		// just the project-wide one — a ${var.environment} that resolved inside
		// resources/db/ and nowhere else would be worse than one that resolved
		// nowhere. See seedProcessVariables.
		seedProcessVariables(&scope, project.Project, opts)
		out[dir] = scope
	}
	return out, ds
}

// diagKey identifies a diagnostic by everything a reader would see of it.
// value.Origin holds a slice, so diag.Diagnostic is not comparable and cannot
// be a map key itself.
func diagKey(d diag.Diagnostic) string {
	return strconv.Itoa(int(d.Severity)) + "\x00" + d.Summary + "\x00" + d.Detail +
		"\x00" + d.Action + "\x00" + d.Origin.String()
}

// declaredEnvironments lists every declared environment, sorted. Empty for a
// project that declares none, which is the case §6.2 leaves unchecked.
func declaredEnvironments(p *config.ProjectDecl) []string {
	out := make([]string, 0, len(p.Environments))
	for _, e := range p.Environments {
		out = append(out, e.Name)
	}
	sort.Strings(out)
	return out
}

func sortedDirs(m map[string]map[string]value.Value) []string {
	out := make([]string, 0, len(m))
	for dir := range m {
		out = append(out, dir)
	}
	sort.Strings(out)
	return out
}

// seedProcessVariables adds the variables that come from the process
// invocation rather than from any file.
//
// `environment` is unconditional and authoritative: configuration names its
// own environment (PLAN.md §10's example is `name: ${var.project_name}-${var.environment}`),
// and a --var that could change it would produce resource names claiming one
// environment while the plan changed another.
//
// `project` is unconditional for the same reason and from the same place the
// plan header reads it: PLAN.md §6.3. It is the one of the four that comes
// from CONFIGURATION rather than from the command line, which is why it is
// passed in rather than read off Options — Options is the invocation, and
// widening it with a value decoded from a file would make two things the
// source of one fact.
//
// `region` and `account` are added only when supplied. Injecting an empty
// string instead would interpolate silently into a resource name; leaving
// them undefined makes stage 6 say so, which the user can act on.
//
// They carry SourceEnvironment because they are facts about the environment
// being planned rather than variables anyone declared, and ScopeCLIOverride
// because the command line is where they enter the process. Source and Scope
// are orthogonal: one says what kind of thing a value is, the other says
// which precedence level supplied it.
//
// Each one is also stamped with SuppliedBy (M4 final review, MAJOR 2). Left
// unset, value.ScopeLabel falls back to Scope.String() — "--var" — so a bare
// `infra plan dev` with no flags at all rendered `cidr: "dev" [environment,
// from --var]`, crediting a flag the user did not type, and this function's
// own doc comment above argues at length that a --var CANNOT set
// "environment". The plan asserted the opposite of the code's contract.
// SuppliedBy is free text at this rung (see its doc comment), so it says what
// actually supplied the value: the environment argument to the command
// itself, not a flag.
//
// TWO variables, not four. "region" and "account" were seeded here from Options fields
// that nothing ever assigned, so they were never in scope and `${var.region}` was an
// undefined variable in every project that ever ran. Removed 2026-09-13; see
// variables.processReservedNames for what that cost while it stood.
func seedProcessVariables(scope *variables.Scope, project string, opts Options) {
	origin := value.Origin{File: "<command line>"}
	set := func(name, text, suppliedBy string) {
		scope.Override(name, value.String(text, value.SourceEnvironment).
			WithScope(value.ScopeCLIOverride).WithOrigin(origin).WithSuppliedBy(suppliedBy))
	}
	set("environment", opts.Environment, "the environment argument")
	set("project", project, "the project name")
}
