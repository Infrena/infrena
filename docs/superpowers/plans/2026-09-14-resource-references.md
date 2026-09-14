# Resource References Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a plugin declare what an attribute refers to, so `vpc_id: ${vpc}` projects to the attribute the plugin named, and passing the wrong resource type fails at compile time.

**Architecture:** `schema.Attribute` gains `References *Reference{Type, Attribute}` — declarative data the plugin owns and the engine only reads. `${vpc}` parses to a resource reference with an EMPTY attribute, and compiler stage 6 fills it in from the consuming attribute's declaration, as a sibling of the existing `canonicaliseRefs` rewrite. Everything downstream — planner, executor, wire format, `ResolveDeferred` — sees an ordinary two-part reference and needs no change at all.

**Tech Stack:** Go 1.27, `gopkg.in/yaml.v3`, Cobra. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-09-14-resource-references-design.md`

## Global Constraints

- **Go 1.27**, pinned by `mise.toml`. `mise` is NOT active in non-interactive shells: run `export PATH="$HOME/.local/share/mise/shims:$PATH"` first, or a bare `go` resolves to 1.20 and fails on `go.mod`.
- **Verify with `go test ./... -count=1`, NEVER plain `go test ./...`.** `tests/integration` builds its binary out-of-band at runtime, so Go's cache does not see the dependency and reuses stale results. This has already cost this project a batch of false confidence.
- **Add NO new third-party dependency.** The entire budget is Cobra and `gopkg.in/yaml.v3`.
- **Nothing in `pkg/schema` may gain a function-typed field** (§31.1). `References` is a type name and an attribute name, never a resolver. A function cannot cross a pipe.
- **THE PLUGIN IS THE DECIDER.** The engine reads `References` and never infers, defaults, or guesses which attribute is meant. There is no fallback to "probably the id" anywhere in this plan.
- **Every diagnostic** carries `Summary`, `Detail`, `Action`, `Origin` and names the fix, not the symptom.
- **Diagnostics are order-stable** (invariant 6): never build diagnostic text by ranging a Go map.
- **Do NOT modify anything under `docs/superpowers/plans/`, `docs/superpowers/specs/`, or `.superpowers/`.** They are historical records. The migration in the previous branch corrupted a spec because its exclusion list omitted `specs/`; do not repeat it.

---

### Task 1: `schema.Reference` and `Attribute.References`

The declaration, plus the validation that makes a broken one a load failure rather than a runtime surprise.

**Files:**
- Modify: `pkg/schema/attribute.go`
- Modify: `pkg/schema/definition.go` (`Validate`)
- Test: `pkg/schema/definition_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `schema.Reference{Type string; Attribute string}`, `Attribute.References *Reference`, and `ResourceDefinition.Validate` refusing a dangling reference.

- [ ] **Step 1: Write the failing test**

Add to `pkg/schema/definition_test.go`:

```go
func TestAReferenceToAnUndeclaredAttributeRefusesTheDefinition(t *testing.T) {
	// A plugin whose relationship names an attribute the target does not have
	// is a plugin that will not load, on §14.1's precedent that a name
	// collision should fail at load rather than surprise someone at apply.
	vpc := &ResourceDefinition{
		Type: "test.vpc",
		Attributes: map[string]Attribute{
			"id": {Kind: value.KindString, Computed: true},
		},
	}
	subnet := &ResourceDefinition{
		Type: "test.subnet",
		Attributes: map[string]Attribute{
			"vpc_id": {Kind: value.KindString, Required: true,
				References: &Reference{Type: "test.vpc", Attribute: "arn"}},
		},
	}
	err := ValidateAll([]*ResourceDefinition{vpc, subnet})
	if err == nil {
		t.Fatal("a reference to an attribute the target does not declare must refuse the definition")
	}
	if !strings.Contains(err.Error(), "arn") || !strings.Contains(err.Error(), "test.vpc") {
		t.Errorf("error = %q, want it to name both the attribute and the target type", err)
	}
}

func TestAReferenceToAnUndeclaredTypeRefusesTheDefinition(t *testing.T) {
	subnet := &ResourceDefinition{
		Type: "test.subnet",
		Attributes: map[string]Attribute{
			"vpc_id": {Kind: value.KindString, Required: true,
				References: &Reference{Type: "test.nosuch", Attribute: "id"}},
		},
	}
	if err := ValidateAll([]*ResourceDefinition{subnet}); err == nil {
		t.Fatal("a reference to a type the plugin does not declare must refuse the definition")
	}
}

func TestAWellFormedReferenceLoads(t *testing.T) {
	vpc := &ResourceDefinition{
		Type: "test.vpc",
		Attributes: map[string]Attribute{"id": {Kind: value.KindString, Computed: true}},
	}
	subnet := &ResourceDefinition{
		Type: "test.subnet",
		Attributes: map[string]Attribute{
			"vpc_id": {Kind: value.KindString, Required: true,
				References: &Reference{Type: "test.vpc", Attribute: "id"}},
		},
	}
	if err := ValidateAll([]*ResourceDefinition{vpc, subnet}); err != nil {
		t.Fatalf("a well-formed reference must load: %v", err)
	}
}
```

Add `"strings"` to the imports if absent.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./pkg/schema/ -run 'TestAReference|TestAWellFormed' -v`
Expected: FAIL to compile — `undefined: Reference`, `unknown field References`, `undefined: ValidateAll`.

- [ ] **Step 3: Write the implementation**

In `pkg/schema/attribute.go`, add above `type Attribute struct`:

```go
// Reference names what an attribute refers to, when it holds another resource's
// identifier rather than a value of its own.
//
// It is DATA, and that is the whole design. §31.1 forbids a function-typed field
// in this package because a function cannot cross a pipe, and a plugin that
// resolved its own references would be a SECOND RESOLVER — free to disagree with
// the engine's about what a reference means, which is the failure
// expressions.ResourceScope exists to prevent.
//
// THE PLUGIN DECIDES. The engine cannot know that Cloud Control's
// AWS::EC2::Subnet.VpcId wants a VPC's id rather than its arn; that is knowledge
// about an API, and it belongs to whoever owns the API. The engine reads this and
// never infers, defaults, or guesses.
type Reference struct {
	// Type is the resource type referred to, in the plugin's own naming —
	// "aws.ec2.vpc", not "AWS::EC2::VPC".
	Type string
	// Attribute is which of that type's attributes this one holds. It is the
	// CANONICAL name, never an alias: §14.1 makes the canonical name the
	// identity, and an alias here would have to be folded at every read.
	Attribute string
}
```

In `Attribute`, below `Aliases`:

```go
	// References declares that this attribute holds another resource's
	// identifier, which is what lets configuration pass the resource whole —
	// `vpc_id: ${vpc}` — instead of naming the attribute.
	//
	// Nil means "not a reference", and that is the honest default: most
	// attributes are not references, and an empty Reference{} would be
	// indistinguishable from an author who meant to fill it in.
	References *Reference
```

In `pkg/schema/definition.go`, add:

```go
// ValidateAll validates each definition on its own and then the relationships
// BETWEEN them, which no single definition can check: a Reference names another
// type, and whether that type exists is a fact about the whole set.
//
// A plugin with a dangling relationship does not load. §14.1 took the same line
// for colliding alias spellings, for the same reason — a silent runtime surprise
// about which relationship won is worse than a plugin that refuses to start.
func ValidateAll(defs []*ResourceDefinition) error {
	byType := make(map[string]*ResourceDefinition, len(defs))
	for _, d := range defs {
		if err := d.Validate(); err != nil {
			return err
		}
		byType[d.Type] = d
	}

	// Sorted, so a plugin with two broken relationships reports the same one
	// first on every run (invariant 6).
	types := make([]string, 0, len(byType))
	for t := range byType {
		types = append(types, t)
	}
	sort.Strings(types)

	for _, t := range types {
		d := byType[t]
		names := make([]string, 0, len(d.Attributes))
		for n := range d.Attributes {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			ref := d.Attributes[n].References
			if ref == nil {
				continue
			}
			target, ok := byType[ref.Type]
			if !ok {
				return fmt.Errorf("%s: attribute %q refers to type %q, which this plugin does not declare",
					d.Type, n, ref.Type)
			}
			if _, ok := target.Attributes[ref.Attribute]; !ok {
				return fmt.Errorf("%s: attribute %q refers to %s.%s, and %s has no attribute %q",
					d.Type, n, ref.Type, ref.Attribute, ref.Type, ref.Attribute)
			}
		}
	}
	return nil
}
```

Add `"sort"` to that file's imports if absent.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./pkg/schema/ -v` then `go test ./... -count=1`
Expected: PASS. `References` is nil everywhere, so nothing existing changes.

- [ ] **Step 5: Commit**

```bash
git add pkg/schema/
git commit -m "schema: an attribute may declare what it refers to

Data, not behaviour: a plugin that resolved its own references would be a
second resolver free to disagree with the engine's. A dangling relationship
refuses the definition rather than surprising someone at apply."
```

---

### Task 2: protocol 3

**Files:**
- Modify: `pkg/pluginproto/proto.go`
- Test: `pkg/pluginproto/proto_test.go`

**Interfaces:**
- Consumes: Task 1's `References`.
- Produces: `pluginproto.Version == 3`, `Supported == []int{3, 2, 1}`.

- [ ] **Step 1: Write the failing test**

Add to `pkg/pluginproto/proto_test.go`:

```go
func TestProtocolIsThreeAndStillSpeaksTwoAndOne(t *testing.T) {
	if Version != 3 {
		t.Errorf("Version = %d, want 3 — References is a schema-payload addition, same as optional/aliases were for 2", Version)
	}
	for _, v := range []int{3, 2, 1} {
		if !IsSupported(v) {
			t.Errorf("protocol %d must still be supported — Supported is a set so raising the version does not orphan every plugin", v)
		}
	}
	if IsSupported(4) {
		t.Error("an unreleased protocol must not be accepted")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./pkg/pluginproto/ -run TestProtocolIsThree -v`
Expected: FAIL — `Version = 2, want 3`.

- [ ] **Step 3: Write the implementation**

In `pkg/pluginproto/proto.go`, replace the `Version` block's trailing comment and value, keeping the existing v2 paragraph above it, and append:

```go
// RAISED TO 3 on 2026-09-14, for `References` on an attribute (PLAN.md §14.3). The
// messages did not change shape; the SCHEMA PAYLOAD gained one key, and the reason is
// exactly the reason 2 was raised: Attribute.UnmarshalJSON decodes leniently, so a
// plugin built with this SDK talking to an OLDER host would have References silently
// DROPPED. `${vpc}` would then report "declares no reference" about an attribute whose
// plugin plainly declares one, with nothing anywhere explaining why it was not heard.
//
// Announcing 3 makes that a refusal that names the plugin and the versions instead.
const Version = 3

// Supported lists every protocol version this build can talk to, newest first.
var Supported = []int{3, 2, 1}
```

Delete the old `const Version = 2` and the old `Supported` literal.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./pkg/pluginproto/ ./internal/pluginhost/ -count=1 -v` then `go test ./... -count=1`
Expected: PASS. Watch `internal/pluginhost` specifically — it holds the handshake tests.

- [ ] **Step 5: Commit**

```bash
git add pkg/pluginproto/
git commit -m "pluginproto: raise the protocol to 3 for an attribute's References

Same reason as 2: lenient decoding means an older host drops the key
silently, and the user meets a diagnostic that contradicts their plugin."
```

---

### Task 3: `${vpc}` parses as a whole-resource reference

Spec one made a bare single segment an error. It becomes a reference with an EMPTY attribute.

**Files:**
- Modify: `internal/expressions/parse.go` (the bare-single-segment branch added by the previous branch)
- Test: `internal/expressions/parse_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `${vpc}` parses to `OpResourceRef` with `Ref.Target.Name == "vpc"` and `Ref.Attribute == ""`.

- [ ] **Step 1: Write the failing test**

In `internal/expressions/parse_test.go`, DELETE `TestABareSingleSegmentIsNotAReference` and add:

```go
func TestABareSingleSegmentIsAWholeResourceReference(t *testing.T) {
	e, ds := Parse("${vpc}", value.Origin{})
	if ds.HasErrors() {
		t.Fatalf("unexpected errors: %v", ds)
	}
	if e.Op != value.OpResourceRef {
		t.Fatalf("Op = %v, want OpResourceRef", e.Op)
	}
	if e.Ref.Target.Name != "vpc" {
		t.Errorf("Target.Name = %q, want %q", e.Ref.Target.Name, "vpc")
	}
	if e.Ref.Attribute != "" {
		t.Errorf("Attribute = %q, want empty — the attribute is filled in at stage 6 from the consuming attribute's declaration", e.Ref.Attribute)
	}
}

func TestAWholeResourceReferenceRendersAsWritten(t *testing.T) {
	e, _ := Parse("${vpc}", value.Origin{})
	if got := e.String(); got != "${vpc}" {
		t.Errorf("String() = %q, want %q — a diagnostic must echo what the user wrote", got, "${vpc}")
	}
}

func TestVarIsStillNotAWholeResourceReference(t *testing.T) {
	// `var` is reserved as a resource name, so ${var} must keep its own
	// diagnostic rather than becoming a reference to a resource called var.
	_, ds := Parse("${var}", value.Origin{})
	if !ds.HasErrors() {
		t.Fatal("${var} must still be refused: var is the variable namespace, not a resource")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/expressions/ -run 'TestABareSingleSegmentIsAWhole|TestAWholeResource|TestVarIsStill' -v`
Expected: FAIL — `${vpc}` currently produces "is not a reference".

- [ ] **Step 3: Write the implementation**

In `internal/expressions/parse.go`, replace the `if len(segments) == 1 { ... "is not a reference" ... }` block with:

```go
	if len(segments) == 1 {
		// A WHOLE-RESOURCE reference: the attribute is not written, and stage 6
		// fills it in from the consuming attribute's own `References`
		// declaration (PLAN.md §14.3).
		//
		// An empty Attribute must never escape stage 6. Downstream,
		// expressions.ResourceScope looks an attribute up by name in a plain
		// map, so "" would miss, report unavailable, and leave the value
		// deferred forever — the resource would be created with the attribute
		// silently unset. Stage 6 therefore either fills it or reports an
		// error; internal/compiler has the test that pins it.
		return &value.Expr{
			Op:     value.OpResourceRef,
			Ref:    value.LocalRef(segments[0], ""),
			Origin: origin,
		}
	}
```

The `var` guard added in the previous branch already runs before this and is unchanged, so `${var}` keeps its own diagnostic.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/expressions/ -v` then `go test ./... -count=1`
Expected: `internal/expressions` PASSES. **`internal/compiler` and `tests/integration` may now FAIL** where a fixture used `${vpc}` expecting a parse error — that is correct and Task 4 resolves it. Record which tests fail; do not fix them here.

- [ ] **Step 5: Commit**

```bash
git add internal/expressions/
git commit -m "expressions: a bare name is a reference to the resource itself

The attribute is filled in at stage 6 from what the consuming attribute
declares. An empty attribute must never escape that stage."
```

---

### Task 4: project at stage 6

**Files:**
- Modify: `internal/compiler/bind.go`
- Modify: `providers/test/definitions.go` (a test type declaring a reference)
- Test: `internal/compiler/bind_test.go`

**Interfaces:**
- Consumes: Task 1's `Attribute.References`, Task 3's empty-attribute reference.
- Produces: `projectRefs(e *value.Expr, consuming *schema.Attribute, declared map[string]refTarget, origin value.Origin, ds *diag.Diagnostics)`, run before `canonicaliseRefs`.

- [ ] **Step 1: Write the failing test**

Add to `internal/compiler/bind_test.go`:

```go
func TestAWholeResourceReferenceIsProjectedToTheDeclaredAttribute(t *testing.T) {
	// test.subnet's vpc_id declares References{test.vpc, "id"}, so ${net}
	// must become ${net.id} before anything downstream sees it.
	cfg := compileForTest(t, `
resources:
  net:
    type: test.vpc
  sub:
    type: test.subnet
    vpc_id: ${net}
`)
	got := refIn(t, cfg, "sub", "vpc_id")
	if got.Attribute != "id" {
		t.Errorf("Attribute = %q, want %q — the projection reads the consuming attribute's declaration", got.Attribute, "id")
	}
	if got.Target.Name != "net" {
		t.Errorf("Target.Name = %q, want %q", got.Target.Name, "net")
	}
}

func TestAWholeResourceReferenceWithNoDeclarationIsAnError(t *testing.T) {
	// test.subnet's `cidr` declares no References, so ${net} there cannot be
	// projected. The engine must NOT guess.
	ds := compileErrorsForTest(t, `
resources:
  net:
    type: test.vpc
  sub:
    type: test.subnet
    cidr: ${net}
`)
	if !ds.HasErrors() {
		t.Fatal("passing a resource to an attribute that declares no reference must be an error, never a guess")
	}
	if !strings.Contains(ds[0].Detail, "${net.") {
		t.Errorf("Detail = %q, want it to show naming an attribute explicitly as the fix", ds[0].Detail)
	}
}

func TestNoEmptyAttributeReferenceEscapesStageSix(t *testing.T) {
	// The invariant Task 3's parser comment relies on. An empty attribute
	// downstream means ResourceScope misses, the value stays deferred forever,
	// and the resource is created with the attribute silently unset.
	cfg := compileForTest(t, `
resources:
  net:
    type: test.vpc
  sub:
    type: test.subnet
    vpc_id: ${net}
`)
	for _, rc := range cfg.Resources {
		for name, v := range rc.Attrs {
			for _, ref := range v.Expr.References() {
				if ref.Attribute == "" {
					t.Errorf("%s.%s kept an empty-attribute reference past stage 6", rc.Address, name)
				}
			}
		}
	}
}
```

Follow `bind_test.go`'s existing helpers for `compileForTest`, `compileErrorsForTest` and `refIn` — read the file first and use whatever it already calls them; the names above are placeholders for that file's own idiom, and the assertions are what matter.

- [ ] **Step 2: Add the test type and run the test to verify it fails**

In `providers/test/definitions.go`, give `test.subnet` a `vpc_id` attribute declaring `References: &schema.Reference{Type: "test.vpc", Attribute: "id"}`, and leave its `cidr` attribute without one. Add `test.vpc` with an `id` attribute if it does not exist.

Run: `go test ./internal/compiler/ -run 'TestAWholeResource|TestNoEmptyAttribute' -v`
Expected: FAIL — `${net}` binds with an empty attribute and reports "no such attribute".

- [ ] **Step 3: Write the implementation**

In `internal/compiler/bind.go`, add beside `canonicaliseRefs`:

```go
// projectRefs fills in the attribute of every WHOLE-RESOURCE reference, from
// the declaration on the attribute that consumes it (PLAN.md §14.3).
//
// `vpc_id: ${vpc}` becomes `${vpc.id}` here and nowhere else, which is what lets
// the planner, the executor, the plan artifact and the wire format stay exactly
// as they are: downstream sees an ordinary two-part reference and cannot tell
// the difference.
//
// IN PLACE on the AST, and BEFORE canonicaliseRefs, for the same reason that one
// runs before the attribute-axis check: a projected name must then be
// canonicalised like any other, in case a plugin declares its reference against
// an attribute spelling that is itself an alias of another.
//
// THE ENGINE NEVER GUESSES. An attribute with no declaration is an error naming
// the fix, not a fallback to "probably the id" — a wrong value shipped silently
// is the failure this whole feature exists to prevent.
func projectRefs(
	e *value.Expr,
	consuming *schema.Attribute,
	consumingName string,
	declared map[string]refTarget,
	origin value.Origin,
	ds *diag.Diagnostics,
) {
	if e == nil {
		return
	}
	if e.Op == value.OpResourceRef && e.Ref.Attribute == "" {
		switch {
		case consuming == nil || consuming.References == nil:
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "${" + e.Ref.Target.String() + "} passes a resource to an attribute that declares no reference",
				Detail: "`" + consumingName + "` does not say which of " +
					strconv.Quote(e.Ref.Target.String()) + "'s attributes it holds, so there is " +
					"nothing to pick. The provider declares that, not infrena.",
				Action: "Name the attribute you mean, as ${" + e.Ref.Target.String() + ".<attribute>}.",
				Origin: origin,
			})
		default:
			e.Ref.Attribute = consuming.References.Attribute
		}
	}
	for _, arg := range e.Args {
		projectRefs(arg, consuming, consumingName, declared, origin, ds)
	}
}
```

Add `"strconv"` and the `pkg/schema` import if absent.

In `bindAttribute`, call it immediately before the existing `canonicaliseRefs(e, declared)`, passing the consuming attribute's schema. The consuming resource's own definition is reachable as `declared[inst.Address.String()].def`; look up `attr.Name` on it, tolerating a nil def (an unregistered type already skips the attribute axis, and must skip this too rather than reporting a second diagnostic about the same unknown type).

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/compiler/ -v` then `go test ./... -count=1`
Expected: PASS, including the tests Task 3 left failing.

- [ ] **Step 5: Commit**

```bash
git add internal/compiler/ providers/test/
git commit -m "compiler: fill in a whole-resource reference from what the attribute declares

${vpc} becomes ${vpc.id} at stage 6 and nowhere else, so the planner, the
executor and the wire format cannot tell the difference. An attribute with
no declaration is an error: the engine never guesses."
```

---

### Task 5: the type check

**Files:**
- Modify: `internal/compiler/bind.go`
- Test: `internal/compiler/bind_test.go`

**Interfaces:**
- Consumes: Tasks 1 and 4.
- Produces: a reference whose target's type differs from `References.Type` is a compile error, for BOTH `${vpc}` and `${vpc.id}`.

- [ ] **Step 1: Write the failing test**

Add to `internal/compiler/bind_test.go`:

```go
func TestPassingTheWrongResourceTypeIsACompileError(t *testing.T) {
	ds := compileErrorsForTest(t, `
resources:
  db:
    type: test.database
  sub:
    type: test.subnet
    vpc_id: ${db}
`)
	if !ds.HasErrors() {
		t.Fatal("vpc_id refers to test.vpc; passing a test.database must fail at compile time, not at the API")
	}
	if !strings.Contains(ds[0].Summary, "test.vpc") || !strings.Contains(ds[0].Summary, "test.database") {
		t.Errorf("Summary = %q, want both type names", ds[0].Summary)
	}
}

func TestNamingAnAttributeDoesNotEscapeTheTypeCheck(t *testing.T) {
	// The projection is sugar; the type check is not. Writing the attribute
	// out avoids the projection and must NOT avoid the check.
	ds := compileErrorsForTest(t, `
resources:
  db:
    type: test.database
  sub:
    type: test.subnet
    vpc_id: ${db.id}
`)
	if !ds.HasErrors() {
		t.Fatal("${db.id} into an attribute that refers to test.vpc must fail too")
	}
}

func TestAnAttributeWithNoDeclarationIsNotTypeChecked(t *testing.T) {
	// Coverage buys checking; absence costs nothing. `cidr` declares no
	// reference, so anything may be interpolated into it, exactly as today.
	cfg := compileForTest(t, `
resources:
  db:
    type: test.database
  sub:
    type: test.subnet
    cidr: ${db.id}
`)
	if cfg == nil {
		t.Fatal("an attribute with no declared reference must not be type checked")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/compiler/ -run 'TestPassingTheWrong|TestNamingAnAttribute|TestAnAttributeWithNo' -v`
Expected: FAIL — nothing checks the referred type yet.

- [ ] **Step 3: Write the implementation**

In `internal/compiler/bind.go`, after projection and canonicalisation, for each resource reference whose consuming attribute declares a `References`:

```go
// checkReferredType reports a reference into a resource of the wrong type.
//
// It runs for a reference the user wrote in FULL as well as for a projected
// one. Naming the attribute explicitly escapes the projection — that is what
// the projection is sugar for — but it must not escape the check: reaching into
// the wrong resource is the same mistake whichever spelling it wears.
//
// It runs ONLY when the consuming attribute declares a reference. An attribute
// with no declaration is checked exactly as much as it is today, which is not at
// all — coverage buys checking, and absence costs nothing.
func checkReferredType(
	ref value.Reference,
	consuming *schema.Attribute,
	consumingName string,
	declared map[string]refTarget,
	origin value.Origin,
	ds *diag.Diagnostics,
) {
	if consuming == nil || consuming.References == nil {
		return
	}
	target, known := declared[ref.Target.String()]
	if !known || target.typeName == "" {
		// An undeclared target already has its own diagnostic; do not tell the
		// same reader about a type mismatch with a resource that does not exist.
		return
	}
	if target.typeName == consuming.References.Type {
		return
	}
	ds.Add(diag.Diagnostic{
		Severity: diag.SeverityError,
		Summary: consumingName + " refers to " + consuming.References.Type +
			", and " + strconv.Quote(ref.Target.String()) + " is " + target.typeName,
		Detail: "${" + ref.String() + "} reaches into a resource of the wrong type.",
		Action: "Pass a " + consuming.References.Type +
			", or name the attribute you mean on a resource of that type.",
		Origin: origin,
	})
}
```

Wire it into `bindAttribute`'s existing per-reference walk, alongside the attribute-axis check.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/compiler/ -v` then `go test ./... -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/compiler/
git commit -m "compiler: refuse a reference into a resource of the wrong type

Catches at compile time what an API would otherwise reject halfway through
an apply. Writing the attribute out avoids the projection, not the check."
```

---

### Task 6: nested attribute shape

Closes the unchecked-key gap the previous branch deferred here, because it shares this branch's protocol bump.

**Files:**
- Modify: `pkg/schema/attribute.go`
- Modify: `internal/compiler/bind.go`
- Modify: `providers/test/definitions.go`
- Test: `internal/compiler/bind_test.go`

**Interfaces:**
- Consumes: Task 2's protocol 3.
- Produces: `Attribute.Fields map[string]Attribute`; a path into an attribute declaring `Fields` is key-checked at compile time.

- [ ] **Step 1: Write the failing test**

Add to `internal/compiler/bind_test.go`:

```go
func TestAPathIntoADeclaredMapIsKeyCheckedAtCompileTime(t *testing.T) {
	ds := compileErrorsForTest(t, `
resources:
  net:
    type: test.vpc
  sub:
    type: test.subnet
    cidr: ${net.meta.nmae}
`)
	if !ds.HasErrors() {
		t.Fatal("a typo in a declared map key must fail at compile time, not halfway through apply")
	}
	if !strings.Contains(ds[0].Detail, "name") {
		t.Errorf("Detail = %q, want it to list the keys that exist", ds[0].Detail)
	}
}

func TestAPathIntoAnOpenMapIsStillUnchecked(t *testing.T) {
	// AWS tags are an open map and always will be. Declaring Fields for them
	// would be a lie, so nil must stay a first-class answer rather than a gap.
	cfg := compileForTest(t, `
resources:
  net:
    type: test.vpc
  sub:
    type: test.subnet
    cidr: ${net.tags.anything}
`)
	if cfg == nil {
		t.Fatal("an attribute with no declared Fields must accept any key, as today")
	}
}
```

- [ ] **Step 2: Add the test shape and run the test to verify it fails**

In `providers/test/definitions.go`, give `test.vpc` a `meta` attribute of `value.KindMap` declaring `Fields: map[string]schema.Attribute{"name": {Kind: value.KindString}}`, and a `tags` attribute of `value.KindMap` with no `Fields`.

Run: `go test ./internal/compiler/ -run 'TestAPathIntoADeclared|TestAPathIntoAnOpen' -v`
Expected: FAIL to compile — `unknown field Fields`.

- [ ] **Step 3: Write the implementation**

In `pkg/schema/attribute.go`, below `References`:

```go
	// Fields describes a KindMap attribute's known keys, where the provider
	// knows them.
	//
	// NIL MEANS OPEN, and that is a first-class answer rather than a gap: AWS
	// tags take any key and always will, so declaring Fields for them would be
	// a lie. A path into an open map is checked at apply, exactly as it is
	// today.
	//
	// Where it IS declared, a typo becomes a compile error listing the keys
	// that exist — which is what internal/compiler/bind.go already does for a
	// top-level attribute name, and what its comment says is worth doing: "a
	// typo here passed `validate`, produced a clean plan, and failed halfway
	// through `apply` after real infrastructure existed".
	Fields map[string]Attribute
```

In `bind.go`'s per-reference walk, when the target attribute declares `Fields`, walk `ref.Path`'s key steps against them and report an unknown key with the known keys listed, SORTED (invariant 6). An index step into a declared map, or any step once `Fields` is nil, stops the walk without complaint.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/compiler/ -v` then `go test ./... -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/schema/ internal/compiler/ providers/test/
git commit -m "schema: a map attribute may declare its keys

A typo in a declared key is now a compile error rather than a clean plan
that fails partway through apply. Nil stays open, because tags are."
```

---

### Task 7: documentation

**Files:**
- Modify: `PLAN.md` (new §14.3), `CLAUDE.md`
- Modify: `internal/cli/explain.go`
- Test: `internal/cli/explain_test.go`

- [ ] **Step 1: Write `PLAN.md` §14.3**

Add `## 14.3 Provider-declared resource references` after §14.2, drawing on the spec's §1, §2, §2.1, §2.3 and §5 in PLAN.md's voice — explaining WHY each rule exists and naming the hazard it prevents. It must state: the plugin decides and the engine never guesses; `${vpc}` is sugar over `${vpc.arn}` and a missing declaration degrades to today rather than to a wrong value; the type check fires for both spellings; and how `References` relates to `Requirement` (attribute-level versus type-level, and that the two must be reconciled rather than left to drift).

- [ ] **Step 2: Update `CLAUDE.md`**

Add `${vpc}` to the grammar list in the current-state section, replacing the note that it is "reserved for a later change". State that the plugin declares the target and the engine reads it.

- [ ] **Step 3: Write the failing test for `explain`**

In `internal/cli/explain_test.go`, assert that `explain test.subnet` renders `vpc_id`'s declared reference — the target type and attribute — so a user can discover what a resource may be passed without reading the plugin's source. Follow the file's existing helper idiom.

- [ ] **Step 4: Render it in `explain`**

In `internal/cli/explain.go`, beside the existing `Requirements` rendering, show each attribute's `References` as `refers to <type>.<attribute>`.

- [ ] **Step 5: Verify and commit**

```bash
go test ./... -count=1
git add -A
git commit -m "docs: provider-declared resource references"
```

---

## Self-Review

**Spec coverage.** §2 projection → Tasks 3, 4. §2.1 consumer-declares → Task 1's type shape. §2.2 sugar and degradation → Task 4's no-declaration error and Task 5's `TestAnAttributeWithNoDeclarationIsNotTypeChecked`. §2.3 `Requirement` reconciliation → Task 7 (documented; the generation cross-check is plugin-side and out of this plan). §3 schema field and Validate → Task 1. §4 table generation → **plugin-side, not in this plan** — `infrena-provider-aws` owns it and has been briefed. §5 type check → Task 5. §6 nested shape → Task 6. §7 protocol 3 → Task 2. §8 diagnostics → Tasks 1, 4, 5, 6. §9 testing → each task's tests, plus Task 4's escape invariant.

**Type consistency.** `schema.Reference{Type, Attribute}` defined in Task 1, used in 4, 5, 6, 7. `projectRefs` defined in Task 4 and called only there. `checkReferredType` defined in Task 5. `Attribute.Fields` defined in Task 6.

**Known gap, deliberate.** Task 4's tests name helpers (`compileForTest`, `compileErrorsForTest`, `refIn`) that may not exist under those names in `bind_test.go`. The task says to read the file and use its own idiom; the assertions are what matter, not the helper names.

**Not covered here, by design.** The saved-plan round trip needs no new test: projection happens at stage 6, so the wire format only ever sees an ordinary two-part reference, which the previous branch already pinned across `MarshalJSON`. Task 4's escape invariant is what guarantees that stays true.
