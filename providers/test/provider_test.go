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
		t.Errorf("ClassifyError = %v, want NotSafeToRetry (rule did not set Retryable)", got)
	}
}
