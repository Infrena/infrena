// Package expressions parses and evaluates infra's deliberately minimal
// expression language: interpolation, references to other resources'
// attributes, and a fixed set of pure functions. It is not a programming
// language and must not grow into one (spec §6).
package expressions

import (
	"fmt"
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
// the ONE place either of those is tracked.
//
// There used to be two loops doing this — matchBrace counting `{}` and splitArgs
// counting `()` — sharing only skipEscape, whose comment already explained the
// hazard:
//
//	when two scanners disagree, input is accepted by one and rejected by the
//	other, which is the class of bug the quote handling was added to fix in the
//	first place
//
// That hazard is about QUOTES AND ESCAPES, which is what the two really must
// agree on, and it is now impossible for them to disagree: a caller never sees a
// quote character or an escaped byte at all.
//
// WHICH delimiters nest is per caller, deliberately. Counting parens toward the
// interpolation's own depth was tried and reverted: it made `${lower(a}` report
// "unclosed interpolation" instead of `unclosed call to "lower" — add the missing
// )`, which is a strictly worse message for the same mistake, and §44 asks for
// the actionable one. The characterisation test caught it.
type scanner struct {
	src    string
	i      int
	depth  int
	quoted bool
	// open and close are matched by index: open[k] nests, close[k] unnests.
	open, close string
}

// next advances to the next significant byte and reports it, along with the depth
// AFTER any delimiter at that position is counted.
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
// Braces ONLY. A stray `(` must not consume the closing brace — see the scanner's
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
// Every bracket pair nests here, including braces: §10.3's map literals put a
// comma inside `{a: b, c: d}` that is NOT an argument separator.
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

	// A literal is only an ARGUMENT (PLAN.md §10.3). Refused here rather than
	// left to parseReference, which would report "malformed reference" and send
	// the reader after a name they did not write.
	if src[0] == '{' || src[0] == '[' {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "a literal may only appear as a function argument",
			Detail: "`" + src + "` is written where a variable, a resource attribute or a " +
				"function call is expected. YAML already expresses maps and lists, so the " +
				"language does not.",
			Action: "Write the value as YAML, or pass it to a function: " +
				"`${merge(tags, " + src + ")}`.",
			Origin: origin,
		})
		return nil
	}

	if open := strings.Index(src, "("); open >= 0 {
		return parseCall(src, open, origin, ds)
	}
	return parseReference(src, origin, ds)
}

// parseArgument parses one argument of a call, which is the ONLY position a map
// or list literal may appear in (PLAN.md §10.3).
//
// Bounding literals to this position is what keeps them from being the first step
// toward a programming language: YAML already expresses a map everywhere else.
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
// INSIDE A LITERAL, A BARE WORD IS A STRING, not a variable. `{team: payments}`
// means the string "payments", which is what the YAML around it would mean and
// what anyone writing it expects. Treating it as a reference would make the
// obvious spelling silently resolve to something else — and there is no need
// for it, because a variable belongs in another argument: `merge(tags, {...})`.
//
// Keys are text, unquoted and uninterpolated, for the reason §10.1 gives: a
// configuration's shape must not depend on a value.
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
// parser, and anything it cannot read unambiguously stays text, which is the
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

func parseReference(src string, origin value.Origin, ds *diag.Diagnostics) *value.Expr {
	segments := strings.Split(src, ".")
	for _, s := range segments {
		if strings.TrimSpace(s) == "" {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "malformed reference " + strconv.Quote(src),
				Detail:   fmt.Sprintf("%q has an empty name segment.", src),
				Action:   "Write ${name} for a variable, or ${resource.attribute} for a resource attribute.",
				Origin:   origin,
			})
			return nil
		}
	}

	// A user-written reference never contains a `module` segment. Qualify
	// PRODUCES module-qualified targets at stage 6; the parser must never accept
	// one (contract Amendment 14a).
	//
	// This is not a stylistic rule. ${module.prod.database.id} parses to the
	// target name "module.prod.database", and address.Address{Name:
	// "module.prod.database"}.String() returns that verbatim — byte for byte
	// what Address{Module: ["prod"], Name: "database"} renders for the real
	// resource inside instance prod. Stage 6 keys its target map by canonical
	// address, so the two collide and a module's internals become addressable
	// from outside it. A module exposes its outputs, not its resources
	// (PLAN.md §11.2).
	//
	// At parse time the two are trivially distinguishable, because Qualify has
	// not run and never routes through here — it sets Target.Module structurally
	// on an already-parsed expression. A guard at the lookup instead would have
	// to tell "qualified by Qualify" from "typed by the user" when both are the
	// same bytes, which is not a check that can be made right.
	for i, s := range segments {
		if s != "module" {
			continue
		}
		// Two different mistakes wear the same segment, and telling a user to
		// reference an output would be nonsense for the second.
		if i+2 <= len(segments)-1 {
			instance := segments[i+1]
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

	// `var` is the variable namespace. Stripping it HERE means nothing below
	// the parser learns the prefix exists: variables.Scope is still keyed on
	// the bare name, and the process variables seeded by
	// compiler.seedProcessVariables need no change at all.
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
		return &value.Expr{
			Op:     value.OpVarRef,
			Ref:    value.VarRef(segments[1]),
			Origin: origin,
		}
	}

	// One segment is a variable; two or more is a resource attribute. The
	// compiler resolves each against a different scope.
	//
	// Both are SCOPE-RELATIVE: the parser has no scope, so it cannot know
	// whether it is reading a module file or infra.yml, and a reference
	// written inside a module is re-rooted by stage 5 rather than here.
	if len(segments) == 1 {
		return &value.Expr{
			Op:     value.OpVarRef,
			Ref:    value.VarRef(segments[0]),
			Origin: origin,
		}
	}
	return &value.Expr{
		Op:     value.OpResourceRef,
		Ref:    value.LocalRef(strings.Join(segments[:len(segments)-1], "."), segments[len(segments)-1]),
		Origin: origin,
	}
}
