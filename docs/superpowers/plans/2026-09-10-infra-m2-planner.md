# infra M2 — Compiler, Graph and Planner Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `infra plan <environment>` work end to end against the fake provider — compiling configuration into a resolved, provenance-carrying model, diffing it against refreshed provider reality, and rendering a plan a user can read and trust.

**Architecture:** Three pure compiler stages (reference binding, schema binding and defaults, whole-graph validation) turn M1's typed declarations into `ResolvedConfig`. A concurrent read-only `Refresh` gathers provider reality. A pure `Plan(cfg, state, observations, opts)` function decides one operation per resource, and a pure renderer turns the result into text. Purity is the point: it is what makes determinism a property test rather than an aspiration.

**Tech Stack:** Go 1.24, Cobra, `gopkg.in/yaml.v3`, standard library `testing`. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-09-09-infra-phase-1-design.md` — read §6 (expressions and unknowns), §7 (compiler pipeline), §10 (refresh), §11 (planner), §12 (plan artifact), §14 (graph and ordering). The spec is the binding authority; where this plan and the spec disagree, the spec wins and the plan is the defect.

## What M1 already shipped, and what this milestone must not re-do

M1 is merged to `main` at tag `m1`. Available and reviewed:

| Package | What you get |
|---|---|
| `infra/pkg/value` | `Value{Kind, Known, Raw, Source, Sensitive, Origin}`, constructors `String/Int/Float/Bool/List/Map/Unknown`, `WithSensitive/WithSource/WithOrigin`, `Equal`, `AsString/AsInt/AsBool`, lossless JSON via a frozen wire-name table. **No `Expr` field yet — Task 1 adds it.** |
| `infra/pkg/address` | `Address{Module, Name}`, `String`, `Parse`, `InModule`, `Sort` |
| `infra/pkg/schema` | `ResourceDefinition`, `Attribute{Kind, Required, Computed, Sensitive, ForceNew, Default, Validate}`, `Requirement{Name, Types, Optional}`, `Capabilities`, `DefaultContext`, `DefaultFunc` |
| `infra/pkg/resource` | `ResolvedResource{Address, Type, Attrs, DependsOn, Lifecycle, Origin}`, `DesiredResource`, `ResourceState`, `Lifecycle{PreventDestroy, Retain}`, `(ResolvedResource).Desired()`, `(*ResourceState).Clone()` |
| `infra/pkg/provider` | `Provider` interface, `Retryability`, `ErrNotImplemented` |
| `infra/internal/diag` | `Diagnostics` with `Add/Extend/HasErrors/Render(io.Writer)`, `Diagnostic{Severity, Summary, Detail, Action, Origin, Related}` |
| `infra/internal/config` | Stages 1–2: `Load(dir)`, `Decode(files)`, `ProjectDecl`, `ResourceDecl{Name, Type, Attributes, DependsOn, Lifecycle, Origin}`, `AttributeDecl{Name, Value, HasExpressions, Origin}` |
| `infra/internal/registry` | `New`, `Register`, `Definition`, `Provider`, `Types` |
| `infra/internal/state` | `State`, `Decode`, `Encode`, `Local` backend with atomic writes and `O_EXCL` locking |
| `infra/providers/test` | Fake provider: `test.network`, `test.database` (with a `tags` map attribute), `test.application`; file-backed hand-editable cloud; three-way retry classification; failure injection |

**Scope boundaries that are easy to get wrong.** Each of these is settled by spec §19 and is not a judgement call:

- **`Refresh` the function is M2; `refresh` the command is M3.** `plan` must read provider reality (spec §10), but only the M3 command persists observations. M2's `plan` uses them in memory and discards them.
- **The graph package is M2; `infra graph` the command is M7.** Build the DAG and its ordering; do not add a CLI command for it.
- **The plan type is serializable in M2; applying a saved plan is M6.** Emit `plan --output`, populate the hashes, and stop there — no `apply <env> plan.json`, no staleness refusal, no `--allow-stale`.
- **No `apply`, `destroy`, `explain` or `import`.** M3, M3, M7 and Phase 2 respectively.
- **Modules, variables and environment inheritance are M4–M5.** Stage 6 binds references within one flat document. `ResolvedConfig.Environment` is a name carried through, not an inheritance system.

## Global Constraints

- **Toolchain:** Go 1.24 is pinned by `mise.toml`, but mise is not active in non-interactive shells. Before any `go` command run `export PATH="$HOME/.local/share/mise/shims:$PATH"` and confirm `go version` prints `go1.24.x`. `make check` already exports it.
- Dependencies stay exactly `github.com/spf13/cobra` and `gopkg.in/yaml.v3`. A third requires a spec amendment.
- `providers/` may import `pkg/`. `providers/` may **never** import `internal/`.
- `yaml.Node` stays confined to `internal/config`. No package outside it may reference yaml.
- No raw `map[string]any` flows through the engine. The three sanctioned exceptions are unchanged: `providers/test`'s cloud file, `internal/state`'s migration functions, and `internal/config`'s decode stage.
- Every state and plan file written to disk is mode `0600`.
- Value provenance is never discarded. Any function returning a resolved value returns a `value.Value`, not its `Raw`.
- **Sensitivity is per-leaf.** Any code that renders, logs or serialises a value must respect it at every nesting level, not only at the top. M1 shipped a leak of exactly this kind.
- Test-driven: failing test first, and confirm it fails for the expected reason. Not complete until `go test ./...`, `go test ./... -race`, `go vet ./...` and `gofmt -l .` are all clean.
- Every exported type, function, method and constant gets a one-line GoDoc comment. Exported struct fields only where the name does not carry the meaning.
- Conventional Commits, ending every message with:
  ```
  Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_015M5QdCFKa6booSVrhnAzTV
  ```

## Lessons from M1 that this plan encodes

M1 produced eleven defects, every one in the plan rather than the implementations. Four recurred as patterns, and the tasks below are shaped to prevent their return:

1. **Never interpret a value by its surface text when its type is available.** `prevent_destroy: True` silently disabled a destruction guard because `"True" != "true"`. Wherever this plan compares or switches on a value, it does so on `Kind`, `Tag` or a decoded type — never on scalar text.
2. **Fix patterns, not instances.** That same bug survived its first fix by living one function away. Every fix task below asks where else the shape occurs.
3. **A regression test that passes against the unfixed code is worse than none.** Every task that fixes behaviour must confirm its test fails first, with the output quoted.
4. **Sensitivity must be handled recursively.** M1's `state show` leaked secrets nested inside composites, and no existing test could reach it.

---

## File Structure

| File | Responsibility |
|---|---|
| `pkg/value/value.go` *(modify)* | Add the `Expr *Expr` field to `Value` |
| `pkg/value/expr.go` | `Expr`, `ExprOp`, `Reference` — the expression AST only |
| `internal/expressions/parse.go` | `${...}` source → `*value.Expr`; interpolation, refs, calls |
| `internal/expressions/funcs.go` | The six pure functions and their registry |
| `internal/expressions/eval.go` | `Evaluate(expr, Scope) (value.Value, diag.Diagnostics)`; unknown and sensitivity propagation |
| `internal/compiler/resolved.go` | `ResolvedConfig`, `Options`, canonical hashing |
| `internal/compiler/bind.go` | Stage 6: reference binding, deferred unknowns, dependency edges |
| `internal/compiler/schema.go` | Stage 7: schema binding, kind checking, default resolution |
| `internal/compiler/validate.go` | Stage 8: cycles, requirements, lifecycle validity |
| `internal/compiler/compile.go` | `Compile(files, environment, opts)` orchestration |
| `internal/graph/graph.go` | Generic DAG: nodes, edges, cycle detection, topological layers |
| `internal/graph/build.go` | Plan operations → execution graph with kind-aware ordering (Task 18) |
| `internal/refresh/refresh.go` | `Refresh(ctx, state, registry, parallelism) Observations` — concurrent, read-only |
| `internal/planner/plan.go` | `Plan` and `Operation` types, `Canonical()`, JSON |
| `internal/planner/planner.go` | `Plan(cfg, state, obs, opts)` — the pure decision function |
| `internal/planner/diff.go` | Attribute diffing, `ForceNew` promotion, change reasons |
| `internal/planner/render.go` | `Render(plan, opts) string` — pure, golden-tested |
| `internal/cli/plan.go` | `infra plan <environment>` |
| `tests/integration/m2_test.go` | CLI-level tests: plan on a fresh project, drift, destroy proposal |

**Why the AST and the evaluator live in different packages.** `value.Value` must carry the expression that will produce it (spec §6), and spec §6's `Expr` carries a `Literal Value` — so AST and value are mutually recursive and **must** share a package or the import cycles. The AST therefore lives in `pkg/value/expr.go`. The parser, functions and evaluator have no such constraint and stay in `internal/expressions` exactly as spec §17 lays out, importing `pkg/value` one way. Nothing in `pkg/` needs to parse or evaluate — only the compiler and executor do, and both are in `internal/`.

An implementer who tries to put `Expr` in its own package outside `pkg/value` will hit `import cycle not allowed` immediately. That is expected; do not solve it by weakening `Value.Expr` to `any` or an interface, which would discard the type safety the two-phase evaluator depends on.

---

## Task 1: Expression AST

**Files:**
- Create: `pkg/value/expr.go`
- Modify: `pkg/value/value.go` (add one field), `pkg/value/json.go` (exclude the new field)
- Test: `pkg/value/expr_test.go`

**Interfaces:**
- Consumes: `value.Value`, `value.Origin`, `value.Kind` (already shipped).
- Produces: `value.ExprOp` with constants `OpLiteral`, `OpVarRef`, `OpResourceRef`, `OpConcat`, `OpCall`; `value.Reference{Resource, Attribute string}` with `String()`; `value.Expr{Op, Literal, Ref, Args, Function, Origin}` with `(*Expr).References() []Reference` and `(*Expr).String() string`; and a new `Expr *Expr` field on `Value`.

The AST lives beside `Value` because the two are mutually recursive: an unknown `Value` carries the `Expr` that will produce it, and an `Expr` carries literal `Value`s. Any other placement is an import cycle.

- [ ] **Step 1: Write the failing test**

Create `pkg/value/expr_test.go`:

```go
package value

import "testing"

func TestReferenceString(t *testing.T) {
	r := Reference{Resource: "database", Attribute: "endpoint"}
	if got := r.String(); got != "database.endpoint" {
		t.Errorf("String() = %q, want \"database.endpoint\"", got)
	}
}

func TestReferencesCollectsFromNestedExpr(t *testing.T) {
	// ${lower(database.endpoint)}-${network.id}
	e := &Expr{
		Op: OpConcat,
		Args: []*Expr{
			{Op: OpCall, Function: "lower", Args: []*Expr{
				{Op: OpResourceRef, Ref: Reference{Resource: "database", Attribute: "endpoint"}},
			}},
			{Op: OpLiteral, Literal: String("-", SourceExplicit)},
			{Op: OpResourceRef, Ref: Reference{Resource: "network", Attribute: "id"}},
		},
	}

	got := e.References()
	if len(got) != 2 {
		t.Fatalf("References() returned %d, want 2: %v", len(got), got)
	}
	if got[0].String() != "database.endpoint" || got[1].String() != "network.id" {
		t.Errorf("References() = %v, want [database.endpoint network.id] in source order", got)
	}
}

func TestReferencesDeduplicates(t *testing.T) {
	e := &Expr{Op: OpConcat, Args: []*Expr{
		{Op: OpResourceRef, Ref: Reference{Resource: "db", Attribute: "id"}},
		{Op: OpResourceRef, Ref: Reference{Resource: "db", Attribute: "id"}},
	}}
	if got := e.References(); len(got) != 1 {
		t.Errorf("References() = %v, want one entry — a reference used twice is one dependency edge", got)
	}
}

func TestReferencesOnNilIsEmptyNotPanic(t *testing.T) {
	var e *Expr
	if got := e.References(); len(got) != 0 {
		t.Errorf("References() on nil = %v, want empty", got)
	}
}

func TestStringRendersPasteableSource(t *testing.T) {
	// A diagnostic that shows source a user cannot paste back is a trap. The
	// delimiters belong at the top level only: rendering a call's arguments
	// through String() would give ${lower(${db.endpoint})}.
	ref := func(res, attr string) *Expr {
		return &Expr{Op: OpResourceRef, Ref: Reference{Resource: res, Attribute: attr}}
	}

	cases := []struct {
		name string
		in   *Expr
		want string
	}{
		{
			name: "lone reference",
			in:   ref("database", "endpoint"),
			want: "${database.endpoint}",
		},
		{
			name: "call over a reference",
			in:   &Expr{Op: OpCall, Function: "lower", Args: []*Expr{ref("database", "engine")}},
			want: "${lower(database.engine)}",
		},
		{
			name: "nested call",
			in: &Expr{Op: OpCall, Function: "upper", Args: []*Expr{
				{Op: OpCall, Function: "lower", Args: []*Expr{ref("database", "engine")}},
			}},
			want: "${upper(lower(database.engine))}",
		},
		{
			name: "call with quoted literals",
			in: &Expr{Op: OpCall, Function: "replace", Args: []*Expr{
				ref("database", "engine"),
				{Op: OpLiteral, Literal: String("sql", SourceExplicit)},
				{Op: OpLiteral, Literal: String("SQL", SourceExplicit)},
			}},
			want: `${replace(database.engine, "sql", "SQL")}`,
		},
		{
			name: "concat of literal and reference",
			in: &Expr{Op: OpConcat, Args: []*Expr{
				{Op: OpLiteral, Literal: String("prefix-", SourceExplicit)},
				ref("network", "id"),
			}},
			want: "prefix-${network.id}",
		},
	}

	for _, tc := range cases {
		if got := tc.in.String(); got != tc.want {
			t.Errorf("%s: String() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestStringOnNilIsEmptyNotPanic(t *testing.T) {
	var e *Expr
	if got := e.String(); got != "" {
		t.Errorf("String() on nil = %q, want empty", got)
	}
}

func TestValueCarriesExpr(t *testing.T) {
	e := &Expr{Op: OpResourceRef, Ref: Reference{Resource: "db", Attribute: "endpoint"}}
	v := Unknown(KindString, SourceComputed)
	v.Expr = e

	if v.Known {
		t.Error("a value awaiting an expression must not be Known")
	}
	if v.Expr.Ref.Resource != "db" {
		t.Error("Value must carry the expression that will produce it")
	}
}

func TestExprIsNotSerialised(t *testing.T) {
	// State on disk records what a provider reported, never a pending
	// expression. Persisting one would resurrect a dangling reference on load.
	v := Unknown(KindString, SourceComputed)
	v.Expr = &Expr{Op: OpResourceRef, Ref: Reference{Resource: "db", Attribute: "endpoint"}}

	data, err := v.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	for _, leak := range []string{"expr", "Expr", "db.endpoint"} {
		if strings.Contains(string(data), leak) {
			t.Errorf("serialised form leaks %q: %s", leak, data)
		}
	}
}
```

The test file's import block is therefore:

```go
import (
	"strings"
	"testing"
)
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./pkg/value/ -run 'TestReference|TestValueCarriesExpr|TestExprIsNot' -v`
Expected: FAIL — `undefined: Reference`, `undefined: Expr`, `undefined: OpConcat`.

- [ ] **Step 3: Implement the AST**

Create `pkg/value/expr.go`:

```go
package value

import (
	"strconv"
	"strings"
)

// ExprOp is the kind of an expression node.
type ExprOp uint8

const (
	// OpLiteral is a constant embedded in an expression.
	OpLiteral ExprOp = iota
	// OpVarRef references a variable. Variables arrive in M4; the node exists
	// now so the AST does not change shape then.
	OpVarRef
	// OpResourceRef references another resource's attribute.
	OpResourceRef
	// OpConcat joins its arguments into one string.
	OpConcat
	// OpCall applies one of the built-in pure functions.
	OpCall
)

// Reference names another resource's attribute.
type Reference struct {
	Resource  string
	Attribute string
}

// String renders a reference in the form used in configuration.
func (r Reference) String() string {
	if r.Attribute == "" {
		return r.Resource
	}
	return r.Resource + "." + r.Attribute
}

// Expr is a parsed expression.
//
// It lives in this package rather than its own because Value and Expr are
// mutually recursive: an unknown Value carries the Expr that will produce it,
// and an Expr carries literal Values.
type Expr struct {
	Op       ExprOp
	Literal  Value     // OpLiteral only
	Ref      Reference // OpVarRef and OpResourceRef only
	Args     []*Expr   // OpConcat and OpCall only
	Function string    // OpCall only
	Origin   Origin
}

// References returns every resource reference in the expression, in source
// order and deduplicated. Each one becomes a dependency edge, and a reference
// used twice is still one edge.
func (e *Expr) References() []Reference {
	if e == nil {
		return nil
	}
	var out []Reference
	seen := map[string]bool{}

	var walk func(*Expr)
	walk = func(n *Expr) {
		if n == nil {
			return
		}
		if n.Op == OpResourceRef && !seen[n.Ref.String()] {
			seen[n.Ref.String()] = true
			out = append(out, n.Ref)
		}
		for _, a := range n.Args {
			walk(a)
		}
	}
	walk(e)
	return out
}

// String renders the expression back toward its configuration source form, for
// diagnostics.
//
// Rendering splits in two because an expression reads differently depending on
// where it sits. At the top level a reference is written ${db.endpoint}, but as
// an argument inside a call it is written db.endpoint — bare. Rendering
// arguments through String() would wrap each of them in delimiters of their own
// and produce ${lower(${db.endpoint})}, which is not syntax a user could paste
// back into their configuration.
func (e *Expr) String() string {
	if e == nil {
		return ""
	}
	switch e.Op {
	case OpLiteral:
		if s, ok := e.Literal.AsString(); ok {
			return s
		}
		return "<literal>"
	case OpConcat:
		var b strings.Builder
		for _, a := range e.Args {
			b.WriteString(a.String())
		}
		return b.String()
	default:
		return "${" + e.inner() + "}"
	}
}

// inner renders an expression as it appears inside ${...}, without the
// delimiters. A literal is re-quoted here because that is how it was written:
// replace(engine, "sql", "SQL") takes quoted arguments, and dropping the quotes
// would render something that no longer parses.
func (e *Expr) inner() string {
	if e == nil {
		return ""
	}
	switch e.Op {
	case OpLiteral:
		if s, ok := e.Literal.AsString(); ok {
			return strconv.Quote(s)
		}
		return "<literal>"
	case OpVarRef, OpResourceRef:
		return e.Ref.String()
	case OpCall:
		parts := make([]string, 0, len(e.Args))
		for _, a := range e.Args {
			parts = append(parts, a.inner())
		}
		return e.Function + "(" + strings.Join(parts, ", ") + ")"
	case OpConcat:
		var b strings.Builder
		for _, a := range e.Args {
			b.WriteString(a.inner())
		}
		return b.String()
	default:
		return "<expr>"
	}
}
```

- [ ] **Step 4: Add the field to Value**

In `pkg/value/value.go`, add to the `Value` struct, after `Sensitive`:

```go
	// Expr is the expression that will produce this value, set when Known is
	// false because the value depends on a resource that does not exist yet.
	// The executor evaluates it once the dependency has been created.
	Expr *Expr
```

- [ ] **Step 5: Confirm Expr is excluded from the wire format**

`pkg/value/json.go`'s `wireValue` has an explicit field list, so `Expr` is already excluded — but that is an accident of construction, not a decision, until a test pins it. `TestExprIsNotSerialised` is that test. Read `MarshalJSON` and confirm it copies fields individually rather than embedding `Value`; if it embeds, exclude `Expr` explicitly and say so in your report.

State records what a provider reported. An expression persisted into state would resurrect a dangling reference on the next load, pointing at a resource that may since have been destroyed.

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./pkg/value/ -v`
Expected: PASS — the six new tests plus every test M1 shipped.

- [ ] **Step 7: Commit**

```bash
git add pkg/value
git commit -m "feat: add the expression AST to the value model"
```

---

## Task 2: Expression parser

**Files:**
- Create: `internal/expressions/parse.go`
- Test: `internal/expressions/parse_test.go`

**Interfaces:**
- Consumes: `value.Expr`, `value.ExprOp` constants, `value.Reference`, `value.String`, `value.Origin` (Task 1); `diag.Diagnostics`, `diag.Diagnostic`, `diag.SeverityError` (M1).
- Produces: `expressions.Parse(src string, origin value.Origin) (*value.Expr, diag.Diagnostics)`.

M1's stage 2 preserves any scalar containing `${` verbatim and flags `HasExpressions`. This task turns that source text into an AST. It parses; it does not evaluate.

The grammar is deliberately tiny and must stay so (spec §6, `PLAN.md` §10):

```
text        := (literal | interpolation)*
interpolation := "${" expr "}"
expr        := call | reference
call        := name "(" [ expr ("," expr)* ] ")"
reference   := name ("." name)*
```

A string may mix literal text and interpolations (`"${project}-db"`), which parses to `OpConcat`. A string that is exactly one interpolation (`"${database.endpoint}"`) parses to that node directly, not wrapped in a concat — this matters because only the unwrapped form can carry a non-string kind through to evaluation.

- [ ] **Step 1: Write the failing test**

Create `internal/expressions/parse_test.go`:

```go
package expressions

import (
	"strings"
	"testing"

	"infra/pkg/value"
)

func mustParse(t *testing.T, src string) *value.Expr {
	t.Helper()
	e, ds := Parse(src, value.Origin{File: "infra.yml", Line: 3})
	if ds.HasErrors() {
		t.Fatalf("Parse(%q) reported errors: %+v", src, ds)
	}
	return e
}

func TestParsePlainTextIsALiteral(t *testing.T) {
	e := mustParse(t, "postgres")
	if e.Op != value.OpLiteral {
		t.Fatalf("Op = %v, want OpLiteral", e.Op)
	}
	if s, _ := e.Literal.AsString(); s != "postgres" {
		t.Errorf("Literal = %q, want \"postgres\"", s)
	}
}

func TestParseLoneInterpolationIsNotWrapped(t *testing.T) {
	// A string that is exactly one interpolation must not become a concat:
	// only the unwrapped form can carry a non-string kind to evaluation.
	e := mustParse(t, "${database.endpoint}")
	if e.Op != value.OpResourceRef {
		t.Fatalf("Op = %v, want OpResourceRef (not wrapped in OpConcat)", e.Op)
	}
	if e.Ref.Resource != "database" || e.Ref.Attribute != "endpoint" {
		t.Errorf("Ref = %+v, want database.endpoint", e.Ref)
	}
}

func TestParseMixedTextIsAConcat(t *testing.T) {
	e := mustParse(t, "${project}-db")
	if e.Op != value.OpConcat {
		t.Fatalf("Op = %v, want OpConcat", e.Op)
	}
	if len(e.Args) != 2 {
		t.Fatalf("Args = %d, want 2", len(e.Args))
	}
	if e.Args[0].Op != value.OpVarRef || e.Args[0].Ref.Resource != "project" {
		t.Errorf("first arg = %+v, want a var ref to project", e.Args[0])
	}
	if s, _ := e.Args[1].Literal.AsString(); s != "-db" {
		t.Errorf("second arg literal = %q, want \"-db\"", s)
	}
}

func TestParseBareNameIsAVarRefNotAResourceRef(t *testing.T) {
	// One segment is a variable; two or more is a resource attribute. The
	// compiler needs the distinction to know which scope to resolve against.
	e := mustParse(t, "${region}")
	if e.Op != value.OpVarRef {
		t.Errorf("Op = %v, want OpVarRef for a single-segment name", e.Op)
	}
}

func TestParseCall(t *testing.T) {
	e := mustParse(t, "${lower(database.engine)}")
	if e.Op != value.OpCall || e.Function != "lower" {
		t.Fatalf("Op = %v Function = %q, want OpCall lower", e.Op, e.Function)
	}
	if len(e.Args) != 1 || e.Args[0].Ref.Resource != "database" {
		t.Errorf("Args = %+v, want one ref to database.engine", e.Args)
	}
}

func TestParseCallWithMultipleArguments(t *testing.T) {
	e := mustParse(t, `${replace(database.engine, "sql", "SQL")}`)
	if len(e.Args) != 3 {
		t.Fatalf("Args = %d, want 3", len(e.Args))
	}
	if s, _ := e.Args[1].Literal.AsString(); s != "sql" {
		t.Errorf("second arg = %q, want the quoted literal \"sql\"", s)
	}
}

func TestParseNestedCall(t *testing.T) {
	e := mustParse(t, "${upper(lower(database.engine))}")
	if e.Function != "upper" || len(e.Args) != 1 {
		t.Fatalf("outer = %+v", e)
	}
	if e.Args[0].Op != value.OpCall || e.Args[0].Function != "lower" {
		t.Errorf("inner = %+v, want a nested lower() call", e.Args[0])
	}
}

func TestParseCollectsReferencesAcrossAConcat(t *testing.T) {
	e := mustParse(t, "${network.id}/${database.endpoint}")
	refs := e.References()
	if len(refs) != 2 {
		t.Fatalf("References() = %v, want 2", refs)
	}
}

func TestParseReportsUnclosedInterpolation(t *testing.T) {
	_, ds := Parse("${database.endpoint", value.Origin{File: "infra.yml", Line: 3})
	if !ds.HasErrors() {
		t.Fatal("an unclosed ${ must be an error, not silently treated as literal text")
	}
	if ds[0].Origin.Line != 3 {
		t.Errorf("diagnostic lost its origin: %+v", ds[0].Origin)
	}
}

func TestParseReportsEmptyInterpolation(t *testing.T) {
	if _, ds := Parse("${}", value.Origin{}); !ds.HasErrors() {
		t.Error("${} must be an error")
	}
}

func TestParseReportsUnclosedCall(t *testing.T) {
	if _, ds := Parse("${lower(database.engine}", value.Origin{}); !ds.HasErrors() {
		t.Error("an unclosed call must be an error")
	}
}

func TestParseReportsTrailingDot(t *testing.T) {
	if _, ds := Parse("${database.}", value.Origin{}); !ds.HasErrors() {
		t.Error("a trailing dot names no attribute and must be an error")
	}
}

func TestParseCollectsEveryError(t *testing.T) {
	_, ds := Parse("${} and ${database.}", value.Origin{})
	if len(ds) < 2 {
		t.Errorf("got %d diagnostics, want at least 2 — parsing reports every problem in one pass", len(ds))
	}
}

func TestParseDiagnosticsNameTheOffendingSource(t *testing.T) {
	_, ds := Parse("${lower(}", value.Origin{File: "infra.yml", Line: 7})
	if len(ds) == 0 {
		t.Fatal("expected a diagnostic")
	}
	var rendered strings.Builder
	ds.Render(&rendered)
	if !strings.Contains(rendered.String(), "infra.yml:7") {
		t.Errorf("diagnostic does not locate the error:\n%s", rendered.String())
	}
}

func TestParseBraceInQuotedLiteral(t *testing.T) {
	// A brace inside a quoted argument is data, not a delimiter. Counting it
	// would end the interpolation early and report a confusing error about
	// whatever followed.
	e := mustParse(t, `${replace(database.engine, "}", "")}`)
	if e.Op != value.OpCall || e.Function != "replace" {
		t.Fatalf("Op = %v Function = %q, want OpCall replace", e.Op, e.Function)
	}
	if len(e.Args) != 3 {
		t.Fatalf("Args = %d, want 3", len(e.Args))
	}
	if s, _ := e.Args[1].Literal.AsString(); s != "}" {
		t.Errorf("second argument = %q, want the literal brace", s)
	}
}

func TestParseEscapedQuoteInLiteral(t *testing.T) {
	e := mustParse(t, `${replace(database.engine, "a"b", "c")}`)
	if len(e.Args) != 3 {
		t.Fatalf("Args = %d, want 3 — an escaped quote must not end the literal", len(e.Args))
	}
	if s, _ := e.Args[1].Literal.AsString(); s != `a"b` {
		t.Errorf("second argument = %q, want `a\"b`", s)
	}
}

func TestParseEscapedDollarIsLiteral(t *testing.T) {
	// $${not_an_expression} is how a user writes a literal dollar-brace.
	e := mustParse(t, "$${literal}")
	if e.Op != value.OpLiteral {
		t.Fatalf("Op = %v, want OpLiteral", e.Op)
	}
	if s, _ := e.Literal.AsString(); s != "${literal}" {
		t.Errorf("Literal = %q, want \"${literal}\"", s)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/expressions/ -v`
Expected: FAIL — the package does not exist.

- [ ] **Step 3: Implement the parser**

Create `internal/expressions/parse.go`:

```go
// Package expressions parses and evaluates infra's deliberately minimal
// expression language: interpolation, references to other resources'
// attributes, and a fixed set of pure functions. It is not a programming
// language and must not grow into one (spec §6).
package expressions

import (
	"fmt"
	"strconv"
	"strings"

	"infra/internal/diag"
	"infra/pkg/value"
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
// at from, accounting for nesting, or -1 if there is none.
//
// Braces inside a quoted literal are not delimiters: ${replace(x, "}", "")} is
// one interpolation, not one that ends at the quoted brace. Counting them would
// truncate the expression and produce a confusing error about the remainder.
func matchBrace(src string, from int) int {
	depth := 1
	quoted := false
	for i := from; i < len(src); i++ {
		if next, skipped := skipEscape(src, i, quoted); skipped {
			i = next
			continue
		}
		if src[i] == '"' {
			quoted = !quoted
			continue
		}
		if quoted {
			continue
		}
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

// splitArgs splits on commas that are not inside parentheses or quotes. An
// escaped character inside a quoted argument is never a delimiter.
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
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/expressions/ -v`
Expected: PASS — fifteen tests.

- [ ] **Step 5: Check the surface-text rule**

Run: `grep -n '== "' internal/expressions/parse.go`

Every hit must be a comparison against a *syntax* character or a fixed keyword, never against a value's data. M1 shipped two bugs where a datum was judged by its text rather than its type. Confirm each hit in your report, or state there are none.

- [ ] **Step 6: Commit**

```bash
git add internal/expressions
git commit -m "feat: parse the expression language into an AST"
```

---

## Task 3: The six pure functions

**Files:**
- Create: `internal/expressions/funcs.go`
- Test: `internal/expressions/funcs_test.go`

**Interfaces:**
- Consumes: `value.Value`, `value.Kind` constants, `value.String/Int/Bool/List`.
- Produces: `expressions.Func` (`func(args []value.Value) (value.Value, error)`), `expressions.Lookup(name string) (Func, int, bool)` returning the function, its arity (`-1` for variadic), and whether it exists, and `expressions.Names() []string` (sorted, for diagnostics).

Spec §6 fixes the set at exactly six: `lower`, `upper`, `trim`, `join`, `replace`, `default`. Adding a seventh is a configuration-language change and needs a spec amendment, not a judgement call. Each is pure, total and side-effect free.

`default` is the only one that inspects knownness: `default(x, fallback)` yields `fallback` when `x` is unknown or an empty string. The others are never called with unknown arguments — the evaluator short-circuits before reaching them (Task 4).

- [ ] **Step 1: Write the failing test**

Create `internal/expressions/funcs_test.go`:

```go
package expressions

import (
	"testing"

	"infra/pkg/value"
)

func call(t *testing.T, name string, args ...value.Value) value.Value {
	t.Helper()
	fn, _, ok := Lookup(name)
	if !ok {
		t.Fatalf("Lookup(%q) not found", name)
	}
	got, err := fn(args)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return got
}

func str(s string) value.Value { return value.String(s, value.SourceExplicit) }

func TestStringFunctions(t *testing.T) {
	cases := []struct {
		name string
		args []value.Value
		want string
	}{
		{"lower", []value.Value{str("PostGres")}, "postgres"},
		{"upper", []value.Value{str("postgres")}, "POSTGRES"},
		{"trim", []value.Value{str("  padded  ")}, "padded"},
		{"replace", []value.Value{str("my-sql-db"), str("sql"), str("SQL")}, "my-SQL-db"},
		{"join", []value.Value{str("-"), value.List([]value.Value{str("a"), str("b")}, value.SourceExplicit)}, "a-b"},
	}
	for _, tc := range cases {
		got, ok := call(t, tc.name, tc.args...).AsString()
		if !ok || got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestDefaultFallsBackOnUnknown(t *testing.T) {
	got := call(t, "default", value.Unknown(value.KindString, value.SourceComputed), str("fallback"))
	if s, _ := got.AsString(); s != "fallback" {
		t.Errorf("default(unknown, fallback) = %q, want \"fallback\"", s)
	}
}

func TestDefaultFallsBackOnEmptyString(t *testing.T) {
	got := call(t, "default", str(""), str("fallback"))
	if s, _ := got.AsString(); s != "fallback" {
		t.Errorf("default(\"\", fallback) = %q, want \"fallback\"", s)
	}
}

func TestDefaultKeepsAPresentValue(t *testing.T) {
	got := call(t, "default", str("present"), str("fallback"))
	if s, _ := got.AsString(); s != "present" {
		t.Errorf("default(present, fallback) = %q, want \"present\"", s)
	}
}

func TestFunctionsPreserveSensitivity(t *testing.T) {
	// Transforming a secret does not declassify it.
	secret := str("hunter2").WithSensitive(true)
	if got := call(t, "upper", secret); !got.Sensitive {
		t.Error("upper() of a sensitive value must stay sensitive")
	}
	if got := call(t, "replace", secret, str("h"), str("H")); !got.Sensitive {
		t.Error("replace() of a sensitive value must stay sensitive")
	}
}

func TestSensitivityUnionsEveryArgumentPosition(t *testing.T) {
	// Both leaks this project shipped were an argument position omitted from a
	// hand-written union: join's separator, then replace's search string. A
	// secret search term reveals its own position through an unclassified
	// result, which is a side channel on the secret's content.
	secret := str("hunter2").WithSensitive(true)
	plain := str("plain")

	cases := []struct {
		name string
		fn   string
		args []value.Value
	}{
		{"replace: sensitive subject", "replace", []value.Value{secret, plain, plain}},
		{"replace: sensitive search term", "replace", []value.Value{plain, secret, plain}},
		{"replace: sensitive replacement", "replace", []value.Value{plain, plain, secret}},
		{"join: sensitive separator", "join", []value.Value{secret, value.List([]value.Value{plain}, value.SourceExplicit)}},
		{"join: sensitive element", "join", []value.Value{plain, value.List([]value.Value{plain, secret}, value.SourceExplicit)}},
		{"lower: sensitive subject", "lower", []value.Value{secret}},
		{"upper: sensitive subject", "upper", []value.Value{secret}},
		{"trim: sensitive subject", "trim", []value.Value{secret}},
	}

	for _, tc := range cases {
		if got := call(t, tc.fn, tc.args...); !got.Sensitive {
			t.Errorf("%s: result is not sensitive — a secret in any argument position classifies the result", tc.name)
		}
	}
}

func TestWrongArityIsAnError(t *testing.T) {
	fn, _, _ := Lookup("replace")
	if _, err := fn([]value.Value{str("only-one")}); err == nil {
		t.Error("replace with one argument must error, not silently do nothing")
	}
}

func TestWrongKindIsAnError(t *testing.T) {
	fn, _, _ := Lookup("lower")
	if _, err := fn([]value.Value{value.Int(42, value.SourceExplicit)}); err == nil {
		t.Error("lower(42) must error rather than coercing")
	}
}

func TestUnknownFunctionIsNotFound(t *testing.T) {
	if _, _, ok := Lookup("md5"); ok {
		t.Error("only the six specified functions may exist; adding one is a spec change")
	}
}

func TestNamesIsSortedAndComplete(t *testing.T) {
	want := []string{"default", "join", "lower", "replace", "trim", "upper"}
	got := Names()
	if len(got) != len(want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Names() = %v, want %v — sorted, for stable diagnostics", got, want)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/expressions/ -run 'TestString|TestDefault|TestFunctions|TestWrong|TestUnknownFunction|TestNames' -v`
Expected: FAIL — `undefined: Lookup`.

- [ ] **Step 3: Implement the functions**

Create `internal/expressions/funcs.go`:

```go
package expressions

import (
	"fmt"
	"sort"
	"strings"

	"infra/pkg/value"
)

// Func is a built-in expression function. Every one is pure, total and
// side-effect free.
type Func func(args []value.Value) (value.Value, error)

type builtin struct {
	fn    Func
	arity int // -1 for variadic
}

// Lookup returns a built-in function, its arity, and whether it exists.
// An arity of -1 means variadic.
func Lookup(name string) (Func, int, bool) {
	b, ok := builtins[name]
	return b.fn, b.arity, ok
}

// Names returns every built-in function name, sorted, for diagnostics.
func Names() []string {
	out := make([]string, 0, len(builtins))
	for name := range builtins {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// builtins is the complete set. Spec §6 fixes it at six; adding one is a
// configuration-language change requiring a spec amendment.
var builtins = map[string]builtin{
	"lower":   {arity: 1, fn: stringFunc(strings.ToLower)},
	"upper":   {arity: 1, fn: stringFunc(strings.ToUpper)},
	"trim":    {arity: 1, fn: stringFunc(strings.TrimSpace)},
	"replace": {arity: 3, fn: replaceFunc},
	"join":    {arity: 2, fn: joinFunc},
	"default": {arity: 2, fn: defaultFunc},
}

// sensitiveAnywhere reports whether a value, or any leaf inside it, is
// sensitive. Sensitivity is a per-leaf property: a list is classified when any
// element is, even when the list itself carries no flag.
func sensitiveAnywhere(v value.Value) bool {
	if v.Sensitive {
		return true
	}
	switch v.Kind {
	case value.KindList:
		items, ok := v.Raw.([]value.Value)
		if !ok {
			// A value whose Raw does not match its Kind cannot be inspected.
			// Its sensitivity is unknown, so classify it: over-redacting a
			// corrupt value is recoverable, leaking a secret is not. A security
			// check that cannot verify safety must deny.
			return true
		}
		for _, item := range items {
			if sensitiveAnywhere(item) {
				return true
			}
		}
	case value.KindMap:
		m, ok := v.Raw.(map[string]value.Value)
		if !ok {
			return true
		}
		for _, item := range m {
			if sensitiveAnywhere(item) {
				return true
			}
		}
	}
	return false
}

// anySensitive reports whether any argument contributes sensitivity.
//
// Every built-in whose result derives from all its arguments uses this rather
// than hand-writing its own union. Hand-written unions are how this shipped
// wrong twice: join() omitted its separator, and replace() omitted its search
// string — which let a secret search term reveal its own position through an
// unclassified result.
func anySensitive(args ...value.Value) bool {
	for _, a := range args {
		if sensitiveAnywhere(a) {
			return true
		}
	}
	return false
}

// stringFunc lifts a string transform into a Func, preserving sensitivity:
// transforming a secret does not declassify it.
func stringFunc(transform func(string) string) Func {
	return func(args []value.Value) (value.Value, error) {
		if len(args) != 1 {
			return value.Value{}, fmt.Errorf("expected 1 argument, got %d", len(args))
		}
		s, ok := args[0].AsString()
		if !ok {
			return value.Value{}, fmt.Errorf("expected a string, got %s", args[0].Kind)
		}
		return value.String(transform(s), value.SourceComputed).
			WithSensitive(anySensitive(args...)).
			WithOrigin(args[0].Origin), nil
	}
}

func replaceFunc(args []value.Value) (value.Value, error) {
	if len(args) != 3 {
		return value.Value{}, fmt.Errorf("expected 3 arguments (string, old, new), got %d", len(args))
	}
	in, ok := args[0].AsString()
	if !ok {
		return value.Value{}, fmt.Errorf("first argument must be a string, got %s", args[0].Kind)
	}
	old, ok := args[1].AsString()
	if !ok {
		return value.Value{}, fmt.Errorf("second argument must be a string, got %s", args[1].Kind)
	}
	replacement, ok := args[2].AsString()
	if !ok {
		return value.Value{}, fmt.Errorf("third argument must be a string, got %s", args[2].Kind)
	}
	return value.String(strings.ReplaceAll(in, old, replacement), value.SourceComputed).
		WithSensitive(anySensitive(args...)).
		WithOrigin(args[0].Origin), nil
}

func joinFunc(args []value.Value) (value.Value, error) {
	if len(args) != 2 {
		return value.Value{}, fmt.Errorf("expected 2 arguments (separator, list), got %d", len(args))
	}
	sep, ok := args[0].AsString()
	if !ok {
		return value.Value{}, fmt.Errorf("separator must be a string, got %s", args[0].Kind)
	}
	items, ok := args[1].Raw.([]value.Value)
	if !ok || args[1].Kind != value.KindList {
		return value.Value{}, fmt.Errorf("second argument must be a list, got %s", args[1].Kind)
	}

	parts := make([]string, 0, len(items))
	for _, item := range items {
		s, ok := item.AsString()
		if !ok {
			return value.Value{}, fmt.Errorf("join needs a list of strings, found %s", item.Kind)
		}
		parts = append(parts, s)
	}
	// anySensitive recurses into the list, so a secret element classifies the
	// result just as a secret separator does.
	return value.String(strings.Join(parts, sep), value.SourceComputed).
		WithSensitive(anySensitive(args...)).
		WithOrigin(args[1].Origin), nil
}

// defaultFunc is the only built-in that inspects knownness: it exists to
// supply a fallback when a value is unknown or blank.
func defaultFunc(args []value.Value) (value.Value, error) {
	if len(args) != 2 {
		return value.Value{}, fmt.Errorf("expected 2 arguments (value, fallback), got %d", len(args))
	}
	if !args[0].Known {
		return args[1], nil
	}
	if s, ok := args[0].AsString(); ok && s == "" {
		return args[1], nil
	}
	return args[0], nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/expressions/ -v`
Expected: PASS — the nine new tests plus Task 2's fifteen.

- [ ] **Step 5: Commit**

```bash
git add internal/expressions/funcs.go internal/expressions/funcs_test.go
git commit -m "feat: add the six built-in expression functions"
```

---

## Task 4: Expression evaluator

**Files:**
- Create: `internal/expressions/eval.go`
- Test: `internal/expressions/eval_test.go`

**Interfaces:**
- Consumes: `value.Expr`, `expressions.Lookup`, `expressions.Names`, `diag.Diagnostics`.
- Produces: `expressions.Scope` interface with `Variable(name string) (value.Value, bool)` and `Attribute(ref value.Reference) (value.Value, bool)`; `expressions.Evaluate(e *value.Expr, scope Scope) (value.Value, diag.Diagnostics)`.

This is the evaluator both phases share (spec §6). At compile time the scope resolves variables but reports resource attributes as unavailable, producing an unknown carrying the expression. At apply time in M3 the same evaluator runs with a scope that can resolve them. One evaluator, two scopes — not two evaluators that can disagree.

Three rules carry the weight:

**Unknownness is contagious.** Any function or concatenation with an unknown argument yields an unknown result. It does not error, and it does not silently substitute a zero value.

**Sensitivity is a union.** An unknown result carries the union of its arguments' sensitivity, so a secret interpolated into a larger string keeps the result classified even before the secret is known.

**An unknown result carries the expression.** That is how the executor finishes the job in M3.

- [ ] **Step 1: Write the failing test**

Create `internal/expressions/eval_test.go`:

```go
package expressions

import (
	"strings"
	"testing"

	"infra/internal/diag"
	"infra/pkg/value"
)

// testScope resolves variables but not resource attributes, which is exactly
// the compile-time scope.
type testScope struct {
	vars  map[string]value.Value
	attrs map[string]value.Value
}

func (s testScope) Variable(name string) (value.Value, bool) {
	v, ok := s.vars[name]
	return v, ok
}

func (s testScope) Attribute(ref value.Reference) (value.Value, bool) {
	v, ok := s.attrs[ref.String()]
	return v, ok
}

func compileScope() testScope {
	return testScope{vars: map[string]value.Value{
		"project": value.String("myapp", value.SourceVariable),
		"region":  value.String("us-east-1", value.SourceVariable),
	}}
}

func evalSrc(t *testing.T, src string, scope Scope) (value.Value, diag.Diagnostics) {
	t.Helper()
	e, ds := Parse(src, value.Origin{File: "infra.yml", Line: 1})
	if ds.HasErrors() {
		t.Fatalf("Parse(%q): %+v", src, ds)
	}
	return Evaluate(e, scope)
}

func TestEvaluateLiteral(t *testing.T) {
	got, d := evalSrc(t, "postgres", compileScope())
	if d.HasErrors() {
		t.Fatal("literal evaluation should not error")
	}
	if s, _ := got.AsString(); s != "postgres" {
		t.Errorf("= %q, want \"postgres\"", s)
	}
}

func TestEvaluateVariable(t *testing.T) {
	got, _ := evalSrc(t, "${project}", compileScope())
	if s, _ := got.AsString(); s != "myapp" {
		t.Errorf("= %q, want \"myapp\"", s)
	}
	if got.Source != value.SourceVariable {
		t.Errorf("Source = %v, want SourceVariable — provenance survives evaluation", got.Source)
	}
}

func TestEvaluateConcat(t *testing.T) {
	got, _ := evalSrc(t, "${project}-${region}", compileScope())
	if s, _ := got.AsString(); s != "myapp-us-east-1" {
		t.Errorf("= %q", s)
	}
}

func TestEvaluateCall(t *testing.T) {
	got, _ := evalSrc(t, "${upper(project)}", compileScope())
	if s, _ := got.AsString(); s != "MYAPP" {
		t.Errorf("= %q, want \"MYAPP\"", s)
	}
}

func TestUnresolvableAttributeBecomesUnknown(t *testing.T) {
	got, d := evalSrc(t, "${database.endpoint}", compileScope())
	if d.HasErrors() {
		t.Fatal("an attribute the compile scope cannot resolve is unknown, not an error")
	}
	if got.Known {
		t.Error("must be unknown")
	}
	if got.Expr == nil {
		t.Fatal("an unknown value must carry the expression that will produce it")
	}
	if got.Source != value.SourceComputed {
		t.Errorf("Source = %v, want SourceComputed", got.Source)
	}
}

func TestUnknownIsContagiousThroughConcat(t *testing.T) {
	got, _ := evalSrc(t, "${project}-${database.endpoint}", compileScope())
	if got.Known {
		t.Error("a concat with an unknown argument must be unknown")
	}
	if got.Expr == nil {
		t.Error("the result must carry its expression for the executor to finish")
	}
}

func TestUnknownIsContagiousThroughCall(t *testing.T) {
	got, d := evalSrc(t, "${upper(database.endpoint)}", compileScope())
	if d.HasErrors() {
		t.Fatal("an unknown argument is not an error")
	}
	if got.Known {
		t.Error("a call with an unknown argument must be unknown")
	}
	if got.Kind != value.KindString {
		t.Errorf("Kind = %v, want KindString — an unknown still knows its type", got.Kind)
	}
}

func TestSensitivityUnionsThroughConcat(t *testing.T) {
	scope := compileScope()
	scope.vars["secret"] = value.String("hunter2", value.SourceVariable).WithSensitive(true)

	got, _ := evalSrc(t, "prefix-${secret}", scope)
	if !got.Sensitive {
		t.Error("a concat containing a secret must be sensitive")
	}
	if s, _ := got.AsString(); strings.Contains(s, "hunter2") && !got.Sensitive {
		t.Error("secret leaked unclassified")
	}
}

func TestSensitivitySurvivesIntoAnUnknown(t *testing.T) {
	scope := compileScope()
	scope.vars["secret"] = value.String("hunter2", value.SourceVariable).WithSensitive(true)

	got, _ := evalSrc(t, "${secret}-${database.endpoint}", scope)
	if got.Known {
		t.Fatal("should be unknown")
	}
	if !got.Sensitive {
		t.Error("an unknown built from a secret must still be sensitive — it is classified before it is known")
	}
}

func TestResolvableAttributeEvaluates(t *testing.T) {
	// The apply-time scope, where the dependency now exists.
	scope := compileScope()
	scope.attrs = map[string]value.Value{
		"database.endpoint": value.String("db-1.db.test", value.SourceProvider),
	}
	got, _ := evalSrc(t, "${database.endpoint}", scope)
	if !got.Known {
		t.Fatal("an attribute the scope can resolve must evaluate")
	}
	if s, _ := got.AsString(); s != "db-1.db.test" {
		t.Errorf("= %q", s)
	}
}

func TestUnknownVariableIsAnError(t *testing.T) {
	// A variable, unlike a resource attribute, cannot become known later.
	_, d := evalSrc(t, "${nonexistent}", compileScope())
	if !d.HasErrors() {
		t.Error("an undefined variable must be an error, not an unknown")
	}
}

func TestUnknownFunctionIsAnErrorNamingTheAlternatives(t *testing.T) {
	e, _ := Parse("${md5(project)}", value.Origin{})
	_, ds := Evaluate(e, compileScope())
	if !ds.HasErrors() {
		t.Fatal("calling an undefined function must be an error")
	}
	var out strings.Builder
	ds.Render(&out)
	if !strings.Contains(out.String(), "lower") {
		t.Errorf("the diagnostic should list the available functions:\n%s", out.String())
	}
}

func TestFunctionErrorBecomesADiagnostic(t *testing.T) {
	_, d := evalSrc(t, "${lower(project, region)}", compileScope())
	if !d.HasErrors() {
		t.Error("wrong arity must surface as a diagnostic, not a panic or a silent result")
	}
}

func TestEvaluateNilIsEmptyNotPanic(t *testing.T) {
	got, ds := Evaluate(nil, compileScope())
	if ds.HasErrors() {
		t.Error("evaluating nil should not error")
	}
	if got.Known {
		t.Error("evaluating nil yields an unknown, not a known zero")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/expressions/ -run 'TestEvaluate|TestUnknown|TestSensitivity|TestResolvable|TestFunctionError' -v`
Expected: FAIL — `undefined: Evaluate`, `undefined: Scope`.

- [ ] **Step 3: Implement the evaluator**

Create `internal/expressions/eval.go`:

```go
package expressions

import (
	"strconv"
	"strings"

	"infra/internal/diag"
	"infra/pkg/value"
)

// Scope resolves the names an expression refers to.
//
// The compile-time scope resolves variables and reports resource attributes as
// unavailable, which is what turns a reference into an unknown carrying its
// expression. The apply-time scope in M3 resolves both. One evaluator serves
// both so the two phases cannot disagree.
type Scope interface {
	Variable(name string) (value.Value, bool)
	Attribute(ref value.Reference) (value.Value, bool)
}

// Evaluate reduces an expression as far as the scope allows.
//
// A reference the scope cannot resolve yields an unknown value carrying the
// expression, not an error: it will become knowable once its dependency
// exists. An undefined variable IS an error, because no later phase can supply
// it.
func Evaluate(e *value.Expr, scope Scope) (value.Value, diag.Diagnostics) {
	var ds diag.Diagnostics
	if e == nil {
		return value.Unknown(value.KindString, value.SourceComputed), ds
	}
	return evaluate(e, scope, &ds), ds
}

func evaluate(e *value.Expr, scope Scope, ds *diag.Diagnostics) value.Value {
	switch e.Op {
	case value.OpLiteral:
		return e.Literal.WithOrigin(e.Origin)

	case value.OpVarRef:
		v, ok := scope.Variable(e.Ref.Resource)
		if !ok {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "undefined variable " + strconv.Quote(e.Ref.Resource),
				Detail:   "No variable of that name is in scope.",
				Action:   "Define it in variables.yml, or pass --var " + e.Ref.Resource + "=value.",
				Origin:   e.Origin,
			})
			return unknownFrom(e, value.KindString, false)
		}
		return v.WithOrigin(e.Origin)

	case value.OpResourceRef:
		v, ok := scope.Attribute(e.Ref)
		if !ok {
			// Not an error: the dependency simply does not exist yet.
			return unknownFrom(e, value.KindString, false)
		}
		return v.WithOrigin(e.Origin)

	case value.OpConcat:
		return evaluateConcat(e, scope, ds)

	case value.OpCall:
		return evaluateCall(e, scope, ds)

	default:
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "unsupported expression",
			Origin:   e.Origin,
		})
		return unknownFrom(e, value.KindString, false)
	}
}

func evaluateConcat(e *value.Expr, scope Scope, ds *diag.Diagnostics) value.Value {
	var b strings.Builder
	sensitive := false
	known := true

	for _, arg := range e.Args {
		v := evaluate(arg, scope, ds)
		sensitive = sensitive || v.Sensitive
		if !v.Known {
			known = false
			continue
		}
		s, ok := stringify(v)
		if !ok {
			// A composite cannot be interpolated into a string. Schema binding
			// (Task 7) will eventually reject this before evaluation runs, but
			// it does not exist yet, so the evaluator must refuse it here
			// rather than emitting "" — a fabricated empty string is a plan
			// that lies about what it will build.
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "cannot interpolate a " + v.Kind.String() + " value into a string",
				Detail:   "Interpolation accepts only string, integer, float and boolean values.",
				Origin:   arg.Origin,
			})
			known = false
			continue
		}
		b.WriteString(s)
	}

	if !known {
		// Unknownness is contagious, and the result is classified before it is
		// known: a secret in any part makes the whole result sensitive.
		return unknownFrom(e, value.KindString, sensitive)
	}
	return value.String(b.String(), value.SourceComputed).
		WithSensitive(sensitive).
		WithOrigin(e.Origin)
}

func evaluateCall(e *value.Expr, scope Scope, ds *diag.Diagnostics) value.Value {
	fn, _, ok := Lookup(e.Function)
	if !ok {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "undefined function " + strconv.Quote(e.Function),
			Detail:   "Available functions:\n  " + strings.Join(Names(), "\n  "),
			Action:   "Use one of the available functions. The set is fixed by the configuration language.",
			Origin:   e.Origin,
		})
		return unknownFrom(e, value.KindString, false)
	}

	args := make([]value.Value, 0, len(e.Args))
	sensitive := false
	anyUnknown := false
	for _, arg := range e.Args {
		v := evaluate(arg, scope, ds)
		sensitive = sensitive || v.Sensitive
		anyUnknown = anyUnknown || !v.Known
		args = append(args, v)
	}

	// `default` is the one function that is meaningful with an unknown
	// argument — supplying a fallback is its entire purpose.
	if anyUnknown && e.Function != "default" {
		return unknownFrom(e, value.KindString, sensitive)
	}

	out, err := fn(args)
	if err != nil {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "cannot evaluate " + strconv.Quote(e.Function) + ": " + err.Error(),
			Origin:   e.Origin,
		})
		return unknownFrom(e, value.KindString, sensitive)
	}
	// `default` may return an argument verbatim, and that argument's Expr
	// points only at the sub-expression it came from. If the result is still
	// unknown, rebuild it around the whole call — otherwise M3 re-evaluating
	// it would resolve the fallback directly and permanently skip the
	// primary-versus-fallback choice this call exists to make.
	if !out.Known {
		return unknownFrom(e, out.Kind, out.Sensitive || sensitive)
	}
	return out.WithSensitive(out.Sensitive || sensitive).WithOrigin(e.Origin)
}

// unknownFrom builds an unknown carrying the expression that will produce it.
func unknownFrom(e *value.Expr, kind value.Kind, sensitive bool) value.Value {
	v := value.Unknown(kind, value.SourceComputed).
		WithSensitive(sensitive).
		WithOrigin(e.Origin)
	v.Expr = e
	return v
}

// stringify renders a scalar for concatenation, reporting whether it could.
//
// It returns false for a composite rather than an empty string. The caller
// turns that into a diagnostic: silently splicing "" into a result would make
// the plan describe infrastructure nobody asked for.
func stringify(v value.Value) (string, bool) {
	switch v.Kind {
	case value.KindString:
		return v.AsString()
	case value.KindInt:
		n, ok := v.AsInt()
		return strconv.FormatInt(n, 10), ok
	case value.KindBool:
		b, ok := v.AsBool()
		return strconv.FormatBool(b), ok
	case value.KindFloat:
		f, ok := v.Raw.(float64)
		return strconv.FormatFloat(f, 'g', -1, 64), ok
	default:
		return "", false
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/expressions/ -v`
Expected: PASS — fourteen new tests plus the previous twenty-four.

- [ ] **Step 5: Verify the sensitivity rule holds under composition**

Run: `go test ./internal/expressions/ -run TestSensitivity -v`

Both sensitivity tests must pass. M1 shipped a secret leak because sensitivity was checked at one level and not propagated; this evaluator is where that propagation is decided for M2, and the union must hold through concat, through calls, and into unknowns.

- [ ] **Step 6: Commit**

```bash
git add internal/expressions/eval.go internal/expressions/eval_test.go
git commit -m "feat: evaluate expressions with unknown and sensitivity propagation"
```

---

## Task 5: Resolved configuration and canonical hashing

**Files:**
- Create: `internal/compiler/resolved.go`
- Test: `internal/compiler/resolved_test.go`

**Interfaces:**
- Consumes: `resource.ResolvedResource`, `address.Address`, `address.Sort`, `value.Value`.
- Produces: `compiler.ResolvedConfig{Project, Environment string; Resources map[string]*resource.ResolvedResource}` with `Get(address.Address) (*resource.ResolvedResource, bool)`, `Addresses() []address.Address`, `Hash() (string, error)`; and `compiler.Options{Environment, Region, Account string; Vars map[string]string}`.

`ResolvedConfig` is the seam of the whole system (spec §5.3): everything above it produces it, everything below consumes it and knows nothing of how it was made.

`Hash()` is what makes plan staleness detectable in M6 and determinism testable now. Spec §12.1 fixes what it covers: attribute values, provenance and lifecycle included; `Origin` excluded, because moving a resource between lines of a file is not a change in desired state.

- [ ] **Step 1: Write the failing test**

Create `internal/compiler/resolved_test.go`:

```go
package compiler

import (
	"testing"

	"infra/pkg/address"
	"infra/pkg/resource"
	"infra/pkg/value"
)

func cfg(resources ...*resource.ResolvedResource) ResolvedConfig {
	c := ResolvedConfig{Project: "myapp", Environment: "dev", Resources: map[string]*resource.ResolvedResource{}}
	for _, r := range resources {
		c.Resources[r.Address.String()] = r
	}
	return c
}

func res(name, typ string, attrs map[string]value.Value) *resource.ResolvedResource {
	return &resource.ResolvedResource{
		Address: address.Address{Name: name},
		Type:    typ,
		Attrs:   attrs,
	}
}

func TestAddressesAreSorted(t *testing.T) {
	c := cfg(res("zebra", "test.network", nil), res("alpha", "test.network", nil))
	got := c.Addresses()
	if len(got) != 2 || got[0].Name != "alpha" || got[1].Name != "zebra" {
		t.Errorf("Addresses() = %v, want sorted", got)
	}
}

func TestHashIsStableAcrossRuns(t *testing.T) {
	c := cfg(
		res("a", "test.network", map[string]value.Value{"cidr": value.String("10.0.0.0/16", value.SourceExplicit)}),
		res("b", "test.database", map[string]value.Value{"engine": value.String("postgres", value.SourceExplicit)}),
	)
	first, err := c.Hash()
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	for i := 0; i < 20; i++ {
		next, err := c.Hash()
		if err != nil {
			t.Fatalf("Hash: %v", err)
		}
		if next != first {
			t.Fatal("Hash must be stable across runs; Go map iteration is randomised")
		}
	}
}

func TestHashIgnoresOrigin(t *testing.T) {
	// Moving a resource between lines is not a change in desired state.
	a := res("db", "test.database", map[string]value.Value{
		"engine": value.String("postgres", value.SourceExplicit).WithOrigin(value.Origin{File: "infra.yml", Line: 3}),
	})
	b := res("db", "test.database", map[string]value.Value{
		"engine": value.String("postgres", value.SourceExplicit).WithOrigin(value.Origin{File: "infra.yml", Line: 99}),
	})
	b.Origin = value.Origin{Line: 42}

	ha, _ := cfg(a).Hash()
	hb, _ := cfg(b).Hash()
	if ha != hb {
		t.Error("Hash must ignore Origin — a line number is not desired state")
	}
}

func TestHashIncludesProvenance(t *testing.T) {
	// A value that arrived as an explicit setting is not the same desired
	// state as the identical value arriving from a default.
	explicit := res("db", "test.database", map[string]value.Value{
		"size": value.Int(10, value.SourceExplicit),
	})
	defaulted := res("db", "test.database", map[string]value.Value{
		"size": value.Int(10, value.SourceDefault),
	})
	ha, _ := cfg(explicit).Hash()
	hb, _ := cfg(defaulted).Hash()
	if ha == hb {
		t.Error("Hash must include provenance — spec §12.1")
	}
}

func TestHashIncludesLifecycle(t *testing.T) {
	plain := res("db", "test.database", nil)
	guarded := res("db", "test.database", nil)
	guarded.Lifecycle = resource.Lifecycle{PreventDestroy: true}

	ha, _ := cfg(plain).Hash()
	hb, _ := cfg(guarded).Hash()
	if ha == hb {
		t.Error("Hash must include lifecycle — turning on prevent_destroy changes desired state")
	}
}

func TestHashDistinguishesDifferentUnresolvedReferences(t *testing.T) {
	// Both values are unknown at compile time, so everything Hash() looked at
	// before — kind, source, known, sensitive — is identical. What differs is
	// which resource the attribute will resolve from, which is the whole point
	// of a reference.
	unknownRef := func(res, attr string) value.Value {
		v := value.Unknown(value.KindString, value.SourceComputed)
		v.Expr = &value.Expr{
			Op:  value.OpResourceRef,
			Ref: value.Reference{Resource: res, Attribute: attr},
		}
		return v
	}

	a := cfg(res("db", "test.database", map[string]value.Value{"network": unknownRef("network_a", "id")}))
	b := cfg(res("db", "test.database", map[string]value.Value{"network": unknownRef("network_b", "id")}))

	ha, _ := a.Hash()
	hb, _ := b.Hash()
	if ha == hb {
		t.Error("two unresolved values referencing different resources must not hash alike — M6 staleness would miss a changed dependency")
	}
}

func TestHashDistinguishesDifferentCalls(t *testing.T) {
	call := func(fn string) value.Value {
		v := value.Unknown(value.KindString, value.SourceComputed)
		v.Expr = &value.Expr{
			Op:       value.OpCall,
			Function: fn,
			Args:     []*value.Expr{{Op: value.OpResourceRef, Ref: value.Reference{Resource: "db", Attribute: "engine"}}},
		}
		return v
	}
	ha, _ := cfg(res("r", "test.network", map[string]value.Value{"cidr": call("lower")})).Hash()
	hb, _ := cfg(res("r", "test.network", map[string]value.Value{"cidr": call("upper")})).Hash()
	if ha == hb {
		t.Error("lower() and upper() over the same reference are different desired states")
	}
}

func TestHashChangesWithAValue(t *testing.T) {
	a := cfg(res("db", "test.database", map[string]value.Value{"engine": value.String("postgres", value.SourceExplicit)}))
	b := cfg(res("db", "test.database", map[string]value.Value{"engine": value.String("mysql", value.SourceExplicit)}))
	ha, _ := a.Hash()
	hb, _ := b.Hash()
	if ha == hb {
		t.Error("a changed attribute must change the hash")
	}
}

func TestHashDistinguishesUnknownFromEmpty(t *testing.T) {
	unknown := cfg(res("db", "test.database", map[string]value.Value{
		"endpoint": value.Unknown(value.KindString, value.SourceComputed),
	}))
	empty := cfg(res("db", "test.database", map[string]value.Value{
		"endpoint": value.String("", value.SourceExplicit),
	}))
	hu, _ := unknown.Hash()
	he, _ := empty.Hash()
	if hu == he {
		t.Error("an unknown value and an empty string are different desired states")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/compiler/ -v`
Expected: FAIL — the package does not exist.

- [ ] **Step 3: Implement resolved configuration**

Create `internal/compiler/resolved.go`:

```go
// Package compiler turns typed configuration declarations into resolved
// configuration: the seam of the system, which everything above produces and
// everything below consumes (spec §5.3).
package compiler

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"

	"infra/pkg/address"
	"infra/pkg/resource"
	"infra/pkg/value"
)

// ResolvedConfig is fully-resolved desired state for one environment.
type ResolvedConfig struct {
	Project     string
	Environment string
	Resources   map[string]*resource.ResolvedResource // keyed by Address.String()
}

// Options carries what compilation needs beyond the files themselves.
type Options struct {
	Environment string
	Region      string
	Account     string
	Vars        map[string]string // from --var; the variable system proper is M4
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

// exprOpNames pins each operator's hashed name, independently of its ordinal.
// An enum reordering must not silently change every hash, which is the same
// reason Kind's wire names are frozen in their own table.
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
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/compiler/ -v`
Expected: PASS — seven tests.

- [ ] **Step 5: Commit**

```bash
git add internal/compiler
git commit -m "feat: add resolved configuration with canonical hashing"
```

---

## Task 6: Stage 6 — reference binding

**Files:**
- Create: `internal/compiler/bind.go`
- Test: `internal/compiler/bind_test.go`

**Interfaces:**
- Consumes: `config.ProjectDecl`, `config.ResourceDecl`, `config.AttributeDecl`; `expressions.Parse`, `expressions.Evaluate`, `expressions.Scope`; `value.Value`, `value.Expr`; `diag.Diagnostics`.
- Produces: `compiler.bindReferences(project *config.ProjectDecl, opts Options) (ResolvedConfig, diag.Diagnostics)` (unexported, called by `Compile` in Task 9); `compiler.compileScope` implementing `expressions.Scope`.

Stage 6 walks every attribute, parses any that carries an expression, evaluates what it can, and turns what it cannot into an unknown carrying its expression — while recording the dependency edge that reference implies (spec §7).

Two rules decide correctness here:

**A reference to a resource that does not exist is an error, not an unknown.** An unknown means "not yet"; a reference to a name nobody declared will never become knowable, and reporting it at plan time is the difference between a clear message and a confusing failure later.

**A reference to an attribute the target's schema does not define is also an error** — but that check needs schemas, which arrive in stage 7. Stage 6 records the edge; stage 7 validates the attribute exists. Do not try to do it here.

- [ ] **Step 1: Write the failing test**

Create `internal/compiler/bind_test.go`:

```go
package compiler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"infra/internal/config"
	"infra/pkg/value"
)

func decl(t *testing.T, body string) *config.ProjectDecl {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "infra.yml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	files, err := config.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p, ds := config.Decode(files)
	if ds.HasErrors() {
		t.Fatalf("Decode: %+v", ds)
	}
	return p
}

func TestBindLiteralAttributesPassThrough(t *testing.T) {
	p := decl(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: 10.0.0.0/16
`)
	cfg, ds := bindReferences(p, Options{Environment: "dev"})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	r := cfg.Resources["network"]
	if s, _ := r.Attrs["cidr"].AsString(); s != "10.0.0.0/16" {
		t.Errorf("cidr = %q", s)
	}
	if r.Attrs["cidr"].Source != value.SourceExplicit {
		t.Errorf("Source = %v, want SourceExplicit", r.Attrs["cidr"].Source)
	}
}

func TestBindResourceReferenceBecomesUnknownWithAnEdge(t *testing.T) {
	p := decl(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: 10.0.0.0/16
  database:
    type: test.database
    engine: postgres
    network: ${network.id}
`)
	cfg, ds := bindReferences(p, Options{Environment: "dev"})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	db := cfg.Resources["database"]
	if db.Attrs["network"].Known {
		t.Error("a reference to a not-yet-created resource must be unknown")
	}
	if db.Attrs["network"].Expr == nil {
		t.Error("the unknown must carry its expression")
	}
	if len(db.DependsOn) != 1 || db.DependsOn[0].Name != "network" {
		t.Errorf("DependsOn = %v, want one edge to network", db.DependsOn)
	}
}

func TestBindExplicitDependsOnBecomesAnEdge(t *testing.T) {
	p := decl(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: 10.0.0.0/16
  database:
    type: test.database
    engine: postgres
    depends_on: [network]
`)
	cfg, _ := bindReferences(p, Options{Environment: "dev"})
	db := cfg.Resources["database"]
	if len(db.DependsOn) != 1 || db.DependsOn[0].Name != "network" {
		t.Errorf("DependsOn = %v", db.DependsOn)
	}
}

func TestBindDeduplicatesEdges(t *testing.T) {
	// A reference and an explicit depends_on naming the same target is one edge.
	p := decl(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: 10.0.0.0/16
  database:
    type: test.database
    engine: postgres
    network: ${network.id}
    depends_on: [network]
`)
	cfg, _ := bindReferences(p, Options{Environment: "dev"})
	if got := cfg.Resources["database"].DependsOn; len(got) != 1 {
		t.Errorf("DependsOn = %v, want one edge", got)
	}
}

func TestBindReferenceToUnknownResourceIsAnError(t *testing.T) {
	p := decl(t, `
project: myapp
resources:
  database:
    type: test.database
    engine: postgres
    network: ${nonexistent.id}
`)
	_, ds := bindReferences(p, Options{Environment: "dev"})
	if !ds.HasErrors() {
		t.Fatal("a reference to a resource nobody declared can never become knowable and must be an error")
	}
	var out strings.Builder
	ds.Render(&out)
	if !strings.Contains(out.String(), "nonexistent") {
		t.Errorf("diagnostic must name the missing resource:\n%s", out.String())
	}
}

func TestBindDependsOnUnknownResourceIsAnError(t *testing.T) {
	p := decl(t, `
project: myapp
resources:
  database:
    type: test.database
    engine: postgres
    depends_on: [nonexistent]
`)
	if _, ds := bindReferences(p, Options{Environment: "dev"}); !ds.HasErrors() {
		t.Error("depends_on naming an undeclared resource must be an error")
	}
}

func TestBindSelfReferenceIsAnError(t *testing.T) {
	p := decl(t, `
project: myapp
resources:
  database:
    type: test.database
    engine: ${database.engine}
`)
	if _, ds := bindReferences(p, Options{Environment: "dev"}); !ds.HasErrors() {
		t.Error("a resource referring to itself is a cycle of one and must be rejected")
	}
}

func TestBindCarriesLifecycleAndOrigin(t *testing.T) {
	p := decl(t, `
project: myapp
resources:
  database:
    type: test.database
    engine: postgres
    lifecycle:
      prevent_destroy: true
`)
	cfg, _ := bindReferences(p, Options{Environment: "dev"})
	db := cfg.Resources["database"]
	if !db.Lifecycle.PreventDestroy {
		t.Error("lifecycle must survive binding")
	}
	if db.Origin.Line == 0 {
		t.Error("origin must survive binding, or diagnostics downstream lose their location")
	}
}

func TestBindResolvesCliVariables(t *testing.T) {
	p := decl(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: ${cidr_block}
`)
	cfg, ds := bindReferences(p, Options{Environment: "dev", Vars: map[string]string{"cidr_block": "10.9.0.0/16"}})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if s, _ := cfg.Resources["network"].Attrs["cidr"].AsString(); s != "10.9.0.0/16" {
		t.Errorf("cidr = %q, want the --var value", s)
	}
}

func TestBindReportsEveryProblemAtOnce(t *testing.T) {
	p := decl(t, `
project: myapp
resources:
  a:
    type: test.network
    cidr: ${missing_one.id}
  b:
    type: test.network
    cidr: ${missing_two.id}
`)
	_, ds := bindReferences(p, Options{Environment: "dev"})
	if len(ds) < 2 {
		t.Errorf("got %d diagnostics, want at least 2 — one bad reference must not mask the next", len(ds))
	}
}

func TestBindPropagatesSensitivityIntoUnknowns(t *testing.T) {
	p := decl(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: 10.0.0.0/16
  database:
    type: test.database
    engine: postgres
    password: ${secret_value}-${network.id}
`)
	cfg, ds := bindReferences(p, Options{
		Environment: "dev",
		Vars:        map[string]string{"secret_value": "hunter2"},
	})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	// The variable is not marked sensitive here (M4 adds secret vars), so this
	// asserts the mechanism rather than the classification: the unknown must
	// carry its expression so the executor can finish it.
	pw := cfg.Resources["database"].Attrs["password"]
	if pw.Known {
		t.Error("an attribute mixing a variable with a resource reference must be unknown")
	}
	if pw.Expr == nil {
		t.Error("and must carry its expression")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/compiler/ -run TestBind -v`
Expected: FAIL — `undefined: bindReferences`.

- [ ] **Step 3: Implement stage 6**

Create `internal/compiler/bind.go`:

```go
package compiler

import (
	"sort"
	"strconv"
	"strings"

	"infra/internal/config"
	"infra/internal/diag"
	"infra/internal/expressions"
	"infra/pkg/address"
	"infra/pkg/resource"
	"infra/pkg/value"
)

// compileScope resolves variables at compile time but reports every resource
// attribute as unavailable. That is what turns a reference into an unknown
// carrying its expression, and simultaneously what makes the dependency edge
// discoverable. The apply-time scope in M3 resolves attributes too.
type compileScope struct {
	vars map[string]value.Value
}

func (s compileScope) Variable(name string) (value.Value, bool) {
	v, ok := s.vars[name]
	return v, ok
}

func (s compileScope) Attribute(value.Reference) (value.Value, bool) { return value.Value{}, false }

// bindReferences is compiler stage 6. It parses and evaluates every attribute,
// records the dependency edges references imply, and rejects references that
// can never become knowable.
func bindReferences(project *config.ProjectDecl, opts Options) (ResolvedConfig, diag.Diagnostics) {
	var ds diag.Diagnostics

	out := ResolvedConfig{
		Project:     project.Project,
		Environment: opts.Environment,
		Resources:   make(map[string]*resource.ResolvedResource, len(project.Resources)),
	}

	declared := make(map[string]bool, len(project.Resources))
	for _, r := range project.Resources {
		declared[r.Name] = true
	}

	scope := compileScope{vars: variableScope(opts)}

	for _, decl := range project.Resources {
		resolved := &resource.ResolvedResource{
			Address:   address.Address{Name: decl.Name},
			Type:      decl.Type,
			Attrs:     make(map[string]value.Value, len(decl.Attributes)),
			Lifecycle: resource.Lifecycle{PreventDestroy: decl.Lifecycle.PreventDestroy, Retain: decl.Lifecycle.Retain},
			Origin:    decl.Origin,
		}

		edges := map[string]value.Origin{}

		// Sorted, because an edge's Origin is decided by which attribute is
		// visited first and Go randomises map iteration. Without this the same
		// configuration produces a different Origin between runs.
		for _, name := range sortedAttributeNames(decl.Attributes) {
			resolved.Attrs[name] = bindAttribute(decl, attr(decl, name), scope, declared, edges, &ds)
		}

		for _, target := range decl.DependsOn {
			if !declared[target] {
				ds.Add(diag.Diagnostic{
					Severity: diag.SeverityError,
					Summary:  "depends_on names an undeclared resource " + strconv.Quote(target),
					Detail:   "Known resources:\n  " + strings.Join(sortedNames(declared), "\n  "),
					Action:   "Correct the name, or declare " + strconv.Quote(target) + ".",
					Origin:   decl.Origin,
				})
				continue
			}
			if target == decl.Name {
				ds.Add(diag.Diagnostic{
					Severity: diag.SeverityError,
					Summary:  "resource " + strconv.Quote(decl.Name) + " depends on itself",
					Origin:   decl.Origin,
				})
				continue
			}
			recordEdge(edges, target, decl.Origin)
		}

		resolved.DependsOn = sortedAddresses(edges)
		out.Resources[resolved.Address.String()] = resolved
	}

	return out, ds
}

// bindAttribute resolves one attribute, recording any edges its references imply.
func bindAttribute(
	decl *config.ResourceDecl,
	attr config.AttributeDecl,
	scope compileScope,
	declared map[string]bool,
	edges map[string]value.Origin,
	ds *diag.Diagnostics,
) value.Value {
	if !attr.HasExpressions {
		return attr.Value
	}

	src, ok := attr.Value.AsString()
	if !ok {
		// A composite carrying an interpolation is not supported: the language
		// interpolates strings, not structures.
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "interpolation inside a " + attr.Value.Kind.String() + " is not supported",
			Detail:   "Expressions may appear in string values only.",
			Origin:   attr.Origin,
		})
		return attr.Value
	}

	e, parseDiags := expressions.Parse(src, attr.Origin)
	ds.Extend(parseDiags)
	if parseDiags.HasErrors() {
		return value.Unknown(attr.Value.Kind, value.SourceComputed).WithOrigin(attr.Origin)
	}

	for _, ref := range e.References() {
		switch {
		case !declared[ref.Resource]:
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "reference to undeclared resource " + strconv.Quote(ref.Resource),
				Detail: "${" + ref.String() + "} names a resource that does not exist.\nKnown resources:\n  " +
					strings.Join(sortedNames(declared), "\n  "),
				Action: "Correct the reference, or declare " + strconv.Quote(ref.Resource) + ".",
				Origin: attr.Origin,
			})
		case ref.Resource == decl.Name:
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "resource " + strconv.Quote(decl.Name) + " refers to itself",
				Detail:   "${" + ref.String() + "} cannot be resolved: its own value would be required to compute it.",
				Origin:   attr.Origin,
			})
		default:
			recordEdge(edges, ref.Resource, attr.Origin)
		}
	}

	v, evalDiags := expressions.Evaluate(e, scope)
	ds.Extend(evalDiags)
	return v
}

// variableScope builds the compile-time variable scope. In M2 that is only
// --var; variables.yml and environment variables arrive in M4.
func variableScope(opts Options) map[string]value.Value {
	vars := make(map[string]value.Value, len(opts.Vars)+3)
	for k, v := range opts.Vars {
		vars[k] = value.String(v, value.SourceVariable)
	}
	// Always available, so configuration can name its own environment.
	vars["environment"] = value.String(opts.Environment, value.SourceEnvironment)
	if opts.Region != "" {
		vars["region"] = value.String(opts.Region, value.SourceEnvironment)
	}
	if opts.Account != "" {
		vars["account"] = value.String(opts.Account, value.SourceEnvironment)
	}
	return vars
}

// recordEdge keeps the earliest origin for a dependency edge.
//
// Two attributes of one resource may reference the same target, and an
// explicit depends_on may name a target an attribute already references. The
// edge is the same either way, but the Origin a diagnostic points at should be
// the first place the dependency was expressed — not whichever the map
// happened to yield last.
func recordEdge(edges map[string]value.Origin, target string, origin value.Origin) {
	existing, ok := edges[target]
	if !ok || originLess(origin, existing) {
		edges[target] = origin
	}
}

// originLess orders two origins within a file.
//
// It deliberately compares position only. M2 reads a single file, so File is
// always equal; M4 introduces variables.yml and environments, and this must
// gain a File comparison before an edge can span two files — otherwise the
// "earliest" origin would be decided by line number across unrelated files.
func originLess(a, b value.Origin) bool {
	if a.Line != b.Line {
		return a.Line < b.Line
	}
	return a.Column < b.Column
}

func sortedNames(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func sortedAddresses(edges map[string]value.Origin) []address.Address {
	out := make([]address.Address, 0, len(edges))
	for name := range edges {
		out = append(out, address.Address{Name: name})
	}
	address.Sort(out)
	return out
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/compiler/ -v`
Expected: PASS — eleven new tests plus Task 5's seven.

- [ ] **Step 5: Commit**

```bash
git add internal/compiler/bind.go internal/compiler/bind_test.go
git commit -m "feat: bind references and derive dependency edges"
```

---

## Task 7: Stage 7 — schema binding and defaults

**Files:**
- Create: `internal/compiler/schema.go`
- Test: `internal/compiler/schema_test.go`

**Interfaces:**
- Consumes: `registry.Registry`, `schema.ResourceDefinition`, `schema.Attribute`, `schema.DefaultContext`; `ResolvedConfig`.
- Produces: `compiler.bindSchemas(cfg *ResolvedConfig, reg *registry.Registry, opts Options) diag.Diagnostics` (unexported).

Stage 7 is where a resolved value meets the schema that governs it: unknown types and attributes are rejected, kinds are checked, computed attributes are refused to configuration, defaults are filled in, and sensitive attributes are marked.

Four rules that are decisions, not mechanics:

**Kind checking skips unknown values.** An unknown carries the kind it *will* have, and checking that is right; but an unknown produced by a failed parse carries a placeholder kind, so mismatches on unknowns are not reported twice.

**Defaults are marked `SourceDefault` and never overwrite an explicit value.** This is what lets a plan print `[default]` and what lets import generate minimal configuration later. A default that silently masquerades as explicit destroys both.

**Default resolvers see only `DefaultContext`** — environment, environment type, region, account, project, type. Never another resource's attributes, so a default can never depend on an unknown.

**Sensitivity comes from the schema and is added to, never replacing, sensitivity the value already carries.** A value that arrived sensitive from a secret stays sensitive even in a non-sensitive attribute.

- [ ] **Step 1: Write the failing test**

Create `internal/compiler/schema_test.go`:

```go
package compiler

import (
	"strings"
	"testing"

	"infra/internal/registry"
	"infra/pkg/address"
	"infra/pkg/resource"
	"infra/pkg/value"
	testprovider "infra/providers/test"
)

func testRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	reg := registry.New()
	if err := reg.Register(testprovider.New(t.TempDir() + "/fake-cloud.json")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return reg
}

func oneResource(typ string, attrs map[string]value.Value) *ResolvedConfig {
	return &ResolvedConfig{
		Project:     "myapp",
		Environment: "dev",
		Resources: map[string]*resource.ResolvedResource{
			"r": {Address: address.Address{Name: "r"}, Type: typ, Attrs: attrs},
		},
	}
}

func TestSchemaRejectsUnknownType(t *testing.T) {
	cfg := oneResource("aws.rds", nil)
	ds := bindSchemas(cfg, testRegistry(t), Options{Environment: "dev"})
	if !ds.HasErrors() {
		t.Fatal("an unregistered type must be an error")
	}
	var out strings.Builder
	ds.Render(&out)
	if !strings.Contains(out.String(), "test.database") {
		t.Errorf("the diagnostic should list known types:\n%s", out.String())
	}
}

func TestSchemaRejectsUnknownAttribute(t *testing.T) {
	cfg := oneResource("test.network", map[string]value.Value{
		"cidr":     value.String("10.0.0.0/16", value.SourceExplicit),
		"nonsense": value.Bool(true, value.SourceExplicit),
	})
	if ds := bindSchemas(cfg, testRegistry(t), Options{Environment: "dev"}); !ds.HasErrors() {
		t.Error("an attribute the schema does not define must be an error")
	}
}

func TestSchemaRejectsSettingAComputedAttribute(t *testing.T) {
	cfg := oneResource("test.network", map[string]value.Value{
		"cidr": value.String("10.0.0.0/16", value.SourceExplicit),
		"id":   value.String("net-1", value.SourceExplicit),
	})
	if ds := bindSchemas(cfg, testRegistry(t), Options{Environment: "dev"}); !ds.HasErrors() {
		t.Error("configuration must not set a computed attribute")
	}
}

func TestSchemaRejectsAMissingRequiredAttribute(t *testing.T) {
	cfg := oneResource("test.network", nil) // cidr is required
	if ds := bindSchemas(cfg, testRegistry(t), Options{Environment: "dev"}); !ds.HasErrors() {
		t.Error("a missing required attribute must be an error")
	}
}

func TestSchemaRejectsAWrongKind(t *testing.T) {
	cfg := oneResource("test.database", map[string]value.Value{
		"engine": value.String("postgres", value.SourceExplicit),
		"size":   value.String("large", value.SourceExplicit), // size is an integer
	})
	ds := bindSchemas(cfg, testRegistry(t), Options{Environment: "dev"})
	if !ds.HasErrors() {
		t.Fatal("a string where an integer is required must be an error")
	}
	var out strings.Builder
	ds.Render(&out)
	if !strings.Contains(out.String(), "integer") {
		t.Errorf("the diagnostic should name the expected kind:\n%s", out.String())
	}
}

func TestSchemaSkipsKindCheckOnUnknowns(t *testing.T) {
	// An unknown carries the kind it will have; a mismatch there is not a
	// user error and reporting it would be noise on every reference.
	cfg := oneResource("test.database", map[string]value.Value{
		"engine": value.String("postgres", value.SourceExplicit),
		"size":   value.Unknown(value.KindString, value.SourceComputed),
	})
	if ds := bindSchemas(cfg, testRegistry(t), Options{Environment: "dev"}); ds.HasErrors() {
		t.Errorf("an unknown must not trip kind checking: %+v", ds)
	}
}

func TestSchemaFillsDefaults(t *testing.T) {
	cfg := oneResource("test.database", map[string]value.Value{
		"engine": value.String("postgres", value.SourceExplicit),
	})
	if ds := bindSchemas(cfg, testRegistry(t), Options{Environment: "dev"}); ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	size, ok := cfg.Resources["r"].Attrs["size"]
	if !ok {
		t.Fatal("the default for size was not filled in")
	}
	if size.Source != value.SourceDefault {
		t.Errorf("Source = %v, want SourceDefault — a plan must be able to print [default]", size.Source)
	}
	if n, _ := size.AsInt(); n != 10 {
		t.Errorf("size = %d, want the dev default of 10", n)
	}
}

func TestSchemaDefaultsAreEnvironmentAware(t *testing.T) {
	cfg := oneResource("test.database", map[string]value.Value{
		"engine": value.String("postgres", value.SourceExplicit),
	})
	bindSchemas(cfg, testRegistry(t), Options{Environment: "production"})

	// The fake provider's size default is 100 when EnvironmentType is
	// production, 10 otherwise.
	if n, _ := cfg.Resources["r"].Attrs["size"].AsInt(); n != 100 {
		t.Errorf("size = %d in production, want 100 — defaults may vary by environment", n)
	}
}

func TestSchemaDefaultNeverOverwritesAnExplicitValue(t *testing.T) {
	cfg := oneResource("test.database", map[string]value.Value{
		"engine": value.String("postgres", value.SourceExplicit),
		"size":   value.Int(50, value.SourceExplicit),
	})
	bindSchemas(cfg, testRegistry(t), Options{Environment: "dev"})

	size := cfg.Resources["r"].Attrs["size"]
	if n, _ := size.AsInt(); n != 50 {
		t.Errorf("size = %d, want the explicit 50", n)
	}
	if size.Source != value.SourceExplicit {
		t.Errorf("Source = %v, want SourceExplicit — an explicit value always wins", size.Source)
	}
}

func TestSchemaMarksSensitiveAttributes(t *testing.T) {
	cfg := oneResource("test.database", map[string]value.Value{
		"engine":   value.String("postgres", value.SourceExplicit),
		"password": value.String("hunter2", value.SourceExplicit),
	})
	bindSchemas(cfg, testRegistry(t), Options{Environment: "dev"})

	if !cfg.Resources["r"].Attrs["password"].Sensitive {
		t.Error("an attribute the schema marks Sensitive must come out sensitive")
	}
}

func TestSchemaDoesNotDeclassifyAnAlreadySensitiveValue(t *testing.T) {
	cfg := oneResource("test.database", map[string]value.Value{
		"engine": value.String("postgres", value.SourceExplicit).WithSensitive(true),
	})
	bindSchemas(cfg, testRegistry(t), Options{Environment: "dev"})

	if !cfg.Resources["r"].Attrs["engine"].Sensitive {
		t.Error("schema binding must add sensitivity, never clear it — engine is not a sensitive attribute but this value arrived classified")
	}
}

func TestSchemaReportsEveryProblemAtOnce(t *testing.T) {
	cfg := oneResource("test.network", map[string]value.Value{
		"nonsense_one": value.Bool(true, value.SourceExplicit),
		"nonsense_two": value.Bool(true, value.SourceExplicit),
	})
	ds := bindSchemas(cfg, testRegistry(t), Options{Environment: "dev"})
	if len(ds) < 3 {
		t.Errorf("got %d diagnostics, want at least 3 (two unknown attributes and one missing required)", len(ds))
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/compiler/ -run TestSchema -v`
Expected: FAIL — `undefined: bindSchemas`.

- [ ] **Step 3: Implement stage 7**

Create `internal/compiler/schema.go`:

```go
package compiler

import (
	"sort"
	"strconv"
	"strings"

	"infra/internal/diag"
	"infra/internal/registry"
	"infra/pkg/schema"
	"infra/pkg/value"
)

// bindSchemas is compiler stage 7. It resolves each resource's type to its
// definition, validates what configuration set, and fills in defaults.
func bindSchemas(cfg *ResolvedConfig, reg *registry.Registry, opts Options) diag.Diagnostics {
	var ds diag.Diagnostics

	for _, addr := range cfg.Addresses() {
		r := cfg.Resources[addr.String()]

		def, ok := reg.Definition(r.Type)
		if !ok {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "unknown resource type " + strconv.Quote(r.Type),
				Detail:   "Known types:\n  " + strings.Join(reg.Types(), "\n  "),
				Action:   "Correct the type, or check that the provider offering it is available.",
				Origin:   r.Origin,
			})
			continue
		}

		checkConfiguredAttributes(r.Attrs, def, r.Origin, &ds)
		applyDefaults(r.Attrs, def, defaultContextFor(cfg, r.Type, opts))
		checkRequired(r.Attrs, def, r.Origin, &ds)
		markSensitive(r.Attrs, def)
	}

	return ds
}

// checkConfiguredAttributes rejects what configuration must not set.
func checkConfiguredAttributes(attrs map[string]value.Value, def *schema.ResourceDefinition, origin value.Origin, ds *diag.Diagnostics) {
	names := make([]string, 0, len(attrs))
	for name := range attrs {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		v := attrs[name]

		attr, known := def.Attribute(name)
		if !known {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  def.Type + " has no attribute " + strconv.Quote(name),
				Detail:   "Attributes of " + def.Type + ":\n  " + strings.Join(attributeNames(def), "\n  "),
				Action:   "Remove the attribute, or correct its name.",
				Origin:   v.Origin,
			})
			continue
		}

		if attr.Computed {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  strconv.Quote(name) + " is computed and cannot be set",
				Detail:   "The provider determines this value. It can be referenced by other resources, but not configured.",
				Origin:   v.Origin,
			})
			continue
		}

		// An unknown carries the kind it will have, but an unknown produced by
		// a failed parse carries a placeholder. Checking it would report the
		// same problem twice, so kind checking applies to known values only.
		if v.Known && v.Kind != attr.Kind {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  strconv.Quote(name) + " must be " + attr.Kind.String() + ", got " + v.Kind.String(),
				Origin:   v.Origin,
			})
			continue
		}

		if attr.Validate != nil && v.Known {
			if err := attr.Validate(v); err != nil {
				ds.Add(diag.Diagnostic{
					Severity: diag.SeverityError,
					Summary:  strconv.Quote(name) + " is not valid: " + err.Error(),
					Origin:   v.Origin,
				})
			}
		}
	}
}

// applyDefaults fills absent optional attributes, marking each SourceDefault.
// It never overwrites a value configuration supplied: an explicit value always
// wins over an implicit one.
func applyDefaults(attrs map[string]value.Value, def *schema.ResourceDefinition, ctx schema.DefaultContext) {
	names := make([]string, 0, len(def.Attributes))
	for name := range def.Attributes {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		attr := def.Attributes[name]
		if attr.Default == nil || attr.Computed {
			continue
		}
		if _, present := attrs[name]; present {
			continue
		}
		raw, ok := attr.Default(ctx)
		if !ok {
			continue
		}
		v, ok := checkedDefault(raw, attr.Kind)
		if !ok {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  def.Type + ": the default for " + strconv.Quote(name) + " is not a " + attr.Kind.String(),
				Detail:   "A provider default must produce the kind its attribute declares. This is a provider bug, not a configuration error.",
				Origin:   origin,
			})
			continue
		}
		attrs[name] = v
	}
}

// fromDefault wraps a resolver's datum as a value marked SourceDefault, which
// is what lets a plan print [default] and import generate minimal config.
func fromDefault(raw any, kind value.Kind) (value.Value, bool) {
	switch v := raw.(type) {
	case string:
		return value.String(v, value.SourceDefault), true
	case int64:
		return value.Int(v, value.SourceDefault), true
	case int:
		return value.Int(int64(v), value.SourceDefault), true
	case float64:
		return value.Float(v, value.SourceDefault), true
	case bool:
		return value.Bool(v, value.SourceDefault), true
	case []value.Value:
		return value.List(v, value.SourceDefault), true
	case map[string]value.Value:
		return value.Map(v, value.SourceDefault), true
	default:
		// A resolver returning a type the value model cannot express is a
		// provider bug. It must NOT become an unknown: an unknown never
		// resolves, so the planner could not prove it unchanged and would
		// report a change on every plan, forever. Report it instead.
		return value.Value{}, false
	}
}

// checkedDefault converts a resolver's datum and confirms it produced the kind
// the attribute declares.
//
// Matching a Go type is not the same as matching the declared kind: a resolver
// for a float attribute that returns int64 builds a perfectly valid KindInt
// value, which would then sail past the kind check that exists to catch exactly
// this. The declared kind is the contract; the Go type is only how it happens
// to arrive.
func checkedDefault(raw any, kind value.Kind) (value.Value, bool) {
	v, ok := fromDefault(raw, kind)
	if !ok || v.Kind != kind {
		return value.Value{}, false
	}
	return v, true
}

func checkRequired(attrs map[string]value.Value, def *schema.ResourceDefinition, origin value.Origin, ds *diag.Diagnostics) {
	for _, name := range def.RequiredAttributes() {
		if _, ok := attrs[name]; !ok {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  def.Type + " requires " + strconv.Quote(name),
				Detail:   def.Attributes[name].Description,
				Action:   "Set " + name + " on this resource.",
				Origin:   origin,
			})
		}
	}
}

// markSensitive adds schema-declared sensitivity. It never clears sensitivity a
// value already carries: a secret interpolated into an ordinary attribute stays
// classified.
func markSensitive(attrs map[string]value.Value, def *schema.ResourceDefinition) {
	for name, v := range attrs {
		if attr, ok := def.Attribute(name); ok && attr.Sensitive {
			attrs[name] = v.WithSensitive(true)
		}
	}
}

func defaultContextFor(cfg *ResolvedConfig, resourceType string, opts Options) schema.DefaultContext {
	return schema.DefaultContext{
		// Options is the authoritative environment for this compilation.
		// cfg.Environment is a copy stage 6 wrote from the same source, and
		// reading the copy invites the two to disagree.
		Environment:     opts.Environment,
		EnvironmentType: environmentType(opts.Environment),
		Region:          opts.Region,
		Account:         opts.Account,
		Project:         cfg.Project,
		Type:            resourceType,
	}
}

// environmentType classifies an environment for default resolution. Explicit
// declaration via `environment: type:` arrives with the environment system in
// M4; until then the name is the only signal available.
func environmentType(name string) string {
	if name == "production" || name == "prod" {
		return "production"
	}
	return name
}

func attributeNames(def *schema.ResourceDefinition) []string {
	out := make([]string, 0, len(def.Attributes))
	for name := range def.Attributes {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/compiler/ -v`
Expected: PASS — twelve new tests plus the twenty-three already in the package.

- [ ] **Step 5: Commit**

```bash
git add internal/compiler/schema.go internal/compiler/schema_test.go
git commit -m "feat: bind schemas, check kinds, and resolve defaults"
```

---
## Task 8: Stage 8 — whole-graph validation

**Files:**
- Create: `internal/compiler/validate.go`
- Test: `internal/compiler/validate_test.go`

**Interfaces:**
- Consumes: `ResolvedConfig`, `resource.ResolvedResource`, `resource.Lifecycle`, `address.Address`, `registry.Registry`, `schema.ResourceDefinition`, `schema.Requirement` (all already shipped or Task 5–7 output); `diag.Diagnostics`.
- Produces: `compiler.validateGraph(cfg *ResolvedConfig, reg *registry.Registry) diag.Diagnostics` (unexported, called by `Compile` in Task 9).

Stage 8 runs once schema binding has already confirmed every resource's type is registered, its kinds check out and its required attributes are present. What is left is what only the *whole* graph can reveal: whether following every `DependsOn` edge ever leads back to where it started, whether a resource declares infrastructure it needs but configuration never supplies, and whether a resource's own lifecycle settings contradict each other.

Cycle detection is one depth-first walk over `DependsOn`, using three colors (unvisited, on the current path, fully explored) rather than a simple visited set — a plain visited set would still report *that* there was a cycle, but not *which* nodes are actually on it versus merely reachable from it. When the walk finds an edge back to a node still on the path, the cycle is the path from that node to the top of the stack, closed by repeating the first address. The same loop can be discovered starting from any of its members, so found cycles are canonicalized — rotated to start at their lexicographically smallest address — and deduplicated before being reported once each, in sorted order, for a deterministic diagnostic list. A cycle of one, a resource referencing itself, is already rejected in stage 6 when the reference is bound; nothing here special-cases it, though the same algorithm would still catch one if it ever reached this stage some other way.

Requirement satisfaction is a global existence check, not a traced reference: a `Requirement` is satisfied when the resolved configuration contains *any* resource whose type appears in the requirement's `Types`, independent of whether the resource declaring the requirement actually points at it. This is deliberate, not a shortcut — `schema.Requirement` carries no attribute name to trace (the ECS example in `PLAN.md` §17 lists five requirements against a handful of attributes, with no declared mapping between them), so there is no principled way to demand a specific reference. The spec text this task was built from (design spec §8.1) also allows a requirement to be satisfied by "an existing resource... in state," but `validateGraph` has no `state` parameter by contract, and neither does `Compile` in Task 9 — so that half of satisfaction is unreachable at compile time in M2. State-aware satisfaction (a database already running in a prior apply, not re-declared this time) has to be a planner-time concern once `state.State` is available; flagging this gap for the plan assembler rather than silently narrowing the spec. An `Optional` requirement produces no diagnostic when unsatisfied, in either direction — M2 does not warn about it, on the reading that a warning is a plan-render concern (letting a user see what's missing without being told to fix it) and not a compiler error. This runs **before any provider call**, which is the headline behavior `PLAN.md` §17 asks for: "an application needs a database," not an opaque failure once the provider is finally asked to create it.

Lifecycle validity is the smallest of the three checks: `prevent_destroy` refuses to destroy a resource, `retain` abandons it from state without destroying it, and a resource cannot mean both at once. This is a plain conjunction over already-resolved `Lifecycle` values — no graph traversal involved — checked per resource alongside the requirement check.

- [ ] **Step 1: Write the failing test**

Create `internal/compiler/validate_test.go`:

```go
package compiler

import (
	"context"
	"strings"
	"testing"

	"infra/internal/registry"
	"infra/pkg/address"
	"infra/pkg/provider"
	"infra/pkg/resource"
	"infra/pkg/schema"
	"infra/pkg/value"
)

func TestValidateGraphDetectsCycle(t *testing.T) {
	alpha := res("alpha", "test.network", map[string]value.Value{"cidr": value.String("10.0.0.0/16", value.SourceExplicit)})
	bravo := res("bravo", "test.network", map[string]value.Value{"cidr": value.String("10.0.1.0/16", value.SourceExplicit)})
	charlie := res("charlie", "test.network", map[string]value.Value{"cidr": value.String("10.0.2.0/16", value.SourceExplicit)})
	alpha.DependsOn = []address.Address{{Name: "bravo"}}
	bravo.DependsOn = []address.Address{{Name: "charlie"}}
	charlie.DependsOn = []address.Address{{Name: "alpha"}}

	graph := cfg(alpha, bravo, charlie)
	ds := validateGraph(&graph, testRegistry(t))
	if !ds.HasErrors() {
		t.Fatal("a dependency cycle must be an error")
	}

	var out strings.Builder
	ds.Render(&out)
	for _, name := range []string{"alpha", "bravo", "charlie"} {
		if !strings.Contains(out.String(), name) {
			t.Errorf("cycle diagnostic must name every member of the cycle, missing %q:\n%s", name, out.String())
		}
	}
}

func TestValidateGraphAcceptsAcyclicGraph(t *testing.T) {
	a := res("a", "test.network", map[string]value.Value{"cidr": value.String("10.0.0.0/16", value.SourceExplicit)})
	b := res("b", "test.network", map[string]value.Value{"cidr": value.String("10.0.1.0/16", value.SourceExplicit)})
	c := res("c", "test.network", map[string]value.Value{"cidr": value.String("10.0.2.0/16", value.SourceExplicit)})
	d := res("d", "test.network", map[string]value.Value{"cidr": value.String("10.0.3.0/16", value.SourceExplicit)})
	b.DependsOn = []address.Address{{Name: "a"}}
	c.DependsOn = []address.Address{{Name: "a"}}
	d.DependsOn = []address.Address{{Name: "b"}, {Name: "c"}}

	graph := cfg(a, b, c, d)
	if ds := validateGraph(&graph, testRegistry(t)); ds.HasErrors() {
		t.Errorf("a diamond dependency shape is not a cycle: %+v", ds)
	}
}

func TestValidateGraphReportsMissingRequiredInfrastructure(t *testing.T) {
	graph := oneResource("test.database", map[string]value.Value{
		"engine": value.String("postgres", value.SourceExplicit),
	})
	ds := validateGraph(graph, testRegistry(t))
	if !ds.HasErrors() {
		t.Fatal("a database with no network anywhere in configuration must be an error before any provider call")
	}

	var out strings.Builder
	ds.Render(&out)
	if !strings.Contains(out.String(), "network") {
		t.Errorf("diagnostic must name the missing requirement:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "test.network") {
		t.Errorf("diagnostic must name what would satisfy it:\n%s", out.String())
	}
}

func TestValidateGraphAcceptsSatisfiedRequirement(t *testing.T) {
	network := res("network", "test.network", map[string]value.Value{"cidr": value.String("10.0.0.0/16", value.SourceExplicit)})
	database := res("database", "test.database", map[string]value.Value{"engine": value.String("postgres", value.SourceExplicit)})

	graph := cfg(network, database)
	if ds := validateGraph(&graph, testRegistry(t)); ds.HasErrors() {
		t.Errorf("a network exists in configuration, so the requirement is satisfied: %+v", ds)
	}
}

// stubProvider satisfies provider.Provider with exactly what this file needs:
// one resource type whose requirement is Optional, to prove stage 8 only
// errors on requirements that are not.
type stubProvider struct{}

func (stubProvider) Name() string { return "stub" }
func (stubProvider) Definitions() []*schema.ResourceDefinition {
	return []*schema.ResourceDefinition{{
		Type: "stub.thing",
		Attributes: map[string]schema.Attribute{
			"name": {Kind: value.KindString, Required: true},
		},
		Requirements: []schema.Requirement{{
			Name:        "cache",
			Types:       []string{"stub.cache"},
			Optional:    true,
			Description: "A cache speeds up stub.thing but is not required.",
		}},
		Capabilities: schema.Capabilities{Create: true, Read: true, Update: true, Delete: true},
	}}
}
func (stubProvider) ClassifyError(error) provider.Retryability { return provider.NotSafeToRetry }
func (stubProvider) Read(context.Context, *resource.ResourceState) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (stubProvider) Create(context.Context, *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (stubProvider) Update(context.Context, *resource.ResourceState, *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (stubProvider) Delete(context.Context, *resource.ResourceState) error {
	return provider.ErrNotImplemented
}
func (stubProvider) Discover(context.Context, provider.DiscoverRequest) ([]provider.DiscoveredResource, error) {
	return nil, provider.ErrNotImplemented
}
func (stubProvider) Import(context.Context, string, string) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}

func TestValidateGraphSkipsOptionalRequirement(t *testing.T) {
	reg := registry.New()
	if err := reg.Register(stubProvider{}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	thing := res("thing", "stub.thing", map[string]value.Value{"name": value.String("widget", value.SourceExplicit)})
	graph := cfg(thing)

	if ds := validateGraph(&graph, reg); ds.HasErrors() {
		t.Errorf("an unsatisfied optional requirement must not be an error: %+v", ds)
	}
}

func TestValidateGraphRejectsPreventDestroyAndRetainTogether(t *testing.T) {
	guarded := res("guarded", "test.network", map[string]value.Value{"cidr": value.String("10.0.0.0/16", value.SourceExplicit)})
	guarded.Lifecycle = resource.Lifecycle{PreventDestroy: true, Retain: true}

	graph := cfg(guarded)
	ds := validateGraph(&graph, testRegistry(t))
	if !ds.HasErrors() {
		t.Fatal("prevent_destroy and retain together are contradictory and must be an error")
	}

	var out strings.Builder
	ds.Render(&out)
	if !strings.Contains(out.String(), "prevent_destroy") || !strings.Contains(out.String(), "retain") {
		t.Errorf("diagnostic must name both contradictory settings:\n%s", out.String())
	}
}

func TestValidateGraphAcceptsLifecycleFlagsIndividually(t *testing.T) {
	cases := []struct {
		name      string
		lifecycle resource.Lifecycle
	}{
		{"neither set", resource.Lifecycle{}},
		{"prevent_destroy alone", resource.Lifecycle{PreventDestroy: true}},
		{"retain alone", resource.Lifecycle{Retain: true}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := res("net", "test.network", map[string]value.Value{"cidr": value.String("10.0.0.0/16", value.SourceExplicit)})
			r.Lifecycle = tc.lifecycle

			graph := cfg(r)
			if ds := validateGraph(&graph, testRegistry(t)); ds.HasErrors() {
				t.Errorf("%s must not be a lifecycle error: %+v", tc.name, ds)
			}
		})
	}
}

func TestValidateGraphReportsEveryProblemAtOnce(t *testing.T) {
	// A 2-cycle of resources that each also lack their own required network,
	// alongside an unrelated resource with a lifecycle contradiction: one
	// category of problem must not mask the others.
	//
	// guarded is deliberately test.application, not test.network: its own
	// requirement (database) is satisfied by x and y, so it contributes no
	// requirement diagnostic of its own — but it also must not accidentally
	// satisfy x and y's network requirement, which a test.network resource
	// here would do regardless of any edge connecting it to them.
	x := res("x", "test.database", nil)
	y := res("y", "test.database", nil)
	x.DependsOn = []address.Address{{Name: "y"}}
	y.DependsOn = []address.Address{{Name: "x"}}

	guarded := res("guarded", "test.application", nil)
	guarded.Lifecycle = resource.Lifecycle{PreventDestroy: true, Retain: true}

	graph := cfg(x, y, guarded)
	ds := validateGraph(&graph, testRegistry(t))
	if len(ds) < 4 {
		t.Errorf("got %d diagnostics, want at least 4 (one cycle, two missing requirements, one lifecycle contradiction): %+v", len(ds), ds)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/compiler/ -run TestValidateGraph -v`
Expected: FAIL — `undefined: validateGraph`.

- [ ] **Step 3: Implement stage 8**

Create `internal/compiler/validate.go`:

```go
package compiler

import (
	"sort"
	"strconv"
	"strings"

	"infra/internal/diag"
	"infra/internal/registry"
	"infra/pkg/address"
	"infra/pkg/value"
)

// validateGraph is compiler stage 8. It runs once schema binding has
// confirmed every resource's type, kinds and required attributes, and checks
// what only the whole graph can reveal: dependency cycles, infrastructure a
// resource needs but configuration never supplies, and lifecycle settings
// that contradict each other. None of these checks contact a provider.
func validateGraph(cfg *ResolvedConfig, reg *registry.Registry) diag.Diagnostics {
	var ds diag.Diagnostics

	for _, cycle := range findCycles(cfg) {
		ds.Add(cycleDiagnostic(cfg, cycle))
	}

	checkRequirements(cfg, reg, &ds)
	checkLifecycle(cfg, &ds)

	return ds
}

// --- dependency cycles ------------------------------------------------

// color tracks a node's state during the depth-first walk: not yet visited,
// on the current path, or fully explored. A plain visited set would say a
// cycle exists but not which nodes are actually on it versus merely
// reachable from it; the third state is what lets the walk tell the two
// apart.
type color int

const (
	white color = iota
	gray
	black
)

// findCycles walks the dependency graph and returns every distinct cycle,
// each in order and closed (a → b → c → a), deduplicated and sorted for a
// deterministic result. A cycle of one — a resource referring to itself — is
// already rejected when stage 6 binds the reference; nothing here special-
// cases that, though the same walk would still catch one if it ever reached
// this stage some other way.
func findCycles(cfg *ResolvedConfig) [][]address.Address {
	colors := map[string]color{}
	onStack := map[string]int{}
	var stack []address.Address
	var found [][]address.Address

	var visit func(addr address.Address)
	visit = func(addr address.Address) {
		key := addr.String()
		colors[key] = gray
		onStack[key] = len(stack)
		stack = append(stack, addr)

		if r, ok := cfg.Get(addr); ok {
			for _, dep := range r.DependsOn {
				dk := dep.String()
				switch colors[dk] {
				case white:
					visit(dep)
				case gray:
					// A back edge to a node still on the path: the cycle is
					// everything from that node to here, closed by repeating it.
					start := onStack[dk]
					cycle := append([]address.Address{}, stack[start:]...)
					cycle = append(cycle, dep)
					found = append(found, cycle)
				}
			}
		}

		stack = stack[:len(stack)-1]
		delete(onStack, key)
		colors[key] = black
	}

	for _, addr := range cfg.Addresses() {
		if colors[addr.String()] == white {
			visit(addr)
		}
	}

	return dedupeCycles(found)
}

// dedupeCycles collapses cycles discovered more than once — the same loop is
// found again starting from any of its members — by rotating each to start
// at its lexicographically smallest address, then removing repeats. The
// result is sorted so the diagnostic list is stable across runs.
func dedupeCycles(cycles [][]address.Address) [][]address.Address {
	seen := map[string]bool{}
	var out [][]address.Address

	for _, c := range cycles {
		body := c[:len(c)-1] // drop the closing repeat of the first element
		minIdx := 0
		for i, a := range body {
			if a.String() < body[minIdx].String() {
				minIdx = i
			}
		}
		rotated := append(append([]address.Address{}, body[minIdx:]...), body[:minIdx]...)
		rotated = append(rotated, rotated[0])

		key := cycleKey(rotated)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, rotated)
	}

	sort.Slice(out, func(i, j int) bool { return cycleKey(out[i]) < cycleKey(out[j]) })
	return out
}

// cycleKey renders a closed cycle as "a → b → c → a", used both as the
// diagnostic text and as the deduplication key.
func cycleKey(cycle []address.Address) string {
	parts := make([]string, len(cycle))
	for i, a := range cycle {
		parts[i] = a.String()
	}
	return strings.Join(parts, " → ")
}

func cycleDiagnostic(cfg *ResolvedConfig, cycle []address.Address) diag.Diagnostic {
	origin := value.Origin{}
	if r, ok := cfg.Get(cycle[0]); ok {
		origin = r.Origin
	}
	return diag.Diagnostic{
		Severity: diag.SeverityError,
		Summary:  "dependency cycle: " + cycleKey(cycle),
		Detail:   "Each resource in the cycle depends, directly or indirectly, on itself through the others, so none of them could ever be created first.",
		Action:   "Break the cycle by removing one dependency, or restructuring the resources so it is not required.",
		Origin:   origin,
		Related:  cycle[1 : len(cycle)-1],
	}
}

// --- requirements -------------------------------------------------------

// checkRequirements enforces the Requirement mechanism (spec §8.1): a
// non-optional requirement unsatisfied by anything in configuration is an
// error, reported before any provider is ever called — "an application
// needs a database," not an opaque failure once a provider API is finally
// asked to create it (PLAN.md §17).
//
// Satisfaction here is existence across the whole resolved configuration, not
// a traced reference from the specific resource that declares the
// requirement: Requirement carries no attribute name to trace against, so
// there is no principled way to demand a specific edge. The spec also allows
// a requirement to be satisfied by an existing resource already in state, but
// validateGraph has no state to consult — that half of satisfaction belongs
// to whichever stage has state, not to compilation.
func checkRequirements(cfg *ResolvedConfig, reg *registry.Registry, ds *diag.Diagnostics) {
	present := map[string]bool{}
	for _, r := range cfg.Resources {
		present[r.Type] = true
	}

	for _, addr := range cfg.Addresses() {
		r := cfg.Resources[addr.String()]

		def, ok := reg.Definition(r.Type)
		if !ok {
			// An unresolved type is stage 7's problem. Compile does not reach
			// this stage until stage 7 is clean; a direct caller of
			// validateGraph passing an unresolved type gets no further noise.
			continue
		}

		for _, req := range def.Requirements {
			if req.Optional || satisfiesAny(present, req.Types) {
				continue
			}
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  strconv.Quote(addr.String()) + " is missing required " + req.Name,
				Detail:   req.Description + "\nSatisfied by a resource of type: " + strings.Join(req.Types, ", "),
				Action:   "Add a resource of type " + strings.Join(req.Types, " or ") + " to this configuration.",
				Origin:   r.Origin,
			})
		}
	}
}

func satisfiesAny(present map[string]bool, types []string) bool {
	for _, t := range types {
		if present[t] {
			return true
		}
	}
	return false
}

// --- lifecycle ------------------------------------------------------------

// checkLifecycle rejects lifecycle settings that contradict each other.
// prevent_destroy refuses to destroy the resource; retain abandons it from
// state without destroying it — a resource cannot mean both at once.
func checkLifecycle(cfg *ResolvedConfig, ds *diag.Diagnostics) {
	for _, addr := range cfg.Addresses() {
		r := cfg.Resources[addr.String()]
		if r.Lifecycle.PreventDestroy && r.Lifecycle.Retain {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  strconv.Quote(addr.String()) + " sets both prevent_destroy and retain",
				Detail:   "prevent_destroy refuses to destroy the resource; retain abandons it from state without destroying it. Setting both is a contradiction.",
				Action:   "Choose one: prevent_destroy to keep infra managing it and refuse destruction, or retain to let infra forget it without deleting it.",
				Origin:   r.Origin,
			})
		}
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/compiler/ -v`
Expected: PASS — eight new tests plus the previous thirty from Tasks 5–7.

- [ ] **Step 5: Commit**

```bash
git add internal/compiler/validate.go internal/compiler/validate_test.go
git commit -m "$(cat <<'EOF'
feat: validate the resolved graph for cycles, missing requirements and lifecycle conflicts

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_015M5QdCFKa6booSVrhnAzTV
EOF
)"
```

---

## Task 9: Compile orchestration

**Files:**
- Create: `internal/compiler/compile.go`
- Modify: `internal/cli/validate.go`
- Test: `internal/compiler/compile_test.go`, `internal/cli/validate_test.go`

**Interfaces:**
- Consumes: `config.File`, `config.Decode`; `compiler.bindReferences`, `compiler.bindSchemas`, `compiler.validateGraph` (Tasks 6–8); `registry.Registry`; `diag.Diagnostics`.
- Produces: `compiler.Compile(files []config.File, reg *registry.Registry, opts Options) (ResolvedConfig, diag.Diagnostics)`; a rewritten `cli.validateProject(dir string, reg *registry.Registry) diag.Diagnostics` that delegates to it.

`Compile` is thin: it runs decode, then the three compiler-stage functions already built, in the order the spec fixes, accumulating diagnostics. The one substantive decision is where to stop.

**A later stage runs only once every prior stage is free of errors.** Diagnostics already collect *within* a stage — Tasks 6–8 each report every problem a single pass finds, not just the first. That rule does not extend *across* stages: `Compile` checks `HasErrors()` after decode, after `bindReferences`, and after `bindSchemas`, and returns immediately the first time it is true. The reasoning is the same at each boundary. A resource whose type never resolved has no `ResourceDefinition` for stage 8 to check requirements against — the assignment's own example, "a stage-7 kind check against a resource whose type was never resolved produces noise, not signal," is exactly the shape of the problem one boundary further up: reasoning about a graph that an earlier stage could not finish building surfaces secondary diagnostics that do not help fix the root cause and can actively bury it. A resource with a duplicate name lost to decode, or a reference to a resource nobody declared, leaves `ResolvedConfig` structurally incomplete in a way no later stage is positioned to reason about correctly. Stopping is not fail-fast in the sense Tasks 6–8 were built to avoid — every diagnostic a *completed* stage found is still returned together — it is a refusal to hand an unfinished config to a stage that assumes a finished one.

Concretely: `checkRequirements` in Task 8 needs `reg.Definition(r.Type)` to succeed to know what a resource requires; if stage 7 already reported that type as unresolved, running stage 8 over the same resource anyway would either skip it silently (masking that stage 8 never got a chance to check it) or, worse, report a second and unrelated diagnostic about the same broken resource. Stopping at the first stage boundary with errors avoids the question entirely — cleanly, and without any resource-by-resource bookkeeping to decide which stage 8 checks are still meaningful for which resources.

`internal/cli/validate.go`'s `validateProject` is rewritten to call `Compile` directly instead of the three checks M1 shipped inline (type registered, attribute exists, attribute not computed — a strict subset of what stage 7 alone now does, before stage 8 even runs). `infra validate` has no `<environment>` argument — that arrives with `infra plan` in Task 15 — so it calls `Compile` with the environment left as the empty string. This is harmless: `${environment}` is always a defined variable regardless of its value (Task 6), and an empty `EnvironmentType` only changes *which* environment-varying default gets filled in, never whether the configuration is valid. `--var` is not threaded through in this task: `GlobalOptions.Vars` has existed as a parsed flag since M1 but nothing has ever consumed it, and wiring it into `compiler.Options.Vars` is variable-system work that belongs with M4, not with this rewiring.

- [ ] **Step 1: Write the failing test**

Create `internal/compiler/compile_test.go`:

```go
package compiler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"infra/internal/config"
	"infra/pkg/address"
	"infra/pkg/value"
)

func loadFiles(t *testing.T, body string) []config.File {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "infra.yml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	files, err := config.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return files
}

func TestCompileRunsTheFullPipelineOnAValidProject(t *testing.T) {
	files := loadFiles(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: 10.0.0.0/16
  database:
    type: test.database
    engine: postgres
    network: ${network.id}
`)
	resolved, ds := Compile(files, testRegistry(t), Options{Environment: "dev"})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if resolved.Project != "myapp" || resolved.Environment != "dev" {
		t.Errorf("Project/Environment = %q/%q, want myapp/dev", resolved.Project, resolved.Environment)
	}

	db, ok := resolved.Get(address.Address{Name: "database"})
	if !ok {
		t.Fatal("database resource missing from the resolved config")
	}
	size, ok := db.Attrs["size"]
	if !ok || size.Source != value.SourceDefault {
		t.Fatalf("stage 7's default for size was not filled in: %+v", size)
	}
	if n, _ := size.AsInt(); n != 10 {
		t.Errorf("size = %d, want the dev default of 10", n)
	}
}

func TestCompileStopsAfterDecodeErrors(t *testing.T) {
	// A duplicate definition leaves the decoder unable to build a complete
	// config. If later stages ran anyway, this lone database — which has no
	// network anywhere in the file — would also trip stage 8's missing-
	// requirement check, burying the real problem: the duplicate itself.
	files := loadFiles(t, `
project: myapp
resources:
  database:
    type: test.database
    engine: postgres
  database:
    type: test.database
    engine: postgres
`)
	_, ds := Compile(files, testRegistry(t), Options{Environment: "dev"})
	if !ds.HasErrors() {
		t.Fatal("a duplicate resource definition must be an error")
	}

	var out strings.Builder
	ds.Render(&out)
	if !strings.Contains(out.String(), "defined more than once") {
		t.Errorf("expected the duplicate-definition diagnostic:\n%s", out.String())
	}
	if strings.Contains(out.String(), "network") {
		t.Errorf("stage 8 must not run against a config the decoder could not build; got noise:\n%s", out.String())
	}
}

func TestCompileStopsAfterSchemaErrorsBeforeGraphValidation(t *testing.T) {
	// bogus's unresolved type keeps stage 7 from finishing cleanly. guarded's
	// contradictory lifecycle would be caught by stage 8 — if stage 8 ran. It
	// must not: a resource whose type never resolved is exactly the config
	// later stages cannot build on.
	files := loadFiles(t, `
project: myapp
resources:
  bogus:
    type: not.a.real.type
  guarded:
    type: test.network
    cidr: 10.0.0.0/16
    lifecycle:
      prevent_destroy: true
      retain: true
`)
	_, ds := Compile(files, testRegistry(t), Options{Environment: "dev"})
	if !ds.HasErrors() {
		t.Fatal("an unregistered type must be an error")
	}

	var out strings.Builder
	ds.Render(&out)
	if !strings.Contains(out.String(), "unknown resource type") {
		t.Errorf("expected stage 7's unknown-type diagnostic:\n%s", out.String())
	}
	if strings.Contains(out.String(), "prevent_destroy") {
		t.Errorf("stage 8 must not run once stage 7 has errors; got its lifecycle diagnostic anyway:\n%s", out.String())
	}
}

func TestCompileAccumulatesDiagnosticsWithinAStage(t *testing.T) {
	files := loadFiles(t, `
project: myapp
resources:
  a:
    type: nope.one
  b:
    type: nope.two
`)
	_, ds := Compile(files, testRegistry(t), Options{Environment: "dev"})
	if len(ds) < 2 {
		t.Errorf("got %d diagnostics, want at least 2 — one bad type must not mask the other", len(ds))
	}
}

func TestCompileReportsStage8Diagnostics(t *testing.T) {
	files := loadFiles(t, `
project: myapp
resources:
  database:
    type: test.database
    engine: postgres
`)
	_, ds := Compile(files, testRegistry(t), Options{Environment: "dev"})
	if !ds.HasErrors() {
		t.Fatal("a database with no network anywhere in the project must be caught before any provider call")
	}

	var out strings.Builder
	ds.Render(&out)
	if !strings.Contains(out.String(), "network") {
		t.Errorf("expected stage 8's missing-requirement diagnostic:\n%s", out.String())
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/compiler/ -run TestCompile -v`
Expected: FAIL — `undefined: Compile`.

- [ ] **Step 3: Implement Compile**

Create `internal/compiler/compile.go`:

```go
package compiler

import (
	"infra/internal/config"
	"infra/internal/diag"
	"infra/internal/registry"
)

// Compile runs the full compiler pipeline — decode, reference binding,
// schema binding, and whole-graph validation — accumulating diagnostics from
// whichever stages run.
//
// Diagnostics still collect within a stage: one bad resource does not stop
// that stage from reporting its siblings' problems too. But a stage that
// could not build a usable ResolvedConfig must not hand it to the next one —
// a stage-7 kind check against a resource whose type was never resolved
// produces noise, not signal, and the same is true of stage 8 reasoning about
// a graph that stage 6 or 7 could not finish. Compile therefore stops at the
// first stage boundary where HasErrors() is true, returning everything
// collected up to and including that stage.
func Compile(files []config.File, reg *registry.Registry, opts Options) (ResolvedConfig, diag.Diagnostics) {
	var ds diag.Diagnostics

	project, decodeDiags := config.Decode(files)
	ds.Extend(decodeDiags)
	if decodeDiags.HasErrors() {
		return ResolvedConfig{}, ds
	}

	cfg, bindDiags := bindReferences(project, opts)
	ds.Extend(bindDiags)
	if bindDiags.HasErrors() {
		return cfg, ds
	}

	schemaDiags := bindSchemas(&cfg, reg, opts)
	ds.Extend(schemaDiags)
	if schemaDiags.HasErrors() {
		return cfg, ds
	}

	validateDiags := validateGraph(&cfg, reg)
	ds.Extend(validateDiags)

	return cfg, ds
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/compiler/ -v`
Expected: PASS — five new tests plus the previous thirty-eight from Tasks 5–8.

- [ ] **Step 5: Commit**

```bash
git add internal/compiler/compile.go internal/compiler/compile_test.go
git commit -m "$(cat <<'EOF'
feat: orchestrate the compiler pipeline with Compile

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_015M5QdCFKa6booSVrhnAzTV
EOF
)"
```

- [ ] **Step 6: Write the failing test for the CLI rewiring**

`infra validate` currently only exercises M1's three-check subset. This test would pass against that subset having caught nothing wrong, because M1 never looks at requirements — it fails only once `validateProject` delegates to the full pipeline. Append it to `internal/cli/validate_test.go`, whose full contents (existing tests unchanged, new test added at the end) are:

```go
package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"infra/internal/diag"
	"infra/pkg/value"
)

func projectDir(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "infra.yml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return dir
}

func TestValidateAcceptsAGoodProject(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: 10.20.0.0/16
  database:
    type: test.database
    engine: postgres
    network: ${network.id}
`)
	ds := validateProject(dir, buildRegistry(dir))
	if ds.HasErrors() {
		t.Fatalf("valid project reported errors: %+v", ds)
	}
}

func TestValidateRejectsUnknownResourceType(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  database:
    type: aws.rds
    engine: postgres
`)
	ds := validateProject(dir, buildRegistry(dir))
	if !ds.HasErrors() {
		t.Fatal("an unregistered resource type must be an error")
	}
	joined := renderToString(ds)
	if !strings.Contains(joined, "aws.rds") {
		t.Errorf("diagnostic does not name the offending type:\n%s", joined)
	}
	if !strings.Contains(joined, "test.database") {
		t.Errorf("diagnostic should suggest the known types:\n%s", joined)
	}
}

func TestValidateRejectsUnknownAttribute(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  database:
    type: test.database
    engine: postgres
    nonsense: true
`)
	ds := validateProject(dir, buildRegistry(dir))
	if !ds.HasErrors() {
		t.Fatal("an attribute the schema does not define must be an error")
	}
	if !strings.Contains(renderToString(ds), "nonsense") {
		t.Error("diagnostic must name the unknown attribute")
	}
}

func TestValidateRejectsSettingComputedAttribute(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  database:
    type: test.database
    engine: postgres
    endpoint: nope.example.com
`)
	ds := validateProject(dir, buildRegistry(dir))
	if !ds.HasErrors() {
		t.Fatal("configuration must not set a computed attribute")
	}
}

func TestFormatValueRedactsNestedSensitiveLeaves(t *testing.T) {
	// Sensitivity is per-leaf: a non-sensitive composite can hold a sensitive
	// element. Redacting only the top level would leak it through %v.
	secret := value.String("hunter2", value.SourceProvider).WithSensitive(true)

	cases := []struct {
		name string
		in   value.Value
	}{
		{"sensitive scalar", secret},
		{"sensitive map leaf", value.Map(map[string]value.Value{
			"user": value.String("admin", value.SourceProvider),
			"pass": secret,
		}, value.SourceProvider)},
		{"sensitive list element", value.List([]value.Value{
			value.String("public", value.SourceProvider),
			secret,
		}, value.SourceProvider)},
		{"map nested inside a list", value.List([]value.Value{
			value.Map(map[string]value.Value{"pass": secret}, value.SourceProvider),
		}, value.SourceProvider)},
	}

	for _, tc := range cases {
		got := formatValue(tc.in)
		if strings.Contains(got, "hunter2") {
			t.Errorf("%s: rendered %q, which leaks the secret", tc.name, got)
		}
		if !strings.Contains(got, "<sensitive>") {
			t.Errorf("%s: rendered %q, want a <sensitive> marker", tc.name, got)
		}
	}
}

func TestFormatValueSortsMapKeys(t *testing.T) {
	got := formatValue(value.Map(map[string]value.Value{
		"b": value.Int(2, value.SourceProvider),
		"a": value.String("x", value.SourceProvider),
	}, value.SourceProvider))
	if got != "{a: x, b: 2}" {
		t.Errorf("formatValue = %q, want %q — map keys must be sorted or output churns between runs", got, "{a: x, b: 2}")
	}
}

func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  a:
    type: nope.one
  b:
    type: nope.two
  c:
    type: nope.three
`)
	ds := validateProject(dir, buildRegistry(dir))
	if len(ds) < 3 {
		t.Errorf("got %d diagnostics, want at least 3", len(ds))
	}
}

func renderToString(ds diag.Diagnostics) string {
	var b strings.Builder
	ds.Render(&b)
	return b.String()
}

// TestDiagnosticsAdviseOnlyRegisteredCommands keeps the tool from telling a
// user to run something it does not have. The unknown-attribute diagnostic
// suggested `infra explain <type>`, which is not registered until M7, so
// following the advice yielded "unknown command". Spec §16 refuses command
// stubs on the grounds that a stub promises a capability that does not exist;
// a diagnostic makes the same promise.
func TestDiagnosticsAdviseOnlyRegisteredCommands(t *testing.T) {
	registered := map[string]bool{}
	var collect func(c *cobra.Command)
	collect = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			registered[sub.Name()] = true
			collect(sub)
		}
	}
	collect(NewRootCommand())

	dir := projectDir(t, `
project: myapp
resources:
  database:
    type: test.database
    engine: postgres
    nonexistent: 1
`)
	ds := validateProject(dir, buildRegistry(dir))
	if !ds.HasErrors() {
		t.Fatal("an unknown attribute must be an error")
	}

	var buf bytes.Buffer
	ds.Render(&buf)
	rendered := buf.String()
	if !strings.Contains(rendered, "nonexistent") {
		t.Fatalf("expected an unknown-attribute diagnostic, got:\n%s", rendered)
	}

	for _, m := range regexp.MustCompile("`infra ([a-z-]+)").FindAllStringSubmatch(rendered, -1) {
		if !registered[m[1]] {
			t.Errorf("diagnostic advises `infra %s`, which is not a registered command:\n%s", m[1], rendered)
		}
	}
}

// TestValidateCatchesMissingRequiredInfrastructure is stage 8, reachable only
// because validate now delegates to the full Compile pipeline. M1's
// three-check subset — type registered, attribute exists, attribute not
// computed — had no way to catch this.
func TestValidateCatchesMissingRequiredInfrastructure(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  database:
    type: test.database
    engine: postgres
`)
	ds := validateProject(dir, buildRegistry(dir))
	if !ds.HasErrors() {
		t.Fatal("a database with no network anywhere in the project must be an error")
	}
	if !strings.Contains(renderToString(ds), "network") {
		t.Errorf("diagnostic must name the missing requirement:\n%s", renderToString(ds))
	}
}
```

- [ ] **Step 7: Run the test to verify it fails**

Run: `go test ./internal/cli/ -run TestValidateCatchesMissingRequiredInfrastructure -v`
Expected: FAIL — `validateProject` still runs only M1's three checks, so `ds.HasErrors()` is false and the test fails at `t.Fatal("a database with no network anywhere in the project must be an error")`.

- [ ] **Step 8: Rewire validate.go**

Replace `internal/cli/validate.go` in full:

```go
package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"infra/internal/compiler"
	"infra/internal/config"
	"infra/internal/diag"
	"infra/internal/registry"
)

// newValidateCommand builds the `infra validate` command.
func newValidateCommand(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "Check configuration for errors without contacting providers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ds := validateProject(opts.Dir, buildRegistry(opts.Dir))
			ds.Render(cmd.ErrOrStderr())

			if ds.HasErrors() {
				return errors.New("configuration is not valid")
			}
			fmt.Fprintln(cmd.OutOrStdout(), "✓ Configuration valid")
			return nil
		},
	}
}

// validateProject runs the full compiler pipeline — stages 1 through 8 — and
// returns every diagnostic it produces. `infra validate` does not pin an
// environment, so environment-varying defaults resolve against the empty
// string; that only changes which default is filled in, never whether the
// configuration is valid.
func validateProject(dir string, reg *registry.Registry) diag.Diagnostics {
	files, err := config.Load(dir)
	if err != nil {
		var ds diag.Diagnostics
		ds.Add(diag.Diagnostic{Severity: diag.SeverityError, Summary: err.Error()})
		return ds
	}

	_, ds := compiler.Compile(files, reg, compiler.Options{})
	return ds
}
```

This drops the `sort`, `strconv`, `strings` and `infra/pkg/schema` imports M1's inline checks needed, along with the now-dead `attributeNames` helper — `compiler.bindSchemas` already has its own equivalent, and nothing else in this file used it.

- [ ] **Step 9: Confirm M1's existing tests need no other changes**

Every M1 test in `internal/cli/validate_test.go` still passes unmodified, and each does so for the same reason: whenever one of them triggers a stage-7 error (an unknown type, an unknown attribute, or setting a computed attribute), `Compile` stops before stage 8 ever runs, so no new stage-8 diagnostic — about a missing requirement, in particular — can appear alongside it and change what the test observes.

- `TestValidateAcceptsAGoodProject`: no stage-7 errors, and stage 8 does run — but `network` is declared, so `database`'s requirement is satisfied. Passes exactly as before.
- `TestValidateRejectsUnknownResourceType`, `TestValidateRejectsUnknownAttribute`, `TestValidateRejectsSettingComputedAttribute`: each trips a stage-7 error, so stage 8 never runs. The diagnostics asserted on are unchanged.
- `TestValidateReportsEveryProblemAtOnce`: three resources with unknown types are three stage-7 errors; stage 8 never runs. `len(ds) >= 3` still holds.
- `TestDiagnosticsAdviseOnlyRegisteredCommands`: trips the unknown-attribute stage-7 error; stage 8 never runs, so the only diagnostic to scan for `infra <command>` advice is the one M1 already produced.
- `TestFormatValueRedactsNestedSensitiveLeaves`, `TestFormatValueSortsMapKeys`: exercise `formatValue` directly, in `internal/cli/state.go`, untouched by this task.

- [ ] **Step 10: Run the tests to verify they pass**

Run: `go test ./internal/cli/ -v`
Expected: PASS — all eight existing tests plus the new one.

- [ ] **Step 11: Commit**

```bash
git add internal/cli/validate.go internal/cli/validate_test.go
git commit -m "$(cat <<'EOF'
feat: wire infra validate to the full compiler pipeline

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_015M5QdCFKa6booSVrhnAzTV
EOF
)"
```

---
## Task 10: The dependency graph

**Files:**
- Create: `internal/graph/graph.go`
- Test: `internal/graph/graph_test.go`

**Interfaces:**
- Consumes: nothing beyond the standard library. `graph` is a leaf package with no dependency on any other `infra` package — that is what lets M3's executor reuse it for tasks the same way M2's planner will reuse it for operations.
- Produces: `graph.Node interface { ID() string }`; `graph.Graph[T Node]` with `New[T]() *Graph[T]`, `(*Graph[T]).Add(node T)`, `(*Graph[T]).Edge(fromID, toID string)`, `(*Graph[T]).Cycle() []string`, `(*Graph[T]).Layers() ([][]T, error)`, `(*Graph[T]).Roots() []T`.

This package exists to be reused, which is why it is generic over anything with an `ID()` rather than over `resource.ResolvedResource` or a plan `Operation` directly. Determinism is the entire point of building it now instead of inlining a topological sort into the planner later: Go map iteration is randomised, so `Cycle`, `Layers` and `Roots` each sort at their own map-to-slice boundary — node IDs, one node's outgoing edges, one layer's ready set, the root set — rather than relying on a single sort somewhere upstream to cover every path through the code.

`Edge(fromID, toID)` naming a node nobody `Add`ed is a programming error in the caller, not user input, so it panics rather than returning an error. A graph in this system is always assembled by another `infra` package — the planner turning `ResolvedConfig` and `State` into operations, the executor turning a plan into tasks — from addresses that package already knows are valid; it is never built from unvalidated configuration text a user typed, which is what would make an error return the right shape. Panicking surfaces the mistake at the call site that made it; returning an error would let a graph silently end up with one fewer edge than its builder intended, and a `Layers()` computed over that graph would just be wrong, quietly. This is the same judgement the codebase already made once (`internal/cli` panics when a provider is registered under a type it does not own), not a new one. `Add` on an ID that is already present simply replaces the node, the same as an ordinary map assignment — there is no ambiguity to guard because nothing downstream distinguishes "replaced" from "first added".

`Cycle()` returns the cycle as the sequence of node IDs in participation order — each consecutive pair, including the wrap from the last ID back to the first, is a real edge — rather than repeating the first ID at the end. `Layers()` calls `Cycle()` first and turns a non-nil result into an error that names the cycle in that same form, so a caller never has to re-derive it from a partial layering. `Roots()` is not "layer zero of `Layers()`" reimplemented — it is nodes with no incoming edge, computed directly from `in`, because both the executor (M3) and `infra graph` (M7) need "what can start right now" without paying for a full topological sort when all they want is the starting set.

- [ ] **Step 1: Write the failing test**

Create `internal/graph/graph_test.go`:

```go
package graph

import (
	"strings"
	"testing"
)

type testNode string

func (n testNode) ID() string { return string(n) }

func build(t *testing.T, nodes []string, edges [][2]string) *Graph[testNode] {
	t.Helper()
	g := New[testNode]()
	for _, id := range nodes {
		g.Add(testNode(id))
	}
	for _, e := range edges {
		g.Edge(e[0], e[1])
	}
	return g
}

func TestRootsOnAGraphWithNoEdgesReturnsEveryNodeSorted(t *testing.T) {
	g := build(t, []string{"c", "a", "b"}, nil)
	roots := g.Roots()
	if len(roots) != 3 || roots[0] != "a" || roots[1] != "b" || roots[2] != "c" {
		t.Errorf("Roots() = %v, want [a b c] sorted", roots)
	}
}

func TestAddReplacesAnExistingID(t *testing.T) {
	g := New[testNode]()
	g.Add(testNode("a"))
	g.Add(testNode("a")) // same ID: replaces, does not duplicate
	if len(g.Roots()) != 1 {
		t.Errorf("Roots() = %v, want exactly one node", g.Roots())
	}
}

func TestRootsExcludesNodesWithAnIncomingEdge(t *testing.T) {
	g := build(t, []string{"a", "b", "c"}, [][2]string{{"a", "b"}})
	roots := g.Roots()
	if len(roots) != 2 || roots[0] != "a" || roots[1] != "c" {
		t.Errorf("Roots() = %v, want [a c] — b has a predecessor", roots)
	}
}

func TestCycleIsNilOnAnAcyclicGraph(t *testing.T) {
	g := build(t, []string{"a", "b", "c"}, [][2]string{{"a", "b"}, {"b", "c"}})
	if cycle := g.Cycle(); cycle != nil {
		t.Errorf("Cycle() = %v, want nil", cycle)
	}
}

func TestCycleFindsAThreeNodeCycle(t *testing.T) {
	g := build(t, []string{"a", "b", "c"}, [][2]string{{"a", "b"}, {"b", "c"}, {"c", "a"}})
	cycle := g.Cycle()
	if len(cycle) != 3 {
		t.Fatalf("Cycle() = %v, want a 3-node cycle", cycle)
	}
	// The cycle is a-b-c in some rotation; every consecutive pair, including
	// the wrap, must be an edge this graph actually has.
	edges := map[[2]string]bool{{"a", "b"}: true, {"b", "c"}: true, {"c", "a"}: true}
	for i := range cycle {
		pair := [2]string{cycle[i], cycle[(i+1)%len(cycle)]}
		if !edges[pair] {
			t.Errorf("Cycle() = %v names pair %v which is not an edge", cycle, pair)
		}
	}
}

func TestCycleOfOneIsASelfEdge(t *testing.T) {
	g := build(t, []string{"a"}, [][2]string{{"a", "a"}})
	cycle := g.Cycle()
	if len(cycle) != 1 || cycle[0] != "a" {
		t.Errorf("Cycle() = %v, want [a]", cycle)
	}
}

func TestCycleIsDeterministicAcrossRuns(t *testing.T) {
	g := build(t, []string{"a", "b", "c", "d"}, [][2]string{
		{"d", "a"}, {"a", "b"}, {"b", "c"}, {"c", "a"},
	})
	first := g.Cycle()
	for i := 0; i < 20; i++ {
		if got := g.Cycle(); strings.Join(got, ",") != strings.Join(first, ",") {
			t.Fatalf("Cycle() = %v, want %v on every run — map iteration is randomised", got, first)
		}
	}
}

func TestLayersOrdersADiamond(t *testing.T) {
	//   a
	//  / \
	// b   c
	//  \ /
	//   d
	g := build(t, []string{"a", "b", "c", "d"}, [][2]string{
		{"a", "b"}, {"a", "c"}, {"b", "d"}, {"c", "d"},
	})
	layers, err := g.Layers()
	if err != nil {
		t.Fatalf("Layers: %v", err)
	}
	if len(layers) != 3 {
		t.Fatalf("Layers() has %d layers, want 3", len(layers))
	}
	if len(layers[0]) != 1 || layers[0][0] != "a" {
		t.Errorf("layer 0 = %v, want [a]", layers[0])
	}
	if len(layers[1]) != 2 || layers[1][0] != "b" || layers[1][1] != "c" {
		t.Errorf("layer 1 = %v, want [b c] sorted", layers[1])
	}
	if len(layers[2]) != 1 || layers[2][0] != "d" {
		t.Errorf("layer 2 = %v, want [d]", layers[2])
	}
}

func TestLayersOnAnEmptyGraphIsEmpty(t *testing.T) {
	g := New[testNode]()
	layers, err := g.Layers()
	if err != nil {
		t.Fatalf("Layers: %v", err)
	}
	if len(layers) != 0 {
		t.Errorf("Layers() = %v, want none", layers)
	}
}

func TestLayersIsDeterministicAcrossRuns(t *testing.T) {
	g := build(t, []string{"a", "b", "c", "d", "e"}, [][2]string{
		{"a", "c"}, {"b", "c"}, {"c", "d"}, {"c", "e"},
	})
	render := func(layers [][]testNode) string {
		var b strings.Builder
		for _, l := range layers {
			for _, n := range l {
				b.WriteString(string(n))
				b.WriteByte(',')
			}
			b.WriteByte('|')
		}
		return b.String()
	}
	first, err := g.Layers()
	if err != nil {
		t.Fatalf("Layers: %v", err)
	}
	want := render(first)
	for i := 0; i < 20; i++ {
		got, err := g.Layers()
		if err != nil {
			t.Fatalf("Layers: %v", err)
		}
		if render(got) != want {
			t.Fatalf("Layers() = %q, want %q on every run", render(got), want)
		}
	}
}

func TestLayersReportsACycleByName(t *testing.T) {
	g := build(t, []string{"a", "b"}, [][2]string{{"a", "b"}, {"b", "a"}})
	_, err := g.Layers()
	if err == nil {
		t.Fatal("Layers() on a cyclic graph must return an error, not a partial result")
	}
	if !strings.Contains(err.Error(), "a") || !strings.Contains(err.Error(), "b") {
		t.Errorf("error should name the cycle: %v", err)
	}
}

func TestDuplicateEdgeIsOneEdge(t *testing.T) {
	g := build(t, []string{"a", "b"}, [][2]string{{"a", "b"}, {"a", "b"}})
	layers, err := g.Layers()
	if err != nil {
		t.Fatalf("Layers: %v", err)
	}
	if len(layers) != 2 || len(layers[0]) != 1 || len(layers[1]) != 1 {
		t.Errorf("Layers() = %v, want two singleton layers regardless of the duplicate edge", layers)
	}
}

func TestEdgeToAnUnaddedNodePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Edge naming a node that was never added must panic — it is a programming error in the caller, never user input")
		}
	}()
	g := New[testNode]()
	g.Add(testNode("a"))
	g.Edge("a", "ghost")
}

func TestEdgeFromAnUnaddedNodePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Edge naming an unadded source node must panic")
		}
	}()
	g := New[testNode]()
	g.Add(testNode("a"))
	g.Edge("ghost", "a")
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/graph/ -v`
Expected: FAIL — the package does not exist.

- [ ] **Step 3: Implement the graph**

Create `internal/graph/graph.go`:

```go
// Package graph implements a generic, deterministic directed graph used to
// order work: M2 orders plan operations with it, and M3's executor reuses
// it for tasks. Determinism is the point — Cycle, Layers and Roots all sort
// before returning, because Go map iteration is randomised and two runs
// over identical nodes and edges must produce identical output (spec §14,
// invariant 6).
package graph

import (
	"fmt"
	"sort"
	"strings"
)

// Node is anything a Graph can hold: a stable, caller-assigned identifier
// that Add and Edge key on.
type Node interface {
	// ID returns the node's identifier, unique within one Graph.
	ID() string
}

// Graph is a generic directed graph over nodes with unique IDs.
type Graph[T Node] struct {
	nodes map[string]T
	out   map[string]map[string]bool // fromID -> set of toID
	in    map[string]map[string]bool // toID -> set of fromID
}

// New returns an empty graph.
func New[T Node]() *Graph[T] {
	return &Graph[T]{
		nodes: map[string]T{},
		out:   map[string]map[string]bool{},
		in:    map[string]map[string]bool{},
	}
}

// Add registers a node, keyed by its ID. Adding a node whose ID is already
// present replaces it, the same as an ordinary map assignment.
func (g *Graph[T]) Add(node T) {
	g.nodes[node.ID()] = node
}

// Edge records that fromID must run before toID.
//
// Naming a node that was never added is a programming error, not user
// input: a graph in this system is always built by another infra package —
// the planner assembling operations, the executor assembling tasks — from
// addresses it already knows are valid, never from unvalidated
// configuration text a user typed. Edge therefore panics rather than
// returning an error, so the mistake surfaces at the call site that made it
// instead of silently leaving a graph with one fewer edge than its builder
// intended. This matches the one other place the codebase already panics on
// an invariant violation rather than a user-facing one (internal/cli's
// provider registration).
func (g *Graph[T]) Edge(fromID, toID string) {
	if _, ok := g.nodes[fromID]; !ok {
		panic("graph: Edge(" + fromID + ", " + toID + "): " + fromID + " was never added")
	}
	if _, ok := g.nodes[toID]; !ok {
		panic("graph: Edge(" + fromID + ", " + toID + "): " + toID + " was never added")
	}
	if g.out[fromID] == nil {
		g.out[fromID] = map[string]bool{}
	}
	g.out[fromID][toID] = true
	if g.in[toID] == nil {
		g.in[toID] = map[string]bool{}
	}
	g.in[toID][fromID] = true
}

// sortedIDs returns every node ID, sorted.
func (g *Graph[T]) sortedIDs() []string {
	ids := make([]string, 0, len(g.nodes))
	for id := range g.nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// sortedOut returns the IDs fromID points to, sorted.
func (g *Graph[T]) sortedOut(fromID string) []string {
	next := g.out[fromID]
	out := make([]string, 0, len(next))
	for id := range next {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Cycle returns the first cycle found, as the node IDs in participation
// order — each consecutive pair, including the wrap from the last ID back
// to the first, is a real edge — or nil when the graph is acyclic. Nodes
// and their outgoing edges are both visited in sorted order, so the result
// is identical on every run.
func (g *Graph[T]) Cycle() []string {
	const (
		unvisited = iota
		visiting
		done
	)
	state := make(map[string]int, len(g.nodes))
	var stack []string

	var visit func(id string) []string
	visit = func(id string) []string {
		state[id] = visiting
		stack = append(stack, id)

		for _, next := range g.sortedOut(id) {
			switch state[next] {
			case visiting:
				// next is still on the stack: the cycle is the suffix of
				// stack starting at next.
				for i, s := range stack {
					if s == next {
						return append([]string(nil), stack[i:]...)
					}
				}
			case unvisited:
				if cycle := visit(next); cycle != nil {
					return cycle
				}
			}
		}

		stack = stack[:len(stack)-1]
		state[id] = done
		return nil
	}

	for _, id := range g.sortedIDs() {
		if state[id] == unvisited {
			if cycle := visit(id); cycle != nil {
				return cycle
			}
		}
	}
	return nil
}

// Layers returns the graph's nodes grouped into topological layers: layer 0
// holds every node with no incoming edge, layer 1 every node whose
// predecessors are all in layer 0, and so on. Each layer is sorted by ID,
// so two runs over the same graph produce identical output.
//
// It returns an error naming the cycle when the graph is not a DAG.
func (g *Graph[T]) Layers() ([][]T, error) {
	if cycle := g.Cycle(); cycle != nil {
		full := append(append([]string(nil), cycle...), cycle[0])
		return nil, fmt.Errorf("graph: cycle detected: %s", strings.Join(full, " -> "))
	}

	remaining := make(map[string]int, len(g.nodes))
	for id := range g.nodes {
		remaining[id] = len(g.in[id])
	}

	var layers [][]T
	for len(remaining) > 0 {
		var ready []string
		for id, degree := range remaining {
			if degree == 0 {
				ready = append(ready, id)
			}
		}
		sort.Strings(ready)

		layer := make([]T, 0, len(ready))
		for _, id := range ready {
			layer = append(layer, g.nodes[id])
			delete(remaining, id)
		}
		for _, id := range ready {
			for _, next := range g.sortedOut(id) {
				if _, ok := remaining[next]; ok {
					remaining[next]--
				}
			}
		}
		layers = append(layers, layer)
	}
	return layers, nil
}

// Roots returns every node with no incoming edge — the nodes with no
// prerequisite, which can run first — sorted by ID.
func (g *Graph[T]) Roots() []T {
	var ids []string
	for id := range g.nodes {
		if len(g.in[id]) == 0 {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)

	out := make([]T, 0, len(ids))
	for _, id := range ids {
		out = append(out, g.nodes[id])
	}
	return out
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/graph/ -v`
Expected: PASS — fourteen tests.

- [ ] **Step 5: Confirm every map-to-slice boundary sorts**

Run: `grep -n "sort.Strings" internal/graph/graph.go`
Expected: four matches — `sortedIDs`, `sortedOut`, the `ready` slice inside `Layers`, and the `ids` slice inside `Roots`. These are the only four places a map in this file becomes a slice; confirm none was missed, or say so in your report if the count differs.

- [ ] **Step 6: Commit**

```bash
git add internal/graph
git commit -m "$(cat <<'EOF'
feat: add the generic dependency graph

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_015M5QdCFKa6booSVrhnAzTV
EOF
)"
```

---

## Task 11: Refresh

**Files:**
- Create: `internal/refresh/refresh.go`
- Test: `internal/refresh/refresh_test.go`

**Interfaces:**
- Consumes: `address.Address`, `resource.ResourceState` (M1); `registry.Registry.Provider`, `.Types` (M1); `state.State.Get`, `.Addresses` (M1); `diag.Diagnostics`, `diag.Diagnostic`, `diag.SeverityError` (M1); `provider.Provider.Read` via the registry (M1).
- Produces: `refresh.Observation{Address address.Address; State *resource.ResourceState; Err error}`; `refresh.Observations map[string]Observation`; `refresh.Refresh(ctx context.Context, st *state.State, reg *registry.Registry, parallelism int) (Observations, diag.Diagnostics)`.

`Refresh` reads and never writes. Spec §10 reserves persistence for the `refresh` command M3 adds; `plan` calls this function, uses its result in memory, and discards it. That asymmetry is what keeps `plan` safe to run against a locked environment, in CI, or back-to-back, and it is what keeps the planner (Task 13) a pure function of the inputs it is handed — nothing in this package may call `(*Local).Put`, and nothing here mutates the `*state.State` it is given. Reads run concurrently through a semaphore sized to `parallelism`, with values below 1 treated as 1 so a misconfigured `--parallelism 0` degrades to serial reads rather than deadlocking on a zero-capacity channel.

The single most dangerous mistake available in this file is conflating a read error with a deleted resource. A provider's `Read` returning `(nil, nil)` is the interface's contract for "gone" — that is how drift caused by deletion outside `infra` is detected, and it is not an error. A `Read` returning a non-nil `error` is the opposite: reality is simply unknown right now, because of a timeout, a permissions change, or an outage, and the resource may be exactly as it was. `Observation.Err` exists so these two facts can never collapse into the same `State: nil` shape without a caller being able to tell them apart — an error becomes a diagnostic that fails planning for that resource specifically, while every other resource's refresh proceeds and reports independently, the same "collect every problem, don't stop at the first one" discipline the compiler stages already follow. Treating a transient failure as deletion would propose destroying infrastructure that is still there.

A resource recorded in state whose type is no longer registered — because a provider was removed from the build, or its type renamed — gets the same treatment as an unknown type at compile time (Task 7): a diagnostic naming the type and listing what is registered, and an `Observation` with `Err` set. It is deliberately not skipped and not folded into the "deleted" case: skipping it would let a plan proceed while blind to a resource it still manages, and reporting it as deleted would propose destroying something nobody has actually looked at.

Concurrency must not leak into the result. Each resource's read is dispatched into its own slot of a slice sized and ordered by `st.Addresses()`, which is already sorted; the goroutines race against each other, but assembling `Observations` and `diag.Diagnostics` afterwards walks that fixed, sorted slice rather than whatever order the reads happened to finish in. Two runs over the same state therefore produce byte-identical diagnostics even though the reads that produced them ran in an unpredictable order — the property `-race` and a differential-timing test both exist to pin down.

- [ ] **Step 1: Write the failing test**

Create `internal/refresh/refresh_test.go`:

```go
package refresh

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"infra/internal/registry"
	"infra/internal/state"
	"infra/pkg/address"
	"infra/pkg/provider"
	"infra/pkg/resource"
	"infra/pkg/schema"
	"infra/pkg/value"
	testprovider "infra/providers/test"
)

func newTestRegistry(t *testing.T, cloudPath string) (*registry.Registry, *testprovider.Provider) {
	t.Helper()
	reg := registry.New()
	prov := testprovider.New(cloudPath)
	if err := reg.Register(prov); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return reg, prov
}

func createNetwork(t *testing.T, prov *testprovider.Provider, name string) *resource.ResourceState {
	t.Helper()
	st, err := prov.Create(context.Background(), &resource.DesiredResource{
		Address: address.Address{Name: name},
		Type:    "test.network",
		Attrs: map[string]value.Value{
			"cidr": value.String("10.0.0.0/16", value.SourceExplicit),
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return st
}

func TestRefreshReadsCurrentProviderState(t *testing.T) {
	dir := t.TempDir()
	reg, prov := newTestRegistry(t, filepath.Join(dir, "cloud.json"))

	created := createNetwork(t, prov, "net")
	st := state.New("myapp", "dev")
	st.Set(created)

	obs, ds := Refresh(context.Background(), st, reg, 4)
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	o, ok := obs[created.Address.String()]
	if !ok {
		t.Fatal("missing observation for net")
	}
	if o.Err != nil {
		t.Fatalf("unexpected error: %v", o.Err)
	}
	if o.State == nil {
		t.Fatal("resource exists at the provider; State must not be nil")
	}
	if cidr, _ := o.State.Attributes["cidr"].AsString(); cidr != "10.0.0.0/16" {
		t.Errorf("cidr = %q", cidr)
	}
}

func TestRefreshDetectsDeletionOutsideInfra(t *testing.T) {
	dir := t.TempDir()
	reg, prov := newTestRegistry(t, filepath.Join(dir, "cloud.json"))

	created := createNetwork(t, prov, "net")
	st := state.New("myapp", "dev")
	st.Set(created)

	// Deleted by something other than infra: the same path a person
	// hand-editing the cloud file, or another tool, would take.
	if err := prov.Delete(context.Background(), created); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	obs, ds := Refresh(context.Background(), st, reg, 4)
	if ds.HasErrors() {
		t.Fatalf("a deletion outside infra is not a planning error: %+v", ds)
	}
	o := obs[created.Address.String()]
	if o.State != nil {
		t.Error("State must be nil — the provider reported (nil, nil)")
	}
	if o.Err != nil {
		t.Errorf("Err must be nil — deletion is not a failure: %v", o.Err)
	}
}

func TestRefreshReadErrorIsADiagnosticAndNeverADeletion(t *testing.T) {
	cloudPath := filepath.Join(t.TempDir(), "cloud.json")
	reg, prov := newTestRegistry(t, cloudPath)

	created := createNetwork(t, prov, "net")
	st := state.New("myapp", "dev")
	st.Set(created)

	// Inject a one-shot read failure directly into the cloud file, the
	// mechanism providers/test/cloud.go defines: a FailureRule keyed by op
	// and address that fires once (Fired: true) and then never again.
	cloud, err := testprovider.LoadCloud(cloudPath)
	if err != nil {
		t.Fatalf("LoadCloud: %v", err)
	}
	cloud.Failures = append(cloud.Failures, testprovider.FailureRule{
		Op:      "read",
		Address: created.Address.String(),
		Nth:     1,
		Message: "simulated provider outage",
	})
	if err := cloud.Save(cloudPath); err != nil {
		t.Fatalf("Save: %v", err)
	}

	obs, ds := Refresh(context.Background(), st, reg, 4)
	if !ds.HasErrors() {
		t.Fatal("a read failure must fail planning for that resource")
	}
	o := obs[created.Address.String()]
	if o.Err == nil {
		t.Fatal("Observation.Err must be set")
	}
	if o.State != nil {
		t.Error("State must not be populated when the read failed")
	}

	// The critical distinction: this resource was never deleted. The
	// injected rule is one-shot, so a second refresh must find the resource
	// exactly where it was — proving the first error was a transient read
	// failure, not the resource going away. Mistaking the first result for
	// absence would have proposed destroying live infrastructure.
	obs2, ds2 := Refresh(context.Background(), st, reg, 4)
	if ds2.HasErrors() {
		t.Fatalf("the injected rule is one-shot; the second refresh must succeed: %+v", ds2)
	}
	o2 := obs2[created.Address.String()]
	if o2.State == nil {
		t.Fatal("the resource still exists; a transient read error must never be mistaken for deletion")
	}
}

func TestRefreshNeverWritesState(t *testing.T) {
	root := t.TempDir()
	cloudPath := filepath.Join(t.TempDir(), "cloud.json")
	reg, prov := newTestRegistry(t, cloudPath)
	created := createNetwork(t, prov, "net")

	backend := state.NewLocal(root)
	st := state.New("myapp", "dev")
	st.Set(created)
	if err := backend.Put(context.Background(), "dev", st); err != nil {
		t.Fatalf("Put: %v", err)
	}

	statePath := filepath.Join(root, "state", "dev.json")
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	// Run Refresh against the state Get returns — never against backend —
	// which is the point: Refresh has no way to write state even if it
	// wanted to.
	loaded, err := backend.Get(context.Background(), "dev")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, ds := Refresh(context.Background(), loaded, reg, 4); ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(before) != string(after) {
		t.Error("the state file changed — Refresh must never write state; only the refresh command in M3 persists observations")
	}
}

func TestRefreshUnregisteredTypeIsADiagnostic(t *testing.T) {
	reg := registry.New() // nothing registered

	st := state.New("myapp", "dev")
	st.Set(&resource.ResourceState{
		Address:    address.Address{Name: "ghost"},
		Type:       "ghost.thing",
		ProviderID: "ghost-1",
	})

	obs, ds := Refresh(context.Background(), st, reg, 4)
	if !ds.HasErrors() {
		t.Fatal("a resource whose type is no longer registered must be a diagnostic, not silently skipped or treated as deleted")
	}
	o := obs["ghost"]
	if o.Err == nil {
		t.Error("Observation.Err must be set")
	}
	if o.State != nil {
		t.Error("State must be nil — nothing could be read")
	}
}

func TestRefreshEmptyStateReturnsEmptyObservations(t *testing.T) {
	reg := registry.New()
	st := state.New("myapp", "dev")

	obs, ds := Refresh(context.Background(), st, reg, 4)
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if len(obs) != 0 {
		t.Errorf("Observations = %v, want none", obs)
	}
}

func TestRefreshTreatsParallelismBelowOneAsOne(t *testing.T) {
	dir := t.TempDir()
	reg, prov := newTestRegistry(t, filepath.Join(dir, "cloud.json"))
	created := createNetwork(t, prov, "net")
	st := state.New("myapp", "dev")
	st.Set(created)

	for _, p := range []int{0, -1} {
		obs, ds := Refresh(context.Background(), st, reg, p)
		if ds.HasErrors() {
			t.Fatalf("parallelism %d: unexpected diagnostics: %+v", p, ds)
		}
		if len(obs) != 1 {
			t.Fatalf("parallelism %d: Observations = %v, want one entry", p, obs)
		}
	}
}

// delayedProvider is a minimal Provider double used to control read timing
// directly. The real fake provider (providers/test) holds one mutex across
// its entire Read call, which would serialize every read regardless of the
// bound Refresh itself applies — so it cannot prove Refresh's own
// parallelism bound is what is doing the limiting. This double has no such
// lock: only Refresh's semaphore governs how many of its Reads run at once.
type delayedProvider struct {
	resourceType string
	delay        time.Duration
	delays       map[string]time.Duration
	fail         map[string]bool

	mu            sync.Mutex
	concurrent    int
	maxConcurrent int
}

func (p *delayedProvider) Name() string { return "delayed" }

func (p *delayedProvider) Definitions() []*schema.ResourceDefinition {
	return []*schema.ResourceDefinition{{Type: p.resourceType}}
}

func (p *delayedProvider) Read(ctx context.Context, current *resource.ResourceState) (*resource.ResourceState, error) {
	p.mu.Lock()
	p.concurrent++
	if p.concurrent > p.maxConcurrent {
		p.maxConcurrent = p.concurrent
	}
	p.mu.Unlock()

	d := p.delay
	if p.delays != nil {
		if perAddr, ok := p.delays[current.Address.String()]; ok {
			d = perAddr
		}
	}
	if d > 0 {
		time.Sleep(d)
	}

	p.mu.Lock()
	p.concurrent--
	p.mu.Unlock()

	if p.fail[current.Address.String()] {
		return nil, fmt.Errorf("delayed provider: simulated failure for %s", current.Address)
	}
	return current.Clone(), nil
}

func (p *delayedProvider) Create(context.Context, *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}

func (p *delayedProvider) Update(context.Context, *resource.ResourceState, *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}

func (p *delayedProvider) Delete(context.Context, *resource.ResourceState) error {
	return provider.ErrNotImplemented
}

func (p *delayedProvider) Discover(context.Context, provider.DiscoverRequest) ([]provider.DiscoveredResource, error) {
	return nil, provider.ErrNotImplemented
}

func (p *delayedProvider) Import(context.Context, string, string) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}

func (p *delayedProvider) ClassifyError(error) provider.Retryability {
	return provider.NotSafeToRetry
}

var _ provider.Provider = (*delayedProvider)(nil)

func TestRefreshBoundsConcurrentReads(t *testing.T) {
	const resourceType = "delayed.thing"
	prov := &delayedProvider{resourceType: resourceType, delay: 50 * time.Millisecond}
	reg := registry.New()
	if err := reg.Register(prov); err != nil {
		t.Fatalf("Register: %v", err)
	}

	st := state.New("myapp", "dev")
	for i := 0; i < 8; i++ {
		name := fmt.Sprintf("r%d", i)
		st.Set(&resource.ResourceState{
			Address:    address.Address{Name: name},
			Type:       resourceType,
			ProviderID: name,
		})
	}

	obs, ds := Refresh(context.Background(), st, reg, 3)
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if len(obs) != 8 {
		t.Fatalf("Observations = %d, want 8", len(obs))
	}

	prov.mu.Lock()
	max := prov.maxConcurrent
	prov.mu.Unlock()
	if max > 3 {
		t.Errorf("max concurrent reads = %d, want at most the parallelism bound of 3", max)
	}
	if max < 2 {
		t.Errorf("max concurrent reads = %d, want at least 2 — reads should overlap, not run one at a time", max)
	}
}

func TestRefreshDiagnosticsAreSortedByAddressNotCompletionOrder(t *testing.T) {
	const resourceType = "delayed.thing"
	prov := &delayedProvider{
		resourceType: resourceType,
		delays: map[string]time.Duration{
			"zzz": 0,
			"aaa": 40 * time.Millisecond,
		},
		fail: map[string]bool{"zzz": true, "aaa": true},
	}
	reg := registry.New()
	if err := reg.Register(prov); err != nil {
		t.Fatalf("Register: %v", err)
	}

	st := state.New("myapp", "dev")
	for _, name := range []string{"zzz", "aaa"} {
		st.Set(&resource.ResourceState{Address: address.Address{Name: name}, Type: resourceType, ProviderID: name})
	}

	// zzz has no delay and fails almost immediately; aaa is deliberately
	// slower. If diagnostics reflected completion order, zzz would come
	// first despite sorting after aaa alphabetically.
	_, ds := Refresh(context.Background(), st, reg, 2)
	if len(ds) != 2 {
		t.Fatalf("got %d diagnostics, want 2", len(ds))
	}
	if !strings.Contains(ds[0].Summary, "aaa") || !strings.Contains(ds[1].Summary, "zzz") {
		t.Errorf("diagnostics = %+v, want aaa before zzz — sorted by address, not by which read finished first", ds)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/refresh/ -v`
Expected: FAIL — the package does not exist.

- [ ] **Step 3: Implement Refresh**

Create `internal/refresh/refresh.go`:

```go
// Package refresh reads every resource in state from its provider,
// concurrently, so the planner has current reality to diff configuration
// against. It never writes: spec §10 reserves persistence for the refresh
// command M3 adds, so plan stays safe to run against a locked environment,
// in CI, or repeatedly, and Compute (Task 13) stays a pure function of the
// inputs it is handed.
package refresh

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"infra/internal/diag"
	"infra/internal/registry"
	"infra/internal/state"
	"infra/pkg/address"
	"infra/pkg/resource"
)

// Observation is what Refresh learned about one resource recorded in state.
type Observation struct {
	// Address identifies the resource.
	Address address.Address
	// State is the provider's current report, or nil when the resource no
	// longer exists there — Read's (nil, nil) contract — or when Err is set.
	State *resource.ResourceState
	// Err is set when reading the resource failed. It is never a stand-in
	// for absence: a read error and a deleted resource are different facts,
	// and treating the first as the second would propose destroying
	// infrastructure that may well still be there.
	Err error
}

// Observations is what Refresh learned, keyed by Address.String().
type Observations map[string]Observation

// Refresh reads the current provider state of every resource recorded in
// st, concurrently, bounded by parallelism (values below 1 behave as 1).
//
// Refresh never writes to st or anywhere else: it is a pure read, and its
// result is meant to be used in memory and discarded, exactly as plan does.
// A read error becomes a diagnostic that fails planning for that resource;
// it is never treated as deletion. Results and diagnostics are assembled in
// address order regardless of which read finishes first, so two runs over
// the same state produce byte-identical output.
func Refresh(ctx context.Context, st *state.State, reg *registry.Registry, parallelism int) (Observations, diag.Diagnostics) {
	if parallelism < 1 {
		parallelism = 1
	}

	addrs := st.Addresses() // already sorted
	results := make([]Observation, len(addrs))
	problems := make([]diag.Diagnostics, len(addrs))

	sem := make(chan struct{}, parallelism)
	var wg sync.WaitGroup
	for i, addr := range addrs {
		i, addr := i, addr
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			results[i], problems[i] = readOne(ctx, st, reg, addr)
		}()
	}
	wg.Wait()

	out := make(Observations, len(addrs))
	var ds diag.Diagnostics
	for i, addr := range addrs {
		out[addr.String()] = results[i]
		ds.Extend(problems[i])
	}
	return out, ds
}

// readOne reads one resource's current provider state.
func readOne(ctx context.Context, st *state.State, reg *registry.Registry, addr address.Address) (Observation, diag.Diagnostics) {
	rs, ok := st.Get(addr)
	if !ok {
		// Unreachable in practice: addr always comes from st.Addresses().
		// Guarded rather than assumed, so a future caller that builds its
		// own address list cannot silently read garbage.
		var ds diag.Diagnostics
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  addr.String() + " is not in state",
			Related:  []address.Address{addr},
		})
		return Observation{Address: addr, Err: fmt.Errorf("%s: not in state", addr)}, ds
	}

	prov, ok := reg.Provider(rs.Type)
	if !ok {
		var ds diag.Diagnostics
		err := fmt.Errorf("%s: resource type %q is no longer registered", addr, rs.Type)
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "resource type " + strconv.Quote(rs.Type) + " is no longer registered",
			Detail: addr.String() + " is recorded in state as " + rs.Type +
				", but no provider registers that type.\nKnown types:\n  " + strings.Join(reg.Types(), "\n  "),
			Action:  "Restore the provider that registers " + rs.Type + ", or remove this resource from state once you have confirmed it is safe to.",
			Related: []address.Address{addr},
		})
		return Observation{Address: addr, Err: err}, ds
	}

	current, err := prov.Read(ctx, rs)
	if err != nil {
		var ds diag.Diagnostics
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "failed to read " + addr.String() + ": " + err.Error(),
			Detail:   "A read failure is a diagnostic, never a deletion: treating it as absence would propose destroying infrastructure that may still exist.",
			Related:  []address.Address{addr},
		})
		return Observation{Address: addr, Err: err}, ds
	}

	// current is nil exactly when the provider reports the resource no
	// longer exists — Read's (nil, nil) contract, and how deletion outside
	// infra is detected. It is not an error.
	return Observation{Address: addr, State: current}, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/refresh/ -v`
Expected: PASS — ten tests.

- [ ] **Step 5: Run under -race**

Run: `go test ./internal/refresh/ -race -v`
Expected: PASS, no data race reported. Every goroutine writes only to its own index of `results`/`problems`; the map assembly happens after `wg.Wait()` on the main goroutine. `-race` is what turns that claim into evidence rather than an assertion.

- [ ] **Step 6: Confirm Refresh never writes**

Run: `grep -n "\.Put(" internal/refresh/refresh.go`
Expected: no output. `TestRefreshNeverWritesState` proves this at the file-content level; this grep is the cheap, permanent guard against a future edit reintroducing a write — confirm it in your report.

- [ ] **Step 7: Commit**

```bash
git add internal/refresh
git commit -m "$(cat <<'EOF'
feat: refresh provider state concurrently without writing state

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_015M5QdCFKa6booSVrhnAzTV
EOF
)"
```

---
## Task 12: Plan and Operation types

**Files:**
- Create: `internal/planner/plan.go`
- Test: `internal/planner/plan_test.go`

**Interfaces:**
- Consumes: `address.Address` and its `String()`; `value.Value` and its `MarshalJSON`; `diag.Diagnostic`, `diag.Diagnostics`, `diag.Severity.String()` (all M1).
- Produces: `planner.PlanVersion = 1`; `planner.OpKind` with constants `OpNoOp`, `OpCreate`, `OpUpdate`, `OpReplace`, `OpDestroy`, `OpForget` and methods `String() string`, `Symbol() string`; `planner.ChangeReason{Attribute string; ForceNew bool; Note string}`; `planner.Operation{Address address.Address; Type string; Kind OpKind; Before, After map[string]value.Value; Reasons []ChangeReason; Dependents []address.Address}`; `planner.Plan{Version int; CreatedAt time.Time; Project, Environment string; ConfigHash string; StateSerial uint64; StateHash string; Operations []Operation; Diagnostics []diag.Diagnostic}` with `(*Plan).Canonical() ([]byte, error)`, `(*Plan).HasChanges() bool`, `(*Plan).Counts() map[OpKind]int` and `(Plan).MarshalJSON() ([]byte, error)`.

This task defines the plan artifact and nothing else. It makes no decisions about infrastructure; Task 13 does that. What it decides is what a plan *is* on the wire, and one of those decisions carries invariant 6 on its back.

**`Canonical()` excludes `CreatedAt`; `MarshalJSON` keeps it.** Invariant 6 requires that identical configuration, state and observations produce a byte-identical plan. A timestamp records when a plan was produced, which is not a property of its inputs, so two runs a second apart would differ in it and the invariant would be unsatisfiable as stated. Spec §12.1 was amended for exactly this: `Canonical()` is what determinism compares and what any plan fingerprint is taken over; `MarshalJSON` is the artifact a user saves and reads. Both must exist, and they must differ in exactly that one field. `TestCanonicalExcludesCreatedAt` pins both halves — if it ever becomes possible to satisfy it by removing `CreatedAt` from the struct entirely, the second half of the test fails.

**Both encoders sort operations by canonical address.** Spec §12.1 fixes the order as address order, never execution order — execution order belongs to the graph (§14) and depends on operation kind. `Compute` already emits them sorted, but sorting again inside the encoder is what makes the canonical form genuinely canonical: a plan assembled by hand, or by a future caller that appends, still serialises identically. The sort runs over a copy, so encoding a plan never mutates it.

**The wire format is written by hand.** `diag.Diagnostic` has no JSON tags and its `Severity` is a `uint8`, so marshalling it directly would put `"Severity":0` in a file M6 has to read back. `OpKind` is likewise a `uint8`. Explicit wire structs, plus `MarshalText` on `OpKind`, keep the artifact readable and stop a reordered constant from silently changing the meaning of every plan ever written — the same reasoning that gave `pkg/value` its frozen wire-name table. Reading a plan back is M6; the wire structs here are shaped so that adding `UnmarshalJSON` then is symmetric and does not change the format.

**The artifact keeps sensitive values.** Spec §12.2 is explicit: `plan --output` writes mode `0600` because the file contains sensitive values, which apply needs. Redaction is the renderer's job (Task 14), not the artifact's. `TestSensitiveValuesArePresentInTheArtifact` exists to stop a well-meaning reader from "fixing" this and breaking M6. What must never carry a value is a `ChangeReason` — reasons name attributes and types, never data — and Task 13 enforces that.

**`Operation` carries its dependents.** Spec §12.3 requires destructive operations rendered with dependent counts, and §20 fixes the wording: "This resource has 3 dependent resources." `Render`'s only inputs are the plan and its options, so a count absent from the plan is structurally unrecoverable and the renderer would have to either fabricate it or drop the warning. `Dependents` is derived from the same inputs as everything else in the plan, so it is stable, and it belongs in `Canonical()` for the same reason the operations do. Task 13 decides which side of the graph each kind's edges come from.

Three shape rules from spec §12.1 that Task 13 must honour and this task's encoder must express: `Before` is nil for Create, `After` is nil for Destroy and Forget, both are populated for NoOp, Update and Replace, and `After` may contain unknown values.

- [ ] **Step 1: Write the failing test**

Create `internal/planner/plan_test.go`:

```go
package planner

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"infra/internal/diag"
	"infra/pkg/address"
	"infra/pkg/value"
)

// addr and str are shared with planner_test.go; they are declared here because
// this is the first file of the package's tests.
func addr(name string) address.Address { return address.Address{Name: name} }

func str(s string) value.Value { return value.String(s, value.SourceExplicit) }

// samplePlan builds a plan whose operations are deliberately out of address
// order, so that any test comparing serialised output also exercises sorting.
func samplePlan(created time.Time) *Plan {
	return &Plan{
		Version:     PlanVersion,
		CreatedAt:   created,
		Project:     "myapp",
		Environment: "dev",
		ConfigHash:  "cfg-hash",
		StateSerial: 7,
		StateHash:   "state-hash",
		Operations: []Operation{
			{
				Address: addr("zebra"),
				Type:    "test.database",
				Kind:    OpUpdate,
				Before:  map[string]value.Value{"size": value.Int(10, value.SourceProvider)},
				After:   map[string]value.Value{"size": value.Int(20, value.SourceExplicit)},
				Reasons: []ChangeReason{{Attribute: "size"}},
			},
			{
				Address: addr("alpha"),
				Type:    "test.network",
				Kind:    OpCreate,
				After:   map[string]value.Value{"cidr": str("10.0.0.0/16")},
			},
		},
	}
}

func TestPlanVersionIsOne(t *testing.T) {
	if PlanVersion != 1 {
		t.Errorf("PlanVersion = %d, want 1 — bumping it is a format change with a migration", PlanVersion)
	}
}

func TestOpKindStringAndSymbol(t *testing.T) {
	cases := []struct {
		kind   OpKind
		name   string
		symbol string
	}{
		{OpNoOp, "noop", ""},
		{OpCreate, "create", "+"},
		{OpUpdate, "update", "~"},
		{OpReplace, "replace", "-/+"},
		{OpDestroy, "destroy", "-"},
		{OpForget, "forget", "="},
	}
	for _, tc := range cases {
		if got := tc.kind.String(); got != tc.name {
			t.Errorf("String() = %q, want %q", got, tc.name)
		}
		if got := tc.kind.Symbol(); got != tc.symbol {
			t.Errorf("%s.Symbol() = %q, want %q", tc.name, got, tc.symbol)
		}
	}
}

func TestCanonicalExcludesCreatedAt(t *testing.T) {
	// Invariant 6: identical inputs produce a byte-identical plan. CreatedAt
	// records when the plan was made, which is not one of its inputs, so the
	// canonical form omits it — spec §12.1.
	first := samplePlan(time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC))
	second := samplePlan(time.Date(2026, 9, 10, 12, 0, 1, 0, time.UTC))

	c1, err := first.Canonical()
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	c2, err := second.Canonical()
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	if !bytes.Equal(c1, c2) {
		t.Errorf("two plans made a second apart must have identical canonical forms:\n%s\n%s", c1, c2)
	}
	if strings.Contains(string(c1), "created_at") {
		t.Errorf("the canonical form must not carry a timestamp at all:\n%s", c1)
	}

	// And the saved artifact must keep it: this half of the test is what stops
	// the first half being satisfied by deleting the field.
	j1, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	j2, err := json.Marshal(second)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if bytes.Equal(j1, j2) {
		t.Error("MarshalJSON must keep CreatedAt — the saved artifact records when it was produced")
	}
	if !strings.Contains(string(j1), "created_at") {
		t.Errorf("the artifact must carry created_at:\n%s", j1)
	}
}

// TestWireFormatUsesNamesNotNumbers pins the artifact's readability at the
// place it can actually regress. TestOpKindStringAndSymbol covers String()
// directly, but the artifact does not call String() — it relies on
// encoding/json finding OpKind's MarshalText. Delete MarshalText and every
// String() test still passes while every plan file silently becomes
// {"kind":1}: unreadable to a human, and pinned to an iota ordering that a
// later inserted constant would renumber, reinterpreting every plan ever
// written. Severity is the same shape one field over.
func TestWireFormatUsesNamesNotNumbers(t *testing.T) {
	p := samplePlan(time.Now())
	// samplePlan carries no diagnostics, and a loop over an empty slice
	// asserts nothing — add one so the severity half of this test can fail.
	p.Diagnostics = []diag.Diagnostic{{
		Severity: diag.SeverityWarning,
		Summary:  "a warning, so severity has something to encode",
	}}

	out, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var wire struct {
		Operations []struct {
			Kind string `json:"kind"`
		} `json:"operations"`
		Diagnostics []struct {
			Severity string `json:"severity"`
		} `json:"diagnostics"`
	}
	// Decoding "kind" into a string fails outright if it was written as a
	// number, which is the regression this test exists to catch.
	if err := json.Unmarshal(out, &wire); err != nil {
		t.Fatalf("plan did not decode with string kinds and severities — the wire format regressed to numbers: %v\n%s", err, out)
	}
	if len(wire.Operations) == 0 || len(wire.Diagnostics) == 0 {
		t.Fatalf("need at least one operation and one diagnostic for this test to mean anything: %s", out)
	}
	for i, op := range wire.Operations {
		if op.Kind == "" {
			t.Errorf("operation %d has an empty kind: %s", i, out)
		}
	}
	for i, d := range wire.Diagnostics {
		if d.Severity == "" {
			t.Errorf("diagnostic %d has an empty severity: %s", i, out)
		}
	}
}

func TestCanonicalIsStableAcrossRepeatedCalls(t *testing.T) {
	p := samplePlan(time.Now())
	first, err := p.Canonical()
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	for i := 0; i < 20; i++ {
		next, err := p.Canonical()
		if err != nil {
			t.Fatalf("Canonical: %v", err)
		}
		if !bytes.Equal(first, next) {
			t.Fatalf("call %d differed; Go map iteration is randomised and every map->slice boundary must sort", i)
		}
	}
}

func TestCanonicalSortsOperationsByAddress(t *testing.T) {
	p := samplePlan(time.Now())
	data, err := p.Canonical()
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	alpha := strings.Index(string(data), `"alpha"`)
	zebra := strings.Index(string(data), `"zebra"`)
	if alpha < 0 || zebra < 0 {
		t.Fatalf("both addresses should appear:\n%s", data)
	}
	if alpha > zebra {
		t.Error("operations must serialise in canonical address order, not insertion order")
	}
	if p.Operations[0].Address.String() != "zebra" {
		t.Error("encoding must sort a copy, never reorder the plan it was given")
	}
}

func TestCreateOmitsBeforeAndDestroyOmitsAfter(t *testing.T) {
	p := &Plan{
		Version: PlanVersion,
		Operations: []Operation{
			{Address: addr("a"), Type: "test.network", Kind: OpCreate, After: map[string]value.Value{"cidr": str("10.0.0.0/16")}},
			{Address: addr("b"), Type: "test.network", Kind: OpDestroy, Before: map[string]value.Value{"cidr": str("10.1.0.0/16")}},
		},
	}
	data, err := p.Canonical()
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}

	var doc struct {
		Operations []map[string]json.RawMessage `json:"operations"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(doc.Operations) != 2 {
		t.Fatalf("got %d operations, want 2", len(doc.Operations))
	}
	if _, ok := doc.Operations[0]["before"]; ok {
		t.Error("a create has nothing before it; the key must be absent, not null")
	}
	if _, ok := doc.Operations[1]["after"]; ok {
		t.Error("a destroy has nothing after it; the key must be absent, not null")
	}
}

func TestUnknownValuesSurviveIntoTheArtifact(t *testing.T) {
	// After may contain unknowns — spec §12.1. They must round-trip as
	// "not yet known", never as a zero value.
	p := &Plan{
		Version: PlanVersion,
		Operations: []Operation{{
			Address: addr("db"), Type: "test.database", Kind: OpCreate,
			After: map[string]value.Value{"endpoint": value.Unknown(value.KindString, value.SourceProvider)},
		}},
	}
	data, err := p.Canonical()
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	if !strings.Contains(string(data), `"known":false`) {
		t.Errorf("an unknown value must serialise as unknown:\n%s", data)
	}
}

func TestSensitiveValuesArePresentInTheArtifact(t *testing.T) {
	// Deliberate, and load-bearing: spec §12.2 says the saved plan contains
	// sensitive values because apply needs them, which is why the CLI writes
	// it mode 0600. Redaction belongs to Render (Task 14), not here. Do not
	// "fix" this test by redacting the artifact — that breaks M6.
	p := &Plan{
		Version: PlanVersion,
		Operations: []Operation{{
			Address: addr("db"), Type: "test.database", Kind: OpCreate,
			After: map[string]value.Value{"password": str("hunter2").WithSensitive(true)},
		}},
	}
	data, err := p.Canonical()
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	if !strings.Contains(string(data), "hunter2") {
		t.Error("the plan artifact carries sensitive values; apply needs them")
	}
	if !strings.Contains(string(data), `"sensitive":true`) {
		t.Errorf("and it must carry the marking that tells the renderer to redact:\n%s", data)
	}
}

func TestDiagnosticsAreCarriedInTheArtifact(t *testing.T) {
	// A plan whose diagnostics contain an error is never applyable (spec
	// §12.2), so the severity has to survive serialisation legibly.
	p := &Plan{
		Version: PlanVersion,
		Diagnostics: []diag.Diagnostic{{
			Severity: diag.SeverityError,
			Summary:  "database is protected by prevent_destroy",
			Origin:   value.Origin{File: "infra.yml", Line: 12, Column: 3},
			Related:  []address.Address{addr("database")},
		}},
	}
	data, err := p.Canonical()
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	for _, want := range []string{`"severity":"Error"`, "prevent_destroy", "infra.yml:12:3", `"database"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("artifact is missing %q:\n%s", want, data)
		}
	}
}

func TestDependentsAreCarriedAndSortedInTheCanonicalForm(t *testing.T) {
	// Spec §12.3 needs the count to warn on a destructive change, and Render
	// sees nothing but the plan. Encoding sorts, so two plans over identical
	// inputs agree on the field whatever order they assembled it in.
	forward := &Plan{
		Version: PlanVersion,
		Operations: []Operation{{
			Address: addr("net"), Type: "test.network", Kind: OpDestroy,
			Before:     map[string]value.Value{"cidr": str("10.0.0.0/16")},
			Dependents: []address.Address{addr("alpha"), addr("middle"), addr("zebra")},
		}},
	}
	reversed := &Plan{
		Version: PlanVersion,
		Operations: []Operation{{
			Address: addr("net"), Type: "test.network", Kind: OpDestroy,
			Before:     map[string]value.Value{"cidr": str("10.0.0.0/16")},
			Dependents: []address.Address{addr("zebra"), addr("middle"), addr("alpha")},
		}},
	}

	a, err := forward.Canonical()
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	b, err := reversed.Canonical()
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Errorf("dependents must serialise sorted:\n%s\n%s", a, b)
	}
	if !strings.Contains(string(a), `"dependents":["alpha","middle","zebra"]`) {
		t.Errorf("the plan must carry its dependents; Render cannot recover them:\n%s", a)
	}
	if reversed.Operations[0].Dependents[0].String() != "zebra" {
		t.Error("encoding must sort a copy, never reorder the plan it was given")
	}
}

func TestHasChangesIgnoresNoOpsOnly(t *testing.T) {
	quiet := &Plan{Operations: []Operation{
		{Address: addr("a"), Kind: OpNoOp},
		{Address: addr("b"), Kind: OpNoOp},
	}}
	if quiet.HasChanges() {
		t.Error("a plan of nothing but NoOps has no changes — invariant 2")
	}

	// Forget counts. The provider is never called, but state changes, and
	// `infra plan` must exit ExitChanges so CI notices.
	forgetful := &Plan{Operations: []Operation{
		{Address: addr("a"), Kind: OpNoOp},
		{Address: addr("b"), Kind: OpForget},
	}}
	if !forgetful.HasChanges() {
		t.Error("a forget is a change: it rewrites state")
	}
}

func TestCounts(t *testing.T) {
	p := &Plan{Operations: []Operation{
		{Address: addr("a"), Kind: OpCreate},
		{Address: addr("b"), Kind: OpCreate},
		{Address: addr("c"), Kind: OpReplace},
		{Address: addr("d"), Kind: OpNoOp},
	}}
	counts := p.Counts()
	if counts[OpCreate] != 2 {
		t.Errorf("creates = %d, want 2", counts[OpCreate])
	}
	if counts[OpReplace] != 1 {
		t.Errorf("replaces = %d, want 1", counts[OpReplace])
	}
	if counts[OpDestroy] != 0 {
		t.Errorf("destroys = %d, want 0 — an absent kind reads as zero", counts[OpDestroy])
	}
}

func TestCanonicalOnNilPlanIsAnErrorNotAPanic(t *testing.T) {
	var p *Plan
	if _, err := p.Canonical(); err == nil {
		t.Error("encoding a nil plan must return an error")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/planner/ -v`
Expected: FAIL — the package does not exist; the build reports `undefined: Plan`, `undefined: Operation`, `undefined: OpKind`, `undefined: PlanVersion`.

- [ ] **Step 3: Implement the plan artifact**

Create `internal/planner/plan.go`:

```go
// Package planner decides what must change. It compares resolved
// configuration against recorded state and observed provider reality and
// produces a plan: one operation per resource, with the reasons for it.
//
// Everything in this package is a pure function of its inputs. It touches no
// filesystem, no network and no provider. That purity is what makes
// determinism (invariant 6) a property test rather than an aspiration.
package planner

import (
	"encoding/json"
	"errors"
	"sort"
	"time"

	"infra/internal/diag"
	"infra/pkg/address"
	"infra/pkg/value"
)

// PlanVersion is the schema version of the plan artifact this build writes.
const PlanVersion = 1

// OpKind is the operation a plan proposes for one resource.
type OpKind uint8

const (
	// OpNoOp means the resource already matches configuration.
	OpNoOp OpKind = iota
	// OpCreate means the resource will be created.
	OpCreate
	// OpUpdate means the resource will be changed in place.
	OpUpdate
	// OpReplace means a ForceNew attribute changed, so the resource must be
	// destroyed and recreated.
	OpReplace
	// OpDestroy means the resource will be deleted through its provider.
	OpDestroy
	// OpForget means the resource will be dropped from state without the
	// provider being called.
	OpForget
)

// String returns the operation's name, as written in the plan artifact.
func (k OpKind) String() string {
	switch k {
	case OpNoOp:
		return "noop"
	case OpCreate:
		return "create"
	case OpUpdate:
		return "update"
	case OpReplace:
		return "replace"
	case OpDestroy:
		return "destroy"
	case OpForget:
		return "forget"
	default:
		return "unknown"
	}
}

// Symbol returns the marker the renderer prefixes to the operation, per spec
// §12.3. NoOp has none: an unchanged resource is not marked.
func (k OpKind) Symbol() string {
	switch k {
	case OpCreate:
		return "+"
	case OpUpdate:
		return "~"
	case OpReplace:
		return "-/+"
	case OpDestroy:
		return "-"
	case OpForget:
		return "="
	default:
		return ""
	}
}

// MarshalText writes the operation kind as its name. The artifact is a
// versioned contract, and a numeric kind would change meaning the moment
// someone reordered the constants.
func (k OpKind) MarshalText() ([]byte, error) { return []byte(k.String()), nil }

// ChangeReason explains one attribute's contribution to an operation.
//
// It names attributes and types, never values: sensitivity is per-leaf, so
// even a composite that is not itself marked sensitive may contain a leaf that
// is, and a reason has no way to redact.
type ChangeReason struct {
	Attribute string `json:"attribute,omitempty"`
	// ForceNew records that this attribute is what promoted an update to a
	// replacement, so the plan can say which one forced it.
	ForceNew bool `json:"force_new,omitempty"`
	// Note carries a short explanation such as "known after apply".
	Note string `json:"note,omitempty"`
}

// Operation is the change proposed for a single resource.
//
// Before is nil for Create; After is nil for Destroy and Forget; both are
// populated for NoOp, Update and Replace. After may contain unknown values.
// Spec §12.1.
type Operation struct {
	Address address.Address
	Type    string
	Kind    OpKind
	Before  map[string]value.Value
	After   map[string]value.Value
	Reasons []ChangeReason
	// Dependents are the resources that depend on this one, sorted. Destroying
	// a resource with dependents is the case spec §20 wants called out loudly,
	// and the count is not recoverable from the plan without it.
	Dependents []address.Address
}

// Plan is what `infra plan` produces and `infra apply` consumes.
type Plan struct {
	Version     int
	CreatedAt   time.Time
	Project     string
	Environment string
	// ConfigHash fingerprints the resolved configuration this plan was made
	// from, so M6 can tell a saved plan has gone stale.
	ConfigHash string
	// StateSerial and StateHash fingerprint the state this plan was made
	// against, for the same reason.
	StateSerial uint64
	StateHash   string
	// Operations are sorted by canonical address, never by execution order:
	// execution order belongs to the graph and depends on operation kind.
	Operations  []Operation
	Diagnostics []diag.Diagnostic
}

// HasChanges reports whether the plan proposes anything at all.
//
// A Forget counts. The provider is never called, but state changes, and
// `infra plan` must exit with ExitChanges so that CI notices.
func (p *Plan) HasChanges() bool {
	if p == nil {
		return false
	}
	for _, op := range p.Operations {
		if op.Kind != OpNoOp {
			return true
		}
	}
	return false
}

// Counts returns how many operations there are of each kind. A kind that does
// not occur is absent from the map, which reads as zero.
func (p *Plan) Counts() map[OpKind]int {
	out := map[OpKind]int{}
	if p == nil {
		return out
	}
	for _, op := range p.Operations {
		out[op.Kind]++
	}
	return out
}

// Canonical returns the plan's deterministic form: identical inputs produce
// byte-identical output.
//
// CreatedAt is excluded. It records when the plan was produced, which is not a
// property of the plan's inputs, so including it would make invariant 6
// unsatisfiable — spec §12.1. This is what determinism compares and what any
// plan fingerprint is taken over.
func (p *Plan) Canonical() ([]byte, error) { return p.encode(false) }

// MarshalJSON writes the plan artifact a user saves and reads, timestamp
// included. The receiver is a value so that marshalling a Plan and a *Plan
// cannot produce two different formats.
func (p Plan) MarshalJSON() ([]byte, error) { return p.encode(true) }

// planWire is the artifact's on-disk shape. It is written by hand rather than
// derived from the structs so that what is and is not persisted is explicit
// and cannot drift when a struct gains a field.
type planWire struct {
	Version     int              `json:"version"`
	CreatedAt   *time.Time       `json:"created_at,omitempty"`
	Project     string           `json:"project"`
	Environment string           `json:"environment"`
	ConfigHash  string           `json:"config_hash"`
	StateSerial uint64           `json:"state_serial"`
	StateHash   string           `json:"state_hash"`
	Operations  []operationWire  `json:"operations"`
	Diagnostics []diagnosticWire `json:"diagnostics,omitempty"`
}

type operationWire struct {
	Address    string                 `json:"address"`
	Type       string                 `json:"type"`
	Kind       OpKind                 `json:"kind"`
	Before     map[string]value.Value `json:"before,omitempty"`
	After      map[string]value.Value `json:"after,omitempty"`
	Reasons    []ChangeReason         `json:"reasons,omitempty"`
	Dependents []string               `json:"dependents,omitempty"`
}

type diagnosticWire struct {
	Severity string   `json:"severity"`
	Summary  string   `json:"summary"`
	Detail   string   `json:"detail,omitempty"`
	Action   string   `json:"action,omitempty"`
	Origin   string   `json:"origin,omitempty"`
	Related  []string `json:"related,omitempty"`
}

// encode renders the plan, with or without its timestamp. Everything that
// crosses a map-to-slice boundary sorts: determinism is invariant 6.
func (p *Plan) encode(withTimestamp bool) ([]byte, error) {
	if p == nil {
		return nil, errors.New("cannot encode a nil plan")
	}

	// Sort a copy. The canonical form must be canonical however the plan was
	// assembled, and encoding must never reorder the caller's plan.
	ops := make([]Operation, len(p.Operations))
	copy(ops, p.Operations)
	sort.SliceStable(ops, func(i, j int) bool {
		return ops[i].Address.String() < ops[j].Address.String()
	})

	w := planWire{
		Version:     p.Version,
		Project:     p.Project,
		Environment: p.Environment,
		ConfigHash:  p.ConfigHash,
		StateSerial: p.StateSerial,
		StateHash:   p.StateHash,
		Operations:  make([]operationWire, 0, len(ops)),
	}
	if withTimestamp {
		created := p.CreatedAt
		w.CreatedAt = &created
	}

	for _, op := range ops {
		// Sort a copy of the dependents too, for the same reason: the
		// canonical form must be canonical however the plan was assembled.
		dependents := make([]address.Address, len(op.Dependents))
		copy(dependents, op.Dependents)
		address.Sort(dependents)

		entry := operationWire{
			Address: op.Address.String(),
			Type:    op.Type,
			Kind:    op.Kind,
			Before:  op.Before,
			After:   op.After,
			Reasons: op.Reasons,
		}
		for _, dependent := range dependents {
			entry.Dependents = append(entry.Dependents, dependent.String())
		}
		w.Operations = append(w.Operations, entry)
	}

	for _, d := range p.Diagnostics {
		entry := diagnosticWire{
			Severity: d.Severity.String(),
			Summary:  d.Summary,
			Detail:   d.Detail,
			Action:   d.Action,
		}
		if d.Origin.File != "" {
			entry.Origin = d.Origin.String()
		}
		for _, related := range d.Related {
			entry.Related = append(entry.Related, related.String())
		}
		w.Diagnostics = append(w.Diagnostics, entry)
	}

	return json.Marshal(w)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/planner/ -v`
Expected: PASS — fourteen tests.

- [ ] **Step 5: Commit**

```bash
git add internal/planner
git commit -m "feat: add the plan artifact with a canonical, timestamp-free form

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_015M5QdCFKa6booSVrhnAzTV"
```

---

## Task 13: The planner

**Files:**
- Create: `internal/planner/planner.go`, `internal/planner/diff.go`
- Test: `internal/planner/planner_test.go`

**Interfaces:**
- Consumes: `compiler.ResolvedConfig` with `Get`, `Addresses`, `Hash` (Task 5); `state.State` with `Get`, `Addresses`, `Encode`, `Serial`, `Environment`; `refresh.Observation{Address, State, Err}` and `refresh.Observations` (Task 11); `registry.Registry.Definition(string) (*schema.ResourceDefinition, bool)` and `Types() []string`; `schema.Attribute{Kind, Computed, ForceNew}`; `resource.ResourceState` (including `Dependencies`), `resource.ResolvedResource` (including `DependsOn`), `resource.Lifecycle`; `value.Value.Equal`; `diag.Diagnostics`; `address.Sort`; and Task 12's `Plan`, `Operation`, `OpKind`, `ChangeReason`, `PlanVersion`.
- Produces: `planner.Options{Environment string; Now func() time.Time; Registry *registry.Registry}`; `planner.Compute(cfg compiler.ResolvedConfig, st *state.State, obs refresh.Observations, opts Options) (*Plan, diag.Diagnostics)`.

`Compute` is a pure function of its four inputs. It reads no file, opens no socket and calls no provider — the reading was done by `Refresh` (Task 11), and the result was handed to it. That purity is what lets the determinism test below be a property test rather than a hope, and it is why `Options.Now` is injectable: a plan needs a timestamp, and a function that calls `time.Now` directly is not pure.

It is named `Compute` because `Plan` is the type.

**A deviation from the contract, stated openly: `Options` carries a `Registry`.** The contract fixes `Options{Environment string; Now func() time.Time}`, and `Compute`'s signature has no registry parameter. But `ResolvedConfig` carries resolved values, not the schema that governs them — Task 7's `bindSchemas` consults the registry and does not record what it learned — so with those two fields alone the planner cannot tell a `ForceNew` attribute from an updatable one, nor a computed attribute from desired state. Two of spec §11's five decision rules would be unimplementable, and the failure mode is the bad kind: replacements silently reported as updates, discovered at apply. `Compute`'s signature is unchanged; `Options` gains a third field, which Task 15 fills from the registry `cli.buildRegistry` already builds. A nil `Registry` is an error diagnostic, never a silent degradation.

**A read error fails planning for that resource; it is not absence.** An `Observation` with a non-nil `Err` means the provider could not be reached, which is not evidence that anything was deleted. Treating it as absence would propose recreating live infrastructure, or dropping a live resource from state. Spec §10 settles it: the resource gets an error diagnostic and no operation, and since a plan whose diagnostics contain an error is never applyable (§12.2), nothing can act on the gap.

**A missing observation is also not absence.** If an address in state has no entry in `Observations` at all, refresh simply did not cover it, so the recorded state stands in for the provider's view. The reasoning is the same as for a read error: only an explicit `Observation{State: nil}` means gone.

**Comparison goes through `value.Equal`, once.** It already ignores `Source`, `Sensitive` and `Origin` — provenance describes how a value was arrived at, not what the desired state is, so it must never show as a change — and an unknown is never equal to anything. A second comparison with different semantics somewhere in `diff.go` is how the two halves of the engine start disagreeing about what changed.

**Unknowns hide inside composites.** `value.Map` sets `Known: true` on the map even when one of its entries is unknown, so checking `desired.Known` alone would miss exactly the case that matters and fall through to `Equal`, which returns false — producing the right operation with a reason that fails to say why. `hasUnknown` recurses, and the "known after apply" check runs before `Equal` so that the reason is explicit rather than incidental. `test.database`'s `tags` map exists to be tested here.

**NoOp operations appear in `Operations`.** Invariant 2 says a plan with desired equal to actual contains zero operations; the contract defines `HasChanges()` as "any op that is not OpNoOp", which only means something if NoOps are in the list. They are, and invariant 2 is read as zero *changing* operations — which is what `HasChanges() == false` asserts, and what `--verbose` rendering (Task 14) needs in order to show unchanged resources at all.

**`retain` is checked before `prevent_destroy`.** A resource carrying both is forgotten, not refused: `retain` does not destroy anything, so it already satisfies what `prevent_destroy` protects. The lifecycle consulted is the one recorded in *state*, because the resource is by definition no longer in configuration — which is why `ResourceState` records it.

**Dependent edges come from whichever side actually has them.** For Destroy and Forget the resource is, by definition, no longer in configuration — so are some of the things that depended on it — and the only surviving record of those edges is `ResourceState.Dependencies` in state. Reading them from configuration would report zero dependents for exactly the operation spec §20 wants shouted about. For Create, Update, Replace and NoOp the resource is in configuration, and `ResolvedResource.DependsOn` is the current truth; state's edges there are stale by construction. Both sides sort with `address.Sort`.

**One known limitation, stated rather than hidden.** An attribute absent from configuration but present on the resource drives an Update only when the schema defines it and it is not computed. An attribute the schema does not define at all is the provider's business and is ignored. This is the literal reading of spec §11's "computed attributes absent from configuration never drive a diff" — the qualifier is load-bearing.

- [ ] **Step 1: Write the failing test**

Create `internal/planner/planner_test.go`:

```go
package planner

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"infra/internal/compiler"
	"infra/internal/diag"
	"infra/internal/refresh"
	"infra/internal/registry"
	"infra/internal/state"
	"infra/pkg/address"
	"infra/pkg/resource"
	"infra/pkg/value"
	testprovider "infra/providers/test"
)

// addr and str come from plan_test.go; do not redeclare them.

func testRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	reg := registry.New()
	if err := reg.Register(testprovider.New(t.TempDir() + "/fake-cloud.json")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return reg
}

// planOpts fixes the clock so that the artifact is reproducible. It is named
// planOpts rather than opts so it cannot shadow Compute's parameter.
func planOpts(t *testing.T) Options {
	t.Helper()
	return Options{
		Environment: "dev",
		Now:         func() time.Time { return time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC) },
		Registry:    testRegistry(t),
	}
}

func config(resources ...*resource.ResolvedResource) compiler.ResolvedConfig {
	c := compiler.ResolvedConfig{
		Project:     "myapp",
		Environment: "dev",
		Resources:   map[string]*resource.ResolvedResource{},
	}
	for _, r := range resources {
		c.Resources[r.Address.String()] = r
	}
	return c
}

func configured(name, typ string, attrs map[string]value.Value) *resource.ResolvedResource {
	return &resource.ResolvedResource{Address: addr(name), Type: typ, Attrs: attrs}
}

// recorded builds a state entry. Its attributes carry SourceProvider so that
// every comparison in these tests also proves Equal ignores provenance.
func recorded(name, typ string, attrs map[string]value.Value) *resource.ResourceState {
	return &resource.ResourceState{
		Address:    addr(name),
		Type:       typ,
		Provider:   "test",
		ProviderID: name + "-1",
		Attributes: attrs,
	}
}

func stateOf(resources ...*resource.ResourceState) *state.State {
	st := state.New("myapp", "dev")
	st.Serial = 7
	for _, r := range resources {
		st.Set(r)
	}
	return st
}

func present(resources ...*resource.ResourceState) refresh.Observations {
	obs := refresh.Observations{}
	for _, r := range resources {
		obs[r.Address.String()] = refresh.Observation{Address: r.Address, State: r}
	}
	return obs
}

func absent(addrs ...address.Address) refresh.Observations {
	obs := refresh.Observations{}
	for _, a := range addrs {
		obs[a.String()] = refresh.Observation{Address: a}
	}
	return obs
}

func merge(sets ...refresh.Observations) refresh.Observations {
	out := refresh.Observations{}
	for _, set := range sets {
		for k, v := range set {
			out[k] = v
		}
	}
	return out
}

func only(t *testing.T, p *Plan) Operation {
	t.Helper()
	if len(p.Operations) != 1 {
		t.Fatalf("expected exactly one operation, got %d: %+v", len(p.Operations), p.Operations)
	}
	return p.Operations[0]
}

func rendered(t *testing.T, ds diag.Diagnostics) string {
	t.Helper()
	var out strings.Builder
	ds.Render(&out)
	return out.String()
}

func provAttr(v value.Value) value.Value { return v.WithSource(value.SourceProvider) }

// --- one test per row of spec §11's decision table -------------------------

func TestInConfigNotInStateIsCreate(t *testing.T) {
	cfg := config(configured("net", "test.network", map[string]value.Value{
		"cidr": str("10.0.0.0/16"),
	}))
	p, ds := Compute(cfg, stateOf(), refresh.Observations{}, planOpts(t))
	if ds.HasErrors() {
		t.Fatalf("unexpected errors: %+v", ds)
	}

	op := only(t, p)
	if op.Kind != OpCreate {
		t.Fatalf("Kind = %s, want create", op.Kind)
	}
	if op.Before != nil {
		t.Error("Before must be nil for a create — spec §12.1")
	}
	if _, ok := op.After["cidr"]; !ok {
		t.Error("After must carry the configured attributes")
	}
	id, ok := op.After["id"]
	if !ok || id.Known {
		t.Error("a computed attribute must appear in After as unknown: it is known after apply")
	}
}

func TestNoDifferencesIsNoOp(t *testing.T) {
	cfg := config(configured("net", "test.network", map[string]value.Value{
		"cidr": str("10.0.0.0/16"),
	}))
	live := recorded("net", "test.network", map[string]value.Value{
		"cidr": provAttr(str("10.0.0.0/16")),
		"id":   provAttr(str("net-1")),
	})

	p, ds := Compute(cfg, stateOf(live), present(live), planOpts(t))
	if ds.HasErrors() {
		t.Fatalf("unexpected errors: %+v", ds)
	}

	op := only(t, p)
	if op.Kind != OpNoOp {
		t.Fatalf("Kind = %s, want noop (reasons: %+v)", op.Kind, op.Reasons)
	}
	if len(op.Reasons) != 0 {
		t.Errorf("a noop has no reasons, got %+v", op.Reasons)
	}
	if p.HasChanges() {
		t.Error("invariant 2: desired equals actual, so the plan proposes no changes")
	}
}

func TestUpdatableDifferenceIsUpdate(t *testing.T) {
	cfg := config(configured("db", "test.database", map[string]value.Value{
		"engine": str("postgres"),
		"size":   value.Int(20, value.SourceExplicit),
	}))
	live := recorded("db", "test.database", map[string]value.Value{
		"engine":   provAttr(str("postgres")),
		"size":     value.Int(10, value.SourceProvider),
		"endpoint": provAttr(str("db-1.test")),
	})

	p, _ := Compute(cfg, stateOf(live), present(live), planOpts(t))
	op := only(t, p)
	if op.Kind != OpUpdate {
		t.Fatalf("Kind = %s, want update", op.Kind)
	}
	if len(op.Reasons) != 1 || op.Reasons[0].Attribute != "size" {
		t.Fatalf("Reasons = %+v, want one naming size", op.Reasons)
	}
	if op.Reasons[0].ForceNew {
		t.Error("size is updatable in place; it must not be marked ForceNew")
	}
	if op.Before == nil || op.After == nil {
		t.Error("an update populates both Before and After — spec §12.1")
	}
}

func TestForceNewDifferenceIsReplace(t *testing.T) {
	// engine is ForceNew on test.database.
	cfg := config(configured("db", "test.database", map[string]value.Value{
		"engine": str("mysql"),
		"size":   value.Int(10, value.SourceExplicit),
	}))
	live := recorded("db", "test.database", map[string]value.Value{
		"engine":   provAttr(str("postgres")),
		"size":     value.Int(10, value.SourceProvider),
		"endpoint": provAttr(str("db-1.test")),
	})

	p, _ := Compute(cfg, stateOf(live), present(live), planOpts(t))
	op := only(t, p)
	if op.Kind != OpReplace {
		t.Fatalf("Kind = %s, want replace — a changed ForceNew attribute promotes update to replace", op.Kind)
	}
	var forced *ChangeReason
	for i := range op.Reasons {
		if op.Reasons[i].ForceNew {
			forced = &op.Reasons[i]
		}
	}
	if forced == nil {
		t.Fatalf("Reasons = %+v, want one marked ForceNew so the plan can say what forced the replacement", op.Reasons)
	}
	if forced.Attribute != "engine" {
		t.Errorf("forcing attribute = %q, want engine", forced.Attribute)
	}
}

func TestObservedAbsentIsRecreate(t *testing.T) {
	// In config, in state, but the provider no longer has it: something
	// deleted it outside infra.
	cfg := config(configured("net", "test.network", map[string]value.Value{
		"cidr": str("10.0.0.0/16"),
	}))
	live := recorded("net", "test.network", map[string]value.Value{
		"cidr": provAttr(str("10.0.0.0/16")),
	})

	p, ds := Compute(cfg, stateOf(live), absent(addr("net")), planOpts(t))
	if ds.HasErrors() {
		t.Fatalf("unexpected errors: %+v", ds)
	}
	op := only(t, p)
	if op.Kind != OpCreate {
		t.Fatalf("Kind = %s, want create — a resource deleted outside infra is recreated", op.Kind)
	}
	if op.Before != nil {
		t.Error("Before must be nil for a create, recreation included")
	}
	if len(op.Reasons) == 0 || !strings.Contains(op.Reasons[0].Note, "recreated") {
		t.Errorf("Reasons = %+v, want one explaining the recreation", op.Reasons)
	}
}

func TestNotInConfigButPresentIsDestroy(t *testing.T) {
	live := recorded("net", "test.network", map[string]value.Value{
		"cidr": provAttr(str("10.0.0.0/16")),
	})

	p, ds := Compute(config(), stateOf(live), present(live), planOpts(t))
	if ds.HasErrors() {
		t.Fatalf("unexpected errors: %+v", ds)
	}
	op := only(t, p)
	if op.Kind != OpDestroy {
		t.Fatalf("Kind = %s, want destroy — invariant 1", op.Kind)
	}
	if op.After != nil {
		t.Error("After must be nil for a destroy — spec §12.1")
	}
	if _, ok := op.Before["cidr"]; !ok {
		t.Error("Before must carry what is about to be destroyed")
	}
}

func TestNotInConfigAndAbsentIsForget(t *testing.T) {
	live := recorded("net", "test.network", map[string]value.Value{
		"cidr": provAttr(str("10.0.0.0/16")),
	})

	p, ds := Compute(config(), stateOf(live), absent(addr("net")), planOpts(t))
	if ds.HasErrors() {
		t.Fatalf("unexpected errors: %+v", ds)
	}
	op := only(t, p)
	if op.Kind != OpForget {
		t.Fatalf("Kind = %s, want forget — it is already gone, so only the state entry remains", op.Kind)
	}
	if op.After != nil {
		t.Error("After must be nil for a forget")
	}
}

// --- one test per rule that is a decision rather than a mechanic -----------

func TestUnknownDesiredValueIsAnUpdate(t *testing.T) {
	// Rule 1. An unknown cannot be proven unchanged, so it must show as a
	// change; treating it as unchanged under-reports, invisibly until apply.
	cfg := config(configured("db", "test.database", map[string]value.Value{
		"engine":  str("postgres"),
		"network": value.Unknown(value.KindString, value.SourceComputed),
	}))
	live := recorded("db", "test.database", map[string]value.Value{
		"engine":  provAttr(str("postgres")),
		"network": provAttr(str("net-1")),
	})

	p, _ := Compute(cfg, stateOf(live), present(live), planOpts(t))
	op := only(t, p)
	if op.Kind != OpUpdate {
		t.Fatalf("Kind = %s, want update", op.Kind)
	}
	if len(op.Reasons) != 1 || op.Reasons[0].Attribute != "network" {
		t.Fatalf("Reasons = %+v, want one naming network", op.Reasons)
	}
	if op.Reasons[0].Note != "known after apply" {
		t.Errorf("Note = %q, want \"known after apply\" — the reason must say why, not merely that", op.Reasons[0].Note)
	}
}

func TestUnknownInsideACompositeIsAnUpdate(t *testing.T) {
	// Rule 1, the case that is easy to miss: a map is Known even when one of
	// its entries is not, so a top-level check alone reports the wrong reason.
	cfg := config(configured("db", "test.database", map[string]value.Value{
		"engine": str("postgres"),
		"tags": value.Map(map[string]value.Value{
			"env":     str("dev"),
			"release": value.Unknown(value.KindString, value.SourceComputed),
		}, value.SourceExplicit),
	}))
	live := recorded("db", "test.database", map[string]value.Value{
		"engine": provAttr(str("postgres")),
		"tags": value.Map(map[string]value.Value{
			"env":     provAttr(str("dev")),
			"release": provAttr(str("v1")),
		}, value.SourceProvider),
	})

	p, _ := Compute(cfg, stateOf(live), present(live), planOpts(t))
	op := only(t, p)
	if op.Kind != OpUpdate {
		t.Fatalf("Kind = %s, want update", op.Kind)
	}
	if len(op.Reasons) != 1 || op.Reasons[0].Attribute != "tags" {
		t.Fatalf("Reasons = %+v, want one naming tags", op.Reasons)
	}
	if op.Reasons[0].Note != "known after apply" {
		t.Errorf("Note = %q, want \"known after apply\" — the unknown check must recurse into composites", op.Reasons[0].Note)
	}
}

func TestComputedAttributesDoNotDriveADiff(t *testing.T) {
	// Rule 2. id and endpoint are provider outputs, not desired state.
	cfg := config(configured("db", "test.database", map[string]value.Value{
		"engine": str("postgres"),
	}))
	live := recorded("db", "test.database", map[string]value.Value{
		"engine":   provAttr(str("postgres")),
		"endpoint": provAttr(str("db-1.test")),
	})

	p, _ := Compute(cfg, stateOf(live), present(live), planOpts(t))
	op := only(t, p)
	if op.Kind != OpNoOp {
		t.Fatalf("Kind = %s, want noop — a computed attribute absent from configuration is an output (reasons: %+v)", op.Kind, op.Reasons)
	}
	if got, ok := op.After["endpoint"]; !ok || !got.Known {
		t.Error("an in-place operation carries the computed attribute across; it survives")
	}
}

func TestRemovedAttributeIsAnUpdate(t *testing.T) {
	// The other half of rule 2: an attribute the schema defines, that is not
	// computed, and that configuration no longer sets, is a change.
	cfg := config(configured("db", "test.database", map[string]value.Value{
		"engine": str("postgres"),
	}))
	live := recorded("db", "test.database", map[string]value.Value{
		"engine":   provAttr(str("postgres")),
		"password": provAttr(str("s3cret")).WithSensitive(true),
		"endpoint": provAttr(str("db-1.test")),
	})

	p, _ := Compute(cfg, stateOf(live), present(live), planOpts(t))
	op := only(t, p)
	if op.Kind != OpUpdate {
		t.Fatalf("Kind = %s, want update", op.Kind)
	}
	if len(op.Reasons) != 1 || op.Reasons[0].Attribute != "password" {
		t.Fatalf("Reasons = %+v, want exactly one naming password — endpoint is computed and must not appear", op.Reasons)
	}
}

func TestReplaceMarksComputedAttributesUnknown(t *testing.T) {
	// Rule 3's consequence: a replacement builds a new object, so the
	// provider's outputs are known after apply rather than carried across.
	cfg := config(configured("db", "test.database", map[string]value.Value{
		"engine": str("mysql"),
	}))
	live := recorded("db", "test.database", map[string]value.Value{
		"engine":   provAttr(str("postgres")),
		"endpoint": provAttr(str("db-1.test")),
	})

	p, _ := Compute(cfg, stateOf(live), present(live), planOpts(t))
	op := only(t, p)
	if op.Kind != OpReplace {
		t.Fatalf("Kind = %s, want replace", op.Kind)
	}
	endpoint, ok := op.After["endpoint"]
	if !ok {
		t.Fatal("After must mention the computed attribute")
	}
	if endpoint.Known {
		t.Error("a replacement re-derives computed attributes; they are known after apply, not carried across")
	}
}

func TestPreventDestroyIsAPlanTimeError(t *testing.T) {
	// Rule 4. The user learns before approving, not after.
	live := recorded("db", "test.database", map[string]value.Value{
		"engine": provAttr(str("postgres")),
	})
	live.Lifecycle = resource.Lifecycle{PreventDestroy: true}

	p, ds := Compute(config(), stateOf(live), present(live), planOpts(t))
	if !ds.HasErrors() {
		t.Fatal("removing a prevent_destroy resource from configuration must be an error at plan time")
	}
	if len(p.Operations) != 0 {
		t.Errorf("the plan must not contain an operation the engine has refused: %+v", p.Operations)
	}
	out := rendered(t, ds)
	if !strings.Contains(out, "prevent_destroy") || !strings.Contains(out, "db") {
		t.Errorf("the diagnostic must name the guard and the resource:\n%s", out)
	}
}

func TestRetainForgetsWithoutDestroying(t *testing.T) {
	// Rule 5. The provider is never called.
	live := recorded("db", "test.database", map[string]value.Value{
		"engine": provAttr(str("postgres")),
	})
	live.Lifecycle = resource.Lifecycle{Retain: true}

	p, ds := Compute(config(), stateOf(live), present(live), planOpts(t))
	if ds.HasErrors() {
		t.Fatalf("retain is not an error: %+v", ds)
	}
	op := only(t, p)
	if op.Kind == OpDestroy {
		t.Fatal("retain must not destroy the resource")
	}
	if op.Kind != OpForget {
		t.Fatalf("Kind = %s, want forget", op.Kind)
	}
	if len(op.Reasons) == 0 || !strings.Contains(op.Reasons[0].Note, "without calling the provider") {
		t.Errorf("Reasons = %+v, want one saying the provider is not called", op.Reasons)
	}
}

func TestRetainWinsOverPreventDestroy(t *testing.T) {
	// A resource carrying both is forgotten, not refused: retain destroys
	// nothing, so it already satisfies what prevent_destroy protects.
	live := recorded("db", "test.database", map[string]value.Value{
		"engine": provAttr(str("postgres")),
	})
	live.Lifecycle = resource.Lifecycle{Retain: true, PreventDestroy: true}

	p, ds := Compute(config(), stateOf(live), present(live), planOpts(t))
	if ds.HasErrors() {
		t.Fatalf("retain satisfies prevent_destroy; this must not error: %+v", ds)
	}
	if op := only(t, p); op.Kind != OpForget {
		t.Errorf("Kind = %s, want forget", op.Kind)
	}
}

// --- judgement calls the signature forces ----------------------------------

func TestReadErrorFailsPlanningRatherThanAssumingAbsence(t *testing.T) {
	inConfig := recorded("net", "test.network", map[string]value.Value{
		"cidr": provAttr(str("10.0.0.0/16")),
	})
	orphan := recorded("db", "test.database", map[string]value.Value{
		"engine": provAttr(str("postgres")),
	})
	cfg := config(configured("net", "test.network", map[string]value.Value{
		"cidr": str("10.0.0.0/16"),
	}))

	obs := refresh.Observations{
		"net": {Address: addr("net"), Err: errors.New("connection refused")},
		"db":  {Address: addr("db"), Err: errors.New("connection refused")},
	}

	p, ds := Compute(cfg, stateOf(inConfig, orphan), obs, planOpts(t))
	if !ds.HasErrors() {
		t.Fatal("a read error must fail planning for that resource")
	}
	if len(p.Operations) != 0 {
		t.Errorf("a resource that could not be read gets no operation; a failed read is not evidence of deletion: %+v", p.Operations)
	}
	out := rendered(t, ds)
	if !strings.Contains(out, "connection refused") {
		t.Errorf("the diagnostic must carry the provider's error:\n%s", out)
	}
}

func TestMissingObservationFallsBackToRecordedState(t *testing.T) {
	// Refresh not having covered an address is not evidence of absence.
	cfg := config(configured("net", "test.network", map[string]value.Value{
		"cidr": str("10.0.0.0/16"),
	}))
	live := recorded("net", "test.network", map[string]value.Value{
		"cidr": provAttr(str("10.0.0.0/16")),
	})

	p, ds := Compute(cfg, stateOf(live), refresh.Observations{}, planOpts(t))
	if ds.HasErrors() {
		t.Fatalf("unexpected errors: %+v", ds)
	}
	if op := only(t, p); op.Kind != OpNoOp {
		t.Errorf("Kind = %s, want noop — with no observation the recorded state stands in", op.Kind)
	}
}

func TestChangeReasonsNeverCarryValues(t *testing.T) {
	cfg := config(configured("db", "test.database", map[string]value.Value{
		"engine":   str("postgres"),
		"password": str("hunter2").WithSensitive(true),
	}))
	live := recorded("db", "test.database", map[string]value.Value{
		"engine":   provAttr(str("postgres")),
		"password": provAttr(str("s3cret")).WithSensitive(true),
	})

	p, _ := Compute(cfg, stateOf(live), present(live), planOpts(t))
	op := only(t, p)
	if op.Kind != OpUpdate {
		t.Fatalf("Kind = %s, want update", op.Kind)
	}
	for _, r := range op.Reasons {
		for _, leak := range []string{"hunter2", "s3cret"} {
			if strings.Contains(r.Attribute+" "+r.Note, leak) {
				t.Errorf("reason %+v leaks a value; reasons name attributes, never data", r)
			}
		}
	}
	if len(op.Reasons) != 1 || op.Reasons[0].Attribute != "password" {
		t.Errorf("Reasons = %+v, want one naming password", op.Reasons)
	}
}

func TestMissingSchemaIsAnErrorNotASilentUpdate(t *testing.T) {
	cfg := config(configured("thing", "aws.rds", map[string]value.Value{
		"engine": str("postgres"),
	}))
	p, ds := Compute(cfg, stateOf(), refresh.Observations{}, planOpts(t))
	if !ds.HasErrors() {
		t.Fatal("without a schema the planner cannot tell ForceNew from updatable; it must say so")
	}
	if len(p.Operations) != 0 {
		t.Errorf("no operation may be proposed for a type the planner cannot reason about: %+v", p.Operations)
	}
}

func TestNilRegistryIsAnErrorNotADegradation(t *testing.T) {
	opts := planOpts(t)
	opts.Registry = nil
	cfg := config(configured("net", "test.network", map[string]value.Value{
		"cidr": str("10.0.0.0/16"),
	}))
	if _, ds := Compute(cfg, stateOf(), refresh.Observations{}, opts); !ds.HasErrors() {
		t.Error("planning without a registry must fail loudly, not quietly report every replacement as an update")
	}
}

func TestPlanningAgainstAnotherEnvironmentsStateIsAnError(t *testing.T) {
	cfg := config(configured("net", "test.network", map[string]value.Value{
		"cidr": str("10.0.0.0/16"),
	}))
	prod := state.New("myapp", "prod")

	_, ds := Compute(cfg, prod, refresh.Observations{}, planOpts(t))
	if !ds.HasErrors() {
		t.Fatal("planning one environment's configuration against another's state must be refused")
	}
	out := rendered(t, ds)
	if !strings.Contains(out, "prod") || !strings.Contains(out, "dev") {
		t.Errorf("the diagnostic must name both environments:\n%s", out)
	}
}

func TestComputeDoesNotMutateItsInputs(t *testing.T) {
	live := recorded("db", "test.database", map[string]value.Value{
		"engine": provAttr(str("postgres")),
	})
	st := stateOf(live)
	before, err := st.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	cfg := config(configured("db", "test.database", map[string]value.Value{
		"engine": str("mysql"),
	}))
	p, _ := Compute(cfg, st, present(live), planOpts(t))

	// Mutating the plan must not reach back into state.
	only(t, p).Before["engine"] = str("tampered")

	after, err := st.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("planning must never mutate the state it was given:\n%s\n%s", before, after)
	}
}

func TestPlanRecordsItsInputFingerprints(t *testing.T) {
	cfg := config(configured("net", "test.network", map[string]value.Value{
		"cidr": str("10.0.0.0/16"),
	}))
	st := stateOf()

	p, _ := Compute(cfg, st, refresh.Observations{}, planOpts(t))
	if p.Version != PlanVersion {
		t.Errorf("Version = %d, want %d", p.Version, PlanVersion)
	}
	if p.Project != "myapp" || p.Environment != "dev" {
		t.Errorf("plan identifies itself as %s/%s", p.Project, p.Environment)
	}
	if p.ConfigHash == "" {
		t.Error("ConfigHash must be populated so M6 can detect a stale plan")
	}
	if p.StateHash == "" {
		t.Error("StateHash must be populated for the same reason")
	}
	if p.StateSerial != st.Serial {
		t.Errorf("StateSerial = %d, want %d", p.StateSerial, st.Serial)
	}
	if !p.CreatedAt.Equal(time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("CreatedAt = %s; the clock is injected so plans are reproducible", p.CreatedAt)
	}
}

// --- dependent counts, for the destructive-change warning ------------------

func TestDestroyReportsDependentsFromState(t *testing.T) {
	// The case the state-side lookup exists for: the dependent is not in
	// configuration either, so a config-side implementation reports zero and
	// the warning spec §20 requires silently disappears.
	net := recorded("net", "test.network", map[string]value.Value{
		"cidr": provAttr(str("10.0.0.0/16")),
	})
	db := recorded("db", "test.database", map[string]value.Value{
		"engine": provAttr(str("postgres")),
	})
	db.Dependencies = []address.Address{addr("net")}

	p, ds := Compute(config(), stateOf(net, db), present(net, db), planOpts(t))
	if ds.HasErrors() {
		t.Fatalf("unexpected errors: %+v", ds)
	}

	var destroyNet *Operation
	for i := range p.Operations {
		if p.Operations[i].Address.String() == "net" {
			destroyNet = &p.Operations[i]
		}
	}
	if destroyNet == nil {
		t.Fatalf("no operation for net: %+v", p.Operations)
	}
	if destroyNet.Kind != OpDestroy {
		t.Fatalf("Kind = %s, want destroy", destroyNet.Kind)
	}
	if len(destroyNet.Dependents) != 1 || destroyNet.Dependents[0].String() != "db" {
		t.Errorf("Dependents = %v, want [db] — a destroy's edges live in state, not configuration", destroyNet.Dependents)
	}
}

func TestConfiguredOperationsReportDependentsFromConfigSorted(t *testing.T) {
	db := configured("db", "test.database", map[string]value.Value{
		"engine": str("mysql"),
	})
	var dependents []*resource.ResolvedResource
	for _, name := range []string{"zebra-app", "alpha-app", "middle-app"} {
		app := configured(name, "test.application", map[string]value.Value{
			"image": str("app:1"),
		})
		app.DependsOn = []address.Address{addr("db")}
		dependents = append(dependents, app)
	}

	live := recorded("db", "test.database", map[string]value.Value{
		"engine": provAttr(str("postgres")),
	})

	cfg := config(append(dependents, db)...)
	p, ds := Compute(cfg, stateOf(live), present(live), planOpts(t))
	if ds.HasErrors() {
		t.Fatalf("unexpected errors: %+v", ds)
	}

	for _, op := range p.Operations {
		if op.Address.String() != "db" {
			continue
		}
		if op.Kind != OpReplace {
			t.Fatalf("Kind = %s, want replace", op.Kind)
		}
		want := []string{"alpha-app", "middle-app", "zebra-app"}
		if len(op.Dependents) != len(want) {
			t.Fatalf("Dependents = %v, want %v", op.Dependents, want)
		}
		for i := range want {
			if op.Dependents[i].String() != want[i] {
				t.Fatalf("Dependents = %v, want %v — sorted, every map->slice boundary sorts", op.Dependents, want)
			}
		}
		return
	}
	t.Fatalf("no operation for db: %+v", p.Operations)
}

func TestResourcesWithoutDependentsReportNone(t *testing.T) {
	cfg := config(configured("net", "test.network", map[string]value.Value{
		"cidr": str("10.0.0.0/16"),
	}))
	p, _ := Compute(cfg, stateOf(), refresh.Observations{}, planOpts(t))
	if got := only(t, p).Dependents; len(got) != 0 {
		t.Errorf("Dependents = %v, want none", got)
	}
}

// --- invariant 6 -----------------------------------------------------------

func TestPlanIsDeterministicAcrossTwentyRuns(t *testing.T) {
	// Invariant 6: identical configuration, state and observations produce an
	// equivalent plan. The clock deliberately advances on every call, so this
	// also proves the canonical form does not depend on CreatedAt.
	var (
		desired []*resource.ResolvedResource
		live    []*resource.ResourceState
		gone    []address.Address
	)

	// Creates.
	for _, name := range []string{"net-a", "net-b", "net-c"} {
		desired = append(desired, configuredNetwork(name, "10.0.0.0/16"))
	}
	// NoOps.
	for _, name := range []string{"keep-a", "keep-b", "keep-c"} {
		desired = append(desired, configuredNetwork(name, "10.1.0.0/16"))
		live = append(live, recordedNetwork(name, "10.1.0.0/16"))
	}
	// Updates and replacements.
	for i, name := range []string{"db-a", "db-b", "db-c", "db-d"} {
		engine, size := "postgres", int64(20)
		if i%2 == 0 {
			engine = "mysql" // ForceNew: a replacement
		}
		desired = append(desired, &resource.ResolvedResource{
			Address:   addr(name),
			Type:      "test.database",
			DependsOn: []address.Address{addr("keep-a")},
			Attrs: map[string]value.Value{
				"engine": str(engine),
				"size":   value.Int(size, value.SourceExplicit),
				"tags": value.Map(map[string]value.Value{
					"env":  str("dev"),
					"team": str("platform"),
				}, value.SourceExplicit),
			},
		})
		live = append(live, &resource.ResourceState{
			Address:      addr(name),
			Type:         "test.database",
			Provider:     "test",
			ProviderID:   name + "-1",
			Dependencies: []address.Address{addr("keep-a")},
			Attributes: map[string]value.Value{
				"engine":   provAttr(str("postgres")),
				"size":     value.Int(10, value.SourceProvider),
				"endpoint": provAttr(str(name + ".test")),
				"tags": value.Map(map[string]value.Value{
					"env":  provAttr(str("dev")),
					"team": provAttr(str("platform")),
				}, value.SourceProvider),
			},
		})
	}
	// Destroys and forgets.
	for _, name := range []string{"old-a", "old-b"} {
		live = append(live, recordedNetwork(name, "10.9.0.0/16"))
	}
	forgotten := recordedNetwork("old-c", "10.9.0.0/16")
	live = append(live, forgotten)
	gone = append(gone, forgotten.Address)

	cfg := config(desired...)
	st := stateOf(live...)
	obs := merge(present(live...), absent(gone...))

	opts := planOpts(t)
	tick := 0
	opts.Now = func() time.Time {
		tick++
		return time.Date(2026, 9, 10, 12, 0, tick, 0, time.UTC)
	}

	first, ds := Compute(cfg, st, obs, opts)
	if ds.HasErrors() {
		t.Fatalf("unexpected errors: %+v", ds)
	}
	if !first.HasChanges() {
		t.Fatal("the fixture should propose changes")
	}
	want, err := first.Canonical()
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}

	for i := 0; i < 20; i++ {
		next, _ := Compute(cfg, st, obs, opts)
		got, err := next.Canonical()
		if err != nil {
			t.Fatalf("Canonical: %v", err)
		}
		if !bytes.Equal(want, got) {
			t.Fatalf("run %d produced a different plan; invariant 6 requires byte-identical output\nwant: %s\ngot:  %s", i, want, got)
		}
		if len(next.Operations) != len(first.Operations) {
			t.Fatalf("run %d produced %d operations, want %d", i, len(next.Operations), len(first.Operations))
		}
	}
}

func configuredNetwork(name, cidr string) *resource.ResolvedResource {
	return configured(name, "test.network", map[string]value.Value{"cidr": str(cidr)})
}

func recordedNetwork(name, cidr string) *resource.ResourceState {
	return recorded(name, "test.network", map[string]value.Value{
		"cidr": provAttr(str(cidr)),
		"id":   provAttr(str(name + "-1")),
	})
}
```

Two naming notes for whoever types this in. `addr` and `str` are declared in `plan_test.go` (Task 12) and reused here — Go shares them across files in one package, so do not redeclare them. And the fixture builder is `planOpts`, not `opts`: a package-level `opts` would be shadowed by `Compute`'s parameter of that name, which compiles but reads as a trap.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/planner/ -run 'TestInConfig|TestNoDifferences|TestUpdatable|TestForceNew|TestObserved|TestNotInConfig|TestUnknown|TestComputed|TestRemoved|TestReplace|TestPrevent|TestRetain|TestRead|TestMissing|TestChangeReasons|TestNil|TestPlanning|TestCompute|TestPlanRecords|TestDestroyReports|TestConfiguredOperations|TestResourcesWithout|TestPlanIsDeterministic' -v`
Expected: FAIL — `undefined: Compute`, `undefined: Options`, and `unknown field Registry in struct literal`.

- [ ] **Step 3: Implement the diff**

Create `internal/planner/diff.go`:

```go
package planner

import (
	"fmt"
	"sort"

	"infra/pkg/schema"
	"infra/pkg/value"
)

// diffAttributes reports every attribute that differs between desired
// configuration and the resource as the provider reports it.
//
// It walks the union of both maps so that an attribute deleted from
// configuration still registers as a change, and consults the schema so that
// computed attributes — which are outputs, not desired state — never do
// (spec §11).
func diffAttributes(def *schema.ResourceDefinition, desired, actual map[string]value.Value) []ChangeReason {
	var reasons []ChangeReason

	for _, name := range unionKeys(desired, actual) {
		attr, defined := def.Attribute(name)
		want, inConfig := desired[name]
		got, inActual := actual[name]

		if !inConfig {
			// The provider reports it but configuration does not set it. A
			// computed attribute is an output; an attribute the schema does
			// not define at all is the provider's own business.
			if !defined || attr.Computed {
				continue
			}
			reasons = append(reasons, ChangeReason{
				Attribute: name,
				ForceNew:  attr.ForceNew,
				Note:      "removed from configuration",
			})
			continue
		}

		// An unknown desired value cannot be proven unchanged, so it is always
		// a change. This runs before Equal — which would also return false —
		// so that the reason says why rather than merely that. Unknowns hide
		// inside composites, so the check recurses.
		if hasUnknown(want) {
			reasons = append(reasons, ChangeReason{
				Attribute: name,
				ForceNew:  attr.ForceNew,
				Note:      "known after apply",
			})
			continue
		}

		if !inActual {
			reasons = append(reasons, ChangeReason{
				Attribute: name,
				ForceNew:  attr.ForceNew,
				Note:      "not set on the resource",
			})
			continue
		}

		// value.Equal already ignores Source, Sensitive and Origin, and
		// recurses through composites. A second comparison with different
		// semantics is how two halves of an engine start disagreeing about
		// what changed.
		if want.Equal(got) {
			continue
		}

		// Reasons name attributes and types, never values: sensitivity is
		// per-leaf, so even a composite that is not itself marked sensitive
		// may contain a leaf that is.
		note := ""
		if got.Kind != want.Kind {
			note = fmt.Sprintf("kind changed from %s to %s", got.Kind, want.Kind)
		}
		reasons = append(reasons, ChangeReason{Attribute: name, ForceNew: attr.ForceNew, Note: note})
	}

	sortReasons(reasons)
	return reasons
}

// forcesReplacement reports whether any reason names a ForceNew attribute,
// which promotes an update to a replacement.
func forcesReplacement(reasons []ChangeReason) bool {
	for _, r := range reasons {
		if r.ForceNew {
			return true
		}
	}
	return false
}

// hasUnknown reports whether a value, or any leaf inside a composite, is not
// yet known.
//
// A map or list is Known even when one of its entries is not, so a top-level
// check alone would miss exactly the case that matters.
func hasUnknown(v value.Value) bool {
	if !v.Known {
		return true
	}
	switch v.Kind {
	case value.KindList:
		items, _ := v.Raw.([]value.Value)
		for _, item := range items {
			if hasUnknown(item) {
				return true
			}
		}
	case value.KindMap:
		entries, _ := v.Raw.(map[string]value.Value)
		for _, entry := range entries {
			if hasUnknown(entry) {
				return true
			}
		}
	}
	return false
}

// afterAttributes builds the After map for an operation.
//
// It starts from desired configuration and adds the computed attributes the
// schema defines. For NoOp and Update those survive in place, so they are
// carried across from the observed resource; for Create and Replace the object
// is built afresh, so they are unknown until the provider reports them.
func afterAttributes(def *schema.ResourceDefinition, desired, actual map[string]value.Value, kind OpKind) map[string]value.Value {
	out := make(map[string]value.Value, len(desired))
	for name, v := range desired {
		out[name] = v
	}
	if def == nil {
		return out
	}
	for name, attr := range def.Attributes {
		if !attr.Computed {
			continue
		}
		if _, ok := out[name]; ok {
			continue
		}
		if kind == OpCreate || kind == OpReplace {
			out[name] = value.Unknown(attr.Kind, value.SourceProvider)
			continue
		}
		if got, ok := actual[name]; ok {
			out[name] = got
		}
	}
	return out
}

// copyAttrs copies an attribute map so that a plan never aliases the state it
// was built from. Refresh and planning must not mutate what was loaded.
func copyAttrs(attrs map[string]value.Value) map[string]value.Value {
	out := make(map[string]value.Value, len(attrs))
	for name, v := range attrs {
		out[name] = v
	}
	return out
}

// unionKeys returns every key across the given maps, sorted. Every map-to-slice
// boundary sorts; determinism is invariant 6.
func unionKeys(maps ...map[string]value.Value) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range maps {
		for name := range m {
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	sort.Strings(out)
	return out
}

// sortReasons orders reasons deterministically. They are already built in key
// order; sorting again means a caller that appends one cannot break invariant 6.
func sortReasons(reasons []ChangeReason) {
	sort.SliceStable(reasons, func(i, j int) bool {
		if reasons[i].Attribute != reasons[j].Attribute {
			return reasons[i].Attribute < reasons[j].Attribute
		}
		return reasons[i].Note < reasons[j].Note
	})
}
```

- [ ] **Step 4: Implement the planner**

Create `internal/planner/planner.go`:

```go
package planner

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"infra/internal/compiler"
	"infra/internal/diag"
	"infra/internal/refresh"
	"infra/internal/registry"
	"infra/internal/state"
	"infra/pkg/address"
	"infra/pkg/resource"
)

// Options carries what planning needs beyond the three inputs it diffs.
type Options struct {
	// Environment names the environment being planned. It is cross-checked
	// against the configuration and the state: planning one environment's
	// configuration against another's state is the mistake that destroys
	// production.
	Environment string
	// Now supplies the plan's timestamp. It is injectable because Compute is
	// pure, and a function that calls time.Now directly is not.
	Now func() time.Time
	// Registry supplies the schemas the diff needs in order to tell a
	// ForceNew attribute from an updatable one and a computed attribute from
	// desired state. ResolvedConfig carries resolved values, not the schema
	// that governs them, and without it two of spec §11's rules are
	// unimplementable. A nil Registry is an error, never a degradation.
	Registry *registry.Registry
}

// Compute decides one operation per resource by comparing resolved
// configuration against recorded state and observed provider reality.
//
// It is a pure function of its inputs: no filesystem, no network, no provider.
// The returned plan is always non-nil, even when diagnostics contain errors,
// so the caller can render both together; a plan carrying an error is never
// applyable (spec §12.2).
func Compute(cfg compiler.ResolvedConfig, st *state.State, obs refresh.Observations, opts Options) (*Plan, diag.Diagnostics) {
	var ds diag.Diagnostics

	now := opts.Now
	if now == nil {
		now = time.Now
	}

	environment := cfg.Environment
	if environment == "" {
		environment = opts.Environment
	}

	p := &Plan{
		Version:     PlanVersion,
		CreatedAt:   now().UTC(),
		Project:     cfg.Project,
		Environment: environment,
		Operations:  []Operation{},
	}

	if opts.Environment != "" && cfg.Environment != "" && opts.Environment != cfg.Environment {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "configuration was compiled for a different environment",
			Detail: "Planning was asked for environment " + strconv.Quote(opts.Environment) +
				" but the configuration resolves environment " + strconv.Quote(cfg.Environment) + ".",
			Action: "Recompile the configuration for " + opts.Environment + ".",
		})
	}
	if st != nil && st.Environment != "" && st.Environment != environment {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "state belongs to a different environment",
			Detail: "The configuration is for environment " + strconv.Quote(environment) +
				" but the state records environment " + strconv.Quote(st.Environment) +
				". Diffing one environment's desired state against another's record would propose destroying everything in both.",
			Action: "Plan against the state for " + environment + ".",
		})
	}

	if hash, err := cfg.Hash(); err != nil {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "could not fingerprint the configuration",
			Detail:   err.Error(),
			Action:   "This is an engine defect; please report it.",
		})
	} else {
		p.ConfigHash = hash
	}

	if st != nil {
		p.StateSerial = st.Serial
		if hash, err := hashState(st); err != nil {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "could not fingerprint the state",
				Detail:   err.Error(),
				Action:   "This is an engine defect; please report it.",
			})
		} else {
			p.StateHash = hash
		}
	}

	// An environment mismatch stops planning here, before a single operation
	// is decided. The diagnostic alone is not enough protection: diffing this
	// configuration against another environment's state produces a complete,
	// well-formed, savable list of operations that destroys everything in one
	// environment and creates everything in the other. Every caller in this
	// codebase checks HasErrors() first, so nothing today would render it —
	// but a plan is an artifact that gets written to a file, passed around,
	// and read by `apply`, and "it is only dangerous if someone ignores the
	// diagnostics" is not a property worth relying on for the one failure mode
	// that can empty a production environment. Returning no operations makes
	// the dangerous plan impossible to produce rather than merely impolite to
	// use. Compile's returned config follows the same rule for the same
	// reason, and says so in its doc comment.
	if ds.HasErrors() {
		p.Diagnostics = append([]diag.Diagnostic(nil), ds...)
		return p, ds
	}

	if opts.Registry == nil {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "planning requires a provider registry",
			Detail: "Without the schemas the planner cannot tell an attribute that forces replacement " +
				"from one that updates in place, nor a computed attribute from desired state.",
			Action: "Pass Options.Registry.",
		})
		p.Diagnostics = append([]diag.Diagnostic(nil), ds...)
		return p, ds
	}

	for _, addr := range planAddresses(cfg, st) {
		op, opDS := operationFor(addr, cfg, st, obs, opts)
		ds.Extend(opDS)
		if op != nil {
			// Which side of the graph the edges come from depends on the
			// operation, so this runs once the kind has been decided.
			op.Dependents = dependentsOf(addr, op.Kind, cfg, st)
			p.Operations = append(p.Operations, *op)
		}
	}

	p.Diagnostics = append([]diag.Diagnostic(nil), ds...)
	return p, ds
}

// operationFor decides the single operation for one address, per spec §11's
// decision table. It returns nil when planning for that resource failed, in
// which case it has reported why.
func operationFor(
	addr address.Address,
	cfg compiler.ResolvedConfig,
	st *state.State,
	obs refresh.Observations,
	opts Options,
) (*Operation, diag.Diagnostics) {
	var ds diag.Diagnostics

	rc, inConfig := cfg.Get(addr)

	var rs *resource.ResourceState
	if st != nil {
		rs, _ = st.Get(addr)
	}
	inState := rs != nil

	ob, observed := obs[addr.String()]
	if observed && ob.Err != nil {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "could not read " + addr.String() + " from its provider",
			Detail: "The provider reported: " + ob.Err.Error() +
				"\nPlanning cannot continue for this resource. A failed read is not evidence that anything was deleted, " +
				"and treating it as absence would propose destroying or recreating live infrastructure.",
			Action:  "Fix the provider error and run plan again.",
			Related: []address.Address{addr},
		})
		return nil, ds
	}

	// A missing observation is not evidence of absence either — refresh may
	// simply not have covered the address — so the record stands in for it.
	// Only an explicit Observation with a nil State means gone.
	actual := rs
	if observed {
		actual = ob.State
	}

	if !inConfig {
		op, removalDS := removalOperation(addr, rs, actual != nil)
		ds.Extend(removalDS)
		return op, ds
	}

	def, known := opts.Registry.Definition(rc.Type)
	if !known {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "no schema for resource type " + strconv.Quote(rc.Type),
			Detail: "The planner needs the schema to tell an attribute that forces replacement from one that " +
				"updates in place, and a computed attribute from desired state. Without it the plan would " +
				"silently under-report replacements.",
			Action:  "Register a provider that defines " + rc.Type + ". Known types: " + strings.Join(opts.Registry.Types(), ", "),
			Origin:  rc.Origin,
			Related: []address.Address{addr},
		})
		return nil, ds
	}

	if !inState || actual == nil {
		var reasons []ChangeReason
		if inState {
			// Recorded, but the provider no longer has it: something deleted
			// it outside infra, so the plan recreates it.
			reasons = []ChangeReason{{Note: "the provider no longer reports this resource; it will be recreated"}}
		}
		return &Operation{
			Address: addr,
			Type:    rc.Type,
			Kind:    OpCreate,
			After:   afterAttributes(def, rc.Attrs, nil, OpCreate),
			Reasons: reasons,
		}, ds
	}

	reasons := diffAttributes(def, rc.Attrs, actual.Attributes)
	kind := OpNoOp
	switch {
	case forcesReplacement(reasons):
		kind = OpReplace
	case len(reasons) > 0:
		kind = OpUpdate
	}

	return &Operation{
		Address: addr,
		Type:    rc.Type,
		Kind:    kind,
		Before:  copyAttrs(actual.Attributes),
		After:   afterAttributes(def, rc.Attrs, actual.Attributes, kind),
		Reasons: reasons,
	}, ds
}

// removalOperation decides what happens to a resource that is in state but no
// longer in configuration: invariant 1, and spec §11's last two rows.
//
// The lifecycle consulted is the one recorded in state, because the resource
// is by definition no longer in configuration — which is why ResourceState
// records it.
func removalOperation(addr address.Address, rs *resource.ResourceState, present bool) (*Operation, diag.Diagnostics) {
	var ds diag.Diagnostics
	if rs == nil {
		return nil, ds
	}
	before := copyAttrs(rs.Attributes)

	if !present {
		return &Operation{
			Address: addr,
			Type:    rs.Type,
			Kind:    OpForget,
			Before:  before,
			Reasons: []ChangeReason{{Note: "already absent from the provider; only the state entry remains"}},
		}, ds
	}

	// retain is checked first. It destroys nothing, so it already satisfies
	// what prevent_destroy protects: a resource carrying both is forgotten,
	// not refused.
	if rs.Lifecycle.Retain {
		return &Operation{
			Address: addr,
			Type:    rs.Type,
			Kind:    OpForget,
			Before:  before,
			Reasons: []ChangeReason{{Note: "retained; removed from state without calling the provider"}},
		}, ds
	}

	if rs.Lifecycle.PreventDestroy {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  addr.String() + " is protected by prevent_destroy but is no longer in configuration",
			Detail: "Removing it from configuration would destroy it. prevent_destroy is refused at plan time " +
				"rather than at apply time so that the refusal arrives before the approval, not after.",
			Action: "Put " + addr.String() + " back in configuration, or set lifecycle.retain to drop it from " +
				"state without destroying it, or clear prevent_destroy if you really mean to destroy it.",
			Related: []address.Address{addr},
		})
		return nil, ds
	}

	return &Operation{
		Address: addr,
		Type:    rs.Type,
		Kind:    OpDestroy,
		Before:  before,
	}, ds
}

// dependentsOf returns the resources that depend on addr, sorted.
//
// The edges come from whichever side actually has them. A resource being
// destroyed or forgotten is no longer in configuration, and neither are some
// of the things that depended on it, so state's Dependencies are the only
// surviving record; reading configuration there would report zero dependents
// for exactly the operation spec §20 wants called out loudly. For every other
// kind the resource is in configuration and DependsOn is the current truth.
func dependentsOf(target address.Address, kind OpKind, cfg compiler.ResolvedConfig, st *state.State) []address.Address {
	var out []address.Address
	name := target.String()

	dependsOn := func(edges []address.Address) bool {
		for _, edge := range edges {
			if edge.String() == name {
				return true
			}
		}
		return false
	}

	if kind == OpDestroy || kind == OpForget {
		if st == nil {
			return nil
		}
		for _, candidate := range st.Addresses() {
			rs, ok := st.Get(candidate)
			if ok && dependsOn(rs.Dependencies) {
				out = append(out, candidate)
			}
		}
		address.Sort(out)
		return out
	}

	for _, candidate := range cfg.Addresses() {
		rc, ok := cfg.Get(candidate)
		if ok && dependsOn(rc.DependsOn) {
			out = append(out, candidate)
		}
	}
	address.Sort(out)
	return out
}

// planAddresses returns every address in configuration or state, deduplicated
// and sorted. Operations are ordered by canonical address; execution order
// belongs to the graph.
func planAddresses(cfg compiler.ResolvedConfig, st *state.State) []address.Address {
	seen := map[string]bool{}
	var out []address.Address

	add := func(a address.Address) {
		if !seen[a.String()] {
			seen[a.String()] = true
			out = append(out, a)
		}
	}

	for _, a := range cfg.Addresses() {
		add(a)
	}
	if st != nil {
		for _, a := range st.Addresses() {
			add(a)
		}
	}

	address.Sort(out)
	return out
}

// hashState fingerprints state so that M6 can detect a saved plan going stale.
// State.Encode is byte-stable for identical input, which is what makes the
// fingerprint meaningful.
func hashState(st *state.State) (string, error) {
	data, err := st.Encode()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
```

Also append this test, which is the one that makes the guard load-bearing:

```go
// TestEnvironmentMismatchProducesNoOperations is the reason the guard exists.
// The diagnostic is necessary but not sufficient: without the early return,
// Compute happily builds a complete list of operations destroying everything
// recorded in one environment's state and creating everything in the other's
// configuration — a well-formed plan a user could save to a file and hand to
// apply. This asserts the operation list is EMPTY, not merely that an error
// was reported, because the error was always reported.
func TestEnvironmentMismatchProducesNoOperations(t *testing.T) {
	cfg := compiler.ResolvedConfig{
		Project:     "myapp",
		Environment: "dev",
		Resources: map[string]*resource.ResolvedResource{
			"network": {
				Address: address.Address{Name: "network"},
				Type:    "test.network",
				Attrs:   map[string]value.Value{"cidr": value.String("10.0.0.0/16", value.SourceExplicit)},
			},
		},
	}
	st := &state.State{
		Version:     1,
		Serial:      3,
		Project:     "myapp",
		Environment: "production",
		Resources: map[string]*resource.ResourceState{
			"database": {
				Address: address.Address{Name: "database"},
				Type:    "test.database",
				// ResourceState's field is Attributes; ResolvedResource's is
				// Attrs. They are different types and the names differ.
				Attributes: nil,
			},
		},
	}

	p, ds := Compute(cfg, st, nil, planOpts(t))

	if !ds.HasErrors() {
		t.Fatal("planning dev configuration against production state must be an error")
	}
	if len(p.Operations) != 0 {
		t.Fatalf("a mismatched-environment plan must contain no operations, got %d: %+v", len(p.Operations), p.Operations)
	}
	if p.HasChanges() {
		t.Error("a plan with no operations must report no changes")
	}
}
```

Note the state's resource carries a nil `Attributes`. That is harmless — ranging a nil map in Go is legal and yields nothing, so it does not make the failure louder, and an earlier draft of this note wrongly claimed it would panic. What removing the guard actually produces was measured: a wrong operation count, destroying `database` from production state and creating `network` from dev configuration. That is the dangerous plan this guard exists to make impossible, and asserting `len(p.Operations) != 0` is what catches it.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/planner/ -v`
Expected: PASS — fourteen tests from Task 12 plus twenty-seven here.

- [ ] **Step 6: Prove determinism is not a fluke**

Run: `go test ./internal/planner/ -run TestPlanIsDeterministic -count=20`
Expected: PASS, twenty times. Map iteration order is randomised per run, so a boundary that fails to sort usually passes once and fails within a handful of runs. This is invariant 6, and M2 is the milestone that must prove it.

Then confirm the whole tree is still clean:

Run: `go test ./... && go test ./... -race && go vet ./... && gofmt -l .`
Expected: all pass, `gofmt -l .` prints nothing.

- [ ] **Step 7: Commit**

```bash
git add internal/planner
git commit -m "feat: decide operations by diffing configuration against provider reality

Compute is pure: no filesystem, no network, no provider. A read error fails
planning for its resource rather than being read as absence, an unknown desired
value always contributes a change, and a changed ForceNew attribute promotes an
update to a replacement and says which attribute forced it.

Each operation carries its dependents so the renderer can warn on a destructive
change: for a destroy or forget those edges are read from state, because the
resource and its dependents may no longer be in configuration at all.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_015M5QdCFKa6booSVrhnAzTV"
```

---
## Task 14: The plan renderer

**Files:**
- Create: `internal/planner/render.go`
- Test: `internal/planner/render_test.go`, `internal/planner/testdata/create.golden`, `internal/planner/testdata/mixed.golden`, `internal/planner/testdata/verbose.golden`, `internal/planner/testdata/nochanges.golden`

**Interfaces:**
- Consumes: `planner.Plan`, `planner.Operation`, `planner.OpKind` and its `Symbol()`, `planner.ChangeReason` (Task 12); `value.Value`, `value.Kind` constants, `value.SourceDefault` (M1).
- Produces: `planner.RenderOptions{Verbose bool; Color bool}`, `planner.Render(p *Plan, opts RenderOptions) string`.

`Render` is a pure function: it reads only its two arguments and returns text. Determinism is the entire point — spec §12.3 commits to golden files, and M1 deferred a "this needs a real test" concern specifically to this task, so every fixture below is a golden comparison, not a loose substring check, except where a secret or ANSI escape makes a literal golden file the wrong tool.

Helpers specific to rendering are named with a `render` prefix (`renderMarker`, `renderLeaf`, `renderDependentsWarning`). `internal/planner/diff.go` lives in the same package, so the prefix is a deliberate collision guard rather than a style preference — an unprefixed `describe` is exactly the kind of name a diffing file would also want.

**Do not define a local key-ordering helper.** Attribute ordering comes from `unionKeys` in `diff.go` (Task 13), which is variadic: `unionKeys(op.After)` for one map, `unionKeys(op.Before, op.After)` for the union an Update or Replace needs. That ordering is load-bearing for determinism, so a second copy here would not merely be duplication — a divergence between the two would be an invariant 6 bug that only appears on attributes present on one side of a diff.

**Annotations are derived from `Value`, not re-invented per call site.** `[default]` comes directly from `Value.Source == value.SourceDefault`; `(known after apply)` comes directly from `!Value.Known`. Both are self-sufficient — Render never needs to ask *why* a value looks the way it does. The one annotation that cannot be recovered from a `Value` in isolation is "which attribute forced this replacement," because that is a fact about the *operation*, not about either value alone; that one, and only that one, is read from `Operation.Reasons` where `ForceNew` is set. This is why `ChangeReason` exists as a separate field rather than folding replacement cause into `Value.Source`.

**Dependent counts come straight off `Operation.Dependents`.** `planner.Operation` (Task 12) carries `Dependents []address.Address` — the resources that depend on this one, sorted, populated by `Compute` from state `Dependencies` for destroy and forget and from config `DependsOn` for the rest. `Render` does not compute anything here; it only counts. A destructive operation (`OpDestroy` or `OpReplace` — a replace is a destroy-then-create, so it is destructive too) with a non-empty `Dependents` gets one extra line under its header:

    ⚠ This resource has 3 dependent resources.

singular ("1 dependent resource") when there is exactly one, and omitted entirely when there are none — a "0 dependent resources" line would be noise, not information. `OpForget`, `OpNoOp`, `OpCreate` and `OpUpdate` never render this line even when `Dependents` is populated for them, because they are not destructive; the field exists on every kind so the graph-building side of `Compute` doesn't need a special case, but the renderer's job is specifically to flag the operations where losing a dependent is the risk.

**Verbose has exactly one defined effect:** it also lists no-op (unchanged) resources, each on its own line, without a diff. Everything else — annotations, redaction, replacement reasons — renders identically whether or not `Verbose` is set, because none of it is safe to make conditional: a secret must never depend on a flag to stay hidden.

- [ ] **Step 1: Write the failing test**

Create `internal/planner/render_test.go`:

```go
package planner

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"infra/pkg/address"
	"infra/pkg/value"
)

var update = flag.Bool("update", false, "update golden files")

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)

	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("writing golden file: %v", err)
		}
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden file %s: %v (run with -update to create it)", path, err)
	}
	if got != string(want) {
		t.Errorf("Render() does not match %s\n--- got ---\n%s--- want ---\n%s", path, got, string(want))
	}
}

func mixedPlan() *Plan {
	return &Plan{
		Project:     "myapp",
		Environment: "dev",
		Operations: []Operation{
			{
				Address: address.Address{Name: "app"}, Type: "test.application", Kind: OpDestroy,
				Before: map[string]value.Value{
					"password": value.String("hunter2", value.SourceProvider).WithSensitive(true),
				},
				Dependents: []address.Address{{Name: "cdn"}, {Name: "monitor"}},
			},
			{
				Address: address.Address{Name: "cache"}, Type: "test.cache", Kind: OpForget,
				Before: map[string]value.Value{
					"id": value.String("cache-1", value.SourceProvider),
				},
			},
			{
				Address: address.Address{Name: "database"}, Type: "test.database", Kind: OpReplace,
				Before:     map[string]value.Value{"engine": value.String("postgres", value.SourceExplicit)},
				After:      map[string]value.Value{"engine": value.String("mysql", value.SourceExplicit)},
				Reasons:    []ChangeReason{{Attribute: "engine", ForceNew: true, Note: "forces replacement"}},
				Dependents: []address.Address{{Name: "reports"}},
			},
			{
				Address: address.Address{Name: "loadbalancer"}, Type: "test.loadbalancer", Kind: OpUpdate,
				Before: map[string]value.Value{"size": value.Int(10, value.SourceDefault)},
				After:  map[string]value.Value{"size": value.Int(50, value.SourceExplicit)},
			},
			{
				Address: address.Address{Name: "network"}, Type: "test.network", Kind: OpCreate,
				After: map[string]value.Value{"cidr": value.String("10.0.0.0/16", value.SourceExplicit)},
			},
		},
	}
}

func TestRenderCreateOnly(t *testing.T) {
	p := &Plan{
		Project:     "myapp",
		Environment: "dev",
		Operations: []Operation{
			{
				Address: address.Address{Name: "network"}, Type: "test.network", Kind: OpCreate,
				After: map[string]value.Value{
					"cidr": value.String("10.20.0.0/16", value.SourceExplicit),
					"id":   value.Unknown(value.KindString, value.SourceComputed),
				},
			},
		},
	}
	checkGolden(t, "create.golden", Render(p, RenderOptions{}))
}

func TestRenderMixedOperations(t *testing.T) {
	checkGolden(t, "mixed.golden", Render(mixedPlan(), RenderOptions{}))
}

func TestRenderVerboseListsUnchangedResources(t *testing.T) {
	p := mixedPlan()
	p.Operations = append(p.Operations, Operation{
		Address: address.Address{Name: "zzz"}, Type: "test.network", Kind: OpNoOp,
		Before: map[string]value.Value{"cidr": value.String("10.0.0.0/16", value.SourceExplicit)},
		After:  map[string]value.Value{"cidr": value.String("10.0.0.0/16", value.SourceExplicit)},
	})
	checkGolden(t, "verbose.golden", Render(p, RenderOptions{Verbose: true}))
}

func TestRenderNoChanges(t *testing.T) {
	p := &Plan{
		Project:     "myapp",
		Environment: "dev",
		Operations: []Operation{
			{
				Address: address.Address{Name: "network"}, Type: "test.network", Kind: OpNoOp,
				Before: map[string]value.Value{"cidr": value.String("10.0.0.0/16", value.SourceExplicit)},
				After:  map[string]value.Value{"cidr": value.String("10.0.0.0/16", value.SourceExplicit)},
			},
		},
	}
	checkGolden(t, "nochanges.golden", Render(p, RenderOptions{}))
}

// TestRenderNeverLeaksASecretForAnyOperationKind is the test Task 12's plan
// text specifically asks for: sensitivity is per-leaf (a lesson M1 shipped a
// leak over), so a secret nested inside a list or a map must be redacted just
// as surely as one at the top level, for every operation kind and under both
// RenderOptions.
func TestRenderNeverLeaksASecretForAnyOperationKind(t *testing.T) {
	secret := value.String("hunter2", value.SourceProvider).WithSensitive(true)
	nested := value.Map(map[string]value.Value{
		"user": value.String("admin", value.SourceProvider),
		"pass": secret,
	}, value.SourceProvider)
	inList := value.List([]value.Value{
		value.String("public", value.SourceProvider),
		secret,
	}, value.SourceProvider)

	var ops []Operation
	for i, kind := range []OpKind{OpCreate, OpUpdate, OpReplace, OpDestroy, OpForget} {
		op := Operation{
			Address: address.Address{Name: fmt.Sprintf("r%d", i)},
			Type:    "test.secret",
			Kind:    kind,
		}
		leaves := map[string]value.Value{"scalar": secret, "nested": nested, "list": inList}
		switch kind {
		case OpCreate:
			op.After = leaves
		case OpDestroy, OpForget:
			op.Before = leaves
		case OpUpdate, OpReplace:
			op.Before = map[string]value.Value{"scalar": value.String("was", value.SourceProvider)}
			op.After = leaves
			if kind == OpReplace {
				op.Reasons = []ChangeReason{{Attribute: "scalar", ForceNew: true}}
			}
		}
		ops = append(ops, op)
	}
	p := &Plan{Project: "myapp", Environment: "dev", Operations: ops}

	for _, verbose := range []bool{false, true} {
		for _, color := range []bool{false, true} {
			got := Render(p, RenderOptions{Verbose: verbose, Color: color})
			if strings.Contains(got, "hunter2") {
				t.Errorf("verbose=%v color=%v: rendered output leaks the secret:\n%s", verbose, color, got)
			}
			if !strings.Contains(got, "<sensitive>") {
				t.Errorf("verbose=%v color=%v: expected a <sensitive> marker somewhere", verbose, color)
			}
		}
	}
}

func TestRenderColorWrapsMarkersInANSIAndPlainDoesNot(t *testing.T) {
	p := &Plan{
		Project:     "myapp",
		Environment: "dev",
		Operations: []Operation{
			{
				Address: address.Address{Name: "network"}, Type: "test.network", Kind: OpCreate,
				After: map[string]value.Value{"cidr": value.String("10.0.0.0/16", value.SourceExplicit)},
			},
		},
	}

	plain := Render(p, RenderOptions{})
	if strings.Contains(plain, "\x1b[") {
		t.Errorf("Color: false must not emit ANSI escape codes:\n%s", plain)
	}

	colored := Render(p, RenderOptions{Color: true})
	if !strings.Contains(colored, "\x1b[32m+\x1b[0m") {
		t.Errorf("Color: true must wrap the create marker in ANSI, got:\n%s", colored)
	}
}

func TestRenderDestructiveWithoutDependentsShowsNoWarning(t *testing.T) {
	p := &Plan{
		Project:     "myapp",
		Environment: "dev",
		Operations: []Operation{
			{
				Address: address.Address{Name: "solo"}, Type: "test.network", Kind: OpDestroy,
				Before: map[string]value.Value{"cidr": value.String("10.0.0.0/16", value.SourceExplicit)},
			},
		},
	}
	got := Render(p, RenderOptions{})
	if strings.Contains(got, "dependent") {
		t.Errorf("a destroy with no dependents must not render a dependents warning:\n%s", got)
	}
}

func TestRenderDestructiveWithDependentsShowsCountSingularAndPlural(t *testing.T) {
	p := &Plan{
		Project:     "myapp",
		Environment: "dev",
		Operations: []Operation{
			{
				Address: address.Address{Name: "one"}, Type: "test.network", Kind: OpDestroy,
				Before:     map[string]value.Value{"cidr": value.String("10.0.0.0/16", value.SourceExplicit)},
				Dependents: []address.Address{{Name: "app"}},
			},
			{
				Address: address.Address{Name: "two"}, Type: "test.network", Kind: OpDestroy,
				Before:     map[string]value.Value{"cidr": value.String("10.1.0.0/16", value.SourceExplicit)},
				Dependents: []address.Address{{Name: "app"}, {Name: "worker"}},
			},
		},
	}
	got := Render(p, RenderOptions{})
	if !strings.Contains(got, "1 dependent resource.") || strings.Contains(got, "1 dependent resources.") {
		t.Errorf("singular count rendered wrong:\n%s", got)
	}
	if !strings.Contains(got, "2 dependent resources.") {
		t.Errorf("plural count rendered wrong:\n%s", got)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/planner/ -run TestRender -v`
Expected: FAIL to compile — `undefined: Render`, `undefined: RenderOptions`.

- [ ] **Step 3: Implement the renderer**

Create `internal/planner/render.go`:

```go
package planner

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"infra/pkg/value"
)

// RenderOptions controls how a Plan is rendered to text.
type RenderOptions struct {
	// Verbose additionally lists resources with no changes.
	Verbose bool
	// Color wraps operation markers in ANSI escape codes.
	Color bool
}

const (
	renderAnsiReset   = "\x1b[0m"
	renderAnsiGreen   = "\x1b[32m"
	renderAnsiYellow  = "\x1b[33m"
	renderAnsiBoldRed = "\x1b[1;31m"
	renderAnsiCyan    = "\x1b[36m"
)

// Render turns a Plan into the text a user reads before approving it.
//
// It is pure: identical plans render identical text, which is what makes
// invariant 6 testable at all (spec §12.1, §12.3) and is why every case in
// render_test.go is a golden-file comparison rather than a spot check.
func Render(p *Plan, opts RenderOptions) string {
	var lines []string
	lines = append(lines, fmt.Sprintf("Plan for project %q, environment %q:", p.Project, p.Environment))
	lines = append(lines, "")

	any := false
	for _, op := range p.Operations {
		if op.Kind == OpNoOp {
			if !opts.Verbose {
				continue
			}
			lines = append(lines, fmt.Sprintf("    %s.%s (no changes)", op.Type, op.Address.String()), "")
			any = true
			continue
		}
		lines = append(lines, renderOperationLines(op, opts)...)
		lines = append(lines, "")
		any = true
	}

	if !any {
		lines = append(lines, "No changes. Configuration matches the observed state.", "")
	}

	lines = append(lines, renderSummary(p))
	return strings.Join(lines, "\n") + "\n"
}

// renderOperationLines renders one changed operation: its header line, plus
// one line per attribute that differs.
func renderOperationLines(op Operation, opts RenderOptions) []string {
	header := "  " + renderMarker(op.Kind, opts.Color) + " " + op.Type + "." + op.Address.String()
	if op.Kind == OpReplace {
		if forced := renderForcedBy(op.Reasons); forced != "" {
			header += "  (replacement forced by: " + forced + ")"
		}
	}
	lines := []string{header}

	if op.Kind == OpDestroy || op.Kind == OpReplace {
		if warning := renderDependentsWarning(op); warning != "" {
			lines = append(lines, warning)
		}
	}

	switch op.Kind {
	case OpCreate:
		for _, k := range unionKeys(op.After) {
			lines = append(lines, "      "+k+": "+renderAnnotated(op.After[k]))
		}
	case OpDestroy, OpForget:
		for _, k := range unionKeys(op.Before) {
			lines = append(lines, "      "+k+": "+renderAnnotated(op.Before[k]))
		}
	case OpUpdate, OpReplace:
		for _, k := range unionKeys(op.Before, op.After) {
			before, hadBefore := op.Before[k]
			after, hasAfter := op.After[k]
			if hadBefore && hasAfter && before.Equal(after) {
				continue
			}
			lines = append(lines, "      "+k+": "+renderSide(before, hadBefore)+" -> "+renderSide(after, hasAfter))
		}
	}
	return lines
}

// renderDependentsWarning names how many resources depend on a destructive
// operation's resource, or the empty string when there are none — a "0
// dependent resources" line would be noise, not information.
func renderDependentsWarning(op Operation) string {
	n := len(op.Dependents)
	if n == 0 {
		return ""
	}
	noun := "resources"
	if n == 1 {
		noun = "resource"
	}
	return fmt.Sprintf("    ⚠ This resource has %d dependent %s.", n, noun)
}

// renderMarker returns an operation's symbol, optionally ANSI-colored.
// Destructive operations (Replace, Destroy) are bold red — the highlighting
// half of spec §12.3's bullet; renderDependentsWarning is the count half.
func renderMarker(k OpKind, color bool) string {
	s := k.Symbol()
	if !color {
		return s
	}
	switch k {
	case OpCreate:
		return renderAnsiGreen + s + renderAnsiReset
	case OpUpdate:
		return renderAnsiYellow + s + renderAnsiReset
	case OpReplace, OpDestroy:
		return renderAnsiBoldRed + s + renderAnsiReset
	case OpForget:
		return renderAnsiCyan + s + renderAnsiReset
	default:
		return s
	}
}

// renderForcedBy names the attributes whose change forced a replacement,
// sorted so Render's own output does not depend on the order Compute
// produced Reasons in.
func renderForcedBy(reasons []ChangeReason) string {
	var names []string
	for _, r := range reasons {
		if r.ForceNew {
			names = append(names, r.Attribute)
		}
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// renderAnnotated renders one value plus, when it applies, the "[default]"
// annotation. Unlike the replacement reason, this is read straight off the
// Value — Source and Known already say everything Render needs.
func renderAnnotated(v value.Value) string {
	s := renderLeaf(v)
	if v.Known && v.Source == value.SourceDefault {
		s += " [default]"
	}
	return s
}

// unrenderable stands in for a value this function cannot safely display.
// It is deliberately not empty: rendering nothing would hide the existence of
// data, and the reader needs to know something is there.
const unrenderable = "<unrenderable>"

// renderLeaf renders one value for display, redacting sensitive data.
//
// Sensitivity is per-leaf: a non-sensitive list or map can hold a sensitive
// element. This checks Sensitive before descending into Raw at all, and
// recurses into List and Map so nothing buried inside a composite reaches the
// page in clear text. Map keys are sorted so output is stable across runs.
//
// The kind switch is an ALLOWLIST and must stay one. The obvious shape —
// ending in `default: fmt.Sprintf("%v", v.Raw)` — looks safe because
// sensitivity is checked at the top, but that check only covers the value in
// hand, not the leaves inside it. value.KindInvalid is the ZERO VALUE of
// value.Kind, so any Value whose Kind was never set carries its Raw straight
// into %v, and %v on a map[string]value.Value prints every field of every
// leaf. This was measured against internal/cli/state.go's formatValue, which
// had exactly that default branch; it rendered
//
//	map[password:{string true hunter2 provider true  <generated>}]
//
// printing the secret in clear text with its own Sensitive flag beside it,
// ignored. formatValue was hardened the same way in the same commit that
// wrote this comment, and its regression test is
// TestFormatValueFailsClosedOnUnexpectedShapes.
//
// The composite branches fail closed for the same reason: a failed type
// assertion used to yield an empty {} or [], which claims a composite was
// empty when it was really unreadable.
func renderLeaf(v value.Value) string {
	if v.Sensitive {
		return "<sensitive>"
	}
	if !v.Known {
		return "(known after apply)"
	}
	switch v.Kind {
	case value.KindList:
		items, ok := v.Raw.([]value.Value)
		if !ok {
			return unrenderable
		}
		parts := make([]string, len(items))
		for i, item := range items {
			parts[i] = renderLeaf(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case value.KindMap:
		m, ok := v.Raw.(map[string]value.Value)
		if !ok {
			return unrenderable
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = k + ": " + renderLeaf(m[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case value.KindString:
		s, _ := v.AsString()
		return strconv.Quote(s)
	case value.KindInt, value.KindFloat, value.KindBool:
		return fmt.Sprintf("%v", v.Raw)
	default:
		// KindInvalid, or a kind added later that nobody taught this
		// function about. Both fail closed. See the comment above.
		return unrenderable
	}
}


// renderSummary is the "N to create, N to update, ..." line spec §12.3 asks
// for. A map lookup for a Counts() key with no entries is Go's zero value, so
// a plan with no operations of some kind needs no special case.
func renderSummary(p *Plan) string {
	counts := p.Counts()
	return fmt.Sprintf("Plan: %d to create, %d to update, %d to replace, %d to destroy, %d to forget.",
		counts[OpCreate], counts[OpUpdate], counts[OpReplace], counts[OpDestroy], counts[OpForget])
}
```

Also append this test to `render_test.go`. It is a literal assertion rather than a golden file, because the point is that these shapes must never reach a fixture at all:

```go
// TestRenderLeafFailsClosedOnUnexpectedShapes covers the branch a golden file
// cannot: values whose Kind and Raw disagree, or whose Kind was never set.
// See renderLeaf's doc comment for the measured leak this prevents.
func TestRenderLeafFailsClosedOnUnexpectedShapes(t *testing.T) {
	secret := value.String("hunter2", value.SourceProvider).WithSensitive(true)

	cases := []struct {
		name string
		v    value.Value
	}{
		{"kind left at the zero value with a composite Raw",
			value.Value{Known: true, Raw: map[string]value.Value{"password": secret}}},
		{"kind says map, Raw is a different map type",
			value.Value{Kind: value.KindMap, Known: true, Raw: map[string]any{"password": "hunter2"}}},
		{"kind says list, Raw is a different slice type",
			value.Value{Kind: value.KindList, Known: true, Raw: []any{"hunter2"}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderLeaf(tc.v)
			if strings.Contains(got, "hunter2") {
				t.Errorf("secret rendered in clear text: %s", got)
			}
			if got != "<unrenderable>" {
				t.Errorf("renderLeaf = %q, want %q", got, "<unrenderable>")
			}
		})
	}
}
```

- [ ] **Step 4: Create the golden fixtures**

Create `internal/planner/testdata/create.golden`:

```
Plan for project "myapp", environment "dev":

  + test.network.network
      cidr: "10.20.0.0/16"
      id: (known after apply)

Plan: 1 to create, 0 to update, 0 to replace, 0 to destroy, 0 to forget.
```

Create `internal/planner/testdata/mixed.golden`:

```
Plan for project "myapp", environment "dev":

  - test.application.app
    ⚠ This resource has 2 dependent resources.
      password: <sensitive>

  = test.cache.cache
      id: "cache-1"

  -/+ test.database.database  (replacement forced by: engine)
    ⚠ This resource has 1 dependent resource.
      engine: "postgres" -> "mysql"

  ~ test.loadbalancer.loadbalancer
      size: 10 [default] -> 50

  + test.network.network
      cidr: "10.0.0.0/16"

Plan: 1 to create, 1 to update, 1 to replace, 1 to destroy, 1 to forget.
```

Create `internal/planner/testdata/verbose.golden`:

```
Plan for project "myapp", environment "dev":

  - test.application.app
    ⚠ This resource has 2 dependent resources.
      password: <sensitive>

  = test.cache.cache
      id: "cache-1"

  -/+ test.database.database  (replacement forced by: engine)
    ⚠ This resource has 1 dependent resource.
      engine: "postgres" -> "mysql"

  ~ test.loadbalancer.loadbalancer
      size: 10 [default] -> 50

  + test.network.network
      cidr: "10.0.0.0/16"

    test.network.zzz (no changes)

Plan: 1 to create, 1 to update, 1 to replace, 1 to destroy, 1 to forget.
```

Create `internal/planner/testdata/nochanges.golden`:

```
Plan for project "myapp", environment "dev":

No changes. Configuration matches the observed state.

Plan: 0 to create, 0 to update, 0 to replace, 0 to destroy, 0 to forget.
```

Each file must end with exactly one trailing newline and no other trailing whitespace — `Render`'s final `+ "\n"` is the only newline after the summary line. If a golden comparison fails and the diff looks like whitespace, regenerate with `go test ./internal/planner/ -run TestRender -update -v` and inspect the diff with `git diff` before committing it — never accept a regenerated golden file without reading what changed.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/planner/ -run TestRender -v`
Expected: PASS — nine tests (four golden comparisons, the cross-operation-kind secret test, the ANSI test, and the two dependent-count tests).

- [ ] **Step 6: Check the surface-text rule**

Run: `grep -n '== "' internal/planner/render.go`
Expected: no hits, or only comparisons against a fixed rendering keyword — never a comparison that judges a `value.Value` by its printed text instead of its `Kind`/`Known`/`Sensitive`/`Source` fields.

- [ ] **Step 7: Commit**

```bash
git add internal/planner/render.go internal/planner/render_test.go internal/planner/testdata
git commit -m "$(cat <<'EOF'
feat: add the pure plan renderer

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_015M5QdCFKa6booSVrhnAzTV
EOF
)"
```

---

## Task 15: The `plan` command

**Files:**
- Create: `internal/cli/plan.go`
- Modify: `internal/cli/root.go`
- Test: `internal/cli/plan_test.go`

**Interfaces:**
- Consumes: `config.Load`, `compiler.Compile`, `compiler.Options` (Task 9); `refresh.Refresh` (Task 11); `planner.Compute`, `planner.Options`, `planner.Render`, `planner.RenderOptions` (Tasks 13, 14); `cli.buildRegistry`, `cli.backendFor`, `cli.GlobalOptions`, `cli.ExitOK/ExitError/ExitChanges` (M1).
- Produces: `cli.newPlanCommand(opts *GlobalOptions) *cobra.Command`, registered in `root.go`; working `infra plan <environment>`.

This task wires the pipeline spec §7–§12 describe end to end for the first time: `config.Load` → `compiler.Compile` → load state → `refresh.Refresh` → `planner.Compute` → `planner.Render`. Three decisions carry the weight, plus one wiring detail that is easy to get wrong precisely because it type-checks either way: `planner.Options` carries a `Registry *registry.Registry` field (`ResolvedConfig` has resolved values but no schema, and without a registry `Compute` cannot tell a `ForceNew` attribute from an updatable one or a computed attribute from desired state — spec §11's rules need it, and a nil `Registry` is an error diagnostic, not a silent degradation). `plan.go` passes the same `reg` that `buildRegistry(opts.Dir)` already produced for `compiler.Compile` — there is no second registry to build.

**`plan` never writes state and never takes the lock (spec §10).** It calls `backendFor(dir).Get(ctx, environment)` and nothing else on the backend — no `Lock`, no `Put`. That is what makes it safe to run repeatedly, in CI, and against an environment another command holds locked; a read-only verb that quietly persisted something would violate exactly the property spec §20's decision table calls out ("`plan` never writes state — a read-only verb that writes is surprising and unsafe to run concurrently").

**Exit code 2 needs a second signal channel, because `Execute()` only had two.** M1's `Execute()` maps any non-nil `RunE` error to `ExitError`. `plan` needs three outcomes — no changes, error, changes — from one `error` return. The fix is a sentinel: `errChanges`, returned by `RunE` when `p.HasChanges()`, and checked in `Execute()` with `errors.Is` before the generic error path runs, so it is never printed as `"Error: ..."` and never confused with an actual failure. This is a real change to `root.go`, not cosmetic — without it, exit code 2 is unreachable no matter what `plan` returns.

**`Plan.Diagnostics` is a second diagnostic channel, and both must gate the exit code.** `planner.Compute` returns `(*Plan, diag.Diagnostics)`, but the `Plan` it returns *also* carries its own `Diagnostics []diag.Diagnostic` — that field exists specifically for plan-time errors that belong to one operation, such as `prevent_destroy` (spec §11: "a plan-time error, not an apply-time refusal"). If `plan.go` only checked the function's own returned diagnostics, a `prevent_destroy` violation would render a plan that looks approvable and exit 0. So `RunE` extends its working diagnostic set with `p.Diagnostics` before deciding pass or fail, and renders the plan text only after that check clears.

`--output` reuses the existing global `--output` flag from `GlobalOptions` (M1) rather than adding a plan-local one — spec §16 already scopes it as a global flag, and `plan` is simply the first command to use it. It writes with `json.MarshalIndent` at mode `0600`, the full artifact including `CreatedAt`, not `Plan.Canonical()` — `Canonical()` exists to define determinism over the plan's *inputs* (spec §12.1) and deliberately excludes `CreatedAt`; the saved artifact is a record of what a specific run produced and needs it. Applying a saved plan is M6 and is not implemented here.

`--parallelism` (M1 global flag, default 10) passes straight through to `refresh.Refresh`. `--color` does not exist yet — no flag in `GlobalOptions` requests it, so `plan` calls `planner.Render` with `Color: false` always; `RenderOptions.Color` stays available in the renderer for whenever a `--color`/TTY-detection flag is added, which is outside this task's three files.

Diagnostics render to stderr (`cmd.ErrOrStderr()`); the plan itself renders to stdout — so `infra plan dev | grep test.database` works and never mixes the two streams.

- [ ] **Step 1: Write the failing test**

Create `internal/cli/plan_test.go`:

```go
package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlanProposesCreatesOnFreshProject(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: 10.20.0.0/16
`)
	opts := &GlobalOptions{Dir: dir, Parallelism: 4}
	cmd := newPlanCommand(opts)
	cmd.SetArgs([]string{"dev"})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := cmd.Execute()
	if !errors.Is(err, errChanges) {
		t.Fatalf("Execute() = %v, want errChanges — a fresh project always has changes", err)
	}
	if !strings.Contains(stdout.String(), "test.network.network") {
		t.Errorf("stdout does not mention the proposed resource:\n%s", stdout.String())
	}
}

func TestPlanRendersDiagnosticsToStderrOnInvalidConfig(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  database:
    type: aws.rds
    engine: postgres
`)
	opts := &GlobalOptions{Dir: dir, Parallelism: 4}
	cmd := newPlanCommand(opts)
	cmd.SetArgs([]string{"dev"})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := cmd.Execute()
	if err == nil || errors.Is(err, errChanges) {
		t.Fatalf("Execute() = %v, want a plain error for invalid configuration", err)
	}
	if !strings.Contains(stderr.String(), "aws.rds") {
		t.Errorf("stderr does not name the offending type:\n%s", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("no plan should reach stdout when configuration fails to compile, got:\n%s", stdout.String())
	}
}

func TestPlanNeverWritesStateOrTakesTheLock(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: 10.20.0.0/16
`)
	opts := &GlobalOptions{Dir: dir, Parallelism: 4}
	cmd := newPlanCommand(opts)
	cmd.SetArgs([]string{"dev"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	_ = cmd.Execute()

	if _, err := os.Stat(filepath.Join(dir, ".infra", "state", "dev.json")); !os.IsNotExist(err) {
		t.Error("plan must never write state")
	}
	if _, err := os.Stat(filepath.Join(dir, ".infra", "state", "dev.lock")); !os.IsNotExist(err) {
		t.Error("plan must never take the environment lock")
	}
}

func TestPlanOutputWritesA0600JSONFile(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: 10.20.0.0/16
`)
	outPath := filepath.Join(t.TempDir(), "plan.json")
	opts := &GlobalOptions{Dir: dir, Parallelism: 4, Output: outPath}
	cmd := newPlanCommand(opts)
	cmd.SetArgs([]string{"dev"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	_ = cmd.Execute()

	info, err := os.Stat(outPath)
	if err != nil {
		t.Fatalf("--output did not write a file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("plan file mode = %v, want 0600", info.Mode().Perm())
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading plan file: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("--output file is not valid JSON: %v", err)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/cli/ -run TestPlan -v`
Expected: FAIL to compile — `undefined: newPlanCommand`, `undefined: errChanges`.

- [ ] **Step 3: Implement the plan command**

Create `internal/cli/plan.go`:

```go
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"infra/internal/compiler"
	"infra/internal/config"
	"infra/internal/planner"
	"infra/internal/refresh"
)

// errChanges signals that a plan completed successfully but found changes to
// propose. It carries exit code 2 (spec §16) and must never be reported to
// the user as a failure — Execute checks for it with errors.Is before the
// generic error path runs.
var errChanges = errors.New("plan has changes")

// newPlanCommand builds `infra plan <environment>`. It never writes state and
// never takes the environment lock (spec §10), so it is safe to run
// repeatedly, in CI, and against an environment another command holds
// locked.
func newPlanCommand(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use: "plan <environment>",
		// Set here as well as on the root. cobra's ExecuteC consults
		// c.Root()'s Silence* fields when the executing command has a
		// parent, so in production the root's settings apply and these are
		// redundant. But the tests construct this command standalone, with
		// no parent, and then cobra consults THIS command — whose zero
		// values are false — and prints usage boilerplate to stdout on
		// every non-nil RunE return, including the errChanges
		// success-with-changes path. Measured: it polluted stdout in
		// TestPlanRendersDiagnosticsToStderrOnInvalidConfig.
		SilenceUsage:  true,
		SilenceErrors: true,
		Short: "Show what infra would change without applying it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			environment := args[0]

			vars, err := parseVars(opts.Vars)
			if err != nil {
				return err
			}

			files, err := config.Load(opts.Dir)
			if err != nil {
				return err
			}

			reg := buildRegistry(opts.Dir)
			cfg, ds := compiler.Compile(files, reg, compiler.Options{
				Environment: environment,
				Vars:        vars,
			})
			if ds.HasErrors() {
				ds.Render(cmd.ErrOrStderr())
				return errors.New("configuration is not valid")
			}

			st, err := backendFor(opts.Dir).Get(cmd.Context(), environment)
			if err != nil {
				return err
			}

			obs, refreshDiags := refresh.Refresh(cmd.Context(), st, reg, opts.Parallelism)
			ds.Extend(refreshDiags)
			if ds.HasErrors() {
				ds.Render(cmd.ErrOrStderr())
				return errors.New("refreshing provider state failed")
			}

			p, planDiags := planner.Compute(cfg, st, obs, planner.Options{
				Environment: environment,
				Now:         time.Now,
				// Registry gives the diff the schemas it needs to tell a
				// ForceNew attribute from an updatable one and a computed
				// attribute from desired state (spec §11) — reuse the same
				// registry Compile already built, rather than constructing
				// a second one.
				Registry: reg,
			})
			ds.Extend(planDiags)
			// Plan.Diagnostics carries plan-time errors that belong to one
			// operation, such as prevent_destroy (spec §11). They must gate
			// the exit code exactly like a compile or refresh error, so they
			// join the same diagnostic set before anything is rendered.
			ds.Extend(p.Diagnostics)
			ds.Render(cmd.ErrOrStderr())
			if ds.HasErrors() {
				return errors.New("planning failed")
			}

			fmt.Fprint(cmd.OutOrStdout(), planner.Render(p, planner.RenderOptions{
				Verbose: opts.Verbose,
			}))

			if opts.Output != "" {
				// The full artifact, CreatedAt included — Canonical() exists
				// to define determinism over the plan's inputs (spec §12.1)
				// and deliberately excludes it; a saved plan is a record of
				// what this run produced. It contains sensitive values, so
				// 0600 (spec §12.2).
				data, err := json.MarshalIndent(p, "", "  ")
				if err != nil {
					return fmt.Errorf("serializing plan: %w", err)
				}
				if err := os.WriteFile(opts.Output, data, 0o600); err != nil {
					return fmt.Errorf("writing plan: %w", err)
				}
			}

			if p.HasChanges() {
				return errChanges
			}
			return nil
		},
	}
}

// parseVars turns --var name=value flags into the map the compiler's
// variable scope consumes. The full variable system — typed schemas,
// --var-file, precedence — is M4; M2 only makes the raw strings available.
func parseVars(raw []string) (map[string]string, error) {
	out := make(map[string]string, len(raw))
	for _, v := range raw {
		name, val, ok := strings.Cut(v, "=")
		if !ok || name == "" {
			return nil, fmt.Errorf("--var %q must be in the form name=value", v)
		}
		out[name] = val
	}
	return out, nil
}
```

- [ ] **Step 4: Register the command and wire exit code 2**

Replace `internal/cli/root.go` entirely:

```go
// Package cli wires the infra command-line interface.
package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// Exit codes. Spec §16: 0 success with no changes, 1 error, 2 success with
// changes present.
const (
	ExitOK      = 0
	ExitError   = 1
	ExitChanges = 2
)

// GlobalOptions holds flags shared by every subcommand.
type GlobalOptions struct {
	Vars        []string
	VarFiles    []string
	Verbose     bool
	Output      string
	Parallelism int
	AutoApprove bool
	Dir         string
}

// NewRootCommand builds the command tree. It is a constructor rather than a
// package-level variable so tests can build independent instances.
func NewRootCommand() *cobra.Command {
	opts := &GlobalOptions{}

	root := &cobra.Command{
		Use:           "infra",
		Short:         "Declarative infrastructure management",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	f := root.PersistentFlags()
	f.StringArrayVar(&opts.Vars, "var", nil, "set a variable (name=value); repeatable")
	f.StringArrayVar(&opts.VarFiles, "var-file", nil, "load variables from a file; repeatable")
	f.BoolVar(&opts.Verbose, "verbose", false, "include provider-level detail in output")
	f.StringVar(&opts.Output, "output", "", "write machine-readable output to this path")
	f.IntVar(&opts.Parallelism, "parallelism", 10, "maximum concurrent operations")
	f.BoolVar(&opts.AutoApprove, "auto-approve", false, "skip interactive approval")
	f.StringVar(&opts.Dir, "chdir", ".", "run as if infra had been started in this directory")

	root.AddCommand(newValidateCommand(opts))
	root.AddCommand(newStateCommand(opts))
	root.AddCommand(newPlanCommand(opts))

	return root
}

// Execute runs the CLI and returns the process exit code. Human output goes to
// stdout; diagnostics go to stderr, so piping works. Spec §16.
func Execute() int {
	root := NewRootCommand()
	root.SetOut(os.Stdout)
	root.SetErr(os.Stderr)

	if err := root.Execute(); err != nil {
		if errors.Is(err, errChanges) {
			return ExitChanges
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return ExitError
	}
	return ExitOK
}
```

The only changes from M1's version are the `errors` import, `root.AddCommand(newPlanCommand(opts))`, and the `errors.Is(err, errChanges)` branch in `Execute`.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/cli/ -run TestPlan -v`
Expected: PASS — four tests.

- [ ] **Step 6: Run the whole suite**

Run: `make check`
Expected: everything green, no formatting or vet findings.

- [ ] **Step 7: Commit**

```bash
git add internal/cli/plan.go internal/cli/root.go internal/cli/plan_test.go
git commit -m "$(cat <<'EOF'
feat: add the plan command

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_015M5QdCFKa6booSVrhnAzTV
EOF
)"
```

---

## Task 16: M2 integration tests

CLI-level tests that drive the built binary the way a person does, the same architecture as M1's Task 15. These are the tests that catch a wiring mistake no unit test — including Task 14's and 15's own — can see, because they are the first tests in the whole milestone that run the real compiler, refresh and planner together against the real fake provider.

**Files:**
- Create: `tests/integration/m2_test.go`

**Interfaces:**
- Consumes: `binary(t)`, `run(t, dir, args...)`, `project(t, body)`, `requireContains(t, haystack, needle)` from M1's `tests/integration/helpers_test.go` — reused directly, not duplicated.
- Produces: nothing importable.

Two seeding helpers are new here because M2 is the first milestone where a test needs to seed *both* sides of drift: the state file (what infra last recorded) and `.infra/fake-cloud.json` (what the provider reports now). M1's `writeState` in `m1_test.go` only ever needed the first. This task's `writeM2State` generalizes that shape to arbitrary resources, and `writeFakeCloud` is new outright — writing the cloud file directly, by hand, as JSON, because that file being hand-editable is the whole reason it is a file (spec §8.4, §20) and is exactly what the drift test exercises.

One scenario needs a note on interpretation. `prevent_destroy` renders as a plan-time diagnostic, and the exact wording of that diagnostic's `Summary` is Task 13's decision (`planner.Compute`), authored in parallel and not available to read here. Asserting a literal string would be guessing at another task's prose. What this task can assert without guessing: exit code 1 (an error, not a proposed change — spec §11), that the offending resource's address appears on stderr (`diag.Diagnostic.Related` exists precisely so a diagnostic always names its resource), and that no plan reaches stdout at all.

- [ ] **Step 1: Write the seeding helpers and the M2 behaviour tests**

Create `tests/integration/m2_test.go`:

```go
package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeM2State seeds a state file directly, standing in for the apply that M3
// will provide. resources is keyed by address string in the shape
// state.Decode expects: capitalized struct field names at the ResourceState
// level, and value.Value's wire format (kind/known/raw/source/sensitive) for
// each attribute — see stateResource and wireAttr below.
func writeM2State(t *testing.T, dir, environment string, resources map[string]any) {
	t.Helper()
	stateDir := filepath.Join(dir, ".infra", "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	doc := map[string]any{
		"version":     1,
		"serial":      1,
		"project":     "myapp",
		"environment": environment,
		"resources":   resources,
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, environment+".json"), data, 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}
}

// stateResource builds one resource entry for writeM2State. lifecycle may be
// nil; pass e.g. map[string]any{"PreventDestroy": true} to seed a lifecycle
// guard on a resource that no longer exists in configuration.
func stateResource(name, resourceType, providerID string, attrs map[string]any, lifecycle map[string]any) map[string]any {
	r := map[string]any{
		"Address":    map[string]any{"Name": name},
		"Type":       resourceType,
		"Provider":   "test",
		"ProviderID": providerID,
		"Attributes": attrs,
	}
	if lifecycle != nil {
		r["Lifecycle"] = lifecycle
	}
	return r
}

// wireAttr builds one attribute in value.Value's wire format.
func wireAttr(kind string, raw any, sensitive bool) map[string]any {
	a := map[string]any{"kind": kind, "known": true, "raw": raw, "source": "provider"}
	if sensitive {
		a["sensitive"] = true
	}
	return a
}

// writeFakeCloud seeds .infra/fake-cloud.json directly, as JSON — the file is
// hand-editable by design (spec §8.4), which is exactly what
// TestPlanShowsExternalDriftAsAnUpdate exercises. Unlike state's Attributes,
// a CloudResource's attributes are plain JSON values, not value.Value's wire
// format: the cloud models an external system, not internal configuration.
func writeFakeCloud(t *testing.T, dir string, resources map[string]map[string]any) {
	t.Helper()
	cloudDir := filepath.Join(dir, ".infra")
	if err := os.MkdirAll(cloudDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	doc := map[string]any{"resources": resources}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal fake cloud: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cloudDir, "fake-cloud.json"), data, 0o600); err != nil {
		t.Fatalf("write fake cloud: %v", err)
	}
}

func cloudResource(resourceType string, attrs map[string]any) map[string]any {
	return map[string]any{"type": resourceType, "attributes": attrs}
}

func TestPlanOnFreshProjectProposesCreatesForEverything(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: 10.20.0.0/16
  database:
    type: test.database
    engine: postgres
    network: ${network.id}
`)
	res := run(t, dir, "plan", "dev")
	if res.ExitCode != 2 {
		t.Fatalf("exit code %d, want 2 (changes present)\n%s", res.ExitCode, res.combined())
	}
	requireContains(t, res.Stdout, "+ test.network.network")
	requireContains(t, res.Stdout, "+ test.database.database")
}

func TestPlanAgainstMatchingStateReportsNoChanges(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: 10.20.0.0/16
`)
	writeM2State(t, dir, "dev", map[string]any{
		"network": stateResource("network", "test.network", "net-1", map[string]any{
			"cidr": wireAttr("string", "10.20.0.0/16", false),
			"id":   wireAttr("string", "net-1", false),
		}, nil),
	})
	writeFakeCloud(t, dir, map[string]map[string]any{
		"net-1": cloudResource("test.network", map[string]any{
			"cidr": "10.20.0.0/16",
			"id":   "net-1",
		}),
	})

	res := run(t, dir, "plan", "dev")
	if res.ExitCode != 0 {
		t.Fatalf("exit code %d, want 0 (no changes)\n%s", res.ExitCode, res.combined())
	}
	requireContains(t, res.Stdout, "No changes")
}

func TestPlanShowsExternalDriftAsAnUpdate(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: 10.20.0.0/16
  database:
    type: test.database
    engine: postgres
    size: 50
    network: ${network.id}
`)
	writeM2State(t, dir, "dev", map[string]any{
		"network": stateResource("network", "test.network", "net-1", map[string]any{
			"cidr": wireAttr("string", "10.20.0.0/16", false),
			"id":   wireAttr("string", "net-1", false),
		}, nil),
		"database": stateResource("database", "test.database", "db-1", map[string]any{
			"engine": wireAttr("string", "postgres", false),
			"size":   wireAttr("int", 50, false),
		}, nil),
	})
	writeFakeCloud(t, dir, map[string]map[string]any{
		"net-1": cloudResource("test.network", map[string]any{
			"cidr": "10.20.0.0/16",
			"id":   "net-1",
		}),
		// Someone resized the database by hand, outside infra entirely — this
		// is the mutation the drift check exists to catch.
		"db-1": cloudResource("test.database", map[string]any{
			"engine": "postgres",
			"size":   90,
		}),
	})

	res := run(t, dir, "plan", "dev")
	if res.ExitCode != 2 {
		t.Fatalf("exit code %d, want 2 (drift is a change)\n%s", res.ExitCode, res.combined())
	}
	requireContains(t, res.Stdout, "~ test.database.database")
	requireContains(t, res.Stdout, "size: 90 -> 50")
}

func TestPlanProposesDestroyForResourceRemovedFromConfiguration(t *testing.T) {
	dir := project(t, `
project: myapp
resources: {}
`)
	writeM2State(t, dir, "dev", map[string]any{
		"orphan": stateResource("orphan", "test.network", "net-99", map[string]any{
			"cidr": wireAttr("string", "10.5.0.0/16", false),
			"id":   wireAttr("string", "net-99", false),
		}, nil),
	})
	writeFakeCloud(t, dir, map[string]map[string]any{
		"net-99": cloudResource("test.network", map[string]any{
			"cidr": "10.5.0.0/16",
			"id":   "net-99",
		}),
	})

	res := run(t, dir, "plan", "dev")
	if res.ExitCode != 2 {
		t.Fatalf("exit code %d, want 2\n%s", res.ExitCode, res.combined())
	}
	requireContains(t, res.Stdout, "- test.network.orphan")
}

func TestPlanErrorsOnPreventDestroyForRemovedResource(t *testing.T) {
	dir := project(t, `
project: myapp
resources: {}
`)
	writeM2State(t, dir, "dev", map[string]any{
		"protected": stateResource("protected", "test.network", "net-42", map[string]any{
			"cidr": wireAttr("string", "10.6.0.0/16", false),
			"id":   wireAttr("string", "net-42", false),
		}, map[string]any{"PreventDestroy": true}),
	})
	writeFakeCloud(t, dir, map[string]map[string]any{
		"net-42": cloudResource("test.network", map[string]any{
			"cidr": "10.6.0.0/16",
			"id":   "net-42",
		}),
	})

	res := run(t, dir, "plan", "dev")
	if res.ExitCode != 1 {
		t.Fatalf("exit code %d, want 1 (a plan-time error, not a proposed change)\n%s", res.ExitCode, res.combined())
	}
	requireContains(t, res.Stderr, "protected")
	if res.Stdout != "" {
		t.Errorf("no plan should be rendered when prevent_destroy blocks it, got:\n%s", res.Stdout)
	}
}

func TestPlanNeverLeaksASecret(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: 10.20.0.0/16
  database:
    type: test.database
    engine: postgres
    password: hunter2
    network: ${network.id}
`)
	res := run(t, dir, "plan", "dev")
	if strings.Contains(res.combined(), "hunter2") {
		t.Errorf("plan output leaked the secret:\n%s", res.combined())
	}
	requireContains(t, res.Stdout, "<sensitive>")
}

func TestPlanOutputWritesValidJSONMode0600(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: 10.20.0.0/16
`)
	outPath := filepath.Join(dir, "plan.json")
	res := run(t, dir, "plan", "dev", "--output", outPath)
	if res.ExitCode != 2 {
		t.Fatalf("exit code %d, want 2\n%s", res.ExitCode, res.combined())
	}

	info, err := os.Stat(outPath)
	if err != nil {
		t.Fatalf("--output did not write a file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("plan file mode = %v, want 0600", info.Mode().Perm())
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading plan file: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("--output file is not valid JSON: %v", err)
	}
}

func TestPlanIsDeterministicAcrossRuns(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: 10.20.0.0/16
  database:
    type: test.database
    engine: postgres
    size: 50
    network: ${network.id}
`)
	first := run(t, dir, "plan", "dev")
	second := run(t, dir, "plan", "dev")

	if first.ExitCode != second.ExitCode {
		t.Fatalf("exit codes differ: %d vs %d", first.ExitCode, second.ExitCode)
	}
	if first.Stdout != second.Stdout {
		t.Errorf("Stdout differs between runs (invariant 6):\n--- first ---\n%s\n--- second ---\n%s", first.Stdout, second.Stdout)
	}

	// Cross-check via --output too, ignoring CreatedAt, which records when
	// each plan was produced rather than a property of its inputs (spec
	// §12.1) — this is invariant 6 as §18's table actually states it.
	out1 := filepath.Join(t.TempDir(), "plan1.json")
	out2 := filepath.Join(t.TempDir(), "plan2.json")
	run(t, dir, "plan", "dev", "--output", out1)
	run(t, dir, "plan", "dev", "--output", out2)

	p1 := readPlanIgnoringCreatedAt(t, out1)
	p2 := readPlanIgnoringCreatedAt(t, out2)
	if p1 != p2 {
		t.Errorf("saved plans differ once CreatedAt is excluded (invariant 6):\n--- first ---\n%s\n--- second ---\n%s", p1, p2)
	}
}

func readPlanIgnoringCreatedAt(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("%s is not valid JSON: %v", path, err)
	}
	delete(decoded, "CreatedAt")
	stripped, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("re-marshaling %s: %v", path, err)
	}
	return string(stripped)
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./tests/integration/ -run 'TestPlan' -v`
Expected: FAIL — `infra plan dev` does not exist as a command until Task 15 lands (`unknown command "plan"`, or a build failure if Task 15 has not been merged into this checkout yet). If Task 15 is already merged and `plan` exists, the expected failure instead comes from `planner.Compute`/`refresh.Refresh` not yet being implemented by Tasks 11 and 13 — either way, every one of these eight tests fails until the whole pipeline is wired, which is the point of an integration suite.

- [ ] **Step 3: Run the tests to verify they pass**

Run: `go test ./tests/integration/ -v`
Expected: PASS — all M1 and M2 integration tests green together. A failure here after every unit-level package is green means a wiring problem between packages, which is exactly what this suite exists to find.

- [ ] **Step 4: Run everything**

Run: `make check`
Expected: all packages green, no vet findings, no formatting differences.

- [ ] **Step 5: Commit**

```bash
git add tests/integration/m2_test.go
git commit -m "$(cat <<'EOF'
test: add M2 integration tests driving the CLI

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_015M5QdCFKa6booSVrhnAzTV
EOF
)"
```

---
## Task 17: Let fake-provider operations overlap

**Files:**
- Modify: `providers/test/provider.go`
- Test: `providers/test/provider_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces: no signature changes. This task changes only *when* the provider holds its lock.

M1's final review found `Cloud.Save` was a non-atomic whole-file write called on every operation, and the fix took a `sync.Mutex` on `*Provider` at the top of all four CRUD methods. That was right about atomicity and wrong about extent: `begin()` sleeps for the cloud file's injected latency **while holding the mutex**, so the fake provider now serialises completely. No caller concurrency can overlap two operations.

That is a latent hole rather than a live bug. M2's refresh reads concurrently and M3's executor is *built* on bounded concurrency, and spec §18 requires concurrency tests for both. Against a fully serial provider those tests pass while proving nothing — the same "test that cannot fail" shape this project has already shipped twice. Fixing it now costs about ten lines; fixing it in M3 means first noticing that a green concurrency suite was meaningless.

The lock exists to protect the load-mutate-save cycle against the file, not to serialise simulated latency. Move the delay out of the critical section.

- [ ] **Step 1: Write the failing test**

Add to `providers/test/provider_test.go`:

```go
func TestOperationsOverlapRatherThanSerialise(t *testing.T) {
	// The mutex exists to protect the cloud file, not to serialise simulated
	// latency. If it covers the delay, every concurrent test in M2 and M3
	// passes while proving nothing.
	p, path := newTestProvider(t)
	ctx := context.Background()

	const (
		resources = 4
		delayMS   = 150
	)

	states := make([]*resource.ResourceState, 0, resources)
	for i := 0; i < resources; i++ {
		st, err := p.Create(ctx, desired(fmt.Sprintf("net%d", i), "test.network", map[string]value.Value{
			"cidr": value.String("10.0.0.0/16", value.SourceExplicit),
		}))
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		states = append(states, st)
	}

	// Introduce latency only now, so setup is not slowed.
	c, err := LoadCloud(path)
	if err != nil {
		t.Fatalf("LoadCloud: %v", err)
	}
	c.LatencyMS = delayMS
	if err := c.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	start := time.Now()
	var wg sync.WaitGroup
	errs := make([]error, resources)
	for i, st := range states {
		wg.Add(1)
		go func(i int, st *resource.ResourceState) {
			defer wg.Done()
			_, errs[i] = p.Read(ctx, st)
		}(i, st)
	}
	wg.Wait()
	elapsed := time.Since(start)

	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent Read %d: %v", i, err)
		}
	}

	// Serial execution takes at least resources*delay. Overlapping execution
	// takes roughly one delay. The midpoint is a generous threshold that is
	// not sensitive to scheduling noise.
	serial := time.Duration(resources) * delayMS * time.Millisecond
	if elapsed >= serial/2 {
		t.Errorf("%d concurrent reads with %dms latency took %v; serial would be ~%v. "+
			"The provider is serialising — the mutex is covering the delay, so no "+
			"concurrency test against this provider can fail.", resources, delayMS, elapsed, serial)
	}
}
```

Add `"fmt"`, `"sync"` and `"time"` to the test file's imports if they are not already present.

- [ ] **Step 2: Run the test to verify it fails**

Run: `export PATH="$HOME/.local/share/mise/shims:$PATH" && go test ./providers/test/ -run TestOperationsOverlap -v`

Expected: FAIL, reporting roughly 600ms elapsed against a ~300ms threshold — four 150ms reads running one after another. That number *is* the defect: it is the provider proving it cannot overlap.

- [ ] **Step 3: Move the delay out of the critical section**

In `providers/test/provider.go`, split the delay out of `begin()` so callers can wait without holding the lock. Replace `begin` with:

```go
// delay applies the cloud file's simulated latency. It is deliberately outside
// the mutex: the lock protects the load-mutate-save cycle against the file, and
// holding it across a sleep would serialise the whole provider, silently
// disarming every concurrency test in M2 and M3.
func (p *Provider) delay(ctx context.Context) error {
	c, err := LoadCloud(p.cloudPath)
	if err != nil {
		return err
	}
	d := c.Delay()
	if d <= 0 {
		return nil
	}
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// begin loads the cloud and applies failure injection. Callers hold p.mu.
func (p *Provider) begin(ctx context.Context, op, addr string) (*Cloud, error) {
	c, err := LoadCloud(p.cloudPath)
	if err != nil {
		return nil, err
	}
	rule, failing := c.ShouldFail(op, addr)
	// ShouldFail advances persisted bookkeeping whenever a rule matches its op
	// and address, so the cloud is written back regardless of outcome. Saving
	// only on failure would reset a non-firing rule's Seen counter on the next
	// load, and an Nth greater than 1 could never be reached.
	if err := c.Save(p.cloudPath); err != nil {
		return nil, err
	}
	if failing {
		msg := rule.Message
		if msg == "" {
			msg = fmt.Sprintf("injected %s failure for %s", op, addr)
		}
		return nil, &ErrInjected{Message: msg, Retryability: rule.Retryability}
	}
	return c, nil
}
```

- [ ] **Step 4: Call delay before taking the lock in all four CRUD methods**

Each of `Create`, `Read`, `Update` and `Delete` currently opens with:

```go
	p.mu.Lock()
	defer p.mu.Unlock()

	c, err := p.begin(ctx, "<op>", <addr>)
```

Change each to wait first, then lock:

```go
	if err := p.delay(ctx); err != nil {
		return nil, err   // Delete returns just err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	c, err := p.begin(ctx, "<op>", <addr>)
```

Keep every operation's existing op string and address argument exactly as they are. `Delete` returns only an error, so its early return is `return err`.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./providers/test/ -v` then `go test ./providers/test/ -race -count=2`

Expected: PASS. Every M1 test must still pass unchanged — in particular `TestNthReadRuleSurvivesAcrossOperations`, which proves failure-rule bookkeeping survives the reload cycle, and the two concurrency tests M1's fix wave added. If any of them now fails, the delay was moved somewhere that changed the load-mutate-save ordering; report it rather than adjusting the test.

- [ ] **Step 6: Confirm the atomicity guarantee is untouched**

Run: `grep -n 'CreateTemp\|Rename\|mu.Lock' providers/test/cloud.go providers/test/provider.go`

`Cloud.Save` must still write via temp-file-then-rename, and all four CRUD methods must still take `p.mu` around `begin` and their own save. Only the *delay* moved. State in your report which lines you checked.

- [ ] **Step 7: Commit**

```bash
git add providers/test
git commit -m "fix: stop the fake provider serialising on simulated latency

The mutex added in M1's fix wave was taken at the top of each CRUD method
and held across begin()'s injected-latency sleep, so no two provider
operations could overlap. The lock exists to protect the load-mutate-save
cycle against the cloud file, not to serialise a simulated delay - and a
fully serial provider silently disarms every concurrency test in M2's
refresh and M3's executor.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_015M5QdCFKa6booSVrhnAzTV"
```

---
## Task 18: Give the graph its purpose, and delete the second cycle detector

**Files:**
- Create: `internal/planner/execution.go`, `internal/planner/execution_test.go`
- Modify: `internal/compiler/validate.go` (delete `firstCycle`, use `graph.Cycle`), `internal/compiler/validate_test.go` (one added characterisation test; existing expectations unchanged)

**Interfaces:**
- Consumes: `graph.New`, `(*graph.Graph[T]).Add/Edge/Cycle/Layers/Roots` (Task 10); `Plan`, `Operation`, `OpKind` constants (Tasks 12–13, same package); `address.Address`.
- Produces: `planner.OpNode{Address address.Address; Kind OpKind; Phase Phase}` with `ID() string`; `planner.Phase` with `PhaseDestroy` and `PhaseCreate`; `planner.BuildExecution(p *Plan, deps func(address.Address) []address.Address) (*graph.Graph[OpNode], error)`.

This task exists because the plan's self-review found two things wrong with it, and both are mine.

**The graph had no consumer.** Task 10 builds a correct generic DAG, and nothing in M2 called it. Spec §19 assigns "graph" to M2, but spec §14's actual content — that nodes are *plan operations*, and that ordering differs by operation kind — was unimplemented. A graph package with no consumer is a package whose correctness nobody has tested against a real question.

**And the codebase would have shipped two cycle detectors.** Task 8's `validateGraph` needs cycle detection and precedes Task 10, so it grew a private three-colour DFS (`findCycles`). Task 10 then built `Graph.Cycle()`. Same algorithm, two implementations, one codebase — the shape a review rightly calls out, and worse than ordinary duplication because a divergence between them means configuration that one accepts and the other rejects.

**The execution graph lives in `internal/planner`, not `internal/graph`.** An earlier draft of this task put `BuildExecution`, `OpNode` and `Phase` in package `graph` — which would have made `internal/graph` import `internal/planner`, destroying the one property Task 10 was built for. Task 10's whole justification is that `graph` is a leaf: it depends on nothing in `infra`, which is what lets M3's executor and M7's `infra graph` reuse it without dragging the planner along. A graph package that imports the planner is not reusable by anything below the planner.

So the dependency runs the only direction that keeps both packages honest: `planner` imports `graph`. `graph` stays generic over `Node`, knowing nothing about operations; `planner` owns the knowledge that nodes are operations and that destroy runs in reverse. M3's executor imports `planner` — which it must anyway, to read a `Plan`.

One consequence to watch while writing the tests: `execution_test.go` is in package `planner` alongside Task 12's `plan_test.go`, so helpers do not get redeclared. `addr` is already declared there; reuse it rather than defining a second one, which will not compile.

Spec §14 fixes the ordering rules, and conflating them causes apply-time failures:

- `create(A)` before `create(B)` when B depends on A
- `destroy(B)` before `destroy(A)` when B depends on A — **reverse** order
- `replace(A)` is `destroy(A)` then `create(A)`, with dependents ordered around both

A replacement is therefore two nodes, not one, which is why `OpNode` carries a `Phase`. Phase 1 implements destroy-then-create only; create-before-destroy is Phase 5.

The `deps` parameter is a function rather than a config reference because the two sides need different sources, exactly as spec §14 says: create-side edges come from configuration, and destroy-side edges come from the `Dependencies` recorded in state — a resource being destroyed may no longer be in configuration at all. `Operation.Dependents` already carries the resolved answer, so the caller supplies a lookup over it rather than the graph reaching into either.

No executor consumes this in M2; M3's does. Building and testing it here means M3 inherits proven ordering rather than writing it under the pressure of also writing concurrency.

- [ ] **Step 1: Write the failing test**

Create `internal/graph/build_test.go`:

```go
package planner

import (
	"strings"
	"testing"

	"infra/internal/graph"
	"infra/pkg/address"
)

// addr is NOT redefined here: plan_test.go (Task 12) already declares it in
// this package, and a second declaration will not compile.

// depsFrom builds the lookup BuildExecution needs from a plain map.
func depsFrom(m map[string][]string) func(address.Address) []address.Address {
	return func(a address.Address) []address.Address {
		var out []address.Address
		for _, name := range m[a.Name] {
			out = append(out, addr(name))
		}
		return out
	}
}

func planWith(ops ...Operation) *Plan {
	return &Plan{Version: PlanVersion, Operations: ops}
}

func orderOf(t *testing.T, g *graph.Graph[OpNode]) []string {
	t.Helper()
	layers, err := g.Layers()
	if err != nil {
		t.Fatalf("Layers: %v", err)
	}
	var out []string
	for _, layer := range layers {
		for _, n := range layer {
			out = append(out, n.ID())
		}
	}
	return out
}

func indexOf(t *testing.T, order []string, id string) int {
	t.Helper()
	for i, got := range order {
		if got == id {
			return i
		}
	}
	t.Fatalf("%q not found in %v", id, order)
	return -1
}

func TestCreatesRunAfterWhatTheyDependOn(t *testing.T) {
	p := planWith(
		Operation{Address: addr("network"), Type: "test.network", Kind: OpCreate},
		Operation{Address: addr("database"), Type: "test.database", Kind: OpCreate},
	)
	// network has one dependent: database.
	g, err := BuildExecution(p, depsFrom(map[string][]string{"network": {"database"}}))
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}

	order := orderOf(t, g)
	if indexOf(t, order, "create:network") > indexOf(t, order, "create:database") {
		t.Errorf("order = %v; a resource must be created after what it depends on", order)
	}
}

func TestDestroysRunInReverseDependencyOrder(t *testing.T) {
	p := planWith(
		Operation{Address: addr("network"), Type: "test.network", Kind: OpDestroy},
		Operation{Address: addr("database"), Type: "test.database", Kind: OpDestroy},
	)
	g, err := BuildExecution(p, depsFrom(map[string][]string{"network": {"database"}}))
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}

	order := orderOf(t, g)
	if indexOf(t, order, "destroy:database") > indexOf(t, order, "destroy:network") {
		t.Errorf("order = %v; a dependent must be destroyed BEFORE what it depends on — "+
			"destroying the network first would strand the database", order)
	}
}

func TestReplaceBecomesTwoNodesDestroyThenCreate(t *testing.T) {
	p := planWith(
		Operation{Address: addr("database"), Type: "test.database", Kind: OpReplace},
	)
	g, err := BuildExecution(p, depsFrom(nil))
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}

	order := orderOf(t, g)
	if len(order) != 2 {
		t.Fatalf("order = %v, want two nodes for a replacement", order)
	}
	if indexOf(t, order, "destroy:database") > indexOf(t, order, "create:database") {
		t.Errorf("order = %v; Phase 1 replacement is destroy-then-create", order)
	}
}

func TestReplaceOrdersDependentsAroundBothPhases(t *testing.T) {
	// Replacing a network with an application on top: the app must be
	// destroyed before the network's destroy, and created after its create.
	p := planWith(
		Operation{Address: addr("network"), Type: "test.network", Kind: OpReplace},
		Operation{Address: addr("app"), Type: "test.application", Kind: OpReplace},
	)
	g, err := BuildExecution(p, depsFrom(map[string][]string{"network": {"app"}}))
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}

	order := orderOf(t, g)
	if indexOf(t, order, "destroy:app") > indexOf(t, order, "destroy:network") {
		t.Errorf("order = %v; the dependent's destroy must precede its dependency's destroy", order)
	}
	if indexOf(t, order, "create:network") > indexOf(t, order, "create:app") {
		t.Errorf("order = %v; the dependency's create must precede its dependent's create", order)
	}
}

func TestNoOpsAreNotScheduled(t *testing.T) {
	p := planWith(
		Operation{Address: addr("network"), Type: "test.network", Kind: OpNoOp},
		Operation{Address: addr("database"), Type: "test.database", Kind: OpCreate},
	)
	g, err := BuildExecution(p, depsFrom(nil))
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}
	order := orderOf(t, g)
	if len(order) != 1 || order[0] != "create:database" {
		t.Errorf("order = %v; a no-op is not work and must not be scheduled", order)
	}
}

func TestForgetIsScheduledWithoutAProviderCall(t *testing.T) {
	// Forget still removes the resource from state, so it is ordered like a
	// destroy even though no provider is called.
	p := planWith(
		Operation{Address: addr("database"), Type: "test.database", Kind: OpForget},
	)
	g, err := BuildExecution(p, depsFrom(nil))
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}
	if order := orderOf(t, g); len(order) != 1 || order[0] != "forget:database" {
		t.Errorf("order = %v, want one forget node", order)
	}
}

func TestOrderingIsDeterministic(t *testing.T) {
	p := planWith(
		Operation{Address: addr("c"), Type: "test.network", Kind: OpCreate},
		Operation{Address: addr("a"), Type: "test.network", Kind: OpCreate},
		Operation{Address: addr("b"), Type: "test.network", Kind: OpCreate},
		Operation{Address: addr("d"), Type: "test.network", Kind: OpCreate},
	)
	deps := depsFrom(nil)

	g, err := BuildExecution(p, deps)
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}
	first := strings.Join(orderOf(t, g), ",")

	for i := 0; i < 20; i++ {
		g, err := BuildExecution(p, deps)
		if err != nil {
			t.Fatalf("BuildExecution: %v", err)
		}
		if got := strings.Join(orderOf(t, g), ","); got != first {
			t.Fatalf("ordering varies between runs: %q then %q", first, got)
		}
	}
}

func TestCycleAmongOperationsIsAnError(t *testing.T) {
	p := planWith(
		Operation{Address: addr("a"), Type: "test.network", Kind: OpCreate},
		Operation{Address: addr("b"), Type: "test.network", Kind: OpCreate},
	)
	// a depends on b and b depends on a.
	g, err := BuildExecution(p, depsFrom(map[string][]string{"a": {"b"}, "b": {"a"}}))
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}
	if cycle := g.Cycle(); len(cycle) == 0 {
		t.Error("a cycle among operations must be detectable before execution")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `export PATH="$HOME/.local/share/mise/shims:$PATH" && go test ./internal/graph/ -run 'TestCreates|TestDestroys|TestReplace|TestNoOps|TestForget|TestOrdering|TestCycleAmong' -v`
Expected: FAIL — `undefined: OpNode`, `undefined: BuildExecution`.

- [ ] **Step 3: Implement the execution graph**

Create `internal/graph/build.go`:

```go
package planner

import (
	"infra/internal/graph"
	"infra/pkg/address"
)

// Phase distinguishes the two halves of a replacement, which is the one
// operation that becomes two nodes.
type Phase uint8

const (
	// PhaseDestroy removes an object.
	PhaseDestroy Phase = iota
	// PhaseCreate builds one.
	PhaseCreate
)

// OpNode is one unit of executable work.
type OpNode struct {
	Address address.Address
	Kind    OpKind
	Phase   Phase
}

// ID identifies the node uniquely, and readably enough to name in a test
// failure or a diagnostic.
func (n OpNode) ID() string {
	switch n.Kind {
	case OpReplace:
		if n.Phase == PhaseDestroy {
			return "destroy:" + n.Address.String()
		}
		return "create:" + n.Address.String()
	case OpCreate:
		return "create:" + n.Address.String()
	case OpUpdate:
		return "update:" + n.Address.String()
	case OpDestroy:
		return "destroy:" + n.Address.String()
	case OpForget:
		return "forget:" + n.Address.String()
	default:
		return "noop:" + n.Address.String()
	}
}

// BuildExecution turns a plan into an execution graph.
//
// Ordering differs by operation kind, and conflating the two directions causes
// apply-time failures (spec §14):
//
//   - a create runs after the creates of what it depends on
//   - a destroy runs BEFORE the destroys of what it depends on — reverse order,
//     because destroying a dependency first would strand its dependents
//   - a replacement is a destroy then a create, with dependents ordered around
//     both halves
//
// deps reports the resources that depend on a given address. The caller
// supplies it because the two sides draw from different places: create-side
// edges come from configuration, destroy-side edges from the Dependencies
// recorded in state, since a resource being destroyed may no longer appear in
// configuration at all. Operation.Dependents already holds the resolved
// answer.
func BuildExecution(p *Plan, deps func(address.Address) []address.Address) (*graph.Graph[OpNode], error) {
	g := graph.New[OpNode]()
	if p == nil {
		return g, nil
	}

	// Which phases exist for each address, so edges are only drawn to nodes
	// that were actually added.
	has := map[string]map[Phase]bool{}

	add := func(n OpNode) {
		g.Add(n)
		key := n.Address.String()
		if has[key] == nil {
			has[key] = map[Phase]bool{}
		}
		has[key][n.Phase] = true
	}

	for _, op := range p.Operations {
		switch op.Kind {
		case OpNoOp:
			// Not work. Scheduling it would make every plan look busy and
			// would put unchanged resources in the executor's path.
			continue
		case OpReplace:
			add(OpNode{Address: op.Address, Kind: op.Kind, Phase: PhaseDestroy})
			add(OpNode{Address: op.Address, Kind: op.Kind, Phase: PhaseCreate})
		case OpDestroy, OpForget:
			add(OpNode{Address: op.Address, Kind: op.Kind, Phase: PhaseDestroy})
		default: // OpCreate, OpUpdate
			add(OpNode{Address: op.Address, Kind: op.Kind, Phase: PhaseCreate})
		}
	}

	for _, op := range p.Operations {
		if op.Kind == OpNoOp {
			continue
		}
		for _, dependent := range deps(op.Address) {
			addEdges(g, has, op.Address, dependent)
		}
	}

	return g, nil
}

// addEdges wires one dependency relationship — dependent depends on target —
// in both directions of work that exist.
func addEdges(g *graph.Graph[OpNode], has map[string]map[Phase]bool, target, dependent address.Address) {
	t, d := target.String(), dependent.String()

	// Build side: the target is created before its dependent is.
	if has[t][PhaseCreate] && has[d][PhaseCreate] {
		g.Edge(createID(t), createID(d))
	}
	// Teardown side: the dependent is destroyed before the target is.
	if has[d][PhaseDestroy] && has[t][PhaseDestroy] {
		g.Edge(destroyID(d), destroyID(t))
	}
	// A dependent being torn down must go before the target is rebuilt.
	if has[d][PhaseDestroy] && has[t][PhaseCreate] {
		g.Edge(destroyID(d), createID(t))
	}
}

func createID(addr string) string  { return "create:" + addr }
func destroyID(addr string) string { return "destroy:" + addr }
```

Note the `ID()` switch renders a destroy-phase node of an `OpForget` as `forget:`, so `destroyID` is not used for forgets. Edges to a forget node are therefore not drawn by `addEdges`; a forget touches no provider and removes only a state entry, so its ordering against other work does not matter. Say so in your report if you disagree — it is a judgement, not a mechanic.

- [ ] **Step 4: Run the new tests to verify they pass**

Run: `go test ./internal/graph/ -v`
Expected: PASS — eight new tests plus Task 10's.

- [ ] **Step 5: Delete the duplicate cycle detector**

`internal/compiler/validate.go` grew a private three-colour DFS because Task 8 precedes Task 10. Now that `graph.Cycle()` exists and is tested, the compiler must not carry a second implementation: two cycle detectors mean configuration one accepts and the other rejects.

**The function is named `firstCycle`, not `findCycles`.** Task 8 shipped `findCycles`; a fix round during Task 8 narrowed it to return one cycle and renamed it (commit `a56d86f`). Delete `firstCycle`, the `color` type, and the `white`/`gray`/`black` constants.

**Two shape differences have to be bridged, and getting either wrong is silent.**

*Direction.* `firstCycle` walked `r.DependsOn` as an edge from the resource to its dependency, so the reported sequence reads in depends-on order — `a → b → d` means "a depends on b depends on d". The planner's graph runs the other way (a dependency must execute first). `cycleFor` exists only to feed the diagnostic, so it builds edges in depends-on direction, and a comment says why. Building it in execution direction detects the same cycle but prints it backwards, and no existing test would notice.

*The closing repeat.* `firstCycle` returned the cycle with its first address repeated at the end; `graph.Cycle()` returns it without. This is not cosmetic — `cycleDiagnostic` computes `Related: cycle[1 : len(cycle)-1]`, which assumes the repeat is there. Hand it an unwrapped slice and it silently drops the last genuine member of the cycle from `Related`, and for a two-node cycle it yields an empty `Related` instead of the one other member. `cycleFor` therefore re-adds the repeat, restoring exactly the slice `cycleDiagnostic` already expects, and `cycleDiagnostic` is left untouched.

In `internal/compiler/validate.go`, add `"infra/internal/graph"` to the import block — it currently imports `strconv`, `strings`, `infra/internal/diag`, `infra/internal/registry`, `infra/pkg/address` and `infra/pkg/value`, all of which stay. `strconv` and `address` are both already there and both are used by the code below, so no other import changes.

Then:

```go
// cycleFor reports a dependency cycle in the resolved configuration, or nil.
// Detection lives in internal/graph so the compiler and the executor cannot
// disagree about what a cycle is.
//
// Edges run resource → dependency, the direction the diagnostic reads in
// ("a → b" means a depends on b), which is the opposite of the planner's
// graph, where an edge means "must execute first". Both orientations detect
// the same cycles; only this one prints in the order the message claims.
//
// graph.Cycle returns members without repeating the first at the end. The
// closing repeat is re-added here because cycleDiagnostic slices
// cycle[1:len(cycle)-1] for its Related list and would otherwise drop a real
// member of the cycle.
func cycleFor(cfg *ResolvedConfig) []address.Address {
	g := graph.New[addrNode]()
	for _, addr := range cfg.Addresses() {
		g.Add(addrNode{addr})
	}
	for _, addr := range cfg.Addresses() {
		r := cfg.Resources[addr.String()]
		for _, dep := range r.DependsOn {
			if _, ok := cfg.Get(dep); !ok {
				continue // not a node in this graph; stage 6 already reported it
			}
			g.Edge(addr.String(), dep.String())
		}
	}

	ids := g.Cycle()
	if len(ids) == 0 {
		return nil
	}

	out := make([]address.Address, 0, len(ids)+1)
	for _, id := range ids {
		addr, err := address.Parse(id)
		if err != nil {
			// Impossible: every id came from an Address we put in.
			panic("compiler: graph returned an unparseable address " + strconv.Quote(id) + ": " + err.Error())
		}
		out = append(out, addr)
	}
	return append(out, out[0])
}

// addrNode adapts an address to the graph's Node interface.
type addrNode struct{ addr address.Address }

func (n addrNode) ID() string { return n.addr.String() }
```

Note the skip for a dependency that is not a node: `firstCycle` had it, and without it `g.Edge` panics on a reference stage 6 already reported as unresolved — turning a reported user error into a crash.

`validateGraph`'s cycle branch changes only its call:

```go
	if cycle := cycleFor(cfg); cycle != nil {
		ds.Add(cycleDiagnostic(cfg, cycle))
	}
```

`cycleDiagnostic` and `cycleKey` are not touched.

- [ ] **Step 5b: Pin the diagnostic's shape BEFORE the swap**

The existing cycle test asserts only that every member is *named somewhere* in the output. That cannot catch either shape difference above: a backwards sequence names the same members, and a dropped closing repeat only shortens `Related`. Write this test first, run it against the **current** `firstCycle` implementation to confirm it passes, and only then perform the swap — it is the characterisation test that makes "behaviour-preserving" a checkable claim rather than an assertion.

Append to `internal/compiler/validate_test.go`:

```go
// TestCycleDiagnosticShapeIsStable pins the two properties that survive the
// move to internal/graph but that the membership assertion above cannot see:
// the sequence reads in depends-on order and closes back on its first member,
// and Related therefore names every member except the one carrying Origin.
func TestCycleDiagnosticShapeIsStable(t *testing.T) {
	a := res("a", "test.network", map[string]value.Value{"cidr": value.String("10.0.0.0/16", value.SourceExplicit)})
	b := res("b", "test.network", map[string]value.Value{"cidr": value.String("10.0.1.0/16", value.SourceExplicit)})
	c := res("c", "test.network", map[string]value.Value{"cidr": value.String("10.0.2.0/16", value.SourceExplicit)})
	a.DependsOn = []address.Address{{Name: "b"}}
	b.DependsOn = []address.Address{{Name: "c"}}
	c.DependsOn = []address.Address{{Name: "a"}}
	graph := cfg(a, b, c)

	ds := validateGraph(&graph, testRegistry(t))
	var d diag.Diagnostic
	for _, cand := range ds {
		if strings.HasPrefix(cand.Summary, "dependency cycle: ") {
			d = cand
		}
	}
	if d.Summary == "" {
		t.Fatalf("no cycle diagnostic: %+v", ds)
	}

	// a depends on b depends on c depends on a: the sequence reads in that
	// order and closes on a. Reversed, it would read "a → c → b → a" and be
	// a false statement about the configuration.
	if want := "dependency cycle: a → b → c → a"; d.Summary != want {
		t.Errorf("summary = %q, want %q", d.Summary, want)
	}

	// Related is every member but the first, which carries Origin. Without
	// the closing repeat this slice silently loses c.
	if len(d.Related) != 2 {
		t.Fatalf("Related = %v, want 2 entries (b and c)", d.Related)
	}
	for i, want := range []string{"b", "c"} {
		if got := d.Related[i].String(); got != want {
			t.Errorf("Related[%d] = %q, want %q", i, got, want)
		}
	}
}
```

Run: `go test ./internal/compiler/ -run TestCycleDiagnosticShapeIsStable -v`
Expected: **PASS against the unmodified code.** This test is a characterisation of behaviour that must not change — it is green before the swap and must stay green after. If it fails before you touch anything, the assumption this task is built on is wrong: stop and report which property differs rather than editing the test to match.

- [ ] **Step 6: Confirm the compiler's cycle tests still pass unchanged**

Run: `go test ./internal/compiler/ -run Cycle -v`

Expected: PASS with **no edits to `validate_test.go`**. If a test needed changing, the replacement altered behaviour rather than removing duplication — report it rather than adjusting the test.

Then confirm the duplicate is gone:

Run: `grep -rn 'func firstCycle\|func findCycles\|white color\|gray\|black' internal/compiler/`
Expected: no output.

(The old text of this step grepped for `func findCycles`, a name that no longer exists — a check that passes whether or not the duplicate was removed. If a verification step cannot fail, it is not verifying anything.)

- [ ] **Step 7: Run everything**

Run: `make check` then `go test ./... -race`
Expected: all green.

- [ ] **Step 8: Commit**

```bash
git add internal/planner/execution.go internal/planner/execution_test.go internal/compiler/validate.go internal/compiler/validate_test.go
git commit -m "feat: order plan operations by kind, and drop the duplicate cycle detector

The graph package had no consumer: spec §14's actual content, that nodes are
plan operations and ordering differs by kind, was unimplemented. Add
BuildExecution, which orders creates forward, destroys in reverse, and splits
a replacement into destroy-then-create with dependents ordered around both.

The compiler had grown a private cycle detector because validation was
written before the graph existed. Two detectors mean configuration one
accepts and the other rejects, so the compiler now uses graph.Cycle.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_015M5QdCFKa6booSVrhnAzTV"
```

---

## M2 Definition of Done

Every box must be true before M3 begins.

- [ ] `make check` is green from a clean checkout, and `go test ./... -race` is clean.
- [ ] `go list -deps ./providers/... | grep '^infra/internal'` prints nothing.
- [ ] No package outside `internal/config` references `yaml.Node`.
- [ ] `infra plan dev` on a fresh project proposes a create for every resource and exits `2`.
- [ ] `infra plan dev` against state that matches configuration reports no changes and exits `0`.
- [ ] Editing `.infra/fake-cloud.json` by hand and re-planning shows the drift as an update.
- [ ] Removing a resource from configuration proposes destroying it.
- [ ] `prevent_destroy` on a removed resource produces a plan-time error, and the plan contains no operation for it.
- [ ] A destructive operation renders the count of resources that depend on it, and omits the line when there are none.
- [ ] A sensitive value never appears in `plan` output for any operation kind, at any nesting depth.
- [ ] Values that came from a provider default render annotated `[default]`.
- [ ] Unknown values render `(known after apply)`.
- [ ] `infra plan dev --output plan.json` writes valid JSON at mode `0600`.
- [ ] **Invariant 6:** twenty consecutive `Compute` calls over identical inputs produce byte-identical `Canonical()` output, and two `infra plan` runs produce identical stdout.
- [ ] Four concurrent fake-provider reads with injected latency complete in roughly one delay, not four.
- [ ] `graph.BuildExecution` orders creates forward, destroys in reverse, and splits a replacement into destroy-then-create with dependents ordered around both halves.
- [ ] Exactly one cycle detector exists in the tree: `grep -rn 'func findCycles' internal/` prints nothing, and `internal/compiler` uses `graph.Cycle`.

## What M2 deliberately leaves undone

Named here so a reviewer does not read them as gaps:

- **No `apply`, `destroy` or `refresh` command.** M3. `refresh` the *function* exists and `plan` uses it; only M3's command persists observations.
- **No `infra graph` command.** M7. The graph package exists and the planner uses it.
- **No applying a saved plan.** M6. `--output` writes one; nothing reads it back, and there is no staleness check or `--allow-stale`.
- **No `explain`, `import`, `export` or `discover`.** M7 and Phase 2.
- **No variables file, environment inheritance or modules.** M4 and M5. `--var` works; `variables.yml` and `environments/*.yml` are still unread.
- **Create-before-destroy replacement.** Phase 5. Replacement is destroy-then-create.
- **Requirement satisfaction from state.** Spec §8.1 allows a requirement to be satisfied by an existing resource in state; the compiler is a pure function of configuration and has no state. Only the configuration half is checked. See the carry-forward below.

## Carry-forwards into M3

Three things this milestone discovered but does not fix. Each was found during authoring, and each is recorded so M3 inherits the knowledge rather than rediscovering it.

**Requirement satisfaction is weaker than the spec describes.** Spec §8.1 says a requirement is satisfied "by a reference in configuration to a resource of a listed type", but `schema.Requirement{Name, Types, Optional, Description}` carries no attribute name, so there is no edge to trace. Stage 8 therefore checks existence anywhere in configuration: two unrelated resources that each need a network are both satisfied by one network existing, even if neither refers to it. That is the right call for M2 and it catches the case the feature exists for, but it gets weaker as providers grow — Phase 3's ECS service declares five requirements. Fixing it means adding `Attribute string` to `schema.Requirement`, which is a schema change and a spec amendment. The state-aware half of the rule has no home until the planner, which is M3's.

**Invariant 2's wording versus `OpNoOp`.** Spec §3 says "desired equals actual ⇒ plan contains zero operations", while spec §12.1 defines `OpNoOp` as a valid operation kind. Taken strictly the second is dead. M2 reads invariant 2 as zero *changing* operations, keeps `NoOp` entries in `Operations`, and exposes `HasChanges()` — `--verbose` rendering needs the unchanged resources. This should become an explicit spec clarification rather than being re-litigated in M3.

**`Local.Put` does not check that a lock is held.** M1's final review raised it and M2 does not touch it, because `plan` deliberately never writes state and never locks. M3's executor is the first thing to write state under a lock, and nothing structurally prevents it writing outside one. It belongs in M3's first task, not its last.
