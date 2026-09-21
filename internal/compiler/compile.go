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

// Compile runs the full compiler pipeline — decode, reference binding, schema
// binding, and whole-graph validation — accumulating diagnostics from whichever
// stages run.
//
// Diagnostics collect within a stage: one bad resource does not stop that stage
// reporting its siblings' problems too. But a stage that could not build a usable
// ResolvedConfig must not hand it to the next one — a stage-7 kind check against
// a resource whose type never resolved produces noise, not signal — so Compile
// stops at the first stage boundary where HasErrors() is true, returning
// everything collected up to and including that stage.
//
// Contract: the returned ResolvedConfig is meaningless if the returned
// diagnostics contain errors. Check HasErrors() before touching it. On an early
// return it holds whatever the last completed stage built — some resources fully
// resolved, others never given defaults or checked for required attributes — and
// nothing in it distinguishes the two. Nothing panics; it simply looks valid.
// `cfg, _ := Compile(...)` is always a bug.
//
// From stage 6 onward the partial config is returned deliberately rather than a
// zero value, because an empty ResolvedConfig is the more dangerous shape: zero
// desired resources diffed against a populated state file reads as "removed from
// configuration", and describes a plan destroying every managed resource. Decode
// and the variable stages cannot do this — they fail before any ResolvedConfig
// exists — so in the syntax-error case the caller's own HasErrors() check is the
// only thing between a malformed file and a destroy-everything plan.
//
// A variable that is merely not set does not halt the compile. Stage 4 binds it
// to an unknown, exactly as it already does for every environment-scoped variable
// when no environment is selected, so an unrelated mistake elsewhere in the
// project is reported in the same run rather than one run later. The run still
// fails on the variable's own error. Stage 4 does halt when the scope it built is
// unusable — a malformed declaration, an unparseable --var, a value that failed
// its own type — because those leave a name with no schema and stage 6 would
// report the consequence at every use site rather than the cause.
func Compile(files []config.File, reg *registry.Registry, opts Options) (ResolvedConfig, diag.Diagnostics) {
	var ds diag.Diagnostics

	stage, stageDS := VariableScope(files, opts)
	ds.Extend(stageDS)
	if stageDS.HasErrors() && !stage.Usable {
		return ResolvedConfig{}, ds
	}
	project, chain, scope := stage.Project, stage.Chain, stage.Scope
	fileValues, varDiags := stage.FileValues, stage.VarDiags

	// Stage 4.5: the provider instances. After variables, because an instance's
	// configuration interpolates them — `cloud: ${var.path}` — and before stage 5,
	// because expansion needs to know which instance a resource belongs to in
	// order to inherit it down a module call.
	//
	// This is also where the registry stops being schemas-only: Prepare constructs
	// each instance's provider object from the values just resolved.
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
	// `skip`/`only` and refuse a name nothing declares.
	env := modules.Env{Name: opts.Environment, Declared: declaredEnvironments(project)}
	expansion, moduleDiags := modules.Expand(project, scope, dirScopes, env, opts.Dir, source.NewCache(opts.Dir), opts.Secrets, opts.SecretsErr, opts.Templates)
	ds.Extend(moduleDiags)
	if moduleDiags.HasErrors() {
		// This halt suppresses all of stage 6, including diagnostics with nothing
		// to do with modules, and that is deliberate. After a failed expansion the
		// resource set stage 6 would walk is not the user's configuration: every
		// reference into the module that failed reports "no such resource", noise
		// proportional to the size of the module rather than of the mistake.
		return ResolvedConfig{}, ds
	}

	// Stage 5.5: plugins only the expanded configuration reveals.
	//
	// A provider used solely inside a module is invisible to stage 4.5, because
	// that pass reads the types the root declares and a `module.` call is not one
	// of them. Module files are not read until the expansion just above, so this
	// is the first moment the full set of types exists.
	//
	// Silent about a plugin it cannot load, because stage 7 raises that with the
	// resource, the origin and the action attached.
	expandedTypes := make([]string, 0, len(expansion.Instances))
	for _, inst := range expansion.Instances {
		expandedTypes = append(expandedTypes, inst.Decl.Type)
	}
	table, lateDiags := providers.PrepareLate(opts.Context(), expandedTypes, table, reg)
	ds.Extend(lateDiags)
	if lateDiags.HasErrors() {
		return ResolvedConfig{}, ds
	}

	// The lockfile comparison is a pure read, which is what lets `validate`
	// report a moved tag without writing anything. Stage 5 collects resolutions;
	// only a command permitted to mutate the project directory writes
	// modules.lock, after a successful walk.
	//
	// Wired here rather than inside stage 5 because internal/modules must not read
	// the project directory for anything but modules, and because a comparison
	// that ran during the walk could not see the complete set.
	//
	// The halt below is on this check's own diagnostics rather than everything
	// accumulated so far, like every other halt in this function: an error carried
	// forward from an earlier stage would otherwise suppress stage 6 for a reason
	// that has nothing to do with lockfiles.
	var lockDS diag.Diagnostics
	if lf, lockDiags := source.LoadLockfile(opts.Dir); !lockDiags.HasErrors() {
		for _, r := range expansion.Resolutions {
			rec, ok := source.Pin(r.Source, r.Resolution)
			if !ok {
				continue // a path source has no revision to pin
			}
			lockDS.Extend(lf.Check(rec, r.Source.Origin))
		}
	} else {
		lockDS.Extend(lockDiags)
	}
	ds.Extend(lockDS)
	if lockDS.HasErrors() {
		return ResolvedConfig{}, ds
	}

	if opts.RecordLocks {
		// The complete set, in one call, after the walk succeeded. Writing per
		// resolution would leave a file recording half a walk as though it were
		// whole. Writing the whole set is also the prune: a source the
		// configuration no longer names is simply absent from what is written.
		recs := make([]source.Record, 0, len(expansion.Resolutions))
		for _, r := range expansion.Resolutions {
			if rec, ok := source.Pin(r.Source, r.Resolution); ok {
				recs = append(recs, rec)
			}
		}
		writeDS := source.WriteLockfile(opts.Dir, recs)
		ds.Extend(writeDS)
		if writeDS.HasErrors() {
			return ResolvedConfig{}, ds
		}
	}

	cfg, bindDiags := bindReferences(expansion, opts, reg, table)
	cfg.Protections = Protections{
		RequireApproval:     chain.RequireApproval,
		RequireApprovalFrom: chain.RequireApprovalFrom,
		PreventDestroy:      chain.PreventDestroy,
		PreventDestroyFrom:  chain.PreventDestroyFrom,
	}
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

// notARelease reports whether v cannot be a version anyone shipped: 0.0.0, or a
// version carrying a pre-release suffix.
//
// A development build must be exempt from a project's `infrena:` floor: it
// satisfies no floor at all, so every build from a checkout would otherwise
// refuse every project that states one. The floor exists to stop a released
// binary quietly misreading a project written for a later one.
//
// Checking for zero alone is not enough once the repository has a release tag. A
// pseudo-version bases itself on the nearest reachable tag plus one patch, so an
// untagged checkout reports something like
// `0.4.1-0.20260914210715-b41497c99237+dirty`. The suffix is what marks it: a
// release stamps a bare MAJOR.MINOR.PATCH, and semver.Parse only fills Pre when a
// `-` precedes the suffix, so a bare `+meta` version such as `0.5.0+dirty` is
// still floor-checked.
func notARelease(v semver.Version) bool {
	return (v.Major == 0 && v.Minor == 0 && v.Patch == 0) || v.Pre != ""
}

// developmentVersion is what a build with no version information calls itself.
// Stated here rather than imported so that internal/compiler does not depend on
// internal/version at all; the one place it is produced has the test that pins
// the spelling.
const developmentVersion = "0.0.0-dev"

// checkRequiredVersion enforces the optional `infrena:` floor a project states
// against the running build. current is passed in rather than read from
// internal/version, so this is a pure function of its inputs and a test can pin a
// build version.
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
		// Neither an unparseable version nor a non-release one is something a user
		// can act on a complaint about.
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

// VariableStage is what stages 1-4 produce: the decoded project, the environment
// chain, and the variable scope everything later interpolates against.
type VariableStage struct {
	Project *config.ProjectDecl
	Chain   environments.Chain
	Scope   variables.Scope

	// FileValues is the file rung of the precedence chain: variables.yml and any
	// --var-file, merged. Kept because directoryScopes re-runs stage 4 per
	// resources directory and must put the same rungs back underneath that
	// directory's own values.
	FileValues map[string]value.Value

	// VarDiags is what the project-wide stage 4 reported, and it is the
	// subtraction base for each directory's re-run: every rung but the
	// directory's own is identical between them, so without this a single
	// project-wide mistake is reported once per directory. See directoryScopes.
	VarDiags diag.Diagnostics

	// Usable reports that Scope is complete enough for the later stages to run
	// against, even when this stage reported errors.
	//
	// True when every error was a properly declared variable that simply has no
	// value: stage 4 binds those to unknowns, which the rest of the pipeline
	// already handles. False for anything that leaves a name without a usable
	// schema — a malformed declaration, an unparseable --var, a value that failed
	// its own type — and false for every early return here, where no scope was
	// built at all.
	//
	// A caller that honours it reports the variable's own diagnostic and
	// everything independently wrong further down the file in one run.
	Usable bool
}

// VariableScope runs stages 1 to 4 and stops there: decode, the `infrena:` floor,
// the environment chain, and the variables.
//
// Exported for the commands that never compile. `discover`, `import`, `refresh`
// and `destroy` still have to resolve `providers:`, whose configuration
// interpolates variables, and that needs no registry, no plugins, no resources
// and no modules.
//
// Shared with Compile rather than reimplemented: the precedence ladder is the
// product's rule, and a second copy of it would drift from the first in exactly
// the way a user could not predict.
//
// An empty opts.Environment is valid and deliberate. It yields an unselected
// chain, so a variable only an environment sets becomes an unknown rather than an
// error — which is what lets `discover`, which has no environment, resolve
// everything that does not depend on one. A caller that cannot use an unknown
// must say so itself.
func VariableScope(files []config.File, opts Options) (VariableStage, diag.Diagnostics) {
	var ds diag.Diagnostics

	project, decodeDiags := config.Decode(files)
	ds.Extend(decodeDiags)
	if decodeDiags.HasErrors() {
		return VariableStage{}, ds
	}

	// Before anything else runs. A binary that cannot understand this project must
	// say so once, rather than reporting twenty unknown-key diagnostics that are
	// all the same problem said badly.
	if versionDiags := checkRequiredVersion(project, opts.Version); versionDiags.HasErrors() {
		ds.Extend(versionDiags)
		return VariableStage{}, ds
	}

	chain, envDiags := environments.Resolve(project.Environments, opts.Environment)
	ds.Extend(envDiags)
	if envDiags.HasErrors() {
		// A broken chain does not by itself make stage 4 noisier: an unselected
		// chain already resolves an unset environment-scoped variable to an
		// unknown rather than an error. What this halt suppresses is stage 4's
		// chain-independent checking — a malformed declaration or a malformed
		// --var — which would otherwise be reported alongside a diagnostic
		// already explaining why the chain itself failed.
		return VariableStage{Project: project}, ds
	}

	fileValues := fileVars(project, opts)
	scope, varDiags, usable := variables.Resolve(project.Variables, chain, fileValues, nil, opts.Vars)
	ds.Extend(varDiags)
	seedProcessVariables(&scope, project.Project, opts)
	if varDiags.HasErrors() && !usable {
		// A malformed declaration or an unparseable --var leaves a name with no
		// usable schema at all, and every use site would report the consequence
		// rather than the cause.
		//
		// A variable that is simply not set is different: it binds to an unknown,
		// which is a complete and honest answer for the later stages to compile
		// against, so the run continues and every independent problem in the
		// project is reported alongside it. The run still fails — the diagnostic
		// is an error and ds carries it out — so no caller reaches a plan on the
		// strength of an unknown.
		return VariableStage{Project: project, Chain: chain}, ds
	}

	return VariableStage{
		Project: project, Chain: chain, Scope: scope,
		FileValues: fileValues, VarDiags: varDiags,
		Usable: usable,
	}, ds
}

// fileVars merges --var-file entries over variables.yml's.
//
// The two arrive as separate fields because they are separate things — one a
// decoded project file, the other a command-line input — and they merge only
// here, at the one rung of the precedence chain they share. The file named on the
// command line is the more specific of the two, so it wins.
func fileVars(project *config.ProjectDecl, opts Options) map[string]value.Value {
	out := make(map[string]value.Value, len(project.VariableValues)+len(opts.FileVars))
	maps.Copy(out, project.VariableValues)
	maps.Copy(out, opts.FileVars)
	return out
}

// directoryScopes runs stage 4 once more for each resources directory that
// declares its own vars/, with that directory's values on the directory rung.
//
// One run per directory rather than one ladder that branches, because the ladder
// is the precedence rule and there must be exactly one of it. A directory's scope
// differs from the project's only in what sits on the directory rung, but every
// rung above it has to be re-applied on top: without that, a --var would stop
// winning inside any directory that happened to set the same name, and no file
// outranks a --var.
//
// baseDiags is what the project-wide run already reported, and it is subtracted
// from each directory's run. Every other rung is identical between the runs, so a
// single project-wide mistake would otherwise be reported once per directory that
// declares any variable at all. What survives the subtraction is what this
// directory's own files caused: an identical message is the same mistake told
// again, and a differing one is a different mistake.
//
// Returns nil when no directory declares variables, keeping the feature off the
// path of a project that does not use it.
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
	// Sorted: the diagnostics below are emitted in this order, Go randomises map
	// iteration, and two runs of one configuration must report the same problems
	// in the same order.
	for _, dir := range sortedDirs(project.ScopedValues) {
		scope, dirDiags, _ := variables.Resolve(project.Variables, chain, files, project.ScopedValues[dir], opts.Vars)
		for _, d := range dirDiags {
			if !reported[diagKey(d)] {
				reported[diagKey(d)] = true
				ds.Add(d)
			}
		}
		// The process variables are authoritative in every scope, not just the
		// project-wide one: a ${var.environment} that resolved inside
		// resources/db/ and nowhere else would be worse than one that resolved
		// nowhere.
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
// project that declares none, where `skip`/`only` names go unchecked.
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

// seedProcessVariables adds the variables that come from the process invocation
// rather than from any file.
//
// Both are unconditional and authoritative. Configuration names its own
// environment — `name: ${var.project_name}-${var.environment}` — so a --var able
// to change `environment` would produce resource names claiming one environment
// while the plan changed another. `project` is the one that comes from
// configuration rather than the command line, which is why it is passed in rather
// than read off Options: Options is the invocation, and widening it with a value
// decoded from a file would make two things the source of one fact.
//
// They carry SourceEnvironment because they are facts about the environment being
// planned rather than variables anyone declared, and ScopeCLIOverride because the
// command line is where they enter the process. Each is also stamped with
// SuppliedBy, because the label otherwise falls back to the scope's own name and
// a plan would credit a `--var` the user never typed for a value a --var cannot
// set.
func seedProcessVariables(scope *variables.Scope, project string, opts Options) {
	origin := value.Origin{File: "<command line>"}
	set := func(name, text, suppliedBy string) {
		scope.Override(name, value.String(text, value.SourceEnvironment).
			WithScope(value.ScopeCLIOverride).WithOrigin(origin).WithSuppliedBy(suppliedBy))
	}
	set("environment", opts.Environment, "the environment argument")
	set("project", project, "the project name")
}
