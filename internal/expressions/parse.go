// Package expressions parses and evaluates infra's deliberately minimal
// expression language: interpolation, references to other resources'
// attributes, and a fixed set of pure functions. It is not a programming
// language and must not grow into one (spec §6).
package expressions

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/infrata/infrata/internal/diag"
	"github.com/infrata/infrata/pkg/value"
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

// skipEscape reports the index to continue scanning from when src[i] begins an
// escape sequence inside a quoted literal, and whether it did.
//
// matchBrace and splitArgs both scan for delimiters while tracking quotes, and
// both must agree about what is escaped. Sharing this is not tidiness: when two
// scanners disagree, input is accepted by one and rejected by the other, which
// is the class of bug the quote handling was added to fix in the first place.
func skipEscape(src string, i int, quoted bool) (int, bool) {
	if quoted && src[i] == '\\' && i+1 < len(src) {
		return i + 1, true
	}
	return i, false
}

// matchBrace returns the index of the } closing the interpolation that starts
// at from, accounting for nesting and quoted literals, or -1 if there is none.
func matchBrace(src string, from int) int {
	depth := 1
	quoted := false
	for i := from; i < len(src); i++ {
		if next, skipped := skipEscape(src, i, quoted); skipped {
			i = next
			continue
		}
		// Toggle quote state
		if src[i] == '"' {
			quoted = !quoted
			continue
		}
		// Only count braces outside of quotes
		if !quoted {
			switch src[i] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					return i
				}
			}
		}
	}
	return -1
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

	if open := strings.Index(src, "("); open >= 0 {
		return parseCall(src, open, origin, ds)
	}
	return parseReference(src, origin, ds)
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
		if a := parseExpr(raw, origin, ds); a != nil {
			args = append(args, a)
		}
	}

	return &value.Expr{Op: value.OpCall, Function: name, Args: args, Origin: origin}
}

// splitArgs splits on commas that are not inside parentheses or quotes.
func splitArgs(src string) []string {
	var out []string
	depth, quoted, start := 0, false, 0
	for i := 0; i < len(src); i++ {
		if next, skipped := skipEscape(src, i, quoted); skipped {
			i = next
			continue
		}
		switch src[i] {
		case '"':
			quoted = !quoted
		case '(':
			if !quoted {
				depth++
			}
		case ')':
			if !quoted {
				depth--
			}
		case ',':
			if !quoted && depth == 0 {
				out = append(out, src[start:i])
				start = i + 1
			}
		}
	}
	return append(out, src[start:])
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

	// One segment is a variable; two or more is a resource attribute. The
	// compiler resolves each against a different scope.
	if len(segments) == 1 {
		return &value.Expr{
			Op:     value.OpVarRef,
			Ref:    value.Reference{Resource: segments[0]},
			Origin: origin,
		}
	}
	return &value.Expr{
		Op: value.OpResourceRef,
		Ref: value.Reference{
			Resource:  strings.Join(segments[:len(segments)-1], "."),
			Attribute: segments[len(segments)-1],
		},
		Origin: origin,
	}
}
