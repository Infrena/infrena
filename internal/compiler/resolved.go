// Package compiler turns typed configuration declarations into resolved
// configuration: the seam of the system, which everything above produces and
// everything below consumes (spec §5.3).
package compiler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"

	"github.com/infrata/infrata/pkg/address"
	"github.com/infrata/infrata/pkg/resource"
	"github.com/infrata/infrata/pkg/value"
)

// ResolvedConfig is fully-resolved desired state for one environment.
type ResolvedConfig struct {
	Project     string
	Environment string
	Resources   map[string]*resource.ResolvedResource // keyed by Address.String()
}

// Options carries what compilation needs beyond the files themselves.
type Options struct {
	// Dir is the project directory. Stage 5 resolves a module's relative
	// `source:` against it, so passing anything but the value --chdir
	// produced makes modules silently wrong while everything else stays
	// right. It travels in Options rather than as a parameter because
	// compilerOptions is the single place the CLI builds this.
	Dir string

	// RecordLocks permits writing modules.lock after a successful expansion.
	//
	// The caller decides, not this package: `validate` answers a question and
	// must not mutate the thing it is answering about (Amendment 18b), while
	// `apply` is already permitted to change the project directory. A command
	// that only reads leaves it false and still gets the COMPARISON, which is a
	// pure read — that separation is what lets validate report a moved tag.
	RecordLocks bool

	Environment string

	// Version is the running build, for a project's `infrata:` floor to be checked
	// against (PLAN.md §61.2). Empty means a development build, which every floor
	// exempts.
	//
	// An INPUT rather than read from internal/version, so that compilation is a pure
	// function of what it is given — and so the wiring is testable at all: a test
	// binary reports a development version, which is exempt, so a Compile that read
	// the build version itself could never be checked for actually calling the check.
	Version string

	// Ctx bounds plugin startup. Compilation itself is pure and never blocks, but
	// stage 4.5 launches provider plugins, and a plugin that never answers must be
	// interruptible by the same Ctrl-C that stops everything else.
	//
	// A context in a struct is usually a mistake. It is here because Options is the
	// single place the CLI assembles what compilation needs, and threading a
	// parameter through eight stages that do not use it would put it in every
	// signature to be used by one.
	Ctx      context.Context
	Vars     map[string]string      // from --var
	FileVars map[string]value.Value // from --var-file
}

// Context is Options.Ctx, or Background when the caller set none.
func (o Options) Context() context.Context {
	if o.Ctx != nil {
		return o.Ctx
	}
	return context.Background()
}

// Get returns one resolved resource.
func (c ResolvedConfig) Get(addr address.Address) (*resource.ResolvedResource, bool) {
	r, ok := c.Resources[addr.String()]
	return r, ok
}

// Addresses returns every configured address, sorted.
func (c ResolvedConfig) Addresses() []address.Address {
	out := make([]address.Address, 0, len(c.Resources))
	for _, r := range c.Resources {
		out = append(out, r.Address)
	}
	address.Sort(out)
	return out
}

// Hash returns a canonical fingerprint of desired state.
//
// It covers attribute values, their provenance, and lifecycle. It excludes
// Origin: moving a resource between lines of a file is not a change in desired
// state (spec §12.1). The encoding is written by hand rather than via
// encoding/json so that what is and is not covered is explicit and cannot
// drift when a struct gains a field.
func (c ResolvedConfig) Hash() (string, error) {
	h := sha256.New()

	write := func(parts ...string) {
		for _, p := range parts {
			// Length-prefix every field so that concatenation is unambiguous:
			// without it, {"ab","c"} and {"a","bc"} would hash identically.
			fmt.Fprintf(h, "%d:%s", len(p), p)
		}
	}

	write("project", c.Project, "environment", c.Environment)

	for _, addr := range c.Addresses() {
		r := c.Resources[addr.String()]
		write("resource", addr.String(), "type", r.Type)
		write("prevent_destroy", strconv.FormatBool(r.Lifecycle.PreventDestroy))
		write("retain", strconv.FormatBool(r.Lifecycle.Retain))

		deps := make([]string, 0, len(r.DependsOn))
		for _, d := range r.DependsOn {
			deps = append(deps, d.String())
		}
		// Redundant with bind.go's sortedAddresses, which already sorted
		// DependsOn when it built it from a map — deliberately kept as
		// defence in depth, because Hash is a pure function of the
		// ResolvedConfig it is handed and M4 will hand it configs from
		// sources other than bind. Pinned in its own right by
		// TestHashIsIndependentOfDependencyOrder (resolved_test.go), which
		// hands Hash the unsorted input bind would never produce.
		sort.Strings(deps)
		for _, d := range deps {
			write("depends_on", d)
		}

		names := make([]string, 0, len(r.Attrs))
		for name := range r.Attrs {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			write("attr", name)
			hashValue(r.Attrs[name], write)
		}
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

var exprOpNames = map[value.ExprOp]string{
	value.OpLiteral:     "literal",
	value.OpVarRef:      "var",
	value.OpResourceRef: "resource",
	value.OpConcat:      "concat",
	value.OpCall:        "call",
}

func hashValue(v value.Value, write func(...string)) {
	write("kind", v.Kind.String(), "source", string(v.Source))
	write("known", strconv.FormatBool(v.Known), "sensitive", strconv.FormatBool(v.Sensitive))
	if !v.Known {
		// An unresolved value's identity is the expression that will produce
		// it. Without this, two configurations differing only in WHICH
		// resource an attribute references hash identically — exactly the case
		// references exist to express, and exactly what M6's staleness check
		// must not miss.
		hashExpr(v.Expr, write)
		return
	}

	switch v.Kind {
	case value.KindList:
		items, ok := v.Raw.([]value.Value)
		if !ok {
			// A malformed value must not hash like an empty one.
			write("malformed", "list")
			return
		}
		write("list", strconv.Itoa(len(items)))
		for _, item := range items {
			hashValue(item, write)
		}
	case value.KindMap:
		m, ok := v.Raw.(map[string]value.Value)
		if !ok {
			// A malformed value must not hash like an empty one.
			write("malformed", "map")
			return
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		write("map", strconv.Itoa(len(keys)))
		for _, k := range keys {
			write("key", k)
			hashValue(m[k], write)
		}
	default:
		write("raw", fmt.Sprintf("%v", v.Raw))
	}
}

// hashExpr folds an unresolved value's expression into the hash.
func hashExpr(e *value.Expr, write func(...string)) {
	if e == nil {
		write("expr", "none")
		return
	}
	name, ok := exprOpNames[e.Op]
	if !ok {
		name = "unknown-op"
	}
	write("expr", name, "fn", e.Function, "ref", e.Ref.String())
	if e.Op == value.OpLiteral {
		write("lit", fmt.Sprintf("%v", e.Literal.Raw))
	}
	write("args", strconv.Itoa(len(e.Args)))
	for _, a := range e.Args {
		hashExpr(a, write)
	}
}
