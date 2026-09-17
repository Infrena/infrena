package config

import (
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/infrena/infrena/internal/diag"
)

// backendBlockHint shows the shape `backend:` takes, for every diagnostic that
// has to correct it. One copy, because a reader given two spellings of the same
// example has to work out which is current.
const backendBlockHint = "backend:\n  plugin: s3        # the only key infrena reads\n  bucket: my-state  # everything else is the backend's own configuration"

// decodeBackend decodes `backend:` (PLAN.md §52).
//
// IT RESERVES ONE KEY. `plugin:` names the implementation and is the engine's;
// every other key belongs to the backend and crosses to it untouched. So this
// is the one block in the language where an unrecognised key is NOT an error:
// infrena cannot know whether an s3 backend accepts `profile`, and refusing
// what it does not recognise would make every backend option a change to the
// core. A missing `plugin:` IS an error, because that is the key infrena owns
// and without it there is no backend to hand the rest of the block to.
func decodeBackend(path string, key, node *yaml.Node, out *ProjectDecl, ds *diag.Diagnostics) {
	origin := originOf(path, key)

	if node.Kind != yaml.MappingNode {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "`backend` must be a mapping",
			Detail: "It names one backend and configures it. Read as anything else it would " +
				"carry no plugin name, which is indistinguishable from declaring no backend " +
				"at all — and that silently means local, so state would be written somewhere " +
				"other than where this block asked for.",
			Action: "Write it as:\n" + backendBlockHint,
			Origin: originOf(path, node),
		})
		return
	}

	d := BackendDecl{Config: map[string]any{}, Origin: origin}
	for i := 0; i+1 < len(node.Content); i += 2 {
		k, val := node.Content[i], node.Content[i+1]

		if k.Value == "plugin" {
			text, ok := requireScalar(path, "`backend`'s `plugin`", val, ds)
			if !ok {
				continue
			}
			if !backendValueIsLiteral(path, "plugin", val, ds) {
				continue
			}
			d.Plugin = text
			continue
		}

		if !backendValueIsLiteral(path, k.Value, val, ds) {
			continue
		}

		// Decoded straight into `any`, not into a value.Value. A value.Value
		// carries a Kind this package would have to invent for a key it has
		// never heard of, and the backend is going to be handed literal JSON
		// regardless. Anything this package understood about `bucket` would be
		// something it could disagree with the backend about.
		var v any
		if err := val.Decode(&v); err != nil {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "`backend." + k.Value + "` could not be read",
				Detail:   err.Error(),
				Action:   "Check the YAML under `backend:`.",
				Origin:   originOf(path, val),
			})
			continue
		}
		d.Config[k.Value] = v
	}

	if d.Plugin == "" {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "`backend` has no `plugin`",
			Detail: "Every other key in this block is the backend's own configuration, so " +
				"without `plugin:` there is nothing to hand it to. Removing the block " +
				"entirely is how a project asks for the local backend.",
			Action: "Add `plugin: <name>`, or remove the `backend:` block to keep state local.",
			Origin: origin,
		})
		return
	}

	out.Backend = d
}

// backendValueIsLiteral reports whether a value under `backend:` is free of
// interpolation, adding a diagnostic naming the key if it is not.
//
// A VARIABLE HERE CAN NEVER BE RESOLVED, and the diagnostic has to say why or
// the reader goes and declares one.
//
// `providers:` MAY interpolate, because compiler stage 4.5 constructs provider
// instances after variables resolve. State cannot: state is read BEFORE any
// compile — `destroy`, `refresh`, `discover` and `import` never compile at all
// — so there is no point at which a value here could be filled in. It is a
// cycle, not a missing feature, and a future request to "just support variables
// in backend:" is a request to break that ordering.
//
// It walks the whole value rather than its top level, because a `${...}` nested
// three maps down reaches the backend by exactly the same route.
func backendValueIsLiteral(path, keyPath string, node *yaml.Node, ds *diag.Diagnostics) bool {
	switch node.Kind {
	case yaml.SequenceNode:
		ok := true
		for i, item := range node.Content {
			if !backendValueIsLiteral(path, keyPath+"["+strconv.Itoa(i)+"]", item, ds) {
				ok = false
			}
		}
		return ok

	case yaml.MappingNode:
		ok := true
		for i := 0; i+1 < len(node.Content); i += 2 {
			if !backendValueIsLiteral(path, keyPath+"."+node.Content[i].Value, node.Content[i+1], ds) {
				ok = false
			}
		}
		return ok

	case yaml.AliasNode:
		// yaml.Node.Decode resolves an alias to its anchor, so the text that
		// reaches the backend is the anchor's. Scanning the alias node itself
		// would scan the anchor's NAME and miss a `${...}` in its value.
		if node.Alias == nil {
			return true
		}
		return backendValueIsLiteral(path, keyPath, node.Alias, ds)

	default:
		if !strings.Contains(node.Value, "${") {
			return true
		}
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "`backend." + keyPath + "` uses a variable, and a variable here can never be resolved",
			Detail: "State is read before anything is compiled, and compiling is what resolves " +
				"`${...}`. `destroy`, `refresh`, `discover` and `import` never compile at all, " +
				"and every other command has to open the backend to find out what already " +
				"exists. So there is no point in any run at which this value could be filled " +
				"in. It is an ordering cycle rather than a feature infrena has not got round " +
				"to: reading state would have to wait for a compile that is waiting for state.",
			Action: "Give `" + keyPath + "` a literal value. If it has to differ between " +
				"environments, keep those environments in separate projects, or let the " +
				"backend read it from the surroundings the way a provider reads credentials.",
			Origin: originOf(path, node),
		})
		return false
	}
}
