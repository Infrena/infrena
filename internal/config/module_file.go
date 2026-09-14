package config

import (
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/pkg/value"
)

// OutputDecl is one value a module publishes to its caller (PLAN.md §11.3).
type OutputDecl struct {
	Name string
	// Value is the output's `value:`, decoded exactly as a resource attribute
	// is: the text kept verbatim, with HasExpressions saying whether it carries
	// an interpolation. Stage 5 parses it and evaluates it in the MODULE's
	// scope, and the result may legitimately be UNKNOWN — an output usually
	// reads a computed attribute of a resource the plan has not created yet
	// (contract Ruling 5), which must stay unknown through the caller's
	// evaluation rather than collapsing to an empty string.
	//
	// Value.Origin is the `value:` node's own position, so a diagnostic about
	// the EXPRESSION points at the expression while Origin below points at the
	// output's name. That is why there is no third field.
	Value          value.Value
	HasExpressions bool
	Origin         value.Origin
}

// ModuleFile is a decoded module.yml: the module's own half of PLAN.md §11, as
// distinct from ModuleLoadDecl, which is one line in a caller's `modules:` list.
//
// Distinct from ProjectDecl: a module has no project name, no variables.yml and
// no environments (§11.3). That is enforced by construction rather than by
// validation — this decoder has no case for `project`, `variables` or
// `environments`, so each falls out of the unknown-key diagnostic, which already
// names the key and the line. Do not add bespoke cases for them; the absence is
// the feature (contract Amendment 1).
type ModuleFile struct {
	// Inputs are the module's declared inputs. §11.3: spelled exactly as §9's
	// `variables:`, same `type`, same `default`, same bounds — which is what
	// lets stage 5 type them by calling variables.Schemas(mf.Inputs), the
	// checker M4 already built, rather than growing a second one (Ruling 3).
	//
	// Sorted by Name.
	Inputs []VariableDecl
	// Resources are the module's own resources, sorted by Name. Stage 5 re-roots
	// each under the instantiating resource's name.
	Resources []*ResourceDecl
	// Modules are this module's own `modules:` entries, sorted by Name. MODULES
	// NEST: §11.3 says a module may declare `modules:`, and spec §7.2 bounds the
	// recursion at depth 32 rather than forbidding it — which is also what gives
	// Ruling 6's depth bound and cycle detector anything to detect.
	Modules []ModuleLoadDecl
	// Outputs are the values this module publishes, sorted by Name.
	Outputs []OutputDecl
	// Origin is the document root. Origin.File is the decoded file's path, which
	// is what stage 5 resolves a nested relative source against — there is
	// deliberately no separate Path field.
	Origin value.Origin
}

// DecodeModule converts one loaded module file into typed declarations (compiler
// stage 2).
//
// Separate from Decode because the documents are different shapes, and because a
// module file is decoded once per SOURCE while Decode runs once per project: two
// resources instantiating the same module share this result and differ only in
// the attributes their callers wrote.
func DecodeModule(f File) (*ModuleFile, diag.Diagnostics) {
	var ds diag.Diagnostics
	out := &ModuleFile{}

	doc := documentRoot(f.Root)
	if doc == nil {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "module file is empty",
			Detail:   "A module declares `inputs`, `resources` and `outputs` (PLAN.md §11.3). An empty one instantiates nothing, so every reference to its outputs would fail with no explanation of why.",
			Action:   "Add a `resources` block, or remove the `modules:` entry that loads this directory.",
			Origin:   value.Origin{File: f.Path},
		})
		return out, ds
	}
	if doc.Kind != yaml.MappingNode {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "module file must be a mapping",
			Detail:   topLevelShapeDetail(File{Path: f.Path, Kind: FileModule}),
			Origin:   originOf(f.Path, doc),
		})
		return out, ds
	}
	out.Origin = originOf(f.Path, doc)

	seenResources := map[string]value.Origin{}
	seenModules := map[string]loadedName{}
	seenInputs := map[string]value.Origin{}
	seenOutputs := map[string]value.Origin{}
	seenBlocks := map[string]value.Origin{}

	for i := 0; i+1 < len(doc.Content); i += 2 {
		key, val := doc.Content[i], doc.Content[i+1]
		keyOrigin := originOf(f.Path, key)

		if first, dup := seenBlocks[key.Value]; dup {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "`" + key.Value + "` is declared more than once in this module file",
				Detail:   "The last block would silently win, discarding everything in the first. It is also declared at " + describeOrigin(first) + ".",
				Action:   "Merge the two blocks.",
				Origin:   keyOrigin,
			})
			continue
		}
		seenBlocks[key.Value] = keyOrigin

		switch key.Value {
		case "inputs":
			decodeModuleInputs(f.Path, val, out, &ds, seenInputs)
		case "resources":
			decodeResources(f.Path, val, &out.Resources, &ds, seenResources)
		case "modules":
			// Modules nest. Task 2's decoder, not a second one: what a
			// `modules:` entry means must not depend on which document it
			// appears in.
			decodeModuleLoads(f.Path, val, &out.Modules, &ds, seenModules)
		case "outputs":
			decodeOutputs(f.Path, val, out, &ds, seenOutputs)
		default:
			// An ERROR, where decodeDocument's equivalent is a warning. The two
			// surfaces differ: infra.yml's top level is still growing, so a key
			// this version does not know may be one a later version adds, and a
			// warning keeps an older binary usable. A module file's surface is
			// exactly four keys, the blast radius of a typo'd or misplaced block
			// is the WHOLE block silently dropped, and the keys most likely to
			// appear here wrongly — `project`, `variables`, `environments` — are
			// ones §11.3 says a module must never have.
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "unknown key " + strconv.Quote(key.Value) + " in module file",
				Detail:   "A module file declares `inputs`, `resources`, `modules` and `outputs`, and nothing else. A module has no project name, no variables of its own and no environments: the values it sees are its inputs plus the ambient `environment`, `region` and `account` (PLAN.md §11.3).",
				Action:   "Remove " + strconv.Quote(key.Value) + ", or move it under `inputs:` if it is a parameter of this module.",
				Origin:   keyOrigin,
			})
		}
	}

	// Sorted ONCE, here, so no consumer has to, and so two runs of the same
	// module file expand in the same order (invariant 6).
	sort.Slice(out.Inputs, func(i, j int) bool { return out.Inputs[i].Name < out.Inputs[j].Name })
	sort.Slice(out.Resources, func(i, j int) bool { return out.Resources[i].Name < out.Resources[j].Name })
	sort.Slice(out.Modules, func(i, j int) bool { return out.Modules[i].Name < out.Modules[j].Name })
	sort.Slice(out.Outputs, func(i, j int) bool { return out.Outputs[i].Name < out.Outputs[j].Name })
	return out, ds
}

// decodeModuleInputs decodes a module's `inputs:` block: typed declarations,
// identical in shape to infra.yml's `variables:` (PLAN.md §11.3).
//
// It reuses decodeVariable so an input's `type`, `default`, `min` and `max` mean
// exactly what a variable's do — and so stage 5 can type an input with
// variables.Schemas rather than a second checker (contract Ruling 3). The
// declNoun is what keeps the diagnostics talking about inputs.
func decodeModuleInputs(path string, node *yaml.Node, out *ModuleFile, ds *diag.Diagnostics, seen map[string]value.Origin) {
	if node.Kind != yaml.MappingNode {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "`inputs` must be a mapping of input name to declaration",
			Detail:   "Each input is a key with `type`, `default`, `min` and `max` beneath it (PLAN.md §11.3).",
			Action:   "Write `replicas:` with `type: integer` beneath it, for example.",
			Origin:   originOf(path, node),
		})
		return
	}

	for i := 0; i+1 < len(node.Content); i += 2 {
		nameNode, body := node.Content[i], node.Content[i+1]
		origin := originOf(path, nameNode)

		if first, dup := seen[nameNode.Value]; dup {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "input " + strconv.Quote(nameNode.Value) + " is declared more than once",
				Detail:   "The last declaration would silently win, discarding a type or a bound. " + strconv.Quote(nameNode.Value) + " is also declared at " + describeOrigin(first) + ".",
				Action:   "Remove one of the two declarations, or merge them.",
				Origin:   origin,
			})
			continue
		}
		seen[nameNode.Value] = origin
		out.Inputs = append(out.Inputs, decodeVariable(path, nameNode.Value, body, origin, inputNoun, ds))
	}
}

func decodeOutputs(path string, node *yaml.Node, out *ModuleFile, ds *diag.Diagnostics, seen map[string]value.Origin) {
	if node.Kind != yaml.MappingNode {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "`outputs` must be a mapping of output name to declaration",
			Detail:   "Each output is a key with `value:` beneath it (PLAN.md §11.3).",
			Action:   "Write `endpoint:` with `value: ${service.endpoint}` beneath it, for example.",
			Origin:   originOf(path, node),
		})
		return
	}

	for i := 0; i+1 < len(node.Content); i += 2 {
		nameNode, body := node.Content[i], node.Content[i+1]
		origin := originOf(path, nameNode)

		if first, dup := seen[nameNode.Value]; dup {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "output " + strconv.Quote(nameNode.Value) + " is declared more than once",
				Detail:   "The last declaration would silently win, so the module would publish a value its author did not write. " + strconv.Quote(nameNode.Value) + " is also declared at " + describeOrigin(first) + ".",
				Action:   "Remove one of the two declarations.",
				Origin:   origin,
			})
			continue
		}
		seen[nameNode.Value] = origin

		if o, ok := decodeOutput(path, nameNode.Value, body, origin, ds); ok {
			out.Outputs = append(out.Outputs, o)
		}
	}
}

func decodeOutput(path, name string, body *yaml.Node, origin value.Origin, ds *diag.Diagnostics) (OutputDecl, bool) {
	if body.Kind != yaml.MappingNode {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "output " + strconv.Quote(name) + " must be a mapping",
			Detail:   "An output holds a `value:` key. A bare value here would be ambiguous with an output whose value is itself a mapping.",
			Action:   "Write `value:` beneath " + strconv.Quote(name) + ", with the expression under it.",
			Origin:   originOf(path, body),
		})
		return OutputDecl{}, false
	}

	o := OutputDecl{Name: name, Origin: origin}
	var valueNode *yaml.Node
	seenKeys := map[string]value.Origin{}

	for i := 0; i+1 < len(body.Content); i += 2 {
		key, val := body.Content[i], body.Content[i+1]
		keyOrigin := originOf(path, key)
		if first, dup := seenKeys[key.Value]; dup {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  strconv.Quote(key.Value) + " is set more than once on output " + strconv.Quote(name),
				Detail:   "The last assignment would silently win. " + strconv.Quote(key.Value) + " is also set at " + describeOrigin(first) + ".",
				Action:   "Remove one of the two assignments.",
				Origin:   keyOrigin,
			})
			continue
		}
		seenKeys[key.Value] = keyOrigin

		switch key.Value {
		case "value":
			valueNode = val
		default:
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "unknown key " + strconv.Quote(key.Value) + " in output " + strconv.Quote(name),
				Detail:   "An output declaration understands `value`.",
				Action:   "Remove " + strconv.Quote(key.Value) + ".",
				Origin:   keyOrigin,
			})
		}
	}

	if valueNode == nil {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "output " + strconv.Quote(name) + " has no `value`",
			Detail:   "An output publishes one value to the module's caller, and this one publishes nothing — so a caller reading it would get an unresolvable reference with no explanation.",
			Action:   "Add `value: ${...}` beneath " + strconv.Quote(name) + ", or remove the output.",
			Origin:   origin,
		})
		return OutputDecl{}, false
	}

	if bare, ok := bareReference(valueNode); ok {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "output " + strconv.Quote(name) + " names " + strconv.Quote(bare) + " without `${...}`",
			Detail: "Written bare, " + strconv.Quote(bare) + " is that literal text, not a reference to it, so the module would publish the text " +
				strconv.Quote(bare) + " to its caller. References are written `${...}` everywhere else in the language.",
			Action: "Write `value: ${" + bare + "}` to publish the value it names, or quote it — `value: " + strconv.Quote(bare) + "` — to publish the literal text.",
			Origin: originOf(path, valueNode),
		})
		return OutputDecl{}, false
	}

	o.Value, o.HasExpressions = decodeValue(path, "output "+strconv.Quote(name)+"'s `value`", valueNode, ds)
	return o, true
}

// bareReference reports whether an output's `value:` was written bare —
// `value: service.endpoint` rather than `value: ${service.endpoint}`.
//
// Contract Amendment 4e: `${...}` is this language's only reference syntax, with
// `$${` as its escape, so a bare scalar is a literal everywhere else, and making
// outputs the one exception would be a second reference syntax. Decoding it as an
// ordinary value would publish that literal text with nothing printed; reading any
// dotted scalar as a reference is worse, because `value: production` is an
// ordinary literal output and a dotted one — a version, a hostname — is just as
// ordinary, so that rule makes every dotted literal ambiguous. Guessing between
// the two is precisely what produces a confidently wrong value.
//
// So the bare form is refused and BOTH working spellings are offered.
//
// The test is deliberately narrow: an UNQUOTED string scalar (Style 0, tag !!str)
// of two or more identifier segments. A quoted scalar means the literal, an
// integer or a boolean is not a reference in any spelling, and a single segment
// would collide with an ordinary one-word literal.
//
// identifierSegment is Task 2's, shared so that what counts as a name has one
// definition.
func bareReference(node *yaml.Node) (string, bool) {
	if node.Kind != yaml.ScalarNode || node.Style != 0 || node.Tag != "!!str" {
		return "", false
	}
	// EXACTLY one dot. Two segments is where the ambiguity lives —
	// `service.endpoint` reads as a reference to a resource's attribute, which
	// is this language's shape for one. Three or more is a hostname:
	// `db.example.com` and `api.internal.corp` are ordinary output values, and
	// an output is the single most likely place in a module to publish one.
	// Refusing those told the user they had forgotten `${...}` when they had
	// not, which is worse than saying nothing (measured: both were REFUSED
	// before this bound, alongside `service.endpoint`).
	segments := strings.Split(node.Value, ".")
	if len(segments) != 2 {
		return "", false
	}
	for _, s := range segments {
		if !identifierSegment(s) {
			return "", false
		}
	}
	return node.Value, true
}
