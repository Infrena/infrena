// Package expressions parses and evaluates infrena's deliberately minimal
// expression language: interpolation, references to other resources'
// attributes, and a fixed set of pure functions. It is not a programming
// language and must not grow into one.
package expressions

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/pkg/value"
)

// Parse converts a configuration scalar into an expression tree. Text with no
// interpolation yields a single literal node. Text that is exactly one
// interpolation yields that node unwrapped, so it can carry a non-string kind
// through to evaluation; anything mixed yields a concatenation.
func Parse(src string, origin value.Origin) (*value.Expr, diag.Diagnostics) {
	var ds diag.Diagnostics
	parts, ok := split(src, origin, &ds)
	if !ok {
		return nil, ds
	}

	switch len(parts) {
	case 0:
		return &value.Expr{Op: value.OpLiteral, Literal: value.String("", value.SourceExplicit), Origin: origin}, ds
	case 1:
		return parts[0], ds
	default:
		return &value.Expr{Op: value.OpConcat, Args: parts, Origin: origin}, ds
	}
}

// split walks the source, emitting literal runs and parsed interpolations.
func split(src string, origin value.Origin, ds *diag.Diagnostics) ([]*value.Expr, bool) {
	var parts []*value.Expr
	var lit strings.Builder

	flush := func() {
		if lit.Len() > 0 {
			parts = append(parts, &value.Expr{
				Op:      value.OpLiteral,
				Literal: value.String(lit.String(), value.SourceExplicit),
				Origin:  origin,
			})
			lit.Reset()
		}
	}

	for i := 0; i < len(src); {
		// $${...} is an escaped literal dollar-brace.
		if strings.HasPrefix(src[i:], "$${") {
			lit.WriteString("${")
			i += 3
			continue
		}
		if !strings.HasPrefix(src[i:], "${") {
			lit.WriteByte(src[i])
			i++
			continue
		}

		end := matchBrace(src, i+2)
		if end < 0 {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "unclosed interpolation in " + strconv.Quote(src),
				Detail:   "An interpolation opened with ${ was never closed.",
				Action:   "Add the missing }, or write $${ for a literal dollar-brace.",
				Origin:   origin,
			})
			return nil, false
		}

		flush()
		inner := strings.TrimSpace(src[i+2 : end])
		if e := parseExpr(inner, origin, ds); e != nil {
			parts = append(parts, e)
		}
		i = end + 1
	}

	flush()
	return parts, true
}

// scanner walks a string once, maintaining quote state and nesting depth, and is
// the one place either of those is tracked.
//
// Quotes and escapes are what two separate scanners would eventually disagree
// about, accepting input one of them rejects. Here a caller never sees a quote
// character or an escaped byte at all, so they cannot.
//
// Which delimiters nest is per caller, deliberately. Counting parens toward the
// interpolation's own depth makes `${lower(a}` report "unclosed interpolation"
// instead of naming the missing `)`, which is a worse message for the same
// mistake.
type scanner struct {
	src    string
	i      int
	depth  int
	quoted bool
	// open and close are matched by index: open[k] nests, close[k] unnests.
	open, close string
}

// next advances to the next significant byte and reports it, along with the depth
// after any delimiter at that position is counted.
//
// Escapes and quote toggles are consumed silently, so no caller can hold an
// opinion about them.
func (s *scanner) next() (b byte, depth int, ok bool) {
	for s.i < len(s.src) {
		c := s.src[s.i]

		if s.quoted && c == '\\' && s.i+1 < len(s.src) {
			s.i += 2
			continue
		}
		if c == '"' {
			s.quoted = !s.quoted
			s.i++
			continue
		}
		if !s.quoted {
			if strings.IndexByte(s.open, c) >= 0 {
				s.depth++
			} else if strings.IndexByte(s.close, c) >= 0 {
				s.depth--
			}
		}
		at := s.i
		s.i++
		if s.quoted {
			// Inside a quoted literal: consumed, but never significant. A caller
			// asking "is this byte a delimiter" must never be handed one from
			// inside quotes.
			continue
		}
		return s.src[at], s.depth, true
	}
	return 0, s.depth, false
}

// matchBrace returns the index of the } closing the interpolation that starts at
// from, or -1 if there is none.
//
// Braces only: a stray `(` must not consume the closing brace. See the scanner's
// doc comment.
func matchBrace(src string, from int) int {
	// Seeded at 1 for the `${` already consumed, so the closing brace is the one
	// that brings the depth back to zero.
	sc := &scanner{src: src, i: from, depth: 1, open: "{", close: "}"}
	for {
		b, depth, ok := sc.next()
		if !ok {
			return -1
		}
		if b == '}' && depth == 0 {
			return sc.i - 1
		}
	}
}

// splitArgs splits on commas at the top level of an argument list.
//
// Every bracket pair nests here, including braces: a map literal puts a comma
// inside `{a: b, c: d}` that is not an argument separator.
func splitArgs(src string) []string {
	sc := &scanner{src: src, open: "({[", close: ")}]"}
	var out []string
	start := 0
	for {
		b, depth, ok := sc.next()
		if !ok {
			break
		}
		if b == ',' && depth == 0 {
			out = append(out, src[start:sc.i-1])
			start = sc.i
		}
	}
	return append(out, src[start:])
}

// parseExpr parses the inside of one interpolation.
func parseExpr(src string, origin value.Origin, ds *diag.Diagnostics) *value.Expr {
	src = strings.TrimSpace(src)
	if src == "" {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "empty interpolation",
			Detail:   "${} names nothing.",
			Action:   "Name a variable, a resource attribute, or a function call.",
			Origin:   origin,
		})
		return nil
	}

	// A quoted scalar is a literal argument, not a name.
	if len(src) >= 2 && src[0] == '"' && src[len(src)-1] == '"' {
		unquoted, err := strconv.Unquote(src)
		if err != nil {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "malformed quoted literal " + src,
				Origin:   origin,
			})
			return nil
		}
		return &value.Expr{Op: value.OpLiteral, Literal: value.String(unquoted, value.SourceExplicit), Origin: origin}
	}

	// A literal may appear only as an argument. Refused here rather than left to
	// parseReference, which would report "malformed reference" and send the
	// reader after a name they did not write.
	if src[0] == '{' || src[0] == '[' {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "a literal may only appear as a function argument",
			Detail: "`" + src + "` is written where a variable, a resource attribute or a " +
				"function call is expected. YAML already expresses maps and lists, so the " +
				"language does not.",
			Action: "Write the value as YAML, or pass it to a function: " +
				"`${merge(var.tags, " + src + ")}`.",
			Origin: origin,
		})
		return nil
	}

	if open := strings.Index(src, "("); open >= 0 {
		return parseCall(src, open, origin, ds)
	}
	return parseReference(src, origin, ds)
}

// parseArgument parses one argument of a call, which is the only position a map
// or list literal may appear in. Bounding them to it keeps them from being the
// first step toward a programming language: YAML already expresses a map
// everywhere else.
func parseArgument(src string, origin value.Origin, ds *diag.Diagnostics) *value.Expr {
	switch {
	case strings.HasPrefix(src, "{"):
		return parseMapLiteral(src, origin, ds)
	case strings.HasPrefix(src, "["):
		return parseListLiteral(src, origin, ds)
	default:
		return parseExpr(src, origin, ds)
	}
}

// parseMapLiteral parses `{key: value, key: value}`.
//
// Inside a literal a bare word is a string, not a variable: `{team: payments}`
// means the string "payments", which is what the YAML around it would mean.
// Treating it as a reference would make the obvious spelling silently resolve to
// something else, and a variable belongs in another argument anyway —
// `merge(var.tags, {...})`.
//
// Keys are text, unquoted and uninterpolated, because a configuration's shape
// must not depend on a value.
func parseMapLiteral(src string, origin value.Origin, ds *diag.Diagnostics) *value.Expr {
	if !strings.HasSuffix(src, "}") {
		ds.Add(literalDiag("map", src, "}", origin))
		return nil
	}
	items := map[string]value.Value{}
	for _, raw := range splitArgs(src[1 : len(src)-1]) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		colon := strings.Index(raw, ":")
		if colon < 0 {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "map literal entry " + strconv.Quote(raw) + " has no `:`",
				Detail:   "Each entry of a map literal is `key: value`.",
				Action:   "Write `" + raw + ": <value>`, or use a list literal if order is what you meant.",
				Origin:   origin,
			})
			continue
		}
		key := strings.TrimSpace(raw[:colon])
		if key == "" {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "map literal entry " + strconv.Quote(raw) + " has an empty key",
				Origin:   origin,
			})
			continue
		}
		items[key] = literalScalar(strings.TrimSpace(raw[colon+1:]), origin)
	}
	return &value.Expr{
		Op:      value.OpLiteral,
		Literal: value.Map(items, value.SourceExplicit).WithOrigin(origin),
		Origin:  origin,
	}
}

// parseListLiteral parses `[a, b, c]`, with the same scalar rules as a map's
// values.
func parseListLiteral(src string, origin value.Origin, ds *diag.Diagnostics) *value.Expr {
	if !strings.HasSuffix(src, "]") {
		ds.Add(literalDiag("list", src, "]", origin))
		return nil
	}
	var items []value.Value
	for _, raw := range splitArgs(src[1 : len(src)-1]) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		items = append(items, literalScalar(raw, origin))
	}
	return &value.Expr{
		Op:      value.OpLiteral,
		Literal: value.List(items, value.SourceExplicit).WithOrigin(origin),
		Origin:  origin,
	}
}

func literalDiag(kind, src, closer string, origin value.Origin) diag.Diagnostic {
	return diag.Diagnostic{
		Severity: diag.SeverityError,
		Summary:  "unclosed " + kind + " literal " + strconv.Quote(src),
		Action:   "Add the missing " + closer + ".",
		Origin:   origin,
	}
}

// literalScalar reads one scalar inside a literal.
//
// Quoted text is a string with the quotes removed. Otherwise the shapes YAML
// itself reads specially are read the same way — true/false, an integer, a float
// — and everything else is a string. Deliberately small: this is not a YAML
// parser, and anything it cannot read unambiguously stays text, which is the one
// answer that cannot silently change a value's meaning.
func literalScalar(src string, origin value.Origin) value.Value {
	if len(src) >= 2 && src[0] == '"' && src[len(src)-1] == '"' {
		if unquoted, err := strconv.Unquote(src); err == nil {
			return value.String(unquoted, value.SourceExplicit).WithOrigin(origin)
		}
	}
	switch src {
	case "true":
		return value.Bool(true, value.SourceExplicit).WithOrigin(origin)
	case "false":
		return value.Bool(false, value.SourceExplicit).WithOrigin(origin)
	}
	if n, err := strconv.ParseInt(src, 10, 64); err == nil {
		return value.Int(n, value.SourceExplicit).WithOrigin(origin)
	}
	if f, err := strconv.ParseFloat(src, 64); err == nil {
		return value.Float(f, value.SourceExplicit).WithOrigin(origin)
	}
	return value.String(src, value.SourceExplicit).WithOrigin(origin)
}

func parseCall(src string, open int, origin value.Origin, ds *diag.Diagnostics) *value.Expr {
	name := strings.TrimSpace(src[:open])
	if !strings.HasSuffix(src, ")") {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "unclosed call to " + strconv.Quote(name),
			Action:   "Add the missing ).",
			Origin:   origin,
		})
		return nil
	}
	if name == "" {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "call with no function name",
			Origin:   origin,
		})
		return nil
	}

	// `${template.x({...})}` and `${file.x}` reach here because they contain a
	// paren, and they are references rather than function calls. Intercepted
	// before the generic path so the argument shape can be checked while the
	// namespace is still visible.
	if rest, isTemplate := strings.CutPrefix(name, "template."); isTemplate {
		return parseTemplateCall(value.OpTemplateRef, rest, src, open, origin, ds)
	}
	if rest, isFile := strings.CutPrefix(name, "file."); isFile {
		return parseTemplateCall(value.OpFileRef, rest, src, open, origin, ds)
	}

	inner := src[open+1 : len(src)-1]
	var args []*value.Expr
	for _, raw := range splitArgs(inner) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if a := parseArgument(raw, origin, ds); a != nil {
			args = append(args, a)
		}
	}

	return &value.Expr{Op: value.OpCall, Function: name, Args: args, Origin: origin}
}

// indexSuffix matches a trailing [N] on one segment.
var indexSuffix = regexp.MustCompile(`^(.*?)\[([^\]]*)\]$`)

// parseSteps turns the segments after a name into path steps.
//
// A segment is a map key, optionally carrying trailing brackets that index or
// key the value it names. Brackets are scanned here rather than by the
// expression scanner because they never nest inside a reference.
func parseSteps(segments []string, ref string, origin value.Origin, ds *diag.Diagnostics) ([]value.Step, bool) {
	var out []value.Step
	for _, seg := range segments {
		key := seg
		var indices []string
		for {
			m := indexSuffix.FindStringSubmatch(key)
			if m == nil {
				break
			}
			key = m[1]
			indices = append([]string{m[2]}, indices...)
		}
		if key == "" {
			// The dot split already refused an outright empty segment
			// (`${var.a..b}`), but a segment can still be entirely brackets
			// (`[0]`) once they are stripped here — an index with nothing to
			// index into.
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "malformed reference " + strconv.Quote(ref),
				Detail: strconv.Quote(seg) + " in ${" + ref + "} has no key before its bracket, so " +
					"the index has nothing to index.",
				Action: "Write the key the index applies to before the bracket, as ${var.name[0]}.",
				Origin: origin,
			})
			return nil, false
		}
		out = append(out, value.Step{Kind: value.StepKey, Key: key})
		for _, raw := range indices {
			// A quoted bracket is a key, not an index: ${subnet["eu-west-1a"]}
			// names a for_each instance, and ${res.tags["Name"]} reads a map
			// entry whose key is not a bare word. Quoting is what distinguishes
			// the two, so a key that looks like a number — ${subnet["0"]} —
			// still reads as a key.
			if unquoted, ok := unquoteBracketKey(raw); ok {
				out = append(out, value.Step{Kind: value.StepKey, Key: unquoted})
				continue
			}
			n, err := strconv.Atoi(raw)
			if err != nil {
				bracket := strings.Index(ref, "[")
				refPrefix := ref
				if bracket >= 0 {
					refPrefix = ref[:bracket]
				}
				ds.Add(diag.Diagnostic{
					Severity: diag.SeverityError,
					Summary:  "index " + strconv.Quote(raw) + " in ${" + ref + "} is not a literal integer",
					Detail: "An index is a literal integer, and a quoted bracket is a key: " +
						"${" + refPrefix + "[0]} reads a list, ${" + refPrefix + "[\"name\"]} reads a map " +
						"entry or a for_each instance. A computed index is not offered — iteration " +
						"happens in `for_each`, where each instance is named rather than numbered.",
					Action: "Write a literal index as ${" + refPrefix + "[0]}, or quote it to mean a key: " +
						"${" + refPrefix + "[\"" + raw + "\"]}.",
					Origin: origin,
				})
				return nil, false
			}
			if n < 0 {
				ds.Add(diag.Diagnostic{
					Severity: diag.SeverityError,
					Summary:  "index " + raw + " in ${" + ref + "} is negative",
					Detail: "A negative index would make the reference's meaning depend on a length " +
						"the reader cannot see.",
					Action: "Count from the start, as [0].",
					Origin: origin,
				})
				return nil, false
			}
			out = append(out, value.Step{Kind: value.StepIndex, Index: n})
		}
	}
	return out, true
}

func parseReference(src string, origin value.Origin, ds *diag.Diagnostics) *value.Expr {
	segments := strings.Split(src, ".")
	for _, s := range segments {
		if strings.TrimSpace(s) == "" {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "malformed reference " + strconv.Quote(src),
				Detail:   fmt.Sprintf("%q has an empty name segment.", src),
				Action:   "Write ${var.name} for a variable, or ${resource.attribute} for a resource attribute.",
				Origin:   origin,
			})
			return nil
		}
	}

	// A user-written reference never contains a `module` segment. Qualify
	// produces module-qualified targets at stage 6; the parser must never accept
	// one.
	//
	// ${module.prod.database.id} parses to the target name
	// "module.prod.database", which is byte for byte what the real resource
	// inside instance prod renders as. Stage 6 keys its target map by canonical
	// address, so the two collide and a module's internals become addressable
	// from outside it — and a module exposes its outputs, not its resources.
	// Refused here rather than at the lookup, because by then the two spellings
	// are the same bytes and no guard could tell them apart.
	//
	// Only segments[0] can be a module qualifier: the target is always exactly
	// one segment, so `module` spelled anywhere else is a map key
	// (${var.tags.module}) or a path step (${vpc.module}) the user wrote.
	if segments[0] == "module" {
		// Two different mistakes wear the same segment, and telling a user to
		// reference an output would be nonsense for the second.
		if len(segments) >= 3 {
			instance := segments[1]
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "reference ${" + src + "} names a module's internals",
				Detail: "`module` is part of an ADDRESS — how a resource is named in state and in a plan — " +
					"and is never part of a reference. A module exposes its OUTPUTS to its caller, not the " +
					"resources it contains, so nothing inside " + strconv.Quote(instance) +
					" can be referenced from outside it.",
				Action: "Reference one of its outputs instead, as ${" + instance + ".<output>}, and add an " +
					"`outputs:` entry to the module if it does not already publish the value you need.",
				Origin: origin,
			})
			return nil
		}
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "reference ${" + src + "} names a resource called " + strconv.Quote("module"),
			Detail: "`module` is reserved: it is the first segment of every module-qualified address, so a " +
				"resource with that name has an address that cannot be read back unambiguously.",
			Action: "Rename the resource to something other than " + strconv.Quote("module") + ".",
			Origin: origin,
		})
		return nil
	}

	// `each` is the for_each namespace, rewritten onto an ordinary variable
	// called "each" holding a two-key map. A rewrite rather than a new node kind,
	// so `${each.key}` is an existing path step over an existing map and needs no
	// evaluation machinery of its own. The name is reserved the way `var` is: a
	// resource called `each` would make `${each.key}` ambiguous with a reference
	// to its `key` attribute.
	if segments[0] == "each" {
		if len(segments) == 1 {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "${each} names nothing",
				Detail:   "`each` carries the current for_each entry: ${each.key} and ${each.value}.",
				Action:   "Write ${each.key} or ${each.value}.",
				Origin:   origin,
			})
			return nil
		}
		rest, ok := parseSteps(segments[1:], src, origin, ds)
		if !ok {
			return nil
		}
		ref := value.VarRef("each")
		ref.Path = rest
		return &value.Expr{Op: value.OpVarRef, Ref: ref, Origin: origin}
	}

	// `template` and `file` name a file under templates/. Both take the rest of
	// the reference as one opaque name rather than parsing path steps, because a
	// filename contains dots: `${template.iam-policy.json}` names one file, not a
	// `json` field of an `iam-policy` field.
	if segments[0] == "template" || segments[0] == "file" {
		kind := segments[0]
		if len(segments) == 1 {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "${" + kind + "} names no file",
				Detail:   "`" + kind + "` is the namespace files under templates/ live in, not a file itself.",
				Action:   "Name one, as ${" + kind + ".policy.json}.",
				Origin:   origin,
			})
			return nil
		}
		op := value.OpTemplateRef
		if kind == "file" {
			op = value.OpFileRef
		}
		// Rejoined rather than taking segments[1]: the split already happened,
		// and a name with two dots (`policy.v2.json`) is still one filename.
		return &value.Expr{
			Op:     op,
			Ref:    value.VarRef(strings.Join(segments[1:], ".")),
			Origin: origin,
		}
	}

	// `secret` is the secret namespace, and is `var`'s sibling by design. A
	// mapping spelling — `password: {secret: DATABASE_PASSWORD}` — was
	// considered and rejected for three reasons:
	//
	//   - It is ambiguous. A mapping whose only key is `secret` is
	//     indistinguishable from a genuine map attribute that happens to have
	//     one, so `tags: {secret: quiet}` would silently become a secret lookup.
	//   - It does not compose. `url: "postgres://app:${secret.PW}@db"` is the
	//     common shape and a mapping cannot express it at all.
	//   - It is a second grammar. `${var.x}` already exists, and another way to
	//     spell "a value from elsewhere" is a thing to learn twice.
	if segments[0] == "secret" {
		if len(segments) == 1 {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "${secret} names no secret",
				Detail:   "`secret` is the namespace secrets live in, not a secret itself.",
				Action:   "Name one, as ${secret.DATABASE_PASSWORD}.",
				Origin:   origin,
			})
			return nil
		}
		if len(segments) > 2 {
			// No path steps. A secret is an opaque string read from the
			// environment, so `${secret.X.y}` cannot mean anything — and
			// letting it parse would make the failure a confusing one at
			// evaluation rather than a clear one here.
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "malformed reference " + strconv.Quote(src),
				Detail:   "A secret is a single opaque value, so `secret.` takes one name and nothing after it.",
				Action:   "Write ${secret." + segments[1] + "}.",
				Origin:   origin,
			})
			return nil
		}
		return &value.Expr{
			Op:     value.OpSecretRef,
			Ref:    value.VarRef(segments[1]),
			Origin: origin,
		}
	}

	// `var` is the variable namespace. Stripped here so that nothing below the
	// parser learns the prefix exists: variable scopes are keyed on the bare
	// name.
	if segments[0] == "var" {
		if len(segments) == 1 {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "${var} names no variable",
				Detail:   "`var` is the namespace variables live in, not a variable itself.",
				Action:   "Name one, as ${var.region}.",
				Origin:   origin,
			})
			return nil
		}
		// The variable's own name may carry an index: ${var.azs[0]}.
		nameSteps, ok := parseSteps(segments[1:2], src, origin, ds)
		if !ok {
			return nil
		}
		if len(nameSteps) == 0 || nameSteps[0].Kind != value.StepKey {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "malformed reference " + strconv.Quote(src),
				Detail:   "`var.` must be followed by a variable name.",
				Action:   "Name one, as ${var.region}.",
				Origin:   origin,
			})
			return nil
		}
		rest, ok := parseSteps(segments[2:], src, origin, ds)
		if !ok {
			return nil
		}
		ref := value.VarRef(nameSteps[0].Key)
		ref.Path = append(nameSteps[1:], rest...)
		return &value.Expr{Op: value.OpVarRef, Ref: ref, Origin: origin}
	}

	if len(segments) == 1 {
		// A whole-resource reference: the attribute is not written, and stage 6
		// fills it in from the consuming attribute's own `References`
		// declaration.
		//
		// An empty Attribute must never escape stage 6. Downstream,
		// ResourceScope looks an attribute up by name in a plain map, so "" would
		// miss, report unavailable, and leave the value deferred forever — the
		// resource would be created with the attribute silently unset.
		return &value.Expr{
			Op:     value.OpResourceRef,
			Ref:    value.LocalRef(segments[0], ""),
			Origin: origin,
		}
	}

	// First segment is the resource, second is the attribute, the rest is a
	// path. A resource's name cannot contain a dot — a name IS an address and a
	// dot separates module levels — so a user-written target is always exactly
	// one segment.
	//
	// The target is still run through parseSteps, because a `for_each` instance
	// is selected on the resource — ${subnet["eu-west-1a"].id} — and the key
	// belongs to the target's address. A numeric index there is refused below,
	// where the diagnostic can say why.
	attrSteps, ok := parseSteps(segments[1:2], src, origin, ds)
	if !ok {
		return nil
	}
	if len(attrSteps) == 0 || attrSteps[0].Kind != value.StepKey {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "malformed reference " + strconv.Quote(src),
			Detail:   "A resource reference names an attribute after the resource.",
			Action:   "Write ${" + segments[0] + ".<attribute>}.",
			Origin:   origin,
		})
		return nil
	}
	rest, ok := parseSteps(segments[2:], src, origin, ds)
	if !ok {
		return nil
	}
	// segments[0] may carry a for_each instance key — ${subnet["eu-west-1a"].id}
	// — so it is parsed rather than taken verbatim. Taken verbatim the target
	// name would literally be `subnet["eu-west-1a"]`, which binds to nothing and
	// reports as a missing resource whose name contains punctuation.
	targetSteps, ok := parseSteps(segments[0:1], src, origin, ds)
	if !ok {
		return nil
	}
	if len(targetSteps) == 0 || targetSteps[0].Kind != value.StepKey {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "malformed reference " + strconv.Quote(src),
			Detail:   "A reference begins with a resource name.",
			Origin:   origin,
		})
		return nil
	}
	ref := value.LocalRef(targetSteps[0].Key, attrSteps[0].Key)
	if len(targetSteps) > 1 {
		if targetSteps[1].Kind != value.StepKey {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "malformed reference " + strconv.Quote(src),
				Detail: "A for_each instance is named, not numbered: an instance's identity is its " +
					"key, so it is selected by key rather than by position.",
				Action: "Write ${" + targetSteps[0].Key + "[\"<name>\"].<attribute>}.",
				Origin: origin,
			})
			return nil
		}
		// The KEY travels on the target address, which is where identity lives
		// — so everything downstream that already handles an address handles an
		// instance without learning a second shape.
		ref.Target.Key = targetSteps[1].Key
		if len(targetSteps) > 2 {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "malformed reference " + strconv.Quote(src),
				Detail:   "A resource has at most one for_each key.",
				Origin:   origin,
			})
			return nil
		}
	}
	ref.Path = append(attrSteps[1:], rest...)
	return &value.Expr{Op: value.OpResourceRef, Ref: ref, Origin: origin}
}

// unquoteBracketKey reads a quoted bracket as a map key.
//
// Both quote styles, because a double-quoted expression inside a double-quoted
// YAML scalar needs escaping, and a user who reaches for
// `${subnet['eu-west-1a']}` to avoid that has not made a mistake.
func unquoteBracketKey(raw string) (string, bool) {
	if len(raw) < 2 {
		return "", false
	}
	q := raw[0]
	if (q != '"' && q != '\'') || raw[len(raw)-1] != q {
		return "", false
	}
	inner := raw[1 : len(raw)-1]
	// A quote inside a quoted key would change where the key ends, so it is
	// refused rather than guessed at.
	if strings.ContainsRune(inner, rune(q)) {
		return "", false
	}
	return inner, true
}

// parseTemplateCall reads `${template.NAME({...})}` and `${file.NAME}`.
//
// A template takes at most one argument, a map, and that is all the `{{ }}` pass
// inside the file can see. Making the inputs explicit at the call site is what
// keeps the two passes honest about secrets: the template engine cannot reach an
// ambient scope, so a secret is either passed deliberately — where it is known to
// be sensitive and can taint the result — or written `${secret.X}` inside the
// file, where the second pass handles it.
//
// `file` takes no argument. It reads the file verbatim, so there is nothing for
// an argument to affect, and accepting one would advertise a behaviour that does
// not happen.
func parseTemplateCall(op value.ExprOp, name, src string, open int, origin value.Origin, ds *diag.Diagnostics) *value.Expr {
	inner := strings.TrimSpace(src[open+1 : len(src)-1])
	if name == "" {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "malformed reference " + strconv.Quote(src),
			Detail:   "The namespace has to be followed by a file name.",
			Origin:   origin,
		})
		return nil
	}

	out := &value.Expr{Op: op, Ref: value.VarRef(name), Origin: origin}
	if inner == "" {
		return out
	}
	if op == value.OpFileRef {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "${file." + name + "} takes no arguments",
			Detail: "A file reference reads the file verbatim, so there is nothing for an argument " +
				"to affect. ${template." + name + "(...)} renders it instead.",
			Action: "Remove the arguments, or use ${template." + name + "(...)}.",
			Origin: origin,
		})
		return nil
	}
	arg := parseArgument(inner, origin, ds)
	if arg == nil {
		return nil
	}
	out.Args = []*value.Expr{arg}
	return out
}
