package config

import (
	"strconv"

	"gopkg.in/yaml.v3"

	"infra/internal/diag"
	"infra/pkg/value"
)

// ParseVariableFile parses raw bytes into the File shape DecodeVariableFile
// expects, for a variable file named directly by path — a --var-file, read by
// internal/cli's loadVarFiles — rather than one Load discovers by walking a
// project directory.
//
// It exists so internal/cli never constructs a yaml.Node itself. Stage 2
// (this package) is the only stage permitted to touch yaml.Node, and that
// rule is enforced by nothing more structural than
// `grep -rn "yaml\." internal/ pkg/ cmd/ | grep -v "^internal/config/"`
// returning nothing — a check worth keeping meaningful, because the day it
// returns one legitimate hit is the day a second one stops looking unusual.
//
// It deliberately does NOT read the file itself. Turning a path into bytes is
// internal/cli's own concern — resolving a --var-file against --chdir vs. an
// absolute path touches no yaml.Node — and internal/cli keeps its own
// read-failure and parse-failure diagnostics, worded for the --var-file flag
// it knows about, which config.File deliberately does not (see
// DecodeVariableFile's doc comment: this package stays provider- and
// flag-agnostic).
//
// path is stored on the returned File verbatim — never resolved or
// re-joined — so a caller that wants a diagnostic naming the path AS THE USER
// TYPED IT, rather than a --chdir-joined path, passes that spelling here
// rather than the path it actually opened.
func ParseVariableFile(path string, data []byte) (File, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return File{}, err
	}
	return File{Path: path, Kind: FileVariables, Root: &root}, nil
}

// DecodeVariableFile converts one variable file into named values.
//
// The shape is PLAN.md §8's variables.yml: a single YAML mapping of variable
// name to value. Sequences and mappings are allowed as well as scalars,
// because PLAN.md §9's variable types include list and map.
//
// This is the ONLY decoder for that shape. variables.yml (via
// decodeVariableValues, called from Decode's FileVariables case) and a
// --var-file (via internal/cli's loadVarFiles) both call this function rather
// than each walking the mapping themselves — internal/compiler/bind.go's
// variableScope being one of two implementations of one concept is the defect
// class that leaked a plaintext secret in M2, and the M4 contract calls it out
// by name.
//
// scope is a parameter, not a constant, because this one decoder serves two
// precedence levels: variables.yml decodes at ScopeUnset — Decode is stage 2,
// it declares rather than resolves, and which precedence level wins is stages
// 3 and 4's answer alone (see pkg/value/scope.go) — while a --var-file decodes
// straight to ScopeCLIOverride, because loadVarFiles runs at the CLI layer,
// after resolution order is already known, with no later stage left to stamp
// it. ScopeUnset is Scope's zero value, so variables.yml's callers stamping it
// is indistinguishable from never stamping it at all.
//
// Source is SourceVariable for every entry — a variable file supplies
// variables, whichever level it was loaded at. Scope, not Source, records
// which level won.
//
// An empty file is not an error: a project may keep an empty variables.yml
// under version control. A non-mapping top level is an error. A value
// containing `${` is an error: an expression inside a variable file would
// need its own evaluation order (a variable referring to a variable), which
// PLAN.md §10 explicitly does not want, and accepting one verbatim would put
// the literal text `${foo}` into a resource attribute — a silent wrong
// answer.
func DecodeVariableFile(f File, scope value.Scope) (map[string]value.Value, diag.Diagnostics) {
	var ds diag.Diagnostics
	out := map[string]value.Value{}

	root := documentRoot(f.Root)
	if root == nil {
		return out, ds
	}
	if root.Kind != yaml.MappingNode {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "variable file must be a mapping of variable names to values",
			Detail:   "The top level of " + f.Path + " is not a mapping.",
			Action:   "Write one `name: value` pair per line, as in PLAN.md §8's variables.yml.",
			Origin:   originOf(f.Path, root),
		})
		return out, ds
	}

	for i := 0; i+1 < len(root.Content); i += 2 {
		key, val := root.Content[i], root.Content[i+1]
		if key.Kind != yaml.ScalarNode {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "variable name must be a plain scalar",
				Detail:   "A composite key cannot name a variable.",
				Action:   "Use a plain name, such as `replicas: 3`.",
				Origin:   originOf(f.Path, key),
			})
			continue
		}
		origin := originOf(f.Path, key)

		// A key that names one of infra.yml's own blocks is almost certainly
		// the wrong file. A warning rather than an error: a variable may
		// legitimately be called "project", and refusing it would break a
		// valid configuration in order to catch a mistake.
		switch key.Value {
		case "project", "resources", "variables", "environments":
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityWarning,
				Summary:  strconv.Quote(key.Value) + " in " + f.Path + " is a variable, not a configuration block",
				Detail: f.Path + " is a flat mapping of variable name to value (PLAN.md §8). " +
					"A `" + key.Value + "` block belongs in " + ProjectFileName + " — in particular, variable DECLARATIONS " +
					"(`type`, `default`, `min`, `max`) live only under " + ProjectFileName + "'s `variables:` key, so that " +
					"where a variable is declared has one answer.",
				Action: "Move it to " + ProjectFileName + ", or ignore this if you really do have a variable called " + strconv.Quote(key.Value) + ".",
				Origin: origin,
			})
		}

		if first, dup := out[key.Value]; dup {
			// Reported rather than resolved: with two entries of one name in
			// one file, neither answer is defensible, and picking one
			// silently means the user's other line does nothing.
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "variable " + strconv.Quote(key.Value) + " is set more than once in " + f.Path,
				Detail:   "The last assignment would silently win. It is also set at " + describeOrigin(first.Origin) + ".",
				Action:   "Remove one of the two assignments.",
				Origin:   origin,
			})
			continue
		}

		v, hasExpr := decodeValue(f.Path, "variable "+strconv.Quote(key.Value), val, &ds)
		if hasExpr {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "variable " + strconv.Quote(key.Value) + " contains an interpolation",
				Detail:   f.Path + " is resolved before any expression scope exists, so `${...}` here has nothing to refer to.",
				Action:   "Write a literal value.",
				Origin:   originOf(f.Path, val),
			})
			continue
		}
		out[key.Value] = retagSource(v, value.SourceVariable, scope).WithOrigin(origin)
	}
	return out, ds
}
