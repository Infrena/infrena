package config

import (
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/infrata/infrata/internal/diag"
	"github.com/infrata/infrata/internal/modules/source"
	"github.com/infrata/infrata/pkg/value"
)

// ModuleTypePrefix marks a resource type that instantiates a loaded module
// (PLAN.md §11.2). Stage 5 selects instances with it, so the two namespaces can
// never collide and a reader never has to consult `modules:` to know which kind
// of type they are looking at.
const ModuleTypePrefix = "module."

// loadedName records where a module name came from, so a collision diagnostic
// can name BOTH entries rather than only the second one. Naming only the second
// tells a user half of what they need: which two sources collided is the whole
// content of the message.
type loadedName struct {
	source string
	origin value.Origin
}

// decodeModuleLoads decodes a `modules:` block into dst.
//
// dst is a *[]ModuleLoadDecl rather than a *ProjectDecl because modules nest: a
// module file has a `modules:` block of its own (PLAN.md §11.3), and that one
// appends to a ModuleFile. One decoder serves both, so what a `modules:` entry
// means cannot differ depending on which document it appears in.
func decodeModuleLoads(path string, node *yaml.Node, dst *[]ModuleLoadDecl, ds *diag.Diagnostics, seen map[string]loadedName) {
	if node.Kind != yaml.SequenceNode {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "`modules` must be a list of module sources",
			Detail: "Unlike `resources`, `variables` and `environments`, `modules` is a LIST. " +
				"A mapping's key would have to be the module's name, and a name is optional — it is derived " +
				"from the source unless `name:` overrides it (PLAN.md §11.1).",
			Action: "Write each module as a list entry, for example `- ./modules/networking`, or " +
				"`- name: app` with `source:` beneath it.",
			Origin: originOf(path, node),
		})
		return
	}

	for i, entry := range node.Content {
		m, raw, ok := decodeModuleLoad(path, i, entry, ds)
		if !ok {
			continue
		}

		if first, dup := seen[m.Name]; dup {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "two modules are both named " + strconv.Quote(m.Name),
				Detail: strconv.Quote(first.source) + ", loaded at " + describeOrigin(first.origin) + ", and " +
					strconv.Quote(raw) + " both resolve to the name " + strconv.Quote(m.Name) +
					". A resource writing `type: " + ModuleTypePrefix + m.Name + "` would have two modules to choose from.",
				Action: "Give one of them a different name with `name:`, for example `- name: " + m.Name + "_v2`.",
				Origin: m.Origin,
			})
			// NEITHER entry is kept. Dropping only the second would be
			// "last one wins" inverted, which silently picks a module rather
			// than reporting that two were offered — and the `name:` form
			// exists precisely so this case is never decided by order.
			removeModuleNamed(dst, m.Name)
			continue
		}
		seen[m.Name] = loadedName{source: raw, origin: m.Origin}
		*dst = append(*dst, m)
	}
}

// removeModuleNamed drops an already-appended entry whose name has turned out
// to collide. Linear because a `modules:` block is short and the alternative —
// deferring every append until the whole list is walked — would lose the
// document order the diagnostics above are built from.
func removeModuleNamed(dst *[]ModuleLoadDecl, name string) {
	out := (*dst)[:0]
	for _, m := range *dst {
		if m.Name != name {
			out = append(out, m)
		}
	}
	*dst = out
}

// decodeModuleLoad returns the decl and the source AS THE USER WROTE IT. The raw
// text is returned separately rather than stored, because ModuleLoadDecl.Source
// is parsed and a parsed source cannot always be rendered back byte for byte — a
// trailing slash, an omitted `.git`. The only thing that needs the original
// spelling is the collision diagnostic, which is built here in the caller, where
// the raw text is still in hand.
func decodeModuleLoad(path string, index int, entry *yaml.Node, ds *diag.Diagnostics) (ModuleLoadDecl, string, bool) {
	what := "`modules`[" + strconv.Itoa(index) + "]"
	origin := originOf(path, entry)

	if entry.Kind == yaml.MappingNode {
		return decodeModuleLoadMapping(path, what, entry, origin, ds)
	}

	// The scalar form: the source, with the name derived from it. requireScalar
	// reports the alias, nested-list and null cases, each of which would
	// otherwise silently produce a wrong source — an alias node's Value is the
	// ANCHOR'S NAME, so `- *base` would load a module from the path "base".
	raw, ok := requireScalar(path, what, entry, ds)
	if !ok {
		return ModuleLoadDecl{}, "", false
	}
	parsed, ok := parseSource(raw, origin, ds)
	if !ok {
		return ModuleLoadDecl{}, "", false
	}
	name, nameDS := source.DeriveName(parsed)
	ds.Extend(nameDS)
	if nameDS.HasErrors() {
		// DeriveName has already said why the name could not be derived and
		// named the `name:` form as the fix. The entry is DROPPED rather than
		// kept with an empty name — a module with no usable name in ProjectDecl
		// is exactly what anything downstream that does not check errors first
		// would trip over.
		return ModuleLoadDecl{}, "", false
	}
	return ModuleLoadDecl{Name: name, Source: parsed, Origin: origin}, raw, true
}

// parseSource validates a source through internal/modules/source and returns the
// parsed form, which is what ModuleLoadDecl carries.
//
// Parse is pure — no disk, no network — so calling it here does not break stage
// 2's rule. What it buys is REPORTING at the right place: Parse takes an origin
// because its diagnostics are meant to point at a line, and stage 2 is the only
// stage that has one. A missing `:tag-or-hash` pin, a refused scheme, and git's
// `ext::` transport are therefore all reported by `infra validate`, at the line
// the source was written on, before anything is fetched.
//
// Storing the PARSED source rather than the string is what makes the other half
// structural (contract Amendment 15b): stage 5 receives a source.Source and hands
// it to Cache.Resolve, so it cannot re-emit a parse diagnostic, because it never
// parses. "Do not report this twice" stops being a rule an implementer has to
// remember and becomes one they cannot break.
func parseSource(raw string, origin value.Origin, ds *diag.Diagnostics) (source.Source, bool) {
	parsed, parseDS := source.Parse(raw, origin)
	ds.Extend(parseDS)
	if parseDS.HasErrors() {
		// Parse has already said what is wrong with this source and where.
		// Deriving a name from a location it could not determine would add a
		// second, confusing diagnostic about the same entry.
		return source.Source{}, false
	}
	return parsed, true
}

func decodeModuleLoadMapping(path, what string, body *yaml.Node, origin value.Origin, ds *diag.Diagnostics) (ModuleLoadDecl, string, bool) {
	m := ModuleLoadDecl{Origin: origin}

	// raw and rawOrigin hold the source as written, until it is parsed below.
	// They are locals rather than fields because ModuleLoadDecl.Source is the
	// PARSED source (contract Amendment 15b), and the raw text is needed only
	// for the collision diagnostic the caller builds.
	var raw string
	var rawOrigin value.Origin

	// sourceReported records that `source` was present but unusable, so the
	// "has no `source`" check below does not fire a second, misleading
	// diagnostic about the same key — the suppression decodeResources makes for
	// `type`.
	sourceReported := false
	seenKeys := map[string]value.Origin{}

	for i := 0; i+1 < len(body.Content); i += 2 {
		key, val := body.Content[i], body.Content[i+1]
		keyOrigin := originOf(path, key)
		if first, dup := seenKeys[key.Value]; dup {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  strconv.Quote(key.Value) + " is set more than once in " + what,
				Detail:   "The last assignment would silently win. " + strconv.Quote(key.Value) + " is also set at " + describeOrigin(first) + ".",
				Action:   "Remove one of the two assignments.",
				Origin:   keyOrigin,
			})
			continue
		}
		seenKeys[key.Value] = keyOrigin

		switch key.Value {
		case "source":
			text, ok := requireScalar(path, what+"'s `source`", val, ds)
			if !ok {
				sourceReported = true
				break
			}
			raw, rawOrigin = text, originOf(path, val)

		case "name":
			text, ok := requireScalar(path, what+"'s `name`", val, ds)
			if !ok {
				break
			}
			if !identifierSegment(text) {
				ds.Add(diag.Diagnostic{
					Severity: diag.SeverityError,
					Summary:  "module name " + strconv.Quote(text) + " is not a valid identifier",
					Detail: "A module's name becomes half of a resource type — `type: " + ModuleTypePrefix + text +
						"` — so it must be a letter or underscore followed by letters, digits, underscores or hyphens. " +
						"A name containing a dot would read as a path into a module.",
					Action: "Use letters, digits and underscores, for example `name: " + strings.NewReplacer(".", "_", "-", "_").Replace(text) + "`.",
					Origin: originOf(path, val),
				})
				break
			}
			m.Name = text

		default:
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "unknown key " + strconv.Quote(key.Value) + " in " + what,
				Detail: "A `modules:` entry understands `name` and `source`, and nothing else. Loading a module says " +
					"only where it comes from and what to call it; values are passed when it is INSTANTIATED (PLAN.md §11.2).",
				Action: moduleEntryKeyAction(key.Value, m.Name),
				Origin: keyOrigin,
			})
		}
	}

	if raw == "" {
		if !sourceReported {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  what + " has no `source`",
				Detail:   "Every module entry must say where the module comes from, and this one would load nothing — so every resource writing `type: " + ModuleTypePrefix + "…` for it would fail with no explanation of why.",
				Action:   "Add `source: ./modules/" + defaultedName(m.Name) + "`.",
				Origin:   origin,
			})
		}
		return ModuleLoadDecl{}, "", false
	}

	// Parsed for EVERY entry, including one that named itself. A `name:` says
	// what to call the module; it says nothing about whether the source is
	// fetchable, so an unpinned or `ext::` source must be reported either way.
	parsed, ok := parseSource(raw, rawOrigin, ds)
	if !ok {
		return ModuleLoadDecl{}, "", false
	}
	m.Source = parsed

	if m.Name == "" {
		name, nameDS := source.DeriveName(parsed)
		ds.Extend(nameDS)
		if nameDS.HasErrors() {
			return ModuleLoadDecl{}, "", false
		}
		m.Name = name
	}
	return m, raw, true
}

// moduleEntryKeyAction says what to do with a key that does not belong on a
// `modules:` entry. `inputs` gets its own answer because it is the migration
// trap: loading used to take inputs, and a user carrying that shape forward
// needs to be told where they went, not merely that they are wrong.
func moduleEntryKeyAction(key, name string) string {
	if key == "inputs" {
		return "Remove `inputs`, and pass the values on the resource that instantiates this module — " +
			"a resource with `type: " + ModuleTypePrefix + defaultedName(name) + "` and the values as its attributes."
	}
	return "Remove " + strconv.Quote(key) + "."
}

// defaultedName gives a stand-in for an entry whose name could not be
// determined, so an action can still show a usable example line.
func defaultedName(name string) string {
	if name == "" {
		return "<name>"
	}
	return name
}

// identifierSegment reports whether s is a plain identifier: a letter or
// underscore, then letters, digits, underscores or hyphens.
//
// Shared by module names, which become half of a resource type, and by Task 3's
// bare-reference detection. One rule for what a name looks like, in one place.
// ValidResourceName reports whether s may be used as a resource's logical name.
//
// Exported so internal/discovery can generate names that this package will
// accept, by asking rather than by restating the rule. A generator with its own
// copy of the predicate produces configuration that loads until the two copies
// drift, and the failure lands on a user who wrote none of it.
func ValidResourceName(s string) bool { return identifierSegment(s) }

func identifierSegment(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case i > 0 && (r >= '0' && r <= '9' || r == '-'):
		default:
			return false
		}
	}
	return true
}

// checkResourceName validates a resource's logical name (contract Amendment 16a).
//
// A name IS an address (pkg/address), and an address is a state key. Stage 2 is
// the only stage that still holds the line the name was written on.
//
// The dot is the dangerous character, and it is why this lands in M5 rather than
// on a follow-up list. Address.String() returns Name verbatim when Module is
// empty, so a resource literally named "module.prod.database" writes the state
// key "module.prod.database" — byte for byte what the resource `database` inside
// module instance `prod` writes once modules exist. Such a state file is
// unambiguous today and becomes ambiguous the moment the first module ships, so
// this is the last milestone in which the fix is a diagnostic rather than a
// migration.
//
// Amendment 14a is the same collision through the other door: it guards the
// REFERENCE grammar in parseReference, so ${module.prod.db.id} cannot be
// written; this guards the DECLARATION, so the string cannot become an address
// at all. A reference is parsed out of an expression and a name is read from a
// mapping key, so neither path reaches the other — delete either as duplicated
// and half the collision reopens.
//
// It serves module files too, because decodeResources is shared: a resource
// inside a module is no freer to name itself `module.x` than a root one.
func checkResourceName(path, name string, origin value.Origin, ds *diag.Diagnostics) bool {
	if identifierSegment(name) {
		return true
	}

	// Two branches, because the dot is a collision and everything else is
	// merely malformed. One message covering both would explain the address
	// hazard to someone who wrote a space.
	if strings.Contains(name, ".") {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "resource name " + strconv.Quote(name) + " contains a dot",
			Detail: "A resource's name is its address, and a dot is how an address separates module levels — so " +
				strconv.Quote(name) + " is indistinguishable from the address of a resource inside a module, and the " +
				"two would share one key in state.",
			Action: "Rename it without dots, for example `" + strings.ReplaceAll(name, ".", "_") + ":`.",
			Origin: origin,
		})
		return false
	}

	ds.Add(diag.Diagnostic{
		Severity: diag.SeverityError,
		Summary:  "resource name " + strconv.Quote(name) + " is not a valid identifier",
		Detail: "A resource's name is its address and appears in state, in plans and in `${...}` references, so it " +
			"must be a letter or underscore followed by letters, digits, underscores or hyphens.",
		Action: "Rename it using letters, digits, underscores and hyphens, starting with a letter or underscore.",
		Origin: origin,
	})
	return false
}

// checkResourceType validates a resource's `type`, which for a module call is a
// property of the text and therefore stage 2's to check.
//
// Stage 5 selects module instances with strings.HasPrefix(r.Type, "module.")
// (contract Amendment 8c), so a malformed `module.` type is SELECTED and then
// looked up as a module named "" or "a.b". Reporting it here, where the line
// number is, is the difference between naming the typo and reporting that some
// module does not exist.
//
// It returns false when the type is unusable, so the caller can suppress its
// "has no `type`" check the way it does for a `type` that failed requireScalar.
func checkResourceType(path, resource, typ string, origin value.Origin, ds *diag.Diagnostics) bool {
	if !strings.HasPrefix(typ, ModuleTypePrefix) {
		return true
	}
	name := strings.TrimPrefix(typ, ModuleTypePrefix)

	switch {
	case name == "":
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "resource " + strconv.Quote(resource) + " has type " + strconv.Quote(typ) + " with no module name",
			Detail:   "`" + ModuleTypePrefix + "` instantiates a module loaded in `modules:`, and this names none.",
			Action:   "Write the loaded module's name, for example `type: " + ModuleTypePrefix + "app_stack`.",
			Origin:   origin,
		})
		return false

	case strings.Contains(name, "."):
		top := name[:strings.Index(name, ".")]
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "resource " + strconv.Quote(resource) + " names a path into a module, not a module",
			Detail: "`" + typ + "` reads as a path inside " + strconv.Quote(top) + ". A resource instantiates a " +
				"top-level module loaded in `modules:`; resources INSIDE that module are reached by referencing " +
				"the instance, such as ${" + resource + ".endpoint}.",
			Action: "Write `type: " + ModuleTypePrefix + top + "`.",
			Origin: origin,
		})
		return false
	}
	return true
}
