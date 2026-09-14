# Reference Grammar Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the segment-counting rule that distinguishes a variable from a resource attribute with a `var.` namespace, and add paths and list indices to both kinds of reference.

**Architecture:** The parser gains one concept — a `Path []Step` on `value.Reference`, where a step is a map key or a list index — and loses `parse.go:478`'s segment arithmetic. `var` becomes the first segment of every variable reference, so a bare first segment is always a resource. The prefix is stripped at parse time, so `variables.Scope` and every stage below it are untouched. Landing is expand → migrate → contract: accept both spellings, rewrite the repository, then remove the old spelling.

**Tech Stack:** Go 1.27, `gopkg.in/yaml.v3`, Cobra. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-09-14-reference-grammar-design.md`

## Global Constraints

- **Go 1.27**, pinned by `mise.toml`. `mise` is NOT active in non-interactive shells: every command below assumes `export PATH="$HOME/.local/share/mise/shims:$PATH"` has been run, or use the `make` targets. A bare `go` resolves to 1.20 and fails.
- **Third-party budget is Cobra and `gopkg.in/yaml.v3`, and that is all.** Add no dependency.
- **Every diagnostic takes the §44 shape** — `Summary`, `Detail`, `Action`, `Origin` — and names the fix rather than the symptom.
- **Diagnostics are order-stable** (invariant 6). Identical input produces byte-identical output on every run; never iterate a Go map to produce them.
- **Nothing in `pkg/schema` gains a function-typed field.** Not touched by this plan; do not be tempted.
- **Tasks 1-5 are the expand phase.** Both spellings work throughout. The repository is NOT releasable between Task 5 and Task 7.
- **Never rewrite `docs/superpowers/plans/` or `.superpowers/sdd/`.** They are historical records; syntax invented after them must not appear in them.

---

### Task 1: `value.Step` and `Reference.Path`

The data model, with no parser or evaluator touching it yet. A reference gains a path; rendering it round-trips.

**Files:**
- Modify: `pkg/value/expr.go`
- Test: `pkg/value/expr_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `value.StepKind` (`value.StepKey`, `value.StepIndex`), `value.Step{Kind StepKind; Key string; Index int}`, `Step.String() string`, `Reference.Path []Step`, and `Reference.String()` rendering the path.

- [ ] **Step 1: Write the failing test**

Add to `pkg/value/expr_test.go`:

```go
func TestAStepRendersAsItWasWritten(t *testing.T) {
	if got := (Step{Kind: StepKey, Key: "team"}).String(); got != ".team" {
		t.Errorf("Step.String() = %q, want %q", got, ".team")
	}
	if got := (Step{Kind: StepIndex, Index: 0}).String(); got != "[0]" {
		t.Errorf("Step.String() = %q, want %q", got, "[0]")
	}
}

func TestAReferenceRendersItsPath(t *testing.T) {
	// A resource attribute with a path: the form ${vpc.tags.Name}.
	r := Reference{
		Target:    address.Address{Name: "vpc"},
		Attribute: "tags",
		Path:      []Step{{Kind: StepKey, Key: "Name"}},
	}
	if got := r.String(); got != "vpc.tags.Name" {
		t.Errorf("String() = %q, want %q", got, "vpc.tags.Name")
	}

	// A variable with a mixed path: the form ${var.subnets[0].cidr}. Under
	// OpVarRef the Attribute is empty and the path carries everything.
	v := Reference{
		Target: address.Address{Name: "subnets"},
		Path:   []Step{{Kind: StepIndex, Index: 0}, {Kind: StepKey, Key: "cidr"}},
	}
	if got := v.String(); got != "subnets[0].cidr" {
		t.Errorf("String() = %q, want %q", got, "subnets[0].cidr")
	}
}

func TestTwoPathsIntoOneAttributeAreTwoReferences(t *testing.T) {
	// References() dedups on String(), so a path must be part of the key or
	// ${vpc.tags.Name} and ${vpc.tags.Env} collapse into one and the second
	// silently resolves to the first.
	e := &Expr{Op: OpConcat, Args: []*Expr{
		{Op: OpResourceRef, Ref: Reference{Target: address.Address{Name: "vpc"}, Attribute: "tags",
			Path: []Step{{Kind: StepKey, Key: "Name"}}}},
		{Op: OpResourceRef, Ref: Reference{Target: address.Address{Name: "vpc"}, Attribute: "tags",
			Path: []Step{{Kind: StepKey, Key: "Env"}}}},
	}}
	if got := len(e.References()); got != 2 {
		t.Errorf("References() returned %d, want 2 — a path must be part of the dedup key", got)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./pkg/value/ -run 'TestAStep|TestAReferenceRendersItsPath|TestTwoPaths' -v`
Expected: FAIL to compile — `undefined: StepKey`, `unknown field Path`.

- [ ] **Step 3: Write the implementation**

In `pkg/value/expr.go`, add above `type Reference struct`:

```go
// StepKind distinguishes the two ways a path moves into a composite.
type StepKind uint8

const (
	// StepKey reads a key from a map.
	StepKey StepKind = iota
	// StepIndex reads an entry from a list.
	StepIndex
)

// Step is one move along a path into a composite value.
//
// The two kinds are kept apart at PARSE time rather than resolved against the
// value's kind at evaluation time, because a map may have a numeric-looking
// key: `${var.ports.0}` would otherwise mean key "0" or index 0 depending on
// what `ports` turned out to be, which makes a reference's meaning depend on
// the type of the thing it names. `.0` is always a key and `[0]` is always an
// index, decided with no value in hand.
type Step struct {
	Kind  StepKind
	Key   string // StepKey only
	Index int    // StepIndex only
}

// String renders a step as it was written, including its leading delimiter, so
// that joining a path's steps reproduces the source.
func (s Step) String() string {
	if s.Kind == StepIndex {
		return "[" + strconv.Itoa(s.Index) + "]"
	}
	return "." + s.Key
}
```

Add the field to `Reference`, below `Attribute`:

```go
	// Path is the steps taken into Attribute's value — or, under OpVarRef,
	// into the variable's own value, where Attribute is empty.
	//
	// It is part of String(), and therefore part of References()' dedup key.
	// Without that, ${vpc.tags.Name} and ${vpc.tags.Env} render identically,
	// the second is dropped as a duplicate, and its expression resolves to the
	// first one's value.
	Path []Step
```

Replace `Reference.String()` with:

```go
func (r Reference) String() string {
	var b strings.Builder
	b.WriteString(r.Target.String())
	if r.Attribute != "" {
		b.WriteString(".")
		b.WriteString(r.Attribute)
	}
	for _, s := range r.Path {
		b.WriteString(s.String())
	}
	return b.String()
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./pkg/value/ -v` then `go test ./...`
Expected: PASS. The whole suite still passes — `Path` is nil everywhere, so `String()` is unchanged for every existing reference.

- [ ] **Step 5: Commit**

```bash
git add pkg/value/expr.go pkg/value/expr_test.go
git commit -m "value: a reference carries a path of keys and indices

Part of the dedup key, not decoration: two paths into one attribute are
two references, and rendering them identically drops the second."
```

---

### Task 2: parse the `var.` prefix (expand)

Accept `${var.x}` alongside bare `${x}`. Purely additive — every existing test passes untouched.

**Files:**
- Modify: `internal/expressions/parse.go:409-490` (`parseReference`)
- Modify: `pkg/value/expr.go` (`Expr.inner`)
- Test: `internal/expressions/parse_test.go`

**Interfaces:**
- Consumes: Task 1's `Reference.Path`.
- Produces: `${var.NAME}` parses to `OpVarRef` with `Ref.Target.Name == NAME`; `Expr.inner()` renders an `OpVarRef` with its `var.` prefix.

- [ ] **Step 1: Write the failing test**

Add to `internal/expressions/parse_test.go`:

```go
func TestVarPrefixParsesAsAVariable(t *testing.T) {
	e, ds := Parse("${var.region}", value.Origin{})
	if ds.HasErrors() {
		t.Fatalf("unexpected errors: %v", ds)
	}
	if e.Op != value.OpVarRef {
		t.Fatalf("Op = %v, want OpVarRef", e.Op)
	}
	if got := e.Ref.VarName(); got != "region" {
		t.Errorf("VarName() = %q, want %q — the prefix is stripped at parse time", got, "region")
	}
}

func TestABareNameStillParsesAsAVariableDuringExpand(t *testing.T) {
	// Removed in the contract task. Here it pins that the expand step is
	// additive: nothing that worked stops working.
	e, ds := Parse("${region}", value.Origin{})
	if ds.HasErrors() {
		t.Fatalf("unexpected errors: %v", ds)
	}
	if e.Op != value.OpVarRef || e.Ref.VarName() != "region" {
		t.Errorf("got %v/%q, want OpVarRef/region", e.Op, e.Ref.VarName())
	}
}

func TestAVariableReferenceRendersItsPrefix(t *testing.T) {
	e, _ := Parse("${var.region}", value.Origin{})
	if got := e.String(); got != "${var.region}" {
		t.Errorf("String() = %q, want %q — a diagnostic must echo what the user wrote", got, "${var.region}")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/expressions/ -run 'TestVarPrefix|TestABareName|TestAVariableReferenceRenders' -v`
Expected: FAIL — `${var.region}` currently parses as `OpResourceRef` with target `var`, so `Op` is wrong and `String()` returns `${var.region}` only by accident of the old rendering.

- [ ] **Step 3: Write the implementation**

In `internal/expressions/parse.go`, in `parseReference`, immediately before the final `if len(segments) == 1 {` block, insert:

```go
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
```

In `pkg/value/expr.go`, split the `inner()` case so a variable renders its prefix:

```go
	case OpVarRef:
		return "var." + e.Ref.String()
	case OpResourceRef:
		return e.Ref.String()
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/expressions/ -v` then `go test ./...`
Expected: PASS everywhere. Existing fixtures use bare names, which still parse.

- [ ] **Step 5: Commit**

```bash
git add internal/expressions/parse.go internal/expressions/parse_test.go pkg/value/expr.go
git commit -m "expressions: accept a var. prefix alongside the bare spelling

Expand step. The prefix is stripped at parse time, so variables.Scope and
every stage below it are untouched."
```

---

### Task 3: parse paths and list indices

`${var.tags.team}`, `${var.azs[0]}`, and the two composed in either order.

**Files:**
- Modify: `internal/expressions/parse.go` (`parseReference`, plus a new `parseSteps`)
- Test: `internal/expressions/parse_test.go`

**Interfaces:**
- Consumes: Task 1's `value.Step`, Task 2's `var` branch.
- Produces: `parseSteps(segments []string, ref string, origin value.Origin, ds *diag.Diagnostics) ([]value.Step, bool)` — turns trailing segments into steps, reporting malformed ones.

- [ ] **Step 1: Write the failing test**

Add to `internal/expressions/parse_test.go`:

```go
func TestAVariablePathParsesToSteps(t *testing.T) {
	for _, tc := range []struct {
		src  string
		name string
		want []value.Step
	}{
		{"${var.tags.team}", "tags", []value.Step{{Kind: value.StepKey, Key: "team"}}},
		{"${var.azs[0]}", "azs", []value.Step{{Kind: value.StepIndex, Index: 0}}},
		{"${var.subnets[1].cidr}", "subnets", []value.Step{
			{Kind: value.StepIndex, Index: 1}, {Kind: value.StepKey, Key: "cidr"}}},
		{"${var.regions.us_east.azs[2]}", "regions", []value.Step{
			{Kind: value.StepKey, Key: "us_east"}, {Kind: value.StepKey, Key: "azs"},
			{Kind: value.StepIndex, Index: 2}}},
	} {
		e, ds := Parse(tc.src, value.Origin{})
		if ds.HasErrors() {
			t.Errorf("%s: unexpected errors: %v", tc.src, ds)
			continue
		}
		if e.Ref.VarName() != tc.name {
			t.Errorf("%s: VarName() = %q, want %q", tc.src, e.Ref.VarName(), tc.name)
		}
		if !reflect.DeepEqual(e.Ref.Path, tc.want) {
			t.Errorf("%s: Path = %+v, want %+v", tc.src, e.Ref.Path, tc.want)
		}
	}
}

func TestAnIndexMustBeALiteralInteger(t *testing.T) {
	_, ds := Parse("${var.azs[i]}", value.Origin{})
	if !ds.HasErrors() {
		t.Fatal("${var.azs[i]} must be refused: a varying index needs iteration, which this language does not have")
	}
	if got := ds[0].Summary; !strings.Contains(got, "literal") {
		t.Errorf("Summary = %q, want it to say the index must be a literal", got)
	}
}

func TestANegativeIndexIsRefused(t *testing.T) {
	_, ds := Parse("${var.azs[-1]}", value.Origin{})
	if !ds.HasErrors() {
		t.Fatal("${var.azs[-1]} must be refused: meaning would depend on a length the reader cannot see")
	}
}

func TestAPathRoundTripsThroughString(t *testing.T) {
	for _, src := range []string{"${var.tags.team}", "${var.azs[0]}", "${var.subnets[1].cidr}"} {
		e, ds := Parse(src, value.Origin{})
		if ds.HasErrors() {
			t.Fatalf("%s: %v", src, ds)
		}
		if got := e.String(); got != src {
			t.Errorf("String() = %q, want %q", got, src)
		}
	}
}
```

Add `"reflect"` and `"strings"` to that file's imports if absent.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/expressions/ -run 'TestAVariablePath|TestAnIndexMust|TestANegative|TestAPathRoundTrips' -v`
Expected: FAIL — `${var.tags.team}` currently yields `Path == nil` and `${var.azs[0]}` keeps the bracket inside the name.

- [ ] **Step 3: Write the implementation**

In `internal/expressions/parse.go`, add:

```go
// indexSuffix matches a trailing [N] on one segment.
var indexSuffix = regexp.MustCompile(`^(.*?)\[([^\]]*)\]$`)

// parseSteps turns the segments after a name into path steps.
//
// A segment is a map key, optionally carrying ONE trailing [N] that indexes
// the value that key names. Brackets are scanned here rather than by the
// expression scanner because they never nest inside a reference: the index is
// a literal integer, so there is nothing to nest.
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
		if key != "" {
			out = append(out, value.Step{Kind: value.StepKey, Key: key})
		}
		for _, raw := range indices {
			n, err := strconv.Atoi(raw)
			if err != nil {
				ds.Add(diag.Diagnostic{
					Severity: diag.SeverityError,
					Summary:  "index " + strconv.Quote(raw) + " in ${" + ref + "} is not a literal integer",
					Detail: "An index is a literal integer. A varying index is only useful if something " +
						"varies it, which is iteration, and this language has none.",
					Action: "Write a literal, as ${" + ref[:strings.Index(ref, "[")] + "[0]}.",
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
```

Add `"regexp"` to the imports.

Replace the `var` branch from Task 2 with the path-aware form:

```go
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
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/expressions/ -v` then `go test ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/expressions/parse.go internal/expressions/parse_test.go
git commit -m "expressions: a variable reference may carry a path

Map keys are dotted and list indices bracketed, so a map with a numeric
key is not ambiguous. An index is a literal integer, which is the rule
that keeps this a read rather than a loop."
```

---

### Task 4: evaluate a path, and carry sensitivity through it

**Files:**
- Create: `internal/expressions/path.go`
- Modify: `internal/expressions/eval.go` (the `OpVarRef` case)
- Test: `internal/expressions/path_test.go`

**Interfaces:**
- Consumes: Task 3's parsed `Ref.Path`.
- Produces: `applyPath(v value.Value, steps []value.Step, ref string, origin value.Origin, ds *diag.Diagnostics) (value.Value, bool)`.

- [ ] **Step 1: Write the failing test**

Create `internal/expressions/path_test.go`:

```go
package expressions

import (
	"strings"
	"testing"

	"github.com/infrena/infrena/pkg/value"
)

func mapVal(m map[string]value.Value) value.Value {
	return value.Value{Kind: value.KindMap, Raw: m, Known: true, Source: value.SourceVariable}
}

func listVal(items ...value.Value) value.Value {
	return value.Value{Kind: value.KindList, Raw: items, Known: true, Source: value.SourceVariable}
}

type oneVar struct {
	name string
	val  value.Value
}

func (s oneVar) Variable(n string) (value.Value, bool) {
	if n == s.name {
		return s.val, true
	}
	return value.Value{}, false
}
func (s oneVar) Attribute(value.Reference) (value.Value, bool) { return value.Value{}, false }

func TestAPathReadsAKeyAndAnIndex(t *testing.T) {
	scope := oneVar{"cfg", mapVal(map[string]value.Value{
		"azs": listVal(value.String("us-east-1a", value.SourceVariable),
			value.String("us-east-1b", value.SourceVariable)),
	})}
	e, ds := Parse("${var.cfg.azs[1]}", value.Origin{})
	if ds.HasErrors() {
		t.Fatalf("parse: %v", ds)
	}
	got, eds := Evaluate(e, scope)
	if eds.HasErrors() {
		t.Fatalf("evaluate: %v", eds)
	}
	s, _ := got.AsString()
	if s != "us-east-1b" {
		t.Errorf("got %q, want %q", s, "us-east-1b")
	}
}

func TestAPathOutOfAContainerMarkedSensitiveStaysSensitive(t *testing.T) {
	// The whole reason §3.1 exists. CarrySensitivity sets Sensitive on the
	// CONTAINER as well as its leaves, so a sensitive map variable holds the
	// flag at the top and may hold nothing on the leaf. Returning the leaf as
	// found declassifies it, and value.Format then prints it in clear.
	inner := mapVal(map[string]value.Value{
		"password": value.String("hunter2", value.SourceVariable),
	})
	inner.Sensitive = true
	scope := oneVar{"creds", inner}

	e, _ := Parse("${var.creds.password}", value.Origin{})
	got, eds := Evaluate(e, scope)
	if eds.HasErrors() {
		t.Fatalf("evaluate: %v", eds)
	}
	if !got.Sensitive {
		t.Fatal("extracting from a sensitive container must stay sensitive — otherwise the secret prints in a plan")
	}
}

func TestAMissingKeyNamesTheKeysThatExist(t *testing.T) {
	scope := oneVar{"tags", mapVal(map[string]value.Value{
		"team":    value.String("payments", value.SourceVariable),
		"project": value.String("billing", value.SourceVariable),
	})}
	e, _ := Parse("${var.tags.tema}", value.Origin{})
	_, eds := Evaluate(e, scope)
	if !eds.HasErrors() {
		t.Fatal("a missing key must be an error, not an unknown: it can never become present")
	}
	d := eds[0]
	if !strings.Contains(d.Detail, "team") || !strings.Contains(d.Detail, "project") {
		t.Errorf("Detail = %q, want it to list the keys that exist", d.Detail)
	}
}

func TestAnOutOfRangeIndexGivesTheLength(t *testing.T) {
	scope := oneVar{"azs", listVal(
		value.String("a", value.SourceVariable),
		value.String("b", value.SourceVariable),
		value.String("c", value.SourceVariable))}
	e, _ := Parse("${var.azs[5]}", value.Origin{})
	_, eds := Evaluate(e, scope)
	if !eds.HasErrors() {
		t.Fatal("an out-of-range index must be an error")
	}
	if !strings.Contains(eds[0].Summary, "3") {
		t.Errorf("Summary = %q, want it to give the length", eds[0].Summary)
	}
}

func TestIndexingAMapAndKeyingAListEachPointAtTheOtherForm(t *testing.T) {
	m := oneVar{"tags", mapVal(map[string]value.Value{"team": value.String("p", value.SourceVariable)})}
	e, _ := Parse("${var.tags[0]}", value.Origin{})
	_, eds := Evaluate(e, m)
	if !eds.HasErrors() || !strings.Contains(eds[0].Action, "team") {
		t.Errorf("indexing a map must point at keying it, got %v", eds)
	}

	l := oneVar{"azs", listVal(value.String("a", value.SourceVariable))}
	e2, _ := Parse("${var.azs.first}", value.Origin{})
	_, eds2 := Evaluate(e2, l)
	if !eds2.HasErrors() || !strings.Contains(eds2[0].Action, "[0]") {
		t.Errorf("keying a list must point at indexing it, got %v", eds2)
	}
}

func TestAStepIntoAScalarNamesTheKind(t *testing.T) {
	scope := oneVar{"region", value.String("us-east-1", value.SourceVariable)}
	e, _ := Parse("${var.region.x}", value.Origin{})
	_, eds := Evaluate(e, scope)
	if !eds.HasErrors() || !strings.Contains(eds[0].Summary, "string") {
		t.Errorf("Summary must name the kind, got %v", eds)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/expressions/ -run 'TestAPath|TestAMissingKey|TestAnOutOfRange|TestIndexingAMap|TestAStepIntoAScalar' -v`
Expected: FAIL — the path is parsed but never applied, so `${var.cfg.azs[1]}` returns the whole map.

- [ ] **Step 3: Write the implementation**

Create `internal/expressions/path.go`:

```go
package expressions

import (
	"sort"
	"strconv"
	"strings"

	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/pkg/value"
)

// applyPath walks v along steps and returns the value at the end.
//
// Sensitivity UNIONS along the whole path. pkg/value marks a container
// sensitive as well as its leaves (see CarrySensitivity), so a sensitive map
// variable carries the flag at the top and may carry nothing on the leaf.
// Returning the leaf as found would declassify it, and value.Format — the one
// and only redaction path — would then print a secret in clear. This is
// PLAN.md §10.2's rule ("sensitivity unions across ALL arguments") in a new
// position; join() and replace() each shipped it wrong.
func applyPath(v value.Value, steps []value.Step, ref string, origin value.Origin, ds *diag.Diagnostics) (value.Value, bool) {
	sensitive := v.Sensitive
	cur := v
	for _, st := range steps {
		if !cur.Known {
			// A dependency that does not exist yet. Leave it deferred with its
			// expression intact rather than reporting a missing member of a
			// value nobody has seen.
			return cur, false
		}
		next, ok := stepInto(cur, st, ref, origin, ds)
		if !ok {
			return value.Value{}, false
		}
		sensitive = sensitive || next.Sensitive
		cur = next
	}
	cur.Sensitive = sensitive
	return cur, true
}

func stepInto(v value.Value, st value.Step, ref string, origin value.Origin, ds *diag.Diagnostics) (value.Value, bool) {
	switch {
	case st.Kind == value.StepKey && v.Kind == value.KindMap:
		m, _ := v.Raw.(map[string]value.Value)
		if got, ok := m[st.Key]; ok {
			return got, true
		}
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "${" + ref + "} has no key " + strconv.Quote(st.Key),
			Detail:   "Known keys:\n  " + strings.Join(sortedKeys(m), "\n  "),
			Action:   "Correct the key.",
			Origin:   origin,
		})
		return value.Value{}, false

	case st.Kind == value.StepIndex && v.Kind == value.KindList:
		l, _ := v.Raw.([]value.Value)
		if st.Index < len(l) {
			return l[st.Index], true
		}
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary: "${" + ref + "} has " + strconv.Itoa(len(l)) +
				" entries; there is no index " + strconv.Itoa(st.Index),
			Detail: "An index must name an entry that exists. The list is fully resolved " +
				"before a plan is made, so this cannot become valid later.",
			Action: "Use an index below " + strconv.Itoa(len(l)) + ".",
			Origin: origin,
		})
		return value.Value{}, false

	case st.Kind == value.StepIndex && v.Kind == value.KindMap:
		m, _ := v.Raw.(map[string]value.Value)
		example := "<key>"
		if ks := sortedKeys(m); len(ks) > 0 {
			example = ks[0]
		}
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "${" + ref + "} is a map; it cannot be indexed",
			Detail:   "Brackets index a list. A map is read by key.",
			Action:   "Read it by key, as ${" + ref + "." + example + "}.",
			Origin:   origin,
		})
		return value.Value{}, false

	case st.Kind == value.StepKey && v.Kind == value.KindList:
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "${" + ref + "} is a list; it has no key " + strconv.Quote(st.Key),
			Detail:   "A dotted name reads a map key. A list is read by index.",
			Action:   "Index it, as ${" + ref + "[0]}.",
			Origin:   origin,
		})
		return value.Value{}, false

	default:
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "${" + ref + "} is " + kindName(v.Kind) + "; it has no members",
			Detail:   "Only a map or a list can be stepped into.",
			Action:   "Reference it whole, without a path.",
			Origin:   origin,
		})
		return value.Value{}, false
	}
}

// sortedKeys keeps a diagnostic byte-identical across runs (invariant 6). Go
// randomises map iteration, so listing keys unsorted would make two runs of
// identical input differ.
func sortedKeys(m map[string]value.Value) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func kindName(k value.Kind) string {
	switch k {
	case value.KindString:
		return "a string"
	case value.KindInt:
		return "an integer"
	case value.KindFloat:
		return "a number"
	case value.KindBool:
		return "a boolean"
	default:
		return "not a container"
	}
}
```

In `internal/expressions/eval.go`, replace the `OpVarRef` case's final return:

```go
		v = v.WithOrigin(e.Origin)
		if len(e.Ref.Path) == 0 {
			return v
		}
		// The ref rendered WITHOUT the path, so a diagnostic names the thing
		// being stepped into rather than echoing the whole failing expression.
		base := "var." + e.Ref.VarName()
		out, ok := applyPath(v, e.Ref.Path, base, e.Origin, ds)
		if !ok {
			return unknownFrom(e, value.KindString, false)
		}
		return out
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/expressions/ -v` then `go test ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/expressions/path.go internal/expressions/path_test.go internal/expressions/eval.go
git commit -m "expressions: evaluate a path, unioning sensitivity along it

A container carries the sensitive flag as well as its leaves, so returning
a leaf as found declassifies it and Format then prints it in clear."
```

---

### Task 5: a resource reference takes a path, and its target is one segment

**Files:**
- Modify: `internal/expressions/parse.go` (`parseReference`, the trailing resource branch)
- Modify: `internal/expressions/resources.go` (`ResourceScope.Attribute`)
- Test: `internal/expressions/parse_test.go`, `internal/expressions/resources_test.go`

**Interfaces:**
- Consumes: Tasks 1, 3, 4.
- Produces: `${vpc.tags.Name}` parses to `OpResourceRef` with `Target.Name == "vpc"`, `Attribute == "tags"`, `Path == [Key("Name")]`, and resolves through `applyPath`.

- [ ] **Step 1: Write the failing test**

Add to `internal/expressions/parse_test.go`:

```go
func TestAResourceReferenceTakesTheFirstSegmentAsItsTarget(t *testing.T) {
	// A resource name cannot contain a dot (config.checkResourceName), so the
	// target is always exactly one segment. The old rule took every segment
	// but the last, which could only ever build a target nothing is allowed
	// to declare.
	e, ds := Parse("${vpc.tags.Name}", value.Origin{})
	if ds.HasErrors() {
		t.Fatalf("unexpected errors: %v", ds)
	}
	if e.Op != value.OpResourceRef {
		t.Fatalf("Op = %v, want OpResourceRef", e.Op)
	}
	if e.Ref.Target.Name != "vpc" {
		t.Errorf("Target.Name = %q, want %q", e.Ref.Target.Name, "vpc")
	}
	if e.Ref.Attribute != "tags" {
		t.Errorf("Attribute = %q, want %q", e.Ref.Attribute, "tags")
	}
	if len(e.Ref.Path) != 1 || e.Ref.Path[0].Key != "Name" {
		t.Errorf("Path = %+v, want one key step Name", e.Ref.Path)
	}
}

func TestATwoSegmentResourceReferenceIsUnchanged(t *testing.T) {
	e, ds := Parse("${vpc.id}", value.Origin{})
	if ds.HasErrors() {
		t.Fatalf("unexpected errors: %v", ds)
	}
	if e.Ref.Target.Name != "vpc" || e.Ref.Attribute != "id" || len(e.Ref.Path) != 0 {
		t.Errorf("got %q/%q/%+v, want vpc/id/no path", e.Ref.Target.Name, e.Ref.Attribute, e.Ref.Path)
	}
}
```

Add to `internal/expressions/resources_test.go`:

```go
func TestAPathIntoAResourceAttributeResolvesAtApply(t *testing.T) {
	scope := ResourceScope{"vpc": {
		"tags": {Kind: value.KindMap, Known: true, Source: value.SourceProvider,
			Raw: map[string]value.Value{"Name": value.String("prod", value.SourceProvider)}},
	}}
	e, ds := Parse("${vpc.tags.Name}", value.Origin{})
	if ds.HasErrors() {
		t.Fatalf("parse: %v", ds)
	}
	got, eds := Evaluate(e, scope)
	if eds.HasErrors() {
		t.Fatalf("evaluate: %v", eds)
	}
	s, _ := got.AsString()
	if s != "prod" {
		t.Errorf("got %q, want %q", s, "prod")
	}
}

func TestAPathIntoAnUnresolvedResourceStaysDeferredWithItsExpression(t *testing.T) {
	// The property `apply --plan` depends on (PLAN.md §37): an unknown must
	// carry the expression that reproduces it, or a saved plan applies with
	// the attribute silently unset.
	e, _ := Parse("${vpc.tags.Name}", value.Origin{})
	got, eds := Evaluate(e, ResourceScope{})
	if eds.HasErrors() {
		t.Fatalf("a not-yet-created dependency is not an error: %v", eds)
	}
	if got.Known {
		t.Fatal("must stay unknown")
	}
	if got.Expr == nil {
		t.Fatal("an unknown must carry its expression, or apply cannot finish it")
	}
	if got.Expr.String() != "${vpc.tags.Name}" {
		t.Errorf("Expr = %q, want %q", got.Expr.String(), "${vpc.tags.Name}")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/expressions/ -run 'TestAResourceReferenceTakes|TestATwoSegment|TestAPathIntoA' -v`
Expected: FAIL — `${vpc.tags.Name}` currently yields target `vpc.tags`, attribute `Name`.

- [ ] **Step 3: Write the implementation**

In `internal/expressions/parse.go`, replace the final `return &value.Expr{Op: value.OpResourceRef, ...}` block with:

```go
	// FIRST segment is the resource, SECOND is the attribute, the rest is a
	// path. A resource's name cannot contain a dot — config.checkResourceName
	// refuses one, because a name IS an address and a dot separates module
	// levels — so a user-written target is always exactly one segment. The old
	// rule, target-is-everything-but-the-last, could only ever construct a
	// target nothing is permitted to declare, which is why ${vpc.tags.Name}
	// reported an undeclared resource "vpc.tags".
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
	ref := value.LocalRef(segments[0], attrSteps[0].Key)
	ref.Path = append(attrSteps[1:], rest...)
	return &value.Expr{Op: value.OpResourceRef, Ref: ref, Origin: origin}
```

In `internal/expressions/resources.go`, `ResourceScope.Attribute` keys its lookup on `ref.Target.String()` and `ref.Attribute`, both of which are already path-free — so it needs no change. Apply the path in `eval.go`'s `OpResourceRef` case instead:

```go
	case value.OpResourceRef:
		v, ok := scope.Attribute(e.Ref)
		if !ok {
			// Not an error: the dependency simply does not exist yet.
			return unknownFrom(e, value.KindString, false)
		}
		v = v.WithOrigin(e.Origin)
		if len(e.Ref.Path) == 0 {
			return v
		}
		base := e.Ref.Target.String() + "." + e.Ref.Attribute
		out, ok := applyPath(v, e.Ref.Path, base, e.Origin, ds)
		if !ok {
			return unknownFrom(e, value.KindString, false)
		}
		return out
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/expressions/ -v` then `go test ./...`
Expected: PASS. `internal/compiler` still passes: `refTarget.has(ref.Attribute)` checks `tags`, which exists in the schema; the path is not checked, by design (spec §3.3).

- [ ] **Step 5: Commit**

```bash
git add internal/expressions/parse.go internal/expressions/parse_test.go internal/expressions/resources_test.go internal/expressions/eval.go
git commit -m "expressions: a resource reference takes a path, and one segment names it

A resource name cannot contain a dot, so all-but-the-last could only build
a target nothing is allowed to declare."
```

---

### Task 6: migrate the repository (243 occurrences, 60 files)

Every bare `${x}` in infrena configuration becomes `${var.x}`. No judgment is involved: under the old grammar a bare single segment could only be a variable.

**Files:**
- Modify: test fixtures under `internal/**/*_test.go`, `tests/integration/*_test.go`, `*.yml` fixtures
- Modify: `PLAN.md`, `README.md`, `CLAUDE.md`
- **Do NOT touch:** `docs/superpowers/plans/**`, `.superpowers/**`, `.github/workflows/**`

- [ ] **Step 1: Write the allowlist and preview the change**

```bash
cd /home/james/projects/infrena
python3 - <<'EOF' | tee /tmp/migration-preview.txt
import re, os
SKIP = ('./docs/superpowers/plans', './.superpowers', './.github', './.git')
pat = re.compile(r'\$\{([A-Za-z_][A-Za-z0-9_]*)\}')
for root, dirs, fs in os.walk('.'):
    dirs[:] = [d for d in dirs if d != '.git']
    for f in fs:
        p = os.path.join(root, f)
        if p.startswith(SKIP) or not f.endswith(('.go', '.yml', '.yaml', '.md')):
            continue
        txt = open(p, encoding='utf8').read()
        hits = pat.findall(txt)
        if hits:
            print(f"{len(hits):4}  {p}  {sorted(set(hits))[:6]}")
EOF
```

Read the output. Confirm no path under `docs/superpowers/plans/`, `.superpowers/` or `.github/` appears.

- [ ] **Step 2: Apply the rewrite**

```bash
python3 - <<'EOF'
import re, os
SKIP = ('./docs/superpowers/plans', './.superpowers', './.github', './.git')
pat = re.compile(r'\$\{([A-Za-z_][A-Za-z0-9_]*)\}')
n = 0
for root, dirs, fs in os.walk('.'):
    dirs[:] = [d for d in dirs if d != '.git']
    for f in fs:
        p = os.path.join(root, f)
        if p.startswith(SKIP) or not f.endswith(('.go', '.yml', '.yaml', '.md')):
            continue
        txt = open(p, encoding='utf8').read()
        out = pat.sub(lambda m: '${var.' + m.group(1) + '}', txt)
        if out != txt:
            open(p, 'w').write(out)
            n += 1
print(f"rewrote {n} files")
EOF
```

- [ ] **Step 3: Run the full suite**

Run: `go test ./...`
Expected: PASS. A fixture rewritten wrongly fails a test — this is the commit's own verification. If something fails, read the failure: it is either a real miss or a string that was never an infrena expression.

- [ ] **Step 4: Check the historical records are untouched**

```bash
git status --porcelain | grep -E '(docs/superpowers/plans|\.superpowers|\.github)' && echo "STOP: historical or CI files changed" || echo "historical records untouched"
```

Expected: `historical records untouched`.

- [ ] **Step 5: Commit**

```bash
git add -A
git commit -m "migrate: variables take the var. prefix

Mechanical. Under the old grammar a bare single segment could only be a
variable, so there is no case to adjudicate. Historical plan and SDD
records are deliberately left as written."
```

---

### Task 7: contract — a bare name is no longer a variable, and `var` is reserved

**Files:**
- Modify: `internal/expressions/parse.go` (remove the bare-variable branch)
- Modify: `internal/config/decode_modules.go:335` (`checkResourceName`)
- Modify: `internal/expressions/characterisation_test.go`
- Test: `internal/expressions/parse_test.go`, `internal/config/decode_test.go`

**Interfaces:**
- Consumes: Tasks 2-6.
- Produces: a bare single segment is an error; a resource named `var` is an error at its declaration.

- [ ] **Step 1: Write the failing test**

In `internal/expressions/parse_test.go`, DELETE `TestABareNameStillParsesAsAVariableDuringExpand` and add:

```go
func TestABareSingleSegmentIsNotAReference(t *testing.T) {
	_, ds := Parse("${region}", value.Origin{})
	if !ds.HasErrors() {
		t.Fatal("a bare single segment must be an error: variables are var.-prefixed and a resource reference needs an attribute")
	}
	d := ds[0]
	if !strings.Contains(d.Detail, "${var.region}") {
		t.Errorf("Detail = %q, want it to name the fix", d.Detail)
	}
	if !strings.Contains(d.Detail, "attribute") {
		t.Errorf("Detail = %q, want it to mention the resource-reference form too", d.Detail)
	}
}
```

In `internal/config/decode_test.go` add:

```go
func TestAResourceNamedVarIsRefused(t *testing.T) {
	var ds diag.Diagnostics
	if checkResourceName("resources", "var", value.Origin{}, &ds) {
		t.Fatal("a resource named var must be refused: it makes ${var.x} mean two things")
	}
	if !strings.Contains(ds[0].Summary, "reserved") {
		t.Errorf("Summary = %q, want it to say reserved", ds[0].Summary)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/expressions/ ./internal/config/ -run 'TestABareSingleSegment|TestAResourceNamedVar' -v`
Expected: FAIL — a bare name still parses, and `var` is still an acceptable resource name.

- [ ] **Step 3: Write the implementation**

In `internal/expressions/parse.go`, replace the `if len(segments) == 1 { ... OpVarRef ... }` block with:

```go
	if len(segments) == 1 {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "${" + src + "} is not a reference",
			Detail: "Variables are written ${var." + src + "}. A resource reference needs an " +
				"attribute, as ${" + src + ".id}.",
			Action: "Add the `var.` prefix, or name an attribute.",
			Origin: origin,
		})
		return nil
	}
```

In `internal/config/decode_modules.go`, at the top of `checkResourceName`, before the `identifierSegment` check:

```go
	// `var` is the variable namespace. A resource so named makes ${var.x} mean
	// two things — the reference is ambiguous at parse time, where there is no
	// scope to disambiguate with — so it is refused HERE, at the declaration,
	// which is where the fix is.
	if name == "var" {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "resource name \"var\" is reserved",
			Detail: "`var` is the namespace every variable reference begins with, so a resource " +
				"called `var` would make ${var.x} mean either that resource's `x` attribute or " +
				"the variable `x`.",
			Action: "Rename the resource.",
			Origin: origin,
		})
		return false
	}
```

In `internal/expressions/characterisation_test.go`, update every pinned bare-variable case to its `var.`-prefixed form. Do NOT delete the file: its diff is the statement of what the grammar change moved.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./...`
Expected: PASS.

- [ ] **Step 5: Add the integration test and commit**

Add to `tests/integration/` a test that plans a project using a map path, a list index and a resource-attribute path end to end, asserting the plan's rendered values. Then:

```bash
go test ./...
git add -A
git commit -m "expressions: a bare name is no longer a variable, and var is reserved

Contract step. parse.go's segment arithmetic is gone: every form now has
exactly one meaning, and the undeclared-resource error can no longer fire
for a name that is really a variable."
```

---

### Task 8: documentation and the version floor

**Files:**
- Modify: `PLAN.md` §6.3, §9, §10, §10.4, §12.1, §57
- Modify: `CLAUDE.md`, `README.md`
- Modify: `internal/cli/init.go`
- Test: `internal/cli/init_test.go`

- [ ] **Step 1: Amend `PLAN.md` §10**

Replace §10's opening examples and add a subsection `## 10.5 The var namespace, paths and indices` carrying the spec's §2 and §3 verbatim in PLAN.md's voice. Amend §6.3 so the process variables are written `${var.environment}` and `${var.project}`. Update the bare variables in §9, §12.1 and §57. Add one line to §10.4 confirming no new token was introduced, the brackets already being tracked by `splitArgs`.

- [ ] **Step 2: Update `CLAUDE.md` and `README.md`**

Add the six-form grammar to CLAUDE.md's current-state section. Move README's examples.

- [ ] **Step 3: Write the failing test for the floor key**

In `internal/cli/init_test.go`:

```go
func TestInitScaffoldsAVersionFloor(t *testing.T) {
	// PLAN.md §61.2. The key turns "unknown key" into "this project needs
	// infrena >= X; this is Y" for the NEXT break, which is the one nobody
	// will remember to prepare for.
	dir := t.TempDir()
	if err := runInit(dir); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "infra.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "infrena:") {
		t.Error("infra.yml must declare a version floor")
	}
}
```

Adjust `runInit` to whatever `init.go` actually exposes; read `init_test.go`'s existing helpers first.

- [ ] **Step 4: Add the floor key to the scaffold**

In `internal/cli/init.go`, add to the `infra.yml` template, above `environments:`:

```yaml
# The oldest infrena that understands this project. Optional, and worth
# keeping: without it, an older binary reports unknown keys one at a time
# instead of saying it is too old.
infrena: ">= 0.5"
```

- [ ] **Step 5: Run everything and commit**

```bash
go test ./...
git add -A
git commit -m "docs: the reference grammar, and init scaffolds a version floor"
```

---

## Self-Review

**Spec coverage.** §2 grammar → Tasks 2, 3, 5, 7. §2.1 one-segment target → Task 5. §3 paths and indices → Tasks 3, 4. §3.1 sensitivity → Task 4. §3.2 path inside the reference → Task 1. §3.3 resource paths → Task 5. §4 migration → Task 6 (allowlist, historical-record guard). §4.3 expand/migrate/contract → Tasks 2-5 / 6 / 7. §4.4 spec of record → Task 8. §4.5 floor key → Task 8. §5 diagnostics → all nine land across Tasks 2, 3, 4, 7. §6 testing → characterisation update (7), sensitivity test (4), round trip (5), integration (7).

**Type consistency.** `value.Step`/`StepKey`/`StepIndex` defined in Task 1 and used unchanged in 3, 4, 5. `parseSteps` defined in Task 3, reused in 5 with the same signature. `applyPath` defined in Task 4, called from both `eval.go` cases.

**Known gap, deliberate.** §5's `${var.azs[i]}` diagnostic builds its Action with `ref[:strings.Index(ref, "[")]`, which assumes a bracket is present — true at every call site, since the branch only runs when one was parsed. Task 3's implementer should still guard it if `Index` returns -1.
