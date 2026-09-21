package config

import (
	"strconv"

	"gopkg.in/yaml.v3"

	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/pkg/value"
)

// ParseVariableFile parses raw bytes into the File shape DecodeVariableFile
// expects, for a variable file named directly by path (a --var-file) rather
// than one Load discovers by walking a project directory.
//
// It exists so that this package stays the only one that constructs a
// yaml.Node. It deliberately does not read the file: the caller keeps its own
// read- and parse-failure diagnostics, worded for the flag it knows about.
//
// path is stored on the returned File verbatim, never resolved or re-joined,
// so a diagnostic can name the path as the user typed it rather than a
// --chdir-joined one.
func ParseVariableFile(path string, data []byte) (File, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return File{}, err
	}
	return File{Path: path, Kind: FileVariables, Root: &root}, nil
}

// DecodeVariableFile converts one variable file into named values. The shape
// is variables.yml: a single YAML mapping of variable name to value, where a
// value may be a scalar, a sequence or a mapping.
//
// This is the only decoder for that shape. variables.yml and a --var-file both
// come through here rather than each walking the mapping themselves.
//
// scope is a parameter because one decoder serves two precedence levels:
// variables.yml decodes at ScopeUnset, since this stage declares rather than
// resolves and later stages decide which level wins, while a --var-file
// decodes straight to ScopeCLIOverride because the CLI loads it once
// resolution order is already known. Source is SourceVariable either way;
// Scope, not Source, records which level won.
//
// An empty file is not an error: a project may keep an empty variables.yml
// under version control. A non-mapping top level is. So is a value containing
// `${`: an expression here would need its own evaluation order, and accepting
// one verbatim would put the literal text into a resource attribute.
func DecodeVariableFile(f File, scope value.Scope) (map[string]value.Value, diag.Diagnostics) {
	var ds diag.Diagnostics
	out := map[string]value.Value{}

	// SuppliedBy names f.Path only for a --var-file, and stays "" for
	// variables.yml. f.Path is the path as typed; see ParseVariableFile.
	var suppliedBy string
	if scope == value.ScopeCLIOverride {
		suppliedBy = f.Path
	}

	root := documentRoot(f.Root)
	if root == nil {
		return out, ds
	}
	if root.Kind != yaml.MappingNode {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "variable file must be a mapping of variable names to values",
			Detail:   "The top level of " + f.Path + " is not a mapping.",
			Action:   "Write one `name: value` pair per line.",
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

		// A key that names one of infrena.yml's own blocks is almost certainly
		// the wrong file. A warning rather than an error: a variable may
		// legitimately be called "project", and refusing it would break a
		// valid configuration in order to catch a mistake.
		switch key.Value {
		case "project", "resources", "variables", "environments":
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityWarning,
				Summary:  strconv.Quote(key.Value) + " in " + f.Path + " is a variable, not a configuration block",
				Detail: f.Path + " is a flat mapping of variable name to value. " +
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
		out[key.Value] = retagSource(v, value.SourceVariable, scope, suppliedBy).WithOrigin(origin)
	}
	return out, ds
}
