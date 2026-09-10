# M4 Variables, Environments and the Precedence Chain — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the precedence chain real — variables, typed variable schemas, environments with `extends` inheritance, and compiler stages 3 and 4 — so a resolved value records which scope supplied it.

**Architecture:** Two new packages (`internal/environments`, `internal/variables`) implement compiler stages 3 and 4. `config.Load` and `config.Decode` extend to read and type `variables.yml` and `environments/*.yml`. `value.Value` gains a `Scope` field recording which precedence level won, orthogonal to the existing `Source`. `internal/compiler/bind.go`'s `variableScope` is deleted and replaced.

**Tech Stack:** Go 1.24, cobra, yaml.v3. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-09-09-infra-phase-1-design.md` (§7 compiler pipeline, §7.1 precedence and provenance) and `PLAN.md` (§7 environment inheritance, §8 variable files, §9 typed variables). `PLAN.md` is the authoritative product spec.

**Interface contract:** `.superpowers/sdd/m4-authoring/contract.md` — binding, and it records the user's provenance decision plus three amendments made during authoring.

## Global Constraints

- Exactly two third-party dependencies: `github.com/spf13/cobra` and `gopkg.in/yaml.v3`. Adding any other is a hard failure.
- Stage 2 (`config.Decode`) is the ONLY stage permitted to touch `yaml.Node` (spec §7). This is how "raw YAML maps must not flow through the application" is enforced structurally rather than by discipline.
- `internal/graph` imports no other `infra` package. `internal/executor` imports neither `internal/cli` nor `os/signal`. `providers/*` import no `internal/*`.
- No AWS types in the core engine.
- Exactly ONE redaction path: `pkg/value.Format`. A sensitive value must not reach any command's output at any nesting depth, whatever its scope.
- Diagnostics collect rather than fail fast (spec §7.4).
- `make check` and `go test -race -count=1 ./...` green across all packages before any task is reported done. **Always `-count=1`**: `tests/integration` shells out to `go build` rather than importing any infra package, so Go's test cache once reported `ok ... (cached)` while production code was sabotaged.

## The three invariants the new `Scope` field must not break

Each one, if violated, breaks something an earlier milestone paid for. Each gets a test that FAILS against a violating implementation — a test that merely names an invariant is not evidence it holds. Invariant 4's test once passed 20 times out of 20 with its dependency edge deleted.

1. **`Equal` MUST IGNORE `Scope`.** Two values differing only in which scope supplied them are the same value. If `Equal` compares `Scope`, a `--var` matching what `variables.yml` already said plans as a change forever — the phantom-diff shape M3 spent a Critical fixing, and a direct breach of acceptance invariant 2.
2. **`ConfigHash` MUST NOT include `Scope`.** A value of `20` is the same input whether it came from a file or a flag. Hashing `Scope` makes unchanged configuration look stale in M6.
3. **`Scope` MUST round-trip through JSON.** `Value` marshals through an explicit `wireValue` struct, so a missing tag fails silently and reads back as `ScopeUnset` — a confident wrong answer rather than an error.

## Committing

Other agents may share this worktree. NEVER `git add` followed by a bare `git commit` — `git commit` commits the INDEX, and that hazard appeared four separate times during M3, once as a stale index holding a 197-line deletion. Always:

    git commit -m "your message" -- <explicit paths>

---

# M4 implementation plan — Tasks 1–3
# M4 implementation plan — Tasks 1–3

Numbering is FINAL. Cross-references elsewhere in the plan use these numbers.

Every task below is written for an implementer who sees only that task.

**Toolchain, required before any `go` command in every task:**

```bash
export PATH="$HOME/.local/share/mise/shims:$PATH"
go version   # must print go1.24.x — a bare `go` resolves to 1.20 and fails
```

**Always pass `-count=1`.** `tests/integration` shells out to `go build` rather
than importing infra packages, so Go's test cache once reported `ok ... (cached)`
for a package while production code was deliberately sabotaged.

---

## Task 1 — `value.Scope`, and the three invariants it must not break

### Why this task exists

`PLAN.md` §7 fixes a SIX-level precedence chain. `value.ValueSource` has SEVEN
fixed values and the spec (§5.1) does not map the two onto each other. Two
collisions make a single enum impossible: `--var` and `variables.yml` are both
"variable", and environment inheritance and environment overrides are both
"environment".

The user ruled on 2026-09-10: keep `Source` exactly as the spec fixes it and add
a separate precedence level, so that

```go
value.Value{Source: value.SourceVariable, Scope: value.ScopeCLIOverride}
```

renders as `replicas: 20 [variable, from --var]`. Do not collapse the two onto
one enum and do not add a `SourceCLI`; both were considered and rejected, the
first because a plan could then never say whether a value came from a file or
the command line (and `explain`, in M7, inherits that blindness permanently),
the second because it makes `Source` mean two things at once.

### Files

| Action | Path |
|--------|------|
| create | `pkg/value/scope.go` |
| create | `pkg/value/scope_test.go` |
| create | `internal/compiler/scope_hash_test.go` |
| modify | `pkg/value/value.go` — add the `Scope` field, add `AsFloat`, document the `Equal` rule at the site |
| modify | `pkg/value/kind.go` — add `ParseKind` and `KindNames` |
| modify | `pkg/value/json.go` — wire field and tag; correct one now-false comment |
| modify | `pkg/value/wire_test.go` — correct the same now-false claim |
| modify | `pkg/value/format.go` — add `Annotate` |

**This task touches no file outside `pkg/value` and
`internal/compiler/scope_hash_test.go`.** In particular it does NOT edit
`internal/planner/render.go`. `Annotate` is added here and wired to nothing;
**Task 9 owns rendering provenance and changes `renderAnnotated` to delegate to
it.** Two tasks editing one file is how a fix round starts, and keeping Task 1
a pure addition means it changes no output and can be reviewed on its own —
whereas a test in this task asserting on rendered plan output would depend on
behaviour Task 9 has not built yet.

### Interfaces

**Consumes (from HEAD):**

```go
// pkg/value
type Value struct { Kind Kind; Known bool; Raw any; Source ValueSource; Sensitive bool; Expr *Expr; Origin Origin }
func (v Value) Equal(other Value) bool
func (v Value) WithSource(src ValueSource) Value
func Format(v Value, opts FormatOptions) string
type FormatOptions struct { Unknown string; QuoteStrings bool }

// internal/compiler
func (c ResolvedConfig) Hash() (string, error)
```

**Produces (Tasks 2+ and every later milestone rely on these exactly):**

```go
// pkg/value
type Scope uint8
const (
    ScopeUnset Scope = iota
    ScopeProviderDefault
    ScopeBaseConfig
    ScopeModuleDefault
    ScopeEnvironmentInherit
    ScopeEnvironmentVar
    ScopeCLIOverride
)
func (s Scope) String() string
func (v Value) WithScope(s Scope) Value
func Annotate(v Value, opts FormatOptions) string   // Task 9 wires the renderer to this
func (v Value) AsFloat() (float64, bool)            // Task 4's min/max checking needs it
func ParseKind(name string) (Kind, bool)            // inverse of Kind.String()
func KindNames() []string                           // sorted, for diagnostics that list the types
// Value gains: Scope Scope
```

**`ParseKind` is the reverse direction `pkg/value` was missing, and M4 is the
milestone that makes it correct to add.** `Kind.String()` already maps
`KindInt → "integer"`; nothing mapped back, so Task 3 would otherwise need its
own spelling table. Until now those six strings were internal — a diagnostic
string and a wire format — which is why `pkg/value/json.go` documents
`Kind.String()` as "free to change". **M4 makes `type: integer` a configuration
keyword**, so that freedom is retired by this milestone rather than overturned
in passing, and step 1.7 corrects the two comments that still claim it.
`kindWireNames` stays independent: the on-disk spelling is a third contract
with its own migration path.

**`AsFloat` is safe as written, because stage 2 coerces.** It matches `AsInt`'s
shape exactly — a plain type assertion on `Raw` — so it returns `(0, false)` for
a `KindInt` value, and YAML tags `min: 1` as `!!int` even under `type: float`.
That trap is closed where the declared type is known: **Task 3 coerces a
declared bound to its variable's declared type**, so by Task 4 a bound's `Kind`
always equals its variable's `Type`. Task 4 therefore switches on `Kind` —
`AsInt` for `KindInt`, `AsFloat` for `KindFloat` — which is also what `int64`
precision requires. There is deliberately no `AsNumber`: a third numeric
accessor whose only job is papering over a coercion that should have happened
earlier is how an accessor set stops meaning anything.

### Global constraints this task could violate

- No new third-party dependency. The budget is exactly two: `cobra` and
  `yaml.v3`. Everything below uses only the standard library.
- **Exactly ONE redaction path**, `pkg/value.Format`. `Annotate` must CALL
  `Format`, never reimplement any part of it. Two copies of the renderer is
  precisely how a secret leak fixed in one place survived in the other (see
  `Format`'s own doc comment for the two measured leaks).

### Steps

#### 1.1 — Write the failing test for invariant 1 (Equal ignores Scope)

Create `pkg/value/scope_test.go`:

```go
package value

import (
	"encoding/json"
	"strings"
	"testing"
)

// allScopes is every defined Scope. Tests that must hold for all of them LOOP
// over this rather than sampling two or three.
//
// Sampling is not a style question here. Measured on this project: an
// assertion over two map keys inserted in sorted order passed ~88% of the time
// against deliberately broken code, and six keys still passed 38%. An
// exhaustive loop over a fixed slice has no such failure mode, and it also
// fails loudly when M5 adds a scope and forgets to extend the coverage.
var allScopes = []Scope{
	ScopeUnset,
	ScopeProviderDefault,
	ScopeBaseConfig,
	ScopeModuleDefault,
	ScopeEnvironmentInherit,
	ScopeEnvironmentVar,
	ScopeCLIOverride,
}

// TestEqualIgnoresScopeForEveryPairOfScopes is invariant 1.
//
// M2 established that value.Equal ignores provenance and sensitivity so that a
// filled-in default does not read as a change. Scope joins that list: two
// values that differ only in WHICH SCOPE supplied them are the same value.
//
// If Equal compares Scope, acceptance invariant 2 (no-op plan) breaks the
// moment a value moves between scopes — a `--var replicas=20` that matches
// what variables.yml already said would plan as a change forever. That is the
// exact phantom-diff shape M3 spent a Critical fixing.
func TestEqualIgnoresScopeForEveryPairOfScopes(t *testing.T) {
	for _, a := range allScopes {
		for _, b := range allScopes {
			x := Int(20, SourceVariable).WithScope(a)
			y := Int(20, SourceVariable).WithScope(b)
			if !x.Equal(y) {
				t.Errorf("Int(20) at scope %s != Int(20) at scope %s: Equal is comparing Scope, "+
					"so a --var matching what variables.yml already said would plan as a change forever", a, b)
			}
		}
	}
}

// TestEqualIgnoresScopeAtEveryDepth is the composite half of invariant 1.
//
// Provenance is per-leaf (spec §5.1), so an implementation could pass the
// scalar test above and still compare Scope inside the KindList and KindMap
// arms of Equal — which are separate code paths with their own recursion.
func TestEqualIgnoresScopeAtEveryDepth(t *testing.T) {
	for _, s := range allScopes {
		a := Map(map[string]Value{
			"tags": List([]Value{String("web", SourceVariable).WithScope(ScopeBaseConfig)}, SourceVariable),
		}, SourceVariable)
		b := Map(map[string]Value{
			"tags": List([]Value{String("web", SourceVariable).WithScope(s)}, SourceVariable),
		}, SourceVariable)
		if !a.Equal(b) {
			t.Errorf("nested leaf at scope %s compared unequal to the same leaf at ScopeBaseConfig", s)
		}
	}
}

// TestEqualStillSeesADifferentDatumAtTheSameScope exercises the OTHER answer.
//
// A predicate asserted in one direction only is not tested: `func Equal() bool
// { return true }` passes the two tests above. Every predicate in this project
// that survived mutation testing survived in exactly one direction.
func TestEqualStillSeesADifferentDatumAtTheSameScope(t *testing.T) {
	for _, s := range allScopes {
		if Int(20, SourceVariable).WithScope(s).Equal(Int(21, SourceVariable).WithScope(s)) {
			t.Errorf("Int(20) == Int(21) at scope %s: Equal has stopped comparing the datum", s)
		}
	}
}
```

#### 1.2 — Write the failing tests for invariant 3 (JSON round trip)

Append to `pkg/value/scope_test.go`:

```go
// TestScopeRoundTripsThroughJSON is invariant 3.
//
// M1 requires Value to survive state serialisation losslessly. Value marshals
// through an explicit wireValue struct (pkg/value/json.go), so Scope needs a
// deliberate field and tag — it will NOT round-trip by accident, and a missing
// tag fails SILENTLY: the value reads back as ScopeUnset, which is a confident
// wrong answer rather than an error. `state show` and `explain` would then
// report "no scope recorded" for values that have one.
//
// The loop deliberately covers every scope. ScopeUnset is the zero value, so a
// test that checked only it would pass against an implementation that persists
// nothing at all.
func TestScopeRoundTripsThroughJSON(t *testing.T) {
	for _, s := range allScopes {
		out := roundTrip(t, Int(20, SourceVariable).WithScope(s))
		if out.Scope != s {
			t.Errorf("Scope %s round-tripped as %s", s, out.Scope)
		}
		if out.Source != SourceVariable {
			t.Errorf("Source lost while adding Scope: got %v", out.Source)
		}
	}
}

// TestScopeRoundTripsAtEveryDepth pins that composites keep per-leaf scope.
// The wireValue path recurses through json.Marshal on []Value and
// map[string]Value, so this covers a different code path from the scalar case.
func TestScopeRoundTripsAtEveryDepth(t *testing.T) {
	in := Map(map[string]Value{
		"replicas": Int(20, SourceVariable).WithScope(ScopeCLIOverride),
		"tags": List([]Value{
			String("web", SourceVariable).WithScope(ScopeEnvironmentVar),
		}, SourceVariable).WithScope(ScopeBaseConfig),
	}, SourceExplicit).WithScope(ScopeBaseConfig)

	out := roundTrip(t, in)
	m, ok := out.Raw.(map[string]Value)
	if !ok {
		t.Fatalf("Raw is %T, want map[string]Value", out.Raw)
	}
	if m["replicas"].Scope != ScopeCLIOverride {
		t.Errorf("nested scalar Scope = %s, want ScopeCLIOverride", m["replicas"].Scope)
	}
	items, ok := m["tags"].Raw.([]Value)
	if !ok || len(items) != 1 {
		t.Fatalf("tags Raw is %#v", m["tags"].Raw)
	}
	if items[0].Scope != ScopeEnvironmentVar {
		t.Errorf("list element Scope = %s, want ScopeEnvironmentVar", items[0].Scope)
	}
}

// TestScopeWireNamesAreFrozen pins the persisted spelling of every Scope.
//
// Same contract as TestKindWireNamesAreFrozen: these strings are on disk.
// Scope is a uint8 whose iota ordering M5 will insert into, so persisting the
// NUMBER would silently reinterpret every state file written before M5. The
// literals below are duplicated deliberately — deriving them from
// scopeWireNames would assert nothing.
func TestScopeWireNamesAreFrozen(t *testing.T) {
	frozen := map[Scope]string{
		ScopeProviderDefault:    "provider_default",
		ScopeBaseConfig:         "base_config",
		ScopeModuleDefault:      "module_default",
		ScopeEnvironmentInherit: "environment_inherit",
		ScopeEnvironmentVar:     "environment_var",
		ScopeCLIOverride:        "cli_override",
	}
	if len(scopeWireNames) != len(frozen) {
		t.Fatalf("scopeWireNames has %d entries, frozen contract has %d; a new Scope needs a state version bump",
			len(scopeWireNames), len(frozen))
	}
	for scope, want := range frozen {
		got, err := scopeToWireName(scope)
		if err != nil {
			t.Errorf("scopeToWireName(%v): %v", scope, err)
			continue
		}
		if got != want {
			t.Errorf("scope %v persists as %q, want %q", scope, got, want)
		}
		back, err := scopeFromWireName(want)
		if err != nil || back != scope {
			t.Errorf("scopeFromWireName(%q) = %v, %v; want %v, nil", want, back, err, scope)
		}
	}
}

// TestUnsetScopeIsOmittedFromTheWire pins that adding Scope does not rewrite
// every state file already on disk.
//
// ScopeUnset is the zero value precisely so that Values constructed before M4
// stay valid. Persisting it as an explicit "scope":"unset" would churn every
// state file on the next apply for no information gain, and would make a
// pre-M4 file and a post-M4 file with identical content compare unequal.
func TestUnsetScopeIsOmittedFromTheWire(t *testing.T) {
	data, err := json.Marshal(Int(20, SourceVariable))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "scope") {
		t.Errorf("ScopeUnset was written to the wire: %s", data)
	}
}

// TestSetScopeReachesTheWireUnderItsOwnKey catches a field that is marshalled
// under the wrong key but symmetrically unmarshalled — which round-trips
// perfectly and is unreadable by anything else that parses the state file.
func TestSetScopeReachesTheWireUnderItsOwnKey(t *testing.T) {
	data, err := json.Marshal(Int(20, SourceVariable).WithScope(ScopeCLIOverride))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"scope":"cli_override"`) {
		t.Errorf("marshalled form lacks \"scope\":\"cli_override\": %s", data)
	}
}
```

`roundTrip` already exists in `pkg/value/json_test.go` (same package) — do not
redefine it.

#### 1.3 — Write the failing test for invariant 2 (ConfigHash excludes Scope)

Create `internal/compiler/scope_hash_test.go`:

```go
package compiler

import (
	"testing"

	"infra/pkg/address"
	"infra/pkg/resource"
	"infra/pkg/value"
)

var allScopes = []value.Scope{
	value.ScopeUnset,
	value.ScopeProviderDefault,
	value.ScopeBaseConfig,
	value.ScopeModuleDefault,
	value.ScopeEnvironmentInherit,
	value.ScopeEnvironmentVar,
	value.ScopeCLIOverride,
}

// configAtScope builds a one-resource config whose single attribute carries the
// given scope and the given datum.
func configAtScope(s value.Scope, replicas int64) ResolvedConfig {
	addr := address.Address{Name: "app"}
	return ResolvedConfig{
		Project:     "demo",
		Environment: "production",
		Resources: map[string]*resource.ResolvedResource{
			addr.String(): {
				Address: addr,
				Type:    "test.database",
				Attrs: map[string]value.Value{
					"replicas": value.Int(replicas, value.SourceVariable).WithScope(s),
				},
			},
		},
	}
}

// TestHashIgnoresScope is invariant 2.
//
// ConfigHash answers "was this plan computed against this configuration". M3
// proved the hash must capture the RESOLVED VALUE: two different --var values
// once hashed identically, which would have let a plan computed with one be
// applied with another.
//
// A value of 20 is the same input whether it came from variables.yml or from
// --var, so hashing Scope would make an UNCHANGED configuration look stale in
// M6 — a saved plan would refuse to apply because the user happened to pass
// the same number a different way.
//
// hashValue in resolved.go writes `write("kind", ..., "source", ...)`. Adding
// "scope" beside them is the single-line change this test exists to catch.
func TestHashIgnoresScope(t *testing.T) {
	want, err := configAtScope(value.ScopeUnset, 20).Hash()
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	for _, s := range allScopes {
		got, err := configAtScope(s, 20).Hash()
		if err != nil {
			t.Fatalf("Hash at scope %s: %v", s, err)
		}
		if got != want {
			t.Errorf("hash differs at scope %s: %s != %s; ConfigHash is covering Scope, "+
				"so an unchanged configuration reads as stale in M6", s, got, want)
		}
	}
}

// TestHashStillSeesADifferentValueAtTheSameScope exercises the other answer.
// Without it, `func Hash() { return "", nil }` passes the test above.
func TestHashStillSeesADifferentValueAtTheSameScope(t *testing.T) {
	for _, s := range allScopes {
		a, err := configAtScope(s, 20).Hash()
		if err != nil {
			t.Fatalf("Hash: %v", err)
		}
		b, err := configAtScope(s, 21).Hash()
		if err != nil {
			t.Fatalf("Hash: %v", err)
		}
		if a == b {
			t.Errorf("replicas=20 and replicas=21 hash identically at scope %s; "+
				"ConfigHash has stopped covering the resolved value", s)
		}
	}
}
```

#### 1.4 — Run the tests and confirm the expected failure

```bash
export PATH="$HOME/.local/share/mise/shims:$PATH"
go test -count=1 ./pkg/value/ ./internal/compiler/
```

Expected: **compilation failure**, not assertion failure —
`undefined: Scope`, `undefined: allScopes` … , `v.WithScope undefined`,
`undefined: scopeWireNames`. That is the correct first failure: the type does
not exist yet. If anything compiles, a `Scope` has been added somewhere already
and you should stop and find out where.

#### 1.5 — Create `pkg/value/scope.go`

```go
package value

import (
	"fmt"
	"strconv"
)

// Scope records WHICH PRECEDENCE LEVEL supplied a value. Source records what
// KIND of thing it is. The two are orthogonal and both are needed: a value can
// be Source=variable from either a file or the command line, and a plan that
// cannot tell them apart cannot explain itself.
//
// The constants are the precedence chain from PLAN.md §7 in order, lowest
// first — later scopes win. Stages 3 and 4 build a scope stack in exactly this
// order and a resolved value's Scope is a record of which entry won, so there
// is one implementation and what a plan claims about a value's origin cannot
// drift away from the rule that produced it (spec §7.1).
//
// ScopeUnset MUST remain the zero value. Every Value constructed before M4 —
// and every Value read back from a state file written before M4 — then stays
// valid rather than claiming a precedence level it never had.
//
// WHERE A VARIABLE'S `default:` SITS: ScopeBaseConfig, with Source
// SourceDefault. Ruled 2026-09-10; binding. A default written in infra.yml's
// `variables:` block IS base configuration — it lives in the base
// configuration file, at the level everything in that file sits at — and
// Source already distinguishes it as a default, so no eighth constant is
// needed. Stage 4 stamps it when the default wins. Stage 2 leaves Scope unset
// on VariableDecl.Default, because a declaration is not a resolution and only
// one place may decide which level won.
type Scope uint8

const (
	ScopeUnset              Scope = iota // provenance not recorded
	ScopeProviderDefault                 // provider defaults
	ScopeBaseConfig                      // base configuration
	ScopeModuleDefault                   // module defaults (M5 populates this)
	ScopeEnvironmentInherit              // environment inheritance
	ScopeEnvironmentVar                  // environment variables
	ScopeCLIOverride                     // CLI overrides
)

// String names a scope for display and for diagnostics.
//
// It is a switch with an explicit default rather than a map lookup with a
// fallback, for the reason diag.Severity.String() spells out: an unrecognised
// value must report as unrecognised rather than collapsing into a real level.
// A value silently attributed to the wrong precedence level is worse than one
// attributed to none, because a user would act on it.
//
// ScopeCLIOverride renders as "--var" rather than "CLI override" because that
// is what the user typed, and PLAN.md §44 requires an error to name something
// the user can act on.
func (s Scope) String() string {
	switch s {
	case ScopeUnset:
		return "unset"
	case ScopeProviderDefault:
		return "provider default"
	case ScopeBaseConfig:
		return "base config"
	case ScopeModuleDefault:
		return "module default"
	case ScopeEnvironmentInherit:
		return "environment inheritance"
	case ScopeEnvironmentVar:
		return "environment variable"
	case ScopeCLIOverride:
		return "--var"
	default:
		return "Scope(" + strconv.Itoa(int(s)) + ")"
	}
}

// WithScope returns a copy of the value recorded as supplied by scope s.
func (v Value) WithScope(s Scope) Value {
	v.Scope = s
	return v
}

// scopeWireNames is the frozen on-disk spelling of every Scope.
//
// Deliberately separate from Scope.String(), exactly as kindWireNames is
// separate from Kind.String(): String() is a diagnostic string and free to
// change, whereas a state file is a versioned contract. Persisting the uint8
// would be worse still — M5 inserts ScopeModuleDefault's population and any
// future level inserted mid-chain would silently reinterpret every state file
// ever written.
//
// ScopeUnset is deliberately ABSENT. It encodes to the empty string and is
// omitted from the wire entirely, so state files written before M4 stay
// byte-identical and read back as ScopeUnset.
var scopeWireNames = map[Scope]string{
	ScopeProviderDefault:    "provider_default",
	ScopeBaseConfig:         "base_config",
	ScopeModuleDefault:      "module_default",
	ScopeEnvironmentInherit: "environment_inherit",
	ScopeEnvironmentVar:     "environment_var",
	ScopeCLIOverride:        "cli_override",
}

// scopeToWireName returns the persisted name for a Scope. ScopeUnset maps to
// the empty string, which json omits. An unrecognised scope is an error rather
// than a fallback: refusing to write it turns an unrepresentable value into a
// failed save rather than a state file that reads back wrong.
func scopeToWireName(s Scope) (string, error) {
	if s == ScopeUnset {
		return "", nil
	}
	name, ok := scopeWireNames[s]
	if !ok {
		return "", fmt.Errorf("cannot encode value of scope %s", s)
	}
	return name, nil
}

// scopeFromWireName resolves a persisted scope name back to its Scope. The
// empty string is ScopeUnset — that is the pre-M4 state file case and it is
// not an error.
func scopeFromWireName(s string) (Scope, error) {
	if s == "" {
		return ScopeUnset, nil
	}
	for scope, name := range scopeWireNames {
		if name == s {
			return scope, nil
		}
	}
	return ScopeUnset, fmt.Errorf("unknown value scope %q", s)
}
```

#### 1.6 — Add the field, and document the Equal rule at the site

In `pkg/value/value.go`, add `Scope` to the struct. Locate it with:

```bash
grep -n "Sensitive bool" pkg/value/value.go
```

Add immediately after `Source`:

```go
	Source    ValueSource
	// Scope records which precedence level supplied this value; Source
	// records what kind of thing it is. See scope.go.
	Scope     Scope
	Sensitive bool
```

Then document the ignore-rule where `Equal` lives. Locate it with:

```bash
grep -n "Provenance, sensitivity and origin are deliberately excluded" pkg/value/value.go
```

Replace that sentence with:

```go
// Provenance, sensitivity and origin are deliberately excluded: they describe
// how a value was arrived at, not what the desired state is, so they must never
// cause a plan to show a change.
//
// THAT INCLUDES Scope, and this comment is the only thing saying so — the rule
// holds today by construction (the comparisons below touch Known, Kind and Raw
// and nothing else), which means a future field can be added to this method
// with no signpost that it must not be. Two values that differ only in which
// precedence level supplied them are THE SAME VALUE: a `--var replicas=20`
// that matches what variables.yml already said must plan as no change.
// Comparing Scope breaks acceptance invariant 2 (no-op plan) permanently and
// silently, which is the phantom-diff shape M3 spent a Critical fixing.
// Pinned by TestEqualIgnoresScopeForEveryPairOfScopes (scope_test.go).
```

#### 1.7 — Add `ParseKind` and `KindNames`, and fix the comments they falsify

Task 3 decodes `type: integer` in a user's configuration. `Kind.String()`
already spells `KindInt` as `"integer"`; nothing maps back. The reverse
direction goes here, in the same file, pinned as an exact inverse — a spelling
table in `internal/config` would be a second copy of what this file owns.

**This changes what `Kind.String()` is allowed to be, and two existing comments
say otherwise.** `pkg/value/json.go` and `pkg/value/wire_test.go` both assert
that `Kind.String()` is "a diagnostic string and free to change". That was true
while nothing but diagnostics consumed it. **M4 is the milestone that makes
`type: integer` a configuration keyword** — so M4 is precisely the event that
retires the claim, not a variables feature quietly overturning an unrelated
decision. Once `ParseKind` is `String()`'s inverse, those six strings are the
CONFIGURATION LANGUAGE, and `CLAUDE.md` makes the configuration language a
product API: renaming `"integer"` to `"int"` would break every `infra.yml` in
the wild. Leaving the old comments would leave the tree contradicting itself,
and the next person to improve an error message would follow the comment
saying they may.

Write the three tests at the end of this step FIRST and run them —

```bash
export PATH="$HOME/.local/share/mise/shims:$PATH"
go test -count=1 -run 'TestKind' ./pkg/value/
```

— expecting `undefined: ParseKind`, `undefined: KindNames`. Then add to
`pkg/value/kind.go`:

```go
// ParseKind resolves a type name written in configuration back to its Kind.
//
// INVERSE OF Kind.String(), AND THEY MUST BE CHANGED TOGETHER. This is a
// deliberate switch rather than a shared table, so the pair reads as one unit
// in one file; TestKindStringAndParseKindAreInverses pins the round trip over
// every Kind, so drift cannot survive a test run.
//
// Because this exists, those six strings are no longer merely diagnostic
// output: they are what a user writes as `type:` in infra.yml, and CLAUDE.md
// makes the configuration language a product API. Renaming one breaks every
// configuration file that used it, so a rename needs the same deliberation as
// a state format change — see kindWireNames in json.go, which keeps the
// ON-DISK spelling independent of both and has its own migration path.
//
// "invalid" is deliberately NOT accepted. Kind.String() answers it for
// KindInvalid so a diagnostic can name an unset Kind, but `type: invalid` in
// configuration is a user error and must reach the unknown-type diagnostic
// rather than quietly producing the zero Kind.
func ParseKind(name string) (Kind, bool) {
	switch name {
	case "string":
		return KindString, true
	case "integer":
		return KindInt, true
	case "float":
		return KindFloat, true
	case "boolean":
		return KindBool, true
	case "list":
		return KindList, true
	case "map":
		return KindMap, true
	default:
		return KindInvalid, false
	}
}

// KindNames returns every type name ParseKind accepts, sorted, for diagnostics
// that must tell a user what is available.
//
// Sorted and fixed so an error message does not reorder itself between
// identical runs — invisible in a test suite, obvious to a user diffing two
// outputs.
func KindNames() []string {
	return []string{"boolean", "float", "integer", "list", "map", "string"}
}
```

Now correct the two comments this falsifies.

```bash
grep -n "diagnostic string" pkg/value/json.go pkg/value/wire_test.go
```

In `pkg/value/json.go`, replace the sentence inside `kindWireNames`'s comment
beginning "It is deliberately separate from Kind.String(), which is a
diagnostic string and free to change." with:

```go
// It is deliberately separate from Kind.String(). Since ParseKind exists,
// Kind.String()'s spellings are the CONFIGURATION language and change only
// with the same care; this table is the ON-DISK language. They remain
// independent contracts with different consumers and different migration
// paths. A state file is a versioned contract: renaming a spelling on either
// side must not silently invalidate every state file ever written, and only a
// table nothing else consults can guarantee that. Entries here change only
// alongside a CurrentVersion bump and a migration.
```

In `pkg/value/wire_test.go`, `TestKindWireNameIsIndependentOfString`'s comment
makes the same claim. Replace "never from Kind.String(), which is a diagnostic
string and free to change" with "never from Kind.String(), which is now the
configuration language's spelling and answers for KindInvalid where this table
must refuse it". **The test body is unchanged and still correct** — it proves
`kindToWireName` rejects `KindInvalid` where `Kind.String()` answers
`"invalid"`, which is exactly the asymmetry `ParseKind` relies on too.

Add to `pkg/value/scope_test.go`:

```go
// allKinds is every Kind ParseKind is expected to handle, plus the zero value.
var allKinds = []Kind{KindInvalid, KindString, KindInt, KindFloat, KindBool, KindList, KindMap}

// TestKindStringAndParseKindAreInverses is what makes two switches safe to keep
// as two switches. It loops every Kind rather than sampling: an exhaustive
// round trip cannot be satisfied by an implementation that happens to agree on
// the two cases a test author picked.
func TestKindStringAndParseKindAreInverses(t *testing.T) {
	for _, k := range allKinds {
		name := k.String()
		got, ok := ParseKind(name)
		if k == KindInvalid {
			// The one deliberate asymmetry: String() answers "invalid" so a
			// diagnostic can name an unset Kind, but `type: invalid` in a
			// configuration file must reach the unknown-type diagnostic.
			if ok {
				t.Errorf("ParseKind(%q) accepted the KindInvalid spelling; `type: invalid` would silently become the zero Kind", name)
			}
			continue
		}
		if !ok || got != k {
			t.Errorf("ParseKind(%q) = %v, %v; want %v, true — String and ParseKind have drifted", name, got, ok, k)
		}
	}
}

// TestKindNamesMatchesParseKindExactly stops the diagnostic list from
// advertising a name that does not work, or omitting one that does. Both
// directions, because either alone is satisfied by an empty list.
func TestKindNamesMatchesParseKindExactly(t *testing.T) {
	names := KindNames()
	for _, name := range names {
		if _, ok := ParseKind(name); !ok {
			t.Errorf("KindNames lists %q, which ParseKind rejects", name)
		}
	}
	for _, k := range allKinds {
		if k == KindInvalid {
			continue
		}
		found := false
		for _, name := range names {
			if name == k.String() {
				found = true
			}
		}
		if !found {
			t.Errorf("ParseKind accepts %q but KindNames omits it, so no diagnostic offers it", k.String())
		}
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] >= names[i] {
			t.Fatalf("KindNames is not sorted: %q before %q", names[i-1], names[i])
		}
	}
}

// TestKindSpellingsAreFrozen duplicates the six literals deliberately. They are
// what a user writes in infra.yml, so deriving them from the implementation
// would assert nothing about the product API they now are.
func TestKindSpellingsAreFrozen(t *testing.T) {
	frozen := map[string]Kind{
		"string":  KindString,
		"integer": KindInt,
		"float":   KindFloat,
		"boolean": KindBool,
		"list":    KindList,
		"map":     KindMap,
	}
	if len(KindNames()) != len(frozen) {
		t.Fatalf("KindNames has %d entries, the frozen language contract has %d", len(KindNames()), len(frozen))
	}
	for name, want := range frozen {
		if got, ok := ParseKind(name); !ok || got != want {
			t.Errorf("ParseKind(%q) = %v, %v; want %v, true", name, got, ok, want)
		}
		if got := want.String(); got != name {
			t.Errorf("%v.String() = %q, want %q", want, got, name)
		}
	}
	// The near-misses a user actually types must NOT resolve.
	for _, wrong := range []string{"int", "bool", "str", "number", "Integer", "invalid", ""} {
		if _, ok := ParseKind(wrong); ok {
			t.Errorf("ParseKind accepted %q", wrong)
		}
	}
}
```

#### 1.8 — Add `AsFloat`

Task 4 checks resolved values against `min` and `max` and needs a float
accessor. `pkg/value/value.go` has `AsString`, `AsInt` and `AsBool` and no
`AsFloat` (verified against HEAD). Add it beside them, matching `AsInt`'s shape
exactly:

```bash
grep -n "func (v Value) AsBool" pkg/value/value.go
```

Append after `AsBool`:

```go
// AsFloat reads a float value. Like AsInt, it is a plain type assertion, so it
// answers false for a KindInt value — that value's Raw is an int64, and YAML
// tags `1` as !!int even where a float was declared. A caller doing arithmetic
// across both numeric kinds must handle int64 itself rather than assume this
// coerces.
func (v Value) AsFloat() (float64, bool) {
	f, ok := v.Raw.(float64)
	return f, ok && v.Known
}
```

Add to `pkg/value/scope_test.go`:

```go
// TestAsFloatDoesNotCoerceIntegers pins the trap in the doc comment, in both
// directions. If this ever starts passing for the KindInt case, a caller that
// relied on the distinction to tell 1 from 1.0 has silently changed behaviour.
func TestAsFloatDoesNotCoerceIntegers(t *testing.T) {
	if f, ok := Float(1.5, SourceVariable).AsFloat(); !ok || f != 1.5 {
		t.Errorf("AsFloat on a float = %v, %v; want 1.5, true", f, ok)
	}
	if _, ok := Int(1, SourceVariable).AsFloat(); ok {
		t.Error("AsFloat coerced a KindInt value; callers must handle int64 explicitly")
	}
	if _, ok := Unknown(KindFloat, SourceVariable).AsFloat(); ok {
		t.Error("AsFloat answered true for an unknown value")
	}
}
```

#### 1.9 — Wire `Scope` through JSON

In `pkg/value/json.go`:

```bash
grep -n "Sensitive bool" pkg/value/json.go        # the wireValue field
grep -n "w := wireValue{" pkg/value/json.go       # MarshalJSON
grep -n "out := Value{Kind: kind" pkg/value/json.go  # UnmarshalJSON
```

Add to `wireValue`, after `Source`:

```go
	Scope     string          `json:"scope,omitempty"`
```

In `MarshalJSON`, before building `w`:

```go
	scopeName, err := scopeToWireName(v.Scope)
	if err != nil {
		return nil, err
	}
```

and add `Scope: scopeName,` to the `wireValue` literal.

In `UnmarshalJSON`, after the kind is resolved:

```go
	scope, err := scopeFromWireName(w.Scope)
	if err != nil {
		return err
	}
```

and add `Scope: scope` to the `out := Value{...}` literal.

Note `err` is already declared in both functions by the `kindToWireName` /
`kindFromWireName` calls, so use `=` not `:=` where appropriate; let the
compiler tell you which.

#### 1.10 — Add `Annotate` to `pkg/value/format.go`

Append to `pkg/value/format.go`:

```go
// Annotate renders one value and appends the provenance annotation a plan
// shows beside it — today "[default]", and with M4's scopes
// "[variable, from --var]".
//
// It lives here, beside Format, and delegates the rendering to Format
// unchanged. The rendering must never be reimplemented: Format is the ONLY
// redaction path in the tree, and the two measured leaks documented on it both
// came from a second copy that had drifted. Annotate adds a SUFFIX to whatever
// Format returned and touches Raw not at all, so a sensitive value is
// "<sensitive> [variable, from --var]" — the annotation describes where a
// value came from, which is not itself secret, and hiding it would remove the
// only clue a user has for finding the secret they need to change.
func Annotate(v Value, opts FormatOptions) string {
	s := Format(v, opts)
	if a := annotation(v); a != "" {
		return s + " " + a
	}
	return s
}

// annotation returns the bracketed provenance suffix, or "" when there is
// nothing worth saying.
//
// The suppression rules, and why each exists:
//
//   - An unknown value is not annotated. It has no origin yet — the expression
//     that will produce it does — and "(known after apply) [explicit, from
//     base config]" describes the attribute rather than the value.
//
//   - Ordinary explicit configuration is not annotated. Annotating it would
//     put "[explicit, from base config]" on nearly every line of every plan,
//     which buries the [default] and [variable] markers that actually carry
//     information. PLAN.md §19 wants a plan a human reads.
//
//   - A value with no Scope recorded falls back to M2's behaviour exactly:
//     "[default]" for a default and nothing otherwise. Every Value in the tree
//     has ScopeUnset until stage 4 exists, so this function is output-identical
//     to M3's renderAnnotated today. That is deliberate — a rendering change
//     and a provenance change landing in the same commit would make it
//     impossible to tell which one moved a golden test.
func annotation(v Value) string {
	if !v.Known {
		return ""
	}
	if v.Source == SourceExplicit && (v.Scope == ScopeUnset || v.Scope == ScopeBaseConfig) {
		return ""
	}
	if v.Scope == ScopeUnset {
		if v.Source == SourceDefault {
			return "[default]"
		}
		return ""
	}
	return "[" + string(v.Source) + ", from " + v.Scope.String() + "]"
}
```

#### 1.11 — Test `Annotate` directly

Append to `pkg/value/scope_test.go`. These assert on `Annotate`'s RETURN VALUE,
not on rendered plan output: Task 9 owns the renderer, and a test here that
reached into it would depend on behaviour this task does not build.

```go
// planOpts is how a plan will render values. Duplicated here rather than
// imported, because internal/planner may not import test helpers from
// pkg/value and this task must not depend on the renderer at all.
var planOpts = FormatOptions{Unknown: "(known after apply)", QuoteStrings: true}

// TestAnnotateMatchesTodaysPlanOutputForUnscopedValues pins the compatibility
// half of the contract with Task 9.
//
// Every Value in the tree has ScopeUnset until stage 4 exists, so when Task 9
// points renderAnnotated at Annotate, plan output must not move. These are the
// exact strings internal/planner produces today; if this table is wrong, Task 9
// discovers it as a wall of moved golden tests with no idea which change caused
// them.
func TestAnnotateMatchesTodaysPlanOutputForUnscopedValues(t *testing.T) {
	cases := []struct {
		name string
		in   Value
		want string
	}{
		{"default", Int(100, SourceDefault), "100 [default]"},
		{"explicit", Int(100, SourceExplicit), "100"},
		{"variable", Int(100, SourceVariable), "100"},
		{"provider", String("x", SourceProvider), `"x"`},
		{"sensitive default", String("s", SourceDefault).WithSensitive(true), "<sensitive> [default]"},
		{"unknown", Unknown(KindString, SourceComputed), "(known after apply)"},
	}
	for _, tc := range cases {
		if got := Annotate(tc.in, planOpts); got != tc.want {
			t.Errorf("%s: Annotate = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestAnnotateNamesTheScopeWhenThereIsOne is the forward half: the annotation
// must actually change once a scope is recorded, or stage 4 could stamp every
// value and no plan would ever say so.
func TestAnnotateNamesTheScopeWhenThereIsOne(t *testing.T) {
	cases := []struct {
		in   Value
		want string
	}{
		{Int(20, SourceVariable).WithScope(ScopeCLIOverride), "20 [variable, from --var]"},
		{Int(2, SourceDefault).WithScope(ScopeProviderDefault), "2 [default, from provider default]"},
		{Int(10, SourceEnvironment).WithScope(ScopeEnvironmentVar), "10 [environment, from environment variable]"},
		{Int(1, SourceEnvironment).WithScope(ScopeEnvironmentInherit), "1 [environment, from environment inheritance]"},
	}
	for _, tc := range cases {
		if got := Annotate(tc.in, planOpts); got != tc.want {
			t.Errorf("Annotate = %q, want %q", got, tc.want)
		}
	}
}

// TestAnnotateStaysQuietForOrdinaryConfiguration pins the suppression rule in
// the direction that would otherwise go unnoticed: explicit base configuration
// gets NO annotation, at either ScopeUnset or ScopeBaseConfig. Annotating it
// would put "[explicit, from base config]" on nearly every line of every plan
// and bury the markers that carry information.
func TestAnnotateStaysQuietForOrdinaryConfiguration(t *testing.T) {
	for _, s := range []Scope{ScopeUnset, ScopeBaseConfig} {
		if got := Annotate(String("web", SourceExplicit).WithScope(s), planOpts); got != `"web"` {
			t.Errorf("explicit value at scope %s annotated as %q, want bare", s, got)
		}
	}
	// An unknown value has no origin yet — the expression that will produce it
	// does — so it is never annotated, whatever scope it carries.
	for _, s := range allScopes {
		got := Annotate(Unknown(KindString, SourceVariable).WithScope(s), planOpts)
		if got != "(known after apply)" {
			t.Errorf("unknown value at scope %s annotated as %q", s, got)
		}
	}
}

// TestAnnotateRedactsThroughFormat pins that Annotate never renders anything
// itself. Format is the ONLY redaction path in the tree; a second one is how
// a leak fixed in one place survived in the other. The annotation is a SUFFIX
// on whatever Format returned — where a value came from is not itself secret,
// and hiding it would remove the only clue a user has for finding the secret
// they need to change.
func TestAnnotateRedactsThroughFormat(t *testing.T) {
	secret := String("hunter2", SourceVariable).WithSensitive(true).WithScope(ScopeCLIOverride)
	got := Annotate(secret, planOpts)
	if strings.Contains(got, "hunter2") {
		t.Fatalf("Annotate leaked a sensitive value: %q", got)
	}
	if got != "<sensitive> [variable, from --var]" {
		t.Errorf("Annotate = %q, want %q", got, "<sensitive> [variable, from --var]")
	}
	// Per-leaf: a non-sensitive composite holding a sensitive leaf.
	nested := Map(map[string]Value{
		"password": String("hunter2", SourceVariable).WithSensitive(true),
	}, SourceExplicit)
	if strings.Contains(Annotate(nested, planOpts), "hunter2") {
		t.Error("Annotate leaked a nested sensitive value")
	}
}
```

**Do not wire this to anything.** `internal/planner/render.go` still has its own
`renderAnnotated`, and it must stay untouched until Task 9 points it here. The
two agree by construction today — that is what the first test above pins — so
there is no live duplication to fix in this task.

#### 1.12 — Run everything

```bash
export PATH="$HOME/.local/share/mise/shims:$PATH"
gofmt -l . && go vet ./... && go test -count=1 ./...
```

All green. `gofmt -l .` must print nothing.

#### 1.13 — Commit

```bash
git add -A && git commit -m "$(cat <<'EOF'
value: add Scope, the precedence level a value came from

Source records what kind of thing a value is; Scope records which of
PLAN.md §7's six precedence levels supplied it. The two are orthogonal:
a Source=variable value can come from variables.yml or from --var, and a
plan that cannot tell them apart cannot explain itself.

Three invariants, each with a test that fails against a violation:
Equal ignores Scope (or a --var matching variables.yml plans as a change
forever), ConfigHash excludes Scope (or an unchanged configuration reads
as stale in M6), and Scope round-trips through JSON (or state show
reports ScopeUnset for values that have a scope).

The Equal ignore-rule is now documented at the site; it previously held
only by construction, with nothing telling the next person adding a
field.

value.Annotate is added beside the one redaction path, and wired to
nothing: Task 9 owns rendering provenance and points renderAnnotated at
it. Annotate is output-identical to today's renderAnnotated for any value
whose Scope is unset, which is every value until stage 4, so that wiring
is a pure refactor when it happens.

AsFloat joins AsString/AsInt/AsBool for stage 4's min/max checking. Like
AsInt it does not coerce, so a KindInt value reads false; that is safe
because stage 2 coerces a declared bound to its declared type, so a
bound's Kind always matches the variable's.

ParseKind is the reverse of Kind.String(), which had no inverse — so
stage 2's variable declarations need no spelling table of their own, and
pkg/value owns those six strings in both directions.

M4 is what makes that correct: until now the spellings were internal, so
json.go documented Kind.String() as free to change. `type: integer` is a
configuration keyword from this milestone on, so the two comments still
claiming that freedom are corrected. kindWireNames stays independent —
the on-disk spelling is a separate contract with its own migration path.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 2 — Stage 1: `Load` reads `variables.yml` and `environments/*.yml`

### Why this task exists

Spec §7's stage table says stage 1 reads "`infra.yml`, `variables.yml`,
`environments/*.yml`, module files". Today `internal/config/load.go` reads only
`infra.yml`, and its own doc comment says M4 extends it. `PLAN.md` §8 fixes the
two new shapes:

```yaml
# variables.yml — a FLAT mapping of variable name to value
project_name: myapp
region: us-east-1
```

```yaml
# environments/production.yml — this environment's overrides
replicas: 10
```

Stage 1 does no interpretation. It reads bytes into `yaml.Node` trees, records
what kind of file each one is, and returns them in a fixed order. Stage 2
(Task 3) is the only stage permitted to touch `yaml.Node`, so nothing in this
task may inspect a node's contents.

### Files

| Action | Path |
|--------|------|
| modify | `internal/config/load.go` |
| create | `internal/config/load_test.go` |

### Interfaces

**Consumes (from HEAD):**

```go
// internal/config
const ProjectFileName = "infra.yml"
type File struct { Path string; Root *yaml.Node }
func Load(dir string) ([]File, error)
```

**Produces (Task 3 consumes all of these; later tasks consume `Load`):**

```go
// internal/config
const ProjectFileName    = "infra.yml"
const VariablesFileName  = "variables.yml"
const EnvironmentsDirName = "environments"

type FileKind uint8
const (
    FileProject FileKind = iota
    FileVariables
    FileEnvironment
)
func (k FileKind) String() string

type File struct {
    Path        string
    Kind        FileKind
    Environment string   // set only when Kind == FileEnvironment
    Root        *yaml.Node
}

func Load(dir string) ([]File, error)
```

### The behaviour this task DECIDES

The spec says which files stage 1 reads and nothing about the edges. These are
the rulings; each one is implemented below and each one has a test.

1. **`variables.yml` absent ⇒ not an error, not in the slice.** A project with
   no variables is a valid project, and every M1/M2 project is one.
2. **`environments/` absent ⇒ not an error.** Environments may be declared
   inline under `infra.yml`'s `environments:` key instead (`PLAN.md` §6, §7).
3. **`environments/` present but containing no YAML ⇒ not an error.** An empty
   directory says nothing. A user who names a nonexistent environment on the
   command line gets a far better message from stage 3, which knows what
   environments DO exist.
4. **A malformed file ⇒ `error`, not a diagnostic.** A YAML syntax error
   produces no node tree, so there is no `Origin` to hang a diagnostic on and
   stage 2 could not say where the problem is. This is exactly how `infra.yml`
   has behaved since M1; the error names the path.
5. **Order is fixed: `infra.yml`, then `variables.yml`, then
   `environments/*.yml` sorted by environment name.** Three reasons, and the
   first is the one that bites: stage 2's duplicate diagnostics report where the
   FIRST definition was, so "first" must not depend on the filesystem. Second,
   `infra.yml` must come first so that `ProjectDecl`'s project name and origin
   come from the project file. Third, invariant 6 (plan determinism) —
   `os.ReadDir` happens to sort, but that is a property of a function this code
   does not own.
6. **Both `.yml` and `.yaml` are accepted in `environments/`, and a directory
   containing both `X.yml` and `X.yaml` is an error.** Silently ignoring
   `.yaml` would produce an environment that exists in the repository and not
   in the tool; silently picking one of two would lose the other.
7. **Non-YAML entries and subdirectories in `environments/` are skipped
   silently.** A real repository has a `README.md`, and environments do not
   nest, so recursing would invent a structure nothing else understands.

### Global constraints this task could violate

- No new third-party dependency (`cobra` and `yaml.v3` only). Everything here
  is `os`, `path/filepath`, `sort`, `strings`, `fmt`, plus the existing
  `yaml.v3`.
- **Stage 2 is the only stage that may touch `yaml.Node`.** This task
  *produces* nodes and must never read one: no `node.Content`, no `node.Value`,
  no `node.Kind`. If you find yourself wanting to check "is this file empty",
  that belongs in Task 3.

### Steps

#### 2.1 — Write the failing tests

Create `internal/config/load_test.go`:

```go
package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTree writes files into a fresh temp dir. Keys are slash-separated paths
// relative to the dir.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range files {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	return dir
}

const minimalProject = "project: demo\nresources: {}\n"

// TestLoadOrderIsFixedAndNotTheFilesystemOrder pins ruling 5.
//
// The fixture is built so that the natural orders CONTRADICT the required
// order, which is the only way an ordering assertion can fail against broken
// code:
//
//   - "aaa" and "bbb" sort before "infra.yml" as full paths
//     ("<dir>/environments/aaa.yml" < "<dir>/infra.yml"), so a global path
//     sort puts the environments first and fails here.
//   - the environments are created zulu, aaa, bbb, so creation order fails.
//   - "variables.yml" sorts after all of them, so a global path sort also
//     puts it last and fails here.
func TestLoadOrderIsFixedAndNotTheFilesystemOrder(t *testing.T) {
	dir := t.TempDir()
	writeFile := func(rel, body string) {
		t.Helper()
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("environments/zulu.yml", "replicas: 1\n")
	writeFile("environments/aaa.yml", "replicas: 2\n")
	writeFile("environments/bbb.yml", "replicas: 3\n")
	writeFile("variables.yml", "region: us-east-1\n")
	writeFile(ProjectFileName, minimalProject)

	files, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := []string{
		ProjectFileName,
		VariablesFileName,
		filepath.Join(EnvironmentsDirName, "aaa.yml"),
		filepath.Join(EnvironmentsDirName, "bbb.yml"),
		filepath.Join(EnvironmentsDirName, "zulu.yml"),
	}
	if len(files) != len(want) {
		t.Fatalf("Load returned %d files, want %d: %v", len(files), len(want), pathsOf(files))
	}
	for i, w := range want {
		if got := filepath.Join(dir, w); files[i].Path != got {
			t.Errorf("files[%d].Path = %s, want %s", i, files[i].Path, got)
		}
	}
}

func pathsOf(files []File) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.Path
	}
	return out
}

// TestLoadTagsEachFileWithItsKind pins that Task 3 can tell the three shapes
// apart. Without Kind, stage 2 would have to guess from the path, and a flat
// variables.yml decoded as a project document produces "unrecognised top-level
// key" for every variable in it.
func TestLoadTagsEachFileWithItsKind(t *testing.T) {
	dir := writeTree(t, map[string]string{
		ProjectFileName:               minimalProject,
		VariablesFileName:             "region: us-east-1\n",
		"environments/production.yml": "replicas: 10\n",
	})
	files, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(files) != 3 {
		t.Fatalf("got %d files, want 3: %v", len(files), pathsOf(files))
	}
	if files[0].Kind != FileProject || files[0].Environment != "" {
		t.Errorf("infra.yml: Kind=%v Environment=%q", files[0].Kind, files[0].Environment)
	}
	if files[1].Kind != FileVariables || files[1].Environment != "" {
		t.Errorf("variables.yml: Kind=%v Environment=%q", files[1].Kind, files[1].Environment)
	}
	if files[2].Kind != FileEnvironment || files[2].Environment != "production" {
		t.Errorf("environments/production.yml: Kind=%v Environment=%q", files[2].Kind, files[2].Environment)
	}
	for i, f := range files {
		if f.Root == nil {
			t.Errorf("files[%d] (%s) has a nil Root", i, f.Path)
		}
	}
}

// TestLoadTreatsTheOptionalFilesAsOptional pins rulings 1, 2, 3 and 7. Each
// case must return exactly the project file and no error.
func TestLoadTreatsTheOptionalFilesAsOptional(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		dirs  []string
	}{
		{
			name:  "neither present",
			files: map[string]string{ProjectFileName: minimalProject},
		},
		{
			name:  "environments dir exists but is empty",
			files: map[string]string{ProjectFileName: minimalProject},
			dirs:  []string{EnvironmentsDirName},
		},
		{
			name: "environments dir holds only non-YAML",
			files: map[string]string{
				ProjectFileName:            minimalProject,
				"environments/README.md":   "notes\n",
				"environments/.gitkeep":    "",
				"environments/notes.txt":   "x\n",
			},
		},
		{
			name: "environments dir holds a subdirectory",
			files: map[string]string{
				ProjectFileName:                minimalProject,
				"environments/old/prod.yml":    "replicas: 1\n",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeTree(t, tc.files)
			for _, d := range tc.dirs {
				if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			files, err := Load(dir)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if len(files) != 1 || files[0].Kind != FileProject {
				t.Fatalf("got %v, want just the project file", pathsOf(files))
			}
		})
	}
}

// TestLoadAcceptsBothYAMLSpellings is the other half of ruling 6: .yaml must
// work, not merely fail loudly when doubled.
func TestLoadAcceptsBothYAMLSpellings(t *testing.T) {
	dir := writeTree(t, map[string]string{
		ProjectFileName:            minimalProject,
		"environments/dev.yaml":    "replicas: 1\n",
		"environments/staging.yml": "replicas: 2\n",
	})
	files, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var envs []string
	for _, f := range files {
		if f.Kind == FileEnvironment {
			envs = append(envs, f.Environment)
		}
	}
	if len(envs) != 2 || envs[0] != "dev" || envs[1] != "staging" {
		t.Fatalf("environments = %v, want [dev staging]", envs)
	}
}

// TestLoadRefusesAnAmbiguousEnvironment pins ruling 6's error half. Picking one
// silently would drop the other file's overrides from every plan, with nothing
// printed.
func TestLoadRefusesAnAmbiguousEnvironment(t *testing.T) {
	dir := writeTree(t, map[string]string{
		ProjectFileName:                minimalProject,
		"environments/production.yml":  "replicas: 10\n",
		"environments/production.yaml": "replicas: 99\n",
	})
	_, err := Load(dir)
	if err == nil {
		t.Fatal("Load accepted production.yml and production.yaml together; one would silently shadow the other")
	}
	for _, want := range []string{"production", "production.yml", "production.yaml"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %q: %v", want, err)
		}
	}
}

// TestLoadReportsMalformedOptionalFiles pins ruling 4 for both new shapes.
func TestLoadReportsMalformedOptionalFiles(t *testing.T) {
	cases := []struct{ name, path string }{
		{"variables.yml", VariablesFileName},
		{"environment file", "environments/production.yml"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeTree(t, map[string]string{
				ProjectFileName: minimalProject,
				tc.path:         "replicas: [1, 2\n",
			})
			_, err := Load(dir)
			if err == nil {
				t.Fatal("Load accepted malformed YAML")
			}
			if !strings.Contains(err.Error(), filepath.Base(tc.path)) {
				t.Errorf("error does not name the file: %v", err)
			}
		})
	}
}

// TestLoadStillDemandsTheProjectFile is the regression guard for M1's
// behaviour: making three files optional must not make the fourth one optional
// too. An empty dir returning zero files and no error would compile a config
// with no resources, and invariant 1 reads that as "everything was removed" —
// a plan destroying every managed resource.
func TestLoadStillDemandsTheProjectFile(t *testing.T) {
	dir := writeTree(t, map[string]string{
		VariablesFileName: "region: us-east-1\n",
	})
	_, err := Load(dir)
	if err == nil {
		t.Fatal("Load accepted a directory with no infra.yml")
	}
	if !strings.Contains(err.Error(), "infra init") {
		t.Errorf("error should still suggest `infra init`: %v", err)
	}
}
```

#### 2.2 — Run and confirm the expected failure

```bash
export PATH="$HOME/.local/share/mise/shims:$PATH"
go test -count=1 ./internal/config/
```

Expected: **compilation failure** — `undefined: VariablesFileName`,
`undefined: EnvironmentsDirName`, `undefined: FileKind`, `undefined:
FileProject`, `f.Kind undefined`, `f.Environment undefined`. The constants and
the field do not exist yet.

#### 2.3 — Rewrite `internal/config/load.go`

```go
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ProjectFileName is the root configuration file. It is the only required one.
const ProjectFileName = "infra.yml"

// VariablesFileName holds project-wide variable VALUES (PLAN.md §8): a flat
// mapping of name to value. Typed DECLARATIONS — type, default, min, max —
// live under infra.yml's `variables:` key instead (PLAN.md §9). The two are
// different things and stage 2 decodes them differently, which is why File
// carries a Kind rather than letting stage 2 guess from the path.
const VariablesFileName = "variables.yml"

// EnvironmentsDirName holds one file per environment (PLAN.md §8). Each file
// is that environment's overrides.
const EnvironmentsDirName = "environments"

// FileKind says which of the three shapes a loaded file has. Stage 1 does not
// read a file's contents — it is not permitted to touch yaml.Node — so this is
// derived entirely from the path.
type FileKind uint8

const (
	// FileProject is infra.yml: `project`, `resources`, `variables`,
	// `environments`.
	FileProject FileKind = iota
	// FileVariables is variables.yml: a flat mapping of name to value.
	FileVariables
	// FileEnvironment is environments/<name>.yml: one environment's overrides.
	FileEnvironment
)

// String names a file kind for diagnostics. Explicit default, for the reason
// diag.Severity.String() gives: an unrecognised kind must not report as a real
// one.
func (k FileKind) String() string {
	switch k {
	case FileProject:
		return "project file"
	case FileVariables:
		return "variables file"
	case FileEnvironment:
		return "environment file"
	default:
		return fmt.Sprintf("FileKind(%d)", uint8(k))
	}
}

// File is a parsed source file. The node tree keeps line and column
// information, which every diagnostic depends on.
type File struct {
	Path string
	Kind FileKind
	// Environment is the environment a FileEnvironment file configures, taken
	// from its base name with the extension removed. Empty for other kinds.
	Environment string
	Root        *yaml.Node
}

// Load reads the project file, the optional variables file, and every
// environment file, in that order (spec §7, stage 1).
//
// THE ORDER IS PART OF THE CONTRACT. Stage 2's duplicate diagnostics say where
// the FIRST definition was, so "first" must not depend on the filesystem;
// infra.yml must come first so the project name and origin come from the
// project file; and invariant 6 (plan determinism) means the same directory
// must always produce the same slice. os.ReadDir happens to sort its results,
// but that is a property of a function this code does not own.
//
// infra.yml is required. The other two are optional and their absence is not a
// diagnostic: a project with no variables is a valid project, and environments
// may be declared inline under infra.yml's `environments:` key instead.
//
// A YAML syntax error is returned as an error rather than collected as a
// diagnostic, because a file that did not parse has no node tree and therefore
// no Origin for a diagnostic to point at.
func Load(dir string) ([]File, error) {
	project, err := loadProjectFile(dir)
	if err != nil {
		return nil, err
	}
	files := []File{project}

	vars, found, err := loadOptionalFile(filepath.Join(dir, VariablesFileName), FileVariables, "")
	if err != nil {
		return nil, err
	}
	if found {
		files = append(files, vars)
	}

	envs, err := loadEnvironmentDir(filepath.Join(dir, EnvironmentsDirName))
	if err != nil {
		return nil, err
	}
	return append(files, envs...), nil
}

func loadProjectFile(dir string) (File, error) {
	path := filepath.Join(dir, ProjectFileName)
	f, found, err := loadOptionalFile(path, FileProject, "")
	if err != nil {
		return File{}, err
	}
	if !found {
		return File{}, fmt.Errorf("no %s found in %s; run `infra init` to create one", ProjectFileName, dir)
	}
	return f, nil
}

// loadOptionalFile reads one file, reporting absence as (File{}, false, nil)
// rather than as an error, so that callers decide whether absence matters.
func loadOptionalFile(path string, kind FileKind, environment string) (File, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return File{}, false, nil
		}
		return File{}, false, err
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return File{}, false, fmt.Errorf("%s: %w", path, err)
	}
	return File{Path: path, Kind: kind, Environment: environment, Root: &root}, true, nil
}

// loadEnvironmentDir reads environments/, returning its files sorted by
// environment name.
//
// The directory is optional in its entirety, and so is its content: an empty
// environments/ says nothing, and a user who names an environment that does
// not exist gets a far better message from stage 3, which knows which ones do.
func loadEnvironmentDir(dir string) ([]File, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	paths := map[string]string{} // environment name -> path
	var names []string
	for _, e := range entries {
		// Environments do not nest. Recursing would invent a structure
		// nothing else in the engine understands, and a directory called
		// `old/` is a perfectly ordinary thing to find in a real repository.
		if e.IsDir() {
			continue
		}
		name, ok := environmentNameFor(e.Name())
		if !ok {
			// Not YAML. README.md, .gitkeep and editor droppings live in real
			// directories; refusing them would make the tool unusable.
			continue
		}
		path := filepath.Join(dir, e.Name())
		if other, dup := paths[name]; dup {
			// Both spellings of the same environment. Picking one would drop
			// the other file's overrides from every plan with nothing
			// printed — the exact silent-loss shape this engine refuses.
			first, second := other, path
			if second < first {
				first, second = second, first
			}
			return nil, fmt.Errorf(
				"environment %q is defined by both %s and %s; one would silently shadow the other. "+
					"Delete or rename one of them.", name, first, second)
		}
		paths[name] = path
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]File, 0, len(names))
	for _, name := range names {
		f, found, err := loadOptionalFile(paths[name], FileEnvironment, name)
		if err != nil {
			return nil, err
		}
		if !found {
			// Deleted between ReadDir and ReadFile. An environment that is no
			// longer there is the same as one that never was.
			continue
		}
		out = append(out, f)
	}
	return out, nil
}

// environmentNameFor returns the environment a file in environments/
// configures, and whether the file is a YAML file at all.
//
// Both .yml and .yaml are accepted. Ignoring one of them would produce an
// environment that exists in the repository and not in the tool, which the
// user discovers as "unknown environment" while looking straight at the file.
func environmentNameFor(base string) (string, bool) {
	for _, ext := range []string{".yml", ".yaml"} {
		if strings.HasSuffix(base, ext) {
			name := strings.TrimSuffix(base, ext)
			if name == "" {
				// A file literally named ".yml" names no environment.
				return "", false
			}
			return name, true
		}
	}
	return "", false
}
```

#### 2.4 — Run

```bash
export PATH="$HOME/.local/share/mise/shims:$PATH"
gofmt -l . && go vet ./... && go test -count=1 ./...
```

`internal/config`, `internal/compiler` and `internal/cli` all construct or
consume `File`. Adding fields to a struct built with keyed literals is
source-compatible, so nothing else should need changing; if a positional
`File{path, root}` literal exists anywhere, fix it to keyed form rather than
reordering the struct.

#### 2.5 — Commit

```bash
git add -A && git commit -m "$(cat <<'EOF'
config: stage 1 loads variables.yml and environments/*.yml

Spec §7 makes stage 1 responsible for all three shapes; only infra.yml
was read. Each File now carries a FileKind so stage 2 decodes the flat
variables file and the per-environment override files differently from
the project document rather than guessing from the path.

infra.yml stays required. The rest are optional, including an empty
environments/ — a user who names a nonexistent environment gets a better
message from stage 3, which knows which ones exist.

The returned order is fixed (project, variables, environments by name)
because stage 2's duplicate diagnostics report where the FIRST definition
was, and "first" must not depend on the filesystem.

Both .yml and .yaml are accepted; a directory holding both spellings of
one environment is refused rather than silently shadowing one.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 3 — Stage 2: `VariableDecl` and `EnvironmentDecl`

### Why this task exists

Stage 2 turns `yaml.Node` into typed declarations, and **it is the only stage
permitted to touch `yaml.Node`** (spec §7). That is not a style rule: it is how
`PLAN.md` §42's "raw YAML maps must not flow through the application" is
enforced structurally. Every YAML shape-check for variables and environments
therefore lives here, where the line and column numbers still exist. A check
deferred to stage 3 or 4 can only say "something is wrong", not where.

`PLAN.md` §9 fixes the variable declaration:

```yaml
variables:
  replicas:
    type: integer
    default: 2
    min: 1
    max: 100
```

`PLAN.md` §6 and §7 fix environments — and use **two spellings** for overrides:

```yaml
environments:
  dev:
    variables:        # §6 spelling
      replicas: 1
  production:
    extends: default  # §7 spelling: overrides are flat keys
    replicas: 10
```

Both are accepted. `extends` is reserved in both spellings and in
`environments/<name>.yml`.

### Files

| Action | Path |
|--------|------|
| modify | `internal/config/declarations.go` |
| modify | `internal/config/decode.go` |
| create | `internal/config/decode_variables_test.go` |
| create | `internal/config/decode_environments_test.go` |

### Interfaces

**Consumes (Task 2's Produces, and HEAD):**

```go
// internal/config — from Task 2
const ProjectFileName = "infra.yml"
const VariablesFileName = "variables.yml"
type FileKind uint8
const ( FileProject FileKind = iota; FileVariables; FileEnvironment )
type File struct { Path string; Kind FileKind; Environment string; Root *yaml.Node }
func Load(dir string) ([]File, error)

// internal/config — existing, unexported, reuse rather than reinvent
func requireScalar(path, what string, node *yaml.Node, ds *diag.Diagnostics) (string, bool)
func decodeValue(path, what string, node *yaml.Node, ds *diag.Diagnostics) (value.Value, bool)
func originOf(path string, n *yaml.Node) value.Origin
func describeOrigin(o value.Origin) string
func documentRoot(n *yaml.Node) *yaml.Node

// pkg/value — Kind and the two accessors exist at HEAD
type Kind uint8 // KindInvalid, KindString, KindInt, KindFloat, KindBool, KindList, KindMap
func (v Value) WithSource(src ValueSource) Value
func (v Value) WithOrigin(o Origin) Value
func (v Value) AsInt() (int64, bool)

// pkg/value — from Task 1. This package must NOT define its own copies.
func ParseKind(name string) (Kind, bool)   // inverse of Kind.String()
func KindNames() []string                  // sorted, for diagnostics
func (v Value) AsFloat() (float64, bool)
```

**Produces (Tasks 4+ consume these exactly; the contract's stage 3 and stage 4
signatures take these types):**

```go
// internal/config
type VariableDecl struct {
    Name           string
    Type           value.Kind   // KindInvalid means untyped
    Default        value.Value
    HasDefault     bool
    Min, Max       value.Value
    HasMin, HasMax bool
    Origin         value.Origin
}

type OverrideDecl struct {
    Name   string
    Value  value.Value
    Origin value.Origin
}

type EnvironmentDecl struct {
    Name          string
    Extends       string
    ExtendsOrigin value.Origin
    Overrides     []OverrideDecl // sorted by Name
    Origin        value.Origin
}

// ProjectDecl gains:
//   Variables      []VariableDecl    // sorted by Name
//   Environments   []EnvironmentDecl // sorted by Name
//   VariableValues map[string]value.Value  // variables.yml; never nil

func Decode(files []File) (*ProjectDecl, diag.Diagnostics)
```

Three shape choices here were contested during authoring and are now RULED.
Each reason is worth carrying, because each was nearly decided the other way:

- **`Type` is a `value.Kind`, not a `string`.** Stage 2's job is typed
  declarations (spec §7), so the spellings a user may write (`integer`,
  `boolean`, …) are validated HERE, where line and column still exist, and an
  unknown one becomes a diagnostic rather than a string passed onward. The
  table that maps them is unexported in `internal/config`, so a `string` Type
  would leave `internal/variables` unable to interpret its own input — it would
  compare raw strings or keep a second copy of the table, and two
  implementations of one concept is the defect that leaked a plaintext secret
  in M2. Task 4's check is `v.Kind == decl.Type`.

- **`Min`/`Max` are `value.Value`, not `float64`.** `int64` does not fit
  `float64` exactly, so a bound above 2^53 would validate wrongly. `value.Value`
  also carries `Origin`, which lets a range diagnostic point at the line where
  the minimum was declared rather than only at the value that failed it. The
  cost is that Task 4's comparison must be kind-aware — `int64` against `int64`
  exactly, `float64` only when a side genuinely is one.

- **`Overrides` is a sorted slice, not a map.** A map loses each override's
  `Origin` as a first-class field and forces every consumer to re-sort. M3
  measured ELEVEN redundant sorts in this tree whose only job was undoing map
  iteration; sorting once, here, is what stops the twelfth.

Contract note for Task 4's author: `variables.Resolve`'s `files
map[string]value.Value` parameter is fed from `ProjectDecl.VariableValues` —
what `variables.yml` declared. That is a different thing from
`compiler.Options.FileVars`, which is what `--var-file` supplied and is a CLI
input, not a `ProjectDecl` field. They sit at different precedence levels and
must not be merged.

### The behaviour this task DECIDES

1. **A `variables:` entry's body must be a MAPPING** of `type` / `default` /
   `min` / `max`. `replicas: 2` under `variables:` is an error, not a shorthand
   default. A bare value would be ambiguous with a declaration whose type is
   `map`, and stage 2 would have to guess. Values belong in `variables.yml`.
2. **Variable type spellings come from `pkg/value`, which owns them in both
   directions.** `value.ParseKind` is the inverse of `value.Kind.String()` and
   `value.KindNames()` supplies the diagnostic list, both added in Task 1. This
   package keeps NO table of its own: `Kind.String()` already spelled `KindInt`
   as `"integer"`, so a table here would be a second copy that drifts from the
   one the parser consults. The on-disk spelling (`kindWireNames`) stays
   separate — that is a different contract with its own migration path.
3. **`min`/`max` are numeric-only, checked after the whole mapping is walked.**
   `type:` may appear textually after `min:`, so checking as you go would accept
   `min: 1` on a string whenever the file happened to be written in that order.
4. **`min`/`max` bounds are stored as `value.Value`, not `float64`.** `int64`
   does not fit `float64` exactly, so a bound above 2^53 would validate
   wrongly, and `value.Value` carries the `Origin` a range diagnostic needs to
   point at the line where the bound was declared. Both numeric kinds are
   accepted for a bound whatever the declared type, because YAML tags `min: 1`
   as `!!int` even under `type: float`.
5. **`min > max` is reported here.** It is a defect in the DECLARATION, visible
   with nothing but the declaration in hand, and no value can ever satisfy it.
   Stage 4 owns range checking of actual values; it does not re-check this.
6. **Stage 2 does not detect `extends` cycles, and does not report an unknown
   parent.** Both need the whole set of environments; stage 3 owns them
   (spec §7's stage table says so explicitly). Stage 2 checks only that
   `extends` is a usable scalar. Two implementations of one concept is what
   leaked a plaintext secret in M2.
7. **Stage 2 never sets `Scope`.** A declaration is not a resolution; which
   precedence level wins is stages 3 and 4's answer. Setting it here would give
   two places that decide provenance. Specifically: a variable's `default:`
   resolves to `SourceDefault` + `ScopeBaseConfig` (ruled 2026-09-10, binding —
   see `pkg/value/scope.go`), and **stage 4 stamps that**, not stage 2.
8. **An environment declared in both `infra.yml` and `environments/<name>.yml`
   MERGES**, and only a key set in both places is an error. `PLAN.md` §7 puts
   `extends` in the block and §8 puts overrides in the file, so a user
   combining them is following the spec. A key set twice, however, silently
   loses one value.
9. **Variables and resources are separate namespaces** and stage 2 does not
   cross-check them. `${db}` is a variable reference and `${db.host}` is a
   resource reference; stage 6 already distinguishes them.
10. **An empty `variables.yml` or environment file is fine.** Only an empty
    `infra.yml` is an error.
11. **A variable declaring neither a `type` nor a `default` is an ERROR**, not
    a dropped declaration with a warning. Dropping it discards something the
    user wrote and carries on as though they had not, which is the silent-loss
    shape this engine refuses everywhere else, with a log line in front of it.
    The error is suppressed when another diagnostic already names the root
    cause for the same declaration — an unusable `type`, an unusable `default`,
    or a bound whose diagnostic already says to add a type.
12. **`variables.yml` holds VALUES ONLY. It may not carry a `variables:` schema
    block.** Declarations live in `infra.yml` under `variables:` and nowhere
    else. `PLAN.md` §8 shows `variables.yml` as a flat mapping and §9 puts the
    schema in the configuration, so this follows the spec rather than extending
    it. Two files that can each declare a variable means "where is `replicas`
    declared?" has two answers and needs merge rules for `type`, `default`,
    `min` and `max` individually — four more silent-loss cases for a capability
    nobody asked for. It is also genuinely ambiguous: a flat namespace permits
    a variable named `variables`, and there would be no way to tell the two
    readings apart. A top-level `variables:` key in `variables.yml` therefore
    produces a WARNING naming where declarations go, not an error, because that
    variable name is legal. Task 4 is unaffected either way — it consumes
    `[]VariableDecl` and `map[string]value.Value` and does not care which file
    they came from.

### Global constraints this task could violate

- No new third-party dependency (`cobra`, `yaml.v3` only).
- **Stage 2 is the last place `yaml.Node` may be touched.** Nothing this task
  produces may contain a `*yaml.Node`, and nothing downstream may need one.
- **Exactly one redaction path** — this task renders nothing; if you find
  yourself formatting a value for a diagnostic, use `value.Format`.
- Provenance is **per-leaf** (spec §5.1). Retagging a composite's `Source` at
  the top level only is a silent provenance bug; use the recursive helper
  below.

### Steps

#### 3.1 — Write the failing variable tests

Create `internal/config/decode_variables_test.go`:

```go
package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"infra/internal/diag"
	"infra/pkg/value"
)

// decodeTree writes files to a temp dir, runs the REAL Load, and decodes the
// result.
//
// It deliberately goes through Load rather than hand-building []File. Whether
// stage 2 gets its files in the right order and tagged with the right FileKind
// is a precondition PRODUCTION establishes, and two M3 Criticals hid behind
// fixtures that manufactured preconditions production never established.
func decodeTree(t *testing.T, files map[string]string) (*ProjectDecl, diag.Diagnostics) {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range files {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return Decode(loaded)
}

// errorSummaries returns every error diagnostic's summary, for assertions that
// care that SOMETHING was reported and what it was about.
func errorSummaries(ds diag.Diagnostics) []string {
	var out []string
	for _, d := range ds {
		if d.Severity == diag.SeverityError {
			out = append(out, d.Summary)
		}
	}
	return out
}

func requireNoErrors(t *testing.T, ds diag.Diagnostics) {
	t.Helper()
	if ds.HasErrors() {
		t.Fatalf("unexpected errors: %v", errorSummaries(ds))
	}
}

// requireErrorAbout asserts exactly one error and that it mentions each
// fragment. Asserting the COUNT matters: a check that fires twice is a user
// reading the same problem described two different ways.
func requireErrorAbout(t *testing.T, ds diag.Diagnostics, fragments ...string) diag.Diagnostic {
	t.Helper()
	var errs []diag.Diagnostic
	for _, d := range ds {
		if d.Severity == diag.SeverityError {
			errs = append(errs, d)
		}
	}
	if len(errs) != 1 {
		t.Fatalf("want exactly 1 error, got %d: %v", len(errs), errorSummaries(ds))
	}
	text := errs[0].Summary + " | " + errs[0].Detail + " | " + errs[0].Action
	for _, f := range fragments {
		if !strings.Contains(text, f) {
			t.Errorf("diagnostic does not mention %q: %s", f, text)
		}
	}
	if errs[0].Origin.Line == 0 {
		t.Errorf("diagnostic has no line number; stage 2 is the last stage that has one: %s", text)
	}
	return errs[0]
}

func findVariable(t *testing.T, p *ProjectDecl, name string) VariableDecl {
	t.Helper()
	for _, v := range p.Variables {
		if v.Name == name {
			return v
		}
	}
	t.Fatalf("no variable %q in %v", name, p.Variables)
	return VariableDecl{}
}

const projectWithNoResources = "project: demo\nresources: {}\n"

// TestDecodeVariableDeclaration decodes PLAN.md §9's example verbatim.
func TestDecodeVariableDeclaration(t *testing.T) {
	p, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources + `
variables:
  replicas:
    type: integer
    default: 2
    min: 1
    max: 100
`,
	})
	requireNoErrors(t, ds)

	v := findVariable(t, p, "replicas")
	if v.Type != value.KindInt {
		t.Errorf("Type = %v, want KindInt", v.Type)
	}
	if !v.HasDefault {
		t.Fatal("HasDefault = false")
	}
	// KindInt, not KindString: a default decoded as text would compare
	// against min and max by text, where "10" < "9".
	if got, ok := v.Default.AsInt(); !ok || got != 2 {
		t.Errorf("Default = %#v, want Int(2)", v.Default)
	}
	if !v.HasMin || !v.HasMax {
		t.Fatalf("HasMin=%v HasMax=%v", v.HasMin, v.HasMax)
	}
	// KindInt, not KindFloat and not KindString: bounds are stored as decoded,
	// so an integer bound stays an int64 and compares exactly.
	if got, ok := v.Min.AsInt(); !ok || got != 1 {
		t.Errorf("Min = %#v, want Int(1)", v.Min)
	}
	if got, ok := v.Max.AsInt(); !ok || got != 100 {
		t.Errorf("Max = %#v, want Int(100)", v.Max)
	}
	// A range diagnostic must be able to point at where the bound was
	// declared, which is the second reason these are Values and not floats.
	if v.Min.Origin.Line == 0 {
		t.Error("Min has no Origin; a range error could not name the line the bound is on")
	}
	if v.Origin.Line == 0 || !strings.HasSuffix(v.Origin.File, ProjectFileName) {
		t.Errorf("Origin = %v, want a line in %s", v.Origin, ProjectFileName)
	}
	if v.Default.Scope != value.ScopeUnset {
		t.Errorf("Default.Scope = %v; stage 2 declares, it does not resolve — Scope is stages 3 and 4's answer", v.Default.Scope)
	}
}

// TestEveryDocumentedVariableTypeIsAccepted and TestUnknownVariableTypeIsRejected
// are the two directions of one predicate. Either alone is satisfied by a
// constant function.
func TestEveryDocumentedVariableTypeIsAccepted(t *testing.T) {
	want := map[string]value.Kind{
		"string":  value.KindString,
		"integer": value.KindInt,
		"float":   value.KindFloat,
		"boolean": value.KindBool,
		"list":    value.KindList,
		"map":     value.KindMap,
	}
	for spelling, kind := range want {
		t.Run(spelling, func(t *testing.T) {
			p, ds := decodeTree(t, map[string]string{
				ProjectFileName: projectWithNoResources +
					"variables:\n  v:\n    type: " + spelling + "\n",
			})
			requireNoErrors(t, ds)
			if got := findVariable(t, p, "v").Type; got != kind {
				t.Errorf("type %q decoded as %v, want %v", spelling, got, kind)
			}
		})
	}
}

func TestUnknownVariableTypeIsRejected(t *testing.T) {
	// "int" and "bool" are the near-misses a user actually types; "widget" is
	// the far miss. All must be refused, and the message must list what IS
	// available (PLAN.md §44: say what was expected and what to do).
	for _, spelling := range []string{"int", "bool", "str", "number", "widget", "Integer"} {
		t.Run(spelling, func(t *testing.T) {
			_, ds := decodeTree(t, map[string]string{
				ProjectFileName: projectWithNoResources +
					"variables:\n  v:\n    type: " + spelling + "\n",
			})
			requireErrorAbout(t, ds, spelling, "integer", "string", "boolean")
		})
	}
}

// TestUnknownTypeDiagnosticOffersEveryRealType. The message must list what IS
// available, or a user who typed `int` learns only that it is wrong.
//
// The list is driven from value.KindNames rather than a copy here, so a type
// added to value.ParseKind cannot start working while the diagnostic keeps
// advertising the old set. The frozen spelling of each type is pinned in
// pkg/value, beside ParseKind and Kind.String(), which own it in both
// directions; this package has no table of its own to freeze.
func TestUnknownTypeDiagnosticOffersEveryRealType(t *testing.T) {
	_, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources + "variables:\n  v:\n    type: widget\n",
	})
	d := requireErrorAbout(t, ds, "widget")
	text := d.Summary + " | " + d.Detail + " | " + d.Action
	for _, name := range value.KindNames() {
		if !strings.Contains(text, name) {
			t.Errorf("diagnostic does not offer %q: %s", name, text)
		}
	}
}

// TestBoundsAreAcceptedOnNumericTypes is the accepting direction of the
// min/max predicate.
func TestBoundsAreAcceptedOnNumericTypes(t *testing.T) {
	for _, spelling := range []string{"integer", "float"} {
		t.Run(spelling, func(t *testing.T) {
			p, ds := decodeTree(t, map[string]string{
				ProjectFileName: projectWithNoResources +
					"variables:\n  v:\n    type: " + spelling + "\n    min: 1\n    max: 10\n",
			})
			requireNoErrors(t, ds)
			v := findVariable(t, p, "v")
			if !v.HasMin || !v.HasMax {
				t.Errorf("HasMin=%v HasMax=%v on type %s", v.HasMin, v.HasMax, spelling)
			}
		})
	}
}

// TestBoundsAreRejectedOnNonNumericTypes is the refusing direction, over every
// type that is not orderable plus the untyped case.
func TestBoundsAreRejectedOnNonNumericTypes(t *testing.T) {
	for _, spelling := range []string{"string", "boolean", "list", "map"} {
		for _, bound := range []string{"min", "max"} {
			t.Run(spelling+"/"+bound, func(t *testing.T) {
				_, ds := decodeTree(t, map[string]string{
					ProjectFileName: projectWithNoResources +
						"variables:\n  v:\n    type: " + spelling + "\n    " + bound + ": 1\n",
				})
				requireErrorAbout(t, ds, bound, "v", spelling)
			})
		}
	}
	t.Run("untyped", func(t *testing.T) {
		_, ds := decodeTree(t, map[string]string{
			ProjectFileName: projectWithNoResources +
				"variables:\n  v:\n    min: 1\n",
		})
		requireErrorAbout(t, ds, "min", "v", "type")
	})
}

// TestBoundIsCheckedEvenWhenTypeIsWrittenAfterIt is the ordering fixture, and
// it is built so the natural order CONTRADICTS the required behaviour: `min`
// appears BEFORE `type`, so an implementation that checks each key as it walks
// the mapping sees no type yet and lets the bound through.
func TestBoundIsCheckedEvenWhenTypeIsWrittenAfterIt(t *testing.T) {
	_, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources +
			"variables:\n  v:\n    min: 1\n    max: 10\n    type: string\n",
	})
	// Two errors here — one per bound — so requireErrorAbout's exactly-one
	// rule does not apply.
	got := errorSummaries(ds)
	if len(got) != 2 {
		t.Fatalf("want 2 errors (one per bound), got %d: %v", len(got), got)
	}
	joined := strings.Join(got, " | ")
	for _, f := range []string{"min", "max"} {
		if !strings.Contains(joined, f) {
			t.Errorf("no diagnostic for %q: %s", f, joined)
		}
	}
}

// TestBoundIsCoercedToTheDeclaredType is the invariant Task 4 relies on: after
// stage 2, a bound's Kind ALWAYS equals its variable's declared Type, so a
// consumer switches on one Kind rather than a cross product — and value.AsFloat,
// which does not coerce an int64, is safe to use directly.
//
// The fixture is the one that would otherwise slip through: YAML tags `min: 1`
// as !!int even under `type: float`, so an implementation that stores the
// bound as decoded produces a KindInt bound on a float variable and AsFloat
// reads false.
func TestBoundIsCoercedToTheDeclaredType(t *testing.T) {
	cases := []struct {
		name, decl string
		wantKind   value.Kind
	}{
		{"int written as int", "type: integer\n    min: 1\n    max: 10\n", value.KindInt},
		{"float written as int", "type: float\n    min: 1\n    max: 10\n", value.KindFloat},
		{"float written as float", "type: float\n    min: 1.5\n    max: 9.5\n", value.KindFloat},
		{"int written as whole float", "type: integer\n    min: 1.0\n    max: 10.0\n", value.KindInt},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, ds := decodeTree(t, map[string]string{
				ProjectFileName: projectWithNoResources + "variables:\n  v:\n    " + tc.decl,
			})
			requireNoErrors(t, ds)
			v := findVariable(t, p, "v")
			if !v.HasMin || !v.HasMax {
				t.Fatalf("HasMin=%v HasMax=%v", v.HasMin, v.HasMax)
			}
			if v.Min.Kind != tc.wantKind || v.Max.Kind != tc.wantKind {
				t.Fatalf("Min.Kind=%v Max.Kind=%v, want %v (Type=%v)", v.Min.Kind, v.Max.Kind, tc.wantKind, v.Type)
			}
			if v.Min.Kind != v.Type {
				t.Errorf("bound Kind %v != declared Type %v; Task 4's Kind switch breaks", v.Min.Kind, v.Type)
			}
			// The accessor for the declared kind must actually read it. This
			// is the assertion that fails if coercion is skipped.
			switch tc.wantKind {
			case value.KindFloat:
				if _, ok := v.Min.AsFloat(); !ok {
					t.Error("AsFloat cannot read a float variable's min; the bound was not coerced")
				}
			case value.KindInt:
				if _, ok := v.Min.AsInt(); !ok {
					t.Error("AsInt cannot read an integer variable's min")
				}
			}
			// Origin must survive the coercion, or a range diagnostic cannot
			// name the line the bound is on.
			if v.Min.Origin.Line == 0 {
				t.Error("coercion dropped the bound's Origin")
			}
		})
	}
}

// TestBoundThatCannotBeCoercedIsRejected pins that a lossy conversion is a
// diagnostic, never a silent truncation. `type: integer` with `min: 1.5`
// quietly becoming 1 would accept values below the stated minimum with nothing
// printed.
func TestBoundThatCannotBeCoercedIsRejected(t *testing.T) {
	_, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources +
			"variables:\n  v:\n    type: integer\n    min: 1.5\n",
	})
	d := requireErrorAbout(t, ds, "min", "v", "integer")
	if strings.Contains(d.Summary+d.Detail, "must be a number") {
		t.Error("1.5 IS a number; the diagnostic should say it is not a whole one")
	}
}

// TestQuotedBoundIsRejected: "10" is a string, and string bounds compare by
// text, so `max: "9"` would silently reject 10.
func TestQuotedBoundIsRejected(t *testing.T) {
	_, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources +
			"variables:\n  v:\n    type: integer\n    max: \"9\"\n",
	})
	requireErrorAbout(t, ds, "max", "number")
}

// TestImpossibleBoundsAreRejected: no value can satisfy min > max, and every
// plan would fail with a range error naming the VALUE rather than the
// declaration that is actually wrong.
func TestImpossibleBoundsAreRejected(t *testing.T) {
	_, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources +
			"variables:\n  v:\n    type: integer\n    min: 100\n    max: 1\n",
	})
	requireErrorAbout(t, ds, "min", "max", "v")
}

// TestBoundsAtTheSameNumberAreAllowed is the boundary: min == max pins a
// variable to one value, which is unusual but not wrong. A `>=` comparison
// instead of `>` fails here.
func TestBoundsAtTheSameNumberAreAllowed(t *testing.T) {
	_, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources +
			"variables:\n  v:\n    type: integer\n    min: 5\n    max: 5\n",
	})
	requireNoErrors(t, ds)
}

// TestDuplicateVariableNameIsRejected. yaml.v3's Node decoding does not
// deduplicate mapping keys, so both entries arrive here and the last would
// silently win — discarding a type or a bound the user wrote.
func TestDuplicateVariableNameIsRejected(t *testing.T) {
	_, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources +
			"variables:\n  v:\n    type: integer\n  v:\n    type: string\n",
	})
	d := requireErrorAbout(t, ds, "v", "more than once")
	if !strings.Contains(d.Detail, "line") {
		t.Errorf("duplicate diagnostic must point at the other declaration: %s", d.Detail)
	}
}

// TestEmptyVariableDeclarationIsRejected. A declaration with neither a type
// nor a default says nothing about the variable.
//
// It is an ERROR, not a dropped declaration with a warning: dropping it
// discards something the user wrote and carries on as though they had not,
// which is the silent-loss shape with a log line in front of it.
func TestEmptyVariableDeclarationIsRejected(t *testing.T) {
	_, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources + "variables:\n  replicas: {}\n",
	})
	requireErrorAbout(t, ds, "replicas", "type", "default")
}

// TestAVariableWithEitherHalfIsAccepted is the other direction, and it is the
// half that catches an over-eager check. A type alone is a complete
// declaration (the variable is required, and stage 4 will say so if it is
// unset); a default alone is a complete declaration (untyped with a fallback,
// which PLAN.md §9 permits by calling schemas optional).
func TestAVariableWithEitherHalfIsAccepted(t *testing.T) {
	for name, body := range map[string]string{
		"type only":    "variables:\n  v:\n    type: string\n",
		"default only": "variables:\n  v:\n    default: 2\n",
		"both":         "variables:\n  v:\n    type: integer\n    default: 2\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, ds := decodeTree(t, map[string]string{
				ProjectFileName: projectWithNoResources + body,
			})
			requireNoErrors(t, ds)
		})
	}
}

// TestEmptyDeclarationSaysTheRootCauseOnce. Each of these is also an empty
// declaration, but each already has a diagnostic naming the real problem —
// so exactly one error, not two describing the same line.
//
// requireErrorAbout asserts the count, which is what makes this test do work:
// without the suppressions it sees two and fails.
func TestEmptyDeclarationSaysTheRootCauseOnce(t *testing.T) {
	cases := map[string][]string{
		// `type` present but unusable.
		"variables:\n  v:\n    type: widget\n": {"widget"},
		// `default` present but unusable.
		"variables:\n  v:\n    default: ${other}\n": {"interpolation"},
		// A bound with no type: decodeBound already says to add one.
		"variables:\n  v:\n    min: 1\n": {"min", "type"},
	}
	for body, fragments := range cases {
		t.Run(strings.ReplaceAll(body, "\n", " "), func(t *testing.T) {
			_, ds := decodeTree(t, map[string]string{
				ProjectFileName: projectWithNoResources + body,
			})
			requireErrorAbout(t, ds, fragments...)
		})
	}
}

// TestVariableBodyMustBeAMapping pins ruling 1, and the Action must tell the
// user where a bare value actually goes.
func TestVariableBodyMustBeAMapping(t *testing.T) {
	_, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources + "variables:\n  v: 2\n",
	})
	requireErrorAbout(t, ds, "v", VariablesFileName)
}

// TestVariablesFileValuesAreTaggedVariableAtEveryDepth pins per-leaf
// provenance (spec §5.1). decodeValue marks everything SourceExplicit because
// that is what an attribute in infra.yml is; a value from variables.yml is a
// variable, and setting only the top level leaves every list element claiming
// to be explicit configuration — which `explain` and minimal generation both
// read.
func TestVariablesFileValuesAreTaggedVariableAtEveryDepth(t *testing.T) {
	p, ds := decodeTree(t, map[string]string{
		ProjectFileName:   projectWithNoResources,
		VariablesFileName: "region: us-east-1\ntags:\n  - web\n  - api\nlimits:\n  cpu: 2\n",
	})
	requireNoErrors(t, ds)

	if got, ok := p.VariableValues["region"].AsString(); !ok || got != "us-east-1" {
		t.Fatalf("region = %#v", p.VariableValues["region"])
	}
	var check func(path string, v value.Value)
	check = func(path string, v value.Value) {
		if v.Source != value.SourceVariable {
			t.Errorf("%s: Source = %v, want SourceVariable", path, v.Source)
		}
		if v.Scope != value.ScopeUnset {
			t.Errorf("%s: Scope = %v; stage 2 declares, it does not resolve", path, v.Scope)
		}
		switch items := v.Raw.(type) {
		case []value.Value:
			for i, item := range items {
				check(path+"[]", item)
				_ = i
			}
		case map[string]value.Value:
			for k, item := range items {
				check(path+"."+k, item)
			}
		}
	}
	for name, v := range p.VariableValues {
		check(name, v)
	}
	if len(p.VariableValues) != 3 {
		t.Fatalf("VariableValues has %d entries, want 3: %v", len(p.VariableValues), p.VariableValues)
	}
}

// TestEmptyOptionalFilesAreNotErrors pins ruling 10 in both directions: an
// empty variables.yml is fine, an empty infra.yml is not.
func TestEmptyOptionalFilesAreNotErrors(t *testing.T) {
	_, ds := decodeTree(t, map[string]string{
		ProjectFileName:               projectWithNoResources,
		VariablesFileName:             "",
		"environments/production.yml": "",
	})
	requireNoErrors(t, ds)

	_, ds = decodeTree(t, map[string]string{ProjectFileName: ""})
	if !ds.HasErrors() {
		t.Fatal("an empty infra.yml must still be an error")
	}
}

// TestVariablesAreSortedByName. The insertion order CONTRADICTS the sorted
// order, and the assertion loops rather than sampling.
func TestVariablesAreSortedByName(t *testing.T) {
	p, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources +
			"variables:\n  zulu:\n    type: string\n  mike:\n    type: string\n" +
			"  alpha:\n    type: string\n  yankee:\n    type: string\n" +
			"  bravo:\n    type: string\n  november:\n    type: string\n",
	})
	requireNoErrors(t, ds)
	for i := 1; i < len(p.Variables); i++ {
		if p.Variables[i-1].Name >= p.Variables[i].Name {
			t.Fatalf("Variables not sorted: %q before %q", p.Variables[i-1].Name, p.Variables[i].Name)
		}
	}
	if len(p.Variables) != 6 {
		t.Fatalf("got %d variables, want 6", len(p.Variables))
	}
}
```

#### 3.2 — Write the failing environment tests

Create `internal/config/decode_environments_test.go`:

```go
package config

import (
	"strings"
	"testing"

	"infra/pkg/value"
)

func findEnvironment(t *testing.T, p *ProjectDecl, name string) EnvironmentDecl {
	t.Helper()
	for _, e := range p.Environments {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("no environment %q in %v", name, p.Environments)
	return EnvironmentDecl{}
}

func overrideOf(t *testing.T, e EnvironmentDecl, name string) OverrideDecl {
	t.Helper()
	for _, o := range e.Overrides {
		if o.Name == name {
			return o
		}
	}
	t.Fatalf("environment %q has no override %q: %v", e.Name, name, e.Overrides)
	return OverrideDecl{}
}

// TestDecodeEnvironmentsBlock decodes PLAN.md §7's example verbatim, which
// uses the FLAT override spelling.
func TestDecodeEnvironmentsBlock(t *testing.T) {
	p, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources + `
environments:
  default:
    replicas: 1
    instance_class: db.t4g.small
  production:
    extends: default
    replicas: 10
    instance_class: db.t4g.large
`,
	})
	requireNoErrors(t, ds)

	prod := findEnvironment(t, p, "production")
	if prod.Extends != "default" {
		t.Errorf("Extends = %q, want %q", prod.Extends, "default")
	}
	if prod.ExtendsOrigin.Line == 0 {
		t.Error("ExtendsOrigin has no line; stage 3's cycle diagnostic needs one")
	}
	// `extends` is a keyword, not an override. If it leaks into Overrides,
	// stage 4 resolves a variable literally called "extends".
	for _, o := range prod.Overrides {
		if o.Name == "extends" {
			t.Error("`extends` leaked into Overrides")
		}
	}
	if got, ok := overrideOf(t, prod, "replicas").Value.AsInt(); !ok || got != 10 {
		t.Errorf("replicas override = %#v, want Int(10)", overrideOf(t, prod, "replicas").Value)
	}

	def := findEnvironment(t, p, "default")
	if def.Extends != "" {
		t.Errorf("default.Extends = %q, want empty", def.Extends)
	}
}

// TestBothOverrideSpellingsAreAccepted: PLAN.md §6 nests overrides under
// `variables:` and §7 writes them as flat keys. Both are in the authoritative
// spec, so both must work.
func TestBothOverrideSpellingsAreAccepted(t *testing.T) {
	p, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources + `
environments:
  nested:
    variables:
      replicas: 1
  flat:
    replicas: 2
`,
	})
	requireNoErrors(t, ds)
	if got, ok := overrideOf(t, findEnvironment(t, p, "nested"), "replicas").Value.AsInt(); !ok || got != 1 {
		t.Errorf("nested spelling did not produce an override (got %v)", got)
	}
	if got, ok := overrideOf(t, findEnvironment(t, p, "flat"), "replicas").Value.AsInt(); !ok || got != 2 {
		t.Errorf("flat spelling did not produce an override (got %v)", got)
	}
}

// TestEnvironmentFileMergesWithTheBlock pins ruling 8's accepting direction:
// PLAN.md §7 puts `extends` in the block and §8 puts overrides in the file, so
// a user doing both is following the spec.
func TestEnvironmentFileMergesWithTheBlock(t *testing.T) {
	p, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources +
			"environments:\n  production:\n    extends: default\n  default:\n    replicas: 1\n",
		"environments/production.yml": "replicas: 10\ndomain: example.com\n",
	})
	requireNoErrors(t, ds)

	prod := findEnvironment(t, p, "production")
	if prod.Extends != "default" {
		t.Errorf("Extends = %q, want %q", prod.Extends, "default")
	}
	if got, ok := overrideOf(t, prod, "replicas").Value.AsInt(); !ok || got != 10 {
		t.Errorf("replicas from the file = %#v", overrideOf(t, prod, "replicas").Value)
	}
	// If this is empty, the duplicate diagnostic in addOverride cannot say
	// where the other assignment is.
	if !strings.HasSuffix(overrideOf(t, prod, "domain").Origin.File, "production.yml") {
		t.Errorf("override Origin should name the file it came from: %v", overrideOf(t, prod, "domain").Origin)
	}
	count := 0
	for _, e := range p.Environments {
		if e.Name == "production" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("production appears %d times; the block and the file must merge into one decl", count)
	}
}

// TestEnvironmentFileCanSetExtends: `extends` is reserved in the file form
// too, so a file-only environment can still inherit.
func TestEnvironmentFileCanSetExtends(t *testing.T) {
	p, ds := decodeTree(t, map[string]string{
		ProjectFileName:               projectWithNoResources,
		"environments/staging.yml":    "extends: default\nreplicas: 2\n",
		"environments/default.yml":    "replicas: 1\n",
	})
	requireNoErrors(t, ds)
	if got := findEnvironment(t, p, "staging").Extends; got != "default" {
		t.Errorf("Extends = %q, want %q", got, "default")
	}
}

// TestOverrideSetInTwoPlacesIsRejected pins ruling 8's refusing direction, in
// all three shapes a collision can take.
func TestOverrideSetInTwoPlacesIsRejected(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
	}{
		{
			name: "block and file",
			files: map[string]string{
				ProjectFileName:               projectWithNoResources + "environments:\n  prod:\n    replicas: 1\n",
				"environments/prod.yml":       "replicas: 10\n",
			},
		},
		{
			name: "flat and nested in one environment",
			files: map[string]string{
				ProjectFileName: projectWithNoResources +
					"environments:\n  prod:\n    replicas: 1\n    variables:\n      replicas: 10\n",
			},
		},
		{
			name: "twice in one file",
			files: map[string]string{
				ProjectFileName:         projectWithNoResources,
				"environments/prod.yml": "replicas: 1\nreplicas: 10\n",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ds := decodeTree(t, tc.files)
			requireErrorAbout(t, ds, "replicas", "prod", "more than once")
		})
	}
}

// TestExtendsSetInTwoPlacesIsRejected: a silently-dropped `extends` changes
// which values an environment inherits, with nothing printed.
func TestExtendsSetInTwoPlacesIsRejected(t *testing.T) {
	_, ds := decodeTree(t, map[string]string{
		ProjectFileName:         projectWithNoResources + "environments:\n  prod:\n    extends: a\n  a: {}\n  b: {}\n",
		"environments/prod.yml": "extends: b\n",
	})
	requireErrorAbout(t, ds, "extends", "prod")
}

// TestMalformedExtendsIsRejected covers every non-scalar shape. The alias case
// is the sharp one: an alias node's Value is the ANCHOR'S NAME, so `extends:
// *base` would silently become the string "base" — which might even name a
// real environment.
func TestMalformedExtendsIsRejected(t *testing.T) {
	cases := map[string]string{
		"sequence": "environments:\n  prod:\n    extends: [a, b]\n",
		"mapping":  "environments:\n  prod:\n    extends:\n      name: a\n",
		"null":     "environments:\n  prod:\n    extends:\n",
		"alias":    "base: &anchor other\nenvironments:\n  prod:\n    extends: *anchor\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			p, ds := decodeTree(t, map[string]string{
				ProjectFileName: projectWithNoResources + body,
			})
			if !ds.HasErrors() {
				t.Fatalf("malformed extends (%s) was accepted: %v", name, p.Environments)
			}
			for _, e := range p.Environments {
				if e.Name == "prod" && e.Extends != "" {
					t.Errorf("malformed extends (%s) still produced Extends = %q", name, e.Extends)
				}
			}
		})
	}
}

// TestValidExtendsIsAccepted is the other direction; without it every test
// above passes against an implementation that rejects all extends.
func TestValidExtendsIsAccepted(t *testing.T) {
	p, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources + "environments:\n  default: {}\n  prod:\n    extends: default\n",
	})
	requireNoErrors(t, ds)
	if got := findEnvironment(t, p, "prod").Extends; got != "default" {
		t.Errorf("Extends = %q, want %q", got, "default")
	}
}

// TestStageTwoDoesNotJudgeExtendsTargets pins ruling 6. Cycles and unknown
// parents need the whole set of environments, which is stage 3's job (spec §7).
// Two implementations of one concept is what leaked a plaintext secret in M2.
func TestStageTwoDoesNotJudgeExtendsTargets(t *testing.T) {
	for name, body := range map[string]string{
		"unknown parent": "environments:\n  prod:\n    extends: nosuch\n",
		"self":           "environments:\n  prod:\n    extends: prod\n",
		"two-cycle":      "environments:\n  a:\n    extends: b\n  b:\n    extends: a\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, ds := decodeTree(t, map[string]string{ProjectFileName: projectWithNoResources + body})
			requireNoErrors(t, ds)
		})
	}
}

// TestEnvironmentOverridesAreTaggedSourceEnvironment: provenance is per-leaf,
// so a list override must not leave its elements claiming SourceExplicit.
func TestEnvironmentOverridesAreTaggedSourceEnvironment(t *testing.T) {
	p, ds := decodeTree(t, map[string]string{
		ProjectFileName:         projectWithNoResources,
		"environments/prod.yml": "tags:\n  - web\n  - api\n",
	})
	requireNoErrors(t, ds)
	o := overrideOf(t, findEnvironment(t, p, "prod"), "tags")
	if o.Value.Source != value.SourceEnvironment {
		t.Errorf("Source = %v, want SourceEnvironment", o.Value.Source)
	}
	items, ok := o.Value.Raw.([]value.Value)
	if !ok || len(items) != 2 {
		t.Fatalf("Raw = %#v", o.Value.Raw)
	}
	for i, item := range items {
		if item.Source != value.SourceEnvironment {
			t.Errorf("tags[%d].Source = %v, want SourceEnvironment", i, item.Source)
		}
	}
}

// TestEnvironmentsAndOverridesAreSorted. Both insertion orders CONTRADICT the
// sorted order, and both assertions loop over every adjacent pair rather than
// sampling — six names, because a two-name check passes far too often against
// broken code.
//
// Sorting here is what spares every consumer from re-sorting. M3 measured
// eleven redundant sorts in this tree whose only job was undoing map
// iteration; this test is what makes the twelfth unnecessary.
func TestEnvironmentsAndOverridesAreSorted(t *testing.T) {
	p, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources + `
environments:
  zulu:
    zeta: 1
    mike: 2
    alpha: 3
    yankee: 4
    bravo: 5
    november: 6
  mike: {}
  alpha: {}
  yankee: {}
  bravo: {}
  november: {}
`,
	})
	requireNoErrors(t, ds)
	if len(p.Environments) != 6 {
		t.Fatalf("got %d environments, want 6", len(p.Environments))
	}
	for i := 1; i < len(p.Environments); i++ {
		if p.Environments[i-1].Name >= p.Environments[i].Name {
			t.Fatalf("Environments not sorted: %q before %q", p.Environments[i-1].Name, p.Environments[i].Name)
		}
	}
	zulu := findEnvironment(t, p, "zulu")
	if len(zulu.Overrides) != 6 {
		t.Fatalf("got %d overrides, want 6", len(zulu.Overrides))
	}
	for i := 1; i < len(zulu.Overrides); i++ {
		if zulu.Overrides[i-1].Name >= zulu.Overrides[i].Name {
			t.Fatalf("Overrides not sorted: %q before %q", zulu.Overrides[i-1].Name, zulu.Overrides[i].Name)
		}
	}
}

// TestOverridesFromTheBlockAndTheFileAreSortedTogether. The merge appends the
// file's overrides after the block's, so sorting only within each source would
// leave the combined slice unsorted. The names are chosen so that the file's
// override sorts BEFORE the block's.
func TestOverridesFromTheBlockAndTheFileAreSortedTogether(t *testing.T) {
	p, ds := decodeTree(t, map[string]string{
		ProjectFileName:         projectWithNoResources + "environments:\n  prod:\n    zulu: 1\n",
		"environments/prod.yml": "alpha: 2\n",
	})
	requireNoErrors(t, ds)
	prod := findEnvironment(t, p, "prod")
	if len(prod.Overrides) != 2 || prod.Overrides[0].Name != "alpha" {
		t.Fatalf("overrides not sorted across sources: %v", prod.Overrides)
	}
}

// TestProjectOriginComesFromTheProjectFile.
//
// Decode assigns out.Origin inside its per-file loop. With three files that
// makes ProjectDecl.Origin the LAST file's — so every diagnostic that points
// at "the project" would point at environments/zulu.yml. The fixture uses
// "zulu" so it sorts last and the bug is reachable.
func TestProjectOriginComesFromTheProjectFile(t *testing.T) {
	p, ds := decodeTree(t, map[string]string{
		ProjectFileName:         projectWithNoResources,
		VariablesFileName:       "region: us-east-1\n",
		"environments/zulu.yml": "replicas: 1\n",
	})
	requireNoErrors(t, ds)
	if !strings.HasSuffix(p.Origin.File, ProjectFileName) {
		t.Errorf("ProjectDecl.Origin.File = %q, want %s", p.Origin.File, ProjectFileName)
	}
	if p.Project != "demo" {
		t.Errorf("Project = %q, want %q", p.Project, "demo")
	}
}

// TestVariablesFileWithAConfigurationBlockWarns: a `resources:` key in
// variables.yml is almost certainly the wrong file. It is a WARNING, not an
// error, because a variable may legitimately be called "project" and refusing
// it would break a valid configuration to catch a mistake.
func TestVariablesFileWithAConfigurationBlockWarns(t *testing.T) {
	_, ds := decodeTree(t, map[string]string{
		ProjectFileName:   projectWithNoResources,
		VariablesFileName: "resources:\n  db: {}\n",
	})
	if ds.HasErrors() {
		t.Fatalf("must be a warning, not an error: %v", errorSummaries(ds))
	}
	if len(ds) != 1 || !strings.Contains(ds[0].Summary, "resources") {
		t.Fatalf("want one warning about `resources`, got %v", ds)
	}
}
```

#### 3.3 — Run and confirm the expected failure

```bash
export PATH="$HOME/.local/share/mise/shims:$PATH"
go test -count=1 ./internal/config/
```

Expected: **compilation failure** — `undefined: VariableDecl`, `undefined:
EnvironmentDecl`, `undefined: OverrideDecl`, `p.Variables undefined`,
`p.Environments undefined`, `p.VariableValues undefined`. If instead you see
`undefined: value.ParseKind` or `undefined: value.KindNames`, Task 1 has not
landed — stop and rebase onto it rather than adding a spelling table here.

#### 3.4 — Add the declarations

Append to `internal/config/declarations.go`:

```go
// VariableDecl is one declared variable, from infra.yml's `variables:` block
// (PLAN.md §9).
//
// It is a DECLARATION — a type and its bounds — not a value. Values come from
// variables.yml, from environment overrides and from --var; stage 4 resolves
// the two against each other. Keeping them apart is what lets `infra validate`
// report a type error in a declaration with no environment selected.
type VariableDecl struct {
	Name string
	// Type is the declared kind, or KindInvalid when the declaration gave no
	// type. Untyped is legal: PLAN.md §9 calls variable schemas optional.
	Type value.Kind
	// Default is the declared default; HasDefault is the authority on whether
	// there is one. A Value's zero is KindInvalid, which is also what a FAILED
	// decode produces, so the flag rather than the Kind is what stage 4 checks.
	// Scope is deliberately left unset on Default. A default resolves to
	// SourceDefault + ScopeBaseConfig, and STAGE 4 stamps that when the
	// default wins — a declaration is not a resolution, and only one place may
	// decide which precedence level won.
	Default    value.Value
	HasDefault bool
	// Min and Max are inclusive bounds, valid only when HasMin/HasMax.
	//
	// value.Value rather than float64, for two reasons. An int64 does not fit
	// a float64 exactly, so a bound above 2^53 would validate wrongly; and a
	// Value carries its Origin, so a range diagnostic can point at the line
	// where the minimum was DECLARED rather than only at the value that failed
	// it. The price is that a consumer comparing against these must be
	// kind-aware: int64 against int64 exactly, float64 only when a side
	// genuinely is one. value.AsFloat deliberately does not coerce.
	Min, Max       value.Value
	HasMin, HasMax bool
	Origin         value.Origin
}

// OverrideDecl is one key an environment sets.
type OverrideDecl struct {
	Name   string
	Value  value.Value
	Origin value.Origin
}

// EnvironmentDecl is one declared environment (PLAN.md §6, §7).
//
// One environment may be declared in two places — infra.yml's `environments:`
// block and environments/<name>.yml — and they MERGE into a single decl:
// PLAN.md §7 puts `extends` in the block and §8 puts overrides in the file, so
// a user doing both is following the spec. A key set in both places is an
// error, because one of the two values would silently disappear.
type EnvironmentDecl struct {
	Name string
	// Extends names the parent environment, or "" when it extends nothing.
	// Stage 2 checks only that it is a usable scalar: an unknown parent and a
	// cycle both need the whole set of environments, which is stage 3's job
	// (spec §7). Two implementations of one concept is the defect that leaked
	// a plaintext secret in M2.
	Extends       string
	ExtendsOrigin value.Origin
	// Overrides is sorted by Name, so stage 3's scope stack and every
	// diagnostic built from it are the same on every run — invariant 6.
	//
	// A slice rather than a map deliberately. A map would lose each override's
	// Origin as a first-class field and would force every consumer to re-sort:
	// M3 measured ELEVEN redundant sorts in this tree whose only job was
	// undoing map iteration. Sorting once, here, is what stops the twelfth.
	Overrides []OverrideDecl
	Origin    value.Origin
}
```

Then extend `ProjectDecl`:

```go
// ProjectDecl is the decoded, still-unresolved configuration.
type ProjectDecl struct {
	Project      string
	Resources    []*ResourceDecl   // sorted by Name
	Variables    []VariableDecl    // sorted by Name
	Environments []EnvironmentDecl // sorted by Name
	// VariableValues is variables.yml's contents: a flat mapping of name to
	// value (PLAN.md §8). Never nil. These are VALUES, not declarations —
	// stage 4 checks them against Variables. variables.yml may NOT carry a
	// `variables:` schema block; declarations live in infra.yml and nowhere
	// else, so that "where is this variable declared" has one answer.
	//
	// Distinct from compiler.Options.FileVars, which is what --var-file
	// supplied. That is a CLI input at a different precedence level and does
	// not belong here; the two must not be merged.
	//
	// Iterate it in sorted key order whenever order is observable; Go's map
	// order is randomised.
	VariableValues map[string]value.Value
	Origin         value.Origin
}
```

#### 3.5 — Extend `Decode` to dispatch on `FileKind`

In `internal/config/decode.go`, locate the loop:

```bash
grep -n "out.Origin = originOf" internal/config/decode.go
grep -n "configuration file is empty" internal/config/decode.go
grep -n "sort.Slice(out.Resources" internal/config/decode.go
```

Replace `Decode`'s body with:

```go
func Decode(files []File) (*ProjectDecl, diag.Diagnostics) {
	var ds diag.Diagnostics
	out := &ProjectDecl{VariableValues: map[string]value.Value{}}

	// Uniqueness is tracked across the whole decode rather than per file: spec
	// §5.2 requires logical names be unique within a module, and M4 adds more
	// files to the same root module.
	//
	// Variables, environments and resources get SEPARATE sets. They are
	// separate namespaces: `${db}` is a variable reference and `${db.host}` a
	// resource reference, and stage 6 already tells them apart.
	seenResources := map[string]value.Origin{}
	seenVariables := map[string]value.Origin{}
	// seenEnvironments maps an environment name to its index in
	// out.Environments, because the block and the file MERGE into one decl.
	seenEnvironments := map[string]int{}

	for _, f := range files {
		doc := documentRoot(f.Root)
		if doc == nil {
			// Only the project file must have content. An empty variables.yml
			// or an empty environment file declares nothing, which is a
			// perfectly ordinary state for a file a user has just created.
			if f.Kind != FileProject {
				continue
			}
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "configuration file is empty",
				Origin:   value.Origin{File: f.Path},
				Action:   "Add a `project` name and a `resources` block.",
			})
			continue
		}
		if doc.Kind != yaml.MappingNode {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  f.Kind.String() + " must be a mapping",
				Detail:   topLevelShapeDetail(f),
				Origin:   originOf(f.Path, doc),
			})
			continue
		}

		switch f.Kind {
		case FileProject:
			// Assigned ONLY here. It was previously assigned for every file in
			// the loop, which with M4's extra files would make
			// ProjectDecl.Origin the last environment file's — so every
			// diagnostic that points at "the project" would point at
			// environments/zulu.yml.
			out.Origin = originOf(f.Path, doc)
			decodeDocument(f.Path, doc, out, &ds, seenResources, seenVariables, seenEnvironments)
		case FileVariables:
			decodeVariableValues(f.Path, doc, out, &ds)
		case FileEnvironment:
			decodeEnvironmentBody(f.Path, f.Environment, doc, out, &ds, seenEnvironments)
		}
	}

	sort.Slice(out.Resources, func(i, j int) bool { return out.Resources[i].Name < out.Resources[j].Name })
	sort.Slice(out.Variables, func(i, j int) bool { return out.Variables[i].Name < out.Variables[j].Name })
	sort.Slice(out.Environments, func(i, j int) bool { return out.Environments[i].Name < out.Environments[j].Name })
	// Sorted ONCE, here, so no consumer has to. Overrides are built by
	// appending in file order, which is the filesystem's order across the
	// block and the file, not the user's.
	for i := range out.Environments {
		o := out.Environments[i].Overrides
		sort.Slice(o, func(a, b int) bool { return o[a].Name < o[b].Name })
	}
	return out, ds
}

// topLevelShapeDetail explains what the top level of each file kind holds.
// A generic "must be a mapping" is true of all three and actionable for none.
func topLevelShapeDetail(f File) string {
	switch f.Kind {
	case FileVariables:
		return "The top level of " + VariablesFileName + " is a flat mapping of variable name to value, for example `region: us-east-1`."
	case FileEnvironment:
		return "The top level of an environment file is a mapping of variable name to value, for example `replicas: 10`."
	default:
		return "The top level of " + ProjectFileName + " must be a set of keys such as `project` and `resources`."
	}
}
```

Then extend `decodeDocument`'s signature and its switch:

```go
func decodeDocument(path string, doc *yaml.Node, out *ProjectDecl, ds *diag.Diagnostics,
	seenResources, seenVariables map[string]value.Origin, seenEnvironments map[string]int) {
	for i := 0; i+1 < len(doc.Content); i += 2 {
		key, val := doc.Content[i], doc.Content[i+1]
		switch key.Value {
		case "project":
			if text, ok := requireScalar(path, "`project`", val, ds); ok {
				out.Project = text
			}
		case "resources":
			decodeResources(path, val, out, ds, seenResources)
		case "variables":
			decodeVariables(path, val, out, ds, seenVariables)
		case "environments":
			decodeEnvironments(path, val, out, ds, seenEnvironments)
		default:
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityWarning,
				Summary:  "unrecognised top-level key " + strconv.Quote(key.Value),
				Detail:   ProjectFileName + " understands `project`, `resources`, `variables` and `environments`.",
				Origin:   originOf(path, key),
			})
		}
	}
}
```

Update `decodeResources`'s parameter name from `seen` to `seenResources` at the
one call site if you prefer; the map's contents are unchanged.

#### 3.6 — Add variable decoding

Append to `internal/config/decode.go`:

```go
// variableTypeList renders the accepted type spellings for a diagnostic.
//
// The spellings come from pkg/value, which owns them in both directions:
// value.ParseKind is the inverse of value.Kind.String(), pinned as such by
// TestKindStringAndParseKindAreInverses. This package deliberately keeps NO
// table of its own — a second copy would drift from the one the parser
// actually consults.
//
// value.KindNames is already sorted, so the message does not reorder itself
// between identical runs.
func variableTypeList() string {
	return strings.Join(value.KindNames(), ", ")
}

func decodeVariables(path string, node *yaml.Node, out *ProjectDecl, ds *diag.Diagnostics, seen map[string]value.Origin) {
	if node.Kind != yaml.MappingNode {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "`variables` must be a mapping of variable name to declaration",
			Detail:   "Each variable is a key with `type`, `default`, `min` and `max` beneath it (PLAN.md §9).",
			Origin:   originOf(path, node),
		})
		return
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		nameNode, body := node.Content[i], node.Content[i+1]
		origin := originOf(path, nameNode)

		// yaml.v3 does not deduplicate mapping keys when decoding into a Node,
		// so both entries arrive here and the last would silently win —
		// discarding a type or a bound the user wrote.
		if first, dup := seen[nameNode.Value]; dup {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "variable " + strconv.Quote(nameNode.Value) + " is declared more than once",
				Detail:   "The last declaration would silently win, discarding a type or a bound. " + strconv.Quote(nameNode.Value) + " is also declared at " + describeOrigin(first) + ".",
				Action:   "Remove one of the two declarations, or merge them.",
				Origin:   origin,
			})
			continue
		}
		seen[nameNode.Value] = origin
		out.Variables = append(out.Variables, decodeVariable(path, nameNode.Value, body, origin, ds))
	}
}

func decodeVariable(path, name string, body *yaml.Node, origin value.Origin, ds *diag.Diagnostics) VariableDecl {
	v := VariableDecl{Name: name, Origin: origin}

	if body.Kind != yaml.MappingNode {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "variable " + strconv.Quote(name) + " must be a mapping",
			Detail:   "A `variables:` entry is a schema — `type`, `default`, `min`, `max` — not a value. A bare value here would be ambiguous with a declaration whose type is `map`.",
			Action:   "Write `default:` beneath " + strconv.Quote(name) + ", or put the value in " + VariablesFileName + " instead.",
			Origin:   originOf(path, body),
		})
		return v
	}

	// minNode and maxNode are held back and checked AFTER the whole mapping is
	// walked. `type:` may appear textually after `min:`, and an implementation
	// that checks each key as it goes would accept `min: 1` on a string
	// whenever the file happened to be written in that order.
	var minNode, maxNode *yaml.Node
	// typeReported and defaultReported suppress the "must specify at least a
	// type or a default" check below when the key WAS present but unusable.
	// Telling a user their declaration is empty, one line under a diagnostic
	// explaining why their `type` was rejected, describes a symptom.
	typeReported := false
	defaultReported := false
	seenKeys := map[string]value.Origin{}

	for i := 0; i+1 < len(body.Content); i += 2 {
		key, val := body.Content[i], body.Content[i+1]
		keyOrigin := originOf(path, key)
		if first, dup := seenKeys[key.Value]; dup {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  strconv.Quote(key.Value) + " is set more than once on variable " + strconv.Quote(name),
				Detail:   "The last assignment would silently win. " + strconv.Quote(key.Value) + " is also set at " + describeOrigin(first) + ".",
				Action:   "Remove one of the two assignments.",
				Origin:   keyOrigin,
			})
			continue
		}
		seenKeys[key.Value] = keyOrigin

		switch key.Value {
		case "type":
			text, ok := requireScalar(path, "variable "+strconv.Quote(name)+"'s `type`", val, ds)
			if !ok {
				typeReported = true
				break
			}
			kind, known := value.ParseKind(text)
			if !known {
				ds.Add(diag.Diagnostic{
					Severity: diag.SeverityError,
					Summary:  "unknown variable type " + strconv.Quote(text),
					Detail:   "Variable " + strconv.Quote(name) + " declares a type the engine does not have. Supported types are " + variableTypeList() + ".",
					Action:   "Change `type` to one of " + variableTypeList() + ", or remove it to leave " + strconv.Quote(name) + " untyped.",
					Origin:   originOf(path, val),
				})
				typeReported = true
				break
			}
			v.Type = kind
		case "default":
			dv, hasExpr := decodeValue(path, "variable "+strconv.Quote(name)+"'s `default`", val, ds)
			if hasExpr {
				defaultReported = true
				ds.Add(diag.Diagnostic{
					Severity: diag.SeverityError,
					Summary:  "variable " + strconv.Quote(name) + "'s default contains an interpolation",
					Detail:   "Defaults are resolved before any expression scope exists, so `${...}` here has nothing to refer to. Accepting it would store the literal text " + strconv.Quote(val.Value) + " as the default.",
					Action:   "Write a literal value, or set " + strconv.Quote(name) + " per environment instead.",
					Origin:   originOf(path, val),
				})
				break
			}
			v.Default, v.HasDefault = dv, true
		case "min":
			minNode = val
		case "max":
			maxNode = val
		default:
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "unknown key " + strconv.Quote(key.Value) + " in variable " + strconv.Quote(name),
				Detail:   "A variable declaration understands `type`, `default`, `min` and `max`.",
				Action:   "Remove " + strconv.Quote(key.Value) + ".",
				Origin:   keyOrigin,
			})
		}
	}

	v.Min, v.HasMin = decodeBound(path, name, "min", minNode, v.Type, typeReported, ds)
	v.Max, v.HasMax = decodeBound(path, name, "max", maxNode, v.Type, typeReported, ds)

	// A declaration with neither a type nor a default carries no information:
	// it names a variable and says nothing about it, so stage 4 has nothing to
	// validate against and nothing to fall back on.
	//
	// This is an ERROR rather than a dropped declaration with a warning.
	// Dropping it discards something the user wrote and continues as though
	// they had not — the silent-loss shape this engine refuses everywhere
	// else, with a log line in front of it. A user who writes `replicas:` under
	// `variables:` meant something by it; the only safe response is to say the
	// declaration is incomplete, at the line it is on, and stop.
	//
	// Three suppressions, all of the same kind: say the root cause once.
	// typeReported and defaultReported mean the key WAS present but unusable
	// and has already been reported. A present min or max means decodeBound
	// just said "add `type: integer` or `type: float`" — the same advice this
	// would give, about the same declaration, one line apart.
	incomplete := v.Type == value.KindInvalid && !v.HasDefault
	alreadyExplained := typeReported || defaultReported || minNode != nil || maxNode != nil
	if incomplete && !alreadyExplained {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "variable " + strconv.Quote(name) + " must specify at least a `type` or a `default`",
			Detail:   "The declaration says nothing about " + strconv.Quote(name) + ", so nothing can be validated against it and there is no value to fall back on when it is not set.",
			Action:   "Add `type: string` (or another type), or `default:` with a value, or remove the declaration and set " + strconv.Quote(name) + " in " + VariablesFileName + ".",
			Origin:   origin,
		})
	}

	// min > max can never be satisfied. Reported here, where the declaration
	// is in hand, because at stage 4 the only thing to point at is a VALUE
	// that failed a range check — naming the symptom instead of the cause.
	// min == max is legal: it pins a variable to one value.
	if v.HasMin && v.HasMax && boundGreater(v.Min, v.Max) {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "variable " + strconv.Quote(name) + " has a `min` greater than its `max`",
			Detail:   "No value can satisfy this declaration, so every plan would fail with a range error naming the value rather than the declaration.",
			Action:   "Swap `min` and `max`, or remove one of them.",
			Origin:   v.Min.Origin,
		})
	}
	return v
}

// boundGreater reports whether bound a is greater than bound b.
//
// It switches on Kind rather than coercing both to float64, because an int64
// does not fit a float64: above 2^53 the conversion rounds and two bounds that
// differ would compare equal. That is the same reason Min and Max are
// value.Value rather than float64, and it is the shape Task 4's range check
// takes too.
//
// There is no mixed case to handle. decodeBound has already coerced both
// bounds to the variable's declared type, so if both are present they have the
// same Kind — which is exactly what that coercion buys.
//
// A non-numeric value is never greater: decodeBound refused it and said why,
// and a second diagnostic about the same declaration would describe the
// symptom.
func boundGreater(a, b value.Value) bool {
	if ai, ok := a.AsInt(); ok {
		bi, ok2 := b.AsInt()
		return ok2 && ai > bi
	}
	if af, ok := a.AsFloat(); ok {
		bf, ok2 := b.AsFloat()
		return ok2 && af > bf
	}
	return false
}

// decodeBound decodes `min` or `max`, refusing it on a type that has no
// ordering.
//
// Numeric-only is PLAN.md §9's rule, and it matters more than it looks: a
// `min` on a string would have to mean either "shortest" or "lowest in some
// collation", and stage 4 would have to pick one silently.
//
// typeReported suppresses this when `type` itself was already rejected —
// "`min` is not valid on an untyped variable" names a symptom of a problem
// already reported one line up.
func decodeBound(path, name, which string, node *yaml.Node, kind value.Kind, typeReported bool, ds *diag.Diagnostics) (value.Value, bool) {
	if node == nil {
		return value.Value{}, false
	}
	if kind != value.KindInt && kind != value.KindFloat {
		if typeReported {
			return value.Value{}, false
		}
		detail := "Variable " + strconv.Quote(name) + " has type " + kind.String() + ", which has no ordering, so `" + which + "` could not be checked against anything."
		action := "Remove `" + which + "`, or declare `type: integer` or `type: float`."
		if kind == value.KindInvalid {
			detail = "Variable " + strconv.Quote(name) + " declares no `type`, so `" + which + "` has no ordering to be checked against."
			action = "Add `type: integer` or `type: float`, or remove `" + which + "`."
		}
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "`" + which + "` is not valid on variable " + strconv.Quote(name),
			Detail:   detail,
			Action:   action,
			Origin:   originOf(path, node),
		})
		return value.Value{}, false
	}

	// Both numeric kinds are accepted here whatever the declared type. YAML
	// tags `min: 1` as !!int even under `type: float`, so refusing KindInt
	// would reject the obvious spelling of a float bound.
	bv, hasExpr := decodeValue(path, "variable "+strconv.Quote(name)+"'s `"+which+"`", node, ds)
	if hasExpr || !bv.Known || (bv.Kind != value.KindInt && bv.Kind != value.KindFloat) {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "`" + which + "` on variable " + strconv.Quote(name) + " must be a number",
			Detail:   "Got " + strconv.Quote(node.Value) + ". A quoted number is a string, and a string bound compares by text: \"10\" sorts before \"9\".",
			Action:   "Write `" + which + ": " + node.Value + "` unquoted.",
			Origin:   originOf(path, node),
		})
		return value.Value{}, false
	}
	return coerceBound(path, name, which, node, bv, kind, ds)
}

// coerceBound converts a decoded bound to the variable's DECLARED kind.
//
// This is why stage 2 is the right place: it is the only stage that knows both
// the declared type and the line number. After it, a bound's Kind always equals
// its variable's Type, so every consumer switches on one Kind instead of
// handling a cross product — and value.AsFloat, which deliberately does not
// coerce an int64, is safe to use directly.
//
// A conversion that would lose information is a DIAGNOSTIC, never a silent
// truncation. `type: integer` with `min: 1.5` must not quietly become 1: the
// user would see values below their stated minimum accepted, with nothing
// printed, which is the silent-loss shape this engine refuses everywhere else.
func coerceBound(path, name, which string, node *yaml.Node, bv value.Value, kind value.Kind, ds *diag.Diagnostics) (value.Value, bool) {
	origin := originOf(path, node)

	switch {
	case kind == value.KindInt && bv.Kind == value.KindFloat:
		f, _ := bv.AsFloat()
		n := int64(f)
		// Exactness both ways: float64(n) == f rejects a fractional part, and
		// it also rejects a float too large to survive the round trip.
		if float64(n) != f {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "`" + which + "` on variable " + strconv.Quote(name) + " is not a whole number",
				Detail:   "Variable " + strconv.Quote(name) + " has type integer, so " + strconv.Quote(node.Value) + " cannot be its " + which + ". Rounding it would silently change the bound the user asked for.",
				Action:   "Write a whole number, or declare `type: float`.",
				Origin:   origin,
			})
			return value.Value{}, false
		}
		return value.Int(n, value.SourceExplicit).WithOrigin(origin), true

	case kind == value.KindFloat && bv.Kind == value.KindInt:
		n, _ := bv.AsInt()
		f := float64(n)
		// An int64 above 2^53 does not survive this. Vanishingly rare for a
		// bound, and a diagnostic beats a bound that silently is not the one
		// that was written.
		if int64(f) != n {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "`" + which + "` on variable " + strconv.Quote(name) + " is too large to represent as a float",
				Detail:   "Variable " + strconv.Quote(name) + " has type float, and " + strconv.Quote(node.Value) + " cannot be converted without changing its value.",
				Action:   "Use a smaller bound, or declare `type: integer`.",
				Origin:   origin,
			})
			return value.Value{}, false
		}
		return value.Float(f, value.SourceExplicit).WithOrigin(origin), true
	}

	// Already the declared kind.
	return bv, true
}

// decodeVariableValues decodes variables.yml: a flat mapping of variable name
// to value (PLAN.md §8).
func decodeVariableValues(path string, doc *yaml.Node, out *ProjectDecl, ds *diag.Diagnostics) {
	for i := 0; i+1 < len(doc.Content); i += 2 {
		key, val := doc.Content[i], doc.Content[i+1]
		origin := originOf(path, key)

		// A key that names one of infra.yml's blocks is almost certainly the
		// wrong file. A WARNING rather than an error: a variable may
		// legitimately be called "project", and refusing it would break a
		// valid configuration in order to catch a mistake.
		switch key.Value {
		case "project", "resources", "variables", "environments":
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityWarning,
				Summary:  strconv.Quote(key.Value) + " in " + VariablesFileName + " is a variable, not a configuration block",
				Detail: VariablesFileName + " is a flat mapping of variable name to value (PLAN.md §8). " +
					"A `" + key.Value + "` block belongs in " + ProjectFileName + " — in particular, variable DECLARATIONS " +
					"(`type`, `default`, `min`, `max`) live only under " + ProjectFileName + "'s `variables:` key, so that " +
					"where a variable is declared has one answer.",
				Action: "Move it to " + ProjectFileName + ", or ignore this if you really do have a variable called " + strconv.Quote(key.Value) + ".",
				Origin: origin,
			})
		}

		if first, dup := out.VariableValues[key.Value]; dup {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "variable " + strconv.Quote(key.Value) + " is set more than once in " + VariablesFileName,
				Detail:   "The last assignment would silently win. It is also set at " + describeOrigin(first.Origin) + ".",
				Action:   "Remove one of the two assignments.",
				Origin:   origin,
			})
			continue
		}

		v, hasExpr := decodeValue(path, "variable "+strconv.Quote(key.Value), val, ds)
		if hasExpr {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "variable " + strconv.Quote(key.Value) + " contains an interpolation",
				Detail:   VariablesFileName + " is resolved before any expression scope exists, so `${...}` here has nothing to refer to.",
				Action:   "Write a literal value.",
				Origin:   originOf(path, val),
			})
			continue
		}
		out.VariableValues[key.Value] = retagSource(v, value.SourceVariable).WithOrigin(origin)
	}
}

// retagSource rewrites a decoded value's provenance recursively.
//
// decodeValue marks everything SourceExplicit, because that is what an
// attribute in infra.yml is. A value from variables.yml is a variable and one
// from an environment file is an environment override, and provenance is
// PER-LEAF (spec §5.1) — setting only the top level would leave every element
// of a list variable claiming to be explicit configuration, which `explain`
// and minimal generation both read straight off the leaves.
//
// Scope is deliberately NOT set: a declaration is not a resolution, and which
// precedence level won is stages 3 and 4's answer. Two places deciding
// provenance is how a plan starts disagreeing with itself.
func retagSource(v value.Value, src value.ValueSource) value.Value {
	switch v.Kind {
	case value.KindList:
		items, ok := v.Raw.([]value.Value)
		if !ok {
			// Malformed: a diagnostic was already emitted where it was
			// decoded. Retag the shell and stop rather than panicking.
			return v.WithSource(src)
		}
		retagged := make([]value.Value, len(items))
		for i, item := range items {
			retagged[i] = retagSource(item, src)
		}
		v.Raw = retagged
	case value.KindMap:
		m, ok := v.Raw.(map[string]value.Value)
		if !ok {
			return v.WithSource(src)
		}
		retagged := make(map[string]value.Value, len(m))
		for k, item := range m {
			retagged[k] = retagSource(item, src)
		}
		v.Raw = retagged
	}
	return v.WithSource(src)
}
```

#### 3.7 — Add environment decoding

Append to `internal/config/decode.go`:

```go
func decodeEnvironments(path string, node *yaml.Node, out *ProjectDecl, ds *diag.Diagnostics, seen map[string]int) {
	if node.Kind != yaml.MappingNode {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "`environments` must be a mapping of environment name to configuration",
			Detail:   "Each environment is a key, optionally with `extends` and its overrides beneath it (PLAN.md §7).",
			Origin:   originOf(path, node),
		})
		return
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		nameNode, body := node.Content[i], node.Content[i+1]
		decodeEnvironmentBody(path, nameNode.Value, body, out, ds, seen)
	}
}

// environmentFor returns the decl for name, creating it on first sight.
//
// One environment may be declared in infra.yml's `environments:` block AND in
// environments/<name>.yml, and the two MERGE: PLAN.md §7 puts `extends` in the
// block, §8 puts overrides in the file, so a user doing both is following the
// spec. Load's fixed file order (project, variables, environments by name) is
// what makes "which one was first" deterministic, which the duplicate
// diagnostics below depend on.
func environmentFor(out *ProjectDecl, name string, origin value.Origin, seen map[string]int) *EnvironmentDecl {
	if idx, ok := seen[name]; ok {
		return &out.Environments[idx]
	}
	out.Environments = append(out.Environments, EnvironmentDecl{Name: name, Origin: origin})
	seen[name] = len(out.Environments) - 1
	return &out.Environments[len(out.Environments)-1]
}

// decodeEnvironmentBody decodes one environment's mapping, from either the
// `environments:` block or a whole environments/<name>.yml document.
//
// Two override spellings are accepted because PLAN.md uses both: §6 nests them
// under `variables:` and §7 writes them as flat keys. `extends` is reserved in
// both, and in the file form.
func decodeEnvironmentBody(path, name string, body *yaml.Node, out *ProjectDecl, ds *diag.Diagnostics, seen map[string]int) {
	origin := originOf(path, body)
	if body.Kind != yaml.MappingNode {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "environment " + strconv.Quote(name) + " must be a mapping",
			Detail:   "An environment holds an optional `extends` and its overrides.",
			Action:   "Write `" + name + ": {}` if it has no settings of its own.",
			Origin:   origin,
		})
		return
	}
	env := environmentFor(out, name, origin, seen)

	for i := 0; i+1 < len(body.Content); i += 2 {
		key, val := body.Content[i], body.Content[i+1]
		keyOrigin := originOf(path, key)

		switch key.Value {
		case "extends":
			// requireScalar reports the alias, non-scalar and null cases, each
			// of which would otherwise silently produce a wrong parent — an
			// alias node's Value is the ANCHOR'S NAME, so `extends: *base`
			// would become the string "base", which might even name a real
			// environment.
			text, ok := requireScalar(path, "environment "+strconv.Quote(name)+"'s `extends`", val, ds)
			if !ok {
				continue
			}
			if env.Extends != "" {
				ds.Add(diag.Diagnostic{
					Severity: diag.SeverityError,
					Summary:  "`extends` is set more than once on environment " + strconv.Quote(name),
					Detail:   "The last assignment would silently win, changing which values " + strconv.Quote(name) + " inherits. It is also set at " + describeOrigin(env.ExtendsOrigin) + ".",
					Action:   "Remove one of the two assignments.",
					Origin:   keyOrigin,
				})
				continue
			}
			env.Extends, env.ExtendsOrigin = text, keyOrigin

		case "variables":
			// PLAN.md §6's spelling.
			if val.Kind != yaml.MappingNode {
				ds.Add(diag.Diagnostic{
					Severity: diag.SeverityError,
					Summary:  "`variables` in environment " + strconv.Quote(name) + " must be a mapping",
					Origin:   originOf(path, val),
				})
				continue
			}
			for j := 0; j+1 < len(val.Content); j += 2 {
				addOverride(path, env, val.Content[j], val.Content[j+1], ds)
			}

		default:
			// PLAN.md §7's spelling: any other key is an override.
			addOverride(path, env, key, val, ds)
		}
	}
}

// addOverride records one environment override, refusing a second setting of
// the same name.
//
// A collision is reachable three ways — the two spellings within one
// environment, the same key twice in one file, and the block plus the file —
// and every one of them silently discards a value the user wrote, producing a
// plan that is wrong with nothing printed.
func addOverride(path string, env *EnvironmentDecl, key, val *yaml.Node, ds *diag.Diagnostics) {
	origin := originOf(path, key)
	// APPENDS IN DOCUMENT ORDER. That is not the order the field promises:
	// Decode sorts every environment's Overrides by name once, after all files
	// are decoded, which is the only point at which the block's and the file's
	// contributions are both present. Do not add a sort here — a reader who
	// looks only at this function concludes the slice is unsorted, which is a
	// real misreading that has already happened once during review.
	//
	// The linear scan below is what lets the duplicate diagnostic name where
	// the FIRST assignment was.
	for _, existing := range env.Overrides {
		if existing.Name == key.Value {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  strconv.Quote(key.Value) + " is set more than once in environment " + strconv.Quote(env.Name),
				Detail:   "The last assignment would silently win. It is also set at " + describeOrigin(existing.Origin) + ".",
				Action:   "Remove one of the two assignments.",
				Origin:   origin,
			})
			return
		}
	}

	v, hasExpr := decodeValue(path, "environment "+strconv.Quote(env.Name)+"'s "+strconv.Quote(key.Value), val, ds)
	if hasExpr {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "environment override " + strconv.Quote(key.Value) + " contains an interpolation",
			Detail:   "Environment overrides are resolved before any expression scope exists, so `${...}` here has nothing to refer to.",
			Action:   "Write a literal value.",
			Origin:   originOf(path, val),
		})
		return
	}
	env.Overrides = append(env.Overrides, OverrideDecl{
		Name:   key.Value,
		Value:  retagSource(v, value.SourceEnvironment).WithOrigin(origin),
		Origin: origin,
	})
}
```

`decode.go` already imports `sort`, `strconv`, `strings`, `yaml`, `diag` and
`value`; nothing new is needed.

#### 3.8 — Run

```bash
export PATH="$HOME/.local/share/mise/shims:$PATH"
gofmt -l . && go vet ./... && go test -count=1 ./...
```

`internal/compiler`'s existing tests call `config.Decode` and `config.Load`
against M1/M2 fixtures with no `variables:` or `environments:` — those must
still pass unchanged. If `internal/cli`'s `validate` now warns on something it
did not before, check the "unrecognised top-level key" text: it changed from
"M1 understands…" to name all four keys, and a test may pin the old string.
Locate it with:

```bash
grep -rn "unrecognised top-level key" --include=*_test.go .
```

#### 3.9 — Commit

```bash
git add -A && git commit -m "$(cat <<'EOF'
config: stage 2 decodes variables and environments

PLAN.md §9's typed variable declarations (type/default/min/max) and §6-§7's
environments (extends plus overrides) now become typed declarations.
Stage 2 is the only stage permitted to touch yaml.Node, so every shape
check lives here, where the line numbers still exist.

Both of PLAN.md's override spellings work: §6 nests them under
`variables:`, §7 writes them as flat keys. An environment declared both
in infra.yml's block and in environments/<name>.yml merges, since §7 puts
extends in the block and §8 puts overrides in the file; only a key set in
both places is an error, because one value would silently disappear.

Variable type spellings come from value.ParseKind, so this package keeps
no table of its own and cannot drift from the one the parser consults.

A declaration with neither a type nor a default is an error. Dropping it
would discard something the user wrote and carry on as though they had
not.

min/max are numeric-only and are checked after the whole mapping is
walked, so a `type:` written below a `min:` still applies. Bounds stay
value.Value rather than float64: an int64 does not fit a float64, so a
bound above 2^53 would validate wrongly, and the Value's Origin lets a
range diagnostic point at the line the bound is on.

A bound is coerced to its variable's declared type here, because this is
the only stage that knows both the type and the line number. After it a
bound's Kind always equals the declared Type, so consumers switch on one
Kind and value.AsFloat is safe to use directly. A conversion that would
lose information — `type: integer` with `min: 1.5` — is a diagnostic, not
a silent truncation that would accept values below the stated minimum.

Cycles and unknown extends targets are NOT checked here: they need the
whole set of environments and belong to stage 3. Scope is not set here
either — a declaration is not a resolution, and stage 4 stamps a winning
default as SourceDefault + ScopeBaseConfig.

variables.yml holds values only; declarations live in infra.yml and
nowhere else, so "where is this variable declared" has one answer.

ProjectDecl.Origin is now assigned only for the project file; it was
assigned per file, which with M4's extra files would have pointed every
project-level diagnostic at the last environment file.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 4 — typed variable schemas

`PLAN.md` §9. A variable may declare a type and constraints; the declaration is
checked when the schema is built, and every resolved value is checked against it.
Validation must happen during `infra validate` and before planning, which is why the
check lives in a package both paths reach (Task 7 wires both).

`PLAN.md` §9 closes with "Do not build a general-purpose programming language into
variable expressions." Every ruling below that looks restrictive is that sentence
being obeyed. Where a construct has two plausible meanings, this task REJECTS it. A
rejection can be relaxed later without breaking anyone's configuration; an accepted
meaning cannot be changed. The configuration language is a product API (CLAUDE.md),
so the asymmetry decides.

## Files

- create `internal/variables/schema.go`
- create `internal/variables/schema_test.go`

Nothing in `pkg/value` changes here. `AsFloat` is added by Task 1; this task only
calls it.

## Interfaces

**Consumes:** `config.VariableDecl` (Task 3), `diag.Diagnostics`, `value.Value`,
`value.Kind`, `value.Scope`, `value.Value.AsFloat` (Task 1).

**Produces** — Tasks 6 and 7 depend on exactly these:

```go
package variables

// Schema is one variable's declared type and constraints.
type Schema struct {
    Name       string
    Kind       value.Kind   // KindInvalid for an untyped declaration
    Default    value.Value  // exactly as declared; stage 4 stamps its provenance
    HasDefault bool
    Min        value.Value
    HasMin     bool
    Max        value.Value
    HasMax     bool
    Origin     value.Origin
}

// Schemas builds the schema table from stage 2's declarations.
func Schemas(decls []config.VariableDecl) (map[string]Schema, diag.Diagnostics)

// Validate reports every way v violates s.
func (s Schema) Validate(v value.Value) diag.Diagnostics

// ParseText converts one command-line string to s's kind.
func (s Schema) ParseText(text string, origin value.Origin) (value.Value, diag.Diagnostics)
```

## The rulings this task fixes, and why

1. **Bounds keep the `value.Value` shape stage 2 decoded them into.** Not `float64`:
   an `int64` above 2^53 does not survive a round trip through `float64`, so a large
   integer `min:` would silently validate the wrong thing. `value.Value` also carries
   `Origin`, which is what lets a bound diagnostic say where the bound was declared —
   `PLAN.md` §44 requires an error to state what was expected AND where.

2. **Comparison happens in the declared kind.** `KindInt` compares `int64` against
   `int64`; `KindFloat` compares `float64` against `float64`. Nothing is funnelled
   through a single numeric type, because funnelling is exactly what ruling 1 rejects.
   `AsFloat` is the accessor for the float side, never the comparison strategy.

   `compareBounds` below is the same comparison stage 2's `numericGreater` performs
   for its `min > max` check (`internal/config/decode.go`), differing only in that
   this one is told the declared `Kind` rather than inferring it from the values,
   which is the stricter reading. They are two unexported helpers in two packages
   because neither package may import the other's internals. If a third caller
   appears, promote one exported `value.CompareNumeric(k value.Kind, a, b value.Value) int`
   into `pkg/value` and delete both — a precision bug in one of these IS a precision
   bug in the other, which is the coupling test that decides.

3. **`Schemas` copies bounds through; it does not re-validate the declaration.**
   Stage 2 has already coerced a bound to its variable's declared `Kind` and rejected
   every malformed declaration — a bound on a non-numeric or untyped variable, a
   non-numeric bound, `min: 1.5` on an `integer`, and `min` above `max`. Re-checking
   any of those here would produce a second diagnostic for one mistake, and a worse
   one: stage 2 has the line and column, and stage 4 has only the variable's name.
   `PLAN.md` §44 wants the error where the user wrote it.

   Do not add such checks back on the reasoning that they are cheap insurance. A
   check that cannot fire is not insurance; it is a branch the next reader writes a
   test around, and that test cannot fail.

4. **Bounds are inclusive.** `min: 1` permits 1.

5. **An untyped declaration is legal; an EMPTY one is stage 2's error, not this
   task's.** `PLAN.md` §9 makes schemas optional, and stage 2 hands untyped
   declarations through as `Type: KindInvalid`. An untyped declaration WITH a
   `default:` is a perfectly good declaration: it supplies a value and constrains
   nothing, and `Schemas` keeps it.

   A declaration with neither a `type` nor a `default` is rejected by stage 2, at the
   line the user wrote it. **`Schemas` must not silently drop such a declaration**,
   and must not downgrade the case to a warning if it ever reaches here. Dropping a
   declaration discards input the user supplied; a warning in front of a discard is
   still a discard, and quietly losing user input is the failure shape this project
   has spent three milestones finding. Rejecting it at decode time tells the user
   immediately, where they can fix it.

   The consequence Tasks 4 and 6 depend on: **every schema either has a real `Kind`,
   or has a default that means the unset case never arises.** That property is bought
   by an error, not by throwing away the case that violates it — a distinction worth
   holding onto, because buying a property by discarding its counterexamples is the
   same move as weakening a test to fit the code.

6. **`list` and `map` are checked for kind only.** There is no element-type syntax in
   `PLAN.md` §9, and inventing one is a language extension. Per-leaf provenance means
   each element already carries its own `Kind`, `Source` and `Scope`.

7. **`--var` cannot supply a `list` or a `map`.** `--var tags=a,b,c` requires a
   parsing mini-language; `PLAN.md` §9 forbids one. `ParseText` reports an error
   naming the files that can set it.

8. **An unknown value passes `Validate` silently.** A value whose `Known` is false
   still carries its `Kind` (spec §5.1, §6), so a kind mismatch is still catchable,
   but there is no datum to bound-check. Task 6 relies on this: `infra validate`
   with no environment selected resolves environment-scoped variables to unknowns.

9. **`Schemas` stores `Default` exactly as declared, unstamped.** Stage 4 decides
    what provenance a WINNING value carries, and it is the only place that decides —
    a declaration is not a resolution. `Schemas` does check the declared default
    against its own bounds (a `default: 500` under `max: 100` can never be satisfied),
    using a locally stamped copy for the message only.

    **This one check stays here even though every other declaration check is stage
    2's**, and the split is not arbitrary. Stage 2 validates the declaration's own
    SHAPE — `min <= max`, a bound's kind matching the declared type. A `default` is
    not part of that shape; it is a VALUE, and checking a value against a schema is
    what `Validate` is for. Routing it through `Validate` means a user's `default:`
    and their `--var` are judged by the same code and can never disagree about an
    edge case — an inclusive bound, or how an unknown compares.

    **§44 is satisfied without moving it, so do not go looking for a way to plumb an
    origin down from stage 2.** `Schema.Default` is a `value.Value`, and a `Value`
    carries its own `Origin`: the line and column travel WITH the datum. That is the
    second time ruling 1 pays for itself — bounds and defaults both keep their
    location because neither was flattened into a bare number.

## Steps

### 4.1 Failing test: schemas carry the declared type through

Both directions of every predicate. A table that only ever feeds it valid input
cannot tell a working implementation from one that accepts everything.

Write `internal/variables/schema_test.go`:

```go
package variables

import (
    "strings"
    "testing"

    "infra/internal/config"
    "infra/internal/diag"
    "infra/pkg/value"
)

func decl(name string, kind value.Kind) config.VariableDecl {
    return config.VariableDecl{
        Name:   name,
        Type:   kind,
        Origin: value.Origin{File: "variables.yml", Line: 3, Column: 5},
    }
}

func withDefault(d config.VariableDecl, v value.Value) config.VariableDecl {
    d.Default, d.HasDefault = v, true
    return d
}

func TestSchemasCarriesEveryKindThrough(t *testing.T) {
    kinds := []value.Kind{
        value.KindString, value.KindInt, value.KindFloat,
        value.KindBool, value.KindList, value.KindMap,
    }
    var decls []config.VariableDecl
    for _, k := range kinds {
        decls = append(decls, decl("v_"+k.String(), k))
    }

    got, ds := Schemas(decls)
    if ds.HasErrors() {
        t.Fatalf("unexpected diagnostics: %+v", ds)
    }
    for _, k := range kinds {
        s, ok := got["v_"+k.String()]
        if !ok {
            t.Fatalf("schema for kind %s is missing", k)
        }
        if s.Kind != k {
            t.Errorf("kind %s came through as %s", k, s.Kind)
        }
    }
}

func TestSchemasKeepsAnUntypedDeclarationThatHasADefault(t *testing.T) {
    // PLAN.md §9 makes schemas optional. A declaration with a default and no
    // type supplies a value and constrains nothing.
    d := decl("greeting", value.KindInvalid)
    d.Default, d.HasDefault = value.String("hello", value.SourceExplicit), true

    got, ds := Schemas([]config.VariableDecl{d})
    if ds.HasErrors() {
        t.Fatalf("an untyped declaration with a default is legal: %+v", ds)
    }
    s, ok := got["greeting"]
    if !ok {
        t.Fatal("greeting must stay in the table: it supplies a default")
    }
    if s.Kind != value.KindInvalid || !s.HasDefault {
        t.Errorf("schema = %+v, want an untyped schema carrying a default", s)
    }
}

func TestSchemasKeepsEveryDeclarationItIsGiven(t *testing.T) {
    // Nothing is dropped here. A declaration with neither a type nor a default
    // is stage 2's error, reported at the line the user wrote it; if one ever
    // reaches this table it must still come out the other side, because
    // discarding a declaration discards input the user supplied and the user
    // has no way to see that it happened.
    decls := []config.VariableDecl{
        decl("typed", value.KindInt),
        withDefault(decl("untyped", value.KindInvalid), value.String("x", value.SourceExplicit)),
    }
    got, ds := Schemas(decls)
    if ds.HasErrors() {
        t.Fatalf("both declarations are legal: %+v", ds)
    }
    if len(got) != len(decls) {
        t.Fatalf("Schemas returned %d schemas for %d declarations: %v", len(got), len(decls), got)
    }
}

func TestSchemasStoresTheDeclaredDefaultUnstamped(t *testing.T) {
    // Stage 4 decides what provenance a WINNING value carries, and it is the
    // only place that decides. A declaration is not a resolution.
    d := decl("replicas", value.KindInt)
    d.Default, d.HasDefault = value.Int(2, value.SourceExplicit), true

    got, _ := Schemas([]config.VariableDecl{d})
    if s := got["replicas"]; s.Default.Scope != value.ScopeUnset {
        t.Errorf("Default.Scope = %v, want ScopeUnset: only stage 4 may say which rung won", s.Default.Scope)
    }
}
```

### 4.2 Run it, see it fail

```bash
export PATH="$HOME/.local/share/mise/shims:$PATH"
go test -count=1 ./internal/variables/
```

Expect a build failure: `undefined: Schemas`. The package does not exist yet.

### 4.3 Minimal code: `Schemas`' skeleton

Create `internal/variables/schema.go`:

```go
// Package variables implements compiler stage 4: it resolves every variable to
// the value that wins the precedence chain, and validates it against its
// declared schema (PLAN.md §8, §9; spec §7, §7.1).
package variables

import (
    "strconv"

    "infra/internal/config"
    "infra/internal/diag"
    "infra/pkg/value"
)

// Schema is one variable's declared type and constraints (PLAN.md §9).
//
// Min and Max keep the value.Value shape stage 2 decoded them into rather than
// collapsing to float64. An int64 above 2^53 does not survive a round trip
// through float64, so a large integer bound would silently validate the wrong
// thing; and a Value carries Origin, which is what lets a bound diagnostic say
// WHERE the bound was declared as PLAN.md §44 requires.
//
// Kind is KindInvalid for an untyped declaration, which PLAN.md §9 permits.
// Schemas guarantees that every entry in its table either has a real Kind or
// has a default — see the empty-declaration warning below — so a consumer that
// needs a kind to build an unknown from always has one when it needs one.
type Schema struct {
    Name       string
    Kind       value.Kind
    Default    value.Value
    HasDefault bool
    Min        value.Value
    HasMin     bool
    Max        value.Value
    HasMax     bool
    Origin     value.Origin
}

// Schemas builds the schema table from stage 2's declarations.
//
// The `type:` spelling has already been mapped to a Kind by stage 2, which is
// where an unknown spelling is reported — with the line and column it was
// written at. Nothing here re-derives that mapping: two implementations of one
// concept is the defect that leaked a plaintext secret in M2.
//
// Every problem in every declaration is reported in one pass (spec §7.4): a
// declaration with a bad bound keeps its type and loses the bound rather than
// stopping the walk, so a second bad declaration is still reported.
func Schemas(decls []config.VariableDecl) (map[string]Schema, diag.Diagnostics) {
    var ds diag.Diagnostics
    out := make(map[string]Schema, len(decls))

    for _, d := range decls {
        // Every declaration reaching here is kept. A declaration with neither a
        // `type` nor a `default` was already rejected by stage 2, at the line
        // the user wrote it — do not add a second check that DROPS one here.
        // Discarding a declaration discards input the user supplied, and a
        // warning in front of a discard is still a discard.
        s := Schema{Name: d.Name, Kind: d.Type, Origin: d.Origin}
        // Bounds are carried across in 4.7.
        if d.HasDefault {
            s.Default, s.HasDefault = d.Default, true
            // Checked against its own constraints here, once, rather than
            // every time the default wins. The stamped copy is for the
            // diagnostic's wording only; the stored Default stays unstamped
            // because stage 4 is the one place that decides what provenance a
            // winning value carries.
            //
            // This is the ONE declaration-level check stage 2 does not make,
            // and it belongs here rather than there: a default is a VALUE, not
            // part of the declaration's shape, so it is checked by the same
            // Validate that checks every supplied value — a `default:` and a
            // `--var` can then never be judged differently.
            //
            // The diagnostic still names the line the default was written on,
            // with nothing plumbed down from stage 2: d.Default is a
            // value.Value and carries its own Origin, which Validate reads.
            // Flattening declarations into bare numbers is what would have
            // lost that.
            ds.Extend(s.Validate(d.Default.
                WithSource(value.SourceDefault).
                WithScope(value.ScopeBaseConfig)))
        }
        out[d.Name] = s
    }
    return out, ds
}
```

`Validate` arrives in 4.11. To keep 4.4 runnable, write `Validate` first, or run 4.4
after 4.11 and say in the commit message that the two steps merged. Do not leave a
stub in a commit.

### 4.4 Run it, see it pass

```bash
go test -count=1 ./internal/variables/
```

Commit: `M4 task 4: variable schema table`.

### 4.5 Failing test: bounds are carried through exactly

Every check on a bound DECLARATION lives in stage 2 (see the guarantees at the top of
this plan): a bound on a non-numeric or untyped variable, a non-numeric bound,
`min: 1.5` on an `integer`, a bound too large to convert, and `min` above `max` are
all rejected there, at the line the user wrote them. What is left for `Schemas` is to
carry the bound across without damaging it, and to check the declared DEFAULT against
those bounds — the one declaration-level check stage 2 does not perform.

```go
func numDecl(name string, kind value.Kind, min, max value.Value, hasMin, hasMax bool) config.VariableDecl {
    d := decl(name, kind)
    d.Min, d.HasMin = min, hasMin
    d.Max, d.HasMax = max, hasMax
    return d
}

func TestSchemasCarriesBoundsThrough(t *testing.T) {
    decls := []config.VariableDecl{
        numDecl("replicas", value.KindInt, value.Int(1, value.SourceExplicit), value.Int(100, value.SourceExplicit), true, true),
        numDecl("ratio", value.KindFloat, value.Float(0.5, value.SourceExplicit), value.Float(1.5, value.SourceExplicit), true, true),
    }
    got, ds := Schemas(decls)
    if ds.HasErrors() {
        t.Fatalf("unexpected diagnostics: %+v", ds)
    }
    if s := got["replicas"]; !s.HasMin || !s.HasMax {
        t.Fatalf("integer bounds were dropped: %+v", s)
    }
    if n, _ := got["replicas"].Min.AsInt(); n != 1 {
        t.Errorf("min = %d, want 1", n)
    }
    if f, _ := got["ratio"].Max.AsFloat(); f != 1.5 {
        t.Errorf("max = %v, want 1.5", f)
    }
}

func TestSchemasCarriesBoundsWithoutFlatteningThem(t *testing.T) {
    // The reason bounds are value.Value and not float64. 2^53+1 is the
    // smallest integer float64 cannot represent; through a float64 it becomes
    // 2^53, and a value of exactly 2^53+1 would then validate as "at the
    // minimum" when it is in fact below it. This test fails against the
    // rejected DESIGN, not merely against a broken implementation.
    const tooBigForFloat64 = int64(1)<<53 + 1
    d := numDecl("big", value.KindInt, value.Int(tooBigForFloat64, value.SourceExplicit), value.Value{}, true, false)

    got, ds := Schemas([]config.VariableDecl{d})
    if ds.HasErrors() {
        t.Fatalf("unexpected diagnostics: %+v", ds)
    }
    if n, _ := got["big"].Min.AsInt(); n != tooBigForFloat64 {
        t.Errorf("min = %d, want %d — the bound must survive exactly", n, tooBigForFloat64)
    }
}

func TestSchemasKeepsTheBoundsOrigin(t *testing.T) {
    // PLAN.md §44: a range error must be able to say WHERE the bound was
    // declared. Stage 2 put an Origin on the bound; losing it here would make
    // that impossible without any test failing elsewhere.
    d := numDecl("replicas", value.KindInt, value.Int(1, value.SourceExplicit), value.Value{}, true, false)
    d.Min = d.Min.WithOrigin(value.Origin{File: "variables.yml", Line: 5, Column: 7})

    got, _ := Schemas([]config.VariableDecl{d})
    if got["replicas"].Min.Origin.Line != 5 {
        t.Errorf("Min.Origin = %+v, want variables.yml:5:7", got["replicas"].Min.Origin)
    }
}

func TestSchemasChecksTheDeclaredDefaultAgainstItsOwnConstraints(t *testing.T) {
    // The one declaration-level check stage 2 does not perform: a `default`
    // outside its own `min`/`max` is a declaration nothing can satisfy, and
    // reporting it once here beats reporting it every time the default wins.
    build := func(def int64) diag.Diagnostics {
        d := withDefault(
            numDecl("replicas", value.KindInt, value.Int(1, value.SourceExplicit), value.Int(100, value.SourceExplicit), true, true),
            value.Int(def, value.SourceExplicit))
        _, ds := Schemas([]config.VariableDecl{d})
        return ds
    }
    if !build(500).HasErrors() {
        t.Error("`default: 500` with `max: 100` can never be satisfied and must be rejected")
    }
    if build(2).HasErrors() {
        t.Error("a default inside its own bounds must be accepted")
    }
}

func TestSchemasDefaultDiagnosticNamesTheLineTheDefaultIsOn(t *testing.T) {
    // The check runs in stage 4, and PLAN.md §44 still gets its location: the
    // Origin travels with the datum because Default is a value.Value. This
    // test is what stops someone concluding the origin is lost and moving the
    // check into stage 2 to recover it.
    d := withDefault(
        numDecl("replicas", value.KindInt, value.Int(1, value.SourceExplicit), value.Int(100, value.SourceExplicit), true, true),
        value.Int(500, value.SourceExplicit).WithOrigin(value.Origin{File: "variables.yml", Line: 9, Column: 11}))

    _, ds := Schemas([]config.VariableDecl{d})
    var sb strings.Builder
    ds.Render(&sb)
    if !strings.Contains(sb.String(), "variables.yml:9:11") {
        t.Errorf("the diagnostic must point at the default, not just name the variable:\n%s", sb.String())
    }
}
```

### 4.6 Run it, see it fail

```bash
go test -count=1 -run TestSchemas ./internal/variables/
```

Expect `TestSchemasCarriesBoundsThrough` to fail with `integer bounds were dropped` —
4.3 left the copy for this step — and the other four to fail alongside it: with no
bounds on the schema there is nothing for the default to be checked against, so the
two default tests report no diagnostic where they expect one.

### 4.7 Minimal code: carry the bounds, and the comparison the checks need

In `Schemas`, replace the `// Bounds are carried across in 4.7.` line with:

```go
        // Bounds are copied, not re-validated. Stage 2 coerced each one to
        // d.Type and rejected every malformed declaration, with the line and
        // column this stage does not have. Adding a second check here would
        // report one mistake twice, and the second report would be the worse
        // of the two.
        s.Min, s.HasMin = d.Min, d.HasMin
        s.Max, s.HasMax = d.Max, d.HasMax
```

Then add the shared helpers to `schema.go`. `compareBounds` is used by both the
declared-default check above and the value range check in 4.11:

```go
// compareBounds orders two values IN THE DECLARED KIND: integers as int64,
// floats as float64. Nothing is funnelled through one numeric type — that
// funnelling loses an int64 above 2^53, which is the whole reason bounds are
// Values and not float64s.
//
// Both operands are known to share s.Kind by the time this is called: stage 2
// coerced the bound to the declared type, and Validate has already compared
// the value's Kind to the schema's. Returns -1, 0 or 1; a pair it cannot read
// compares as 0, so a malformed value produces no bound complaint on top of
// the malformed-value diagnostic its caller already emits.
func compareBounds(k value.Kind, a, b value.Value) int {
    switch k {
    case value.KindInt:
        x, okA := a.AsInt()
        y, okB := b.AsInt()
        if !okA || !okB {
            return 0
        }
        switch {
        case x < y:
            return -1
        case x > y:
            return 1
        }
    case value.KindFloat:
        x, okA := a.AsFloat()
        y, okB := b.AsFloat()
        if !okA || !okB {
            return 0
        }
        switch {
        case x < y:
            return -1
        case x > y:
            return 1
        }
    }
    return 0
}

// numericDatumMatchesKind reports whether v's datum really is of kind k.
//
// Kind is a CLAIM about Raw and this never takes it on trust: value.Equal and
// value.Format each shipped a version that did, producing a false equality in
// one case and a printed secret in the other. Used by Validate on incoming
// values, where a mismatch is reachable — a Value can arrive from state, from
// a provider, or from a test.
func numericDatumMatchesKind(k value.Kind, v value.Value) bool {
    switch k {
    case value.KindInt:
        _, ok := v.AsInt()
        return ok
    case value.KindFloat:
        _, ok := v.AsFloat()
        return ok
    }
    return false
}

// show renders a value for a diagnostic. It goes through value.Format, which
// is the engine's only rendering path — a second one is what leaked a
// plaintext secret in M2, and a bound is as capable of being sensitive as
// anything else.
func show(v value.Value) string {
    return value.Format(v, value.FormatOptions{Unknown: "(unknown)"})
}

// originOr prefers the more specific of two origins. A bound decoded from YAML
// carries its own line; a synthesised one does not, and the declaration's
// origin is then the closest true answer.
func originOr(specific, fallback value.Origin) value.Origin {
    if specific.File != "" {
        return specific
    }
    return fallback
}

// article picks "a" or "an" so diagnostics read as English.
func article(k value.Kind) string {
    if k == value.KindInt {
        return "an"
    }
    return "a"
}
```

Check `value.Format`'s exact signature before you write `show` —
`grep -n "func Format" pkg/value/format.go` — and match it.

### 4.8 Run it, see it pass

```bash
go test -count=1 ./internal/variables/
```

Commit: `M4 task 4: carry variable bounds`.


### 4.9 Failing test: `Validate` reports a kind mismatch and enforces bounds in both directions

```go
func intSchema(t *testing.T, min, max int64) Schema {
    t.Helper()
    d := numDecl("replicas", value.KindInt, value.Int(min, value.SourceExplicit), value.Int(max, value.SourceExplicit), true, true)
    d.Min = d.Min.WithOrigin(value.Origin{File: "variables.yml", Line: 5, Column: 7})
    got, ds := Schemas([]config.VariableDecl{d})
    if ds.HasErrors() {
        t.Fatalf("fixture schema is itself invalid: %+v", ds)
    }
    return got["replicas"]
}

func TestValidateRejectsTheWrongKind(t *testing.T) {
    s := intSchema(t, 1, 100)
    v := value.String("many", value.SourceVariable).WithScope(value.ScopeCLIOverride)

    ds := s.Validate(v)
    if !ds.HasErrors() {
        t.Fatal("a string supplied for an integer variable must be rejected")
    }
    var sb strings.Builder
    ds.Render(&sb)
    out := sb.String()
    for _, want := range []string{"replicas", "integer", "string", "--var"} {
        if !strings.Contains(out, want) {
            t.Errorf("the diagnostic must name the variable, what was expected, what was found, and WHICH SCOPE supplied it; %q is missing:\n%s", want, out)
        }
    }
}

func TestValidateAcceptsTheRightKind(t *testing.T) {
    if ds := intSchema(t, 1, 100).Validate(value.Int(4, value.SourceVariable)); ds.HasErrors() {
        t.Fatalf("a valid integer must produce no diagnostics: %+v", ds)
    }
}

func TestValidateEnforcesBoundsInclusively(t *testing.T) {
    s := intSchema(t, 1, 100)
    for _, tc := range []struct {
        n       int64
        wantErr bool
    }{
        {0, true},    // below min
        {1, false},   // ON min: inclusive
        {50, false},
        {100, false}, // ON max: inclusive
        {101, true},  // above max
    } {
        ds := s.Validate(value.Int(tc.n, value.SourceVariable))
        if got := ds.HasErrors(); got != tc.wantErr {
            t.Errorf("Validate(%d) errored = %v, want %v (bounds are inclusive: 1 and 100 are permitted, 0 and 101 are not)", tc.n, got, tc.wantErr)
        }
    }
}

func TestValidateComparesIntegersThatFloat64WouldConflate(t *testing.T) {
    // 2^53 and 2^53+1 are the same float64. Comparison must happen in int64 or
    // a value exactly one below the minimum validates as being on it.
    const min = int64(1)<<53 + 1
    s := intSchema(t, min, min+1000)
    if ds := s.Validate(value.Int(min-1, value.SourceVariable)); !ds.HasErrors() {
        t.Errorf("%d is below the minimum of %d and must be rejected; through float64 the two are indistinguishable", min-1, min)
    }
    if ds := s.Validate(value.Int(min, value.SourceVariable)); ds.HasErrors() {
        t.Errorf("%d is exactly the minimum and must be accepted: %+v", min, ds)
    }
}

func TestValidatePointsAtWhereTheBoundWasDeclared(t *testing.T) {
    // PLAN.md §44: an error says what was expected AND where. A bound
    // diagnostic that names only the offending value leaves the reader hunting
    // for the constraint.
    ds := intSchema(t, 1, 100).Validate(value.Int(500, value.SourceVariable))
    var sb strings.Builder
    ds.Render(&sb)
    if !strings.Contains(sb.String(), "variables.yml:5:7") {
        t.Errorf("the diagnostic must locate the declared bound:\n%s", sb.String())
    }
}

func TestValidateIgnoresAnUnknownValue(t *testing.T) {
    // `infra validate` with no environment selected resolves environment-scoped
    // variables to unknowns (task 6). An unknown carries its Kind but has no
    // datum, so a bound check on it would be an invented answer.
    if ds := intSchema(t, 1, 100).Validate(value.Unknown(value.KindInt, value.SourceVariable)); ds.HasErrors() {
        t.Fatalf("an unknown value has no datum to bound-check: %+v", ds)
    }
}

func TestValidateRejectsAnUnknownOfTheWrongKind(t *testing.T) {
    if ds := intSchema(t, 1, 100).Validate(value.Unknown(value.KindString, value.SourceVariable)); !ds.HasErrors() {
        t.Fatal("an unknown still carries its Kind, so a kind mismatch is catchable even when the datum is not")
    }
}

func TestValidateAcceptsAnythingForAnUntypedDeclaration(t *testing.T) {
    d := decl("anything", value.KindInvalid)
    d.Default, d.HasDefault = value.String("x", value.SourceExplicit), true
    got, _ := Schemas([]config.VariableDecl{d})
    s := got["anything"]

    for _, v := range []value.Value{
        value.String("x", value.SourceVariable),
        value.Int(1, value.SourceVariable),
        value.Bool(true, value.SourceVariable),
    } {
        if ds := s.Validate(v); ds.HasErrors() {
            t.Errorf("an untyped declaration constrains nothing, but %s was rejected: %+v", v.Kind, ds)
        }
    }
}

func TestValidateChecksListAndMapByKindOnly(t *testing.T) {
    got, ds := Schemas([]config.VariableDecl{decl("tags", value.KindList)})
    if ds.HasErrors() {
        t.Fatalf("fixture: %+v", ds)
    }
    s := got["tags"]

    mixed := value.List([]value.Value{
        value.String("a", value.SourceVariable),
        value.Int(2, value.SourceVariable),
    }, value.SourceVariable)
    if ds := s.Validate(mixed); ds.HasErrors() {
        t.Fatalf("PLAN.md §9 has no element-type syntax, so a list's elements are unconstrained: %+v", ds)
    }
    if ds := s.Validate(value.String("a,b", value.SourceVariable)); !ds.HasErrors() {
        t.Fatal("a string supplied for a list variable is still a kind mismatch")
    }
}
```

### 4.10 Run it, see it fail

```bash
go test -count=1 -run TestValidate ./internal/variables/
```

If you stubbed `Validate` in 4.3, expect the kind, bound and origin tests to fail on
their own messages. If you did not, expect `s.Validate undefined`.

### 4.11 Minimal code: `Validate`

```go
// Validate reports every way v violates s.
//
// An untyped declaration (Kind KindInvalid) constrains nothing: PLAN.md §9
// makes schemas optional, and a declaration that gave no type has said nothing
// about what values are acceptable. Bounds cannot reach an untyped schema —
// stage 2 rejects them — so there is nothing left to check.
//
// An unknown value is checked for kind and nothing else. Spec §5.1 keeps Kind
// on an unknown precisely so type errors surface at plan time rather than
// apply time, but there is no datum to compare against a bound, and inventing
// one would be a confident wrong answer.
//
// The kind switch is an allowlist with no `default` arm that trusts Raw.
// KindInvalid is the zero value of Kind, and value.Equal and value.Format each
// shipped a permissive default that turned a malformed value into a wrong
// answer — a false equality in one case and a printed secret in the other.
// A Value whose Raw does not match its Kind is reported, not interpreted.
func (s Schema) Validate(v value.Value) diag.Diagnostics {
    var ds diag.Diagnostics

    if s.Kind == value.KindInvalid {
        return ds
    }

    if v.Kind != s.Kind {
        ds.Add(diag.Diagnostic{
            Severity: diag.SeverityError,
            Summary:  "variable " + strconv.Quote(s.Name) + " must be " + article(s.Kind) + " " + s.Kind.String(),
            Detail: "Declared as " + s.Kind.String() + " at " + s.Origin.String() +
                ". The value supplied by " + v.Scope.String() + " is " + article(v.Kind) + " " + v.Kind.String() + ".",
            Action: "Supply " + article(s.Kind) + " " + s.Kind.String() + " value, or change the declared type.",
            Origin: originOr(v.Origin, s.Origin),
        })
        return ds
    }
    if !v.Known {
        return ds
    }

    switch s.Kind {
    case value.KindInt, value.KindFloat:
        if !numericDatumMatchesKind(s.Kind, v) {
            ds.Add(malformed(s, v))
            return ds
        }
        checkBounds(s, v, &ds)
    case value.KindString, value.KindBool, value.KindList, value.KindMap:
        // Kind is the whole constraint. PLAN.md §9 declares no element type
        // for list or map, and adding one is a language extension, not a
        // validation detail.
    default:
        ds.Add(malformed(s, v))
    }
    return ds
}

// checkBounds reports a value outside its schema's inclusive range. Both
// bounds are inclusive: `min: 1` permits 1. Comparison happens in the declared
// kind (see compareBounds).
func checkBounds(s Schema, v value.Value, ds *diag.Diagnostics) {
    if s.HasMin && compareBounds(s.Kind, v, s.Min) < 0 {
        ds.Add(boundDiag(s, "at least", s.Min, v))
    }
    if s.HasMax && compareBounds(s.Kind, v, s.Max) > 0 {
        ds.Add(boundDiag(s, "at most", s.Max, v))
    }
}

// boundDiag names the constraint, the offending value, the scope that supplied
// it, and WHERE the bound was declared. PLAN.md §44 requires the last of those
// and it is the one a reader cannot reconstruct for themselves.
func boundDiag(s Schema, relation string, bound value.Value, v value.Value) diag.Diagnostic {
    limit := show(bound)
    return diag.Diagnostic{
        Severity: diag.SeverityError,
        Summary:  "variable " + strconv.Quote(s.Name) + " must be " + relation + " " + limit,
        Detail: "The value supplied by " + v.Scope.String() + " is " + show(v) + ". The bound is declared at " +
            originOr(bound.Origin, s.Origin).String() + ".",
        Action: "Choose a value " + relation + " " + limit + ".",
        Origin: originOr(v.Origin, s.Origin),
    }
}

// malformed reports a Value whose Raw does not match the Kind it claims. This
// is an engine bug rather than a user error, but it is reported rather than
// ignored: the alternative is a silently unchecked value.
func malformed(s Schema, v value.Value) diag.Diagnostic {
    return diag.Diagnostic{
        Severity: diag.SeverityError,
        Summary:  "variable " + strconv.Quote(s.Name) + " holds a malformed value",
        Detail:   "It claims kind " + v.Kind.String() + " but its datum does not match. This is an internal error.",
        Action:   "Report this, with the configuration that produced it.",
        Origin:   originOr(v.Origin, s.Origin),
    }
}
```

### 4.12 Run it, see it pass

```bash
go test -count=1 ./internal/variables/ && go vet ./... && gofmt -l .
```

`gofmt -l .` must print nothing. Commit: `M4 task 4: variable value validation`.

### 4.13 Failing test: `ParseText` converts `--var` strings, and refuses composites

`--var name=value` yields a string and nothing else. A typed variable therefore needs
the string converted to its kind, or `--var replicas=20` would fail its own integer
schema.

```go
func schemaFor(t *testing.T, kind value.Kind) Schema {
    t.Helper()
    got, ds := Schemas([]config.VariableDecl{decl("v", kind)})
    if ds.HasErrors() {
        t.Fatalf("fixture schema %s is invalid: %+v", kind, ds)
    }
    return got["v"]
}

func TestParseTextConvertsToTheDeclaredKind(t *testing.T) {
    origin := value.Origin{File: "--var"}
    for _, tc := range []struct {
        kind value.Kind
        text string
        want any
    }{
        {value.KindString, "20", "20"},
        {value.KindInt, "20", int64(20)},
        {value.KindFloat, "2.5", 2.5},
        {value.KindBool, "true", true},
    } {
        got, ds := schemaFor(t, tc.kind).ParseText(tc.text, origin)
        if ds.HasErrors() {
            t.Fatalf("%s: unexpected diagnostics: %+v", tc.kind, ds)
        }
        if got.Kind != tc.kind || got.Raw != tc.want {
            t.Errorf("%s: ParseText(%q) = %v/%v, want %v/%v", tc.kind, tc.text, got.Kind, got.Raw, tc.kind, tc.want)
        }
        if got.Source != value.SourceVariable || got.Scope != value.ScopeCLIOverride {
            t.Errorf("%s: Source/Scope = %v/%v, want SourceVariable/ScopeCLIOverride", tc.kind, got.Source, got.Scope)
        }
    }
}

func TestParseTextRejectsTextThatIsNotTheDeclaredKind(t *testing.T) {
    for _, tc := range []struct {
        kind value.Kind
        text string
    }{
        {value.KindInt, "many"},
        {value.KindInt, "2.5"},
        {value.KindFloat, "many"},
        {value.KindBool, "maybe"},
    } {
        if _, ds := schemaFor(t, tc.kind).ParseText(tc.text, value.Origin{File: "--var"}); !ds.HasErrors() {
            t.Errorf("--var for a %s variable given %q must be rejected", tc.kind, tc.text)
        }
    }
}

func TestParseTextRefusesListAndMapVariables(t *testing.T) {
    for _, k := range []value.Kind{value.KindList, value.KindMap} {
        if _, ds := schemaFor(t, k).ParseText("a,b,c", value.Origin{File: "--var"}); !ds.HasErrors() {
            t.Errorf("--var cannot supply a %s: parsing one needs a mini-language, and PLAN.md §9 forbids building one", k)
        }
    }
}

func TestParseTextLeavesAnUntypedDeclarationAsText(t *testing.T) {
    d := decl("anything", value.KindInvalid)
    d.Default, d.HasDefault = value.String("x", value.SourceExplicit), true
    got, _ := Schemas([]config.VariableDecl{d})

    v, ds := got["anything"].ParseText("20", value.Origin{File: "--var"})
    if ds.HasErrors() {
        t.Fatalf("an untyped declaration constrains nothing: %+v", ds)
    }
    if v.Kind != value.KindString {
        t.Errorf("Kind = %v, want KindString — guessing a type from the spelling would make `--var version=1.10` the float 1.1", v.Kind)
    }
}
```

### 4.14 Run it, see it fail

```bash
go test -count=1 -run TestParseText ./internal/variables/
```

Expect `s.ParseText undefined`.

### 4.15 Minimal code: `ParseText`

```go
// ParseText converts one command-line string to s's kind.
//
// `--var name=value` can only ever produce text, so a typed variable needs the
// text converted or `--var replicas=20` would fail its own integer schema. The
// conversion is the whole of the mini-language this system has, and it stops
// at scalars deliberately: `--var tags=a,b,c` would require a separator
// convention, an escape for the separator, and then a nesting syntax, which is
// the general-purpose language PLAN.md §9 forbids.
//
// An untyped declaration yields text unchanged. Guessing a type from the
// spelling — "20" becomes an integer, "true" a boolean — would make a
// variable's type depend on the value someone happened to pass, and
// `--var version=1.10` would silently become the float 1.1.
//
// Booleans go through strconv.ParseBool rather than a bespoke table, so the
// accepted spellings are Go's and documented rather than invented here.
func (s Schema) ParseText(text string, origin value.Origin) (value.Value, diag.Diagnostics) {
    var ds diag.Diagnostics
    stamp := func(v value.Value) value.Value {
        return v.WithScope(value.ScopeCLIOverride).WithOrigin(origin)
    }
    bad := func(expected string) (value.Value, diag.Diagnostics) {
        ds.Add(diag.Diagnostic{
            Severity: diag.SeverityError,
            Summary:  "--var " + s.Name + "=" + text + " is not " + article(s.Kind) + " " + s.Kind.String(),
            Detail:   strconv.Quote(s.Name) + " is declared as " + s.Kind.String() + " at " + s.Origin.String() + ". " + expected,
            Action:   "Correct the value passed to --var.",
            Origin:   origin,
        })
        return stamp(value.Unknown(s.Kind, value.SourceVariable)), ds
    }

    switch s.Kind {
    case value.KindInvalid, value.KindString:
        return stamp(value.String(text, value.SourceVariable)), ds
    case value.KindInt:
        n, err := strconv.ParseInt(text, 10, 64)
        if err != nil {
            return bad("Expected a whole number.")
        }
        return stamp(value.Int(n, value.SourceVariable)), ds
    case value.KindFloat:
        f, err := strconv.ParseFloat(text, 64)
        if err != nil {
            return bad("Expected a number.")
        }
        return stamp(value.Float(f, value.SourceVariable)), ds
    case value.KindBool:
        b, err := strconv.ParseBool(text)
        if err != nil {
            return bad("Expected true or false.")
        }
        return stamp(value.Bool(b, value.SourceVariable)), ds
    default:
        ds.Add(diag.Diagnostic{
            Severity: diag.SeverityError,
            Summary:  "variable " + strconv.Quote(s.Name) + " cannot be set with --var",
            Detail:   "It is declared as " + s.Kind.String() + " at " + s.Origin.String() + ", and --var carries a single line of text.",
            Action:   "Set " + strconv.Quote(s.Name) + " in variables.yml or in environments/<environment>.yml, where YAML can express " + article(s.Kind) + " " + s.Kind.String() + ".",
            Origin:   origin,
        })
        return stamp(value.Unknown(s.Kind, value.SourceVariable)), ds
    }
}
```

### 4.16 Run it, see it pass

```bash
go test -count=1 ./... && go vet ./... && gofmt -l .
```

Commit: `M4 task 4: typed variable schemas`.

---

## Task 5 — compiler stage 3: environment resolution

Spec §7 stage 3: "Walk the `extends` chain, detect cycles, produce an ordered scope
stack." `PLAN.md` §7 fixes what the stack is for.

## Files

- create `internal/environments/resolve.go`
- create `internal/environments/resolve_test.go`

## Interfaces

**Consumes:** `config.EnvironmentDecl` and `config.OverrideDecl` (Task 3),
`diag.Diagnostics`, `value.Value`, `value.Scope`, `value.Origin`.

**Produces** — Tasks 6 and 7 depend on exactly these:

```go
package environments

// Layer is one environment in a resolved extends chain.
type Layer struct {
    Name      string
    Overrides []config.OverrideDecl // stage 2's order, not re-sorted
    Scope     value.Scope
    Origin    value.Origin
}

// Chain is the ordered scope stack stage 3 produces.
type Chain struct {
    Name     string  // the environment named on the command line
    Selected bool    // false when no environment was named, or resolution failed
    Layers   []Layer // ancestors first; the named environment last
}

func Resolve(decls []config.EnvironmentDecl, name string) (Chain, diag.Diagnostics)
```

`Layer.Overrides` is `config.OverrideDecl` and not a map. Stage 2 already sorted
these by name and gave each one its own `Origin`, so a map here would throw away the
line number a diagnostic needs and force every consumer to re-sort what was already
ordered. M3 measured eleven sorts in this codebase whose only job was undoing Go's
randomised map iteration; this would have been the twelfth, on the path invariant 6
depends on.

## The rulings this task fixes, and why

1. **`Layers` runs ancestors first.** `production extends default` gives
   `[default, production]`. Later layers win, which is the direction the rest of the
   ladder runs in Task 6, so one loop over `Layers` in order applies precedence
   correctly with no reversal anywhere.

2. **Every layer but the last carries `ScopeEnvironmentInherit`; the last carries
   `ScopeEnvironmentVar`.** `PLAN.md` §7's chain has both "environment inheritance"
   and "environment variables", one above the other, and the only reading that makes
   them distinct is: a value inherited from an ancestor environment, versus a value
   set on the environment you actually asked for. Stage 3 assigns the scope because
   stage 3 is what knows which layer is which; Task 6 only reads it.

3. **A cycle or a missing environment produces `Chain{Selected: false}` with no
   layers.** CONTRACT, matching `compiler.Compile`'s existing rule: the returned
   `Chain` is meaningless if the returned diagnostics contain errors. A PARTIAL chain
   would be worse than an empty one — half an inheritance chain silently drops the
   overrides that make production production, and the result plans successfully.

4. **`Resolve(decls, "")` succeeds with `Selected: false` and no layers.**
   `infra validate` has no environment argument and calls `Compile` with the
   environment left empty (`internal/cli/validate.go`). Stage 3 must not fail there,
   and must not silently pick an environment either. Task 6 reads `Selected` to
   decide whether an unset variable is an error or an unknown.

5. **An unknown environment name is an error ONLY when the project declares
   environments at all.** M2 ships `infra plan dev` against projects with no
   environments block, and the integration suite depends on it. So: zero
   declarations plus any name ⇒ one synthetic layer named `name`, no overrides, no
   diagnostics, `Selected: true`. At least one declaration plus a name that is not
   among them ⇒ error listing the known names. A project that has not adopted
   environments still plans; a project that has adopted them and typed the name
   wrong gets told.

## Why `internal/graph` cannot serve here

Spec §7.4 says cycle detection appears three times and "is one shared algorithm
parameterized by an edge function". That is the right instinct and it does not fit
this call site. Three reasons, checked against the code rather than assumed:

- `Graph.Edge` **panics** when either endpoint was never added
  (`internal/graph/graph.go`), and its own doc comment states the rule: a graph "is
  always built by another infra package ... from addresses it already knows are
  valid, never from unvalidated configuration text a user typed." `extends:
  nonexistent` is exactly unvalidated configuration text. A graph-based stage 3 would
  have to check every `extends` target before adding a single edge — which is the
  whole walk, done first.
- `Graph.Cycle()` returns the first cycle **in the whole graph**, in sorted node
  order. `Resolve` is asked about one environment. A cycle between two environments
  that `name` never reaches would fail a healthy `plan production` and name innocent
  environments in the diagnostic.
- `extends` is **functional** — at most one parent — so the traversal IS the
  resolution: the ordered layer stack falls out of the same walk that detects the
  cycle. Building a graph would be a second traversal producing data the first
  already has.

What IS shared is the diagnostic shape: the cycle is rendered in participation order
with the wrap included, `a -> b -> a`, exactly as `Graph.Layers` renders it, so the
three cycle errors in this system read alike.

## Steps

### 5.1 Failing test: a three-deep chain resolves ancestors first, with the right scopes

The fixture's values contradict their natural order on purpose. `base` holds 30,
`middle` holds 20, `leaf` holds 10; sorted ascending, or by insertion, or by name,
the answer is different from the correct one. A fixture where the winner is also the
largest, or the alphabetically last, cannot distinguish a correct implementation
from an accident.

Write `internal/environments/resolve_test.go`:

```go
package environments

import (
    "strings"
    "testing"

    "infra/internal/config"
    "infra/pkg/value"
)

func override(name string, n int64) config.OverrideDecl {
    return config.OverrideDecl{
        Name:   name,
        Value:  value.Int(n, value.SourceEnvironment),
        Origin: value.Origin{File: "infra.yml", Line: 4, Column: 5},
    }
}

func env(name, extends string, overrides ...config.OverrideDecl) config.EnvironmentDecl {
    return config.EnvironmentDecl{
        Name:          name,
        Extends:       extends,
        ExtendsOrigin: value.Origin{File: "infra.yml", Line: 3, Column: 5},
        Overrides:     overrides,
        Origin:        value.Origin{File: "infra.yml", Line: 2, Column: 3},
    }
}

func TestResolveOrdersAncestorsFirst(t *testing.T) {
    decls := []config.EnvironmentDecl{
        env("base", "", override("replicas", 30)),
        env("middle", "base", override("replicas", 20)),
        env("leaf", "middle", override("replicas", 10)),
    }

    chain, ds := Resolve(decls, "leaf")
    if ds.HasErrors() {
        t.Fatalf("unexpected diagnostics: %+v", ds)
    }
    if !chain.Selected || chain.Name != "leaf" {
        t.Fatalf("Chain = {Name:%q Selected:%v}, want {leaf true}", chain.Name, chain.Selected)
    }

    var got []string
    for _, l := range chain.Layers {
        got = append(got, l.Name)
    }
    want := []string{"base", "middle", "leaf"}
    if strings.Join(got, ",") != strings.Join(want, ",") {
        t.Fatalf("layers = %v, want %v (ancestors first, so a single forward loop applies precedence)", got, want)
    }
}

func TestResolveMarksInheritedLayersDifferentlyFromTheSelectedOne(t *testing.T) {
    decls := []config.EnvironmentDecl{
        env("base", "", override("replicas", 30)),
        env("middle", "base", override("replicas", 20)),
        env("leaf", "middle", override("replicas", 10)),
    }
    chain, _ := Resolve(decls, "leaf")

    for i, l := range chain.Layers {
        want := value.ScopeEnvironmentInherit
        if i == len(chain.Layers)-1 {
            want = value.ScopeEnvironmentVar
        }
        if l.Scope != want {
            t.Errorf("layer %q scope = %v, want %v (PLAN.md §7 separates environment inheritance from environment variables; the named environment is the latter)", l.Name, l.Scope, want)
        }
    }
}

func TestResolveKeepsOverridesInStageTwosOrder(t *testing.T) {
    // Stage 2 sorted these and gave each its own Origin. Re-sorting here — or
    // routing them through a map and sorting on the way out — would be the
    // twelfth redundant sort in this codebase and would lose the origins.
    decls := []config.EnvironmentDecl{
        env("dev", "", override("alpha", 1), override("beta", 2)),
    }
    chain, _ := Resolve(decls, "dev")
    got := chain.Layers[0].Overrides
    if len(got) != 2 || got[0].Name != "alpha" || got[1].Name != "beta" {
        t.Fatalf("overrides = %+v, want alpha then beta, unchanged", got)
    }
    if got[0].Origin.Line == 0 {
        t.Error("each override must keep its own origin, or a diagnostic cannot point at the offending line")
    }
}
```

### 5.2 Run it, see it fail

```bash
export PATH="$HOME/.local/share/mise/shims:$PATH"
go test -count=1 ./internal/environments/
```

Expect a build failure: the package does not exist — `undefined: Resolve`.

### 5.3 Minimal code: the walk

Create `internal/environments/resolve.go`:

```go
// Package environments implements compiler stage 3: it walks an environment's
// `extends` chain and produces the ordered scope stack the variable resolver
// consumes (spec §7 stage 3; PLAN.md §7).
package environments

import (
    "strconv"
    "strings"

    "infra/internal/config"
    "infra/internal/diag"
    "infra/pkg/value"
)

// Layer is one environment in a resolved extends chain.
//
// Overrides are stage 2's slice, passed through untouched. Stage 2 already
// sorted them by name and gave each one its own Origin; a map here would throw
// away the line number a diagnostic needs and force every consumer to re-sort
// what was already ordered.
type Layer struct {
    Name      string
    Overrides []config.OverrideDecl
    Scope     value.Scope
    Origin    value.Origin
}

// Chain is the ordered scope stack stage 3 produces: ancestors first, the
// environment named on the command line last. Later layers win, which is the
// same direction the rest of the precedence ladder runs in, so stage 4 applies
// it with one forward loop and no reversal anywhere.
//
// CONTRACT: the returned Chain is meaningless if the returned diagnostics
// contain errors, and on failure it is deliberately EMPTY rather than partial.
// Half an inheritance chain is the dangerous shape: it drops exactly the
// overrides that distinguish production from the base environment, and then
// resolves and plans successfully.
type Chain struct {
    Name     string
    Selected bool
    Layers   []Layer
}

// Resolve walks name's extends chain.
//
// An empty name is not an error: `infra validate` has no environment argument
// (internal/cli/validate.go) and must still compile the project. It yields a
// Chain with Selected false and no layers, and stage 4 reads Selected to decide
// whether a variable that only an environment sets is an error or an unknown.
func Resolve(decls []config.EnvironmentDecl, name string) (Chain, diag.Diagnostics) {
    var ds diag.Diagnostics

    if name == "" {
        return Chain{}, ds
    }

    byName := make(map[string]config.EnvironmentDecl, len(decls))
    for _, d := range decls {
        byName[d.Name] = d
    }

    // A project that has not adopted environments at all still plans: M2 ships
    // `infra plan dev` against a bare infra.yml, and the integration suite
    // depends on it. Once a project DOES declare environments, a name that is
    // not among them is a typo worth reporting.
    if len(byName) == 0 {
        return Chain{
            Name:     name,
            Selected: true,
            Layers:   []Layer{{Name: name, Scope: value.ScopeEnvironmentVar}},
        }, ds
    }

    start, ok := byName[name]
    if !ok {
        ds.Add(diag.Diagnostic{
            Severity: diag.SeverityError,
            Summary:  "unknown environment " + strconv.Quote(name),
            Detail:   "Declared environments:\n  " + strings.Join(declaredNames(decls), "\n  "),
            Action:   "Use one of those, or declare " + strconv.Quote(name) + " under `environments:` or in environments/" + name + ".yml.",
        })
        return Chain{}, ds
    }

    // Walk parent-ward, recording where each name was seen so a repeat yields
    // the exact cycle rather than just its existence.
    var order []config.EnvironmentDecl
    seen := map[string]int{}
    current := start
    for {
        if at, dup := seen[current.Name]; dup {
            ds.Add(cycleDiagnostic(order[at:], current))
            return Chain{}, ds
        }
        seen[current.Name] = len(order)
        order = append(order, current)

        if current.Extends == "" {
            break
        }
        parent, ok := byName[current.Extends]
        if !ok {
            ds.Add(diag.Diagnostic{
                Severity: diag.SeverityError,
                Summary:  "environment " + strconv.Quote(current.Name) + " extends unknown environment " + strconv.Quote(current.Extends),
                Detail:   "Declared environments:\n  " + strings.Join(declaredNames(decls), "\n  "),
                Action:   "Correct `extends`, or declare " + strconv.Quote(current.Extends) + ".",
                Origin:   current.ExtendsOrigin,
            })
            return Chain{}, ds
        }
        current = parent
    }

    // order is leaf-first because the walk goes parent-ward; reverse it once,
    // here, so no consumer has to know that.
    layers := make([]Layer, 0, len(order))
    for i := len(order) - 1; i >= 0; i-- {
        d := order[i]
        scope := value.ScopeEnvironmentInherit
        if i == 0 {
            scope = value.ScopeEnvironmentVar
        }
        layers = append(layers, Layer{Name: d.Name, Overrides: d.Overrides, Scope: scope, Origin: d.Origin})
    }

    return Chain{Name: name, Selected: true, Layers: layers}, ds
}

// cycleDiagnostic renders a cycle in participation order with the wrap
// included — `a -> b -> a` — matching graph.Layers' rendering, so the three
// cycle errors in this system (environment extends, the module graph, the
// resource graph) read alike. Spec §7.4 requires the full cycle, not one
// participant: naming only one leaves the reader to find the rest by hand.
func cycleDiagnostic(cycle []config.EnvironmentDecl, repeat config.EnvironmentDecl) diag.Diagnostic {
    names := make([]string, 0, len(cycle)+1)
    for _, d := range cycle {
        names = append(names, d.Name)
    }
    names = append(names, repeat.Name)

    return diag.Diagnostic{
        Severity: diag.SeverityError,
        Summary:  "environment inheritance forms a cycle",
        Detail:   strings.Join(names, " -> ") + "\n\nEach environment `extends` the next, so none of them has a base to inherit from.",
        Action:   "Remove one `extends` so the chain ends at an environment that has none.",
        Origin:   cycle[0].ExtendsOrigin,
    }
}

// declaredNames lists environment names for a diagnostic, reading them from
// the declaration SLICE rather than the lookup map. ProjectDecl.Environments
// is already sorted by name (stage 2), so there is nothing to sort; ranging
// the map instead would reorder the message between identical runs and need a
// sort to undo it.
func declaredNames(decls []config.EnvironmentDecl) []string {
    out := make([]string, 0, len(decls))
    for _, d := range decls {
        out = append(out, d.Name)
    }
    return out
}
```

Note the walk goes leaf-to-root and reverses once. Do not "simplify" it into a
root-to-leaf walk: you do not know the root until you have found it, and the cycle
would then be detected in a chain you cannot enter.

### 5.4 Run it, see it pass

```bash
go test -count=1 ./internal/environments/
```

Commit: `M4 task 5: environment extends chain`.

### 5.5 Failing test: the four failure shapes, and the two that must NOT fail

```go
func TestResolveRejectsSelfExtends(t *testing.T) {
    chain, ds := Resolve([]config.EnvironmentDecl{env("dev", "dev")}, "dev")
    if !ds.HasErrors() {
        t.Fatal("`dev: {extends: dev}` is a one-node cycle and must be reported, not walked forever")
    }
    if chain.Selected || len(chain.Layers) != 0 {
        t.Errorf("a failed Resolve must return an EMPTY chain, not a partial one: %+v", chain)
    }
    var sb strings.Builder
    ds.Render(&sb)
    if !strings.Contains(sb.String(), "dev -> dev") {
        t.Errorf("the diagnostic must render the full cycle including the wrap:\n%s", sb.String())
    }
}

func TestResolveRejectsATwoCycle(t *testing.T) {
    decls := []config.EnvironmentDecl{env("a", "b"), env("b", "a")}
    _, ds := Resolve(decls, "a")
    if !ds.HasErrors() {
        t.Fatal("a extends b extends a must be reported")
    }
    var sb strings.Builder
    ds.Render(&sb)
    if !strings.Contains(sb.String(), "a -> b -> a") {
        t.Errorf("the diagnostic must name every participant in order:\n%s", sb.String())
    }
}

func TestResolveRejectsExtendsOfAnUndeclaredEnvironment(t *testing.T) {
    decls := []config.EnvironmentDecl{env("dev", "shared"), env("production", "")}
    _, ds := Resolve(decls, "dev")
    if !ds.HasErrors() {
        t.Fatal("`extends: shared` with no `shared` declared must be reported")
    }
    var sb strings.Builder
    ds.Render(&sb)
    for _, want := range []string{"shared", "production", "infra.yml:3:5"} {
        if !strings.Contains(sb.String(), want) {
            t.Errorf("the diagnostic must name the missing environment, list the ones that exist, and point at the `extends` line; %q missing:\n%s", want, sb.String())
        }
    }
}

func TestResolveRejectsAnUnknownEnvironmentName(t *testing.T) {
    _, ds := Resolve([]config.EnvironmentDecl{env("production", "")}, "prod")
    if !ds.HasErrors() {
        t.Fatal("`infra plan prod` against a project declaring only `production` must be reported")
    }
    var sb strings.Builder
    ds.Render(&sb)
    if !strings.Contains(sb.String(), "production") {
        t.Errorf("the diagnostic must list the environments that DO exist, so the typo is visible:\n%s", sb.String())
    }
}

func TestResolveAcceptsAnyNameWhenNoEnvironmentsAreDeclared(t *testing.T) {
    // M2 plans projects with no environments block at all, and the integration
    // suite depends on it. Adopting environments is what turns a name into a
    // checkable claim.
    chain, ds := Resolve(nil, "dev")
    if ds.HasErrors() {
        t.Fatalf("a project with no environments must still plan: %+v", ds)
    }
    if !chain.Selected || len(chain.Layers) != 1 || chain.Layers[0].Name != "dev" {
        t.Fatalf("chain = %+v, want one synthetic layer named dev", chain)
    }
}

func TestResolveWithNoEnvironmentNameSelectsNothing(t *testing.T) {
    // `infra validate` compiles with the environment left empty.
    chain, ds := Resolve([]config.EnvironmentDecl{env("production", "")}, "")
    if ds.HasErrors() {
        t.Fatalf("`infra validate` has no environment argument and must not fail here: %+v", ds)
    }
    if chain.Selected || len(chain.Layers) != 0 {
        t.Fatalf("chain = %+v, want an unselected, empty chain", chain)
    }
}
```

### 5.6 Run it, see it fail or pass, and check WHICH

```bash
go test -count=1 ./internal/environments/
```

Written against the code in 5.3 these should already pass — the walk was written with
them in mind. **Run them against a deliberately broken copy before trusting them.**
Temporarily delete the `if at, dup := seen[current.Name]; dup` block and re-run: the
two cycle tests must hang or fail, not pass. Restore it. This is the check M3 skipped:
invariant 4's test once passed 20 times out of 20 with its dependency edge deleted.

### 5.7 Commit

```bash
go test -count=1 ./... && go vet ./... && gofmt -l .
```

Commit: `M4 task 5: environment resolution diagnostics`.

---

## Task 6 — compiler stage 4: variable resolution

Spec §7 stage 4, and §7.1: "Stages 3 and 4 build an ordered scope stack in exactly
that order. A resolved value's `Source` is simply a record of which scope won. There
is one implementation, so what a plan claims about a value's origin cannot drift away
from the precedence rule that produced it."

The contract refines that: `Source` keeps the seven meanings the spec fixes, and the
new `Scope` field records which rung won. Both are recorded here, in one pass, by one
loop.

## Files

- create `internal/variables/resolve.go`
- create `internal/variables/resolve_test.go`

## Interfaces

**Consumes:** `Schema`, `Schemas`, `Schema.Validate`, `Schema.ParseText` (Task 4);
`environments.Chain` and `environments.Layer` (Task 5); `config.VariableDecl`
(Task 3); `value.Scope`, `value.Value.WithScope` (Task 1).

**Produces** — Task 7 and the plan renderer depend on exactly these:

```go
package variables

// Scope is the resolved variable scope: every variable name mapped to the
// value that won the precedence chain.
type Scope struct {
    vars map[string]value.Value
}

// Variable satisfies half of expressions.Scope.
func (s Scope) Variable(name string) (value.Value, bool)

// Names lists every resolved variable, sorted.
func (s Scope) Names() []string

// Override records a value that comes from the process invocation rather than
// from any configuration file.
func (s *Scope) Override(name string, v value.Value)

func Resolve(decls []config.VariableDecl, chain environments.Chain,
             files map[string]value.Value, cliVars map[string]string,
) (Scope, diag.Diagnostics)
```

**What feeds each argument** — this is not inferable from the signature, and Task 7
is the only caller:

| Argument | Fed by |
|---|---|
| `decls` | `config.ProjectDecl.Variables` — the `variables:` block's typed declarations |
| `chain` | `environments.Resolve(project.Environments, opts.Environment)` |
| `files` | `config.ProjectDecl.VariableValues` (variables.yml), merged under `compiler.Options.FileVars` (`--var-file`) |
| `cliVars` | `compiler.Options.Vars` — `--var name=value` |

`VariableValues` and `FileVars` are deliberately two fields and not one:
`VariableValues` is a decoded project file, `FileVars` is a command-line input. Task 7
merges them because they sit at the same rung; they stay distinct upstream because
`--var-file` and `variables.yml` are exactly what a user needs a plan to be able to
tell apart.

## The ladder, as this task implements it

Lowest first. The LAST writer wins, so the loop runs in this order and never
reverses.

| Rung | `value.Scope` | Source of entries | `ValueSource` recorded |
|---|---|---|---|
| 1 | `ScopeBaseConfig` | a schema's `default:` | `SourceDefault` |
| 2 | `ScopeBaseConfig` | `files` — variables.yml, then `--var-file` | `SourceVariable` |
| 3 | `ScopeModuleDefault` | — M5 fills this in | — |
| 4 | `ScopeEnvironmentInherit` | `chain.Layers` except the last | `SourceEnvironment` |
| 5 | `ScopeEnvironmentVar` | `chain.Layers`' last layer | `SourceEnvironment` |
| 6 | `ScopeCLIOverride` | `cliVars` (`--var`) | `SourceVariable` |

Rungs 4 and 5 are not applied by this task's own logic — `environments.Chain` already
stamps each layer with its scope (Task 5), and this task walks `chain.Layers` in
order and copies it. That is the single-implementation property §7.1 asks for: the
scope a value reports IS the rung that supplied it, not a parallel judgement about it.

`ScopeProviderDefault` is not used here. Provider defaults apply to resource
attributes in stage 7, never to variables.

**Rungs 1 and 2 share a `value.Scope` on purpose.** A variable's `default:` is
written in the same base configuration as a plain `variables.yml` entry, so they sit
at the same precedence LEVEL; what separates them is `Source`, which already says one
is a default and the other is not. Within the rung, the explicit entry wins —
`PLAN.md` §7: "Explicit user configuration always overrides an implicit default."
This is why rung 2 runs after rung 1 rather than being merged with it.

**The decision the contract records lands here.** `--var replicas=20` and a
`variables.yml` entry `replicas: 20` must produce values that are `Equal`, carry the
SAME `Source` (`SourceVariable`), and carry DIFFERENT `Scope`
(`ScopeCLIOverride` vs `ScopeBaseConfig`). Step 6.1 tests exactly that.

**Every winning value is stamped here even when stage 2 already tagged it.** Stage 2
tags an environment override `SourceEnvironment` when it decodes it, and this
re-stamps it to the same thing. That is deliberate, not redundant: stage 4 is the one
place that decides what provenance a winning value carries, and a value whose `Source`
was set somewhere else and merely survived is a value nobody is responsible for.

## The unset-variable ruling, and why it depends on `chain.Selected`

A declared variable with no default and no supplied value:

- **When an environment IS selected** (`infra plan production`) it is an ERROR.
  Everything that could set it has been consulted, so there is a definite answer and
  it is "nothing set this".
- **When no environment is selected** (`infra validate`, which has no environment
  argument) it resolves to `value.Unknown(schema.Kind, SourceVariable)` with
  `ScopeUnset`, and no diagnostic. `infra validate` genuinely cannot know what
  production sets, and an unknown is the machinery the engine already has for "typed
  but not yet determined" (spec §5.1, §6) — downstream kind checks still work and
  nothing pretends to an answer it does not have. **Erroring here instead would make
  `infra validate` reject configuration that `infra plan production` plans perfectly
  well**, which is the failure mode that matters: a validate command users learn to
  ignore is worse than no validate command.

This is precisely `PLAN.md` §9's "Validation must happen during `infra validate` AND
before planning" being two different checks at two different times: `validate` checks
shape, `plan <env>` additionally checks presence.

Stage 2 guarantees the `Kind` this needs: a declaration with neither a `type` nor a
`default` is a decode-time error, so every schema reaching here either has a real
`Kind` to build an unknown from, or has a default — in which case rung 1 always sets
it and the unset branch is never reached. Nothing is dropped anywhere along the way
to produce that property; it is bought by an error at the line the user wrote.

The error fires whether or not any configuration references the variable. Making it
depend on use would mean `infra validate`'s output changed when an unrelated file
stopped mentioning a name, which is not a property anyone can reason about.

## Steps

### 6.1 Failing test: the full ladder, with values that contradict their natural order

The single most important test in this task. Every rung supplies a value for the SAME
variable, and the values descend as the rungs ascend: the winner is the SMALLEST
number. A fixture where the top of the ladder also happens to be the maximum cannot
distinguish a correct implementation from `max(candidates)`, and one with two rungs
cannot distinguish it from "return the last thing I looked at".

Write `internal/variables/resolve_test.go`:

```go
package variables

import (
    "strings"
    "testing"

    "infra/internal/config"
    "infra/internal/environments"
    "infra/pkg/value"
)

func envOverride(name string, n int64) config.OverrideDecl {
    return config.OverrideDecl{Name: name, Value: value.Int(n, value.SourceEnvironment)}
}

// ladderInputs builds a chain and inputs where EVERY rung sets `replicas`, and
// the values descend as precedence ascends.
func ladderInputs() (decls []config.VariableDecl, chain environments.Chain, files map[string]value.Value, cli map[string]string) {
    d := config.VariableDecl{
        Name:       "replicas",
        Type:       value.KindInt,
        Default:    value.Int(50, value.SourceExplicit),
        HasDefault: true,
        Origin:     value.Origin{File: "variables.yml", Line: 2, Column: 3},
    }
    envDecls := []config.EnvironmentDecl{
        {Name: "base", Overrides: []config.OverrideDecl{envOverride("replicas", 30)}},
        {Name: "production", Extends: "base", Overrides: []config.OverrideDecl{envOverride("replicas", 20)}},
    }
    chain, _ = environments.Resolve(envDecls, "production")
    return []config.VariableDecl{d},
        chain,
        map[string]value.Value{"replicas": value.Int(40, value.SourceVariable)},
        map[string]string{"replicas": "10"}
}

func TestResolveWalksTheWholePrecedenceLadder(t *testing.T) {
    decls, chain, files, cli := ladderInputs()

    // Each step removes the winning rung, so the expected answer walks DOWN the
    // ladder one rung at a time. An implementation that always returns the last
    // entry it saw, or the largest, fails at the first step it does not happen
    // to match.
    for _, tc := range []struct {
        name  string
        files map[string]value.Value
        cli   map[string]string
        chain environments.Chain
        want  int64
        scope value.Scope
    }{
        {"--var wins", files, cli, chain, 10, value.ScopeCLIOverride},
        {"environment wins", files, nil, chain, 20, value.ScopeEnvironmentVar},
        {"inherited environment wins", files, nil, trimLast(chain), 30, value.ScopeEnvironmentInherit},
        {"variables.yml wins", files, nil, bare(chain), 40, value.ScopeBaseConfig},
        {"declared default is the floor", nil, nil, bare(chain), 50, value.ScopeBaseConfig},
    } {
        t.Run(tc.name, func(t *testing.T) {
            scope, ds := Resolve(decls, tc.chain, tc.files, tc.cli)
            if ds.HasErrors() {
                t.Fatalf("unexpected diagnostics: %+v", ds)
            }
            got, ok := scope.Variable("replicas")
            if !ok {
                t.Fatal("replicas did not resolve at all")
            }
            if n, _ := got.AsInt(); n != tc.want {
                t.Errorf("replicas = %d, want %d", n, tc.want)
            }
            if got.Scope != tc.scope {
                t.Errorf("Scope = %v, want %v", got.Scope, tc.scope)
            }
        })
    }
}

// trimLast drops the selected layer, leaving the inherited one as the top of
// the environment part of the ladder. It re-stamps nothing: the remaining
// layer keeps the ScopeEnvironmentInherit stage 3 gave it, which is what the
// test is checking travels through unchanged.
func trimLast(c environments.Chain) environments.Chain {
    out := c
    out.Layers = append([]environments.Layer(nil), c.Layers[:len(c.Layers)-1]...)
    return out
}

// bare keeps the chain selected but removes every layer, so the environment
// rungs contribute nothing.
func bare(c environments.Chain) environments.Chain {
    return environments.Chain{Name: c.Name, Selected: true}
}

func TestResolveRecordsSourceSeparatelyFromScope(t *testing.T) {
    decls, chain, _, _ := ladderInputs()

    fromFile, ds := Resolve(decls, bare(chain), map[string]value.Value{"replicas": value.Int(20, value.SourceVariable)}, nil)
    if ds.HasErrors() {
        t.Fatalf("file: %+v", ds)
    }
    fromFlag, ds := Resolve(decls, bare(chain), nil, map[string]string{"replicas": "20"})
    if ds.HasErrors() {
        t.Fatalf("flag: %+v", ds)
    }

    a, _ := fromFile.Variable("replicas")
    b, _ := fromFlag.Variable("replicas")

    if !a.Equal(b) {
        t.Error("the same datum from two scopes must be Equal — value.Equal ignores provenance, and acceptance invariant 2 (no-op plan) depends on it")
    }
    if a.Source != value.SourceVariable || b.Source != value.SourceVariable {
        t.Errorf("Source = %v and %v, want SourceVariable for both: Source says WHAT KIND of thing a value is, and both of these are variables", a.Source, b.Source)
    }
    if a.Scope != value.ScopeBaseConfig || b.Scope != value.ScopeCLIOverride {
        t.Errorf("Scope = %v and %v, want ScopeBaseConfig and ScopeCLIOverride: Scope says WHICH RUNG won, and a plan that cannot tell a file from a flag cannot explain itself", a.Scope, b.Scope)
    }
}

func TestResolveLetsAnExplicitEntryBeatItsOwnDeclaredDefault(t *testing.T) {
    // PLAN.md §7: "Explicit user configuration always overrides an implicit
    // default." Both sit at ScopeBaseConfig; Source is what separates them.
    decls, _, _, _ := ladderInputs()
    scope, ds := Resolve(decls, environments.Chain{}, map[string]value.Value{"replicas": value.Int(40, value.SourceVariable)}, nil)
    if ds.HasErrors() {
        t.Fatalf("unexpected diagnostics: %+v", ds)
    }
    got, _ := scope.Variable("replicas")
    if n, _ := got.AsInt(); n != 40 {
        t.Errorf("replicas = %d, want the explicit 40 rather than the declared default 50", n)
    }
    if got.Source != value.SourceVariable {
        t.Errorf("Source = %v, want SourceVariable: it is no longer a default", got.Source)
    }
}

func TestResolveStampsTheDeclaredDefaultWhenItWins(t *testing.T) {
    // Stage 2 leaves Default's provenance unset on purpose; stage 4 is the
    // only place that says which rung won.
    decls, _, _, _ := ladderInputs()
    scope, _ := Resolve(decls, environments.Chain{}, nil, nil)
    got, _ := scope.Variable("replicas")
    if got.Source != value.SourceDefault || got.Scope != value.ScopeBaseConfig {
        t.Errorf("Source/Scope = %v/%v, want SourceDefault/ScopeBaseConfig", got.Source, got.Scope)
    }
}
```

### 6.2 Run it, see it fail

```bash
go test -count=1 ./internal/variables/
```

Expect a build failure: `undefined: Resolve` and `scope.Variable undefined`.

### 6.3 Minimal code: the ladder

Create `internal/variables/resolve.go`:

```go
package variables

import (
    "sort"
    "strconv"

    "infra/internal/config"
    "infra/internal/diag"
    "infra/internal/environments"
    "infra/pkg/value"
)

// Scope is the resolved variable scope: every variable name mapped to the
// value that won the precedence chain (PLAN.md §7, spec §7.1).
//
// It is the ONLY variable scope in the engine. internal/compiler used to build
// a second one (variableScope, deleted in task 7); two implementations of one
// concept is the defect that leaked a plaintext secret in M2, because a fix
// applied to one copy left the other one wrong.
type Scope struct {
    vars map[string]value.Value
}

// Variable resolves a variable by name. It satisfies half of
// expressions.Scope, which is how compiler stage 6 reaches these values.
func (s Scope) Variable(name string) (value.Value, bool) {
    v, ok := s.vars[name]
    return v, ok
}

// Names lists every resolved variable, sorted, for diagnostics that suggest
// what the user might have meant. Go's map iteration is randomised and such a
// list must not reorder itself between runs of the same configuration.
func (s Scope) Names() []string {
    out := make([]string, 0, len(s.vars))
    for name := range s.vars {
        out = append(out, name)
    }
    sort.Strings(out)
    return out
}

// Override records a value that comes from the process invocation rather than
// from any configuration file.
//
// This is NOT a second precedence ladder. It exists for exactly three fixed
// names — `environment`, `region`, `account` — whose values are not written in
// any file and which must be authoritative: a ${environment} that disagreed
// with the environment being planned would make every diagnostic and every
// resource name that interpolates it lie about which environment it belongs
// to. See task 7, which is the only caller.
func (s *Scope) Override(name string, v value.Value) {
    if s.vars == nil {
        s.vars = map[string]value.Value{}
    }
    s.vars[name] = v
}

// Resolve is compiler stage 4. It builds the ordered scope stack in exactly
// PLAN.md §7's order and resolves each variable to the entry that wins,
// recording both what KIND of thing the value is (Source) and WHICH RUNG
// supplied it (Scope).
//
// The rungs, lowest first — the last writer wins, and the loop never reverses:
//
//	ScopeBaseConfig          a schema's `default:`            SourceDefault
//	ScopeBaseConfig          variables.yml and --var-file     SourceVariable
//	ScopeModuleDefault       — M5 fills this in
//	ScopeEnvironmentInherit  inherited environment layers     SourceEnvironment
//	ScopeEnvironmentVar      the selected environment         SourceEnvironment
//	ScopeCLIOverride         --var                            SourceVariable
//
// The environment rungs are not judged here: environments.Chain already stamps
// each layer with the scope it represents, and this walks the layers in order
// and copies it. That is what spec §7.1 means by one implementation — the
// scope a value reports IS the rung that supplied it, never a parallel opinion
// about it that could drift.
//
// Every winning value is stamped here even when stage 2 already tagged it the
// same way. Stage 4 is the one place that decides what provenance a winning
// value carries; a Source that was set elsewhere and merely survived is a
// Source nobody is responsible for.
//
// ScopeProviderDefault is absent deliberately: provider defaults apply to
// resource attributes in stage 7 and never to variables.
//
// Every problem is reported in one pass (spec §7.4); a variable that fails
// validation keeps its winning value so later stages see a value of the right
// shape rather than a hole, and the diagnostics are what make the compile fail.
func Resolve(decls []config.VariableDecl, chain environments.Chain,
    files map[string]value.Value, cliVars map[string]string,
) (Scope, diag.Diagnostics) {
    var ds diag.Diagnostics

    schemas, schemaDiags := Schemas(decls)
    ds.Extend(schemaDiags)

    out := Scope{vars: make(map[string]value.Value, len(schemas)+len(files)+len(cliVars))}

    // Rung 1: declared defaults.
    for _, name := range sortedSchemaNames(schemas) {
        if s := schemas[name]; s.HasDefault {
            out.vars[name] = s.Default.
                WithSource(value.SourceDefault).
                WithScope(value.ScopeBaseConfig)
        }
    }

    // Rung 2: variables.yml and --var-file, already merged by the caller.
    // Explicit configuration beats the implicit default rung 1 just wrote
    // (PLAN.md §7's closing line), which is why this is a separate pass at the
    // same scope rather than merged with it.
    for _, name := range sortedValueNames(files) {
        out.vars[name] = files[name].
            WithSource(value.SourceVariable).
            WithScope(value.ScopeBaseConfig)
    }

    // Rung 3 (ScopeModuleDefault) is M5's. The constant exists so M5 inserts a
    // pass here rather than renumbering the whole ladder.

    // Rungs 4 and 5: the environment chain, ancestors first. Overrides are
    // walked in the slice order stage 2 built and stage 3 preserved — nothing
    // is sorted here, because nothing here is a map.
    for _, layer := range chain.Layers {
        for _, o := range layer.Overrides {
            out.vars[o.Name] = o.Value.
                WithSource(value.SourceEnvironment).
                WithScope(layer.Scope)
        }
    }

    // Rung 6: --var.
    for _, name := range sortedTextNames(cliVars) {
        origin := value.Origin{File: "--var"}
        if s, declared := schemas[name]; declared {
            v, parseDiags := s.ParseText(cliVars[name], origin)
            ds.Extend(parseDiags)
            out.vars[name] = v
            continue
        }
        // An undeclared variable is untyped, and --var carries text, so text
        // is what it is. See Schema.ParseText for why no type is guessed.
        out.vars[name] = value.String(cliVars[name], value.SourceVariable).
            WithScope(value.ScopeCLIOverride).WithOrigin(origin)
    }

    ds.Extend(checkAgainstSchemas(schemas, &out, chain))
    return out, ds
}

func sortedSchemaNames(m map[string]Schema) []string {
    out := make([]string, 0, len(m))
    for name := range m {
        out = append(out, name)
    }
    sort.Strings(out)
    return out
}

func sortedValueNames(m map[string]value.Value) []string {
    out := make([]string, 0, len(m))
    for name := range m {
        out = append(out, name)
    }
    sort.Strings(out)
    return out
}

func sortedTextNames(m map[string]string) []string {
    out := make([]string, 0, len(m))
    for name := range m {
        out = append(out, name)
    }
    sort.Strings(out)
    return out
}
```

The three sorts exist because their inputs are genuinely maps — `files` and `cliVars`
arrive as maps from the CLI, and the schema table is keyed by name. Within one rung
the order cannot change the winner, since each name is written once; what it orders
is the sequence of DIAGNOSTICS emitted while walking the rung, and spec §7.4's
collected diagnostics must not reorder themselves between runs of the same
configuration. The environment rung has no sort because stage 2 handed it a slice
that was already ordered — do not add one.

### 6.4 Failing test: unset variables, both sides of `Selected`

```go
func TestResolveReportsAnUnsetVariableWhenAnEnvironmentIsSelected(t *testing.T) {
    decls := []config.VariableDecl{{
        Name:   "domain",
        Type:   value.KindString,
        Origin: value.Origin{File: "variables.yml", Line: 4, Column: 3},
    }}
    chain, _ := environments.Resolve([]config.EnvironmentDecl{{Name: "production"}}, "production")

    _, ds := Resolve(decls, chain, nil, nil)
    if !ds.HasErrors() {
        t.Fatal("`infra plan production` has consulted everything that could set `domain`; nothing did, so this is a definite error")
    }
    var sb strings.Builder
    ds.Render(&sb)
    for _, want := range []string{"domain", "production", "--var"} {
        if !strings.Contains(sb.String(), want) {
            t.Errorf("the diagnostic must name the variable, the environment, and a way to set it; %q missing:\n%s", want, sb.String())
        }
    }
}

func TestResolveLeavesAnUnsetVariableUnknownWhenNoEnvironmentIsSelected(t *testing.T) {
    // `infra validate` has no environment argument, so it cannot know what
    // production sets. An unknown carries the declared Kind, so kind checks
    // downstream still work; an error here would fail configuration that plans
    // perfectly well, and a validate command users learn to ignore is worse
    // than no validate command.
    decls := []config.VariableDecl{{Name: "domain", Type: value.KindString}}

    scope, ds := Resolve(decls, environments.Chain{}, nil, nil)
    if ds.HasErrors() {
        t.Fatalf("validate must not fail on a variable only an environment sets: %+v", ds)
    }
    got, ok := scope.Variable("domain")
    if !ok {
        t.Fatal("the variable must still be DEFINED, or stage 6 reports `undefined variable` instead")
    }
    if got.Known {
        t.Error("it must be unknown: there is no value, and inventing an empty string would be a confident wrong answer")
    }
    if got.Kind != value.KindString {
        t.Errorf("Kind = %v, want KindString: an unknown still carries its declared type", got.Kind)
    }
    if got.Scope != value.ScopeUnset {
        t.Errorf("Scope = %v, want ScopeUnset: no rung supplied it", got.Scope)
    }
}

func TestResolveValidatesTheWinningValueAgainstItsSchema(t *testing.T) {
    decls := []config.VariableDecl{{
        Name: "replicas", Type: value.KindInt,
        Min: value.Int(1, value.SourceExplicit), HasMin: true,
        Max: value.Int(100, value.SourceExplicit), HasMax: true,
        Origin: value.Origin{File: "variables.yml", Line: 2, Column: 3},
    }}
    chain, _ := environments.Resolve([]config.EnvironmentDecl{{Name: "dev"}}, "dev")

    if _, ds := Resolve(decls, chain, nil, map[string]string{"replicas": "500"}); !ds.HasErrors() {
        t.Error("--var replicas=500 violates max: 100 and must be reported")
    }
    if _, ds := Resolve(decls, chain, nil, map[string]string{"replicas": "50"}); ds.HasErrors() {
        t.Errorf("--var replicas=50 is inside the declared range and must be accepted: %+v", ds)
    }
}

func TestResolveValidatesAnEnvironmentOverrideToo(t *testing.T) {
    // The check is on the WINNING value, whichever rung it came from — not on
    // --var alone.
    decls := []config.VariableDecl{{
        Name: "replicas", Type: value.KindInt,
        Max: value.Int(100, value.SourceExplicit), HasMax: true,
        Origin: value.Origin{File: "variables.yml", Line: 2, Column: 3},
    }}
    chain, _ := environments.Resolve([]config.EnvironmentDecl{
        {Name: "production", Overrides: []config.OverrideDecl{envOverride("replicas", 500)}},
    }, "production")

    if _, ds := Resolve(decls, chain, nil, nil); !ds.HasErrors() {
        t.Error("an environment override outside the declared range must be reported")
    }
}

func TestResolveKeepsAnUndeclaredVariable(t *testing.T) {
    // A variable need not be declared at all: PLAN.md §9 says schemas are
    // OPTIONAL. An undeclared name is untyped and unconstrained.
    scope, ds := Resolve(nil, environments.Chain{}, map[string]value.Value{
        "domain": value.String("example.com", value.SourceVariable),
    }, nil)
    if ds.HasErrors() {
        t.Fatalf("an undeclared variable is not an error: %+v", ds)
    }
    if v, ok := scope.Variable("domain"); !ok {
        t.Fatal("domain must resolve")
    } else if s, _ := v.AsString(); s != "example.com" {
        t.Errorf("domain = %q, want example.com", s)
    }
}
```

### 6.5 Run it, see it fail

```bash
go test -count=1 -run TestResolve ./internal/variables/
```

Expect `undefined: checkAgainstSchemas` — a build failure across the package.

### 6.6 Minimal code: `checkAgainstSchemas`

```go
// checkAgainstSchemas validates every declared variable's winning value, and
// decides what an unset variable means.
//
// The meaning depends on whether an environment was selected, and this is the
// only place in the engine that distinguishes the two:
//
//   - An environment IS selected (`infra plan production`): every rung that
//     could set the variable has been consulted, so "nothing set it" is a
//     definite answer and an error.
//   - No environment is selected (`infra validate`, which takes no environment
//     argument): the variable may well be set by an environment this run never
//     looked at. It resolves to an unknown of the declared kind, which is the
//     engine's existing machinery for "typed but not yet determined" (spec
//     §5.1). Downstream kind checks still work, and nothing invents a value.
//     Erroring instead would make `infra validate` reject configuration that
//     `infra plan production` plans perfectly well.
//
// This is PLAN.md §9's "validation must happen during `infra validate` AND
// before planning" being two checks at two times: validate checks shape,
// plan additionally checks presence.
//
// Stage 2 guarantees the Kind this needs: a declaration with neither a type
// nor a default is a decode-time error, so an entry reaching the unset branch
// always has a real Kind to build an unknown from. An untyped declaration is
// legal but necessarily has a default, so rung 1 set it and it never reaches
// here.
func checkAgainstSchemas(schemas map[string]Schema, out *Scope, chain environments.Chain) diag.Diagnostics {
    var ds diag.Diagnostics

    for _, name := range sortedSchemaNames(schemas) {
        s := schemas[name]
        v, set := out.vars[name]
        if set {
            ds.Extend(s.Validate(v))
            continue
        }

        if !chain.Selected {
            out.vars[name] = value.Unknown(s.Kind, value.SourceVariable).WithOrigin(s.Origin)
            continue
        }

        ds.Add(diag.Diagnostic{
            Severity: diag.SeverityError,
            Summary:  "variable " + strconv.Quote(name) + " is not set",
            Detail: strconv.Quote(name) + " is declared at " + s.Origin.String() +
                " with no `default`, and nothing set it while resolving environment " +
                strconv.Quote(chain.Name) + ".",
            Action: "Give it a `default`, set it in variables.yml or environments/" +
                chain.Name + ".yml, or pass --var " + name + "=<value>.",
            Origin: s.Origin,
        })
        // Left absent rather than filled with a poison value: stage 6 will
        // report `undefined variable` at each USE SITE, which tells the user
        // where the missing value is needed. Both diagnostics are useful and
        // spec §7.4 collects rather than choosing between them.
    }
    return ds
}
```

`value.Unknown(...)` here does not call `WithScope`: `ScopeUnset` is the zero value,
and stamping it explicitly would suggest a rung had been consulted.

### 6.7 Run it, see it pass

```bash
go test -count=1 ./internal/variables/
```

### 6.8 Prove the ladder test can fail

Do not skip this. A precedence test that only ever checks the top of the chain cannot
tell a correct implementation from one that always returns the last entry it saw.

Temporarily reorder `Resolve` so the `--var` rung runs FIRST instead of last, and
re-run:

```bash
go test -count=1 -run TestResolveWalksTheWholePrecedenceLadder ./internal/variables/
```

The `--var wins` subtest must fail (`replicas = 20, want 10`). Restore the order.
Then temporarily delete the rung-2 (`files`) pass and re-run: the `variables.yml
wins` subtest must fail with `replicas = 50, want 40`. Restore it. Finally, change
the environment rung to walk `chain.Layers` in reverse: the `environment wins`
subtest must fail with `replicas = 30, want 20`. Restore it. If any sabotage passes,
the fixture is not discriminating and must be fixed before you go on.

### 6.9 Commit

```bash
go test -count=1 ./... && go vet ./... && gofmt -l .
```

Commit: `M4 task 6: variable precedence resolution`.

---

## Task 7 — wire stages 3 and 4 into `Compile`, deleting `variableScope`

`internal/compiler/bind.go`'s `variableScope(opts)` is REPLACED, not supplemented.
Two implementations of one concept is the defect that leaked a plaintext secret in
M2: a fix applied to one copy left the other wrong, and nothing pointed at the second
copy.

## Files

- modify `internal/compiler/compile.go`
- modify `internal/compiler/bind.go`
- modify `internal/compiler/resolved.go` (`Options`)
- modify `internal/compiler/bind_test.go`
- modify `internal/compiler/compile_test.go`
- create `tests/integration/m4_variables_test.go`

## Interfaces

**Consumes:** `environments.Resolve` (Task 5), `variables.Resolve`,
`variables.Scope`, `variables.Scope.Override` (Task 6), `config.ProjectDecl`'s new
`Variables`, `Environments` and `VariableValues` fields (Task 3).

**Produces:**

```go
package compiler

// Options gains one field; the rest are unchanged.
type Options struct {
    Environment string
    Region      string
    Account     string
    Vars        map[string]string      // from --var
    FileVars    map[string]value.Value // from --var-file
}
```

`FileVars` is the seam `--var-file` plugs into (a later task). It is defined here so
that task has a name to fill in rather than inventing a second path into the ladder.
Until then, `internal/cli/plan.go` still errors on `--var-file` and leaves it nil.

`Options.FileVars` and `config.ProjectDecl.VariableValues` are two fields and not
one. `VariableValues` is what `variables.yml` declared — a decoded project file, so
it belongs on `ProjectDecl`. `FileVars` is what `--var-file` supplied — a CLI input,
so it does not belong on `ProjectDecl` at all. They merge at the same rung inside
`Compile` and nowhere earlier: collapsing them upstream would make `--var-file`
indistinguishable from `variables.yml`, which is exactly the distinction the whole
`Scope` field exists to preserve.

## Locate the code by grep, not by line number

Line numbers in this plan will be stale by the time you run it. Find each site:

```bash
export PATH="$HOME/.local/share/mise/shims:$PATH"
grep -rn "variableScope" --include=*.go .          # the function and its one caller
grep -rn "compileScope" --include=*.go .           # the expressions.Scope adapter
grep -rn "opts.Region\|opts.Account" --include=*.go .
```

`grep -rn "variableScope"` must return NOTHING when this task is finished. That is
the check that the replacement is a replacement.

## Where the three synthetic entries go, and why

`variableScope` currently injects `environment` unconditionally and `region` and
`account` when `Options` carries them, all as strings with `SourceEnvironment` /
`SourceVariable`, written AFTER the `--var` entries so they win. Nothing in the
existing suite asserts any of this — grep for `${environment}` across the tests and
you will find only spec prose — so dropping them is a silent regression that no
existing test catches. Step 7.1 adds the test that catches it.

They stay, with the same precedence, injected through `variables.Scope.Override`
after `variables.Resolve` returns:

- **`environment` is authoritative and unconditional.** Configuration names its own
  environment with `${environment}` (`PLAN.md` §10's own example is
  `name: ${project_name}-${environment}`). If `--var environment=staging` could
  change it while `infra plan production` planned production, resource names and the
  plan header would disagree about which environment was being changed.
- **`region` and `account` are injected only when `Options` carries them**, which
  preserves today's behaviour exactly: absent, `${region}` is an undefined variable
  and stage 6 says so, which is the honest answer rather than an empty string
  interpolated into a resource name.
- They keep `SourceEnvironment`, not `SourceVariable`. `Source` says what KIND of
  thing a value is, and these are facts about the environment being planned, not
  variables anyone declared. `Scope` is `ScopeCLIOverride`, because that is where
  they enter the process — the command line.

## Steps

### 7.1 Failing test: the three synthetic entries survive, in both directions

Add to `internal/compiler/compile_test.go`. Write this FIRST, before touching
`compile.go`, and confirm it passes against the CURRENT code — it is a
characterisation test whose whole job is to fail if the rewiring drops something.

```go
func TestCompileAlwaysDefinesEnvironmentRegionAndAccount(t *testing.T) {
    files := loadFiles(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: ${environment}/${region}/${account}
`)
    cfg, ds := Compile(files, testRegistry(t), Options{
        Environment: "production",
        Region:      "us-east-1",
        Account:     "123456789012",
    })
    if ds.HasErrors() {
        t.Fatalf("unexpected diagnostics: %+v", ds)
    }
    got, _ := cfg.Resources["network"].Attrs["cidr"].AsString()
    if got != "production/us-east-1/123456789012" {
        t.Errorf("cidr = %q, want %q — configuration must be able to name its own environment, region and account", got, "production/us-east-1/123456789012")
    }
}

func TestCompileLeavesRegionUndefinedWhenNoneWasSupplied(t *testing.T) {
    // The other direction of the predicate. Injecting an empty string instead
    // would interpolate silently into a resource name; an undefined variable
    // is a diagnostic the user can act on.
    files := loadFiles(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: ${region}
`)
    _, ds := Compile(files, testRegistry(t), Options{Environment: "dev"})
    if !ds.HasErrors() {
        t.Fatal("${region} with no region supplied must be reported as undefined, not resolved to an empty string")
    }
}

func TestCompileWillNotLetAVarFlagRedefineTheEnvironment(t *testing.T) {
    files := loadFiles(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: ${environment}
`)
    cfg, ds := Compile(files, testRegistry(t), Options{
        Environment: "production",
        Vars:        map[string]string{"environment": "staging"},
    })
    if ds.HasErrors() {
        t.Fatalf("unexpected diagnostics: %+v", ds)
    }
    if got, _ := cfg.Resources["network"].Attrs["cidr"].AsString(); got != "production" {
        t.Errorf("cidr = %q, want production — a plan whose resource names said staging while it planned production would be lying about what it was changing", got)
    }
}
```

### 7.2 Run it against the CURRENT code

```bash
go test -count=1 -run "TestCompileAlwaysDefines|TestCompileLeavesRegion|TestCompileWillNotLetAVar" ./internal/compiler/
```

All three must PASS now. If any fails, the current behaviour is not what this task
claims to preserve — stop and report that before rewiring anything. Commit:
`M4 task 7: pin the synthetic environment variables`.

### 7.3 Failing test: `Compile` honours environments and variables files

```go
func TestCompileResolvesVariablesThroughTheEnvironmentChain(t *testing.T) {
    files := loadFiles(t, `
project: myapp
variables:
  replicas:
    type: integer
    default: 50
    min: 1
    max: 100
environments:
  base:
    replicas: 30
  production:
    extends: base
    replicas: 20
resources:
  database:
    type: test.database
    engine: postgres
    size: ${replicas}
`)
    cfg, ds := Compile(files, testRegistry(t), Options{Environment: "production"})
    if ds.HasErrors() {
        t.Fatalf("unexpected diagnostics: %+v", ds)
    }
    size := cfg.Resources["database"].Attrs["size"]
    if n, _ := size.AsInt(); n != 20 {
        t.Errorf("size = %d, want 20 — production's own value beats the one it inherits and the declared default", n)
    }
    if size.Scope != value.ScopeEnvironmentVar {
        t.Errorf("Scope = %v, want ScopeEnvironmentVar", size.Scope)
    }
}

func TestCompileReportsAnUnknownEnvironment(t *testing.T) {
    files := loadFiles(t, `
project: myapp
environments:
  production: {}
resources:
  network:
    type: test.network
    cidr: 10.0.0.0/16
`)
    _, ds := Compile(files, testRegistry(t), Options{Environment: "prod"})
    if !ds.HasErrors() {
        t.Fatal("`infra plan prod` against a project declaring only `production` must be reported by stage 3")
    }
}

func TestCompileStopsAfterEnvironmentErrors(t *testing.T) {
    // Stage 4 with a failed chain would report every environment-scoped
    // variable as unset — noise piled on the one real error.
    files := loadFiles(t, `
project: myapp
variables:
  domain:
    type: string
environments:
  a:
    extends: b
  b:
    extends: a
resources:
  network:
    type: test.network
    cidr: ${domain}
`)
    _, ds := Compile(files, testRegistry(t), Options{Environment: "a"})
    if len(ds) != 1 {
        t.Fatalf("want exactly the cycle diagnostic, got %d:\n%+v", len(ds), ds)
    }
}
```

`${replicas}` binding to an integer-typed attribute exercises that a variable's value
keeps its `Kind` through stage 6 — a string-only variable system would produce
`"20"` and fail stage 7's kind check.

### 7.4 Run it, see it fail

```bash
go test -count=1 -run TestCompile ./internal/compiler/
```

Expect the `variables:` and `environments:` blocks to be reported as unrecognised
top-level keys by stage 2 if Task 3 is not merged (`unrecognised top-level key`), or
`size = 0` / `undefined variable "replicas"` if it is. Either way it fails.

### 7.5 Minimal code: stages 3 and 4 in `Compile`

In `internal/compiler/compile.go`, insert between `config.Decode` and
`bindReferences`:

```go
    chain, envDiags := environments.Resolve(project.Environments, opts.Environment)
    ds.Extend(envDiags)
    if envDiags.HasErrors() {
        // A failed chain makes stage 4 report every environment-scoped variable
        // as unset, which buries the one diagnostic that explains the failure.
        return ResolvedConfig{}, ds
    }

    scope, varDiags := variables.Resolve(project.Variables, chain, fileVars(project, opts), opts.Vars)
    ds.Extend(varDiags)
    seedProcessVariables(&scope, opts)
    if varDiags.HasErrors() {
        // Stage 6 would report `undefined variable` for each of the same names
        // at each use site — the same problem told twice, with the second
        // telling less informative than the first.
        return ResolvedConfig{}, ds
    }

    cfg, bindDiags := bindReferences(project, scope, opts)
```

Extend `Compile`'s existing doc comment — the one that already explains why it stops
at stage boundaries and why a partial config is returned from stage 6 onward — to
cover the two new early returns, and to say why THEY return a zero `ResolvedConfig`
rather than a partial one: no `ResolvedConfig` exists yet at stages 3 and 4, exactly
as with `Decode`. The paragraph warning that `cfg, _ := Compile(...)` is always a bug
applies unchanged and must not be deleted.

Add the two helpers to `compile.go`:

```go
// fileVars merges --var-file entries over variables.yml's.
//
// The two arrive as separate fields because they are separate things — one is
// a decoded project file, the other a command-line input — and they merge only
// here, at the one rung of the precedence chain they share. The file named on
// the command line is the more specific of the two, so it wins.
func fileVars(project *config.ProjectDecl, opts Options) map[string]value.Value {
    out := make(map[string]value.Value, len(project.VariableValues)+len(opts.FileVars))
    for name, v := range project.VariableValues {
        out[name] = v
    }
    for name, v := range opts.FileVars {
        out[name] = v
    }
    return out
}

// seedProcessVariables adds the three variables that come from the process
// invocation rather than from any file.
//
// `environment` is unconditional and authoritative: configuration names its own
// environment (PLAN.md §10's example is `name: ${project_name}-${environment}`),
// and a --var that could change it would produce resource names claiming one
// environment while the plan changed another.
//
// `region` and `account` are added only when supplied. Injecting an empty
// string instead would interpolate silently into a resource name; leaving them
// undefined makes stage 6 say so, which the user can act on.
//
// They carry SourceEnvironment because they are facts about the environment
// being planned rather than variables anyone declared, and ScopeCLIOverride
// because the command line is where they enter the process. Source and Scope
// are orthogonal: one says what kind of thing a value is, the other says which
// precedence level supplied it.
func seedProcessVariables(scope *variables.Scope, opts Options) {
    origin := value.Origin{File: "<command line>"}
    set := func(name, text string) {
        scope.Override(name, value.String(text, value.SourceEnvironment).
            WithScope(value.ScopeCLIOverride).WithOrigin(origin))
    }
    set("environment", opts.Environment)
    if opts.Region != "" {
        set("region", opts.Region)
    }
    if opts.Account != "" {
        set("account", opts.Account)
    }
}
```

### 7.6 Minimal code: delete `variableScope`

In `internal/compiler/bind.go`:

1. Delete the whole `variableScope` function.
2. Change `compileScope` to hold the resolved scope and delegate:

```go
// compileScope resolves variables at compile time but reports every resource
// attribute as unavailable. That is what turns a reference into an unknown
// carrying its expression, and simultaneously what makes the dependency edge
// discoverable. The apply-time scope in M3 resolves attributes too.
//
// The variables come from stage 4 (internal/variables) and are not rebuilt
// here. This package used to construct its own flat variable map; there is now
// exactly one implementation of the precedence chain, so what a plan claims
// about a value's origin cannot drift away from the rule that produced it
// (spec §7.1).
type compileScope struct {
    vars variables.Scope
}

// Variable resolves a compile-time variable by name.
func (s compileScope) Variable(name string) (value.Value, bool) {
    return s.vars.Variable(name)
}
```

3. Change `bindReferences`' signature to take the scope and use it:

```go
func bindReferences(project *config.ProjectDecl, vars variables.Scope, opts Options) (ResolvedConfig, diag.Diagnostics) {
    ...
    scope := compileScope{vars: vars}
```

`opts` stays: `bindReferences` still reads `opts.Environment` for
`ResolvedConfig.Environment`.

4. Update `bind_test.go`. `TestBindResolvesCliVariables` and every other
   `bindReferences(p, Options{...})` call gains a scope argument. Build it through
   the real resolver, not a hand-made map — a test that constructs its own scope
   stops testing the thing that produces one:

```go
func scopeFor(t *testing.T, opts Options) variables.Scope {
    t.Helper()
    chain, ds := environments.Resolve(nil, opts.Environment)
    if ds.HasErrors() {
        t.Fatalf("fixture chain: %+v", ds)
    }
    scope, ds := variables.Resolve(nil, chain, nil, opts.Vars)
    if ds.HasErrors() {
        t.Fatalf("fixture scope: %+v", ds)
    }
    seedProcessVariables(&scope, opts)
    return scope
}
```

### 7.7 Run it, see it pass, and confirm the deletion

```bash
go test -count=1 ./... && go vet ./... && gofmt -l .
grep -rn "variableScope" --include=*.go .    # must print nothing
```

Commit: `M4 task 7: wire stages 3 and 4 into Compile, delete variableScope`.

### 7.8 Failing test: the bottom rung of the chain is stamped

`PLAN.md` §7's chain is provider defaults → base config → module defaults →
environment inheritance → environment variables → CLI overrides. Stages 3 and 4 stamp
the top five. The floor — provider defaults — is filled in **stage 7**
(`internal/compiler/schema.go`'s `applyDefaults`), which the wiring above does not
touch, so without this step a plan can name every rung of the chain except the one it
stands on.

It belongs here rather than in Task 9 because stamping a value as it is produced is
wiring, and Task 9 renders whatever `Scope` it is handed. A rendering task reaching
into `internal/compiler` would be the two-tasks-one-file collision the plan is split
to avoid.

Locate the code by grep, not by line number:

```bash
export PATH="$HOME/.local/share/mise/shims:$PATH"
grep -n "func applyDefaults\|func checkedDefault\|func fromDefault" internal/compiler/schema.go
```

`applyDefaults` is unexported, so this is an in-package test. Add to
`internal/compiler/schema_test.go`, reusing the `oneResource` and `testRegistry`
helpers already at the top of that file:

```go
func TestSchemaStampsProviderDefaultsWithTheirScope(t *testing.T) {
    // test.database's `size` is optional with a default (providers/test's
    // definitions.go): 10 outside production. Filling it is the only rung of
    // PLAN.md §7's chain that stages 3 and 4 never see.
    cfg := oneResource("test.database", map[string]value.Value{
        "engine": value.String("postgres", value.SourceExplicit),
    })
    ds := bindSchemas(cfg, testRegistry(t), Options{Environment: "dev"})
    if ds.HasErrors() {
        t.Fatalf("unexpected diagnostics: %+v", ds)
    }

    size, ok := cfg.Resources["r"].Attrs["size"]
    if !ok {
        t.Fatal("the default for `size` was not filled in at all")
    }
    if size.Source != value.SourceDefault {
        t.Errorf("Source = %v, want SourceDefault", size.Source)
    }
    if size.Scope != value.ScopeProviderDefault {
        t.Errorf("Scope = %v, want ScopeProviderDefault — provider defaults are the floor of PLAN.md §7's precedence chain, and a chain that cannot show its own floor is not explainable", size.Scope)
    }
}

func TestSchemaDoesNotStampValuesConfigurationSupplied(t *testing.T) {
    // The other direction, and the one that matters more: applyDefaults'
    // contract is that an explicit value always beats an implicit one, so a
    // stamp that leaked onto explicit values would make a plan claim the
    // provider supplied something the user wrote. That is a precedence lie,
    // and unlike a missing stamp it is invisible — the value is right and only
    // its provenance is wrong.
    cfg := oneResource("test.database", map[string]value.Value{
        "engine": value.String("postgres", value.SourceExplicit),
        "size":   value.Int(50, value.SourceExplicit),
    })
    ds := bindSchemas(cfg, testRegistry(t), Options{Environment: "dev"})
    if ds.HasErrors() {
        t.Fatalf("unexpected diagnostics: %+v", ds)
    }

    size := cfg.Resources["r"].Attrs["size"]
    if n, _ := size.AsInt(); n != 50 {
        t.Fatalf("size = %d, want the explicit 50 — applyDefaults must never overwrite a configured value", n)
    }
    if size.Scope != value.ScopeUnset {
        t.Errorf("Scope = %v, want ScopeUnset: nothing in stage 7 supplied this value, so stage 7 must not claim it did", size.Scope)
    }
}

func TestSchemaStampingADefaultDoesNotMakeItCompareUnequal(t *testing.T) {
    // Task 1's invariant 1 at this site. Equal ignores Scope; if it ever
    // stopped doing so, every resource with a filled default would diff
    // against the same value from configuration or state, and acceptance
    // invariant 2 (no-op plan) would fail for most resources in most projects.
    cfg := oneResource("test.database", map[string]value.Value{
        "engine": value.String("postgres", value.SourceExplicit),
    })
    if ds := bindSchemas(cfg, testRegistry(t), Options{Environment: "dev"}); ds.HasErrors() {
        t.Fatalf("unexpected diagnostics: %+v", ds)
    }

    stamped := cfg.Resources["r"].Attrs["size"]
    plain := value.Int(10, value.SourceDefault)
    if !stamped.Equal(plain) {
        t.Error("a stamped default must still be Equal to the same datum with no Scope — provenance describes how a value was arrived at, not what the desired state is")
    }
}
```

### 7.9 Run it, see it fail

```bash
go test -count=1 -run "TestSchemaStamps|TestSchemaDoesNotStamp" ./internal/compiler/
```

Expect `TestSchemaStampsProviderDefaultsWithTheirScope` to fail with
`Scope = unset, want ScopeProviderDefault`. The other two must PASS already — they
assert what the code does today, and their job is to fail if the change in 7.10
overreaches. If either fails now, stop: the premise that `applyDefaults` never
touches an explicit value is wrong, and this step is not the place to discover that.

### 7.10 Minimal code: stamp the default where it is built

In `internal/compiler/schema.go`, change `checkedDefault`'s success return:

```go
func checkedDefault(raw any, kind value.Kind) (value.Value, bool) {
    v, ok := fromDefault(raw, kind)
    if !ok || v.Kind != kind {
        return value.Value{}, false
    }
    return v.WithScope(value.ScopeProviderDefault), true
}
```

**Why here and not the other two candidates.** `fromDefault` sets `SourceDefault` in
seven arms, one per kind, and `Scope` is `Source`'s orthogonal partner — stamping
beside it would mean seven edits and seven chances to stamp six kinds and miss the
seventh. `applyDefaults`' `attrs[name] = v` is a single site too, but there the scope
would be a property of "was assigned by `applyDefaults`" rather than of "came from a
provider default", and it would not travel if the value were ever built elsewhere.
`checkedDefault` is the one choke point every provider default passes through, and it
already exists to enforce a property of defaults rather than to construct them, so a
second such property sits naturally next to the first.

Add a line to `fromDefault`'s doc comment so the pairing is discoverable from the arm
that sets `SourceDefault`:

```go
// The matching Scope — ScopeProviderDefault, the floor of PLAN.md §7's
// precedence chain — is stamped once by checkedDefault rather than in each arm
// here, so it cannot be applied to six kinds and missed on the seventh.
```

Do **not** touch `value.Equal` or `ResolvedConfig.Hash`. Task 1 fixed both:
`Equal` ignores `Scope`, and `ConfigHash` excludes it. A provider default that
compared unequal to the same datum from configuration would break acceptance
invariant 2 for every resource that has a default, which is most of them; a hash that
included `Scope` would make an unchanged configuration read as stale in M6.

### 7.11 Run it, see it pass, and check the goldens

```bash
go test -count=1 ./internal/compiler/ ./internal/planner/ ./tests/integration/
```

**No golden is regenerated by this step, and none should change.** Two reasons,
both checked rather than assumed:

- `internal/planner/testdata/mixed.golden` shows `size: 10 [default]`, but its
  fixture never goes through `applyDefaults`: `mixedPlan()` in `render_test.go`
  hand-builds `value.Int(10, value.SourceDefault)`, which carries `ScopeUnset`. Verify
  before you believe it — `grep -n "SourceDefault" internal/planner/render_test.go`.
- Nothing renders a scope suffix yet. Task 9 is what wires `renderAnnotated` to
  `value.Annotate`; until then the stamp is carried and not printed.

So if a golden DOES change here, that is a signal the stamp reached somewhere it
should not have — most likely onto explicit values. Investigate it; do not regenerate.

`internal/planner/testdata/scopes.golden`, which renders a provider default as
`size: 100 [default, from provider defaults]`, is **Task 9's** to write, and it is the
test that proves this stamp reaches the page. If it is missing or wrong when Task 9
runs, that is a Task 9 defect, not a licence to change this step.

Commit: `M4 task 7: stamp provider defaults with ScopeProviderDefault`.

### 7.12 Failing test: end to end through the CLI

Create `tests/integration/m4_variables_test.go`. The unit tests above prove the
compiler; this proves the wiring the user actually reaches, and it is the test that
would catch `internal/cli` never passing an environment through.

```go
package integration

import (
    "os"
    "path/filepath"
    "testing"
)

// projectWithFiles writes infra.yml plus any extra files, keyed by path
// relative to the project directory.
func projectWithFiles(t *testing.T, body string, extra map[string]string) string {
    t.Helper()
    dir := project(t, body)
    for rel, content := range extra {
        path := filepath.Join(dir, rel)
        if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
            t.Fatalf("mkdir %s: %v", rel, err)
        }
        if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
            t.Fatalf("write %s: %v", rel, err)
        }
    }
    return dir
}

func TestPlanResolvesVariablesFromFilesEnvironmentsAndFlags(t *testing.T) {
    dir := projectWithFiles(t, `
project: myapp
variables:
  replicas:
    type: integer
    default: 50
    min: 1
    max: 100
environments:
  base:
    replicas: 30
  production:
    extends: base
    replicas: 20
resources:
  database:
    type: test.database
    engine: postgres
    size: ${replicas}
`, map[string]string{
        "variables.yml": "replicas: 40\n",
    })

    // The environment beats variables.yml, which beats the declared default.
    res := run(t, dir, "plan", "production")
    if res.ExitCode != 0 {
        t.Fatalf("plan failed:\n%s", res.combined())
    }
    requireContains(t, res.combined(), "20")

    // --var beats all of them.
    res = run(t, dir, "plan", "production", "--var", "replicas=10")
    if res.ExitCode != 0 {
        t.Fatalf("plan failed:\n%s", res.combined())
    }
    requireContains(t, res.combined(), "10")

    // And a value outside the declared range is refused before anything runs.
    res = run(t, dir, "plan", "production", "--var", "replicas=500")
    if res.ExitCode == 0 {
        t.Fatalf("replicas=500 violates max: 100 and must fail:\n%s", res.combined())
    }
    requireContains(t, res.combined(), "replicas")
}

func TestPlanNamesTheEnvironmentItIsPlanning(t *testing.T) {
    dir := project(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: 10.0.0.0/16
    name: net-${environment}
`)
    res := run(t, dir, "plan", "production")
    if res.ExitCode != 0 {
        t.Fatalf("plan failed:\n%s", res.combined())
    }
    requireContains(t, res.combined(), "net-production")
}
```

Adjust the resource types and attribute names to whatever the fake provider actually
registers — check `providers/test/` — but do not weaken the assertions. The second
test is the integration-level guard on the synthetic `environment` variable: it fails
if stage 4 stops seeding it, and it exercises a project with no `environments:` block
at all, which is the M2 shape that must keep working.

### 7.13 Run it, see it pass

```bash
go test -count=1 ./tests/integration/
```

`-count=1` matters most here: this package shells out to `go build` rather than
importing infra packages, so a cached result once reported `ok` while production code
was sabotaged.

### 7.14 Full verification and commit

```bash
go build ./cmd/infra
go test -count=1 ./...
go vet ./...
gofmt -l .
grep -rn "variableScope" --include=*.go .
```

The last two must print nothing. Commit:
`M4 task 7: end-to-end variable resolution`.
# M4 implementation plan — Tasks 8 through 12

These are tasks 8-12 of about twelve, numbered in FINAL ORDER. Do not renumber.

Earlier tasks produce, and these consume by exactly these names:

| Task | Produces |
|------|----------|
| 1 | `value.Scope`, its seven constants, `Value.Scope`, `Value.WithScope` |
| 2 | `config.Load` reading `infra.yml`, `variables.yml`, `environments/*.yml` |
| 3 | `config.VariableDecl`, `config.EnvironmentDecl` |
| 4 | typed variable schema validation (PLAN.md §9: type, default, min, max) |
| 5 | `environments.Resolve(decls []config.EnvironmentDecl, name string) (Chain, diag.Diagnostics)` |
| 6 | `variables.Resolve(decls []config.VariableDecl, chain environments.Chain, files map[string]value.Value, cliVars map[string]string) (Scope, diag.Diagnostics)` |
| 7 | stages 3 and 4 wired into `compiler.Compile`; `internal/compiler/bind.go`'s `variableScope` deleted |

**Toolchain, before any `go` command in any task:**

```bash
export PATH="$HOME/.local/share/mise/shims:$PATH"
go version   # must print go1.24.x; a bare `go` is 1.20 and fails
```

**Every `go test` invocation in these tasks carries `-count=1`.** Not a
preference. `tests/integration` shells out to `go build` rather than importing
infra packages, so Go's cache sees no changed input and prints
`ok infra/tests/integration (cached)` while production code is sabotaged —
measured in M3, and the reason the Makefile's `test` target already sets it.
Task 10 and Task 12 add integration files that DO import `infra/pkg/value`,
which narrows the hole but does not close it: the binary under test is produced
by a `go build` subprocess, and no import graph tracks that. `-count=1` stays.

Global constraints that apply to all five tasks:

- Exactly two third-party dependencies: `github.com/spf13/cobra` and
  `gopkg.in/yaml.v3`. `go.mod` must not change. Check with `git diff --stat go.mod go.sum`.
- Exactly ONE redaction path: `pkg/value.Format`. Nothing else may produce the
  string `<sensitive>`.
- A sensitive value must not reach any command's output at any nesting depth.
- `internal/executor` must not import `internal/cli`.

---

## Task 8 — wire `--var-file`

### Why this task exists

`--var-file` is registered on the root command and `checkUnsupportedFlags`
(`internal/cli/root.go`) turns any use of it into an error. That is the
standard this project holds: a flag advertised in `--help` and silently ignored
is worse than an absent one. M4 is the milestone that makes it work, so the
entry in `checkUnsupportedFlags` is deleted **because the promise is now kept**,
never on its own.

### Decisions this task fixes (and the reasoning, so you can check it)

**Format.** A variable file is `variables.yml`'s shape (PLAN.md §8): one YAML
mapping of variable name to value. Scalars, sequences and mappings are all
accepted, because PLAN.md §9's variable types include `list` and `map` and a
file that could not express those could not supply half the declarable
variables. An empty file is not an error (a project may keep an empty
`variables.yml` in version control). A non-mapping top level is an error. A
value containing `${` is an error: expressions inside variable files would need
their own evaluation order (variables referring to variables), which PLAN.md §10
explicitly does not want, and accepting one verbatim would put a literal
`${foo}` into a resource attribute — a silent wrong answer.

**Multiple `--var-file` flags.** Applied left to right; a later file overrides
an earlier one for the same name. Repeated flags composing left-to-right is the
universal convention, and it makes `--var-file base.yml --var-file prod.yml`
read as a stack of overlays.

**Precedence.** `--var-file` sits at `ScopeCLIOverride`, immediately below
`--var`, above `variables.yml` and above environment configuration. Justified
against PLAN.md §8's "CLI values override variable files":

1. `variables.yml` is a variable file and `--var` is a CLI value, so §8 settles
   `--var` > `variables.yml` directly.
2. `--var-file` is a CLI value that carries a file's contents. The user named it
   on the command line for this run; `variables.yml` and `environments/*.yml`
   are what the project says by default. PLAN.md §7's "explicit user
   configuration always overrides an implicit default" points the same way.
3. The alternative — `--var-file` below environment configuration — means
   `infra plan production --var-file overrides.yml` silently ignores the file
   for every name `production` also sets. That is the advertised-and-ignored
   failure this flag is currently erroring to avoid.
4. Both live at one precedence level, so no eighth `Scope` constant is needed
   and the contract's enum stays exactly as fixed.

Within `ScopeCLIOverride`, files apply in flag order, then `--var` applies last,
which honours §8's sentence literally.

**Semantics, stated for the user:** `--var-file f` applies each `name: value`
pair in `f` as if it had been given with `--var name=value`, before any `--var`
on the same command line. This wording is load-bearing for Task 9: a plan
renders a `ScopeCLIOverride` value as `[variable, from --var]`, and that is a
true statement about a `--var-file` value under this definition. It cannot be
made finer-grained: `internal/expressions/eval.go:53` resolves a variable
reference as `return v.WithOrigin(e.Origin)`, replacing the variable's `Origin`
with the referencing attribute's, so `Origin` cannot carry which file a value
came from. Do not try to render the filename from `Origin` — it will print the
name of `infra.yml`.

The full M4 chain, for reference:

```
1 ScopeProviderDefault     provider defaults               (internal/compiler/schema.go)
2 ScopeBaseConfig          variables.yml + declared defaults
3 ScopeModuleDefault       (M5; slot left empty)
4 ScopeEnvironmentInherit  environments reached via extends
5 ScopeEnvironmentVar      the named environment's own variables
6 ScopeCLIOverride         --var-file (flag order), then --var
```

### Files

| Action | Path |
|--------|------|
| create | `internal/config/varfile.go` |
| create | `internal/config/varfile_test.go` |
| create | `internal/cli/varfiles.go` |
| create | `internal/cli/varfiles_test.go` |
| modify | `internal/cli/root.go` (flag help; delete the `--var-file` entry from `checkUnsupportedFlags`) |
| modify | `internal/cli/root_test.go` (rewrite the test at line 52) |
| modify | `internal/compiler/resolved.go` (`Options` gains `FileVars`) |
| modify | `internal/compiler/compile.go` or wherever Task 7 calls `variables.Resolve` |
| modify | `internal/variables/resolve.go` (precedence honours each entry's own `Scope`) |
| modify | `internal/variables/resolve_test.go` |

### Interfaces

Consumes: `config.File`, `config.Load`, `config.decodeValue`, `config.documentRoot`,
`config.originOf` (all in `internal/config/decode.go`), `value.Scope`,
`Value.WithScope` (Task 1), `variables.Resolve` (Task 6), `compiler.Options`.

Produces:

```go
// internal/config
func DecodeVariableFile(f File, scope value.Scope) (map[string]value.Value, diag.Diagnostics)

// internal/cli
func loadVarFiles(dir string, paths []string) (map[string]value.Value, diag.Diagnostics)

// internal/compiler
type Options struct {
    Environment string
    Region      string
    Account     string
    Vars        map[string]string        // --var
    FileVars    map[string]value.Value   // variables.yml and --var-file, each entry already scoped
}
```

`FileVars` is `map[string]value.Value` rather than `map[string]string` for one
reason: a variable file can hold a list or a map, and each entry has to carry
the `Scope` of the layer it came from. That is what lets one map hold two
precedence levels without a second parameter.

### Steps

**8.1 — Failing test for the decoder.**

Create `internal/config/varfile_test.go`:

```go
package config

import (
	"testing"

	"gopkg.in/yaml.v3"

	"infra/pkg/value"
)

func fileFrom(t *testing.T, path, body string) File {
	t.Helper()
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(body), &root); err != nil {
		t.Fatalf("parsing fixture: %v", err)
	}
	return File{Path: path, Root: &root}
}

func TestDecodeVariableFileStampsSourceAndScopeOnEveryLeaf(t *testing.T) {
	f := fileFrom(t, "variables.yml", `
project_name: myapp
replicas: 3
enabled: true
regions:
  - us-east-1
  - eu-west-1
tags:
  team: platform
`)

	got, ds := DecodeVariableFile(f, value.ScopeCLIOverride)
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %v", ds)
	}

	if s, ok := got["project_name"].AsString(); !ok || s != "myapp" {
		t.Errorf("project_name = %#v, want the string myapp", got["project_name"])
	}
	// An integer must stay an integer. If a variable file flattened everything
	// to a string, a variable feeding an integer attribute would fail schema
	// validation with a kind mismatch the user cannot fix from YAML.
	if n, ok := got["replicas"].AsInt(); !ok || n != 3 {
		t.Errorf("replicas = %#v, want the integer 3", got["replicas"])
	}
	if b, ok := got["enabled"].AsBool(); !ok || !b {
		t.Errorf("enabled = %#v, want the boolean true", got["enabled"])
	}

	for name, v := range got {
		if v.Source != value.SourceVariable {
			t.Errorf("%s: Source = %q, want %q — a variable file supplies variables at every level",
				name, v.Source, value.SourceVariable)
		}
		if v.Scope != value.ScopeCLIOverride {
			t.Errorf("%s: Scope = %v, want ScopeCLIOverride", name, v.Scope)
		}
	}

	// Provenance is per-leaf (spec §5.1), so a composite's children carry it
	// too: ConfigHash folds every leaf's Source, and a child left
	// SourceExplicit would make the same map hash differently depending on
	// which layer supplied it.
	items, ok := got["regions"].Raw.([]value.Value)
	if !ok || len(items) != 2 {
		t.Fatalf("regions did not decode as a two-item list: %#v", got["regions"])
	}
	for i, item := range items {
		if item.Source != value.SourceVariable || item.Scope != value.ScopeCLIOverride {
			t.Errorf("regions[%d]: Source=%q Scope=%v, want variable/ScopeCLIOverride",
				i, item.Source, item.Scope)
		}
	}
	m, ok := got["tags"].Raw.(map[string]value.Value)
	if !ok {
		t.Fatalf("tags did not decode as a map: %#v", got["tags"])
	}
	if m["team"].Source != value.SourceVariable || m["team"].Scope != value.ScopeCLIOverride {
		t.Errorf("tags.team: Source=%q Scope=%v, want variable/ScopeCLIOverride",
			m["team"].Source, m["team"].Scope)
	}
}

func TestDecodeVariableFileScopeIsAParameterNotAConstant(t *testing.T) {
	// One decoder serves two precedence levels. If it hard-coded a scope,
	// variables.yml and --var-file could not be told apart, which is the
	// whole point of the field.
	f := fileFrom(t, "variables.yml", "a: 1\n")
	got, _ := DecodeVariableFile(f, value.ScopeBaseConfig)
	if got["a"].Scope != value.ScopeBaseConfig {
		t.Errorf("Scope = %v, want ScopeBaseConfig", got["a"].Scope)
	}
}

func TestDecodeVariableFileRejectsANonMappingTopLevel(t *testing.T) {
	_, ds := DecodeVariableFile(fileFrom(t, "vars.yml", "- a\n- b\n"), value.ScopeBaseConfig)
	if !ds.HasErrors() {
		t.Fatal("a sequence at the top level of a variable file must be an error")
	}
}

func TestDecodeVariableFileRejectsInterpolation(t *testing.T) {
	// Accepting it would put the literal text ${project_name} into a resource
	// attribute: a silent wrong answer, which is worse than a refusal.
	_, ds := DecodeVariableFile(fileFrom(t, "vars.yml", "name: ${project_name}-web\n"), value.ScopeBaseConfig)
	if !ds.HasErrors() {
		t.Fatal("an interpolation inside a variable file must be an error while expressions in variable files are unsupported")
	}
}

func TestDecodeVariableFileAcceptsAnEmptyFile(t *testing.T) {
	got, ds := DecodeVariableFile(fileFrom(t, "variables.yml", ""), value.ScopeBaseConfig)
	if ds.HasErrors() {
		t.Fatalf("an empty variable file is not an error: %v", ds)
	}
	if len(got) != 0 {
		t.Errorf("got %d variables from an empty file", len(got))
	}
}
```

Run it. Expected failure: `undefined: DecodeVariableFile`.

```bash
go test -count=1 ./internal/config/
```

**8.2 — Implement the decoder.**

First check whether Task 2 or Task 6 already built a `variables.yml` decoder:

```bash
grep -rn "variables.yml\|VariableFile\|variableFile" internal/config/ internal/compiler/ internal/variables/
```

If one exists, **move its body into `DecodeVariableFile` and make the existing
caller use it**. Do not add a second decoder: `internal/compiler/bind.go`'s
`variableScope` being one of two implementations of one concept is the defect
class that leaked a plaintext secret in M2, and the contract calls it out.

Create `internal/config/varfile.go`:

```go
package config

import (
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"infra/internal/diag"
	"infra/pkg/value"
)

// DecodeVariableFile converts one variable file into named values.
//
// The shape is PLAN.md §8's variables.yml: a single YAML mapping of variable
// name to value. Sequences and mappings are allowed as well as scalars,
// because PLAN.md §9's variable types include list and map.
//
// scope is a parameter, not a constant, because this one decoder serves two
// precedence levels: variables.yml is ScopeBaseConfig and a --var-file is
// ScopeCLIOverride. A second decoder for the second level would be two
// implementations of one concept, which is what the M4 contract forbids.
//
// Source is SourceVariable for every entry — a variable file supplies
// variables, whichever level it was loaded at. Scope, not Source, records which
// level won.
func DecodeVariableFile(f File, scope value.Scope) (map[string]value.Value, diag.Diagnostics) {
	var ds diag.Diagnostics
	out := map[string]value.Value{}

	root := documentRoot(f.Root)
	if root == nil {
		// An empty variable file is deliberately not an error: a project may
		// keep an empty variables.yml under version control precisely so the
		// file exists to be filled in.
		return out, ds
	}
	if root.Kind != yaml.MappingNode {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "variable file must be a mapping of variable names to values",
			Detail:   "The top level of " + f.Path + " is not a mapping.",
			Action:   "Write one `name: value` pair per line, as in PLAN.md §8's variables.yml.",
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
		if _, dup := out[key.Value]; dup {
			// Reported rather than resolved: with two entries of one name in
			// one file, neither answer is defensible, and picking one
			// silently means the user's other line does nothing.
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "variable " + strconv.Quote(key.Value) + " is defined twice in " + f.Path,
				Detail:   "Two entries of the same name in one file have no defined precedence.",
				Action:   "Delete one of them.",
				Origin:   originOf(f.Path, key),
			})
			continue
		}
		if containsInterpolation(val) {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "variable " + strconv.Quote(key.Value) + " contains an interpolation",
				Detail:   "Variable files hold plain values; expressions inside them are not supported.",
				Action:   "Move the expression to the resource attribute that needs it, or write the literal value here.",
				Origin:   originOf(f.Path, val),
			})
			continue
		}

		v, _ := decodeValue(f.Path, key.Value, val, &ds)
		out[key.Value] = stampVariable(v, scope)
	}
	return out, ds
}

// containsInterpolation reports whether a node, or anything inside it, holds
// ${...}. decodeValue accepts an interpolation verbatim for resource
// attributes, where stage 6 later parses it; nothing parses one here.
func containsInterpolation(n *yaml.Node) bool {
	if n == nil {
		return false
	}
	if n.Kind == yaml.ScalarNode {
		return strings.Contains(n.Value, "${")
	}
	for _, c := range n.Content {
		if containsInterpolation(c) {
			return true
		}
	}
	return false
}

// stampVariable rewrites provenance on a decoded value and every leaf inside
// it. decodeValue marks everything SourceExplicit, which is right for a
// resource attribute and wrong here.
//
// It recurses because provenance is per-leaf (spec §5.1) and
// ResolvedConfig.Hash folds every leaf's Source: a child left SourceExplicit
// would make the same map hash differently depending on which layer supplied
// it, which is the staleness false positive invariant 2 of the M4 contract
// exists to prevent.
func stampVariable(v value.Value, scope value.Scope) value.Value {
	v = v.WithSource(value.SourceVariable).WithScope(scope)
	switch v.Kind {
	case value.KindList:
		items, ok := v.Raw.([]value.Value)
		if !ok {
			return v
		}
		out := make([]value.Value, len(items))
		for i, item := range items {
			out[i] = stampVariable(item, scope)
		}
		v.Raw = out
	case value.KindMap:
		m, ok := v.Raw.(map[string]value.Value)
		if !ok {
			return v
		}
		out := make(map[string]value.Value, len(m))
		for k, item := range m {
			out[k] = stampVariable(item, scope)
		}
		v.Raw = out
	}
	return v
}
```

`go test -count=1 ./internal/config/` — green.

**8.3 — Failing test for the CLI loader.**

Create `internal/cli/varfiles_test.go`. The multi-file case tests the MIDDLE of
the ladder: an implementation that only reads the last file passes a two-file
test that checks the last file's value, and fails this one.

```go
package cli

import (
	"os"
	"path/filepath"
	"testing"

	"infra/pkg/value"
)

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func TestLoadVarFilesAppliesFilesInFlagOrderSoALaterFileWins(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "one.yml", "a: one\nb: one\nc: one\n")
	writeFile(t, dir, "two.yml", "b: two\nc: two\n")
	writeFile(t, dir, "three.yml", "c: three\n")

	got, ds := loadVarFiles(dir, []string{"one.yml", "two.yml", "three.yml"})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %v", ds)
	}

	// a from the FIRST file, b from the MIDDLE one, c from the last. Only the
	// middle rung distinguishes "applies every file in order" from "applies
	// the last file" or "applies the first file".
	for _, tc := range []struct{ name, want string }{
		{"a", "one"}, {"b", "two"}, {"c", "three"},
	} {
		s, ok := got[tc.name].AsString()
		if !ok || s != tc.want {
			t.Errorf("%s = %#v, want %q", tc.name, got[tc.name], tc.want)
		}
	}
	if len(got) != 3 {
		t.Errorf("got %d variables, want 3 — every file must contribute", len(got))
	}
	if got["a"].Scope != value.ScopeCLIOverride {
		t.Errorf("a: Scope = %v, want ScopeCLIOverride", got["a"].Scope)
	}
}

func TestLoadVarFilesResolvesRelativePathsAgainstChdir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "vars.yml", "a: yes-please\n")
	// The process working directory is the package directory, not dir. If
	// loadVarFiles resolved against os.Getwd, every --var-file under --chdir
	// would fail, and every integration test would have to pass an absolute
	// path.
	got, ds := loadVarFiles(dir, []string{"vars.yml"})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %v", ds)
	}
	if s, _ := got["a"].AsString(); s != "yes-please" {
		t.Errorf("a = %#v", got["a"])
	}
}

func TestLoadVarFilesReportsAMissingFile(t *testing.T) {
	ds := func() string {
		_, ds := loadVarFiles(t.TempDir(), []string{"nope.yml"})
		return renderToString(ds)
	}()
	if ds == "" {
		t.Fatal("a --var-file that does not exist must be an error, not an empty layer")
	}
	if !contains(ds, "nope.yml") {
		t.Errorf("the diagnostic must name the path the user typed:\n%s", ds)
	}
}
```

`renderToString` already exists in this package (`internal/cli/validate_test.go`
uses it — confirm with `grep -rn "func renderToString" internal/cli/`). If
`contains` is not already present, use `strings.Contains` directly.

Expected failure: `undefined: loadVarFiles`.

**8.4 — Implement the loader.**

Create `internal/cli/varfiles.go`:

```go
package cli

import (
	"os"
	"path/filepath"
	"strconv"

	"gopkg.in/yaml.v3"

	"infra/internal/config"
	"infra/internal/diag"
	"infra/pkg/value"
)

// loadVarFiles reads every --var-file in flag order and merges them into one
// layer.
//
// A later file overrides an earlier one for the same name, so
// `--var-file base.yml --var-file prod.yml` reads as a stack of overlays. Every
// entry is stamped ScopeCLIOverride: a file named on the command line is a
// value the invoker chose for this run, which outranks the project's implicit
// variables.yml and its environment configuration (PLAN.md §7, §8). `--var` is
// applied after this layer, so it still wins.
//
// Errors collect rather than stopping at the first bad file, per spec §7.4:
// `infra validate` reports every problem in one pass.
func loadVarFiles(dir string, paths []string) (map[string]value.Value, diag.Diagnostics) {
	var ds diag.Diagnostics
	out := map[string]value.Value{}

	for _, p := range paths {
		full := p
		if !filepath.IsAbs(p) {
			// Relative to --chdir, not to the process working directory:
			// --chdir means "run as if infra had been started in this
			// directory", and a flag that ignored it would resolve paths from
			// somewhere the user is not looking.
			full = filepath.Join(dir, p)
		}
		data, err := os.ReadFile(full)
		if err != nil {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "cannot read variable file " + strconv.Quote(p),
				Detail:   err.Error(),
				Action:   "Check the path. Relative paths resolve against " + strconv.Quote(dir) + ".",
				Origin:   value.Origin{File: p},
			})
			continue
		}
		var root yaml.Node
		if err := yaml.Unmarshal(data, &root); err != nil {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "cannot parse variable file " + strconv.Quote(p),
				Detail:   err.Error(),
				Action:   "Fix the YAML syntax.",
				Origin:   value.Origin{File: p},
			})
			continue
		}
		// Path as WRITTEN on the command line, not the joined path: it is what
		// the user typed, and it keeps diagnostics free of the temporary
		// directory an integration test happens to run in.
		vals, fds := config.DecodeVariableFile(config.File{Path: p, Root: &root}, value.ScopeCLIOverride)
		ds.Extend(fds)
		for name, v := range vals {
			out[name] = v
		}
	}
	return out, ds
}
```

`go test -count=1 ./internal/cli/` — the new tests pass; `TestVarFileIsRejected`
in `root_test.go` still passes because nothing is wired yet.

**8.5 — Failing test: `variables.Resolve` honours each entry's own `Scope`.**

Locate the precedence comparison:

```bash
grep -rn "ScopeEnvironmentVar\|ScopeBaseConfig\|ScopeCLIOverride" internal/variables/
```

Add to `internal/variables/resolve_test.go` (adapt the fixture construction to
Task 5's `Chain` and Task 3's `VariableDecl` — the assertions are the point):

```go
func TestFileEntryAtCLIScopeOutranksEnvironmentConfiguration(t *testing.T) {
	// A --var-file entry arrives inside the same `files` map as a
	// variables.yml entry and is told apart ONLY by its Scope. An
	// implementation that assigns one scope to the whole map cannot express
	// this, and --var-file would be silently ignored for every name the
	// environment also sets.
	chain := chainWith(t, "production", map[string]string{"region": "us-east-1"}) // ScopeEnvironmentVar
	files := map[string]value.Value{
		"region": value.String("eu-west-1", value.SourceVariable).WithScope(value.ScopeCLIOverride),
	}

	scope, ds := Resolve(nil, chain, files, nil)
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %v", ds)
	}
	got, ok := scope.Variable("region")
	if !ok {
		t.Fatal("region is not in scope")
	}
	if s, _ := got.AsString(); s != "eu-west-1" {
		t.Errorf("region = %q, want eu-west-1 — a --var-file outranks environment configuration", s)
	}
	if got.Scope != value.ScopeCLIOverride {
		t.Errorf("Scope = %v, want ScopeCLIOverride — the winning level must be recorded", got.Scope)
	}
}

func TestFileEntryAtBaseScopeLosesToEnvironmentConfiguration(t *testing.T) {
	// The mirror image, and the reason the first test is not satisfied by
	// "the files map always wins": variables.yml sits BELOW the environment.
	chain := chainWith(t, "production", map[string]string{"region": "us-east-1"})
	files := map[string]value.Value{
		"region": value.String("eu-west-1", value.SourceVariable).WithScope(value.ScopeBaseConfig),
	}

	scope, _ := Resolve(nil, chain, files, nil)
	got, _ := scope.Variable("region")
	if s, _ := got.AsString(); s != "us-east-1" {
		t.Errorf("region = %q, want us-east-1", s)
	}
	if got.Scope != value.ScopeEnvironmentVar {
		t.Errorf("Scope = %v, want ScopeEnvironmentVar", got.Scope)
	}
}

func TestCLIVarOutranksAFileEntryAtTheSameScope(t *testing.T) {
	// PLAN.md §8: "CLI values override variable files." Both sit at
	// ScopeCLIOverride, so the tie is broken by application order, not by
	// comparing Scope.
	files := map[string]value.Value{
		"region": value.String("eu-west-1", value.SourceVariable).WithScope(value.ScopeCLIOverride),
	}
	scope, _ := Resolve(nil, emptyChain(t), files, map[string]string{"region": "ap-south-1"})
	got, _ := scope.Variable("region")
	if s, _ := got.AsString(); s != "ap-south-1" {
		t.Errorf("region = %q, want ap-south-1", s)
	}
}
```

**8.6 — Make `variables.Resolve` layer-ordered.**

The rule, stated as code, so it can be checked rather than only obeyed:

```go
// Candidates for one variable, in precedence order, lowest first. The winner
// is the LAST candidate present, which is equivalent to "highest Scope wins"
// while also settling the one tie the Scope enum cannot: --var and --var-file
// share ScopeCLIOverride, and --var is applied last.
//
//   1 declared default          (SourceDefault,     ScopeBaseConfig)
//   2 files[name] when its Scope is ScopeBaseConfig (variables.yml)
//   3 chain inherited value     (SourceEnvironment, ScopeEnvironmentInherit)
//   4 chain own value           (SourceEnvironment, ScopeEnvironmentVar)
//   5 files[name] when its Scope is ScopeCLIOverride (--var-file)
//   6 cliVars[name]             (SourceVariable,    ScopeCLIOverride)
```

Implement it by splitting `files` on `Scope` and applying the two halves at
positions 2 and 5. Do not iterate the map to find "the highest scope" without a
deterministic tie-break — with one entry per name there is no tie inside the
map, but the tie between position 5 and position 6 is real and must be resolved
by order.

`go test -count=1 ./internal/variables/` — green.

**8.7 — Wire it through the compiler and the CLI.**

Add `FileVars map[string]value.Value` to `compiler.Options`
(`internal/compiler/resolved.go:26`), and pass it where Task 7 calls
`variables.Resolve`:

```bash
grep -rn "variables.Resolve" internal/compiler/
```

The `files` argument becomes variables.yml's values overlaid with
`opts.FileVars`. Because a `--var-file` entry is stamped `ScopeCLIOverride` and
a `variables.yml` entry `ScopeBaseConfig`, a plain overwrite is correct — the
overlay is always the higher level:

```go
files := variablesFileValues            // ScopeBaseConfig, from variables.yml
for name, v := range opts.FileVars {    // ScopeCLIOverride, from --var-file
	files[name] = v
}
```

Then in `internal/cli/root.go`, delete the `--var-file` clause of
`checkUnsupportedFlags` and update the flag's help text:

```go
f.StringArrayVar(&opts.VarFiles, "var-file", nil,
	"read variables from a YAML file, as if each entry had been passed with --var; repeatable, later files win")
```

**Keep `checkUnsupportedFlags` itself.** It is the mechanism, not the entry —
deleting the function would remove the guard the next unwired flag needs. If it
would be left with an empty body, leave it returning `nil` with its doc comment
intact and a note that the list is currently empty.

Task 11 does the per-command wiring of `opts.VarFiles` into `compiler.Options`.
For this task, make `plan` compile: in `internal/cli/plan.go`, alongside
`parseVars`, add

```go
fileVars, fds := loadVarFiles(opts.Dir, opts.VarFiles)
if fds.HasErrors() {
	fds.Render(cmd.ErrOrStderr())
	return errors.New("configuration is not valid")
}
```

and pass `FileVars: fileVars` in the `compiler.Options` literal.

**8.8 — Rewrite the flag-rejection test.**

`internal/cli/root_test.go:52` asserts `--var-file` errors. That behaviour is
now gone, so the test must be REWRITTEN, not deleted — a deleted test is how a
guarantee disappears without anyone noticing (M3's lesson). Replace it with the
one thing still true of a flag with nothing behind it:

```go
func TestVarFileReportsAMissingFileRatherThanIgnoringIt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "infra.yml"), []byte("project: myapp\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := NewRootCommand()
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"--chdir", dir, "--var-file", "vars.yml", "validate"})

	err := root.Execute()
	if err == nil {
		t.Fatal("--var-file naming a file that does not exist must fail, not silently contribute nothing")
	}
}
```

**8.9 — Full run and commit.**

```bash
export PATH="$HOME/.local/share/mise/shims:$PATH"
gofmt -l . && go vet ./... && go test -count=1 ./...
git diff --stat go.mod go.sum   # must print nothing
git commit -am "M4 Task 8: wire --var-file at the CLI-override precedence level"
```

---

## Task 9 — render which scope supplied a value (spec §12.3)

### Why this task exists

A plan today annotates `[default]` and nothing else, so it can say a value was
not written by the user but not where it came from. With `Scope` it can, and the
user's ruling fixes the shape: `replicas: 20 [variable, from --var]`.

### The rule

`internal/planner/render.go`'s `renderAnnotated` gains one helper. In order:

1. `!v.Known` → no annotation. Unchanged. An unknown already renders
   `(known after apply)`; its provenance is "computed during apply", and
   `(known after apply) [variable, from --var]` is noise.
2. `v.Source == value.SourceExplicit` → no annotation. A value written
   literally in the configuration the reader is holding needs no explanation,
   and annotating every literal would double the length of a create plan.
   This also matches today: explicit values are unannotated now.
3. Otherwise, if `scopeLabel(v.Scope) != ""` → `[<source>, from <label>]`.
4. Otherwise (`ScopeUnset`, or a `Scope` value this function was never taught)
   → `[default]` when `Source == SourceDefault`, else nothing. This is exactly
   M2/M3 behaviour, and it is what values that predate M4 get — including every
   value read back from state, which carries `SourceProvider` and
   `ScopeUnset` because `providers/test/provider.go:272` stamps
   `SourceProvider` on everything a provider reports. State records what was
   observed, not which configuration layer asked for it; inventing a scope for
   those would be a confident wrong answer.

The seven scopes render as:

| Scope | Label | Example line |
|-------|-------|--------------|
| `ScopeUnset` | *(no scope clause; rule 4)* | `size: 10 [default]` |
| `ScopeProviderDefault` | `provider defaults` | `size: 10 [default, from provider defaults]` |
| `ScopeBaseConfig` | `base config` | `cidr: "10.0.0.0/16" [variable, from base config]` |
| `ScopeModuleDefault` | `module defaults` | *(M5 populates; label exists now so M5 adds no rendering)* |
| `ScopeEnvironmentInherit` | `inherited environment` | `cidr: "10.1.0.0/16" [environment, from inherited environment]` |
| `ScopeEnvironmentVar` | `environment config` | `cidr: "10.2.0.0/16" [environment, from environment config]` |
| `ScopeCLIOverride` | `--var` | `replicas: 20 [variable, from --var]` |

`ScopeCLIOverride` renders `--var` for `--var-file` values too. That is exact
under Task 8's published semantics ("`--var-file f` applies each pair as if it
had been given with `--var`"), and it is the only option available: the
filename cannot be carried, because `internal/expressions/eval.go:53` replaces a
variable's `Origin` with the referencing attribute's when it resolves the
reference.

Spec §12.3's "values sourced from defaults annotated `[default]`" is still
satisfied: `[default, from provider defaults]` names `default` as the source.

**M7 follow-up, recorded not solved:** `infra explain` will want to name the
FILE a value came from, which needs a carrier that survives expression
evaluation — `Value.Origin` does not, because
`internal/expressions/eval.go:53` returns `v.WithOrigin(e.Origin)` and replaces
the variable's origin with the referencing attribute's. Do not add one in M4;
the plan has no use for it beyond this annotation, and a carrier added
speculatively is a field nothing populates correctly.

**Determinism.** The annotation is a pure function of two scalar fields. No
map iteration, no time, no I/O. `scopeLabel` is a `switch`, not a slice index,
so a `Scope` outside the enum returns `""` and falls to rule 4 rather than
panicking on an out-of-range index.

**Redaction.** The annotation is appended to `renderLeaf`'s output and never
touches `Raw`. `pkg/value.Format` remains the one redaction path; a sensitive
value renders `<sensitive> [variable, from --var]` — redacted regardless of
scope, and its scope still disclosed, which leaks nothing (a precedence level is
metadata, not data).

**`internal/cli/state.go` needs no change.** `state show` renders state values,
which are `ScopeUnset` — see rule 4.

### Files

| Action | Path |
|--------|------|
| modify | `internal/planner/render.go` |
| modify | `internal/planner/render_test.go` |
| create | `internal/planner/testdata/scopes.golden` |

`internal/compiler/schema.go` is deliberately NOT in this list: stamping
`ScopeProviderDefault` on a filled default is Task 7's, for the reason in step
9.3.

### Interfaces

Consumes: `value.Scope`, `Value.Scope`, `Value.WithScope` (Task 1),
`value.Format`.
Produces: nothing exported. `scopeLabel` and the modified `renderAnnotated` are
package-private to `internal/planner`.

### Steps

**9.1 — Failing test for the annotation.**

Add to `internal/planner/render_test.go`:

```go
func TestRenderAnnotatesEveryScope(t *testing.T) {
	cases := []struct {
		name string
		v    value.Value
		want string
	}{
		{
			name: "unset scope keeps the M2 annotation",
			v:    value.Int(10, value.SourceDefault),
			want: "10 [default]",
		},
		{
			name: "explicit is never annotated whatever its scope",
			v:    value.String("web", value.SourceExplicit).WithScope(value.ScopeBaseConfig),
			want: `"web"`,
		},
		{
			name: "provider default",
			v:    value.Int(10, value.SourceDefault).WithScope(value.ScopeProviderDefault),
			want: "10 [default, from provider defaults]",
		},
		{
			name: "base config",
			v:    value.String("10.0.0.0/16", value.SourceVariable).WithScope(value.ScopeBaseConfig),
			want: `"10.0.0.0/16" [variable, from base config]`,
		},
		{
			name: "module default",
			v:    value.Int(2, value.SourceModule).WithScope(value.ScopeModuleDefault),
			want: "2 [module, from module defaults]",
		},
		{
			name: "environment inheritance",
			v:    value.String("small", value.SourceEnvironment).WithScope(value.ScopeEnvironmentInherit),
			want: `"small" [environment, from inherited environment]`,
		},
		{
			name: "environment variables",
			v:    value.String("large", value.SourceEnvironment).WithScope(value.ScopeEnvironmentVar),
			want: `"large" [environment, from environment config]`,
		},
		{
			name: "cli override — the shape the user fixed",
			v:    value.Int(20, value.SourceVariable).WithScope(value.ScopeCLIOverride),
			want: "20 [variable, from --var]",
		},
		{
			name: "unknown values are not annotated",
			v:    value.Unknown(value.KindString, value.SourceComputed).WithScope(value.ScopeCLIOverride),
			want: "(known after apply)",
		},
		{
			name: "a sensitive value redacts regardless of scope",
			v:    value.String("hunter2", value.SourceVariable).WithScope(value.ScopeCLIOverride).WithSensitive(true),
			want: "<sensitive> [variable, from --var]",
		},
		{
			name: "a scope outside the enum falls back rather than panicking",
			v:    value.Int(1, value.SourceDefault).WithScope(value.Scope(200)),
			want: "1 [default]",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderAnnotated(tc.v)
			if got != tc.want {
				t.Errorf("renderAnnotated = %q, want %q", got, tc.want)
			}
			if tc.v.Sensitive && strings.Contains(got, "hunter2") {
				t.Errorf("the datum leaked through the annotation path: %q", got)
			}
		})
	}
}
```

Expected failure: every scoped case reports the bare value with no scope clause.

**9.2 — Implement.**

In `internal/planner/render.go`, replace `renderAnnotated` and add `scopeLabel`:

```go
// renderAnnotated renders one value plus, when it applies, the note saying
// where the value came from.
//
// Two orthogonal facts are disclosed: Source is WHAT KIND of thing the value is
// (a default, a variable, a value the provider reported), Scope is WHICH
// PRECEDENCE LEVEL supplied it. A plan that could not tell a --var from a
// variables.yml entry cannot explain itself, and `infra explain` (M7) would
// inherit that blindness permanently.
//
// Explicit values are deliberately unannotated: they are what the reader wrote
// in the file they are looking at, and annotating every one of them would
// double the length of a create plan for no information.
func renderAnnotated(v value.Value) string {
	s := renderLeaf(v)
	if a := annotation(v); a != "" {
		s += " " + a
	}
	return s
}

// annotation returns the bracketed provenance note, or "" when there is
// nothing worth saying.
func annotation(v value.Value) string {
	if !v.Known {
		// An unknown already says "(known after apply)". Its provenance is
		// that it will be computed, which the text already states.
		return ""
	}
	if v.Source == value.SourceExplicit {
		return ""
	}
	if label := scopeLabel(v.Scope); label != "" {
		return "[" + string(v.Source) + ", from " + label + "]"
	}
	// ScopeUnset: a value that predates M4, or one read back from state, where
	// provenance records what a provider reported rather than which
	// configuration layer asked for it. Fall back to M2's annotation, which is
	// still true.
	if v.Source == value.SourceDefault {
		return "[default]"
	}
	return ""
}

// scopeLabel names a precedence level for a reader, or returns "" for
// ScopeUnset and for any value outside the enum.
//
// A switch rather than a table lookup: an out-of-range index would panic, and
// Render is called on every plan a user is asked to approve.
func scopeLabel(s value.Scope) string {
	switch s {
	case value.ScopeProviderDefault:
		return "provider defaults"
	case value.ScopeBaseConfig:
		return "base config"
	case value.ScopeModuleDefault:
		return "module defaults"
	case value.ScopeEnvironmentInherit:
		return "inherited environment"
	case value.ScopeEnvironmentVar:
		return "environment config"
	case value.ScopeCLIOverride:
		// --var-file values arrive here too: `--var-file f` is defined as
		// applying each of f's entries as if it had been given with --var
		// (Task 8), so this label is exact for both. The filename cannot be
		// shown — expressions/eval.go replaces a variable's Origin with the
		// referencing attribute's when it resolves the reference.
		return "--var"
	default:
		return ""
	}
}
```

`go test -count=1 ./internal/planner/` — the new test passes.
`testdata/mixed.golden`'s `size: 10 [default]` is unchanged, because that
fixture's value carries `ScopeUnset` (rule 4). If any golden did change, the
change is a bug in the rule, not in the golden — re-read rule 4 before
regenerating anything.

**9.3 — A golden covering the ladder.**

`ScopeProviderDefault` is stamped by **Task 7**, not here. The gap is real —
provider defaults are filled in stage 7 (`internal/compiler/schema.go`'s
`applyDefaults`), so nothing in stages 3 and 4 sees them, and without a stamp
the lowest rung of PLAN.md §7's chain is invisible in every plan: a chain that
cannot show its own floor. But stamping the bottom of the chain is wiring, and
this is a rendering task. A rendering task reaching into `internal/compiler`
recreates the two-tasks-editing-one-file collision the plan was split to avoid.
Task 7 owns the one-line change at `applyDefaults`' single assignment site and
the test that pins it; this task renders whatever `Scope` it is handed.

If Task 7's stamp is missing when you get here, a provider default renders
`size: 10 [default]` (rule 4) rather than
`size: 10 [default, from provider defaults]`. Report that as a Task 7 defect —
do not fix it from here, and do not weaken the golden below to match it.

Add a golden covering a whole plan with mixed scopes. Build the fixture in
`render_test.go` alongside the existing ones and write
`internal/planner/testdata/scopes.golden`:

```
Plan for project "myapp", environment "production":

  + test.database.db
      engine: "postgres"
      network: "net-1" [variable, from base config]
      size: 100 [default, from provider defaults]

  ~ test.network.net
      cidr: "10.0.0.0/16" [variable, from base config] -> "10.9.0.0/16" [variable, from --var]

Plan: 1 to create, 1 to update, 0 to replace, 0 to destroy, 0 to forget.
```

Assert determinism explicitly by rendering that plan 100 times and comparing:
`TestRenderIsDeterministicAcrossRepeatedCalls` already exists — extend its
fixture with scoped values rather than writing a second determinism test
(`grep -n "TestRenderIsDeterministic" internal/planner/render_test.go`).

**9.4 — Full run and commit.**

```bash
export PATH="$HOME/.local/share/mise/shims:$PATH"
gofmt -l . && go vet ./... && go test -count=1 ./...
git commit -am "M4 Task 9: render which precedence level supplied each value"
```

---

## Task 10 — the three contract invariants, proved end to end

### Why this task exists

Task 1 pins the three invariants as unit tests on `pkg/value`. This task proves
them through the real binary, because that is where M3's Criticals actually
surfaced: a unit test can hold while the pipeline that assembles the values
breaks the property.

### Read this before writing the tests: what invariant 3 does and does not mean

The M4 contract's invariant 3 is about the `Value` JSON codec: a `Scope` that is
written and not read back would make `state show` and `explain` report
`ScopeUnset` for values that have a scope. It is NOT a requirement that a scope
survive an apply. It cannot, and that is correct rather than a gap:

```bash
grep -n "WithSource(value.SourceProvider)" providers/test/provider.go   # line 272
```

Every value a provider reports is restamped `SourceProvider`. Three kinds of
metadata travel differently, and conflating them is what M3 spent a Critical
separating:

- **Sensitivity is a property OF THE VALUE.** A secret stays a secret wherever
  it travels, so it MUST cross the provider boundary. M3 fixed exactly that.
- **Lifecycle is a property OF THE RESOURCE that only state can carry**, because
  at destroy time the resource is gone from configuration. M3 fixed that too.
- **`Scope` is a property of HOW A VALUE WAS SUPPLIED at plan time.** Once the
  provider owns the value, no precedence level supplied it. "Which of the six
  scopes won" has no answer for a value the chain never produced, and
  `ScopeUnset` is the truthful answer — not a lost one.

This looks like M3's Critical (the engine recording what the provider returned
and dropping what the engine knew) and is the opposite of it. Do not try to
carry `Scope` across the provider boundary: that reintroduces the conflation M3
removed.

What this task therefore asserts:

- **the write end**, through the plan artifact: `infra plan --output p.json`
  serialises desired values, and the test decodes that file with the production
  `value.Value` codec and reads the `Scope` back. That is a genuine
  marshal-by-the-binary / unmarshal-by-the-codec round trip.
- **the boundary**, in state: every attribute in `.infra/<env>.json` has
  `source: "provider"` and no recorded scope. If a later change starts stamping
  configuration scope into state, `state show` would claim a value came from
  `--var` when what is recorded is what the provider reported — and this test
  catches it.

The full JSON round trip of the field itself stays where it belongs: Task 1's
unit test on `pkg/value`. The apply-to-state leg is recorded in the Definition
of Done as **deliberately not applicable, with the reasoning above** — never as
"not met". "Not met" reads as a gap someone should close, and the someone who
tried would carry `Scope` across the provider boundary.

### Files

| Action | Path |
|--------|------|
| create | `tests/integration/m4_invariants_test.go` |

This file imports `infra/pkg/value` — the first integration file to import an
infra package. That narrows the stale-cache hole the Makefile's `-count=1`
comment describes (changes to `pkg/value` now invalidate the cache) and closes
none of it: the binary under test is produced by a `go build` subprocess that no
import graph tracks. Keep `-count=1` on every run.

Before writing helpers, check for name collisions:

```bash
grep -rn "^func " tests/integration/*_test.go
```

### Interfaces

Consumes: the built binary via `run` and `project` (`helpers_test.go`),
`value.Value`'s `UnmarshalJSON` (`pkg/value/json.go`).
Produces: nothing importable.

### Steps

**10.1 — Helpers and the shared fixture.**

```go
package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"infra/pkg/value"
)

// writeIn writes one file into an existing project directory. `project` only
// writes infra.yml; M4 fixtures need variables.yml and environments/*.yml too.
func writeIn(t *testing.T, dir, name, body string) {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// planArtifact is the part of a saved plan these tests read. It is a local
// struct rather than planner.Plan because Plan has no UnmarshalJSON until M6 —
// but the map values are real value.Value, so decoding exercises the
// production codec on bytes the production binary wrote.
type planArtifact struct {
	ConfigHash string `json:"config_hash"`
	Operations []struct {
		Address string                 `json:"address"`
		Before  map[string]value.Value `json:"before"`
		After   map[string]value.Value `json:"after"`
	} `json:"operations"`
}

func readPlanArtifact(t *testing.T, path string) planArtifact {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading plan artifact: %v", err)
	}
	var p planArtifact
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatalf("decoding plan artifact: %v", err)
	}
	return p
}

func opAfter(t *testing.T, p planArtifact, address, attr string) value.Value {
	t.Helper()
	for _, op := range p.Operations {
		if op.Address == address {
			v, ok := op.After[attr]
			if !ok {
				t.Fatalf("operation %s has no attribute %q", address, attr)
			}
			return v
		}
	}
	t.Fatalf("no operation for %s in the plan artifact", address)
	return value.Value{}
}

// varProject writes a project whose one attribute is fed by a variable. cidr
// is a plain string attribute, so no typed variable declaration is needed and
// these tests do not depend on Task 4.
func varProject(t *testing.T, variablesYML string) string {
	t.Helper()
	dir := project(t, `
project: myapp
resources:
  net:
    type: test.network
    cidr: ${cidr}
`)
	writeIn(t, dir, "variables.yml", variablesYML)
	return dir
}
```

**10.2 — Invariant 1: `Equal` ignores `Scope`.**

```go
// TestAVarMatchingTheFileValueIsNotAChange is acceptance invariant 2 (no-op
// plan) under the new field. A --var that repeats what variables.yml already
// said changes the Scope of the desired value and nothing else. If value.Equal
// compared Scope, the desired value (ScopeCLIOverride) would differ from the
// recorded one (ScopeUnset, because state records what the provider reported)
// and every plan would propose a change forever — the phantom-diff shape M3
// spent a Critical fixing.
//
// cidr is ForceNew, so the failure would be loud: a proposed REPLACEMENT of a
// resource nobody asked to change.
func TestAVarMatchingTheFileValueIsNotAChange(t *testing.T) {
	dir := varProject(t, "cidr: 10.0.0.0/16\n")

	if r := run(t, dir, "apply", "dev", "--auto-approve"); r.ExitCode != 2 {
		t.Fatalf("apply exit = %d, want 2 (success with changes)\n%s", r.ExitCode, r.combined())
	}

	r := run(t, dir, "plan", "dev", "--var", "cidr=10.0.0.0/16")
	if r.ExitCode != 0 {
		t.Errorf("plan exit = %d, want 0 (no changes)\n%s", r.ExitCode, r.combined())
	}
	requireContains(t, r.Stdout, "No changes.")
	// Presence of "No changes." is not enough on its own: assert the absence
	// of every changing marker too, so a renderer that printed both could not
	// pass.
	for _, marker := range []string{" -/+ ", "  ~ ", "  + ", "  - "} {
		if strings.Contains(r.Stdout, marker) {
			t.Errorf("plan proposes an operation (%q) for a value that did not change:\n%s", marker, r.Stdout)
		}
	}
	requireContains(t, r.Stdout, "Plan: 0 to create, 0 to update, 0 to replace, 0 to destroy, 0 to forget.")
}
```

**How to break it, and confirm the test catches it:** add
`if v.Scope != other.Scope { return false }` to `Value.Equal` in
`pkg/value/value.go`, then

```bash
go test -count=1 -run TestAVarMatchingTheFileValueIsNotAChange ./tests/integration/
```

It must fail with a proposed replacement. Revert. (Under that sabotage the plain
`plan dev` with no `--var` fails too, because state values always carry
`ScopeUnset` — which is a second reason the sabotage is caught, not a reason to
weaken the test.)

**10.3 — Invariant 2: `ConfigHash` excludes `Scope`.**

**The fixture pairing is load-bearing; read this before changing it.**
`ResolvedConfig.Hash` folds `Source` into the digest — `hashValue` writes
`"source", string(v.Source)` (`internal/compiler/resolved.go:114`) — while this
test's whole claim is that two arms differ ONLY in `Scope`. That is why the pair
is `variables.yml` versus `--var`: both produce `SourceVariable`, so `Source` is
held constant and `Scope` is the only variable. A pairing of `variables.yml`
versus `environments/*.yml` looks equivalent and is not — environment values
carry `SourceEnvironment`, so those two arms would hash differently no matter
what `Scope` does, and the test would fail for a reason its own comment
disclaims. An "improved" fixture that swaps in the environment layer converts
this test from a `Scope` guard into a `Source` tautology.

```go
// TestConfigHashIgnoresWhichLevelSuppliedAValue proves the M4 contract's
// invariant 2 end to end. The same effective configuration, supplied two
// different ways, must fingerprint identically: a value of 10.0.0.0/16 is the
// same input whether it came from a file or a flag, and hashing Scope would
// make an unchanged configuration look stale in M6.
//
// Both arms hold Source constant at SourceVariable — ResolvedConfig.Hash
// DOES fold Source (internal/compiler/resolved.go, hashValue), by design, so a
// pair that differed in Source would fail this test for an unrelated reason.
// If the two arms disagree on Source, fix the producer, not this test.
func TestConfigHashIgnoresWhichLevelSuppliedAValue(t *testing.T) {
	hashOf := func(t *testing.T, dir string, args ...string) string {
		t.Helper()
		out := filepath.Join(dir, "plan.json")
		r := run(t, dir, append([]string{"plan", "dev", "--output", out}, args...)...)
		if r.ExitCode != 2 {
			t.Fatalf("plan exit = %d, want 2\n%s", r.ExitCode, r.combined())
		}
		h := readPlanArtifact(t, out).ConfigHash
		if h == "" {
			t.Fatal("the plan artifact carries no config_hash")
		}
		return h
	}

	fromFile := hashOf(t, varProject(t, "cidr: 10.0.0.0/16\n"))
	fromFlag := hashOf(t, varProject(t, "cidr: 192.168.0.0/16\n"), "--var", "cidr=10.0.0.0/16")
	if fromFile != fromFlag {
		t.Errorf("config_hash differs for one effective configuration supplied two ways:\n"+
			"  variables.yml: %s\n  --var:         %s", fromFile, fromFlag)
	}

	// The fixture must be able to fail: a Hash that ignored the value
	// altogether would pass the assertion above. A different value must
	// produce a different hash.
	different := hashOf(t, varProject(t, "cidr: 192.168.0.0/16\n"), "--var", "cidr=10.9.0.0/16")
	if different == fromFile {
		t.Errorf("config_hash is identical for two different cidr values (%s) — "+
			"the hash does not see the value at all", different)
	}
}
```

**How to break it:** add `strconv.Itoa(int(v.Scope))` to `hashValue`'s
`write("kind", ...)` line in `internal/compiler/resolved.go`. The first
assertion must fail and the third must still pass. Revert.

**10.4 — Invariant 3: `Scope` survives serialisation, and stays out of state.**

```go
// TestScopeSurvivesTheSavedPlanArtifact is the M4 contract's invariant 3 at
// the level the binary can show it. The binary marshals the value; this test unmarshals
// it with the production codec (value.Value.UnmarshalJSON) and reads the field
// back. A Scope written without a JSON tag fails silently — the value reads
// back as ScopeUnset, a confident wrong answer rather than an error — so the
// test asserts the exact scope, never merely that decoding succeeded.
func TestScopeSurvivesTheSavedPlanArtifact(t *testing.T) {
	dir := varProject(t, "cidr: 192.168.0.0/16\n")
	out := filepath.Join(dir, "plan.json")
	if r := run(t, dir, "plan", "dev", "--output", out, "--var", "cidr=10.0.0.0/16"); r.ExitCode != 2 {
		t.Fatalf("plan exit = %d, want 2\n%s", r.ExitCode, r.combined())
	}
	got := opAfter(t, readPlanArtifact(t, out), "net", "cidr")
	if s, _ := got.AsString(); s != "10.0.0.0/16" {
		t.Fatalf("cidr = %#v, want 10.0.0.0/16", got)
	}
	if got.Scope != value.ScopeCLIOverride {
		t.Errorf("cidr: Scope = %v, want ScopeCLIOverride", got.Scope)
	}

	// The middle of the ladder, so the test cannot be satisfied by a codec
	// that returns a constant: the same attribute, supplied by variables.yml
	// instead, must read back ScopeBaseConfig.
	dir2 := varProject(t, "cidr: 10.0.0.0/16\n")
	out2 := filepath.Join(dir2, "plan.json")
	if r := run(t, dir2, "plan", "dev", "--output", out2); r.ExitCode != 2 {
		t.Fatalf("plan exit = %d, want 2\n%s", r.ExitCode, r.combined())
	}
	if s := opAfter(t, readPlanArtifact(t, out2), "net", "cidr").Scope; s != value.ScopeBaseConfig {
		t.Errorf("cidr from variables.yml: Scope = %v, want ScopeBaseConfig", s)
	}
}

// TestStateRecordsObservationNotConfigurationProvenance pins the boundary the
// brief's "apply, then read the scope back from state" assumed away.
// providers/test/provider.go restamps every reported value SourceProvider, so
// state describes what the provider said, not which configuration layer asked
// for it. If a later change starts stamping configuration scope into state,
// `state show` and `explain` would attribute an observed value to --var.
func TestStateRecordsObservationNotConfigurationProvenance(t *testing.T) {
	dir := varProject(t, "cidr: 192.168.0.0/16\n")
	if r := run(t, dir, "apply", "dev", "--auto-approve", "--var", "cidr=10.0.0.0/16"); r.ExitCode != 2 {
		t.Fatalf("apply exit = %d, want 2\n%s", r.ExitCode, r.combined())
	}

	data, err := os.ReadFile(filepath.Join(dir, ".infra", "dev.json"))
	if err != nil {
		t.Fatalf("reading state: %v", err)
	}
	var st struct {
		Resources map[string]struct {
			Attributes map[string]value.Value `json:"attributes"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("decoding state: %v", err)
	}
	if len(st.Resources) == 0 {
		t.Fatal("state records no resources; the fixture applied nothing")
	}
	for name, r := range st.Resources {
		if len(r.Attributes) == 0 {
			t.Errorf("%s: state records no attributes", name)
		}
		for attr, v := range r.Attributes {
			if v.Source != value.SourceProvider {
				t.Errorf("%s.%s: Source = %q, want %q — state records what the provider reported",
					name, attr, v.Source, value.SourceProvider)
			}
			if v.Scope != value.ScopeUnset {
				t.Errorf("%s.%s: Scope = %v, want ScopeUnset — configuration provenance must not "+
					"be written into state", name, attr, v.Scope)
			}
		}
	}
	// The applied value is still the --var one: this test asserts where
	// provenance goes, not that --var was ignored.
	requireContains(t, string(data), "10.0.0.0/16")
}
```

**How to break the artifact test:** delete `Scope`'s field or its json tag from
`wireValue` in `pkg/value/json.go`. Both assertions fail with `ScopeUnset`.
Revert.

**10.5 — Run and commit.**

```bash
export PATH="$HOME/.local/share/mise/shims:$PATH"
go test -count=1 ./tests/integration/
gofmt -l . && go vet ./... && go test -count=1 ./...
git commit -am "M4 Task 10: prove the three Scope invariants through the binary"
```

---

## Task 11 — CLI wiring: one variable resolution for every command that has one

### Why this task exists

Three commands compile configuration (`validate`, `plan`, `apply`) and two do
not (`destroy`, `refresh`). Today each of the three builds its own
`compiler.Options` literal, which is how `validate` came to omit `--var`
entirely and reject configuration that `plan` accepted — the bug
`internal/cli/validate.go`'s doc comment describes. M4 adds a second variable
input (`--var-file`), which is a second chance to make the same mistake in two
places out of three.

### Decisions this task fixes

**`destroy` and `refresh` reject `--var` and `--var-file`.** Neither calls
`config.Load` or `compiler.Compile`: `destroy` synthesises an empty
`compiler.ResolvedConfig` from `state.State.Project` and lets the planner's
"in state, not in config" row do the work, and `refresh` reads state and calls
`Provider.Read`. There is no configuration for a variable to interpolate into,
so a variable cannot affect the outcome. Accepting a flag that cannot affect the
outcome is the advertised-and-ignored shape `checkUnsupportedFlags` exists to
prevent, so both commands refuse it with a message that says why and points at
the command that does take it. The counter-argument was weighed: a wrapper
script that passes `--var` to every subcommand now fails on `destroy`. That is
the intended outcome — the script's author believes the flag does something
there, and it does not.

`destroy.go`'s doc comment currently argues the opposite ("--var/--var-file are
simply inapplicable here … not a broken promise"). Update it — a stale comment
arguing against the code is worse than no comment:

```bash
grep -n "simply inapplicable" internal/cli/destroy.go
```

**`infra validate` takes an OPTIONAL environment.** Spec §16 lists `validate`
with no environment and today it is `cobra.NoArgs`; requiring one would break
every existing invocation. But PLAN.md §9 requires typed variable validation to
happen during `infra validate` specifically, and a variable whose value exists
only in `environments/production.yml` cannot be validated without resolving that
environment. Worse, validating with no environment at all would report
"required variable not set" for a variable every environment does set —
`infra validate` failing on valid configuration.

So: `infra validate <env>` validates that environment. `infra validate` with no
argument validates **every declared environment**, in sorted order; with no
environments declared it validates once with the empty environment, exactly as
today. This is §7.4's "report every problem in one pass" applied to
environments.

A diagnostic produced identically in every validated environment is
environment-independent and is reported once, untagged. Anything else is
reported tagged with the environment it came from, per §44's requirement that an
error say which environment it is about. Diagnostics are never deduplicated
across a subset of environments: an error that occurs in `production` and not in
`dev` is exactly the error a user needs to see attributed.

**Exit codes are unchanged.** `validate`: 0 valid, 1 invalid. There is no exit 2
for `validate` — spec §16's exit 2 means "success with changes present", and
`validate` never computes changes.

### Files

| Action | Path |
|--------|------|
| create | `internal/cli/varopts.go` |
| modify | `internal/cli/validate.go` |
| modify | `internal/cli/validate_test.go` |
| modify | `internal/cli/plan.go` |
| modify | `internal/cli/apply.go` |
| modify | `internal/cli/destroy.go` (guard + doc comment) |
| modify | `internal/cli/refresh.go` (guard) |
| modify | `internal/cli/destroy_test.go`, `internal/cli/refresh_test.go` |
| create | `tests/integration/m4_test.go` |

### Interfaces

Consumes: `loadVarFiles` (Task 8), `parseVars` (`internal/cli/plan.go:142`),
`compiler.Options` incl. `FileVars` (Task 8), `config.Decode`,
`config.EnvironmentDecl` (Task 3).

Produces:

```go
// internal/cli
func compilerOptions(opts *GlobalOptions, environment string) (compiler.Options, diag.Diagnostics)
func rejectVariableFlags(opts *GlobalOptions, command string) error
func environmentsToValidate(dir string, args []string) ([]string, diag.Diagnostics)
func validateProject(dir string, reg *registry.Registry, copts compiler.Options) diag.Diagnostics  // signature CHANGED
```

`validateProject`'s third parameter changes from `vars map[string]string` to the
whole `compiler.Options`. That is the point: a command cannot forget to pass a
variable input it does not know about.

### Steps

**11.1 — Failing test: all three compiling commands see the same layers.**

Create `tests/integration/m4_test.go` (Task 12 extends this same file) — assert
behaviour, not structure, because
"they share a helper" is not the property; "they resolve the same value" is:

```go
func TestValidatePlanAndApplyResolveTheSameVariableLayers(t *testing.T) {
	newDir := func(t *testing.T) string {
		dir := project(t, `
project: myapp
resources:
  net:
    type: test.network
    cidr: ${cidr}
`)
		writeIn(t, dir, "overrides.yml", "cidr: 10.7.0.0/16\n")
		return dir
	}

	// With the file, every command resolves ${cidr}.
	for _, cmd := range [][]string{
		{"validate"},
		{"plan", "dev"},
		{"apply", "dev", "--auto-approve"},
	} {
		dir := newDir(t)
		r := run(t, dir, append(append([]string{}, cmd...), "--var-file", "overrides.yml")...)
		if r.ExitCode == 1 {
			t.Errorf("infra %v --var-file overrides.yml failed:\n%s", cmd, r.combined())
		}
		if strings.Contains(r.combined(), "undefined variable") {
			t.Errorf("infra %v did not consult --var-file:\n%s", cmd, r.combined())
		}
	}

	// Without it, every command fails the same way. The negative arm is what
	// makes the positive arm mean something: a command that ignored the
	// reference entirely would pass the first loop.
	for _, cmd := range [][]string{
		{"validate"},
		{"plan", "dev"},
		{"apply", "dev", "--auto-approve"},
	} {
		dir := newDir(t)
		r := run(t, dir, cmd...)
		if r.ExitCode != 1 {
			t.Errorf("infra %v with no value for ${cidr}: exit = %d, want 1\n%s", cmd, r.ExitCode, r.combined())
		}
		requireContains(t, r.combined(), "undefined variable")
	}
}
```

**11.2 — Implement the shared options builder.**

Create `internal/cli/varopts.go`:

```go
package cli

import (
	"fmt"

	"infra/internal/compiler"
	"infra/internal/diag"
)

// compilerOptions builds the compiler options for a command that compiles
// configuration.
//
// It exists so validate, plan and apply cannot resolve variables three
// slightly different ways. They already did once: validate omitted --var
// entirely and rejected configuration plan accepted (see validate.go's doc
// comment). M4 adds --var-file, which is a second chance to make the same
// mistake in two places out of three.
//
// Diagnostics are returned rather than errors because a bad variable file is a
// configuration problem, and spec §7.4 wants every one of them reported in one
// pass rather than the first one aborting the run.
func compilerOptions(opts *GlobalOptions, environment string) (compiler.Options, diag.Diagnostics) {
	var ds diag.Diagnostics

	vars, err := parseVars(opts.Vars)
	if err != nil {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  err.Error(),
			Action:   "Pass variables as --var name=value.",
		})
	}
	fileVars, fds := loadVarFiles(opts.Dir, opts.VarFiles)
	ds.Extend(fds)

	return compiler.Options{Environment: environment, Vars: vars, FileVars: fileVars}, ds
}

// rejectVariableFlags refuses --var and --var-file for the commands that never
// compile configuration.
//
// destroy synthesises an empty ResolvedConfig from state and refresh reads
// state and calls Provider.Read; neither calls config.Load or compiler.Compile,
// so a variable has nothing to interpolate into and cannot change the outcome.
// Accepting a flag that cannot change the outcome is exactly what
// checkUnsupportedFlags refuses for an unwired flag, and the reasoning does not
// change because the flag works elsewhere.
func rejectVariableFlags(opts *GlobalOptions, command string) error {
	if len(opts.Vars) == 0 && len(opts.VarFiles) == 0 {
		return nil
	}
	return fmt.Errorf("%s does not take --var or --var-file: it works from recorded state rather "+
		"than from configuration, so a variable has nothing to interpolate into. "+
		"Use `infra plan <environment>` to see what configuration would change", command)
}
```

Then in `plan.go` and `apply.go`, replace the `parseVars` block and the
`compiler.Options{...}` literal with:

```go
			copts, cds := compilerOptions(opts, environment)
			if cds.HasErrors() {
				cds.Render(cmd.ErrOrStderr())
				return errors.New("configuration is not valid")
			}
			...
			cfg, ds := compiler.Compile(files, reg, copts)
			ds.Extend(cds)
```

and add, as the first statement of `destroy`'s and `refresh`'s `RunE`:

```go
			if err := rejectVariableFlags(opts, "destroy"); err != nil {   // "refresh" in refresh.go
				return err
			}
```

**11.3 — Failing test: validate takes an optional environment and covers all of them.**

Add to `tests/integration/m4_test.go`. Six environments, not two: with six keys
an implementation that iterates a Go map without sorting produces the sorted
order by chance once in 720 runs, so this fails 719 times in 720 against an
unsorted implementation. Two keys would pass roughly 88% of the time against
broken code, which is not a test.

```go
func TestValidateWithNoArgumentChecksEveryDeclaredEnvironment(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  net:
    type: test.network
    cidr: ${cidr}
`)
	// Five environments set cidr; the sixth does not, so validate must fail
	// and must name the one that is broken.
	for _, env := range []string{"alpha", "bravo", "charlie", "delta", "echo"} {
		writeIn(t, dir, "environments/"+env+".yml", "cidr: 10.0.0.0/16\n")
	}
	writeIn(t, dir, "environments/foxtrot.yml", "unrelated: 1\n")

	r := run(t, dir, "validate")
	if r.ExitCode != 1 {
		t.Fatalf("validate exit = %d, want 1 — an environment with no value for ${cidr} is invalid\n%s",
			r.ExitCode, r.combined())
	}
	requireContains(t, r.combined(), "foxtrot")
	// Absence as well as presence: the five good environments must not be
	// blamed for the sixth's problem.
	for _, env := range []string{"alpha", "bravo", "charlie", "delta", "echo"} {
		if strings.Contains(r.combined(), "environment \""+env+"\"") {
			t.Errorf("validate blames %s for foxtrot's missing variable:\n%s", env, r.combined())
		}
	}

	// Naming an environment validates only that one.
	if g := run(t, dir, "validate", "alpha"); g.ExitCode != 0 {
		t.Errorf("validate alpha exit = %d, want 0\n%s", g.ExitCode, g.combined())
	}
	if b := run(t, dir, "validate", "foxtrot"); b.ExitCode != 1 {
		t.Errorf("validate foxtrot exit = %d, want 1\n%s", b.ExitCode, b.combined())
	}
}

func TestValidateReportsAnEnvironmentIndependentErrorOnce(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  net:
    type: test.nosuchtype
    cidr: 10.0.0.0/16
`)
	for _, env := range []string{"alpha", "bravo", "charlie"} {
		writeIn(t, dir, "environments/"+env+".yml", "unused: 1\n")
	}

	r := run(t, dir, "validate")
	if r.ExitCode != 1 {
		t.Fatalf("validate exit = %d, want 1\n%s", r.ExitCode, r.combined())
	}
	if n := strings.Count(r.combined(), "test.nosuchtype"); n != 1 {
		t.Errorf("an unknown resource type is environment-independent and must be reported once, got %d:\n%s",
			n, r.combined())
	}
}
```

**11.4 — Implement validate.**

`internal/cli/validate.go`:

```go
// newValidateCommand builds `infra validate [environment]`.
//
// The environment argument is optional. Spec §16 lists validate without one and
// today it is NoArgs, so requiring one would break every existing invocation.
// But PLAN.md §9 requires typed variable validation to happen during validate,
// and a variable whose value exists only in environments/production.yml cannot
// be checked without resolving that environment — while validating with NO
// environment would report "required variable not set" for a variable every
// environment does set, which is validate failing on valid configuration.
//
// So: no argument validates every declared environment, which is spec §7.4's
// "report every problem in one pass" applied to environments. Exit 0 valid,
// exit 1 invalid; there is no exit 2 here, because §16's exit 2 means "success
// with changes present" and validate never computes changes.
func newValidateCommand(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:           "validate [environment]",
		Short:         "Check configuration for errors without contacting providers",
		Args:          cobra.MaximumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			envs, ds := environmentsToValidate(opts.Dir, args)
			if ds.HasErrors() {
				ds.Render(cmd.ErrOrStderr())
				return errors.New("configuration is not valid")
			}

			reg := buildRegistry(opts.Dir)
			perEnv := make([]diag.Diagnostics, len(envs))
			for i, env := range envs {
				copts, cds := compilerOptions(opts, env)
				if !cds.HasErrors() {
					cds.Extend(validateProject(opts.Dir, reg, copts))
				}
				perEnv[i] = cds
			}
			ds.Extend(foldByEnvironment(envs, perEnv))
			ds.Render(cmd.ErrOrStderr())

			if ds.HasErrors() {
				return errors.New("configuration is not valid")
			}
			fmt.Fprintln(cmd.OutOrStdout(), "✓ Configuration valid")
			return nil
		},
	}
}

// environmentsToValidate decides which environments one `infra validate` run
// covers: the named one, or every declared one, or the empty environment for a
// project that declares none.
func environmentsToValidate(dir string, args []string) ([]string, diag.Diagnostics) {
	var ds diag.Diagnostics
	if len(args) == 1 {
		return []string{args[0]}, ds
	}

	files, err := config.Load(dir)
	if err != nil {
		ds.Add(diag.Diagnostic{Severity: diag.SeverityError, Summary: err.Error()})
		return nil, ds
	}
	project, dds := config.Decode(files)
	ds.Extend(dds)
	if dds.HasErrors() {
		// The per-environment pass would report the same syntax errors once
		// per environment; returning here reports them once.
		return nil, ds
	}

	names := environmentNames(project)
	if len(names) == 0 {
		// A project with no environments validates exactly as it did in M3.
		return []string{""}, ds
	}
	return names, ds
}

// environmentNames lists declared environments in sorted order.
//
// The sort is not cosmetic: with six environments, an unsorted Go map iteration
// lands on sorted order about once in 720 runs, so unsorted output would be a
// diagnostic list that reorders itself between runs of the same command.
func environmentNames(p *config.ProjectDecl) []string {
	out := make([]string, 0, len(p.Environments))
	for _, e := range p.Environments {
		out = append(out, e.Name)
	}
	sort.Strings(out)
	return out
}

// foldByEnvironment merges per-environment diagnostics into one list.
//
// A diagnostic produced identically in EVERY validated environment is
// environment-independent — an unknown resource type does not become known in
// staging — so it is reported once, untagged. Anything else is tagged with the
// environment it came from, per spec §44: an error says which environment it is
// about. Nothing is deduplicated across a subset of environments; an error that
// occurs in production and not in dev is precisely the error a user needs
// attributed.
//
// Order comes from the slices, never from the maps: the maps are consulted by
// key and never ranged over, so output is byte-stable across runs.
func foldByEnvironment(envs []string, perEnv []diag.Diagnostics) diag.Diagnostics {
	type key struct {
		severity diag.Severity
		summary  string
		detail   string
		action   string
		origin   string
	}
	keyOf := func(d diag.Diagnostic) key {
		return key{d.Severity, d.Summary, d.Detail, d.Action, d.Origin.String()}
	}

	counts := map[key]int{}
	for _, ds := range perEnv {
		seen := map[key]bool{}
		for _, d := range ds {
			k := keyOf(d)
			if !seen[k] {
				counts[k]++
				seen[k] = true
			}
		}
	}

	var out diag.Diagnostics
	emitted := map[key]bool{}
	for i, ds := range perEnv {
		for _, d := range ds {
			k := keyOf(d)
			if counts[k] == len(perEnv) {
				if emitted[k] {
					continue
				}
				emitted[k] = true
				out.Add(d)
				continue
			}
			if envs[i] != "" {
				d.Summary = "environment " + strconv.Quote(envs[i]) + ": " + d.Summary
			}
			out.Add(d)
		}
	}
	return out
}

// validateProject runs the full compiler pipeline — stages 1 through 8 — for
// one environment and returns every diagnostic it produces.
//
// It takes the whole compiler.Options rather than just the --var map so that a
// command cannot forget to pass a variable input it does not know about. It
// used to take only vars, and the version before that took none at all, which
// is how `infra validate --var cidr=10.0.0.0/16` came to report
//
//	Error: undefined variable "cidr"
//
// for configuration `infra plan dev --var cidr=10.0.0.0/16` planned without
// complaint.
func validateProject(dir string, reg *registry.Registry, copts compiler.Options) diag.Diagnostics {
	files, err := config.Load(dir)
	if err != nil {
		var ds diag.Diagnostics
		ds.Add(diag.Diagnostic{Severity: diag.SeverityError, Summary: err.Error()})
		return ds
	}
	_, ds := compiler.Compile(files, reg, copts)
	return ds
}
```

Adapt `environmentNames` to Task 3's actual shape:

```bash
grep -rn "EnvironmentDecl" internal/config/
```

If `ProjectDecl.Environments` is a `map[string]EnvironmentDecl`, range the keys
instead — and keep the `sort.Strings`, which then becomes load-bearing rather
than defensive.

**11.5 — Guard tests for destroy and refresh.**

```go
func TestDestroyAndRefreshRefuseVariableFlags(t *testing.T) {
	dir := varProject(t, "cidr: 10.0.0.0/16\n")
	if r := run(t, dir, "apply", "dev", "--auto-approve"); r.ExitCode != 2 {
		t.Fatalf("apply exit = %d\n%s", r.ExitCode, r.combined())
	}

	for _, tc := range []struct {
		args []string
	}{
		{[]string{"destroy", "dev", "--auto-approve", "--var", "cidr=10.1.0.0/16"}},
		{[]string{"destroy", "dev", "--auto-approve", "--var-file", "variables.yml"}},
		{[]string{"refresh", "dev", "--var", "cidr=10.1.0.0/16"}},
		{[]string{"refresh", "dev", "--var-file", "variables.yml"}},
	} {
		r := run(t, dir, tc.args...)
		if r.ExitCode != 1 {
			t.Errorf("infra %v exit = %d, want 1 — a flag that cannot affect the outcome must be "+
				"refused, not ignored\n%s", tc.args, r.ExitCode, r.combined())
		}
		requireContains(t, r.combined(), "does not take --var")
	}

	// And the environment still exists: a refused command must not have run.
	if r := run(t, dir, "plan", "dev"); r.ExitCode != 0 {
		t.Errorf("plan after the refused commands exit = %d, want 0 (nothing should have changed)\n%s",
			r.ExitCode, r.combined())
	}
}
```

**11.6 — Run and commit.**

```bash
export PATH="$HOME/.local/share/mise/shims:$PATH"
gofmt -l . && go vet ./... && go test -count=1 ./...
git commit -am "M4 Task 11: one variable resolution for validate/plan/apply; destroy and refresh refuse variable flags"
```

---

## Task 12 — M4 integration suite and Definition of Done

### Why this task exists

Spec §19's success criteria for M4 are exactly two: "PLAN.md §7 precedence
chain" and "provenance gains four sources". Both are user-visible through the
binary, so both are provable by driving the binary. Everything here drives the
real `infra`; nothing calls an internal package's function to assert a
milestone criterion.

### Files

| Action | Path |
|--------|------|
| modify | `tests/integration/m4_test.go` (created by Task 11) |
| modify | `CLAUDE.md` (current state: M4 merged; `--var-file` no longer errors) |

Tasks 10 and 11 already added tests to `tests/integration/`. This task adds the
criteria suite and the checklist. Reuse `writeIn`, `varProject`,
`readPlanArtifact` from Task 10 rather than writing second copies.

### Steps

**12.1 — The precedence ladder, middle rungs included.**

```go
// attrLine returns the rendered plan line for one attribute of the operation
// whose header ends with `header`. Prose like "look for the cidr line" is not
// a test; this locates the value inside the right operation block, so a plan
// that printed the right value under the wrong resource fails.
func attrLine(t *testing.T, out, header, attr string) string {
	t.Helper()
	lines := strings.Split(out, "\n")
	for i, l := range lines {
		if !strings.HasSuffix(strings.TrimSpace(l), header) {
			continue
		}
		for _, next := range lines[i+1:] {
			trimmed := strings.TrimSpace(next)
			if trimmed == "" {
				break // end of this operation's block
			}
			if strings.HasPrefix(trimmed, attr+":") {
				return trimmed
			}
		}
		t.Fatalf("operation %q has no %q line:\n%s", header, attr, out)
	}
	t.Fatalf("no operation header ending in %q:\n%s", header, out)
	return ""
}

// TestPrecedenceChainEveryRungWins is spec §19's first M4 criterion.
//
// Five variables, each won by a DIFFERENT rung of PLAN.md §7's chain, all five
// defined at every rung below the one that wins. Testing only the top rung
// cannot distinguish a correct implementation from one that always returns the
// last layer it looked at; testing only the bottom cannot distinguish it from
// one that ignores overrides entirely. The middle three are the test.
//
// The module-defaults rung is deliberately absent: M5 populates it, and M4
// leaves the slot.
func TestPrecedenceChainEveryRungWins(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  a:
    type: test.network
    cidr: ${a}
  b:
    type: test.network
    cidr: ${b}
  c:
    type: test.network
    cidr: ${c}
  d:
    type: test.network
    cidr: ${d}
  e:
    type: test.network
    cidr: ${e}
`)
	// rung 2: base configuration
	writeIn(t, dir, "variables.yml", "a: base\nb: base\nc: base\nd: base\ne: base\n")
	// rung 4: environment inheritance
	writeIn(t, dir, "environments/shared.yml", "b: inherited\nc: inherited\nd: inherited\ne: inherited\n")
	// rung 5: the named environment's own variables
	writeIn(t, dir, "environments/prod.yml", "extends: shared\nc: env\nd: env\ne: env\n")
	// rung 6a: --var-file
	writeIn(t, dir, "overrides.yml", "d: file\ne: file\n")

	// rung 6b: --var
	r := run(t, dir, "plan", "prod", "--var-file", "overrides.yml", "--var", "e=cli")
	if r.ExitCode != 2 {
		t.Fatalf("plan exit = %d, want 2\n%s", r.ExitCode, r.combined())
	}

	for _, tc := range []struct{ resource, want, scopeClause string }{
		{"a", `"base"`, "from base config"},
		{"b", `"inherited"`, "from inherited environment"},
		{"c", `"env"`, "from environment config"},
		{"d", `"file"`, "from --var"},
		{"e", `"cli"`, "from --var"},
	} {
		line := attrLine(t, r.Stdout, "test.network."+tc.resource, "cidr")
		if !strings.HasPrefix(line, "cidr: "+tc.want) {
			t.Errorf("resource %s: %q, want the value %s", tc.resource, line, tc.want)
		}
		if !strings.Contains(line, tc.scopeClause) {
			t.Errorf("resource %s: %q does not name the winning scope (%q)", tc.resource, line, tc.scopeClause)
		}
	}

	// Absence as well as presence: each losing layer's value must appear
	// nowhere it did not win. "base" wins once (a) and loses four times.
	for _, tc := range []struct {
		text string
		want int
	}{
		{`"base"`, 1}, {`"inherited"`, 1}, {`"env"`, 1}, {`"file"`, 1}, {`"cli"`, 1},
	} {
		if n := strings.Count(r.Stdout, tc.text); n != tc.want {
			t.Errorf("%s appears %d times in the plan, want %d — a losing layer leaked into a "+
				"resource it did not win:\n%s", tc.text, n, tc.want, r.Stdout)
		}
	}

	// The user's fixed target shape, asserted literally once.
	if line := attrLine(t, r.Stdout, "test.network.e", "cidr"); line != `cidr: "cli" [variable, from --var]` {
		t.Errorf("got %q, want %q", line, `cidr: "cli" [variable, from --var]`)
	}
}
```

If Task 5 spelled inheritance differently from `extends: shared` inside
`environments/prod.yml` — an `environments:` block in `infra.yml`, say — adapt
the fixture. The assertion set is the point, not the spelling:

```bash
grep -rn "extends" internal/environments/ internal/config/
```

**12.2 — `--var-file` stacking, through the binary.**

```go
func TestVarFileStackAppliesInFlagOrder(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  a:
    type: test.network
    cidr: ${a}
  b:
    type: test.network
    cidr: ${b}
  c:
    type: test.network
    cidr: ${c}
`)
	writeIn(t, dir, "one.yml", "a: one\nb: one\nc: one\n")
	writeIn(t, dir, "two.yml", "b: two\nc: two\n")
	writeIn(t, dir, "three.yml", "c: three\n")

	r := run(t, dir, "plan", "dev", "--var-file", "one.yml", "--var-file", "two.yml", "--var-file", "three.yml")
	if r.ExitCode != 2 {
		t.Fatalf("plan exit = %d, want 2\n%s", r.ExitCode, r.combined())
	}
	// The middle file's value must survive for b: an implementation that read
	// only the last file, or only the first, fails here and passes a
	// two-file test.
	for _, tc := range []struct{ resource, want string }{
		{"a", `"one"`}, {"b", `"two"`}, {"c", `"three"`},
	} {
		line := attrLine(t, r.Stdout, "test.network."+tc.resource, "cidr")
		if !strings.HasPrefix(line, "cidr: "+tc.want) {
			t.Errorf("resource %s: %q, want %s", tc.resource, line, tc.want)
		}
	}
}

func TestMissingVarFileIsReportedNotIgnored(t *testing.T) {
	dir := varProject(t, "cidr: 10.0.0.0/16\n")
	r := run(t, dir, "plan", "dev", "--var-file", "nope.yml")
	if r.ExitCode != 1 {
		t.Errorf("plan exit = %d, want 1 — a --var-file that cannot be read is an error\n%s",
			r.ExitCode, r.combined())
	}
	requireContains(t, r.combined(), "nope.yml")
}
```

**12.3 — Typed variables validated during `infra validate` (PLAN.md §9).**

```go
func typedProject(t *testing.T) string {
	t.Helper()
	return project(t, `
project: myapp
variables:
  size:
    type: integer
    default: 10
    min: 1
    max: 100
resources:
  net:
    type: test.network
    cidr: 10.0.0.0/16
  db:
    type: test.database
    engine: postgres
    network: ${net.id}
    size: ${size}
`)
}

// TestTypedVariablesAreValidatedDuringValidate is PLAN.md §9's "validation must
// happen during infra validate", asserted on `validate` specifically and not
// on plan — §9 names the command.
func TestTypedVariablesAreValidatedDuringValidate(t *testing.T) {
	dir := typedProject(t)

	if r := run(t, dir, "validate"); r.ExitCode != 0 {
		t.Fatalf("validate exit = %d, want 0 — the declared default satisfies the schema\n%s",
			r.ExitCode, r.combined())
	}

	over := run(t, dir, "validate", "--var", "size=1000")
	if over.ExitCode != 1 {
		t.Errorf("validate --var size=1000 exit = %d, want 1\n%s", over.ExitCode, over.combined())
	}
	requireContains(t, over.combined(), "size")
	requireContains(t, over.combined(), "100") // the bound it violated (spec §44: what was expected)

	wrongType := run(t, dir, "validate", "--var", "size=abc")
	if wrongType.ExitCode != 1 {
		t.Errorf("validate --var size=abc exit = %d, want 1\n%s", wrongType.ExitCode, wrongType.combined())
	}
	requireContains(t, wrongType.combined(), "integer")
}

// TestATypedVariableKeepsItsTypeThroughTheChain: --var carries strings, so
// without the declared type `size: ${size}` would reach an integer attribute
// as a string and fail schema binding with a kind mismatch the user cannot fix
// from YAML.
func TestATypedVariableKeepsItsTypeThroughTheChain(t *testing.T) {
	dir := typedProject(t)

	r := run(t, dir, "plan", "dev", "--var", "size=42")
	if r.ExitCode != 2 {
		t.Fatalf("plan exit = %d, want 2\n%s", r.ExitCode, r.combined())
	}
	// Unquoted: an integer. `size: "42"` would mean the string survived.
	if line := attrLine(t, r.Stdout, "test.database.db", "size"); line != `size: 42 [variable, from --var]` {
		t.Errorf("got %q, want %q", line, `size: 42 [variable, from --var]`)
	}

	// The declared default rung still renders as an integer and still carries
	// an annotation — the exact source word is Task 6's to fix if it differs,
	// but a declared default is never SourceExplicit and so is never bare.
	d := run(t, dir, "plan", "dev")
	line := attrLine(t, d.Stdout, "test.database.db", "size")
	if !strings.HasPrefix(line, "size: 10 [") {
		t.Errorf("declared default rendered as %q, want `size: 10 [...]`", line)
	}
}
```

**12.4 — A sensitive value stays redacted whichever scope supplied it.**

```go
func TestASensitiveAttributeIsRedactedWhicheverLayerSuppliedIt(t *testing.T) {
	const secret = "hunter2-do-not-print"
	body := `
project: myapp
resources:
  net:
    type: test.network
    cidr: 10.0.0.0/16
  db:
    type: test.database
    engine: postgres
    network: ${net.id}
    password: ${dbpass}
`
	for _, tc := range []struct {
		name string
		args []string
		setup func(t *testing.T, dir string)
	}{
		{
			name:  "from --var",
			args:  []string{"--var", "dbpass=" + secret},
			setup: func(t *testing.T, dir string) {},
		},
		{
			name:  "from --var-file",
			args:  []string{"--var-file", "secrets.yml"},
			setup: func(t *testing.T, dir string) { writeIn(t, dir, "secrets.yml", "dbpass: "+secret+"\n") },
		},
		{
			name:  "from variables.yml",
			args:  nil,
			setup: func(t *testing.T, dir string) { writeIn(t, dir, "variables.yml", "dbpass: "+secret+"\n") },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := project(t, body)
			tc.setup(t, dir)

			r := run(t, dir, append([]string{"plan", "dev"}, tc.args...)...)
			if r.ExitCode != 2 {
				t.Fatalf("plan exit = %d, want 2\n%s", r.ExitCode, r.combined())
			}
			if strings.Contains(r.combined(), secret) {
				t.Errorf("the secret reached the command's output:\n%s", r.combined())
			}
			line := attrLine(t, r.Stdout, "test.database.db", "password")
			if !strings.HasPrefix(line, "password: <sensitive>") {
				t.Errorf("password rendered as %q, want a redacted value", line)
			}
			// The scope is still disclosed. A precedence level is metadata,
			// not data, so saying it leaks nothing — and a plan that redacted
			// the annotation too would be less useful for no gain.
			if !strings.Contains(line, "[") {
				t.Errorf("password line %q carries no provenance annotation", line)
			}
		})
	}
}
```

Composite sensitivity at depth is not re-asserted here: the fake provider
declares no sensitive composite attribute, and per-leaf redaction inside maps
and lists is already pinned by `pkg/value`'s `format_test.go` and
`tests/integration/m3_sensitivity_test.go`. Task 9 adds no new formatting path
— the annotation is appended to `value.Format`'s output and never touches
`Raw`.

**12.5 — Run the whole suite, then the constraint checks.**

```bash
export PATH="$HOME/.local/share/mise/shims:$PATH"
go version                       # go1.24.x
gofmt -l .                       # prints nothing
go vet ./...
go test -count=1 ./...

git diff --stat go.mod go.sum    # prints nothing: still exactly cobra + yaml.v3
grep -rn '"<sensitive>"' --include=*.go . | grep -v _test.go    # only pkg/value/format.go
go list -deps infra/internal/executor | grep infra/internal/cli # prints nothing
grep -rn "variableScope" internal/                              # prints nothing: Task 7 deleted it
```

**12.6 — Update `CLAUDE.md`.**

Its "Current state" section says M1 and M2 are merged and lists variables,
environments and `--var-file` under "Absent until M3-M7", including the line
"`--var-file` errors rather than being silently ignored, which is the standard
to hold". That line is now false, and the standard it names is now kept by
implementing the flag. Update the section to record M4: the precedence chain,
`value.Scope`, `variables.yml`, `environments/*.yml`, `--var-file`, typed
variables, and `infra validate [environment]`. Leave modules, plan artifacts
read back, `explain`, `graph`, `discover` and `import` listed as absent.

**12.7 — Commit.**

```bash
git commit -am "M4 Task 12: integration suite for the precedence chain and provenance"
```

### Definition of Done

Every line names the test that proves it. **A line that cannot be proven is
reported as NOT MET, with what is missing — it is never ticked.** In M3 a
checklist line claimed behaviour that shipped asserted by a checkbox and
exercised by nothing, and a separate line contradicted the spec on an exit code.
A checkbox is not evidence; a named test that fails when the behaviour is
removed is.

**Spec §19 criterion 1 — PLAN.md §7's precedence chain**

| # | Claim | Proof |
|---|-------|-------|
| 1 | Each of the five rungs M4 implements wins where it should, including the middle three | `TestPrecedenceChainEveryRungWins` (12.1) |
| 2 | A losing layer's value appears nowhere it did not win | the `strings.Count` block of `TestPrecedenceChainEveryRungWins` |
| 3 | `--var` beats `--var-file` | `TestCLIVarOutranksAFileEntryAtTheSameScope` (8.5) and rung `e` of `TestPrecedenceChainEveryRungWins` |
| 4 | `--var-file` beats environment configuration | `TestFileEntryAtCLIScopeOutranksEnvironmentConfiguration` (8.5) and rung `d` of `TestPrecedenceChainEveryRungWins` |
| 5 | `variables.yml` loses to environment configuration | `TestFileEntryAtBaseScopeLosesToEnvironmentConfiguration` (8.5) and rungs `b`/`c` |
| 6 | Multiple `--var-file` flags apply in flag order | `TestLoadVarFilesAppliesFilesInFlagOrderSoALaterFileWins` (8.3), `TestVarFileStackAppliesInFlagOrder` (12.2) |
| 7 | The module-defaults rung is left empty for M5 | no test — this is an ABSENCE, recorded here as a design note, not a claim about behaviour |

**Spec §19 criterion 2 — provenance gains four sources**

| # | Claim | Proof |
|---|-------|-------|
| 8 | Every scope renders a distinct, deterministic annotation | `TestRenderAnnotatesEveryScope` (9.1) |
| 9 | `ScopeUnset` keeps M2's `[default]` behaviour, so values from state and from before M4 stay correct | the first and last cases of `TestRenderAnnotatesEveryScope`; `testdata/mixed.golden` unchanged |
| 10 | A filled provider default records `ScopeProviderDefault` | Task 7's test at `internal/compiler/schema.go`'s `applyDefaults`. Not this task's to write; if it is absent, report it against Task 7 rather than ticking this line |
| 11 | Plan output is byte-identical across repeated renders with scoped values | `TestRenderIsDeterministicAcrossRepeatedCalls` (extended in 9.3) |
| 12 | `Scope` survives the saved plan artifact and reads back through the production codec | `TestScopeSurvivesTheSavedPlanArtifact` (10.4) |
| 13 | State records observation, not configuration provenance | `TestStateRecordsObservationNotConfigurationProvenance` (10.4) |
| 14 | The full write-then-read JSON round trip of the field itself | Task 1's `pkg/value` unit test; line 12 is the binary-level half |
| 14a | Scope surviving apply into state | **Deliberately not applicable, not a gap.** `Scope` records how a value was SUPPLIED at plan time; once the provider owns the value no precedence level supplied it, so `ScopeUnset` is the truthful answer (`providers/test/provider.go:272`). Sensitivity crosses the provider boundary because it is a property of the value, and lifecycle lives in state because only state has it at destroy time — `Scope` is neither. Line 13 pins the boundary. Do not close this "gap": doing so reintroduces the conflation M3 removed |

**Contract invariants**

| # | Claim | Proof |
|---|-------|-------|
| 15 | `Equal` ignores `Scope` (acceptance invariant 2 holds) | `TestAVarMatchingTheFileValueIsNotAChange` (10.2), plus Task 1's unit test |
| 16 | `ConfigHash` excludes `Scope` | `TestConfigHashIgnoresWhichLevelSuppliedAValue` (10.3) — including its third arm, which proves the hash sees the value at all |
| 17 | Each of 15-17 fails against a violating implementation | the "How to break it" step under 10.2, 10.3, 10.4. Run each sabotage once and confirm the failure before ticking. |

**PLAN.md §8 and §9**

| # | Claim | Proof |
|---|-------|-------|
| 18 | `variables.yml` and `environments/*.yml` load automatically for `infra plan <env>` | `TestPrecedenceChainEveryRungWins` (12.1) |
| 19 | Typed variables are validated during `infra validate` | `TestTypedVariablesAreValidatedDuringValidate` (12.3) |
| 20 | A declared type survives `--var`'s string form | `TestATypedVariableKeepsItsTypeThroughTheChain` (12.3) |
| 21 | An out-of-range value names the bound it violated | the `requireContains(..., "100")` assertion in 12.3 |

**CLI**

| # | Claim | Proof |
|---|-------|-------|
| 22 | `validate`, `plan`, `apply` resolve identical variable layers | `TestValidatePlanAndApplyResolveTheSameVariableLayers` (11.1), both arms |
| 23 | `infra validate` with no argument covers every declared environment, in sorted order | `TestValidateWithNoArgumentChecksEveryDeclaredEnvironment` (11.3) — six environments, so an unsorted implementation passes about once in 720 runs |
| 24 | An environment-independent error is reported once | `TestValidateReportsAnEnvironmentIndependentErrorOnce` (11.3) |
| 25 | `validate` exits 0 valid / 1 invalid, and never 2 | the exit-code assertions in 11.3 and 12.3. Spec §16's exit 2 means "success with changes present"; `validate` computes no changes |
| 26 | `destroy` and `refresh` refuse `--var`/`--var-file` rather than ignoring them | `TestDestroyAndRefreshRefuseVariableFlags` (11.5) |
| 27 | A missing `--var-file` is an error naming the path | `TestLoadVarFilesReportsAMissingFile` (8.3), `TestMissingVarFileIsReportedNotIgnored` (12.2) |
| 28 | `--var-file` no longer appears in `checkUnsupportedFlags`, and the function survives for the next unwired flag | `TestVarFileReportsAMissingFileRatherThanIgnoringIt` (8.8); `grep -n "checkUnsupportedFlags" internal/cli/root.go` |

**Global constraints**

| # | Claim | Proof |
|---|-------|-------|
| 29 | Exactly two third-party dependencies | `git diff --stat go.mod go.sum` prints nothing (12.5) |
| 30 | One redaction path | `grep -rn '"<sensitive>"' --include=*.go . \| grep -v _test.go` names only `pkg/value/format.go` (12.5) |
| 31 | No sensitive value reaches command output, whichever layer supplied it | `TestASensitiveAttributeIsRedactedWhicheverLayerSuppliedIt` (12.4), all three sub-cases |
| 32 | `internal/executor` does not import `internal/cli` | `go list -deps infra/internal/executor \| grep infra/internal/cli` prints nothing (12.5) |
| 33 | One variable scope implementation | `grep -rn "variableScope" internal/` prints nothing (12.5) |
| 34 | The whole suite passes with the cache disabled | `go test -count=1 ./...` (12.5). Without `-count=1`, `tests/integration` can report a pass it did not earn |
| 35 | Documentation reflects the shipped state | `CLAUDE.md`'s "Current state" updated (12.6); the vault note per the repo's documentation rules |
