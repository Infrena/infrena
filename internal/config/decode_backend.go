package config

import (
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/infrena/infrena/internal/diag"
)

// backendBlockHint shows the shape a backend-shaped block takes, for every
// diagnostic that has to correct one. One copy serving both blocks, so the
// examples cannot drift apart.
func backendBlockHint(block string) string {
	return block + ":\n  plugin: s3        # the only key infrena reads\n  bucket: my-state  # everything else is the backend's own configuration"
}

// backendBlockTexts carries the only sentences that differ between the two
// blocks: what leaving the block out means. Nothing else is parameterised,
// because a block that could differ in more than this could disagree with the
// other about what a key means.
type backendBlockTexts struct {
	absentMeans  string
	absentAction string
}

func backendTextsFor(block string) backendBlockTexts {
	if block == "migrate_from" {
		return backendBlockTexts{
			absentMeans: "Leaving the block out entirely is how a project says there is no " +
				"migration to perform.",
			absentAction: "Add `plugin: <name>`, or remove the `migrate_from:` block if there is " +
				"nothing to migrate.",
		}
	}
	return backendBlockTexts{
		absentMeans:  "Removing the block entirely is how a project asks for the local backend.",
		absentAction: "Add `plugin: <name>`, or remove the `backend:` block to keep state local.",
	}
}

// decodeBackend decodes `backend:`, where this project's state lives.
func decodeBackend(path string, key, node *yaml.Node, out *ProjectDecl, ds *diag.Diagnostics) {
	decodeBackendBlock(path, "backend", key, node, &out.Backend, ds)
}

// decodeMigrateFrom decodes `migrate_from:`, the other end of a migration.
//
// It is the same code as `backend:`, not a copy of it: a migration that read
// its source by different rules from its destination could move state
// somewhere nobody configured.
func decodeMigrateFrom(path string, key, node *yaml.Node, out *ProjectDecl, ds *diag.Diagnostics) {
	decodeBackendBlock(path, "migrate_from", key, node, &out.MigrateFrom, ds)
}

// decodeBackendBlock decodes one backend-shaped block into out.
//
// It reserves one key. `plugin:` names the implementation and is the engine's;
// every other key belongs to the backend and crosses to it untouched. So this
// is the one shape in the language where an unrecognised key is not an error:
// infrena cannot know whether an s3 backend accepts `profile`, and refusing
// what it does not recognise would make every backend option a change to the
// core. A missing `plugin:` is an error, because without it there is no
// backend to hand the rest of the block to.
//
// `plugin: local` decodes here like any other name: naming the built-in
// backend is how a migration says "local" without relying on a block being
// absent.
func decodeBackendBlock(
	path, block string, key, node *yaml.Node, out *BackendDecl, ds *diag.Diagnostics,
) {
	origin := originOf(path, key)
	texts := backendTextsFor(block)

	if node.Kind != yaml.MappingNode {
		detail := "It names one backend and configures it. Read as anything else it would " +
			"carry no plugin name, which is indistinguishable from declaring no `" + block +
			":` at all. "
		if block == "backend" {
			detail += "That silently means local, so state would be written somewhere other " +
				"than where this block asked for."
		} else {
			detail += "That means no migration, so `infrena state migrate` would report " +
				"nothing to do rather than reading the state this block points at."
		}
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "`" + block + "` must be a mapping",
			Detail:   detail,
			Action:   "Write it as:\n" + backendBlockHint(block),
			Origin:   originOf(path, node),
		})
		// The block is recorded even though it did not decode, carrying its
		// origin and no plugin. The zero value would say "this project
		// declared no backend", which means local — and falling back to local
		// here would write state somewhere other than where the block asked
		// for.
		*out = BackendDecl{Origin: originOf(path, node)}
		return
	}

	d := BackendDecl{Config: map[string]any{}, Origin: origin}
	for i := 0; i+1 < len(node.Content); i += 2 {
		k, val := node.Content[i], node.Content[i+1]

		if k.Value == "plugin" {
			text, ok := requireScalar(path, "`"+block+"`'s `plugin`", val, ds)
			if !ok {
				continue
			}
			if !backendValueIsLiteral(path, block, "plugin", val, ds) {
				continue
			}
			d.Plugin = text
			continue
		}

		if !backendValueIsLiteral(path, block, k.Value, val, ds) {
			continue
		}

		// Decoded straight into `any`, not into a value.Value: a Kind would
		// have to be invented for a key this package has never heard of, and
		// anything it understood about `bucket` is something it could disagree
		// with the backend about.
		var v any
		if err := val.Decode(&v); err != nil {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "`" + block + "." + k.Value + "` could not be read",
				Detail:   err.Error(),
				Action:   "Check the YAML under `" + block + ":`.",
				Origin:   originOf(path, val),
			})
			continue
		}
		d.Config[k.Value] = v
	}

	if d.Plugin == "" {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "`" + block + "` has no `plugin`",
			Detail: "Every other key in this block is the backend's own configuration, so " +
				"without `plugin:` there is nothing to hand it to. " + texts.absentMeans,
			Action: texts.absentAction,
			Origin: origin,
		})
		// Recorded without a plugin, for the reason above: a block that is
		// present and unreadable is not a project that declared no backend.
		*out = BackendDecl{Origin: origin}
		return
	}

	*out = d
}

// backendValueIsLiteral reports whether a value under `backend:` or
// `migrate_from:` is free of interpolation, adding a diagnostic naming the key
// if it is not.
//
// A variable here can never be resolved. `providers:` may interpolate, because
// provider instances are constructed after variables resolve; state cannot,
// because state is read before any compile and some commands never compile at
// all. That is an ordering cycle, not a missing feature. `migrate_from:` is
// refused for the identical reason and in the identical words.
//
// It walks the whole value rather than its top level, because a `${...}`
// nested three maps down reaches the backend by the same route.
func backendValueIsLiteral(path, block, keyPath string, node *yaml.Node, ds *diag.Diagnostics) bool {
	switch node.Kind {
	case yaml.SequenceNode:
		ok := true
		for i, item := range node.Content {
			if !backendValueIsLiteral(path, block, keyPath+"["+strconv.Itoa(i)+"]", item, ds) {
				ok = false
			}
		}
		return ok

	case yaml.MappingNode:
		ok := true
		for i := 0; i+1 < len(node.Content); i += 2 {
			if !backendValueIsLiteral(path, block, keyPath+"."+node.Content[i].Value, node.Content[i+1], ds) {
				ok = false
			}
		}
		return ok

	case yaml.AliasNode:
		// yaml.Node.Decode resolves an alias to its anchor, so the text that
		// reaches the backend is the anchor's. Scanning the alias node itself
		// would scan the anchor's name and miss a `${...}` in its value.
		if node.Alias == nil {
			return true
		}
		return backendValueIsLiteral(path, block, keyPath, node.Alias, ds)

	default:
		if !strings.Contains(node.Value, "${") {
			return true
		}
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary: "`" + block + "." + keyPath +
				"` uses a variable, and a variable here can never be resolved",
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
