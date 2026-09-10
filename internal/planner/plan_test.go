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
