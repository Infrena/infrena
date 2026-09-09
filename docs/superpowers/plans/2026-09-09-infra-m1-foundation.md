# infra M1 — Foundation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the foundation of the `infra` core engine — typed values with provenance, addressing, diagnostics, resource schemas, the provider interface, a file-backed fake provider, versioned state with a locking local backend, and the first two compiler stages — so that `infra validate`, `infra state`, and lock contention all work end to end.

**Architecture:** Layered Go packages with a one-directional dependency rule. `pkg/` holds types that providers depend on (`value`, `address`, `schema`, `resource`, `provider`); `internal/` holds engine machinery (`diag`, `registry`, `state`, `config`, `cli`); `providers/test/` implements a fake cloud whose world lives in a hand-editable JSON file. Nothing in `providers/` may import `internal/`. Values are never bare Go types — every resolved value carries its provenance, kind, and origin.

**Tech Stack:** Go 1.24, Cobra (CLI), `gopkg.in/yaml.v3` (parsing with position info), standard library `testing`. No other dependencies in M1.

**Spec:** `docs/superpowers/specs/2026-09-09-infra-phase-1-design.md`

## Global Constraints

- **Toolchain:** Go 1.24 is pinned by `mise.toml` in the repo root, but mise is not active in non-interactive shells. Before running any `go` command, run `export PATH="$HOME/.local/share/mise/shims:$PATH"`. Verify with `go version` — it must report `go1.24.x`, not `go1.20.x`. Building with 1.20 fails immediately on the `go.mod` version directive.
- Go 1.24 or later. Module path is `infra` — a bare, non-URL path, chosen deliberately because the product name is not yet decided (`PLAN.md` §5, "Working Name"); renaming later is a mechanical find-and-replace and baking a hosting decision in now would be premature.
- Dependencies in M1 are exactly two: `github.com/spf13/cobra` and `gopkg.in/yaml.v3`. Adding any third dependency requires a spec amendment.
- `providers/` may import `pkg/`. `providers/` may **never** import `internal/`. Spec §17.
- No raw `map[string]any` may flow through the engine. `internal/config/decode.go` is the only file permitted to touch `yaml.Node`. Spec §7. The one deliberate exception is `providers/test`, whose cloud file models an external system, not internal configuration.
- Every state and plan file on disk is written with mode `0600`. Spec §9.3.
- Test-driven: every task writes a failing test first, and no task is complete until `go test ./...`, `go vet ./...`, and `gofmt -l .` are all clean.
- Commit after every task. Commit messages use Conventional Commits (`feat:`, `test:`, `chore:`, `fix:`).
- Value provenance is never discarded. Any function that returns a resolved value returns a `value.Value`, not its `Raw` contents. Spec §5.1.

---

## File Structure

| File | Responsibility |
|------|----------------|
| `go.mod`, `Makefile` | Module definition; build/test/lint entry points |
| `pkg/value/kind.go` | `Kind` enum and its string form |
| `pkg/value/value.go` | `Value`, `ValueSource`, `Origin`, constructors, `Equal` |
| `pkg/value/json.go` | `Value` JSON round-tripping (needed by state) |
| `pkg/address/address.go` | `Address`, `String`, `ParseAddress` |
| `internal/diag/diagnostic.go` | `Diagnostic`, `Diagnostics`, §44 rendering |
| `pkg/schema/attribute.go` | `Attribute`, `DefaultFunc`, `DefaultContext` |
| `pkg/schema/definition.go` | `ResourceDefinition`, `Requirement`, `Capabilities`, `ImportSpec` |
| `pkg/resource/resource.go` | `Lifecycle`, `ResolvedResource`, `DesiredResource`, `ResourceState` |
| `pkg/provider/provider.go` | `Provider` interface, `Retryability`, discovery types, sentinel errors |
| `internal/registry/registry.go` | Type → definition and provider lookup |
| `providers/test/definitions.go` | Schemas for `test.network`, `test.database`, `test.application` |
| `providers/test/cloud.go` | `.infra/fake-cloud.json` load/save, failure and latency rules |
| `providers/test/provider.go` | Fake provider CRUD against the cloud file |
| `internal/state/state.go` | `State`, `CurrentVersion`, migration chain |
| `internal/state/backend.go` | `Backend` interface, `Lock` descriptor |
| `internal/state/local.go` | Local JSON backend with atomic writes |
| `internal/state/lock.go` | Lock file acquire/release/inspect |
| `internal/config/declarations.go` | Typed unresolved declarations (stage 2 output) |
| `internal/config/load.go` | Stage 1: read files into positioned YAML nodes |
| `internal/config/decode.go` | Stage 2: YAML nodes → typed declarations |
| `internal/cli/root.go` | Cobra root, global flags, exit codes |
| `internal/cli/validate.go` | `infra validate` |
| `internal/cli/state.go` | `infra state list\|show\|unlock` |
| `cmd/infra/main.go` | Entry point |
| `tests/integration/m1_test.go` | CLI-level tests for M1 behaviour |

Rationale for two decisions locked in here: `Registry` lives in `internal/registry` rather than `pkg/schema` because `Provider.Definitions()` returns `*schema.ResourceDefinition`, so a registry inside `schema` would create an import cycle. `Value` JSON handling is its own file because `Raw any` needs custom marshalling to round-trip `Kind` — state persistence depends on it and it deserves isolated tests.

**Note on `Value.Expr`:** spec §5.1 defines `Value` with an `Expr *Expr` field for deferred expressions. M1 omits that field because expressions do not exist until M2. Task 2 of the M2 plan adds it. This is a deliberate deferral, not an oversight.

---

## Task 1: Repository scaffolding

**Files:**
- Create: `go.mod`, `Makefile`, `cmd/infra/main.go`, `internal/cli/root.go`
- Test: `internal/cli/root_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `cli.NewRootCommand() *cobra.Command`, `cli.Execute() int` returning the process exit code.

- [ ] **Step 1: Initialise the module and pin dependencies**

```bash
cd /home/james/projects/ilan
export PATH="$HOME/.local/share/mise/shims:$PATH"
go version   # must report go1.24.x
go mod init infra
go get github.com/spf13/cobra@latest
go get gopkg.in/yaml.v3@latest
go mod tidy
```

Confirm `go.mod` declares `go 1.24` or later. If it declares an older version, edit the `go` directive to `1.24`.

- [ ] **Step 2: Write the failing test**

Create `internal/cli/root_test.go`:

```go
package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestRootCommandHasExpectedSubcommands(t *testing.T) {
	root := NewRootCommand()
	want := []string{"validate", "state"}
	have := map[string]bool{}
	for _, c := range root.Commands() {
		have[c.Name()] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("root command is missing subcommand %q", w)
		}
	}
}

func TestRootCommandGlobalFlags(t *testing.T) {
	root := NewRootCommand()
	for _, name := range []string{"var", "var-file", "verbose", "output"} {
		if root.PersistentFlags().Lookup(name) == nil {
			t.Errorf("missing global flag --%s", name)
		}
	}
}

func TestRootCommandPrintsUsage(t *testing.T) {
	root := NewRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("--help returned error: %v", err)
	}
	if !strings.Contains(out.String(), "infra") {
		t.Errorf("usage output did not mention the tool name: %q", out.String())
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./internal/cli/ -run TestRootCommand -v`
Expected: FAIL — `undefined: NewRootCommand`.

- [ ] **Step 4: Implement the root command**

Create `internal/cli/root.go`:

```go
// Package cli wires the infra command-line interface.
package cli

import (
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

	return root
}

// Execute runs the CLI and returns the process exit code. Human output goes to
// stdout; diagnostics go to stderr, so piping works. Spec §16.
func Execute() int {
	root := NewRootCommand()
	root.SetOut(os.Stdout)
	root.SetErr(os.Stderr)

	if err := root.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return ExitError
	}
	return ExitOK
}
```

Create placeholder-free stubs so the package compiles — these are replaced in Tasks 12 and 13, and each returns a real error rather than pretending to succeed. Create `internal/cli/validate.go`:

```go
package cli

import (
	"errors"

	"github.com/spf13/cobra"
)

func newValidateCommand(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "Check configuration for errors without contacting providers",
		RunE: func(cmd *cobra.Command, args []string) error {
			return errors.New("validate is implemented in Task 14")
		},
	}
}
```

Create `internal/cli/state.go`:

```go
package cli

import (
	"errors"

	"github.com/spf13/cobra"
)

func newStateCommand(opts *GlobalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "state",
		Short: "Inspect and manage recorded state",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List managed resources",
		RunE: func(cmd *cobra.Command, args []string) error {
			return errors.New("state list is implemented in Task 14")
		},
	})
	return cmd
}
```

Create `cmd/infra/main.go`:

```go
// Command infra is the declarative infrastructure management CLI.
package main

import (
	"os"

	"infra/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
```

- [ ] **Step 5: Create the Makefile**

Create `Makefile`. Recipe lines must begin with a literal tab, not spaces:

```make
# The project pins Go 1.24 via mise.toml. The shims directory puts that
# toolchain on PATH even in non-interactive shells, where mise is not active.
export PATH := $(HOME)/.local/share/mise/shims:$(PATH)

GO ?= go

.PHONY: build test vet fmt check

build:
	$(GO) build -o bin/infra ./cmd/infra

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	@test -z "$$(gofmt -l .)" || { echo "unformatted files:"; gofmt -l .; exit 1; }

check: fmt vet test
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `make check`
Expected: PASS — three tests in `internal/cli`, no vet findings, no formatting complaints.

- [ ] **Step 7: Commit**

```bash
git add go.mod go.sum Makefile cmd internal
git commit -m "feat: scaffold Go module and CLI command tree"
```

---

## Task 2: Value type with provenance

**Files:**
- Create: `pkg/value/kind.go`, `pkg/value/value.go`
- Test: `pkg/value/value_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `value.Kind` (`KindString`, `KindInt`, `KindFloat`, `KindBool`, `KindList`, `KindMap`), `value.ValueSource` constants, `value.Origin`, `value.Value`, constructors `String`, `Int`, `Float`, `Bool`, `List`, `Map`, `Unknown`, and methods `WithSensitive`, `WithSource`, `WithOrigin`, `Equal`, `AsString`, `AsInt`, `AsBool`.

- [ ] **Step 1: Write the failing test**

Create `pkg/value/value_test.go`:

```go
package value

import "testing"

func TestConstructorsRecordKindAndSource(t *testing.T) {
	v := String("postgres", SourceExplicit)
	if v.Kind != KindString {
		t.Errorf("Kind = %v, want KindString", v.Kind)
	}
	if v.Source != SourceExplicit {
		t.Errorf("Source = %v, want SourceExplicit", v.Source)
	}
	if !v.Known {
		t.Error("a literal value must be Known")
	}
	if got, ok := v.AsString(); !ok || got != "postgres" {
		t.Errorf("AsString() = %q, %v; want \"postgres\", true", got, ok)
	}
}

func TestUnknownCarriesKindButNoValue(t *testing.T) {
	v := Unknown(KindString, SourceComputed)
	if v.Known {
		t.Error("Unknown must not be Known")
	}
	if v.Kind != KindString {
		t.Errorf("Kind = %v, want KindString — unknown values must keep their kind so type checking still runs", v.Kind)
	}
	if v.Raw != nil {
		t.Errorf("Raw = %v, want nil", v.Raw)
	}
}

func TestEqualIgnoresProvenanceAndOrigin(t *testing.T) {
	a := String("postgres", SourceExplicit)
	b := String("postgres", SourceDefault)
	b.Origin = Origin{File: "other.yml", Line: 9}
	if !a.Equal(b) {
		t.Error("Equal must compare kind and datum only; provenance and origin are not part of desired state")
	}
	if a.Equal(String("mysql", SourceExplicit)) {
		t.Error("different data must not be equal")
	}
}

func TestUnknownIsNeverEqual(t *testing.T) {
	u := Unknown(KindString, SourceComputed)
	if u.Equal(String("x", SourceExplicit)) || u.Equal(Unknown(KindString, SourceComputed)) {
		t.Error("an unknown value can never be proven equal to anything — spec §11")
	}
}

func TestCompositesHoldValuesRecursively(t *testing.T) {
	m := Map(map[string]Value{
		"engine":  String("postgres", SourceExplicit),
		"storage": Int(100, SourceDefault),
	}, SourceExplicit)

	raw, ok := m.Raw.(map[string]Value)
	if !ok {
		t.Fatalf("Map Raw is %T, want map[string]Value — provenance must survive per leaf", m.Raw)
	}
	if raw["storage"].Source != SourceDefault {
		t.Error("per-leaf provenance was lost")
	}
}

func TestWithSensitiveIsSticky(t *testing.T) {
	v := String("hunter2", SourceVariable).WithSensitive(true)
	if !v.Sensitive {
		t.Error("WithSensitive(true) must mark the value sensitive")
	}
	if v.WithSource(SourceModule).Sensitive == false {
		t.Error("changing source must not clear sensitivity")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./pkg/value/ -v`
Expected: FAIL — the package does not exist.

- [ ] **Step 3: Implement Kind**

Create `pkg/value/kind.go`:

```go
// Package value defines the typed, provenance-carrying values that flow through
// the infra engine. Resolved configuration never uses bare Go values or
// map[string]any — see spec §5.1.
package value

// Kind is the type of a Value. An unknown value still carries its Kind so that
// type checking happens at plan time rather than apply time.
type Kind uint8

const (
	KindInvalid Kind = iota
	KindString
	KindInt
	KindFloat
	KindBool
	KindList
	KindMap
)

func (k Kind) String() string {
	switch k {
	case KindString:
		return "string"
	case KindInt:
		return "integer"
	case KindFloat:
		return "float"
	case KindBool:
		return "boolean"
	case KindList:
		return "list"
	case KindMap:
		return "map"
	default:
		return "invalid"
	}
}
```

- [ ] **Step 4: Implement Value**

Create `pkg/value/value.go`:

```go
package value

import "fmt"

// ValueSource records where a resolved value came from. Spec §5.1 and §43 of
// PLAN.md. Provenance is what lets plans mark defaults, import generate minimal
// configuration, and `infra explain` describe behaviour accurately.
type ValueSource string

const (
	SourceExplicit    ValueSource = "explicit"
	SourceDefault     ValueSource = "default"
	SourceEnvironment ValueSource = "environment"
	SourceVariable    ValueSource = "variable"
	SourceModule      ValueSource = "module"
	SourceComputed    ValueSource = "computed"
	SourceProvider    ValueSource = "provider"
)

// Origin locates a value in the source configuration, including the module
// instantiation chain. Compile-time module flattening (spec §7.2) means error
// messages depend entirely on this to say which module a problem came from.
type Origin struct {
	File   string
	Line   int
	Column int
	Module []string
}

func (o Origin) String() string {
	if o.File == "" {
		return "<generated>"
	}
	if o.Line == 0 {
		return o.File
	}
	return fmt.Sprintf("%s:%d:%d", o.File, o.Line, o.Column)
}

// Value is a resolved configuration or state value.
//
// Raw holds a Go value matching Kind: string, int64, float64, bool, []Value or
// map[string]Value. Composites hold Values recursively so provenance is
// per-leaf. When Known is false, Raw is nil.
type Value struct {
	Kind      Kind
	Known     bool
	Raw       any
	Source    ValueSource
	Sensitive bool
	Origin    Origin
}

func String(s string, src ValueSource) Value {
	return Value{Kind: KindString, Known: true, Raw: s, Source: src}
}

func Int(i int64, src ValueSource) Value {
	return Value{Kind: KindInt, Known: true, Raw: i, Source: src}
}

func Float(f float64, src ValueSource) Value {
	return Value{Kind: KindFloat, Known: true, Raw: f, Source: src}
}

func Bool(b bool, src ValueSource) Value {
	return Value{Kind: KindBool, Known: true, Raw: b, Source: src}
}

func List(items []Value, src ValueSource) Value {
	return Value{Kind: KindList, Known: true, Raw: items, Source: src}
}

func Map(items map[string]Value, src ValueSource) Value {
	return Value{Kind: KindMap, Known: true, Raw: items, Source: src}
}

// Unknown builds a value whose datum is not yet determined but whose type is.
func Unknown(k Kind, src ValueSource) Value {
	return Value{Kind: k, Known: false, Source: src}
}

func (v Value) WithSensitive(s bool) Value {
	v.Sensitive = s
	return v
}

func (v Value) WithSource(src ValueSource) Value {
	v.Source = src
	return v
}

func (v Value) WithOrigin(o Origin) Value {
	v.Origin = o
	return v
}

// Equal reports whether two values hold the same datum of the same kind.
// Provenance, sensitivity and origin are deliberately excluded: they describe
// how a value was arrived at, not what the desired state is, so they must never
// cause a plan to show a change.
//
// An unknown value is never equal to anything, including another unknown. The
// planner relies on this: an attribute that cannot be proven unchanged must be
// reported as a change (spec §11).
func (v Value) Equal(other Value) bool {
	if !v.Known || !other.Known {
		return false
	}
	if v.Kind != other.Kind {
		return false
	}
	switch v.Kind {
	case KindList:
		a, _ := v.Raw.([]Value)
		b, _ := other.Raw.([]Value)
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if !a[i].Equal(b[i]) {
				return false
			}
		}
		return true
	case KindMap:
		a, _ := v.Raw.(map[string]Value)
		b, _ := other.Raw.(map[string]Value)
		if len(a) != len(b) {
			return false
		}
		for k, av := range a {
			bv, ok := b[k]
			if !ok || !av.Equal(bv) {
				return false
			}
		}
		return true
	default:
		return v.Raw == other.Raw
	}
}

func (v Value) AsString() (string, bool) {
	s, ok := v.Raw.(string)
	return s, ok && v.Known
}

func (v Value) AsInt() (int64, bool) {
	i, ok := v.Raw.(int64)
	return i, ok && v.Known
}

func (v Value) AsBool() (bool, bool) {
	b, ok := v.Raw.(bool)
	return b, ok && v.Known
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./pkg/value/ -v`
Expected: PASS — six tests.

- [ ] **Step 6: Commit**

```bash
git add pkg/value
git commit -m "feat: add provenance-carrying Value type"
```

---

## Task 3: Value JSON round-tripping

State is persisted as JSON and holds `Value`s. `Raw any` will not round-trip through
`encoding/json` unaided: a `KindInt` decodes back as `float64`, and composites lose their
per-leaf provenance. This task makes persistence lossless.

**Files:**
- Create: `pkg/value/json.go`
- Test: `pkg/value/json_test.go`

**Interfaces:**
- Consumes: `value.Value` from Task 2.
- Produces: `(Value).MarshalJSON`, `(*Value).UnmarshalJSON`. No new exported names.

- [ ] **Step 1: Write the failing test**

Create `pkg/value/json_test.go`:

```go
package value

import (
	"encoding/json"
	"testing"
)

func roundTrip(t *testing.T, in Value) Value {
	t.Helper()
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Value
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal of %s: %v", data, err)
	}
	return out
}

func TestRoundTripPreservesIntKind(t *testing.T) {
	out := roundTrip(t, Int(100, SourceDefault))
	if out.Kind != KindInt {
		t.Fatalf("Kind = %v, want KindInt (encoding/json would give float64)", out.Kind)
	}
	if got, ok := out.AsInt(); !ok || got != 100 {
		t.Errorf("AsInt() = %d, %v; want 100, true", got, ok)
	}
	if out.Source != SourceDefault {
		t.Errorf("Source = %v, want SourceDefault", out.Source)
	}
}

func TestRoundTripPreservesNestedProvenance(t *testing.T) {
	in := Map(map[string]Value{
		"engine":  String("postgres", SourceExplicit),
		"storage": Int(100, SourceDefault),
		"tags":    List([]Value{String("a", SourceModule)}, SourceModule),
	}, SourceExplicit)

	out := roundTrip(t, in)
	m, ok := out.Raw.(map[string]Value)
	if !ok {
		t.Fatalf("Raw is %T, want map[string]Value", out.Raw)
	}
	if m["storage"].Source != SourceDefault {
		t.Errorf("nested Source = %v, want SourceDefault", m["storage"].Source)
	}
	items, ok := m["tags"].Raw.([]Value)
	if !ok || len(items) != 1 || items[0].Source != SourceModule {
		t.Errorf("nested list lost provenance: %#v", m["tags"].Raw)
	}
}

func TestRoundTripPreservesUnknownAndSensitive(t *testing.T) {
	out := roundTrip(t, Unknown(KindString, SourceComputed).WithSensitive(true))
	if out.Known {
		t.Error("unknown must survive as unknown")
	}
	if out.Kind != KindString {
		t.Errorf("Kind = %v, want KindString", out.Kind)
	}
	if !out.Sensitive {
		t.Error("sensitivity must survive persistence")
	}
}

func TestRoundTripPreservesEquality(t *testing.T) {
	in := Bool(true, SourceProvider)
	if !in.Equal(roundTrip(t, in)) {
		t.Error("a persisted value must compare equal to its original, or every plan after a restart shows spurious changes")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./pkg/value/ -run RoundTrip -v`
Expected: FAIL — `TestRoundTripPreservesIntKind` reports `Kind = string` or an unmarshal error, because the default encoding cannot reconstruct `Raw`.

- [ ] **Step 3: Implement custom marshalling**

Create `pkg/value/json.go`:

```go
package value

import (
	"encoding/json"
	"fmt"
)

// wireValue is the on-disk shape of a Value. Kind is written explicitly so that
// integers do not come back as float64 and composites keep per-leaf provenance.
type wireValue struct {
	Kind      string          `json:"kind"`
	Known     bool            `json:"known"`
	Raw       json.RawMessage `json:"raw,omitempty"`
	Source    ValueSource     `json:"source"`
	Sensitive bool            `json:"sensitive,omitempty"`
	Origin    *Origin         `json:"origin,omitempty"`
}

func kindFromString(s string) (Kind, error) {
	for _, k := range []Kind{KindString, KindInt, KindFloat, KindBool, KindList, KindMap} {
		if k.String() == s {
			return k, nil
		}
	}
	return KindInvalid, fmt.Errorf("unknown value kind %q", s)
}

func (v Value) MarshalJSON() ([]byte, error) {
	w := wireValue{
		Kind:      v.Kind.String(),
		Known:     v.Known,
		Source:    v.Source,
		Sensitive: v.Sensitive,
	}
	// Origin cannot be compared with == because Module is a slice, so the
	// zero-check is field by field.
	if v.Origin.File != "" || v.Origin.Line != 0 || v.Origin.Column != 0 || v.Origin.Module != nil {
		o := v.Origin
		w.Origin = &o
	}
	if v.Known {
		raw, err := json.Marshal(v.Raw)
		if err != nil {
			return nil, err
		}
		w.Raw = raw
	}
	return json.Marshal(w)
}

func (v *Value) UnmarshalJSON(data []byte) error {
	var w wireValue
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	kind, err := kindFromString(w.Kind)
	if err != nil {
		return err
	}

	out := Value{Kind: kind, Known: w.Known, Source: w.Source, Sensitive: w.Sensitive}
	if w.Origin != nil {
		out.Origin = *w.Origin
	}

	if w.Known {
		switch kind {
		case KindString:
			var s string
			if err := json.Unmarshal(w.Raw, &s); err != nil {
				return err
			}
			out.Raw = s
		case KindInt:
			var i int64
			if err := json.Unmarshal(w.Raw, &i); err != nil {
				return err
			}
			out.Raw = i
		case KindFloat:
			var f float64
			if err := json.Unmarshal(w.Raw, &f); err != nil {
				return err
			}
			out.Raw = f
		case KindBool:
			var b bool
			if err := json.Unmarshal(w.Raw, &b); err != nil {
				return err
			}
			out.Raw = b
		case KindList:
			var items []Value
			if err := json.Unmarshal(w.Raw, &items); err != nil {
				return err
			}
			out.Raw = items
		case KindMap:
			var items map[string]Value
			if err := json.Unmarshal(w.Raw, &items); err != nil {
				return err
			}
			out.Raw = items
		default:
			return fmt.Errorf("cannot decode value of kind %s", kind)
		}
	}

	*v = out
	return nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./pkg/value/ -v`
Expected: PASS — ten tests across the package.

- [ ] **Step 5: Commit**

```bash
git add pkg/value/json.go pkg/value/json_test.go
git commit -m "feat: make Value round-trip losslessly through JSON"
```

---

## Task 4: Resource addressing

**Files:**
- Create: `pkg/address/address.go`
- Test: `pkg/address/address_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `address.Address{Module []string; Name string}`, `(Address).String`, `(Address).InModule(string) Address`, `address.Parse(string) (Address, error)`, `address.Sort([]Address)`.

- [ ] **Step 1: Write the failing test**

Create `pkg/address/address_test.go`:

```go
package address

import "testing"

func TestStringForRootAndModule(t *testing.T) {
	if got := (Address{Name: "main"}).String(); got != "main" {
		t.Errorf("root address = %q, want \"main\"", got)
	}
	a := Address{Module: []string{"network", "inner"}, Name: "vpc"}
	if got := a.String(); got != "module.network.module.inner.vpc" {
		t.Errorf("nested address = %q", got)
	}
}

func TestParseRoundTrips(t *testing.T) {
	for _, in := range []string{"main", "module.network.vpc", "module.a.module.b.c"} {
		got, err := Parse(in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", in, err)
		}
		if got.String() != in {
			t.Errorf("round trip of %q gave %q", in, got.String())
		}
	}
}

func TestParseRejectsMalformed(t *testing.T) {
	for _, in := range []string{"", "module", "module.", "module.a", "module..x", "a.b"} {
		if _, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) succeeded; want an error", in)
		}
	}
}

func TestInModuleNestsWithoutAliasing(t *testing.T) {
	base := Address{Module: []string{"a"}, Name: "x"}
	child := base.InModule("b")
	if child.String() != "module.b.module.a.x" {
		t.Errorf("InModule gave %q", child.String())
	}
	if len(base.Module) != 1 {
		t.Error("InModule must not mutate the receiver's slice")
	}
}

func TestSortIsDeterministic(t *testing.T) {
	in := []Address{
		{Name: "zebra"},
		{Module: []string{"net"}, Name: "a"},
		{Name: "alpha"},
	}
	Sort(in)
	want := []string{"alpha", "module.net.a", "zebra"}
	for i, w := range want {
		if in[i].String() != w {
			t.Fatalf("position %d = %q, want %q", i, in[i].String(), w)
		}
	}
}
```

Note on `"a.b"` being rejected: a bare dotted name is ambiguous with the `type.name` display
form, so only the canonical `module.` prefix syntax parses. Spec §5.2.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./pkg/address/ -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Implement Address**

Create `pkg/address/address.go`:

```go
// Package address defines canonical resource addresses.
//
// The canonical form is the module path plus the logical name. Type is never
// part of an address: "aws.rds.database" cannot be split unambiguously because
// types themselves contain dots. Spec §5.2.
package address

import (
	"fmt"
	"sort"
	"strings"
)

type Address struct {
	Module []string // empty at the root
	Name   string
}

func (a Address) String() string {
	if len(a.Module) == 0 {
		return a.Name
	}
	parts := make([]string, 0, len(a.Module)*2+1)
	for _, m := range a.Module {
		parts = append(parts, "module", m)
	}
	parts = append(parts, a.Name)
	return strings.Join(parts, ".")
}

// InModule returns the address as seen from inside a parent module
// instantiation. It copies the module slice so callers cannot alias.
func (a Address) InModule(name string) Address {
	next := make([]string, 0, len(a.Module)+1)
	next = append(next, name)
	next = append(next, a.Module...)
	return Address{Module: next, Name: a.Name}
}

func Parse(s string) (Address, error) {
	if s == "" {
		return Address{}, fmt.Errorf("empty address")
	}
	parts := strings.Split(s, ".")
	var out Address
	for len(parts) > 0 {
		if parts[0] != "module" {
			break
		}
		if len(parts) < 2 || parts[1] == "" {
			return Address{}, fmt.Errorf("address %q: %q must be followed by a module name", s, "module")
		}
		out.Module = append(out.Module, parts[1])
		parts = parts[2:]
	}
	if len(parts) != 1 || parts[0] == "" {
		return Address{}, fmt.Errorf("address %q: expected a single logical name after any module path", s)
	}
	out.Name = parts[0]
	return out, nil
}

// Sort orders addresses by their canonical string. Plan operations are sorted
// this way so serialized plans are byte-identical across runs. Spec §12.1.
func Sort(addrs []Address) {
	sort.Slice(addrs, func(i, j int) bool {
		return addrs[i].String() < addrs[j].String()
	})
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./pkg/address/ -v`
Expected: PASS — five tests.

- [ ] **Step 5: Commit**

```bash
git add pkg/address
git commit -m "feat: add canonical resource addressing"
```

---

## Task 5: Diagnostics

**Files:**
- Create: `internal/diag/diagnostic.go`
- Test: `internal/diag/diagnostic_test.go`

**Interfaces:**
- Consumes: `value.Origin`, `address.Address`.
- Produces: `diag.Severity` (`SeverityError`, `SeverityWarning`), `diag.Diagnostic`, `diag.Diagnostics`, `(Diagnostics).Add`, `(Diagnostics).Extend`, `(Diagnostics).HasErrors`, `(Diagnostics).Render(io.Writer)`.

- [ ] **Step 1: Write the failing test**

Create `internal/diag/diagnostic_test.go`:

```go
package diag

import (
	"bytes"
	"strings"
	"testing"

	"infra/pkg/value"
)

func TestHasErrorsIgnoresWarnings(t *testing.T) {
	var ds Diagnostics
	ds.Add(Diagnostic{Severity: SeverityWarning, Summary: "unused variable"})
	if ds.HasErrors() {
		t.Error("warnings must not make HasErrors true")
	}
	ds.Add(Diagnostic{Severity: SeverityError, Summary: "unknown type"})
	if !ds.HasErrors() {
		t.Error("an error must make HasErrors true")
	}
}

func TestCollectsRatherThanStopping(t *testing.T) {
	var ds Diagnostics
	ds.Add(Diagnostic{Severity: SeverityError, Summary: "first"})
	ds.Add(Diagnostic{Severity: SeverityError, Summary: "second"})
	if len(ds) != 2 {
		t.Fatalf("len = %d, want 2 — validate reports every problem in one pass (spec §7.4)", len(ds))
	}
}

func TestRenderIncludesLocationExpectationAndAction(t *testing.T) {
	ds := Diagnostics{{
		Severity: SeverityError,
		Summary:  "application requires a network",
		Detail:   "No network resource was found in environment \"production\".\nExpected one of:\n  test.network",
		Action:   "Add a networking module or resource.",
		Origin:   value.Origin{File: "infra.yml", Line: 12, Column: 3},
	}}

	var out bytes.Buffer
	ds.Render(&out)
	got := out.String()

	for _, want := range []string{
		"application requires a network",
		"infra.yml:12:3",
		"Expected one of:",
		"Suggested action:",
		"Add a networking module or resource.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered output is missing %q\n---\n%s", want, got)
		}
	}
}

func TestRenderNamesTheModuleChain(t *testing.T) {
	ds := Diagnostics{{
		Severity: SeverityError,
		Summary:  "unknown attribute",
		Origin:   value.Origin{File: "modules/db/module.yml", Line: 4, Module: []string{"platform", "database"}},
	}}
	var out bytes.Buffer
	ds.Render(&out)
	if !strings.Contains(out.String(), "module.platform.module.database") {
		t.Errorf("module chain missing — flattening makes this the only way to locate the instantiation (spec §7.2)\n%s", out.String())
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/diag/ -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Implement diagnostics**

Create `internal/diag/diagnostic.go`:

```go
// Package diag carries the errors and warnings produced by compilation and
// planning. Stages collect diagnostics rather than failing fast, so a single
// typo does not mask the rest of a file. Spec §7.4.
package diag

import (
	"fmt"
	"io"
	"strings"

	"infra/pkg/address"
	"infra/pkg/value"
)

type Severity uint8

const (
	SeverityError Severity = iota
	SeverityWarning
)

func (s Severity) String() string {
	if s == SeverityWarning {
		return "Warning"
	}
	return "Error"
}

// Diagnostic follows the shape PLAN.md §44 requires: what is wrong, where, what
// was expected, and what to do about it.
type Diagnostic struct {
	Severity Severity
	Summary  string
	Detail   string
	Action   string
	Origin   value.Origin
	Related  []address.Address
}

type Diagnostics []Diagnostic

func (ds *Diagnostics) Add(d Diagnostic) { *ds = append(*ds, d) }

func (ds *Diagnostics) Extend(other Diagnostics) { *ds = append(*ds, other...) }

func (ds Diagnostics) HasErrors() bool {
	for _, d := range ds {
		if d.Severity == SeverityError {
			return true
		}
	}
	return false
}

func (ds Diagnostics) Render(w io.Writer) {
	for i, d := range ds {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "%s: %s\n", d.Severity, d.Summary)

		if loc := location(d.Origin); loc != "" {
			fmt.Fprintf(w, "  at %s\n", loc)
		}
		if d.Detail != "" {
			fmt.Fprintln(w)
			for _, line := range strings.Split(d.Detail, "\n") {
				fmt.Fprintf(w, "  %s\n", line)
			}
		}
		if len(d.Related) > 0 {
			fmt.Fprintln(w)
			fmt.Fprintln(w, "  Related resources:")
			for _, a := range d.Related {
				fmt.Fprintf(w, "    %s\n", a.String())
			}
		}
		if d.Action != "" {
			fmt.Fprintln(w)
			fmt.Fprintln(w, "  Suggested action:")
			fmt.Fprintf(w, "    %s\n", d.Action)
		}
	}
}

func location(o value.Origin) string {
	pos := o.String()
	if len(o.Module) == 0 {
		if pos == "<generated>" {
			return ""
		}
		return pos
	}
	parts := make([]string, 0, len(o.Module)*2)
	for _, m := range o.Module {
		parts = append(parts, "module", m)
	}
	return fmt.Sprintf("%s, in %s", pos, strings.Join(parts, "."))
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/diag/ -v`
Expected: PASS — four tests.

- [ ] **Step 5: Commit**

```bash
git add internal/diag
git commit -m "feat: add collecting diagnostics with PLAN.md §44 rendering"
```

---

## Task 6: Resource schemas

**Files:**
- Create: `pkg/schema/attribute.go`, `pkg/schema/definition.go`
- Test: `pkg/schema/definition_test.go`

**Interfaces:**
- Consumes: `value.Value`, `value.Kind`.
- Produces: `schema.DefaultContext`, `schema.DefaultFunc`, `schema.Attribute`, `schema.Requirement`, `schema.Capabilities`, `schema.ImportSpec`, `schema.ResourceDefinition`, `(ResourceDefinition).Attribute(name) (Attribute, bool)`, `(ResourceDefinition).RequiredAttributes() []string`, `(ResourceDefinition).ForceNewAttributes() []string`, `(ResourceDefinition).Validate() error`.

- [ ] **Step 1: Write the failing test**

Create `pkg/schema/definition_test.go`:

```go
package schema

import (
	"testing"

	"infra/pkg/value"
)

func sampleDefinition() *ResourceDefinition {
	return &ResourceDefinition{
		Type:        "test.database",
		Description: "A fake database",
		Attributes: map[string]Attribute{
			"engine":   {Kind: value.KindString, Required: true, ForceNew: true},
			"size":     {Kind: value.KindInt, Default: func(DefaultContext) (any, bool) { return int64(10), true }},
			"password": {Kind: value.KindString, Sensitive: true},
			"endpoint": {Kind: value.KindString, Computed: true},
		},
		Requirements: []Requirement{{Name: "network", Types: []string{"test.network"}}},
		Capabilities: Capabilities{Create: true, Read: true, Update: true, Delete: true},
	}
}

func TestAttributeLookup(t *testing.T) {
	d := sampleDefinition()
	if _, ok := d.Attribute("engine"); !ok {
		t.Error("engine should be found")
	}
	if _, ok := d.Attribute("nope"); ok {
		t.Error("unknown attribute should not be found")
	}
}

func TestRequiredAndForceNewAreSortedForDeterminism(t *testing.T) {
	d := sampleDefinition()
	d.Attributes["alpha"] = Attribute{Kind: value.KindString, Required: true, ForceNew: true}

	req := d.RequiredAttributes()
	if len(req) != 2 || req[0] != "alpha" || req[1] != "engine" {
		t.Errorf("RequiredAttributes() = %v, want sorted [alpha engine]", req)
	}
	fn := d.ForceNewAttributes()
	if len(fn) != 2 || fn[0] != "alpha" {
		t.Errorf("ForceNewAttributes() = %v, want sorted", fn)
	}
}

func TestDefaultResolverSeesOnlyItsContext(t *testing.T) {
	d := sampleDefinition()
	attr, _ := d.Attribute("size")
	got, ok := attr.Default(DefaultContext{Environment: "dev", Type: "test.database"})
	if !ok || got != int64(10) {
		t.Errorf("default = %v, %v; want 10, true", got, ok)
	}
}

func TestValidateRejectsComputedRequired(t *testing.T) {
	d := sampleDefinition()
	d.Attributes["broken"] = Attribute{Kind: value.KindString, Required: true, Computed: true}
	if err := d.Validate(); err == nil {
		t.Error("an attribute cannot be both Required and Computed — the user may not set it")
	}
}

func TestValidateRejectsComputedWithDefault(t *testing.T) {
	d := sampleDefinition()
	d.Attributes["broken"] = Attribute{
		Kind:     value.KindString,
		Computed: true,
		Default:  func(DefaultContext) (any, bool) { return "x", true },
	}
	if err := d.Validate(); err == nil {
		t.Error("a computed attribute cannot carry a default — the provider supplies it")
	}
}

func TestValidateRejectsMissingKindOrType(t *testing.T) {
	d := sampleDefinition()
	d.Attributes["broken"] = Attribute{}
	if err := d.Validate(); err == nil {
		t.Error("an attribute with no Kind must be rejected")
	}

	empty := &ResourceDefinition{Attributes: map[string]Attribute{}}
	if err := empty.Validate(); err == nil {
		t.Error("a definition with no Type must be rejected")
	}
}

func TestValidateRejectsRequirementWithNoTypes(t *testing.T) {
	d := sampleDefinition()
	d.Requirements = append(d.Requirements, Requirement{Name: "cluster"})
	if err := d.Validate(); err == nil {
		t.Error("a requirement that names no satisfying types can never be satisfied")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./pkg/schema/ -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Implement attributes**

Create `pkg/schema/attribute.go`:

```go
// Package schema describes provider resource types as inspectable data.
//
// Schemas are plain values rather than struct tags so that `infra explain`,
// validation and default resolution all read one source, and so that
// environment-aware default resolvers can be functions. Spec §8.1.
package schema

import "infra/pkg/value"

// DefaultContext is everything a default resolver is allowed to see.
//
// It deliberately excludes other resources' attributes: defaults must never
// depend on unknown values, so that they are always computable at plan time.
// Spec §7.3.
type DefaultContext struct {
	Environment     string
	EnvironmentType string // e.g. "production"
	Region          string
	Account         string
	Project         string
	Type            string
}

// DefaultFunc returns a default datum and whether one applies. Returning false
// means the attribute stays absent.
type DefaultFunc func(DefaultContext) (any, bool)

type Attribute struct {
	Kind        value.Kind
	Required    bool
	Computed    bool // the provider sets it; configuration may not
	Sensitive   bool
	ForceNew    bool // a change replaces the resource rather than updating it
	Default     DefaultFunc
	Description string
	Validate    func(value.Value) error
}
```

- [ ] **Step 4: Implement definitions**

Create `pkg/schema/definition.go`:

```go
package schema

import (
	"fmt"
	"sort"

	"infra/pkg/value"
)

// Requirement declares infrastructure a resource needs in order to exist. The
// planner reports unsatisfied requirements before apply rather than letting a
// provider API call fail. PLAN.md §17.
type Requirement struct {
	Name        string   // "cluster", "network", "execution_role"
	Types       []string // resource types that satisfy it
	Optional    bool
	Description string
}

type Capabilities struct {
	Create, Read, Update, Delete, Import bool
}

// ImportSpec describes the provider ID form for a resource type. M1 uses
// Description only, to render `infra explain`. Parse is declared now so the
// field does not change shape in Phase 2, and may be nil until then.
type ImportSpec struct {
	Description string
	Parse       func(id string) (map[string]value.Value, error)
}

type ResourceDefinition struct {
	Type         string
	Description  string
	Attributes   map[string]Attribute
	Requirements []Requirement
	Capabilities Capabilities
	ImportID     ImportSpec
}

func (d *ResourceDefinition) Attribute(name string) (Attribute, bool) {
	a, ok := d.Attributes[name]
	return a, ok
}

// RequiredAttributes returns required attribute names in sorted order. Sorting
// matters: diagnostics built from this list must be identical across runs, and
// Go map iteration order is randomised.
func (d *ResourceDefinition) RequiredAttributes() []string {
	return d.filterNames(func(a Attribute) bool { return a.Required })
}

func (d *ResourceDefinition) ForceNewAttributes() []string {
	return d.filterNames(func(a Attribute) bool { return a.ForceNew })
}

func (d *ResourceDefinition) filterNames(keep func(Attribute) bool) []string {
	var out []string
	for name, attr := range d.Attributes {
		if keep(attr) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Validate checks a definition for internal contradictions. Providers call it
// at registration so a malformed schema fails at startup rather than during a
// plan.
func (d *ResourceDefinition) Validate() error {
	if d.Type == "" {
		return fmt.Errorf("resource definition has no Type")
	}
	names := make([]string, 0, len(d.Attributes))
	for name := range d.Attributes {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		attr := d.Attributes[name]
		switch {
		case attr.Kind == value.KindInvalid:
			return fmt.Errorf("%s: attribute %q has no Kind", d.Type, name)
		case attr.Required && attr.Computed:
			return fmt.Errorf("%s: attribute %q is both Required and Computed; configuration may not set a computed attribute", d.Type, name)
		case attr.Computed && attr.Default != nil:
			return fmt.Errorf("%s: attribute %q is Computed and also has a Default; the provider supplies computed values", d.Type, name)
		case attr.Required && attr.Default != nil:
			return fmt.Errorf("%s: attribute %q is Required and also has a Default; a default makes it optional", d.Type, name)
		}
	}

	for _, req := range d.Requirements {
		if req.Name == "" {
			return fmt.Errorf("%s: a requirement has no Name", d.Type)
		}
		if len(req.Types) == 0 {
			return fmt.Errorf("%s: requirement %q names no satisfying types, so it can never be satisfied", d.Type, req.Name)
		}
	}
	return nil
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./pkg/schema/ -v`
Expected: PASS — seven tests.

- [ ] **Step 6: Commit**

```bash
git add pkg/schema
git commit -m "feat: add declarative resource schemas"
```

---

## Task 7: Resource and provider interfaces

**Files:**
- Create: `pkg/resource/resource.go`, `pkg/provider/provider.go`
- Test: `pkg/resource/resource_test.go`

**Interfaces:**
- Consumes: `address.Address`, `value.Value`, `schema.ResourceDefinition`.
- Produces: `resource.Lifecycle`, `resource.ResolvedResource`, `resource.DesiredResource`, `resource.ResourceState`, `(ResolvedResource).Desired() (DesiredResource, error)`, `(ResourceState).Clone()`; `provider.Provider`, `provider.Retryability` (`SafeToRetry`, `ConditionallyRetryable`, `NotSafeToRetry`), `provider.DiscoverRequest`, `provider.DiscoveredResource`, `provider.ErrNotImplemented`.

- [ ] **Step 1: Write the failing test**

Create `pkg/resource/resource_test.go`:

```go
package resource

import (
	"testing"

	"infra/pkg/address"
	"infra/pkg/value"
)

func TestDesiredRejectsUnknownAttributes(t *testing.T) {
	r := ResolvedResource{
		Address: address.Address{Name: "db"},
		Type:    "test.database",
		Attrs: map[string]value.Value{
			"engine": value.String("postgres", value.SourceExplicit),
			"url":    value.Unknown(value.KindString, value.SourceComputed),
		},
	}
	if _, err := r.Desired(); err == nil {
		t.Fatal("Desired() must refuse unknown attributes: providers are never called with unresolved values (spec §5.3)")
	}
}

func TestDesiredSucceedsWhenFullyKnown(t *testing.T) {
	r := ResolvedResource{
		Address:   address.Address{Name: "db"},
		Type:      "test.database",
		Attrs:     map[string]value.Value{"engine": value.String("postgres", value.SourceExplicit)},
		Lifecycle: Lifecycle{PreventDestroy: true},
	}
	d, err := r.Desired()
	if err != nil {
		t.Fatalf("Desired(): %v", err)
	}
	if d.Type != "test.database" || !d.Lifecycle.PreventDestroy {
		t.Errorf("Desired() lost fields: %#v", d)
	}
	if got, _ := d.Attrs["engine"].AsString(); got != "postgres" {
		t.Errorf("engine = %q", got)
	}
}

func TestCloneIsDeep(t *testing.T) {
	s := &ResourceState{
		Address:      address.Address{Name: "db"},
		Type:         "test.database",
		Attributes:   map[string]value.Value{"engine": value.String("postgres", value.SourceProvider)},
		Dependencies: []address.Address{{Name: "net"}},
	}
	c := s.Clone()
	c.Attributes["engine"] = value.String("mysql", value.SourceProvider)
	c.Dependencies[0] = address.Address{Name: "other"}

	if got, _ := s.Attributes["engine"].AsString(); got != "postgres" {
		t.Error("Clone shares its attribute map with the original")
	}
	if s.Dependencies[0].Name != "net" {
		t.Error("Clone shares its dependency slice with the original")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./pkg/resource/ -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Implement resource types**

Create `pkg/resource/resource.go`:

```go
// Package resource defines the resource representations shared by the engine
// and providers.
//
// The distinction between ResolvedResource and DesiredResource is load-bearing:
// the planner works with ResolvedResource, where unknown values are permitted;
// providers only ever receive DesiredResource, where they are not. Spec §5.3.
package resource

import (
	"fmt"
	"sort"
	"time"

	"infra/pkg/address"
	"infra/pkg/value"
)

type Lifecycle struct {
	PreventDestroy bool
	Retain         bool
}

type ResolvedResource struct {
	Address   address.Address
	Type      string
	Attrs     map[string]value.Value
	DependsOn []address.Address
	Lifecycle Lifecycle
	Origin    value.Origin
}

// DesiredResource is what the executor hands a provider. Every attribute is
// known.
type DesiredResource struct {
	Address   address.Address
	Type      string
	Attrs     map[string]value.Value
	Lifecycle Lifecycle
}

// Desired converts a resolved resource for provider consumption, refusing any
// attribute that is still unknown. Callers reach this point only after every
// dependency has been created and its deferred expressions evaluated.
func (r ResolvedResource) Desired() (DesiredResource, error) {
	var unresolved []string
	attrs := make(map[string]value.Value, len(r.Attrs))
	for name, v := range r.Attrs {
		if !v.Known {
			unresolved = append(unresolved, name)
			continue
		}
		attrs[name] = v
	}
	if len(unresolved) > 0 {
		sort.Strings(unresolved)
		return DesiredResource{}, fmt.Errorf("%s: attributes still unknown: %v", r.Address, unresolved)
	}
	return DesiredResource{
		Address:   r.Address,
		Type:      r.Type,
		Attrs:     attrs,
		Lifecycle: r.Lifecycle,
	}, nil
}

// ResourceState is the recorded association between a logical resource and the
// external object it manages. PLAN.md §3.4.
type ResourceState struct {
	Address      address.Address
	Type         string
	Provider     string
	ProviderID   string
	Attributes   map[string]value.Value
	Dependencies []address.Address
	Lifecycle    Lifecycle
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Clone deep-copies a resource state. Refresh and planning must never mutate
// the state that was loaded from disk.
func (s *ResourceState) Clone() *ResourceState {
	if s == nil {
		return nil
	}
	out := *s
	out.Attributes = make(map[string]value.Value, len(s.Attributes))
	for k, v := range s.Attributes {
		out.Attributes[k] = v
	}
	out.Dependencies = append([]address.Address(nil), s.Dependencies...)
	return &out
}
```

- [ ] **Step 4: Implement the provider interface**

Create `pkg/provider/provider.go`:

```go
// Package provider defines the boundary between the infra core and the systems
// it manages. No provider-specific type may appear above this interface.
// PLAN.md §31.
package provider

import (
	"context"
	"errors"

	"infra/pkg/resource"
	"infra/pkg/schema"
	"infra/pkg/value"
)

// ErrNotImplemented is returned by capabilities a provider does not offer.
var ErrNotImplemented = errors.New("not implemented")

// Retryability classifies a provider error. The provider classifies; the core
// owns backoff. PLAN.md §35.
type Retryability uint8

const (
	NotSafeToRetry Retryability = iota
	ConditionallyRetryable
	SafeToRetry
)

// DiscoverRequest and DiscoveredResource are declared in M1 so the interface
// does not churn in Phase 2, where discovery is implemented.
type DiscoverRequest struct {
	Types  []string
	Region string
}

type DiscoveredResource struct {
	Type       string
	ProviderID string
	Attributes map[string]value.Value
}

type Provider interface {
	Name() string
	Definitions() []*schema.ResourceDefinition

	// Read returns the current state of a managed resource. A nil state with a
	// nil error means the resource no longer exists.
	Read(ctx context.Context, current *resource.ResourceState) (*resource.ResourceState, error)
	Create(ctx context.Context, desired *resource.DesiredResource) (*resource.ResourceState, error)
	Update(ctx context.Context, current *resource.ResourceState, desired *resource.DesiredResource) (*resource.ResourceState, error)
	Delete(ctx context.Context, current *resource.ResourceState) error

	Discover(ctx context.Context, req DiscoverRequest) ([]DiscoveredResource, error)
	Import(ctx context.Context, resourceType, id string) (*resource.ResourceState, error)

	ClassifyError(err error) Retryability
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./pkg/... -v`
Expected: PASS — three new tests in `pkg/resource`, everything else still green.

- [ ] **Step 6: Commit**

```bash
git add pkg/resource pkg/provider
git commit -m "feat: add resource types and provider interface"
```

---

## Task 8: Type registry

**Files:**
- Create: `internal/registry/registry.go`
- Test: `internal/registry/registry_test.go`

**Interfaces:**
- Consumes: `provider.Provider`, `schema.ResourceDefinition`.
- Produces: `registry.New() *Registry`, `(*Registry).Register(provider.Provider) error`, `(*Registry).Definition(string) (*schema.ResourceDefinition, bool)`, `(*Registry).Provider(string) (provider.Provider, bool)`, `(*Registry).Types() []string`.

The registry lives in `internal/` rather than `pkg/schema` because `Provider.Definitions()`
returns `*schema.ResourceDefinition`; a registry inside `schema` would create an import cycle.

- [ ] **Step 1: Write the failing test**

Create `internal/registry/registry_test.go`:

```go
package registry

import (
	"context"
	"strings"
	"testing"

	"infra/pkg/provider"
	"infra/pkg/resource"
	"infra/pkg/schema"
	"infra/pkg/value"
)

// stubProvider is the minimum a registry test needs. Behavioural provider tests
// live with the fake provider in Task 9.
type stubProvider struct {
	name string
	defs []*schema.ResourceDefinition
}

func (s stubProvider) Name() string                                { return s.name }
func (s stubProvider) Definitions() []*schema.ResourceDefinition   { return s.defs }
func (s stubProvider) ClassifyError(error) provider.Retryability   { return provider.NotSafeToRetry }
func (s stubProvider) Read(context.Context, *resource.ResourceState) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (s stubProvider) Create(context.Context, *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (s stubProvider) Update(context.Context, *resource.ResourceState, *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (s stubProvider) Delete(context.Context, *resource.ResourceState) error {
	return provider.ErrNotImplemented
}
func (s stubProvider) Discover(context.Context, provider.DiscoverRequest) ([]provider.DiscoveredResource, error) {
	return nil, provider.ErrNotImplemented
}
func (s stubProvider) Import(context.Context, string, string) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}

func def(t string) *schema.ResourceDefinition {
	return &schema.ResourceDefinition{
		Type:       t,
		Attributes: map[string]schema.Attribute{"name": {Kind: value.KindString, Required: true}},
	}
}

func TestRegisterAndLookup(t *testing.T) {
	r := New()
	if err := r.Register(stubProvider{name: "test", defs: []*schema.ResourceDefinition{def("test.database")}}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, ok := r.Definition("test.database"); !ok {
		t.Error("definition not found after registration")
	}
	p, ok := r.Provider("test.database")
	if !ok || p.Name() != "test" {
		t.Error("provider not found after registration")
	}
	if _, ok := r.Definition("test.missing"); ok {
		t.Error("unregistered type must not be found")
	}
}

func TestRegisterRejectsDuplicateType(t *testing.T) {
	r := New()
	p := stubProvider{name: "test", defs: []*schema.ResourceDefinition{def("test.database")}}
	if err := r.Register(p); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	err := r.Register(stubProvider{name: "other", defs: []*schema.ResourceDefinition{def("test.database")}})
	if err == nil || !strings.Contains(err.Error(), "test.database") {
		t.Errorf("duplicate registration error = %v; want one naming the type", err)
	}
}

func TestRegisterRejectsDuplicateTypeWithinOneProvider(t *testing.T) {
	r := New()
	err := r.Register(stubProvider{name: "test", defs: []*schema.ResourceDefinition{
		def("test.database"), def("test.database"),
	}})
	if err == nil {
		t.Fatal("a provider declaring the same type twice must be rejected, not silently clobbered")
	}
	if len(r.Types()) != 0 {
		t.Error("a failed registration must leave the registry untouched")
	}
}

func TestRegisterValidatesDefinitions(t *testing.T) {
	r := New()
	broken := &schema.ResourceDefinition{
		Type:       "test.broken",
		Attributes: map[string]schema.Attribute{"x": {Kind: value.KindString, Required: true, Computed: true}},
	}
	if err := r.Register(stubProvider{name: "test", defs: []*schema.ResourceDefinition{broken}}); err == nil {
		t.Error("a malformed schema must fail at registration, not during a plan")
	}
}

func TestTypesIsSorted(t *testing.T) {
	r := New()
	_ = r.Register(stubProvider{name: "test", defs: []*schema.ResourceDefinition{
		def("test.network"), def("test.application"), def("test.database"),
	}})
	got := r.Types()
	want := []string{"test.application", "test.database", "test.network"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Types() = %v, want %v — explain output must be stable", got, want)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/registry/ -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Implement the registry**

Create `internal/registry/registry.go`:

```go
// Package registry maps resource types to their definitions and providers.
// `infra explain` renders directly from it, so documentation cannot drift from
// the schemas it describes. Spec §8.2.
package registry

import (
	"fmt"
	"sort"

	"infra/pkg/provider"
	"infra/pkg/schema"
)

type Registry struct {
	definitions map[string]*schema.ResourceDefinition
	providers   map[string]provider.Provider
}

func New() *Registry {
	return &Registry{
		definitions: map[string]*schema.ResourceDefinition{},
		providers:   map[string]provider.Provider{},
	}
}

// Register adds every definition a provider offers. A malformed schema or a
// type already claimed by another provider is an error here, at startup.
func (r *Registry) Register(p provider.Provider) error {
	defs := p.Definitions()

	// Validate everything before mutating, so a failed registration leaves the
	// registry untouched. `seen` catches a provider declaring the same type
	// twice in one call, which the registry-state check alone cannot see
	// because the first loop never mutates.
	seen := make(map[string]bool, len(defs))
	for _, d := range defs {
		if err := d.Validate(); err != nil {
			return fmt.Errorf("provider %s: %w", p.Name(), err)
		}
		if existing, ok := r.providers[d.Type]; ok {
			return fmt.Errorf("provider %s: resource type %q is already registered by provider %s", p.Name(), d.Type, existing.Name())
		}
		if seen[d.Type] {
			return fmt.Errorf("provider %s declares resource type %q more than once", p.Name(), d.Type)
		}
		seen[d.Type] = true
	}

	for _, d := range defs {
		r.definitions[d.Type] = d
		r.providers[d.Type] = p
	}
	return nil
}

func (r *Registry) Definition(resourceType string) (*schema.ResourceDefinition, bool) {
	d, ok := r.definitions[resourceType]
	return d, ok
}

func (r *Registry) Provider(resourceType string) (provider.Provider, bool) {
	p, ok := r.providers[resourceType]
	return p, ok
}

// Types returns every registered type in sorted order.
func (r *Registry) Types() []string {
	out := make([]string, 0, len(r.definitions))
	for t := range r.definitions {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/registry/ -v`
Expected: PASS — four tests.

- [ ] **Step 5: Commit**

```bash
git add internal/registry
git commit -m "feat: add resource type registry"
```

---

## Task 9: Fake cloud file

The fake provider's world lives in a hand-editable JSON file so a human can mutate fake
infrastructure to demonstrate drift, exactly as `PLAN.md` §48's MVP script requires. This
task builds the file format and its controls; Task 10 builds the provider on top.

This is the one deliberate exception to the no-`map[string]any` rule: the cloud file models
an external system, not internal configuration, so it stores plain JSON.

**Files:**
- Create: `providers/test/cloud.go`
- Test: `providers/test/cloud_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `test.Cloud`, `test.CloudResource`, `test.FailureRule`, `test.LoadCloud(path string) (*Cloud, error)`, `(*Cloud).Save(path string) error`, `(*Cloud).ShouldFail(op, address string) (*FailureRule, bool)`, `(*Cloud).Delay() time.Duration`.

- [ ] **Step 1: Write the failing test**

Create `providers/test/cloud_test.go`:

```go
package test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadCloudMissingFileIsEmptyNotError(t *testing.T) {
	c, err := LoadCloud(filepath.Join(t.TempDir(), "fake-cloud.json"))
	if err != nil {
		t.Fatalf("LoadCloud on a missing file: %v", err)
	}
	if len(c.Resources) != 0 {
		t.Errorf("expected an empty cloud, got %d resources", len(c.Resources))
	}
}

func TestSaveThenLoadRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fake-cloud.json")
	c := &Cloud{Resources: map[string]*CloudResource{
		"db-1": {Type: "test.database", Attributes: map[string]any{"engine": "postgres", "size": float64(10)}},
	}}
	if err := c.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := LoadCloud(path)
	if err != nil {
		t.Fatalf("LoadCloud: %v", err)
	}
	if got.Resources["db-1"].Attributes["engine"] != "postgres" {
		t.Errorf("round trip lost data: %#v", got.Resources["db-1"])
	}
}

func TestSaveIsHumanEditable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fake-cloud.json")
	c := &Cloud{Resources: map[string]*CloudResource{"db-1": {Type: "test.database"}}}
	if err := c.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Contains(data, []byte("\n  ")) {
		t.Error("the cloud file must be indented — a human edits it to induce drift (PLAN.md §48)")
	}
}

func TestShouldFailMatchesNthOccurrence(t *testing.T) {
	c := &Cloud{Failures: []FailureRule{{Op: "create", Address: "db", Nth: 2, Message: "boom"}}}

	if _, ok := c.ShouldFail("create", "db"); ok {
		t.Error("first attempt must not fail when Nth is 2")
	}
	rule, ok := c.ShouldFail("create", "db")
	if !ok || rule.Message != "boom" {
		t.Fatalf("second attempt should fail, got ok=%v rule=%#v", ok, rule)
	}
	if _, ok := c.ShouldFail("create", "db"); ok {
		t.Error("a rule fires once, then stops")
	}
}

func TestShouldFailIgnoresOtherOpsAndAddresses(t *testing.T) {
	c := &Cloud{Failures: []FailureRule{{Op: "delete", Address: "db", Nth: 1}}}
	if _, ok := c.ShouldFail("create", "db"); ok {
		t.Error("rule must not match a different operation")
	}
	if _, ok := c.ShouldFail("delete", "other"); ok {
		t.Error("rule must not match a different address")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./providers/test/ -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Implement the cloud file**

Create `providers/test/cloud.go`:

```go
// Package test implements a fake provider whose world lives in a
// hand-editable JSON file, so drift can be induced by a person or a test with
// equal ease. PLAN.md §33 and §48; spec §8.4.
package test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// DefaultCloudPath is where the fake cloud lives inside a project.
const DefaultCloudPath = ".infra/fake-cloud.json"

// CloudResource is one object in the fake cloud. Attributes are plain JSON
// because this models an external system, not internal configuration.
type CloudResource struct {
	Type       string         `json:"type"`
	Address    string         `json:"address,omitempty"`
	Attributes map[string]any `json:"attributes"`
}

// FailureRule injects a failure. Nth counts from 1; the rule fires once.
type FailureRule struct {
	Op        string `json:"op"` // create, read, update, delete
	Address   string `json:"address"`
	Nth       int    `json:"nth"`
	Retryable bool   `json:"retryable,omitempty"`
	Message   string `json:"message,omitempty"`

	// Seen and Fired are bookkeeping, persisted deliberately. The provider
	// reloads this file on every operation — it must, or a human's hand edit
	// would never be observed — so unexported counters would reset each time:
	// an Nth:1 rule would fire on every attempt instead of once, and an Nth:2
	// rule could never reach its count at all. A human writing a rule by hand
	// simply omits both fields.
	Seen  int  `json:"seen,omitempty"`
	Fired bool `json:"fired,omitempty"`
}

type Cloud struct {
	Resources map[string]*CloudResource `json:"resources"`
	Failures  []FailureRule             `json:"failures,omitempty"`
	LatencyMS int                       `json:"latency_ms,omitempty"`
	NextID    int                       `json:"next_id,omitempty"`
}

// LoadCloud reads the cloud file. A missing file is an empty cloud, not an
// error: a project that has never applied anything has no infrastructure.
func LoadCloud(path string) (*Cloud, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &Cloud{Resources: map[string]*CloudResource{}}, nil
	}
	if err != nil {
		return nil, err
	}

	var c Cloud
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.Resources == nil {
		c.Resources = map[string]*CloudResource{}
	}
	return &c, nil
}

// Save writes the cloud file indented, because a human edits it.
func (c *Cloud) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

// ShouldFail reports whether an injected failure applies to this operation.
func (c *Cloud) ShouldFail(op, addr string) (*FailureRule, bool) {
	for i := range c.Failures {
		rule := &c.Failures[i]
		if rule.Fired || rule.Op != op || rule.Address != addr {
			continue
		}
		rule.Seen++
		nth := rule.Nth
		if nth <= 0 {
			nth = 1
		}
		if rule.Seen == nth {
			rule.Fired = true
			return rule, true
		}
	}
	return nil, false
}

func (c *Cloud) Delay() time.Duration {
	return time.Duration(c.LatencyMS) * time.Millisecond
}

// AllocateID returns a stable, increasing provider ID.
func (c *Cloud) AllocateID(prefix string) string {
	c.NextID++
	return fmt.Sprintf("%s-%d", prefix, c.NextID)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./providers/test/ -v`
Expected: PASS — five tests.

- [ ] **Step 5: Commit**

```bash
git add providers/test/cloud.go providers/test/cloud_test.go
git commit -m "feat: add hand-editable fake cloud file"
```

---

## Task 10: Fake provider

**Files:**
- Create: `providers/test/definitions.go`, `providers/test/provider.go`
- Test: `providers/test/provider_test.go`

**Interfaces:**
- Consumes: `provider.Provider`, `schema.*`, `resource.*`, `value.*`, `Cloud` from Task 9.
- Produces: `test.New(cloudPath string) *Provider` satisfying `provider.Provider`; definitions for `test.network`, `test.database`, `test.application`; `test.ErrInjected`.

Resource shapes are chosen to exercise the engine: `test.network` has no dependencies,
`test.database` has a `ForceNew` attribute plus a computed one plus a sensitive one and
requires a network, and `test.application` requires a database.

- [ ] **Step 1: Write the failing test**

Create `providers/test/provider_test.go`:

```go
package test

import (
	"context"
	"path/filepath"
	"testing"

	"infra/pkg/address"
	"infra/pkg/provider"
	"infra/pkg/resource"
	"infra/pkg/value"
)

func newTestProvider(t *testing.T) (*Provider, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-cloud.json")
	return New(path), path
}

func desired(name, resourceType string, attrs map[string]value.Value) *resource.DesiredResource {
	return &resource.DesiredResource{
		Address: address.Address{Name: name},
		Type:    resourceType,
		Attrs:   attrs,
	}
}

func TestCreateAssignsProviderIDAndComputedAttributes(t *testing.T) {
	p, _ := newTestProvider(t)
	st, err := p.Create(context.Background(), desired("db", "test.database", map[string]value.Value{
		"engine": value.String("postgres", value.SourceExplicit),
	}))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if st.ProviderID == "" {
		t.Error("Create must assign a provider ID")
	}
	if _, ok := st.Attributes["endpoint"]; !ok {
		t.Error("Create must populate computed attributes")
	}
	if st.Attributes["endpoint"].Source != value.SourceProvider {
		t.Error("provider-supplied values must carry SourceProvider")
	}
}

func TestReadReflectsExternalMutation(t *testing.T) {
	p, path := newTestProvider(t)
	st, err := p.Create(context.Background(), desired("db", "test.database", map[string]value.Value{
		"engine": value.String("postgres", value.SourceExplicit),
	}))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Mutate the cloud the way a human would, by editing the file.
	c, err := LoadCloud(path)
	if err != nil {
		t.Fatalf("LoadCloud: %v", err)
	}
	c.Resources[st.ProviderID].Attributes["engine"] = "mysql"
	if err := c.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := p.Read(context.Background(), st)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if s, _ := got.Attributes["engine"].AsString(); s != "mysql" {
		t.Errorf("engine = %q, want \"mysql\" — Read must observe external mutation, which is how drift is demonstrated", s)
	}
}

func TestReadReturnsNilWhenDeletedExternally(t *testing.T) {
	p, path := newTestProvider(t)
	st, _ := p.Create(context.Background(), desired("net", "test.network", map[string]value.Value{
		"cidr": value.String("10.0.0.0/16", value.SourceExplicit),
	}))

	c, _ := LoadCloud(path)
	delete(c.Resources, st.ProviderID)
	_ = c.Save(path)

	got, err := p.Read(context.Background(), st)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != nil {
		t.Error("Read must return a nil state, and no error, for a resource that no longer exists")
	}
}

func TestUpdateAndDelete(t *testing.T) {
	p, _ := newTestProvider(t)
	ctx := context.Background()
	st, _ := p.Create(ctx, desired("db", "test.database", map[string]value.Value{
		"engine": value.String("postgres", value.SourceExplicit),
		"size":   value.Int(10, value.SourceDefault),
	}))

	updated, err := p.Update(ctx, st, desired("db", "test.database", map[string]value.Value{
		"engine": value.String("postgres", value.SourceExplicit),
		"size":   value.Int(50, value.SourceExplicit),
	}))
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got, _ := updated.Attributes["size"].AsInt(); got != 50 {
		t.Errorf("size = %d, want 50", got)
	}
	if updated.ProviderID != st.ProviderID {
		t.Error("Update must not change the provider ID")
	}

	if err := p.Delete(ctx, updated); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	gone, err := p.Read(ctx, updated)
	if err != nil || gone != nil {
		t.Errorf("after Delete, Read = %v, %v; want nil, nil", gone, err)
	}
}

func TestInjectedFailureIsClassified(t *testing.T) {
	p, path := newTestProvider(t)
	c, _ := LoadCloud(path)
	c.Failures = []FailureRule{{Op: "create", Address: "db", Nth: 1, Retryable: true, Message: "throttled"}}
	_ = c.Save(path)

	_, err := p.Create(context.Background(), desired("db", "test.database", map[string]value.Value{
		"engine": value.String("postgres", value.SourceExplicit),
	}))
	if err == nil {
		t.Fatal("expected the injected failure")
	}
	if got := p.ClassifyError(err); got != provider.SafeToRetry {
		t.Errorf("ClassifyError = %v, want SafeToRetry", got)
	}
}

func TestSensitiveAttributeIsMarkedOnRead(t *testing.T) {
	p, _ := newTestProvider(t)
	st, _ := p.Create(context.Background(), desired("db", "test.database", map[string]value.Value{
		"engine":   value.String("postgres", value.SourceExplicit),
		"password": value.String("hunter2", value.SourceVariable),
	}))
	if !st.Attributes["password"].Sensitive {
		t.Error("an attribute the schema marks Sensitive must come back marked sensitive")
	}
}

func TestNthReadRuleSurvivesAcrossOperations(t *testing.T) {
	// Read is the only operation with no save on its success path, so this is
	// the only test that actually exercises begin()'s unconditional save. A
	// create-based version of this test passes either way, because Create
	// persists the advanced counter itself.
	p, path := newTestProvider(t)
	ctx := context.Background()
	st, err := p.Create(ctx, desired("net", "test.network", map[string]value.Value{
		"cidr": value.String("10.0.0.0/16", value.SourceExplicit),
	}))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	c, err := LoadCloud(path)
	if err != nil {
		t.Fatalf("LoadCloud: %v", err)
	}
	c.Failures = []FailureRule{{Op: "read", Address: "net", Nth: 2, Message: "second read fails"}}
	if err := c.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if _, err := p.Read(ctx, st); err != nil {
		t.Fatalf("first Read should succeed: %v", err)
	}
	if _, err := p.Read(ctx, st); err == nil {
		t.Fatal("second Read should fail: the rule's counter must survive the reload between operations")
	}
}

func TestNullAttributeIsTreatedAsUnset(t *testing.T) {
	p, path := newTestProvider(t)
	ctx := context.Background()
	st, err := p.Create(ctx, desired("db", "test.database", map[string]value.Value{
		"engine": value.String("postgres", value.SourceExplicit),
	}))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Someone hand-edits the cloud file and nulls an attribute out.
	c, err := LoadCloud(path)
	if err != nil {
		t.Fatalf("LoadCloud: %v", err)
	}
	c.Resources[st.ProviderID].Attributes["password"] = nil
	if err := c.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := p.Read(ctx, st)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if v, ok := got.Attributes["password"]; ok {
		t.Errorf("a null attribute must be absent, not present as %#v", v)
	}
}

func TestDiscoverAndImportAreNotImplementedYet(t *testing.T) {
	p, _ := newTestProvider(t)
	if _, err := p.Discover(context.Background(), provider.DiscoverRequest{}); err == nil {
		t.Error("Discover belongs to Phase 2 and must report that clearly")
	}
	if _, err := p.Import(context.Background(), "test.database", "db-1"); err == nil {
		t.Error("Import belongs to Phase 2 and must report that clearly")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./providers/test/ -run 'TestCreate|TestRead|TestUpdate|TestInjected|TestSensitive|TestDiscover' -v`
Expected: FAIL — `undefined: New`, `undefined: Provider`.

- [ ] **Step 3: Implement the definitions**

Create `providers/test/definitions.go`:

```go
package test

import (
	"infra/pkg/schema"
	"infra/pkg/value"
)

func definitions() []*schema.ResourceDefinition {
	return []*schema.ResourceDefinition{
		{
			Type:        "test.network",
			Description: "A fake network. Has no dependencies.",
			Attributes: map[string]schema.Attribute{
				"cidr": {Kind: value.KindString, Required: true, ForceNew: true, Description: "Address range"},
				"id":   {Kind: value.KindString, Computed: true, Description: "Assigned network identifier"},
			},
			Capabilities: schema.Capabilities{Create: true, Read: true, Update: true, Delete: true, Import: true},
			ImportID:     schema.ImportSpec{Description: "the network identifier, e.g. net-1"},
		},
		{
			Type:        "test.database",
			Description: "A fake database. Requires a network.",
			Attributes: map[string]schema.Attribute{
				"engine":   {Kind: value.KindString, Required: true, ForceNew: true, Description: "Database engine"},
				"size":     {Kind: value.KindInt, Description: "Storage in GB", Default: func(c schema.DefaultContext) (any, bool) {
					if c.EnvironmentType == "production" {
						return int64(100), true
					}
					return int64(10), true
				}},
				"password": {Kind: value.KindString, Sensitive: true, Description: "Administrator password"},
				"network":  {Kind: value.KindString, Description: "Network this database sits in"},
				"endpoint": {Kind: value.KindString, Computed: true, Description: "Connection endpoint"},
			},
			Requirements: []schema.Requirement{{
				Name:        "network",
				Types:       []string{"test.network"},
				Description: "A database must sit inside a network",
			}},
			Capabilities: schema.Capabilities{Create: true, Read: true, Update: true, Delete: true, Import: true},
			ImportID:     schema.ImportSpec{Description: "the database identifier, e.g. db-1"},
		},
		{
			Type:        "test.application",
			Description: "A fake application. Requires a database.",
			Attributes: map[string]schema.Attribute{
				"image":        {Kind: value.KindString, Required: true, Description: "Container image"},
				"replicas":     {Kind: value.KindInt, Description: "Instance count", Default: func(schema.DefaultContext) (any, bool) { return int64(1), true }},
				"database_url": {Kind: value.KindString, Description: "Connection string"},
				"url":          {Kind: value.KindString, Computed: true, Description: "Public URL"},
			},
			Requirements: []schema.Requirement{{
				Name:        "database",
				Types:       []string{"test.database"},
				Description: "An application must have a database",
			}},
			Capabilities: schema.Capabilities{Create: true, Read: true, Update: true, Delete: true, Import: true},
			ImportID:     schema.ImportSpec{Description: "the application identifier, e.g. app-1"},
		},
	}
}
```

- [ ] **Step 4: Implement the provider**

Create `providers/test/provider.go`:

```go
package test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"infra/pkg/address"
	"infra/pkg/provider"
	"infra/pkg/resource"
	"infra/pkg/schema"
	"infra/pkg/value"
)

// ErrInjected wraps a deliberately injected failure so ClassifyError can
// recognise it.
type ErrInjected struct {
	Message   string
	Retryable bool
}

func (e *ErrInjected) Error() string { return e.Message }

type Provider struct {
	cloudPath string
	defs      []*schema.ResourceDefinition
	byType    map[string]*schema.ResourceDefinition
}

func New(cloudPath string) *Provider {
	defs := definitions()
	byType := make(map[string]*schema.ResourceDefinition, len(defs))
	for _, d := range defs {
		byType[d.Type] = d
	}
	return &Provider{cloudPath: cloudPath, defs: defs, byType: byType}
}

// Provider satisfies the provider interface. Asserted at compile time so
// interface drift surfaces here rather than at integration.
var _ provider.Provider = (*Provider)(nil)

func (p *Provider) Name() string { return "test" }

func (p *Provider) Definitions() []*schema.ResourceDefinition { return p.defs }

func (p *Provider) ClassifyError(err error) provider.Retryability {
	var injected *ErrInjected
	if errors.As(err, &injected) {
		if injected.Retryable {
			return provider.SafeToRetry
		}
		return provider.NotSafeToRetry
	}
	return provider.NotSafeToRetry
}

// begin loads the cloud, applies latency, and checks for an injected failure.
func (p *Provider) begin(ctx context.Context, op, addr string) (*Cloud, error) {
	c, err := LoadCloud(p.cloudPath)
	if err != nil {
		return nil, err
	}
	if d := c.Delay(); d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
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
		return nil, &ErrInjected{Message: msg, Retryable: rule.Retryable}
	}
	return c, nil
}

func (p *Provider) Create(ctx context.Context, d *resource.DesiredResource) (*resource.ResourceState, error) {
	c, err := p.begin(ctx, "create", d.Address.String())
	if err != nil {
		return nil, err
	}

	id := c.AllocateID(idPrefix(d.Type))
	attrs := map[string]any{}
	for name, v := range d.Attrs {
		attrs[name] = v.Raw
	}
	for name, computed := range p.computedFor(d.Type, id) {
		attrs[name] = computed
	}

	c.Resources[id] = &CloudResource{Type: d.Type, Address: d.Address.String(), Attributes: attrs}
	if err := c.Save(p.cloudPath); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	st := p.toState(d.Address.String(), d.Type, id, attrs)
	st.CreatedAt, st.UpdatedAt = now, now
	st.Lifecycle = d.Lifecycle
	return st, nil
}

func (p *Provider) Read(ctx context.Context, current *resource.ResourceState) (*resource.ResourceState, error) {
	c, err := p.begin(ctx, "read", current.Address.String())
	if err != nil {
		return nil, err
	}
	obj, ok := c.Resources[current.ProviderID]
	if !ok {
		return nil, nil // deleted outside infra
	}
	st := p.toState(current.Address.String(), obj.Type, current.ProviderID, obj.Attributes)
	st.CreatedAt = current.CreatedAt
	st.UpdatedAt = current.UpdatedAt
	st.Dependencies = append([]address.Address(nil), current.Dependencies...)
	st.Lifecycle = current.Lifecycle
	return st, nil
}

func (p *Provider) Update(ctx context.Context, current *resource.ResourceState, d *resource.DesiredResource) (*resource.ResourceState, error) {
	c, err := p.begin(ctx, "update", d.Address.String())
	if err != nil {
		return nil, err
	}
	obj, ok := c.Resources[current.ProviderID]
	if !ok {
		return nil, fmt.Errorf("cannot update %s: %s no longer exists", d.Address, current.ProviderID)
	}
	for name, v := range d.Attrs {
		obj.Attributes[name] = v.Raw
	}
	if err := c.Save(p.cloudPath); err != nil {
		return nil, err
	}

	st := p.toState(d.Address.String(), obj.Type, current.ProviderID, obj.Attributes)
	st.CreatedAt = current.CreatedAt
	st.UpdatedAt = time.Now().UTC()
	st.Lifecycle = d.Lifecycle
	return st, nil
}

func (p *Provider) Delete(ctx context.Context, current *resource.ResourceState) error {
	c, err := p.begin(ctx, "delete", current.Address.String())
	if err != nil {
		return err
	}
	delete(c.Resources, current.ProviderID)
	return c.Save(p.cloudPath)
}

func (p *Provider) Discover(context.Context, provider.DiscoverRequest) ([]provider.DiscoveredResource, error) {
	return nil, fmt.Errorf("test provider: discovery arrives in Phase 2: %w", provider.ErrNotImplemented)
}

func (p *Provider) Import(context.Context, string, string) (*resource.ResourceState, error) {
	return nil, fmt.Errorf("test provider: import arrives in Phase 2: %w", provider.ErrNotImplemented)
}

// toState converts cloud JSON back into typed, provenance-carrying state. Every
// value a provider reports carries SourceProvider; the schema decides which are
// sensitive.
func (p *Provider) toState(addr, resourceType, id string, attrs map[string]any) *resource.ResourceState {
	def := p.byType[resourceType]
	out := map[string]value.Value{}
	for name, raw := range attrs {
		// A JSON null means the attribute is not set. Someone hand-editing the
		// cloud file may null a value out; turning that into the string
		// "<nil>" would silently corrupt it.
		if raw == nil {
			continue
		}
		v := fromRaw(raw)
		v = v.WithSource(value.SourceProvider)
		if def != nil {
			if a, ok := def.Attribute(name); ok && a.Sensitive {
				v = v.WithSensitive(true)
			}
		}
		out[name] = v
	}
	parsed, err := address.Parse(addr)
	if err != nil {
		parsed = address.Address{Name: addr}
	}
	return &resource.ResourceState{
		Address:    parsed,
		Type:       resourceType,
		Provider:   p.Name(),
		ProviderID: id,
		Attributes: out,
	}
}

func (p *Provider) computedFor(resourceType, id string) map[string]any {
	switch resourceType {
	case "test.network":
		return map[string]any{"id": id}
	case "test.database":
		return map[string]any{"endpoint": id + ".db.test"}
	case "test.application":
		return map[string]any{"url": "https://" + id + ".test"}
	default:
		return nil
	}
}

func idPrefix(resourceType string) string {
	switch resourceType {
	case "test.network":
		return "net"
	case "test.database":
		return "db"
	case "test.application":
		return "app"
	default:
		return strings.ReplaceAll(resourceType, ".", "-")
	}
}

// fromRaw converts a JSON datum into a typed Value. JSON numbers arrive as
// float64; whole numbers become integers so they compare equal to configured
// integer attributes.
func fromRaw(raw any) value.Value {
	switch v := raw.(type) {
	case string:
		return value.String(v, value.SourceProvider)
	case bool:
		return value.Bool(v, value.SourceProvider)
	case float64:
		if v == float64(int64(v)) {
			return value.Int(int64(v), value.SourceProvider)
		}
		return value.Float(v, value.SourceProvider)
	case int64:
		return value.Int(v, value.SourceProvider)
	case []any:
		items := make([]value.Value, 0, len(v))
		for _, item := range v {
			items = append(items, fromRaw(item))
		}
		return value.List(items, value.SourceProvider)
	case map[string]any:
		items := map[string]value.Value{}
		for k, item := range v {
			items[k] = fromRaw(item)
		}
		return value.Map(items, value.SourceProvider)
	case nil:
		// Unreachable for a top-level attribute (toState skips nulls), but a
		// null nested inside a list or map lands here. Return the zero Value,
		// whose KindInvalid fails loudly downstream rather than masquerading
		// as the string "<nil>".
		return value.Value{}
	default:
		return value.String(fmt.Sprint(v), value.SourceProvider)
	}
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./providers/test/ -v`
Expected: PASS — twelve tests across the package.

- [ ] **Step 6: Verify the layering rule**

Run: `go list -deps ./providers/... | grep '^infra/internal' || echo "OK: providers do not import internal"`
Expected: `OK: providers do not import internal`. If anything prints, a dependency rule from
Global Constraints has been broken and must be fixed before committing.

- [ ] **Step 7: Commit**

```bash
git add providers/test
git commit -m "feat: add fake provider with file-backed cloud"
```

---

## Task 11: State model and migrations

**Files:**
- Create: `internal/state/state.go`
- Test: `internal/state/state_test.go`

**Interfaces:**
- Consumes: `resource.ResourceState`, `address.Address`.
- Produces: `state.CurrentVersion`, `state.State`, `state.New(project, environment string) *State`, `(*State).Get`, `(*State).Set`, `(*State).Remove`, `(*State).Addresses() []address.Address`, `state.Decode([]byte) (*State, error)`, `(*State).Encode() ([]byte, error)`, `state.Migration`, `state.RegisterMigration`.

Migrations exist from the first release because retrofitting them once state files are in the
wild is far more expensive. `CurrentVersion` is 1 and there are no real migrations yet, so the
mechanism is proved by a test that registers one.

- [ ] **Step 1: Write the failing test**

Create `internal/state/state_test.go`:

```go
package state

import (
	"encoding/json"
	"testing"

	"infra/pkg/address"
	"infra/pkg/resource"
	"infra/pkg/value"
)

func sampleResource(name string) *resource.ResourceState {
	return &resource.ResourceState{
		Address:    address.Address{Name: name},
		Type:       "test.database",
		Provider:   "test",
		ProviderID: "db-1",
		Attributes: map[string]value.Value{
			"engine": value.String("postgres", value.SourceProvider),
			"size":   value.Int(10, value.SourceProvider),
		},
	}
}

func TestSetGetRemove(t *testing.T) {
	s := New("myapp", "dev")
	addr := address.Address{Name: "db"}

	if _, ok := s.Get(addr); ok {
		t.Error("empty state must not report a resource")
	}
	s.Set(sampleResource("db"))
	if _, ok := s.Get(addr); !ok {
		t.Error("Set then Get failed")
	}
	s.Remove(addr)
	if _, ok := s.Get(addr); ok {
		t.Error("Remove did not remove")
	}
}

func TestAddressesAreSorted(t *testing.T) {
	s := New("myapp", "dev")
	for _, n := range []string{"zebra", "alpha", "middle"} {
		s.Set(sampleResource(n))
	}
	got := s.Addresses()
	want := []string{"alpha", "middle", "zebra"}
	for i := range want {
		if got[i].String() != want[i] {
			t.Fatalf("Addresses() = %v, want %v", got, want)
		}
	}
}

func TestEncodeDecodePreservesTypedValues(t *testing.T) {
	s := New("myapp", "dev")
	s.Set(sampleResource("db"))

	data, err := s.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	r, ok := got.Get(address.Address{Name: "db"})
	if !ok {
		t.Fatal("resource missing after round trip")
	}
	if n, ok := r.Attributes["size"].AsInt(); !ok || n != 10 {
		t.Errorf("size = %d, %v; want 10, true — an integer must not decay to a float", n, ok)
	}
}

func TestDecodeRejectsFutureVersion(t *testing.T) {
	data := []byte(`{"version": 9999, "serial": 1, "resources": {}}`)
	if _, err := Decode(data); err == nil {
		t.Error("state written by a newer infra must be refused, not silently misread")
	}
}

func TestDecodeRunsMigrations(t *testing.T) {
	original := migrations
	t.Cleanup(func() { migrations = original })

	migrations = nil
	RegisterMigration(Migration{From: 0, To: 1, Apply: func(raw map[string]any) error {
		raw["project"] = "migrated"
		return nil
	}})

	got, err := Decode([]byte(`{"version": 0, "serial": 3, "resources": {}}`))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Project != "migrated" {
		t.Errorf("Project = %q, want \"migrated\" — the migration did not run", got.Project)
	}
	if got.Version != CurrentVersion {
		t.Errorf("Version = %d, want %d", got.Version, CurrentVersion)
	}
}

func TestDecodeRefusesNonAdvancingMigration(t *testing.T) {
	original := migrations
	t.Cleanup(func() { migrations = original })

	migrations = nil
	RegisterMigration(Migration{From: 0, To: 0, Apply: func(map[string]any) error { return nil }})

	_, err := Decode([]byte(`{"version": 0, "serial": 1, "resources": {}}`))
	if err == nil {
		t.Fatal("a migration that does not advance the version must be refused, not looped on forever")
	}
}

func TestDecodeRunsMultiStepChain(t *testing.T) {
	original := migrations
	t.Cleanup(func() { migrations = original })

	migrations = nil
	RegisterMigration(Migration{From: 0, To: 1, Apply: func(raw map[string]any) error {
		raw["project"] = "step-one"
		return nil
	}})

	got, err := Decode([]byte(`{"version": 0, "serial": 1, "resources": {}}`))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Project != "step-one" || got.Version != CurrentVersion {
		t.Errorf("chain did not run to completion: project=%q version=%d", got.Project, got.Version)
	}
}

func TestEncodeIsStableAcrossRuns(t *testing.T) {
	s := New("myapp", "dev")
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		s.Set(sampleResource(n))
	}
	first, err := s.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	for i := 0; i < 20; i++ {
		next, err := s.Encode()
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		if string(first) != string(next) {
			t.Fatal("state encoding must be byte-stable; Go map ordering is randomised and a churning state file makes diffs useless")
		}
	}
	var check map[string]any
	if err := json.Unmarshal(first, &check); err != nil {
		t.Fatalf("encoded state is not valid JSON: %v", err)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/state/ -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Implement the state model**

Create `internal/state/state.go`:

```go
// Package state holds the record of what infra manages. Spec §9.
package state

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"infra/pkg/address"
	"infra/pkg/resource"
)

// CurrentVersion is the state schema version this build writes.
const CurrentVersion = 1

type State struct {
	Version     int                                `json:"version"`
	Serial      uint64                             `json:"serial"`
	Project     string                             `json:"project"`
	Environment string                             `json:"environment"`
	Resources   map[string]*resource.ResourceState `json:"resources"`
	UpdatedAt   time.Time                          `json:"updated_at"`
}

func New(project, environment string) *State {
	return &State{
		Version:     CurrentVersion,
		Project:     project,
		Environment: environment,
		Resources:   map[string]*resource.ResourceState{},
	}
}

func (s *State) Get(addr address.Address) (*resource.ResourceState, bool) {
	r, ok := s.Resources[addr.String()]
	return r, ok
}

func (s *State) Set(r *resource.ResourceState) {
	if s.Resources == nil {
		s.Resources = map[string]*resource.ResourceState{}
	}
	s.Resources[r.Address.String()] = r
}

func (s *State) Remove(addr address.Address) {
	delete(s.Resources, addr.String())
}

// Addresses returns every managed address in sorted order.
func (s *State) Addresses() []address.Address {
	out := make([]address.Address, 0, len(s.Resources))
	for _, r := range s.Resources {
		out = append(out, r.Address)
	}
	address.Sort(out)
	return out
}

// Encode serialises state. Output is byte-stable for identical input: keys are
// sorted by encoding/json for maps, and indentation is fixed, so a state file
// only changes when the state actually changed.
func (s *State) Encode() ([]byte, error) {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// Migration transforms a decoded state document from one version to the next.
// It operates on the generic document rather than the typed struct, because the
// typed struct only ever describes CurrentVersion.
type Migration struct {
	From  int
	To    int
	Apply func(raw map[string]any) error
}

var migrations []Migration

func RegisterMigration(m Migration) { migrations = append(migrations, m) }

// Decode reads a state document, applying migrations until it is current.
func Decode(data []byte) (*State, error) {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("state is not valid JSON: %w", err)
	}

	version := 0
	if v, ok := raw["version"].(float64); ok {
		version = int(v)
	}
	if version > CurrentVersion {
		return nil, fmt.Errorf("state version %d was written by a newer version of infra; this build understands up to version %d", version, CurrentVersion)
	}

	for version < CurrentVersion {
		m, ok := migrationFrom(version)
		if !ok {
			return nil, fmt.Errorf("no migration registered from state version %d to %d", version, version+1)
		}
		// A migration that does not advance the version would loop forever.
		// Refuse it: a hang while loading state is far worse than an error,
		// because it gives the user nothing to act on.
		if m.To <= version {
			return nil, fmt.Errorf("migration from state version %d declares To=%d, which does not advance the version", m.From, m.To)
		}
		if err := m.Apply(raw); err != nil {
			return nil, fmt.Errorf("migrating state from version %d to %d: %w", m.From, m.To, err)
		}
		version = m.To
		raw["version"] = float64(version)
	}

	normalised, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}

	var s State
	if err := json.Unmarshal(normalised, &s); err != nil {
		return nil, err
	}
	if s.Resources == nil {
		s.Resources = map[string]*resource.ResourceState{}
	}
	s.Version = CurrentVersion
	return &s, nil
}

func migrationFrom(version int) (Migration, bool) {
	candidates := make([]Migration, 0, len(migrations))
	for _, m := range migrations {
		if m.From == version {
			candidates = append(candidates, m)
		}
	}
	if len(candidates) == 0 {
		return Migration{}, false
	}
	// Stable, so two migrations registered with identical From and To resolve
	// in registration order rather than arbitrarily.
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].To < candidates[j].To })
	return candidates[0], true
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/state/ -v`
Expected: PASS — six tests.

- [ ] **Step 5: Commit**

```bash
git add internal/state
git commit -m "feat: add versioned state model with migration chain"
```

---

## Task 12: Local state backend and locking

**Files:**
- Create: `internal/state/backend.go`, `internal/state/local.go`, `internal/state/lock.go`
- Test: `internal/state/local_test.go`, `internal/state/lock_test.go`

**Interfaces:**
- Consumes: `state.State` from Task 11.
- Produces: `state.Backend` interface, `state.Lock`, `state.ErrLocked`, `state.NewLocal(root string) *Local`, `(*Local).Get`, `(*Local).Put`, `(*Local).Lock`, `(*Local).Unlock`, `(*Local).ForceUnlock`, `(*Local).Inspect(environment string) (Lock, bool, error)`.

- [ ] **Step 1: Write the failing tests**

Create `internal/state/local_test.go`:

```go
package state

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"infra/pkg/address"
)

func TestGetMissingEnvironmentReturnsEmptyState(t *testing.T) {
	b := NewLocal(t.TempDir())
	s, err := b.Get(context.Background(), "dev")
	if err != nil {
		t.Fatalf("Get on a fresh project: %v", err)
	}
	if len(s.Resources) != 0 {
		t.Error("a project that has never applied has no resources; this must not be an error")
	}
}

func TestPutThenGetRoundTrips(t *testing.T) {
	b := NewLocal(t.TempDir())
	ctx := context.Background()

	s := New("myapp", "dev")
	s.Set(sampleResource("db"))
	if err := b.Put(ctx, "dev", s); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := b.Get(ctx, "dev")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, ok := got.Get(address.Address{Name: "db"}); !ok {
		t.Error("resource missing after Put/Get")
	}
}

func TestPutIncrementsSerial(t *testing.T) {
	b := NewLocal(t.TempDir())
	ctx := context.Background()
	s := New("myapp", "dev")

	if err := b.Put(ctx, "dev", s); err != nil {
		t.Fatalf("Put: %v", err)
	}
	first := s.Serial
	if err := b.Put(ctx, "dev", s); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if s.Serial != first+1 {
		t.Errorf("Serial = %d, want %d — every write must advance the serial so plan staleness can be detected", s.Serial, first+1)
	}
}

func TestFailedPutDoesNotAdvanceSerial(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; directory permissions would not block the write")
	}

	root := t.TempDir()
	b := NewLocal(root)
	ctx := context.Background()
	s := New("myapp", "dev")

	if err := b.Put(ctx, "dev", s); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	before := s.Serial

	// Make the state directory unwritable so the temp file cannot be created.
	stateDir := filepath.Join(root, "state")
	if err := os.Chmod(stateDir, 0o500); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(stateDir, 0o700) })

	if err := b.Put(ctx, "dev", s); err == nil {
		t.Fatal("Put into an unwritable directory should fail")
	}
	if s.Serial != before {
		t.Errorf("Serial = %d after a failed Put, want %d unchanged — a serial ahead of disk makes a retry skip a value and misreports staleness", s.Serial, before)
	}
}

func TestPutIsAtomicAndPrivate(t *testing.T) {
	root := t.TempDir()
	b := NewLocal(root)
	if err := b.Put(context.Background(), "dev", New("myapp", "dev")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	path := filepath.Join(root, "state", "dev.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("state file mode = %o, want 600 — state holds sensitive values (spec §9.3)", perm)
	}

	entries, err := os.ReadDir(filepath.Join(root, "state"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("temporary files were left behind: %v", entries)
	}
}

func TestEnvironmentsAreIndependent(t *testing.T) {
	b := NewLocal(t.TempDir())
	ctx := context.Background()

	dev := New("myapp", "dev")
	dev.Set(sampleResource("db"))
	if err := b.Put(ctx, "dev", dev); err != nil {
		t.Fatalf("Put dev: %v", err)
	}

	prod, err := b.Get(ctx, "production")
	if err != nil {
		t.Fatalf("Get production: %v", err)
	}
	if len(prod.Resources) != 0 {
		t.Error("environments must have completely independent state (PLAN.md §6)")
	}
}
```

Create `internal/state/lock_test.go`:

```go
package state

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestSecondLockOnSameEnvironmentFails(t *testing.T) {
	b := NewLocal(t.TempDir())
	ctx := context.Background()

	if _, err := b.Lock(ctx, "production"); err != nil {
		t.Fatalf("first Lock: %v", err)
	}
	_, err := b.Lock(ctx, "production")
	if err == nil {
		t.Fatal("a second lock on the same environment must fail — invariant 5")
	}
	if !errors.Is(err, ErrLocked) {
		t.Errorf("error = %v, want one wrapping ErrLocked", err)
	}
}

func TestLockErrorNamesTheHolder(t *testing.T) {
	b := NewLocal(t.TempDir())
	ctx := context.Background()
	if _, err := b.Lock(ctx, "production"); err != nil {
		t.Fatalf("first Lock: %v", err)
	}

	_, err := b.Lock(ctx, "production")
	if err == nil {
		t.Fatal("expected a conflict")
	}
	msg := err.Error()
	for _, want := range []string{"held by", "pid"} {
		if !strings.Contains(strings.ToLower(msg), want) {
			t.Errorf("conflict message %q does not say %q — it must identify who holds the lock (spec §9.2)", msg, want)
		}
	}
}

func TestDifferentEnvironmentsLockIndependently(t *testing.T) {
	b := NewLocal(t.TempDir())
	ctx := context.Background()

	if _, err := b.Lock(ctx, "production"); err != nil {
		t.Fatalf("Lock production: %v", err)
	}
	if _, err := b.Lock(ctx, "dev"); err != nil {
		t.Errorf("Lock dev while production is locked: %v — concurrent applies to different environments are allowed", err)
	}
}

func TestUnlockThenRelock(t *testing.T) {
	b := NewLocal(t.TempDir())
	ctx := context.Background()

	if _, err := b.Lock(ctx, "dev"); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if err := b.Unlock(ctx, "dev"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if _, err := b.Lock(ctx, "dev"); err != nil {
		t.Errorf("relock after unlock: %v", err)
	}
}

func TestInspectDescribesTheLock(t *testing.T) {
	b := NewLocal(t.TempDir())
	ctx := context.Background()

	if _, held, err := b.Inspect("dev"); err != nil || held {
		t.Fatalf("Inspect before locking = held %v, err %v", held, err)
	}
	if _, err := b.Lock(ctx, "dev"); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	lock, held, err := b.Inspect("dev")
	if err != nil || !held {
		t.Fatalf("Inspect after locking = held %v, err %v", held, err)
	}
	if lock.PID == 0 || lock.Host == "" || lock.Environment != "dev" {
		t.Errorf("lock descriptor is incomplete: %#v", lock)
	}
}

func TestUnlockRefusesAnotherProcessesLockButForceSucceeds(t *testing.T) {
	root := t.TempDir()
	b := NewLocal(root)
	ctx := context.Background()

	// Write a lock file as if another process holds it.
	stateDir := filepath.Join(root, "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	held := `{"environment":"production","pid":999999,"host":"elsewhere","user":"someone","operation":"apply","at":"2026-09-09T10:00:00Z"}`
	if err := os.WriteFile(filepath.Join(stateDir, "production.lock"), []byte(held), 0o600); err != nil {
		t.Fatalf("write lock: %v", err)
	}

	if err := b.Unlock(ctx, "production"); err == nil {
		t.Error("Unlock must refuse a lock held by another process, or `force` means nothing")
	}
	if err := b.ForceUnlock("production"); err != nil {
		t.Errorf("ForceUnlock should override: %v", err)
	}
	if _, ok, _ := b.Inspect("production"); ok {
		t.Error("the lock should be gone after ForceUnlock")
	}
}

func TestConcurrentLockAttemptsElectExactlyOneWinner(t *testing.T) {
	// The other lock tests are single-goroutine: they verify the observable
	// contract, but a naive stat-then-create implementation would pass every
	// one of them. This is the only test that actually exercises the race
	// O_EXCL exists to prevent, and so the only direct proof of invariant 5.
	b := NewLocal(t.TempDir())
	ctx := context.Background()

	const goroutines = 16
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		granted int
		refused int
	)
	start := make(chan struct{})

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release together, to maximise contention
			_, err := b.Lock(ctx, "production")

			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				granted++
			case errors.Is(err, ErrLocked):
				refused++
			default:
				t.Errorf("unexpected lock error: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if granted != 1 {
		t.Fatalf("%d goroutines acquired the lock, want exactly 1 — invariant 5", granted)
	}
	if refused != goroutines-1 {
		t.Errorf("%d goroutines saw ErrLocked, want %d", refused, goroutines-1)
	}
}

func TestUnlockUnheldEnvironmentIsAnError(t *testing.T) {
	b := NewLocal(t.TempDir())
	if err := b.Unlock(context.Background(), "dev"); err == nil {
		t.Error("unlocking an environment that is not locked must report that clearly rather than succeeding silently")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/state/ -run 'TestGet|TestPut|TestEnvironments|TestSecond|TestLock|TestDifferent|TestUnlock|TestInspect' -v`
Expected: FAIL — `undefined: NewLocal`, `undefined: ErrLocked`.

- [ ] **Step 3: Define the backend interface**

Create `internal/state/backend.go`:

```go
package state

import (
	"context"
	"errors"
	"time"
)

// ErrLocked is wrapped by every lock conflict so callers can test for it.
var ErrLocked = errors.New("environment is locked")

// Lock describes who holds an environment lock. It exists so a conflict can
// report the holder rather than merely refusing. Spec §9.2.
type Lock struct {
	Environment string    `json:"environment"`
	PID         int       `json:"pid"`
	Host        string    `json:"host"`
	User        string    `json:"user"`
	Operation   string    `json:"operation"`
	At          time.Time `json:"at"`
}

// Backend is the storage abstraction from PLAN.md §21. Get and Put move whole
// states, which is what lets the same shape serve a local file today and an S3
// object in Phase 4.
type Backend interface {
	Get(ctx context.Context, environment string) (*State, error)
	Put(ctx context.Context, environment string, s *State) error
	Lock(ctx context.Context, environment string) (Lock, error)
	Unlock(ctx context.Context, environment string) error
}
```

- [ ] **Step 4: Implement the local backend**

Create `internal/state/local.go`:

```go
package state

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Local stores one JSON document per environment beneath a project root,
// conventionally ".infra".
type Local struct {
	root string
}

func NewLocal(root string) *Local { return &Local{root: root} }

func (l *Local) statePath(environment string) string {
	return filepath.Join(l.root, "state", environment+".json")
}

func (l *Local) Get(ctx context.Context, environment string) (*State, error) {
	data, err := os.ReadFile(l.statePath(environment))
	if errors.Is(err, fs.ErrNotExist) {
		return New("", environment), nil
	}
	if err != nil {
		return nil, err
	}
	s, err := Decode(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", l.statePath(environment), err)
	}
	return s, nil
}

// Put writes state atomically: a temporary file in the same directory, then a
// rename. A crash mid-write therefore cannot corrupt state.
func (l *Local) Put(ctx context.Context, environment string, s *State) error {
	path := l.statePath(environment)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	// Stamp the write, but restore on any failure path: a Serial that has
	// advanced past what is on disk would make a retry skip a value, and any
	// caller inspecting s.Serial after a failed Put would believe a write
	// happened. The serial is what detects stale plans, so it must not drift.
	prevSerial, prevEnv, prevUpdated := s.Serial, s.Environment, s.UpdatedAt
	committed := false
	defer func() {
		if !committed {
			s.Serial, s.Environment, s.UpdatedAt = prevSerial, prevEnv, prevUpdated
		}
	}()

	s.Serial++
	s.Environment = environment
	s.UpdatedAt = time.Now().UTC()

	data, err := s.Encode()
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename has succeeded

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	committed = true
	return nil
}
```

- [ ] **Step 5: Implement locking**

Create `internal/state/lock.go`:

```go
package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"time"
)

func (l *Local) lockPath(environment string) string {
	return filepath.Join(l.root, "state", environment+".lock")
}

// Lock acquires an exclusive environment lock by creating a file with O_EXCL.
// Locks never expire: a timeout that guesses wrong is exactly how two applies
// end up running at once. Spec §9.2.
func (l *Local) Lock(ctx context.Context, environment string) (Lock, error) {
	path := l.lockPath(environment)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Lock{}, err
	}

	lock := Lock{
		Environment: environment,
		PID:         os.Getpid(),
		Host:        hostname(),
		User:        username(),
		Operation:   operationFromContext(ctx),
		At:          time.Now().UTC(),
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, fs.ErrExist) {
		held, _, inspectErr := l.Inspect(environment)
		if inspectErr != nil {
			return Lock{}, fmt.Errorf("environment %q is locked, and the lock file could not be read: %w", environment, ErrLocked)
		}
		return Lock{}, fmt.Errorf("environment %q is locked: held by %s on %s (pid %d) since %s, running %q; release it with `infra state unlock %s`: %w",
			environment, held.User, held.Host, held.PID, held.At.Format(time.RFC3339), held.Operation, environment, ErrLocked)
	}
	if err != nil {
		return Lock{}, err
	}
	defer f.Close()

	data, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return Lock{}, err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		return Lock{}, err
	}
	return lock, nil
}

// Unlock releases a lock this process holds. It refuses to release a lock held
// by anyone else — otherwise "force" would mean nothing and a stray Unlock
// could free an environment another apply is actively mutating.
func (l *Local) Unlock(ctx context.Context, environment string) error {
	held, ok, err := l.Inspect(environment)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("environment %q is not locked", environment)
	}
	if held.PID != os.Getpid() || held.Host != hostname() {
		return fmt.Errorf("environment %q is locked by %s on %s (pid %d), not by this process; use `infra state unlock %s` to override",
			environment, held.User, held.Host, held.PID, environment)
	}
	return l.removeLock(environment)
}

// ForceUnlock removes a lock regardless of holder. `infra state unlock` uses it
// after telling the user who holds the lock.
func (l *Local) ForceUnlock(environment string) error {
	return l.removeLock(environment)
}

// removeLock deletes the lock file, reporting a missing lock as an error rather
// than a silent success — a typo in an environment name must not look like it
// worked.
func (l *Local) removeLock(environment string) error {
	err := os.Remove(l.lockPath(environment))
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("environment %q is not locked", environment)
	}
	return err
}

// Inspect reports the current lock holder, if any.
func (l *Local) Inspect(environment string) (Lock, bool, error) {
	data, err := os.ReadFile(l.lockPath(environment))
	if errors.Is(err, fs.ErrNotExist) {
		return Lock{}, false, nil
	}
	if err != nil {
		return Lock{}, false, err
	}
	var lock Lock
	if err := json.Unmarshal(data, &lock); err != nil {
		return Lock{}, true, fmt.Errorf("lock file for %q is malformed: %w", environment, err)
	}
	return lock, true, nil
}

type contextKey string

const operationKey contextKey = "infra.operation"

// WithOperation labels a context with the operation being performed, so a lock
// conflict can say what the holder is doing.
func WithOperation(ctx context.Context, op string) context.Context {
	return context.WithValue(ctx, operationKey, op)
}

func operationFromContext(ctx context.Context) string {
	if op, ok := ctx.Value(operationKey).(string); ok && op != "" {
		return op
	}
	return "unknown"
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}

func username() string {
	u, err := user.Current()
	if err != nil {
		return "unknown"
	}
	return u.Username
}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/state/ -v`
Expected: PASS — seventeen tests across the package.

- [ ] **Step 7: Confirm Local satisfies Backend**

Add to `internal/state/local.go`, beneath the `Local` declaration:

```go
var _ Backend = (*Local)(nil)
```

Run: `go build ./...`
Expected: no output. A compile error here means a method signature drifted from the interface.

- [ ] **Step 8: Commit**

```bash
git add internal/state
git commit -m "feat: add local state backend with atomic writes and locking"
```

---

## Task 13: Compiler stages 1 and 2 — load and decode

**Files:**
- Create: `internal/config/declarations.go`, `internal/config/load.go`, `internal/config/decode.go`
- Test: `internal/config/decode_test.go`

**Interfaces:**
- Consumes: `value.*`, `diag.*`.
- Produces: `config.File`, `config.Load(dir string) ([]File, error)`, `config.ProjectDecl`, `config.ResourceDecl`, `config.AttributeDecl`, `config.LifecycleDecl`, `config.Decode(files []File) (*ProjectDecl, diag.Diagnostics)`.

M1 loads `infra.yml` only. `variables.yml` and `environments/*.yml` are loaded and decoded in
M4, when the variable and environment systems exist; loading them earlier would produce data
nothing consumes.

Attribute values containing `${` are preserved verbatim as strings and flagged. M2's stage 6
replaces `AttributeDecl.Value` with a parsed expression tree; M1 must not lose the source.

- [ ] **Step 1: Write the failing test**

Create `internal/config/decode_test.go`:

```go
package config

import (
	"os"
	"path/filepath"
	"testing"

	"infra/pkg/value"
)

func writeConfig(t *testing.T, body string) []File {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "infra.yml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	files, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return files
}

func TestDecodeResources(t *testing.T) {
	files := writeConfig(t, `
project: myapp

resources:
  network:
    type: test.network
    cidr: 10.20.0.0/16

  database:
    type: test.database
    engine: postgres
    size: 50
`)
	got, ds := Decode(files)
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if got.Project != "myapp" {
		t.Errorf("Project = %q", got.Project)
	}
	if len(got.Resources) != 2 {
		t.Fatalf("decoded %d resources, want 2", len(got.Resources))
	}

	// Resources are returned in a deterministic order.
	if got.Resources[0].Name != "database" || got.Resources[1].Name != "network" {
		t.Errorf("resources are not sorted by name: %s, %s", got.Resources[0].Name, got.Resources[1].Name)
	}

	db := got.Resources[0]
	if db.Type != "test.database" {
		t.Errorf("Type = %q", db.Type)
	}
	size, ok := db.Attributes["size"]
	if !ok {
		t.Fatal("size attribute missing")
	}
	if size.Value.Kind != value.KindInt {
		t.Errorf("size Kind = %v, want KindInt", size.Value.Kind)
	}
	if n, _ := size.Value.AsInt(); n != 50 {
		t.Errorf("size = %d, want 50", n)
	}
	if size.Value.Source != value.SourceExplicit {
		t.Errorf("Source = %v, want SourceExplicit — everything written in configuration is explicit", size.Value.Source)
	}
}

func TestDecodeRecordsOrigins(t *testing.T) {
	files := writeConfig(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: 10.20.0.0/16
`)
	got, _ := Decode(files)
	attr := got.Resources[0].Attributes["cidr"]
	if attr.Origin.Line == 0 {
		t.Error("attribute origin has no line number; diagnostics depend on it")
	}
	if filepath.Base(attr.Origin.File) != "infra.yml" {
		t.Errorf("origin file = %q", attr.Origin.File)
	}
}

func TestDecodePreservesExpressionSource(t *testing.T) {
	files := writeConfig(t, `
project: myapp
resources:
  application:
    type: test.application
    image: myapp:latest
    database_url: ${database.endpoint}
`)
	got, ds := Decode(files)
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	attr := got.Resources[0].Attributes["database_url"]
	if !attr.HasExpressions {
		t.Error("an attribute containing ${...} must be flagged so stage 6 can parse it in M2")
	}
	if s, _ := attr.Value.AsString(); s != "${database.endpoint}" {
		t.Errorf("expression source = %q; it must be preserved verbatim", s)
	}
}

func TestDecodeLifecycleAndDependsOn(t *testing.T) {
	files := writeConfig(t, `
project: myapp
resources:
  database:
    type: test.database
    engine: postgres
    depends_on: [network]
    lifecycle:
      prevent_destroy: true
      retain: true
`)
	got, ds := Decode(files)
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	r := got.Resources[0]
	if !r.Lifecycle.PreventDestroy || !r.Lifecycle.Retain {
		t.Errorf("lifecycle not decoded: %#v", r.Lifecycle)
	}
	if len(r.DependsOn) != 1 || r.DependsOn[0] != "network" {
		t.Errorf("depends_on = %v", r.DependsOn)
	}
	if _, ok := r.Attributes["lifecycle"]; ok {
		t.Error("lifecycle is structure, not an attribute, and must not appear in Attributes")
	}
	if _, ok := r.Attributes["depends_on"]; ok {
		t.Error("depends_on is structure, not an attribute")
	}
}

func TestDecodeReportsMissingType(t *testing.T) {
	files := writeConfig(t, `
project: myapp
resources:
  database:
    engine: postgres
`)
	_, ds := Decode(files)
	if !ds.HasErrors() {
		t.Fatal("a resource with no type must be an error")
	}
	if ds[0].Origin.Line == 0 {
		t.Error("the diagnostic must point at the offending resource")
	}
}

func TestDecodeCollectsEveryError(t *testing.T) {
	files := writeConfig(t, `
project: myapp
resources:
  a:
    engine: postgres
  b:
    engine: mysql
  c:
    engine: sqlite
`)
	_, ds := Decode(files)
	if len(ds) < 3 {
		t.Errorf("got %d diagnostics, want at least 3 — validation reports every problem in one pass (spec §7.4)", len(ds))
	}
}

func TestLoadReportsMissingProjectFile(t *testing.T) {
	if _, err := Load(t.TempDir()); err == nil {
		t.Error("a directory with no infra.yml must be an error naming the missing file")
	}
}

func TestDecodeReportsMalformedYAML(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "infra.yml"), []byte("project: [unclosed\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Load(dir); err == nil {
		t.Error("malformed YAML must be reported")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/config/ -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Implement the declarations**

Create `internal/config/declarations.go`:

```go
// Package config implements compiler stages 1 (load) and 2 (decode).
//
// Stage 2 is the only place in the engine permitted to touch yaml.Node. Every
// later stage works with the typed declarations defined here. Spec §7.
package config

import "infra/pkg/value"

// AttributeDecl is one configured attribute.
//
// In M1, Value holds the literal datum, and any string containing "${" is kept
// verbatim with HasExpressions set. M2's stage 6 replaces this with a parsed
// expression tree.
type AttributeDecl struct {
	Name           string
	Value          value.Value
	HasExpressions bool
	Origin         value.Origin
}

type LifecycleDecl struct {
	PreventDestroy bool
	Retain         bool
}

type ResourceDecl struct {
	Name       string
	Type       string
	Attributes map[string]AttributeDecl
	DependsOn  []string
	Lifecycle  LifecycleDecl
	Origin     value.Origin
}

// ProjectDecl is the decoded, still-unresolved configuration.
type ProjectDecl struct {
	Project   string
	Resources []*ResourceDecl // sorted by Name
	Origin    value.Origin
}
```

- [ ] **Step 4: Implement stage 1 (load)**

Create `internal/config/load.go`:

```go
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// ProjectFileName is the root configuration file.
const ProjectFileName = "infra.yml"

// File is a parsed source file. The node tree keeps line and column
// information, which every diagnostic depends on.
type File struct {
	Path string
	Root *yaml.Node
}

// Load reads the project file from dir.
//
// M4 extends this to variables.yml and environments/*.yml, when the systems
// that consume them exist.
func Load(dir string) ([]File, error) {
	path := filepath.Join(dir, ProjectFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no %s found in %s; run `infra init` to create one", ProjectFileName, dir)
		}
		return nil, err
	}

	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return []File{{Path: path, Root: &root}}, nil
}
```

- [ ] **Step 5: Implement stage 2 (decode)**

Create `internal/config/decode.go`:

```go
package config

import (
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"infra/internal/diag"
	"infra/pkg/value"
)

// Decode converts parsed YAML into typed declarations, collecting every problem
// rather than stopping at the first.
func Decode(files []File) (*ProjectDecl, diag.Diagnostics) {
	var ds diag.Diagnostics
	out := &ProjectDecl{}

	for _, f := range files {
		doc := documentRoot(f.Root)
		if doc == nil {
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
				Summary:  "configuration must be a mapping",
				Detail:   "The top level of " + ProjectFileName + " must be a set of keys such as `project` and `resources`.",
				Origin:   originOf(f.Path, doc),
			})
			continue
		}
		out.Origin = originOf(f.Path, doc)
		decodeDocument(f.Path, doc, out, &ds)
	}

	sort.Slice(out.Resources, func(i, j int) bool { return out.Resources[i].Name < out.Resources[j].Name })
	return out, ds
}

func decodeDocument(path string, doc *yaml.Node, out *ProjectDecl, ds *diag.Diagnostics) {
	for i := 0; i+1 < len(doc.Content); i += 2 {
		key, val := doc.Content[i], doc.Content[i+1]
		switch key.Value {
		case "project":
			out.Project = val.Value
		case "resources":
			decodeResources(path, val, out, ds)
		default:
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityWarning,
				Summary:  "unrecognised top-level key " + strconv.Quote(key.Value),
				Detail:   "M1 understands `project` and `resources`.",
				Origin:   originOf(path, key),
			})
		}
	}
}

func decodeResources(path string, node *yaml.Node, out *ProjectDecl, ds *diag.Diagnostics) {
	if node.Kind != yaml.MappingNode {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "`resources` must be a mapping of logical name to resource",
			Origin:   originOf(path, node),
		})
		return
	}

	for i := 0; i+1 < len(node.Content); i += 2 {
		nameNode, body := node.Content[i], node.Content[i+1]
		r := &ResourceDecl{
			Name:       nameNode.Value,
			Attributes: map[string]AttributeDecl{},
			Origin:     originOf(path, nameNode),
		}

		if body.Kind != yaml.MappingNode {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "resource " + strconv.Quote(r.Name) + " must be a mapping",
				Origin:   originOf(path, body),
			})
			continue
		}

		for j := 0; j+1 < len(body.Content); j += 2 {
			key, val := body.Content[j], body.Content[j+1]
			switch key.Value {
			case "type":
				r.Type = val.Value
			case "depends_on":
				for _, item := range val.Content {
					r.DependsOn = append(r.DependsOn, item.Value)
				}
			case "lifecycle":
				decodeLifecycle(path, val, r, ds)
			default:
				v, hasExpr := decodeValue(path, val)
				r.Attributes[key.Value] = AttributeDecl{
					Name:           key.Value,
					Value:          v,
					HasExpressions: hasExpr,
					Origin:         originOf(path, key),
				}
			}
		}

		if r.Type == "" {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "resource " + strconv.Quote(r.Name) + " has no `type`",
				Detail:   "Every resource must name the provider resource type it manages, for example `type: test.database`.",
				Action:   "Add a `type` key to " + strconv.Quote(r.Name) + ".",
				Origin:   r.Origin,
			})
		}
		out.Resources = append(out.Resources, r)
	}
}

func decodeLifecycle(path string, node *yaml.Node, r *ResourceDecl, ds *diag.Diagnostics) {
	if node.Kind != yaml.MappingNode {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "`lifecycle` must be a mapping",
			Origin:   originOf(path, node),
		})
		return
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, val := node.Content[i], node.Content[i+1]
		switch key.Value {
		case "prevent_destroy":
			r.Lifecycle.PreventDestroy = val.Value == "true"
		case "retain":
			r.Lifecycle.Retain = val.Value == "true"
		default:
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "unknown lifecycle option " + strconv.Quote(key.Value),
				Detail:   "Supported options are `prevent_destroy` and `retain`.",
				Origin:   originOf(path, key),
			})
		}
	}
}

// decodeValue converts a YAML node into a typed Value, reporting whether any
// string within it contains an interpolation.
func decodeValue(path string, node *yaml.Node) (value.Value, bool) {
	origin := originOf(path, node)

	switch node.Kind {
	case yaml.SequenceNode:
		items := make([]value.Value, 0, len(node.Content))
		anyExpr := false
		for _, item := range node.Content {
			v, has := decodeValue(path, item)
			anyExpr = anyExpr || has
			items = append(items, v)
		}
		return value.List(items, value.SourceExplicit).WithOrigin(origin), anyExpr

	case yaml.MappingNode:
		items := map[string]value.Value{}
		anyExpr := false
		for i := 0; i+1 < len(node.Content); i += 2 {
			v, has := decodeValue(path, node.Content[i+1])
			anyExpr = anyExpr || has
			items[node.Content[i].Value] = v
		}
		return value.Map(items, value.SourceExplicit).WithOrigin(origin), anyExpr

	default:
		return decodeScalar(node, origin)
	}
}

func decodeScalar(node *yaml.Node, origin value.Origin) (value.Value, bool) {
	raw := node.Value

	// An interpolation is kept verbatim; stage 6 parses it in M2.
	if strings.Contains(raw, "${") {
		return value.String(raw, value.SourceExplicit).WithOrigin(origin), true
	}

	// A quoted scalar is always a string, whatever it looks like.
	if node.Style == yaml.DoubleQuotedStyle || node.Style == yaml.SingleQuotedStyle {
		return value.String(raw, value.SourceExplicit).WithOrigin(origin), false
	}

	switch node.Tag {
	case "!!int":
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return value.Int(n, value.SourceExplicit).WithOrigin(origin), false
		}
	case "!!float":
		if f, err := strconv.ParseFloat(raw, 64); err == nil {
			return value.Float(f, value.SourceExplicit).WithOrigin(origin), false
		}
	case "!!bool":
		return value.Bool(raw == "true", value.SourceExplicit).WithOrigin(origin), false
	}
	return value.String(raw, value.SourceExplicit).WithOrigin(origin), false
}

func documentRoot(n *yaml.Node) *yaml.Node {
	if n == nil {
		return nil
	}
	if n.Kind == yaml.DocumentNode {
		if len(n.Content) == 0 {
			return nil
		}
		return n.Content[0]
	}
	return n
}

func originOf(path string, n *yaml.Node) value.Origin {
	if n == nil {
		return value.Origin{File: path}
	}
	return value.Origin{File: path, Line: n.Line, Column: n.Column}
}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/config/ -v`
Expected: PASS — eight tests.

- [ ] **Step 7: Commit**

```bash
git add internal/config
git commit -m "feat: add config loading and decoding into typed declarations"
```

---

## Task 14: validate and state commands

**Files:**
- Modify: `internal/cli/validate.go` (replace the Task 1 stub), `internal/cli/state.go` (replace the Task 1 stub), `internal/cli/root.go`
- Create: `internal/cli/context.go`
- Test: `internal/cli/validate_test.go`

**Interfaces:**
- Consumes: `config.Load`, `config.Decode`, `registry.New`, `test.New`, `state.NewLocal`, `diag.Diagnostics`.
- Produces: `cli.buildRegistry() *registry.Registry`, `cli.validateProject(dir string, reg *registry.Registry) diag.Diagnostics`, working `infra validate`, `infra state list`, `infra state show`, `infra state unlock`.

M1's `validate` performs three checks beyond YAML well-formedness: every resource type is
registered, every configured attribute exists in that type's schema, and no configured
attribute is `Computed`. Kind checking, defaults, references and requirements arrive with
stage 7 in M2.

- [ ] **Step 1: Write the failing test**

Create `internal/cli/validate_test.go`:

```go
package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
```

Add this helper to the bottom of the same file. `strings.Builder` satisfies `io.Writer`, so
it can receive rendered diagnostics directly:

```go
func renderToString(ds diag.Diagnostics) string {
	var b strings.Builder
	ds.Render(&b)
	return b.String()
}
```

The test file's import block is therefore:

```go
import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"infra/internal/diag"
)
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/cli/ -run TestValidate -v`
Expected: FAIL — `undefined: validateProject`, `undefined: buildRegistry`.

- [ ] **Step 3: Implement the shared command context**

Create `internal/cli/context.go`:

```go
package cli

import (
	"path/filepath"

	"infra/internal/registry"
	"infra/internal/state"
	"infra/providers/test"
)

// StateDirName is the per-project directory holding state, locks and, for the
// test provider, the fake cloud.
const StateDirName = ".infra"

// buildRegistry constructs the provider registry for a project directory.
// M1 registers only the test provider; the AWS provider joins it in Phase 3.
func buildRegistry(dir string) *registry.Registry {
	reg := registry.New()
	cloudPath := filepath.Join(dir, test.DefaultCloudPath)
	if err := reg.Register(test.New(cloudPath)); err != nil {
		// A malformed built-in schema is a programming error, not user error.
		panic("registering the test provider: " + err.Error())
	}
	return reg
}

func backendFor(dir string) *state.Local {
	return state.NewLocal(filepath.Join(dir, StateDirName))
}
```

- [ ] **Step 4: Implement validate**

Replace `internal/cli/validate.go` entirely:

```go
package cli

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"infra/internal/config"
	"infra/internal/diag"
	"infra/internal/registry"
	"infra/pkg/schema"
)

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

// validateProject runs compiler stages 1 and 2, then the checks that need only
// the registry. Kind checking, defaults, references and requirements arrive
// with stage 7 in M2.
func validateProject(dir string, reg *registry.Registry) diag.Diagnostics {
	var ds diag.Diagnostics

	files, err := config.Load(dir)
	if err != nil {
		ds.Add(diag.Diagnostic{Severity: diag.SeverityError, Summary: err.Error()})
		return ds
	}

	project, decodeDiags := config.Decode(files)
	ds.Extend(decodeDiags)

	for _, r := range project.Resources {
		if r.Type == "" {
			continue // already reported by Decode
		}

		def, ok := reg.Definition(r.Type)
		if !ok {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "unknown resource type " + strconv.Quote(r.Type),
				Detail:   "Known types:\n  " + strings.Join(reg.Types(), "\n  "),
				Action:   "Correct the `type`, or check that the provider offering it is available.",
				Origin:   r.Origin,
			})
			continue
		}

		for name, attr := range r.Attributes {
			schemaAttr, known := def.Attribute(name)
			if !known {
				ds.Add(diag.Diagnostic{
					Severity: diag.SeverityError,
					Summary:  fmt.Sprintf("%s has no attribute %s", r.Type, strconv.Quote(name)),
					Detail:   "Attributes of " + r.Type + ":\n  " + strings.Join(attributeNames(def), "\n  "),
					Action:   "Remove the attribute, or run `infra explain " + r.Type + "`.",
					Origin:   attr.Origin,
				})
				continue
			}
			if schemaAttr.Computed {
				ds.Add(diag.Diagnostic{
					Severity: diag.SeverityError,
					Summary:  fmt.Sprintf("%s is computed and cannot be set", strconv.Quote(name)),
					Detail:   "The provider determines this value. It can be referenced by other resources, but not configured.",
					Origin:   attr.Origin,
				})
			}
		}
	}

	return ds
}

// attributeNames lists a type's attributes in sorted order, for the "did you
// mean" detail on an unknown-attribute diagnostic.
func attributeNames(def *schema.ResourceDefinition) []string {
	names := make([]string, 0, len(def.Attributes))
	for name := range def.Attributes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
```

- [ ] **Step 5: Implement the state commands**

Replace `internal/cli/state.go` entirely:

```go
package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"infra/pkg/address"
)

func newStateCommand(opts *GlobalOptions) *cobra.Command {
	cmd := &cobra.Command{Use: "state", Short: "Inspect and manage recorded state"}
	cmd.AddCommand(newStateListCommand(opts), newStateShowCommand(opts), newStateUnlockCommand(opts))
	return cmd
}

func newStateListCommand(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "list <environment>",
		Short: "List managed resources",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := backendFor(opts.Dir).Get(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			addrs := s.Addresses()
			if len(addrs) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "No resources are managed in environment %q.\n", args[0])
				return nil
			}
			for _, a := range addrs {
				r, _ := s.Get(a)
				fmt.Fprintf(cmd.OutOrStdout(), "%s.%s\n", r.Type, a.String())
			}
			return nil
		},
	}
}

func newStateShowCommand(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "show <environment> <address>",
		Short: "Show one managed resource",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := backendFor(opts.Dir).Get(cmd.Context(), args[0])
			if err != nil {
				return err
			}

			addr, err := resolveAddress(args[1], s.Addresses(), func(a address.Address) string {
				r, _ := s.Get(a)
				return r.Type
			})
			if err != nil {
				return err
			}

			r, ok := s.Get(addr)
			if !ok {
				return fmt.Errorf("%s is not managed in environment %q", addr, args[0])
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%s.%s\n", r.Type, r.Address)
			fmt.Fprintf(out, "  provider     %s\n", r.Provider)
			fmt.Fprintf(out, "  provider_id  %s\n", r.ProviderID)
			for _, name := range sortedAttributeKeys(r.Attributes) {
				v := r.Attributes[name]
				if v.Sensitive {
					fmt.Fprintf(out, "  %-12s <sensitive>\n", name)
					continue
				}
				fmt.Fprintf(out, "  %-12s %v\n", name, v.Raw)
			}
			return nil
		},
	}
}

func newStateUnlockCommand(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "unlock <environment>",
		Short: "Release a lock left behind by an interrupted run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b := backendFor(opts.Dir)
			lock, held, err := b.Inspect(args[0])
			if err != nil {
				return err
			}
			if !held {
				return fmt.Errorf("environment %q is not locked", args[0])
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Releasing lock held by %s on %s (pid %d) since %s.\n",
				lock.User, lock.Host, lock.PID, lock.At.Format("2006-01-02 15:04:05 MST"))
			return b.ForceUnlock(args[0])
		},
	}
}

// resolveAddress accepts a canonical address, or a "type.name" display form
// when it is unambiguous. Spec §5.2.
func resolveAddress(input string, known []address.Address, typeOf func(address.Address) string) (address.Address, error) {
	if addr, err := address.Parse(input); err == nil {
		for _, k := range known {
			if k.String() == addr.String() {
				return addr, nil
			}
		}
	}

	var matches []address.Address
	for _, k := range known {
		if typeOf(k)+"."+k.String() == input {
			matches = append(matches, k)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return address.Address{}, fmt.Errorf("no managed resource matches %q", input)
	default:
		var names []string
		for _, m := range matches {
			names = append(names, m.String())
		}
		return address.Address{}, fmt.Errorf("%q is ambiguous; it matches %s", input, strings.Join(names, ", "))
	}
}
```

Add this helper to `internal/cli/context.go`, whose import block becomes:

```go
import (
	"path/filepath"
	"sort"

	"infra/internal/registry"
	"infra/internal/state"
	"infra/pkg/value"
	"infra/providers/test"
)
```

```go
func sortedAttributeKeys(attrs map[string]value.Value) []string {
	names := make([]string, 0, len(attrs))
	for name := range attrs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/cli/ -v`
Expected: PASS — eight tests.

- [ ] **Step 7: Run the whole suite**

Run: `make check`
Expected: everything green, no formatting or vet findings.

- [ ] **Step 8: Commit**

```bash
git add internal/cli
git commit -m "feat: implement validate and state commands"
```

---

## Task 15: M1 integration tests

CLI-level tests that drive the built binary the way a person does. These are the tests that
would catch a wiring mistake no unit test sees.

**Files:**
- Create: `tests/integration/helpers_test.go`, `tests/integration/m1_test.go`

**Interfaces:**
- Consumes: the `infra` binary, built by the test itself.
- Produces: nothing importable.

- [ ] **Step 1: Write the test harness**

Create `tests/integration/helpers_test.go`:

```go
package integration

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var (
	buildOnce sync.Once
	binPath   string
	buildErr  error
)

// binary builds the CLI once per test run and returns its path.
func binary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "infra-bin-*")
		if err != nil {
			buildErr = err
			return
		}
		binPath = filepath.Join(dir, "infra")
		cmd := exec.Command("go", "build", "-o", binPath, "infra/cmd/infra")
		cmd.Dir = repoRoot(t)
		out, err := cmd.CombinedOutput()
		if err != nil {
			buildErr = err
			binPath = string(out)
		}
	})
	if buildErr != nil {
		t.Fatalf("building the CLI: %v\n%s", buildErr, binPath)
	}
	return binPath
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	return filepath.Dir(filepath.Dir(wd)) // tests/integration → repo root
}

type result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

func (r result) combined() string { return r.Stdout + r.Stderr }

// run executes the CLI inside dir.
func run(t *testing.T, dir string, args ...string) result {
	t.Helper()
	cmd := exec.Command(binary(t), append([]string{"--chdir", dir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("running infra %v: %v", args, err)
		}
		code = exitErr.ExitCode()
	}
	return result{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: code}
}

// project writes a project directory containing infra.yml.
func project(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "infra.yml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write infra.yml: %v", err)
	}
	return dir
}

func requireContains(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("output does not contain %q\n---\n%s", needle, haystack)
	}
}
```

- [ ] **Step 2: Write the M1 behaviour tests**

Create `tests/integration/m1_test.go`:

```go
package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const goodProject = `
project: myapp

resources:
  network:
    type: test.network
    cidr: 10.20.0.0/16

  database:
    type: test.database
    engine: postgres
    size: 50
`

func TestValidateAcceptsGoodProject(t *testing.T) {
	dir := project(t, goodProject)
	res := run(t, dir, "validate")
	if res.ExitCode != 0 {
		t.Fatalf("exit code %d, want 0\n%s", res.ExitCode, res.combined())
	}
	requireContains(t, res.Stdout, "Configuration valid")
}

func TestValidateRejectsUnknownTypeWithUsefulMessage(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  database:
    type: aws.rds
    engine: postgres
`)
	res := run(t, dir, "validate")
	if res.ExitCode == 0 {
		t.Fatal("expected a non-zero exit code for invalid configuration")
	}
	requireContains(t, res.Stderr, "aws.rds")
	requireContains(t, res.Stderr, "test.database")
	requireContains(t, res.Stderr, "Suggested action:")
}

func TestValidateFailsWithoutProjectFile(t *testing.T) {
	res := run(t, t.TempDir(), "validate")
	if res.ExitCode == 0 {
		t.Fatal("expected failure when infra.yml is absent")
	}
	requireContains(t, res.combined(), "infra.yml")
}

func TestStateListOnFreshProject(t *testing.T) {
	dir := project(t, goodProject)
	res := run(t, dir, "state", "list", "dev")
	if res.ExitCode != 0 {
		t.Fatalf("exit code %d\n%s", res.ExitCode, res.combined())
	}
	requireContains(t, res.Stdout, "No resources are managed")
}

// writeState seeds a state file directly, standing in for the apply that M3
// will provide.
func writeState(t *testing.T, dir, environment string) {
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
		"resources": map[string]any{
			"database": map[string]any{
				"Address":    map[string]any{"Name": "database"},
				"Type":       "test.database",
				"Provider":   "test",
				"ProviderID": "db-1",
				"Attributes": map[string]any{
					"engine":   map[string]any{"kind": "string", "known": true, "raw": "postgres", "source": "provider"},
					"password": map[string]any{"kind": "string", "known": true, "raw": "hunter2", "source": "provider", "sensitive": true},
				},
			},
		},
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, environment+".json"), data, 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}
}

func TestStateListAndShow(t *testing.T) {
	dir := project(t, goodProject)
	writeState(t, dir, "dev")

	list := run(t, dir, "state", "list", "dev")
	requireContains(t, list.Stdout, "test.database.database")

	show := run(t, dir, "state", "show", "dev", "database")
	if show.ExitCode != 0 {
		t.Fatalf("state show exit %d\n%s", show.ExitCode, show.combined())
	}
	requireContains(t, show.Stdout, "db-1")
	requireContains(t, show.Stdout, "postgres")
}

func TestStateShowRedactsSensitiveValues(t *testing.T) {
	dir := project(t, goodProject)
	writeState(t, dir, "dev")

	show := run(t, dir, "state", "show", "dev", "database")
	requireContains(t, show.Stdout, "<sensitive>")
	if strings.Contains(show.combined(), "hunter2") {
		t.Error("a sensitive value was printed in clear text")
	}
}

func TestStateUnlockReportsHolderAndReleases(t *testing.T) {
	dir := project(t, goodProject)
	stateDir := filepath.Join(dir, ".infra", "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	lock := `{"environment":"dev","pid":4242,"host":"buildbox","user":"someone","operation":"apply","at":"2026-09-09T10:00:00Z"}`
	if err := os.WriteFile(filepath.Join(stateDir, "dev.lock"), []byte(lock), 0o600); err != nil {
		t.Fatalf("write lock: %v", err)
	}

	res := run(t, dir, "state", "unlock", "dev")
	if res.ExitCode != 0 {
		t.Fatalf("unlock exit %d\n%s", res.ExitCode, res.combined())
	}
	requireContains(t, res.Stdout, "buildbox")
	requireContains(t, res.Stdout, "4242")

	if _, err := os.Stat(filepath.Join(stateDir, "dev.lock")); !os.IsNotExist(err) {
		t.Error("the lock file still exists after unlock")
	}
}

func TestStateUnlockOnUnlockedEnvironment(t *testing.T) {
	dir := project(t, goodProject)
	res := run(t, dir, "state", "unlock", "dev")
	if res.ExitCode == 0 {
		t.Error("unlocking an environment that is not locked must fail clearly")
	}
	requireContains(t, res.combined(), "not locked")
}
```

- [ ] **Step 3: Run the integration tests**

Run: `go test ./tests/integration/ -v`
Expected: PASS — nine tests. A failure here after green unit tests means a wiring problem
between packages, which is exactly what these tests exist to find.

- [ ] **Step 4: Run everything**

Run: `make check`
Expected: all packages green, no vet findings, no formatting differences.

- [ ] **Step 5: Commit**

```bash
git add tests/integration
git commit -m "test: add M1 integration tests driving the CLI"
```

- [ ] **Step 6: Tag the milestone**

```bash
git tag -a m1 -m "M1: foundation — types, schemas, fake provider, state, locking, validate"
```

---

## M1 Definition of Done

Every box below must be true before M2 begins.

- [ ] `make check` is green from a clean checkout.
- [ ] `go list -deps ./providers/... | grep '^infra/internal'` prints nothing.
- [ ] `infra validate` accepts a well-formed project and reports `✓ Configuration valid`.
- [ ] `infra validate` on a project with three unknown resource types reports all three, each naming the offending type and suggesting the known ones, and exits non-zero.
- [ ] `infra state list <env>` and `infra state show <env> <address>` work against a seeded state file, and sensitive attributes render as `<sensitive>`.
- [ ] `infra state unlock <env>` names the lock holder before releasing, and fails clearly when nothing is locked.
- [ ] Two `Lock` calls on the same environment conflict; two on different environments do not.
- [ ] Sixteen goroutines racing to `Lock` one environment elect exactly one winner under `go test -race` — the only test in M1 that distinguishes `O_EXCL` from a stat-then-create implementation, and so the only direct proof of invariant 5.
- [ ] A state file written by `Put` has mode `0600`, no temporary files are left behind, and repeated `Encode` calls are byte-identical.
- [ ] Editing `.infra/fake-cloud.json` by hand changes what the fake provider's `Read` returns.

## What M1 deliberately leaves undone

Named here so a reviewer does not read them as gaps:

- `Value` has no `Expr` field yet; expressions arrive in M2.
- `validate` does not check attribute kinds, apply defaults, resolve references, or verify requirements. Those are compiler stage 7 and 8, in M2.
- `variables.yml` and `environments/*.yml` are not read. M4.
- `Discover` and `Import` return errors that say Phase 2.
- No `plan`, `apply`, `destroy`, or `refresh` command exists. M2 and M3.
