package test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	c.Failures = []FailureRule{{Op: "create", Address: "db", Nth: 1, Retryability: RetrySafe, Message: "throttled"}}
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

// TestNthFailureRuleSurvivesAcrossOperations is the regression guard for the
// controller's ruling on Task 9: FailureRule's Seen/Fired bookkeeping must be
// exported and persisted, and begin() must save the cloud after every
// ShouldFail call, not only on failure. The provider reloads the cloud file on
// every operation — it must, to observe hand-edited drift — so if Seen is not
// written back after a non-firing check, it resets to zero on the next load
// and an Nth: 2 rule can never fire.
func TestNthFailureRuleSurvivesAcrossOperations(t *testing.T) {
	p, path := newTestProvider(t)
	c, err := LoadCloud(path)
	if err != nil {
		t.Fatalf("LoadCloud: %v", err)
	}
	c.Failures = []FailureRule{{Op: "create", Address: "db", Nth: 2, Message: "boom"}}
	if err := c.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	attrs := map[string]value.Value{
		"engine": value.String("postgres", value.SourceExplicit),
	}

	if _, err := p.Create(context.Background(), desired("db", "test.database", attrs)); err != nil {
		t.Fatalf("first Create must succeed (Nth: 2 has not fired yet), got: %v", err)
	}

	_, err = p.Create(context.Background(), desired("db", "test.database", attrs))
	if err == nil {
		t.Fatal("second Create must fail: the rule's Seen counter must have persisted across the first call")
	}
	if got := p.ClassifyError(err); got != provider.NotSafeToRetry {
		t.Errorf("ClassifyError = %v, want NotSafeToRetry (rule set no retryability)", got)
	}
}

func TestUpdatePreservesDependencies(t *testing.T) {
	// Spec §14 makes the Dependencies recorded in state the only source of
	// destroy-ordering edges for a resource that is no longer in configuration.
	// An executor that persists Update's return value — what spec §15's "state
	// is persisted after every operation" implies — would erase those edges for
	// every resource it ever updates, and the failure would surface much later
	// as a destroy in the wrong order.
	p, _ := newTestProvider(t)
	created, err := p.Create(context.Background(), desired("db", "test.database", map[string]value.Value{
		"engine": value.String("postgres", value.SourceExplicit),
	}))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	created.Dependencies = []address.Address{{Name: "net"}}
	created.CreatedAt = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	created.Lifecycle = resource.Lifecycle{PreventDestroy: true}

	updated, err := p.Update(context.Background(), created, desired("db", "test.database", map[string]value.Value{
		"engine": value.String("mysql", value.SourceExplicit),
	}))
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	if len(updated.Dependencies) != 1 || updated.Dependencies[0].String() != "net" {
		t.Errorf("Update returned Dependencies %v, want [net]", updated.Dependencies)
	}
	if !updated.CreatedAt.Equal(created.CreatedAt) {
		t.Errorf("Update returned CreatedAt %v, want %v", updated.CreatedAt, created.CreatedAt)
	}
}

func TestReadPreservesCarriedFields(t *testing.T) {
	p, _ := newTestProvider(t)
	created, err := p.Create(context.Background(), desired("db", "test.database", map[string]value.Value{
		"engine": value.String("postgres", value.SourceExplicit),
	}))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	created.Dependencies = []address.Address{{Name: "net"}}

	read, err := p.Read(context.Background(), created)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(read.Dependencies) != 1 || read.Dependencies[0].String() != "net" {
		t.Errorf("Read returned Dependencies %v, want [net]", read.Dependencies)
	}

	// The carried slice must be a copy: aliasing would let a refresh mutate the
	// state that was loaded from disk.
	read.Dependencies[0] = address.Address{Name: "other"}
	if created.Dependencies[0].Name != "net" {
		t.Error("Read aliased the caller's Dependencies slice")
	}
}

func TestCreateLeavesDependenciesNil(t *testing.T) {
	// Deliberate: on a create there is no prior state, and the executor knows
	// the edges from configuration. Asserted so the carry-forward helper is not
	// extended to Create by mistake.
	p, _ := newTestProvider(t)
	st, err := p.Create(context.Background(), desired("net", "test.network", map[string]value.Value{
		"cidr": value.String("10.0.0.0/16", value.SourceExplicit),
	}))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if st.Dependencies != nil {
		t.Errorf("Create set Dependencies to %v, want nil", st.Dependencies)
	}
}

// TestCompositeAttributesRoundTripAsPlainJSON asserts the cloud file holds
// plain JSON for a composite attribute.
//
// Writing v.Raw directly serialises []value.Value or map[string]value.Value
// through Value.MarshalJSON, so the file gets the engine's internal wire
// objects. That defeats spec §8.4's premise that a human can hand-edit fake
// infrastructure, and reading it back yields a Map whose every leaf is itself a
// four-key kind/known/raw/source Map — which presents in M3 as inexplicable
// permanent drift rather than an obvious serialisation bug.
func TestCompositeAttributesRoundTripAsPlainJSON(t *testing.T) {
	p, path := newTestProvider(t)
	tags := value.Map(map[string]value.Value{
		"env":   value.String("dev", value.SourceExplicit),
		"tier":  value.Int(2, value.SourceExplicit),
		"inner": value.List([]value.Value{value.String("a", value.SourceExplicit)}, value.SourceExplicit),
	}, value.SourceExplicit)

	st, err := p.Create(context.Background(), desired("db", "test.database", map[string]value.Value{
		"engine": value.String("postgres", value.SourceExplicit),
		"tags":   tags,
	}))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	requirePlainTags := func(t *testing.T, when string) {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if bytes.Contains(data, []byte(`"kind"`)) || bytes.Contains(data, []byte(`"source"`)) {
			t.Errorf("cloud file after %s contains the engine's internal wire shape:\n%s", when, data)
		}
		var doc struct {
			Resources map[string]struct {
				Attributes map[string]any `json:"attributes"`
			} `json:"resources"`
		}
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Fatalf("unmarshal cloud file: %v", err)
		}
		got, ok := doc.Resources[st.ProviderID].Attributes["tags"].(map[string]any)
		if !ok {
			t.Fatalf("tags is %T, want a plain JSON object", doc.Resources[st.ProviderID].Attributes["tags"])
		}
		if got["env"] != "dev" {
			t.Errorf(`tags.env after %s = %#v, want "dev"`, when, got["env"])
		}
	}
	requirePlainTags(t, "Create")

	// And back out again, with no nesting garbage.
	requireLeaves := func(t *testing.T, v value.Value, when string) {
		t.Helper()
		m, ok := v.Raw.(map[string]value.Value)
		if !ok {
			t.Fatalf("tags after %s is %T, want map[string]value.Value", when, v.Raw)
		}
		if got, ok := m["env"].AsString(); !ok || got != "dev" {
			t.Errorf("tags.env after %s = %#v; want the string dev, not a re-wrapped wire object", when, m["env"])
		}
		if got, ok := m["tier"].AsInt(); !ok || got != 2 {
			t.Errorf("tags.tier after %s = %#v, want 2", when, m["tier"])
		}
		items, ok := m["inner"].Raw.([]value.Value)
		if !ok || len(items) != 1 {
			t.Fatalf("tags.inner after %s = %#v, want a one-element list", when, m["inner"])
		}
		if got, ok := items[0].AsString(); !ok || got != "a" {
			t.Errorf("tags.inner[0] after %s = %#v, want the string a", when, items[0])
		}
	}
	requireLeaves(t, st.Attributes["tags"], "Create")

	read, err := p.Read(context.Background(), st)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	requireLeaves(t, read.Attributes["tags"], "Read")

	updated, err := p.Update(context.Background(), st, desired("db", "test.database", map[string]value.Value{
		"engine": value.String("postgres", value.SourceExplicit),
		"tags":   tags,
	}))
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	requirePlainTags(t, "Update")
	requireLeaves(t, updated.Attributes["tags"], "Update")
}

// TestFailureRuleReachesAllThreeClassifications drives ClassifyError from the
// cloud file, the way a person or an M3 test would.
//
// The rule carried a `Retryable bool`, which maps onto exactly two of the three
// provider.Retryability constants. Spec §15 gives the three materially
// different executor behaviour and spec §18 names "retry classification honored
// for each of the three categories" as a required test, so with a boolean a
// third of M3's retry logic was untestable — the fake provider is the only
// thing that will ever produce these errors.
func TestFailureRuleReachesAllThreeClassifications(t *testing.T) {
	cases := []struct {
		name  string
		field string
		want  provider.Retryability
	}{
		{"absent defaults to not safe", "", provider.NotSafeToRetry},
		{"not_safe", `"retryability": "not_safe",`, provider.NotSafeToRetry},
		{"conditional", `"retryability": "conditional",`, provider.ConditionallyRetryable},
		{"safe", `"retryability": "safe",`, provider.SafeToRetry},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "fake-cloud.json")
			doc := `{
  "resources": {},
  "failures": [
    {"op": "create", "address": "db", "nth": 1, ` + tc.field + ` "message": "boom"}
  ]
}`
			if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
				t.Fatalf("write cloud: %v", err)
			}
			p := New(path)

			_, err := p.Create(context.Background(), desired("db", "test.database", map[string]value.Value{
				"engine": value.String("postgres", value.SourceExplicit),
			}))
			if err == nil {
				t.Fatal("expected the injected failure")
			}
			if got := p.ClassifyError(err); got != tc.want {
				t.Errorf("ClassifyError = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestUnknownRetryabilityIsRejected keeps a typo in a hand-edited file loud.
// Silently treating "sfe" as not-safe is the same defect class that has already
// bitten this branch twice: a value interpreted by its surface text.
func TestUnknownRetryabilityIsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fake-cloud.json")
	doc := `{"resources": {}, "failures": [{"op": "create", "address": "db", "retryability": "sfe"}]}`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write cloud: %v", err)
	}

	_, err := LoadCloud(path)
	if err == nil {
		t.Fatal("LoadCloud accepted an unrecognised retryability")
	}
	if !strings.Contains(err.Error(), "sfe") {
		t.Errorf("error does not name the offending value: %v", err)
	}
}
