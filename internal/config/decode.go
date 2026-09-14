package config

import (
	"maps"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/pkg/semver"
	"github.com/infrena/infrena/pkg/value"
)

// Decode converts parsed YAML into typed declarations, collecting every problem
// rather than stopping at the first.
func Decode(files []File) (*ProjectDecl, diag.Diagnostics) {
	var ds diag.Diagnostics
	out := &ProjectDecl{VariableValues: map[string]value.Value{}, ScopedValues: map[string]map[string]value.Value{}}

	// Uniqueness is tracked across the whole decode rather than per file: spec
	// §5.2 requires logical names be unique within a module, and M4 adds more
	// files to the same root module.
	//
	// Variables, environments and resources get SEPARATE sets. They are
	// separate namespaces: `${var.db}` is a variable reference and `${db.host}` a
	// resource reference, and stage 6 already tells them apart.
	seenResources := map[string]value.Origin{}
	seenVariables := map[string]value.Origin{}
	// seenEnvironments maps an environment name to its index in
	// out.Environments, because the block and the file MERGE into one decl.
	seenEnvironments := map[string]int{}
	// seenModules maps a module's NAME to the entry that claimed it, so a
	// collision names both sources. It is separate from seenResources because
	// they are separate namespaces: a module name is only ever half of a
	// resource type (`module.app`), never a bare name, so a module called `app`
	// and a resource called `app` do not collide.
	seenModules := map[string]loadedName{}

	for _, f := range files {
		doc := documentRoot(f.Root)
		if doc == nil {
			// Only the project file must have content. An empty variables.yml
			// or an empty environment file declares nothing, which is a
			// perfectly ordinary state for a file a user has just created.
			if f.Kind != FileProject {
				continue
			}
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "configuration file is empty",
				Origin:   value.Origin{File: f.Path},
				Action:   "Add a `project` name and a `resources` block.",
			})
			continue
		}
		if doc.Kind != yaml.MappingNode {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  f.Kind.String() + " must be a mapping",
				Detail:   topLevelShapeDetail(f),
				Origin:   originOf(f.Path, doc),
			})
			continue
		}

		switch f.Kind {
		case FileProject:
			// Assigned ONLY here. It was previously assigned for every file in
			// the loop, which with M4's extra files would make
			// ProjectDecl.Origin the last environment file's — so every
			// diagnostic that points at "the project" would point at
			// environments/zulu.yml.
			out.Origin = originOf(f.Path, doc)
			decodeDocument(f.Path, doc, out, &ds, seenResources, seenVariables, seenEnvironments, seenModules)
		case FileVariables:
			decodeVariableValues(f, out, &ds)
		case FileEnvironment:
			decodeEnvironmentBody(f.Path, f.Environment, doc, out, &ds, seenEnvironments)
		case FileVars:
			decodeVarsFile(f, doc, out, &ds, seenEnvironments)
		case FileResources:
			// A file under resources/** or discovered/** carries a `resources:`
			// block and means exactly what the same block in infra.yml means
			// (§4.1). It goes through decodeResources with the SAME seen map, so
			// a name declared in two files collides exactly as two in one file
			// would -- globbing makes that easy to do by accident, and two
			// declarations silently becoming one is what this language refuses
			// everywhere else.
			decodeResourcesFile(f.Path, f.Dir, doc, out, &ds, seenResources)
		}
	}

	sort.Slice(out.Resources, func(i, j int) bool { return out.Resources[i].Name < out.Resources[j].Name })
	sort.Slice(out.Variables, func(i, j int) bool { return out.Variables[i].Name < out.Variables[j].Name })
	sort.Slice(out.Environments, func(i, j int) bool { return out.Environments[i].Name < out.Environments[j].Name })
	sort.Slice(out.Modules, func(i, j int) bool { return out.Modules[i].Name < out.Modules[j].Name })
	// Sorted ONCE, here, so no consumer has to. Overrides are built by
	// appending in file order, which is the filesystem's order across the
	// block and the file, not the user's.
	for i := range out.Environments {
		o := out.Environments[i].Overrides
		sort.Slice(o, func(a, b int) bool { return o[a].Name < o[b].Name })
	}
	return out, ds
}

// topLevelShapeDetail explains what the top level of each file kind holds.
// A generic "must be a mapping" is true of all three and actionable for none.
func topLevelShapeDetail(f File) string {
	switch f.Kind {
	case FileVariables:
		return "The top level of " + VariablesFileName + " is a flat mapping of variable name to value, for example `region: us-east-1`."
	case FileEnvironment:
		return "The top level of an environment file is a mapping of variable name to value, for example `replicas: 10`."
	case FileModule:
		return "The top level of " + ModuleFileName + " must be a set of keys such as `inputs`, `resources` and `outputs`."
	default:
		return "The top level of " + ProjectFileName + " must be a set of keys such as `project` and `resources`."
	}
}

// decodeVarsFile decodes one file from vars/** (§4.1).
//
// Three shapes, and the filename chooses between them:
//
//   - default.yml            every environment; base configuration
//   - <declared-env>.yml     that environment only
//   - anything else          bare keys are defaults, keys naming an
//     environment are that environment's overrides
//
// A file naming an environment is a set of DIFFERENCES, not a replacement:
// default.yml setting size and region while production.yml sets only size
// leaves production with default's region. That falls out of routing the two
// into the existing base-config and environment rungs rather than merging them
// here, which is why this function chooses a destination instead of computing a
// result.
func decodeVarsFile(f File, doc *yaml.Node, out *ProjectDecl, ds *diag.Diagnostics, seenEnv map[string]int) {
	if f.ScopeDir != "" {
		// Scoped to one resources directory. No environment convention applies
		// here — the directory already says who the values are for, and a
		// filename convention on top would make
		// resources/db/vars/production.yml ambiguous between "production's
		// values for db" and "a file named production".
		if _, ok := out.ScopedValues[f.ScopeDir]; !ok {
			out.ScopedValues[f.ScopeDir] = map[string]value.Value{}
		}
		for i := 0; i+1 < len(doc.Content); i += 2 {
			key, val := doc.Content[i], doc.Content[i+1]
			v, _ := decodeValue(f.Path, "variable "+strconv.Quote(key.Value), val, ds)
			out.ScopedValues[f.ScopeDir][key.Value] = retagSource(v, value.SourceVariable, value.ScopeUnset, "")
		}
		return
	}

	switch {
	case f.Environment == "default":
		decodeVariableValues(File{Path: f.Path, Kind: FileVariables, Root: f.Root}, out, ds)
		return
	case f.Environment != "" && declaresEnvironment(out, f.Environment):
		decodeEnvironmentBody(f.Path, f.Environment, doc, out, ds, seenEnv)
		return
	}

	// The in-file form. A top-level key is an environment block ONLY if it
	// names a declared environment; anything else is a variable, whatever shape
	// its value has. Reinterpreting a map-valued variable as a block would make
	// a file's meaning depend on its value's shape, and a map is an ordinary
	// variable value here.
	for i := 0; i+1 < len(doc.Content); i += 2 {
		key, val := doc.Content[i], doc.Content[i+1]
		if !declaresEnvironment(out, key.Value) {
			v, _ := decodeValue(f.Path, "variable "+strconv.Quote(key.Value), val, ds)
			out.VariableValues[key.Value] = retagSource(v, value.SourceVariable, value.ScopeUnset, "")
			continue
		}
		if val.Kind != yaml.MappingNode {
			// A variable that happens to share a name with an environment. An
			// error rather than a guess, because the alternative is that adding
			// an environment months later silently changes what this file means
			// without anyone touching it.
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "variable " + strconv.Quote(key.Value) + " collides with the environment of that name",
				Detail: "A top-level key naming a declared environment is that environment's overrides, so " +
					strconv.Quote(key.Value) + " cannot also be a variable here.",
				Action: "Rename the variable, or set it under an environment block.",
				Origin: originOf(f.Path, key),
			})
			continue
		}
		decodeEnvironmentBody(f.Path, key.Value, val, out, ds, seenEnv)
	}
}

// declaresEnvironment reports whether name is an environment the project
// declares. Vars files are decoded after infra.yml and environments/, so the
// set is complete by the time this is asked.
func declaresEnvironment(out *ProjectDecl, name string) bool {
	for i := range out.Environments {
		if out.Environments[i].Name == name {
			return true
		}
	}
	return false
}

// decodeResourcesFile decodes one file from resources/** or discovered/**.
//
// Its top level holds `resources:` and nothing else. A `project:` key or an
// environments block in such a file is a mistake worth naming rather than
// ignoring: it reads as though it would work, and silently doing nothing is how
// a user spends an afternoon wondering why their environment has no effect.
func decodeResourcesFile(path, dir string, doc *yaml.Node, out *ProjectDecl, ds *diag.Diagnostics,
	seenResources map[string]value.Origin) {
	for i := 0; i+1 < len(doc.Content); i += 2 {
		key, val := doc.Content[i], doc.Content[i+1]
		if key.Value == "resources" {
			before := len(out.Resources)
			decodeResources(path, val, &out.Resources, ds, seenResources)
			// Stamped here rather than inside decodeResources, which is shared
			// with infra.yml and with module files and has no directory to
			// speak of.
			for i := before; i < len(out.Resources); i++ {
				out.Resources[i].Dir = dir
			}
			continue
		}
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "unexpected key " + strconv.Quote(key.Value) + " in a resources file",
			Detail: "A file under " + ResourcesDirName + "/ or " + DiscoveredDirName +
				"/ holds `resources:` and nothing else. Variables belong in " + VarsDirName +
				"/, and environments in " + EnvironmentsDirName + "/.",
			Action: "Move " + strconv.Quote(key.Value) + " to the file that owns it, or remove it.",
			Origin: originOf(path, key),
		})
	}
}

func decodeDocument(path string, doc *yaml.Node, out *ProjectDecl, ds *diag.Diagnostics,
	seenResources, seenVariables map[string]value.Origin, seenEnvironments map[string]int,
	seenModules map[string]loadedName) {
	for i := 0; i+1 < len(doc.Content); i += 2 {
		key, val := doc.Content[i], doc.Content[i+1]
		switch key.Value {
		case "project":
			if text, ok := requireScalar(path, "`project`", val, ds); ok {
				out.Project = text
			}
		case "resources":
			decodeResources(path, val, &out.Resources, ds, seenResources)
		case "variables":
			decodeVariables(path, val, out, ds, seenVariables)
		case "environments":
			decodeEnvironments(path, val, out, ds, seenEnvironments)
		case "providers":
			decodeProviders(path, val, out, ds)
		case "modules":
			decodeModuleLoads(path, val, &out.Modules, ds, seenModules)
		case "infrena":
			decodeRequiredVersion(path, key, val, out, ds)
		case "plugins":
			decodePluginConstraints(path, val, out, ds)
		default:
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityWarning,
				Summary:  "unrecognised top-level key " + strconv.Quote(key.Value),
				Detail: ProjectFileName + " understands `project`, `infrena`, `plugins`, " +
					"`resources`, `variables`, `environments`, `providers` and `modules`.",
				Origin: originOf(path, key),
			})
		}
	}
}

// decodeRequiredVersion reads `infrena: ">= 0.4"`, the optional floor a project may
// state on the tool itself (PLAN.md §61.2).
//
// A CONSTRAINT, not a format version: the language is additive and already fails
// closed on syntax it does not know, so this exists to turn "unknown key `foo`" into
// "this project needs infrena >= 0.4", which is what a team sharing a repository
// between CI and laptops actually needs.
//
// Declared TWICE is an error naming both lines. Two floors could contradict each other
// — `>= 0.4` in one file and `< 0.4` in another — and silently taking the last one read
// makes which file wins depend on directory order.
func decodeRequiredVersion(path string, key, node *yaml.Node, out *ProjectDecl, ds *diag.Diagnostics) {
	origin := originOf(path, key)
	if out.RequiredVersionOrigin.File != "" {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "`infrena` is declared twice",
			Detail: "The first is at " + describeOrigin(out.RequiredVersionOrigin) +
				". Two version floors could contradict each other, and taking whichever " +
				"was read last would make the answer depend on the order files are loaded.",
			Action: "Keep one `infrena:` constraint for the project.",
			Origin: origin,
		})
		return
	}

	text, ok := requireScalar(path, "`infrena`", node, ds)
	if !ok {
		return
	}
	constraint, err := semver.ParseConstraint(text)
	if err != nil {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "`infrena` is not a version constraint: " + err.Error(),
			Detail: "A constraint is comparison operators on MAJOR.MINOR.PATCH, with a comma " +
				"meaning AND — `>= 0.4`, or `>= 0.4, < 1.0`.",
			Origin: origin,
		})
		return
	}
	out.RequiredVersion = constraint
	out.RequiredVersionOrigin = origin
}

// decodePluginConstraints reads `plugins:`, a map of plugin name to version
// constraint (PLAN.md §31.1).
//
//	plugins:
//	  aws: ">= 0.3.0, < 0.4.0"
//
// A MAPPING, unlike `providers:`, and for the opposite reason: `providers:` is a list
// because a project may declare two instances of one plugin, while a plugin has exactly
// one version however many instances use it — they share one process.
func decodePluginConstraints(path string, node *yaml.Node, out *ProjectDecl, ds *diag.Diagnostics) {
	if node.Kind != yaml.MappingNode {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "`plugins` must be a mapping of plugin name to version constraint",
			Detail:   "For example:\n  plugins:\n    aws: \">= 0.3.0, < 0.4.0\"",
			Origin:   originOf(path, node),
		})
		return
	}

	for i := 0; i+1 < len(node.Content); i += 2 {
		key, val := node.Content[i], node.Content[i+1]
		origin := originOf(path, key)

		if existing, declared := out.Plugins[key.Value]; declared {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "plugin " + strconv.Quote(key.Value) + " is constrained twice",
				Detail: "The first is at " + describeOrigin(existing.Origin) +
					". Two constraints on one plugin could contradict each other, and taking " +
					"whichever was read last would make the answer depend on the order files " +
					"are loaded.",
				Action: "Keep one constraint per plugin.",
				Origin: origin,
			})
			continue
		}

		text, ok := requireScalar(path, "the constraint for plugin "+strconv.Quote(key.Value), val, ds)
		if !ok {
			continue
		}
		constraint, err := semver.ParseConstraint(text)
		if err != nil {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "plugin " + strconv.Quote(key.Value) + " has an invalid constraint: " + err.Error(),
				Detail: "A constraint is comparison operators on MAJOR.MINOR.PATCH, with a comma " +
					"meaning AND — `>= 0.3.0`, or `>= 0.3.0, < 0.4.0`.",
				Origin: origin,
			})
			continue
		}
		if out.Plugins == nil {
			out.Plugins = make(map[string]PluginConstraint)
		}
		out.Plugins[key.Value] = PluginConstraint{Constraint: constraint, Origin: origin}
	}
}

func decodeResources(path string, node *yaml.Node, dst *[]*ResourceDecl, ds *diag.Diagnostics, seen map[string]value.Origin) {
	if node.Kind != yaml.MappingNode {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "`resources` must be a mapping of logical name to resource",
			Origin:   originOf(path, node),
		})
		return
	}

	for i := 0; i+1 < len(node.Content); i += 2 {
		nameNode, body := node.Content[i], node.Content[i+1]
		r := &ResourceDecl{
			Name:       nameNode.Value,
			Attributes: map[string]AttributeDecl{},
			Origin:     originOf(path, nameNode),
		}

		if !checkResourceName(path, r.Name, r.Origin, ds) {
			continue
		}

		// A duplicate name is silent resource loss, not a stylistic problem:
		// M2 keys its resource map by address, so one definition simply
		// disappears from the plan. Stage 2 is the only stage that still has
		// the line numbers to say where the other one is.
		if first, dup := seen[r.Name]; dup {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "resource " + strconv.Quote(r.Name) + " is defined more than once",
				Detail:   "A logical name must be unique within a module. " + strconv.Quote(r.Name) + " is also defined at " + describeOrigin(first) + ".",
				Action:   "Rename one of them, or merge the two definitions.",
				Origin:   r.Origin,
			})
			continue
		}
		seen[r.Name] = r.Origin

		if body.Kind != yaml.MappingNode {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "resource " + strconv.Quote(r.Name) + " must be a mapping",
				Origin:   originOf(path, body),
			})
			continue
		}

		// typeReported records that `type` was present but unusable, so the
		// "has no `type`" check below does not fire a second, misleading
		// diagnostic for the same key.
		typeReported := false

		// Keys are tracked separately from r.Attributes because `type`,
		// `depends_on` and `lifecycle` are keys too, and repeating one of those
		// silently loses a value just as an attribute does.
		seenKeys := map[string]value.Origin{}

		for j := 0; j+1 < len(body.Content); j += 2 {
			key, val := body.Content[j], body.Content[j+1]
			keyOrigin := originOf(path, key)
			if first, dup := seenKeys[key.Value]; dup {
				ds.Add(diag.Diagnostic{
					Severity: diag.SeverityError,
					Summary:  strconv.Quote(key.Value) + " is set more than once on resource " + strconv.Quote(r.Name),
					Detail:   "The last assignment would silently win. " + strconv.Quote(key.Value) + " is also set at " + describeOrigin(first) + ".",
					Action:   "Remove one of the two assignments.",
					Origin:   keyOrigin,
				})
				continue
			}
			seenKeys[key.Value] = keyOrigin

			switch key.Value {
			case "type":
				text, ok := requireScalar(path, "`type`", val, ds)
				if !ok {
					// The `type` key is present but unusable. Reporting "has no
					// `type`" as well would name a symptom rather than the
					// problem.
					typeReported = true
					break
				}
				if !checkResourceType(path, r.Name, text, originOf(path, val), ds) {
					// Same suppression, same reason: a malformed `module.` type
					// has been reported, and "has no `type`" would describe a
					// symptom of it.
					typeReported = true
					break
				}
				r.Type = text
			case "depends_on":
				if val.Kind != yaml.SequenceNode {
					ds.Add(diag.Diagnostic{
						Severity: diag.SeverityError,
						Summary:  "`depends_on` must be a list",
						Detail:   "A scalar silently produces no dependencies, and a missing edge means a resource can be created before what it depends on.",
						Action:   "Write depends_on: [" + val.Value + "]",
						Origin:   originOf(path, val),
					})
					break
				}
				for i, item := range val.Content {
					text, ok := requireScalar(path, "`depends_on`["+strconv.Itoa(i)+"]", item, ds)
					if !ok {
						continue
					}
					r.DependsOn = append(r.DependsOn, text)
				}
			case "provider":
				// A named key, not an attribute, for the reason `skip` and `only`
				// are: everything this switch does not recognise BECOMES an
				// attribute, so falling through would reach stage 7 as "no
				// attribute provider" on every resource that names one.
				text, ok := requireScalar(path, "`provider`", val, ds)
				if !ok {
					break
				}
				r.Provider = AttributeDecl{
					Name:   "provider",
					Value:  value.String(text, value.SourceExplicit).WithOrigin(originOf(path, val)),
					Origin: keyOrigin,
				}
			case "skip", "only":
				// Named keys rather than attributes: everything this switch does
				// not recognise becomes an ATTRIBUTE, so falling through would
				// reach stage 7 as "no attribute skip" on every resource using
				// the feature.
				//
				// The VALUE is decoded exactly as an attribute would be, because
				// it may be an expression (§6.2) and stage 5 evaluates it with
				// the machinery that already evaluates attributes.
				v, hasExpr := decodeValue(path, "`"+key.Value+"`", val, ds)
				d := AttributeDecl{Name: key.Value, Value: v, HasExpressions: hasExpr, Origin: keyOrigin}
				if key.Value == "skip" {
					r.Skip = d
				} else {
					r.Only = d
				}
			case "lifecycle":
				decodeLifecycle(path, val, r, ds)
			default:
				v, hasExpr := decodeValue(path, "attribute "+strconv.Quote(key.Value), val, ds)
				r.Attributes[key.Value] = AttributeDecl{
					Name:           key.Value,
					Value:          v,
					HasExpressions: hasExpr,
					Origin:         keyOrigin,
				}
			}
		}

		// §6.2: two spellings of one idea, and they can contradict — `skip: [dev]`
		// with `only: [dev]` means nothing coherent. Refused rather than given a
		// precedence, because any precedence here is a coin-flip a reader would
		// have to memorise.
		if r.Skip.Name != "" && r.Only.Name != "" {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "resource " + strconv.Quote(r.Name) + " sets both `skip` and `only`",
				Detail: "They are two ways of saying the same thing and can contradict each other: " +
					"`only` is also set at " + describeOrigin(r.Only.Origin) + ".",
				Action: "Keep whichever reads better and remove the other. `only` lists the " +
					"environments the resource belongs to; `skip` lists the ones it does not.",
				Origin: r.Skip.Origin,
			})
		}

		if r.Type == "" && !typeReported {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "resource " + strconv.Quote(r.Name) + " has no `type`",
				Detail:   "Every resource must name the provider resource type it manages, for example `type: fake.database`.",
				Action:   "Add a `type` key to " + strconv.Quote(r.Name) + ".",
				Origin:   r.Origin,
			})
		}
		*dst = append(*dst, r)
	}
}

func decodeLifecycle(path string, node *yaml.Node, r *ResourceDecl, ds *diag.Diagnostics) {
	if node.Kind != yaml.MappingNode {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "`lifecycle` must be a mapping",
			Origin:   originOf(path, node),
		})
		return
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, val := node.Content[i], node.Content[i+1]
		switch key.Value {
		case "prevent_destroy":
			if b, ok := decodeLifecycleBool(path, key, val, ds); ok {
				r.Lifecycle.PreventDestroy = b
				r.Lifecycle.PreventDestroySet = true
			}
		case "retain":
			if b, ok := decodeLifecycleBool(path, key, val, ds); ok {
				r.Lifecycle.Retain = b
				r.Lifecycle.RetainSet = true
			}
		case "ignore_changes":
			decodeIgnoreChanges(path, key, val, r, ds)
		default:
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "unknown lifecycle option " + strconv.Quote(key.Value),
				Detail:   "Supported options are `prevent_destroy`, `retain` and `ignore_changes`.",
				Origin:   originOf(path, key),
			})
		}
	}
}

// decodeIgnoreChanges reads `ignore_changes:` — a LIST of attribute names whose drift
// this resource does not want reverted (PLAN.md §14.2).
//
// The names are kept exactly as written. Resolving them against the schema happens in the
// compiler, at the same boundary that canonicalises everything else a user spells, so
// `ignore_changes: [taskRevision]` and `[task_revision]` reach the same attribute — and so
// a name matching nothing is refused there, where the attribute list is known.
//
// A list, not a scalar, and not a mapping: every entry is one attribute name, and the
// shape says so. An empty list is legal and means nothing is ignored, which is what a
// reader editing the list down to nothing expects.
func decodeIgnoreChanges(path string, key, node *yaml.Node, r *ResourceDecl, ds *diag.Diagnostics) {
	if node.Kind != yaml.SequenceNode {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "`ignore_changes` must be a list of attribute names",
			Detail:   "For example:\n  lifecycle:\n    ignore_changes: [task_revision]",
			Origin:   originOf(path, key),
		})
		return
	}
	if r.Lifecycle.IgnoreChangesOrigin == nil {
		r.Lifecycle.IgnoreChangesOrigin = map[string]value.Origin{}
	}
	for _, entry := range node.Content {
		if entry.Kind != yaml.ScalarNode || entry.Value == "" {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "`ignore_changes` entries must be attribute names",
				Origin:   originOf(path, entry),
			})
			continue
		}
		r.Lifecycle.IgnoreChanges = append(r.Lifecycle.IgnoreChanges, entry.Value)
		r.Lifecycle.IgnoreChangesOrigin[entry.Value] = originOf(path, entry)
	}
}

// decodeLifecycleBool decodes a lifecycle flag, letting the YAML decoder judge
// what is boolean rather than comparing the raw scalar text.
//
// Raw comparison against "true" is not merely imprecise here, it is unsafe:
// `prevent_destroy: True` and `prevent_destroy: TRUE` both carry the !!bool tag
// but scalar text that is not literally "true", so a raw check silently
// disables a destruction guard the user believed they had enabled. A quoted
// "true" is a string and is rejected loudly rather than silently accepted.
func decodeLifecycleBool(path string, key, val *yaml.Node, ds *diag.Diagnostics) (bool, bool) {
	var b bool
	if err := val.Decode(&b); err != nil {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "lifecycle option " + strconv.Quote(key.Value) + " must be true or false",
			Detail:   "Got " + strconv.Quote(val.Value) + ", which is not a boolean. Note a quoted value is a string.",
			Action:   "Write " + key.Value + ": true (unquoted).",
			Origin:   originOf(path, val),
		})
		return false, false
	}
	return b, true
}

// requireScalar reads a node's scalar text, refusing anything that is not one.
//
// Reading node.Value without checking the node's kind is the defect class that
// has already bitten this branch twice. A mapping or sequence node has an empty
// Value, so the value silently became ""; an alias node's Value is the
// anchor's name, so `type: *base` silently became the string "base"; and a
// !!null scalar's Value is "" too, so a blank required attribute would satisfy
// requiredness in M2's stage 7. None of them produced a diagnostic.
//
// `what` names the offending position, already quoted or backticked.
func requireScalar(path, what string, node *yaml.Node, ds *diag.Diagnostics) (string, bool) {
	switch {
	case node.Kind == yaml.AliasNode:
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  what + " is a YAML alias, which M1 does not resolve",
			Detail:   "An alias node carries the anchor's name rather than its value, so accepting it would silently substitute " + strconv.Quote(node.Value) + ".",
			Action:   "Write the value out in full.",
			Origin:   originOf(path, node),
		})
	case node.Kind != yaml.ScalarNode:
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  what + " must be a single value, not a list or a mapping",
			Detail:   "A list or mapping has no scalar text, so it would silently read as an empty value.",
			Origin:   originOf(path, node),
		})
	case node.Tag == "!!null":
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  what + " has no value",
			Detail:   "An empty value would silently read as the empty string, which is indistinguishable from a value that was deliberately set to \"\".",
			Action:   "Give it a value, or remove the key.",
			Origin:   originOf(path, node),
		})
	default:
		return node.Value, true
	}
	return "", false
}

// decodeValue converts a YAML node into a typed Value, reporting whether any
// string within it contains an interpolation.
//
// `what` names the position being decoded, for diagnostics.
func decodeValue(path, what string, node *yaml.Node, ds *diag.Diagnostics) (value.Value, bool) {
	origin := originOf(path, node)

	switch node.Kind {
	case yaml.SequenceNode:
		items := make([]value.Value, 0, len(node.Content))
		anyExpr := false
		for i, item := range node.Content {
			v, has := decodeValue(path, what+"["+strconv.Itoa(i)+"]", item, ds)
			anyExpr = anyExpr || has
			items = append(items, v)
		}
		return value.List(items, value.SourceExplicit).WithOrigin(origin), anyExpr

	case yaml.MappingNode:
		items := map[string]value.Value{}
		anyExpr := false
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i]
			v, has := decodeValue(path, what+"."+key.Value, node.Content[i+1], ds)
			anyExpr = anyExpr || has
			items[key.Value] = v
		}
		return value.Map(items, value.SourceExplicit).WithOrigin(origin), anyExpr

	default:
		return decodeScalar(path, what, node, ds, origin)
	}
}

func decodeScalar(path, what string, node *yaml.Node, ds *diag.Diagnostics, origin value.Origin) (value.Value, bool) {
	raw, ok := requireScalar(path, what, node, ds)
	if !ok {
		// KindInvalid rather than an empty string: a value that could not be
		// read must not masquerade as one that was.
		return value.Value{Source: value.SourceExplicit, Origin: origin}, false
	}

	// An interpolation is kept verbatim; stage 6 parses it in M2.
	if strings.Contains(raw, "${") {
		return value.String(raw, value.SourceExplicit).WithOrigin(origin), true
	}

	// A quoted scalar is always a string, whatever it looks like.
	if node.Style == yaml.DoubleQuotedStyle || node.Style == yaml.SingleQuotedStyle {
		return value.String(raw, value.SourceExplicit).WithOrigin(origin), false
	}

	switch node.Tag {
	case "!!int":
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return value.Int(n, value.SourceExplicit).WithOrigin(origin), false
		}
	case "!!float":
		if f, err := strconv.ParseFloat(raw, 64); err == nil {
			return value.Float(f, value.SourceExplicit).WithOrigin(origin), false
		}
	case "!!bool":
		// Let the decoder judge, exactly as decodeLifecycleBool does. `True`
		// and `TRUE` carry the !!bool tag but scalar text that is not literally
		// "true", so a raw comparison silently yields the wrong boolean for
		// every attribute in every resource, with no diagnostic.
		var b bool
		if err := node.Decode(&b); err == nil {
			return value.Bool(b, value.SourceExplicit).WithOrigin(origin), false
		}
	}
	return value.String(raw, value.SourceExplicit).WithOrigin(origin), false
}

func documentRoot(n *yaml.Node) *yaml.Node {
	// An empty file leaves the root at its zero value rather than producing a
	// DocumentNode: yaml.Unmarshal never invokes the decoder for empty input.
	if n == nil || n.Kind == 0 {
		return nil
	}
	if n.Kind == yaml.DocumentNode {
		if len(n.Content) == 0 {
			return nil
		}
		return n.Content[0]
	}
	return n
}

// describeOrigin renders an origin for use inside a sentence, naming the file
// only when it differs from the one the diagnostic already points at.
func describeOrigin(o value.Origin) string {
	if o.Line == 0 {
		return o.File
	}
	return "line " + strconv.Itoa(o.Line) + " of " + o.File
}

func originOf(path string, n *yaml.Node) value.Origin {
	if n == nil {
		return value.Origin{File: path}
	}
	return value.Origin{File: path, Line: n.Line, Column: n.Column}
}

// variableTypeList renders the accepted type spellings for a diagnostic.
//
// The spellings come from pkg/value, which owns them in both directions:
// value.ParseKind is the inverse of value.Kind.String(), pinned as such by
// TestKindStringAndParseKindAreInverses. This package deliberately keeps NO
// table of its own — a second copy would drift from the one the parser
// actually consults.
//
// value.KindNames is already sorted, so the message does not reorder itself
// between identical runs.
func variableTypeList() string {
	return strings.Join(value.KindNames(), ", ")
}

// declNoun names what a `type`/`default`/`min`/`max` declaration declares, so ONE
// decoder serves both a project variable (PLAN.md §9) and a module input (§11.3)
// without either one's diagnostics carrying the other's advice.
//
// §11.3 says a module's `inputs:` is spelled exactly as §9's `variables:`, which
// is what makes the reuse right — but a user with a broken input in
// modules/net/module.yml must not be told a "variable" is wrong and sent to
// variables.yml, a file that has nothing to do with their problem. That is the
// §44 failure this parameter closes; a second copy of decodeVariable would close
// it by reintroducing the duplication contract Ruling 3 forbids.
type declNoun struct {
	singular string // "variable"
	titled   string // "Variable", at the start of a sentence
	// supplied is how a value reaches this kind of declaration when the
	// declaration itself does not carry one, phrased to slot into an action:
	// "... or put the value <supplied> instead."
	supplied string
}

// named renders "variable \"replicas\"" or "input \"replicas\"".
func (d declNoun) named(name string) string {
	return d.singular + " " + strconv.Quote(name)
}

var (
	variableNoun = declNoun{singular: "variable", titled: "Variable", supplied: "in " + VariablesFileName}
	inputNoun    = declNoun{
		singular: "input",
		titled:   "Input",
		supplied: "on the resource that instantiates this module",
	}
)

func decodeVariables(path string, node *yaml.Node, out *ProjectDecl, ds *diag.Diagnostics, seen map[string]value.Origin) {
	if node.Kind != yaml.MappingNode {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "`variables` must be a mapping of variable name to declaration",
			Detail:   "Each variable is a key with `type`, `default`, `min` and `max` beneath it (PLAN.md §9).",
			Origin:   originOf(path, node),
		})
		return
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		nameNode, body := node.Content[i], node.Content[i+1]
		origin := originOf(path, nameNode)

		// yaml.v3 does not deduplicate mapping keys when decoding into a Node,
		// so both entries arrive here and the last would silently win —
		// discarding a type or a bound the user wrote.
		if first, dup := seen[nameNode.Value]; dup {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "variable " + strconv.Quote(nameNode.Value) + " is declared more than once",
				Detail:   "The last declaration would silently win, discarding a type or a bound. " + strconv.Quote(nameNode.Value) + " is also declared at " + describeOrigin(first) + ".",
				Action:   "Remove one of the two declarations, or merge them.",
				Origin:   origin,
			})
			continue
		}
		seen[nameNode.Value] = origin
		out.Variables = append(out.Variables, decodeVariable(path, nameNode.Value, body, origin, variableNoun, ds))
	}
}

func decodeVariable(path, name string, body *yaml.Node, origin value.Origin, d declNoun, ds *diag.Diagnostics) VariableDecl {
	v := VariableDecl{Name: name, Origin: origin}

	if body.Kind != yaml.MappingNode {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  d.named(name) + " must be a mapping",
			Detail:   "A `variables:` entry is a schema — `type`, `default`, `min`, `max` — not a value. A bare value here would be ambiguous with a declaration whose type is `map`.",
			Action:   "Write `default:` beneath " + strconv.Quote(name) + ", or put the value " + d.supplied + " instead.",
			Origin:   originOf(path, body),
		})
		return v
	}

	// defaultNode, minNode and maxNode are held back and resolved AFTER the
	// whole mapping is walked. `type:` may appear textually after any of
	// them — YAML mappings carry no ordering guarantee — and an
	// implementation that resolves each key as it walks would silently skip
	// coercion (or accept `min: 1` on a string) whenever the file happened to
	// declare `type:` last.
	var defaultNode, minNode, maxNode *yaml.Node
	// typeReported and defaultReported suppress the "must specify at least a
	// type or a default" check below when the key WAS present but unusable.
	// Telling a user their declaration is empty, one line under a diagnostic
	// explaining why their `type` was rejected, describes a symptom.
	typeReported := false
	defaultReported := false
	seenKeys := map[string]value.Origin{}

	for i := 0; i+1 < len(body.Content); i += 2 {
		key, val := body.Content[i], body.Content[i+1]
		keyOrigin := originOf(path, key)
		if first, dup := seenKeys[key.Value]; dup {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  strconv.Quote(key.Value) + " is set more than once on " + d.singular + " " + strconv.Quote(name),
				Detail:   "The last assignment would silently win. " + strconv.Quote(key.Value) + " is also set at " + describeOrigin(first) + ".",
				Action:   "Remove one of the two assignments.",
				Origin:   keyOrigin,
			})
			continue
		}
		seenKeys[key.Value] = keyOrigin

		switch key.Value {
		case "type":
			text, ok := requireScalar(path, d.named(name)+"'s `type`", val, ds)
			if !ok {
				typeReported = true
				break
			}
			kind, known := value.ParseKind(text)
			if !known {
				ds.Add(diag.Diagnostic{
					Severity: diag.SeverityError,
					Summary:  "unknown " + d.singular + " type " + strconv.Quote(text),
					Detail:   d.titled + " " + strconv.Quote(name) + " declares a type the engine does not have. Supported types are " + variableTypeList() + ".",
					Action:   "Change `type` to one of " + variableTypeList() + ", or remove it to leave " + strconv.Quote(name) + " untyped.",
					Origin:   originOf(path, val),
				})
				typeReported = true
				break
			}
			v.Type = kind
		case "default":
			defaultNode = val
		case "min":
			minNode = val
		case "max":
			maxNode = val
		default:
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "unknown key " + strconv.Quote(key.Value) + " in " + d.named(name),
				Detail:   "A " + d.singular + " declaration understands `type`, `default`, `min` and `max`.",
				Action:   "Remove " + strconv.Quote(key.Value) + ".",
				Origin:   keyOrigin,
			})
		}
	}

	if defaultNode != nil {
		if dv, ok := decodeDefault(path, name, defaultNode, v.Type, d, ds); ok {
			v.Default, v.HasDefault = dv, true
		} else {
			// decodeDefault already added its own diagnostic (interpolation,
			// or a lossy numeric coercion) — suppress "must specify at least
			// a `type` or a `default`" below the same way typeReported does,
			// so the user sees one error about this declaration, not two.
			defaultReported = true
		}
	}

	v.Min, v.HasMin = decodeBound(path, name, "min", minNode, v.Type, typeReported, d, ds)
	v.Max, v.HasMax = decodeBound(path, name, "max", maxNode, v.Type, typeReported, d, ds)

	// A declaration with neither a type nor a default carries no information:
	// it names a variable and says nothing about it, so stage 4 has nothing to
	// validate against and nothing to fall back on.
	//
	// This is an ERROR rather than a dropped declaration with a warning.
	// Dropping it discards something the user wrote and continues as though
	// they had not — the silent-loss shape this engine refuses everywhere
	// else, with a log line in front of it. A user who writes `replicas:` under
	// `variables:` meant something by it; the only safe response is to say the
	// declaration is incomplete, at the line it is on, and stop.
	//
	// Three suppressions, all of the same kind: say the root cause once.
	// typeReported and defaultReported mean the key WAS present but unusable
	// and has already been reported. A present min or max means decodeBound
	// just said "add `type: integer` or `type: float`" — the same advice this
	// would give, about the same declaration, one line apart.
	incomplete := v.Type == value.KindInvalid && !v.HasDefault
	alreadyExplained := typeReported || defaultReported || minNode != nil || maxNode != nil
	if incomplete && !alreadyExplained {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  d.named(name) + " must specify at least a `type` or a `default`",
			Detail:   "The declaration says nothing about " + strconv.Quote(name) + ", so nothing can be validated against it and there is no value to fall back on when it is not set.",
			Action:   "Add `type: string` (or another type), or `default:` with a value, or remove the declaration and set " + strconv.Quote(name) + " " + d.supplied + ".",
			Origin:   origin,
		})
	}

	// min > max can never be satisfied. Reported here, where the declaration
	// is in hand, because at stage 4 the only thing to point at is a VALUE
	// that failed a range check — naming the symptom instead of the cause.
	// min == max is legal: it pins a variable to one value.
	if v.HasMin && v.HasMax && boundGreater(v.Min, v.Max) {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  d.named(name) + " has a `min` greater than its `max`",
			Detail:   "No value can satisfy this declaration, so every plan would fail with a range error naming the value rather than the declaration.",
			Action:   "Swap `min` and `max`, or remove one of them.",
			Origin:   v.Min.Origin,
		})
	}
	return v
}

// boundGreater reports whether bound a is greater than bound b.
//
// It switches on Kind rather than coercing both to float64, because an int64
// does not fit a float64: above 2^53 the conversion rounds and two bounds that
// differ would compare equal. That is the same reason Min and Max are
// value.Value rather than float64, and it is the shape Task 4's range check
// takes too.
//
// There is no mixed case to handle. decodeBound has already coerced both
// bounds to the variable's declared type, so if both are present they have the
// same Kind — which is exactly what that coercion buys.
//
// A non-numeric value is never greater: decodeBound refused it and said why,
// and a second diagnostic about the same declaration would describe the
// symptom.
func boundGreater(a, b value.Value) bool {
	if ai, ok := a.AsInt(); ok {
		bi, ok2 := b.AsInt()
		return ok2 && ai > bi
	}
	if af, ok := a.AsFloat(); ok {
		bf, ok2 := b.AsFloat()
		return ok2 && af > bf
	}
	return false
}

// decodeBound decodes `min` or `max`, refusing it on a type that has no
// ordering.
//
// Numeric-only is PLAN.md §9's rule, and it matters more than it looks: a
// `min` on a string would have to mean either "shortest" or "lowest in some
// collation", and stage 4 would have to pick one silently.
//
// typeReported suppresses this when `type` itself was already rejected —
// "`min` is not valid on an untyped variable" names a symptom of a problem
// already reported one line up.
func decodeBound(path, name, which string, node *yaml.Node, kind value.Kind, typeReported bool, d declNoun, ds *diag.Diagnostics) (value.Value, bool) {
	if node == nil {
		return value.Value{}, false
	}
	if kind != value.KindInt && kind != value.KindFloat {
		if typeReported {
			return value.Value{}, false
		}
		detail := d.titled + " " + strconv.Quote(name) + " has type " + kind.String() + ", which has no ordering, so `" + which + "` could not be checked against anything."
		action := "Remove `" + which + "`, or declare `type: integer` or `type: float`."
		if kind == value.KindInvalid {
			detail = d.titled + " " + strconv.Quote(name) + " declares no `type`, so `" + which + "` has no ordering to be checked against."
			action = "Add `type: integer` or `type: float`, or remove `" + which + "`."
		}
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "`" + which + "` is not valid on " + d.named(name),
			Detail:   detail,
			Action:   action,
			Origin:   originOf(path, node),
		})
		return value.Value{}, false
	}

	// Both numeric kinds are accepted here whatever the declared type. YAML
	// tags `min: 1` as !!int even under `type: float`, so refusing KindInt
	// would reject the obvious spelling of a float bound.
	bv, hasExpr := decodeValue(path, d.named(name)+"'s `"+which+"`", node, ds)
	if hasExpr || !bv.Known || (bv.Kind != value.KindInt && bv.Kind != value.KindFloat) {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "`" + which + "` on " + d.named(name) + " must be a number",
			Detail:   "Got " + strconv.Quote(node.Value) + ". A quoted number is a string, and a string bound compares by text: \"10\" sorts before \"9\".",
			Action:   "Write `" + which + ": " + node.Value + "` unquoted.",
			Origin:   originOf(path, node),
		})
		return value.Value{}, false
	}
	return coerceBound(path, name, which, node, bv, kind, d, ds)
}

// coerceBound converts a decoded bound to the variable's DECLARED kind, via
// value.Coerce — the ONE int<->float exactness rule shared with
// decodeDefault and, from M4 Task 6, stage 4's resolution of a supplied
// value against a schema. Extracted to pkg/value rather than kept here or
// duplicated per caller, because the rule must never differ between the
// places applying it: a precision bug in one is a precision bug in all of
// them, which is the shared-fate argument for one function rather than one
// copy per caller — the defect class this project has already paid for
// twice (M2's leaked plaintext secret; M3's eleven redundant sorts).
//
// This is why stage 2 is the right place: it is the only stage that knows both
// the declared type and the line number. After it, a bound's Kind always equals
// its variable's Type, so every consumer switches on one Kind instead of
// handling a cross product — and value.AsFloat, which deliberately does not
// coerce an int64, is safe to use directly.
//
// A conversion that would lose information is a DIAGNOSTIC, never a silent
// truncation. `type: integer` with `min: 1.5` must not quietly become 1: the
// user would see values below their stated minimum accepted, with nothing
// printed, which is the silent-loss shape this engine refuses everywhere else.
func coerceBound(path, name, which string, node *yaml.Node, bv value.Value, kind value.Kind, d declNoun, ds *diag.Diagnostics) (value.Value, bool) {
	// origin is computed once, up front, and applied on the success path
	// below. value.Coerce itself stays pure and never touches Origin — stage
	// 4 (Task 6) has no line to re-origin to, and stage 2 does, so re-origining
	// belongs here, in the caller, not in the shared arithmetic.
	origin := originOf(path, node)
	coerced, ok := value.Coerce(bv, kind)
	if ok {
		return coerced.WithOrigin(origin), true
	}

	// decodeBound already refused a non-numeric kind, and a non-numeric bv,
	// before ever calling coerceBound — so value.Coerce's failure here is
	// always the genuine numeric exactness case, and which direction follows
	// from `kind` alone: KindInt means bv was the float being rounded,
	// KindFloat means bv was the int overflowing 2^53. Diagnostics kept
	// word-for-word identical to before this used value.Coerce, so no
	// existing test's assertion needed to change.
	if kind == value.KindInt {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "`" + which + "` on " + d.named(name) + " is not a whole number",
			Detail:   d.titled + " " + strconv.Quote(name) + " has type integer, so " + strconv.Quote(node.Value) + " cannot be its " + which + ". Rounding it would silently change the bound the user asked for.",
			Action:   "Write a whole number, or declare `type: float`.",
			Origin:   origin,
		})
	} else {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "`" + which + "` on " + d.named(name) + " is too large to represent as a float",
			Detail:   d.titled + " " + strconv.Quote(name) + " has type float, and " + strconv.Quote(node.Value) + " cannot be converted without changing its value.",
			Action:   "Use a smaller bound, or declare `type: integer`.",
			Origin:   origin,
		})
	}
	return value.Value{}, false
}

// decodeDefault decodes and, where both the declared type and the literal
// are numeric, coerces a variable's `default:` value (M4 contract Amendment
// 4: "A numeric literal coerces to the declared kind wherever it appears
// ... This applies to a declared `default:` as it already does to
// `min`/`max`"). Called AFTER decodeVariable's key-walking loop closes, for
// the same reason minNode/maxNode are: `type:` may appear textually after
// `default:`, and coercing inside the loop would silently skip it whenever
// the file happened to be written that way.
//
// ok is false whenever a diagnostic was added — an interpolation, or a lossy
// numeric conversion — and the caller must not set HasDefault in that case.
func decodeDefault(path, name string, node *yaml.Node, kind value.Kind, d declNoun, ds *diag.Diagnostics) (value.Value, bool) {
	dv, hasExpr := decodeValue(path, d.named(name)+"'s `default`", node, ds)
	if hasExpr {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  d.named(name) + "'s default contains an interpolation",
			Detail:   "Defaults are resolved before any expression scope exists, so `${...}` here has nothing to refer to. Accepting it would store the literal text " + strconv.Quote(node.Value) + " as the default.",
			Action:   "Write a literal value, or set " + strconv.Quote(name) + " per environment instead.",
			Origin:   originOf(path, node),
		})
		return value.Value{}, false
	}

	// Attempt coercion only when BOTH sides are numeric — and this guard is
	// LOAD-BEARING here, unlike in coerceBound. decodeBound already refuses
	// any non-numeric kind or bv before coerceBound is ever called, so
	// value.Coerce there only ever sees a genuine numeric exactness question.
	// A default has no such upfront refusal — it is valid on every type,
	// typed or not — and value.Coerce reports ok=false for ANY kind
	// mismatch, not only a numeric one (that is the point of it not judging
	// a type mismatch as a lossy conversion: it does not itself distinguish
	// "nothing to coerce" from "coercion failed"). Calling it unconditionally
	// would treat an untyped variable's default, or a default under a
	// non-numeric declared type, as a coercion FAILURE worth one of the
	// numeric diagnostics below — when it is neither: an untyped variable has
	// no declared kind to coerce toward (the amendment's ruling on that
	// case), and a default under a non-numeric declared type — or a
	// non-numeric literal under a numeric one — is a type MISMATCH for
	// stage 4 to report (`v.Kind == decl.Type`, per Task 3's contract note
	// for Task 4), not a numeric conversion for stage 2 to perform.
	numericType := kind == value.KindInt || kind == value.KindFloat
	numericLiteral := dv.Kind == value.KindInt || dv.Kind == value.KindFloat
	if !numericType || !numericLiteral {
		return dv, true
	}

	// origin computed up front, applied on the success path below — the same
	// re-origining coerceBound does, and for the same reason: value.Coerce
	// stays pure, stage 2 stamps its own line.
	origin := originOf(path, node)
	coerced, ok := value.Coerce(dv, kind)
	if ok {
		return coerced.WithOrigin(origin), true
	}

	// Diagnostics mirror coerceBound's wording for the same exactness rule,
	// substituting "default" for "min"/"max" — PLAN.md §44's shape (name the
	// variable, the declared type, the literal written, and what to write
	// instead), kept in the two callers rather than a third shared string
	// because "a `min` on variable X" and "variable X's default" read
	// differently in a sentence.
	if kind == value.KindInt {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  d.named(name) + "'s default is not a whole number",
			Detail:   d.titled + " " + strconv.Quote(name) + " has type integer, so " + strconv.Quote(node.Value) + " cannot be its default. Rounding it would silently change the value the user wrote.",
			Action:   "Write a whole number, or declare `type: float`.",
			Origin:   origin,
		})
	} else {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  d.named(name) + "'s default is too large to represent as a float",
			Detail:   d.titled + " " + strconv.Quote(name) + " has type float, and " + strconv.Quote(node.Value) + " cannot be converted without changing its value.",
			Action:   "Use a smaller default, or declare `type: integer`.",
			Origin:   origin,
		})
	}
	return value.Value{}, false
}

// decodeVariableValues decodes variables.yml: a flat mapping of variable name
// to value (PLAN.md §8).
//
// It delegates to DecodeVariableFile rather than walking the mapping itself.
// variables.yml and a --var-file have the identical shape, and a second
// implementation of "decode a flat variable mapping" is exactly the defect
// class that leaked a plaintext secret in M2 (internal/compiler/bind.go's
// variableScope, deleted by task 7) — one fix applied to one copy left the
// other silently wrong. scope is value.ScopeUnset because this runs at
// decode time (stage 2): it declares, it does not resolve, and which
// precedence level wins is stages 3 and 4's answer alone.
func decodeVariableValues(f File, out *ProjectDecl, ds *diag.Diagnostics) {
	vals, fds := DecodeVariableFile(f, value.ScopeUnset)
	ds.Extend(fds)
	maps.Copy(out.VariableValues, vals)
}

// retagSource rewrites a decoded value's provenance recursively.
//
// decodeValue marks everything SourceExplicit, because that is what an
// attribute in infra.yml is. A value from variables.yml is a variable and one
// from an environment file is an environment override, and provenance is
// PER-LEAF (spec §5.1) — setting only the top level would leave every element
// of a list variable claiming to be explicit configuration, which `explain`
// and minimal generation both read straight off the leaves.
//
// scope is threaded through for the same per-leaf reason, but every DECODE-TIME
// caller (this file's two: environment overrides, and variables.yml via
// DecodeVariableFile) passes value.ScopeUnset: a declaration is not a
// resolution, and which precedence level won is stages 3 and 4's answer — see
// pkg/value/scope.go's doc comment. Passing a non-zero Scope through this same
// function is what lets DecodeVariableFile serve --var-file too, which decodes
// at the CLI layer, after resolution order is already known, with no stage 4
// pass left to stamp it later. ScopeUnset is the zero value, so stamping it is
// equivalent to never having set Scope at all.
//
// suppliedBy is threaded the same way, for the same reason (Amendment 6,
// contract.md): a --var-file with a composite value must stamp every LEAF
// with the path that supplied it, not just the composite's shell, or
// annotation() would only ever see it on values a plan never actually
// touches directly. Every caller except DecodeVariableFile's --var-file case
// passes "", the zero value — equivalent to never stamping it — because
// SuppliedBy is meaningful only at ScopeCLIOverride.
func retagSource(v value.Value, src value.ValueSource, scope value.Scope, suppliedBy string) value.Value {
	switch v.Kind {
	case value.KindList:
		items, ok := v.Raw.([]value.Value)
		if !ok {
			// Malformed: a diagnostic was already emitted where it was
			// decoded. Retag the shell and stop rather than panicking.
			return v.WithSource(src).WithScope(scope).WithSuppliedBy(suppliedBy)
		}
		retagged := make([]value.Value, len(items))
		for i, item := range items {
			retagged[i] = retagSource(item, src, scope, suppliedBy)
		}
		v.Raw = retagged
	case value.KindMap:
		m, ok := v.Raw.(map[string]value.Value)
		if !ok {
			return v.WithSource(src).WithScope(scope).WithSuppliedBy(suppliedBy)
		}
		retagged := make(map[string]value.Value, len(m))
		for k, item := range m {
			retagged[k] = retagSource(item, src, scope, suppliedBy)
		}
		v.Raw = retagged
	}
	return v.WithSource(src).WithScope(scope).WithSuppliedBy(suppliedBy)
}

func decodeEnvironments(path string, node *yaml.Node, out *ProjectDecl, ds *diag.Diagnostics, seen map[string]int) {
	if node.Kind != yaml.MappingNode {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "`environments` must be a mapping of environment name to configuration",
			Detail:   "Each environment is a key, optionally with `extends` and its overrides beneath it (PLAN.md §7).",
			Origin:   originOf(path, node),
		})
		return
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		nameNode, body := node.Content[i], node.Content[i+1]
		decodeEnvironmentBody(path, nameNode.Value, body, out, ds, seen)
	}
}

// environmentFor returns the decl for name, creating it on first sight.
//
// One environment may be declared in infra.yml's `environments:` block AND in
// environments/<name>.yml, and the two MERGE: PLAN.md §7 puts `extends` in the
// block, §8 puts overrides in the file, so a user doing both is following the
// spec. Load's fixed file order (project, variables, environments by name) is
// what makes "which one was first" deterministic, which the duplicate
// diagnostics below depend on.
func environmentFor(out *ProjectDecl, name string, origin value.Origin, seen map[string]int) *EnvironmentDecl {
	if idx, ok := seen[name]; ok {
		return &out.Environments[idx]
	}
	out.Environments = append(out.Environments, EnvironmentDecl{Name: name, Origin: origin})
	seen[name] = len(out.Environments) - 1
	return &out.Environments[len(out.Environments)-1]
}

// decodeEnvironmentBody decodes one environment's mapping, from either the
// `environments:` block or a whole environments/<name>.yml document.
//
// Two override spellings are accepted because PLAN.md uses both: §6 nests them
// under `variables:` and §7 writes them as flat keys. `extends` is reserved in
// both, and in the file form.
func decodeEnvironmentBody(path, name string, body *yaml.Node, out *ProjectDecl, ds *diag.Diagnostics, seen map[string]int) {
	origin := originOf(path, body)
	if body.Kind != yaml.MappingNode {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "environment " + strconv.Quote(name) + " must be a mapping",
			Detail:   "An environment holds an optional `extends` and its overrides.",
			Action:   "Write `" + name + ": {}` if it has no settings of its own.",
			Origin:   origin,
		})
		return
	}
	env := environmentFor(out, name, origin, seen)

	for i := 0; i+1 < len(body.Content); i += 2 {
		key, val := body.Content[i], body.Content[i+1]
		keyOrigin := originOf(path, key)

		switch key.Value {
		case "extends":
			// requireScalar reports the alias, non-scalar and null cases, each
			// of which would otherwise silently produce a wrong parent — an
			// alias node's Value is the ANCHOR'S NAME, so `extends: *base`
			// would become the string "base", which might even name a real
			// environment.
			text, ok := requireScalar(path, "environment "+strconv.Quote(name)+"'s `extends`", val, ds)
			if !ok {
				continue
			}
			if env.Extends != "" {
				ds.Add(diag.Diagnostic{
					Severity: diag.SeverityError,
					Summary:  "`extends` is set more than once on environment " + strconv.Quote(name),
					Detail:   "The last assignment would silently win, changing which values " + strconv.Quote(name) + " inherits. It is also set at " + describeOrigin(env.ExtendsOrigin) + ".",
					Action:   "Remove one of the two assignments.",
					Origin:   keyOrigin,
				})
				continue
			}
			env.Extends, env.ExtendsOrigin = text, keyOrigin

		case "variables":
			// PLAN.md §6's spelling.
			if val.Kind != yaml.MappingNode {
				ds.Add(diag.Diagnostic{
					Severity: diag.SeverityError,
					Summary:  "`variables` in environment " + strconv.Quote(name) + " must be a mapping",
					Origin:   originOf(path, val),
				})
				continue
			}
			for j := 0; j+1 < len(val.Content); j += 2 {
				addOverride(path, env, val.Content[j], val.Content[j+1], ds)
			}

		case "type":
			// PLAN.md §13, withdrawn. Refused rather than left alone, because
			// "left alone" is not inert: every other key in this block becomes a
			// variable override, so `type: production` silently declared a
			// VARIABLE named `type` and classified nothing. Someone writing it
			// expects it to do something, and a key that quietly does something
			// else is worse than one that is rejected.
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "environment " + strconv.Quote(name) + " sets `type`, which no longer means anything",
				Detail: "Environment classification was withdrawn (PLAN.md §13): a provider default is one " +
					"value per attribute, and anything that differs between environments is a variable. " +
					"Left in place, `type:` would declare a variable named \"type\" — which is what it did " +
					"before this became an error.",
				Action: "Remove it. To vary a value by environment, set that value in this environment's " +
					"variables. For production protections, see PLAN.md §38.",
				Origin: keyOrigin,
			})

		default:
			// PLAN.md §7's spelling: any other key is an override.
			addOverride(path, env, key, val, ds)
		}
	}
}

// addOverride records one environment override, refusing a second setting of
// the same name.
//
// A collision is reachable three ways — the two spellings within one
// environment, the same key twice in one file, and the block plus the file —
// and every one of them silently discards a value the user wrote, producing a
// plan that is wrong with nothing printed.
func addOverride(path string, env *EnvironmentDecl, key, val *yaml.Node, ds *diag.Diagnostics) {
	origin := originOf(path, key)
	// APPENDS IN DOCUMENT ORDER. That is not the order the field promises:
	// Decode sorts every environment's Overrides by name once, after all files
	// are decoded, which is the only point at which the block's and the file's
	// contributions are both present. Do not add a sort here — a reader who
	// looks only at this function concludes the slice is unsorted, which is a
	// real misreading that has already happened once during review.
	//
	// The linear scan below is what lets the duplicate diagnostic name where
	// the FIRST assignment was.
	for _, existing := range env.Overrides {
		if existing.Name == key.Value {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  strconv.Quote(key.Value) + " is set more than once in environment " + strconv.Quote(env.Name),
				Detail:   "The last assignment would silently win. It is also set at " + describeOrigin(existing.Origin) + ".",
				Action:   "Remove one of the two assignments.",
				Origin:   origin,
			})
			return
		}
	}

	v, hasExpr := decodeValue(path, "environment "+strconv.Quote(env.Name)+"'s "+strconv.Quote(key.Value), val, ds)
	if hasExpr {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "environment override " + strconv.Quote(key.Value) + " contains an interpolation",
			Detail:   "Environment overrides are resolved before any expression scope exists, so `${...}` here has nothing to refer to.",
			Action:   "Write a literal value.",
			Origin:   originOf(path, val),
		})
		return
	}
	env.Overrides = append(env.Overrides, OverrideDecl{
		Name:   key.Value,
		Value:  retagSource(v, value.SourceEnvironment, value.ScopeUnset, "").WithOrigin(origin),
		Origin: origin,
	})
}
