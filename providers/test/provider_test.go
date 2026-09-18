package test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/provider"
	"github.com/infrena/infrena/pkg/resource"
	"github.com/infrena/infrena/pkg/value"
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
	st, err := p.Create(context.Background(), desired("db", "fake.database", map[string]value.Value{
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
	st, err := p.Create(context.Background(), desired("db", "fake.database", map[string]value.Value{
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
	st, _ := p.Create(context.Background(), desired("net", "fake.network", map[string]value.Value{
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
	st, _ := p.Create(ctx, desired("db", "fake.database", map[string]value.Value{
		"engine": value.String("postgres", value.SourceExplicit),
		"size":   value.Int(10, value.SourceDefault),
	}))

	updated, err := p.Update(ctx, st, desired("db", "fake.database", map[string]value.Value{
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

	_, err := p.Create(context.Background(), desired("db", "fake.database", map[string]value.Value{
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
	st, _ := p.Create(context.Background(), desired("db", "fake.database", map[string]value.Value{
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
	st, err := p.Create(ctx, desired("net", "fake.network", map[string]value.Value{
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
	st, err := p.Create(ctx, desired("db", "fake.database", map[string]value.Value{
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

// TestDiscoverAndImportAreNotImplementedYet was here until M8 implemented both.
// Its replacements are TestDiscoverFindsEveryResourceInTheCloud and
// TestImportReadsARealResourceByID at the bottom of this file, which assert
// what the capability DOES rather than that it refuses.
//
// One thing it checked is worth keeping and is not obvious from those: a
// capability a provider does not offer must report so rather than returning an
// empty result, because a discovery that silently finds nothing and one that
// cannot run look identical to a caller. The fake provider now offers both, so
// the assertion belongs to whichever provider does not — pkg/provider's
// ErrNotImplemented is still the contract, and Phase 3's AWS provider inherits
// this obligation for anything it leaves unbuilt.

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

	if _, err := p.Create(context.Background(), desired("db", "fake.database", attrs)); err != nil {
		t.Fatalf("first Create must succeed (Nth: 2 has not fired yet), got: %v", err)
	}

	_, err = p.Create(context.Background(), desired("db", "fake.database", attrs))
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
	created, err := p.Create(context.Background(), desired("db", "fake.database", map[string]value.Value{
		"engine": value.String("postgres", value.SourceExplicit),
	}))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	created.Dependencies = []address.Address{{Name: "net"}}
	created.CreatedAt = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	created.Lifecycle = resource.Lifecycle{PreventDestroy: true}

	updated, err := p.Update(context.Background(), created, desired("db", "fake.database", map[string]value.Value{
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
	created, err := p.Create(context.Background(), desired("db", "fake.database", map[string]value.Value{
		"engine": value.String("postgres", value.SourceExplicit),
	}))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	created.Dependencies = []address.Address{{Name: "net"}}
	created.Lifecycle = resource.Lifecycle{PreventDestroy: true}

	read, err := p.Read(context.Background(), created)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(read.Dependencies) != 1 || read.Dependencies[0].String() != "net" {
		t.Errorf("Read returned Dependencies %v, want [net]", read.Dependencies)
	}
	// Lifecycle is carried for a sharper reason than the others: `infra
	// refresh` persists whatever Read returns (internal/cli/refresh.go calls
	// st.Set on it and Puts the result), so a Read that dropped it would not
	// leave state stale — it would ERASE a prevent_destroy or retain guard from
	// state outright, and the next destroy would proceed with nothing to stop
	// it. This is the one carried field whose loss is silent and destructive,
	// and it was the field this test did not check.
	if !read.Lifecycle.PreventDestroy {
		t.Error("Read dropped Lifecycle — a refresh would erase the guard from state")
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
	st, err := p.Create(context.Background(), desired("net", "fake.network", map[string]value.Value{
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

	st, err := p.Create(context.Background(), desired("db", "fake.database", map[string]value.Value{
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

	updated, err := p.Update(context.Background(), st, desired("db", "fake.database", map[string]value.Value{
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

			_, err := p.Create(context.Background(), desired("db", "fake.database", map[string]value.Value{
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

func TestOperationsOverlapRatherThanSerialise(t *testing.T) {
	// The mutex exists to protect the cloud file, not to serialise simulated
	// latency. If it covers the delay, every concurrent test in M2 and M3
	// passes while proving nothing.
	p, path := newTestProvider(t)
	ctx := context.Background()

	const (
		resources = 4
		delayMS   = 200
	)

	states := make([]*resource.ResourceState, 0, resources)
	for i := range resources {
		st, err := p.Create(ctx, desired(fmt.Sprintf("net%d", i), "fake.network", map[string]value.Value{
			"cidr": value.String("10.0.0.0/16", value.SourceExplicit),
		}))
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		states = append(states, st)
	}

	readAll := func() time.Duration {
		t.Helper()
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
		return elapsed
	}

	readEachInTurn := func() time.Duration {
		t.Helper()
		start := time.Now()
		for _, st := range states {
			if _, err := p.Read(ctx, st); err != nil {
				t.Fatalf("serial Read: %v", err)
			}
		}
		return time.Since(start)
	}

	c, err := LoadCloud(path)
	if err != nil {
		t.Fatalf("LoadCloud: %v", err)
	}
	c.LatencyMS = delayMS
	if err := c.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// BOTH SIDES MEASURED, WHICH IS THE POINT OF THE REWRITE.
	//
	// This used to compare the concurrent time against a PREDICTED serial time,
	// resources*delay, and fail at half of it. That prediction assumes the only
	// thing four reads cost is four delays — no file loading, no goroutine
	// scheduling, no race detector, and a runner that is not busy. On a shared
	// CI runner none of that holds, and the assumption failed twice: at 150ms,
	// where the fix was to raise the delay to 400ms, and again at 400ms, at
	// 827ms against an 800ms threshold, blocking a release. Raising it a third
	// time is the same losing move, because the quantity being guessed at is
	// the one that varies.
	//
	// Running the same reads SEQUENTIALLY measures what serial actually costs
	// on this machine, under this load, a second apart from the concurrent run.
	// Whatever slows one slows the other, so the comparison survives a busy
	// runner instead of being the first casualty of it.
	serial := readEachInTurn()
	concurrent := readAll()

	// Half of a measured serial run. An overlapping provider comes in around a
	// quarter of it — one delay against four — so there is a full delay of
	// headroom, and a serialising one exceeds it by construction rather than by
	// arithmetic anybody had to predict.
	if concurrent >= serial/2 {
		t.Errorf("%d reads with %dms latency took %v concurrently and %v one after another; "+
			"concurrent must come in under %v. The provider is serialising — the mutex is "+
			"covering the delay, so no concurrency test against this provider can fail.",
			resources, delayMS, concurrent, serial, serial/2)
	}
}

// writeCloud writes a cloud file directly, so a test can describe
// infrastructure this tool never created — which is the whole subject of
// discovery.
func writeCloud(t *testing.T, path string, c *Cloud) {
	t.Helper()
	if err := c.Save(path); err != nil {
		t.Fatalf("writing the cloud: %v", err)
	}
}

// preexisting is a cloud holding two resources with no `address` — nothing here
// was created by this project, which is the case discovery exists for.
func preexisting() *Cloud {
	return &Cloud{Resources: map[string]*CloudResource{
		"net-1": {Type: "fake.network", Attributes: map[string]any{
			"cidr": "10.0.0.0/16", "id": "net-1",
		}},
		"db-9": {Type: "fake.database", Attributes: map[string]any{
			"engine": "postgres", "id": "db-9", "password": "hunter2",
		}},
	}}
}

// TestDiscoverFindsEveryResourceInTheCloud, including ones this project never
// created — which is the entire point: discovery is for infrastructure that
// predates the tool. Neither fixture resource carries an `address`.
func TestDiscoverFindsEveryResourceInTheCloud(t *testing.T) {
	p, path := newTestProvider(t)
	writeCloud(t, path, preexisting())

	got, err := p.Discover(context.Background(), provider.DiscoverRequest{})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("found %d resources, want 2: %+v", len(got), got)
	}
	// Sorted by provider ID. The list is printed to a user and diffed by
	// scripts, and Go's map iteration is randomised — "db-9" before "net-1"
	// contradicts the order the fixture happens to be written in.
	if got[0].ProviderID != "db-9" || got[1].ProviderID != "net-1" {
		t.Errorf("discovery is not sorted: %s, %s", got[0].ProviderID, got[1].ProviderID)
	}
	if got[0].Type != "fake.database" {
		t.Errorf("got[0].Type = %q, want fake.database", got[0].Type)
	}
	if cidr, _ := got[1].Attributes["cidr"].AsString(); cidr != "10.0.0.0/16" {
		t.Errorf("net-1 cidr = %v, want the provider's value", got[1].Attributes["cidr"])
	}
	// Provenance survives discovery: these values came from the provider, and
	// a later generation step decides what to emit by reading exactly this.
	if got[1].Attributes["cidr"].Source != value.SourceProvider {
		t.Errorf("cidr source = %v, want SourceProvider", got[1].Attributes["cidr"].Source)
	}
	// Sensitivity is applied from the SCHEMA, here as everywhere else. A
	// discovered password that arrives unmarked is a password a generator will
	// happily write into a file destined for version control.
	if !got[0].Attributes["password"].Sensitive {
		t.Error("a discovered password is not marked sensitive; generation would write it to disk")
	}
}

// TestDiscoverIsDeterministic — map iteration is randomised, so one run proving
// the order proves nothing.
func TestDiscoverIsDeterministic(t *testing.T) {
	p, path := newTestProvider(t)
	c := preexisting()
	for _, id := range []string{"alpha", "zeta", "mid", "beta"} {
		c.Resources[id] = &CloudResource{Type: "fake.network", Attributes: map[string]any{"id": id}}
	}
	writeCloud(t, path, c)

	var first []string
	for i := range 20 {
		got, err := p.Discover(context.Background(), provider.DiscoverRequest{})
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, r := range got {
			ids = append(ids, r.ProviderID)
		}
		if i == 0 {
			first = ids
			continue
		}
		if strings.Join(ids, ",") != strings.Join(first, ",") {
			t.Fatalf("run %d returned %v, want %v", i, ids, first)
		}
	}
	if strings.Join(first, ",") != "alpha,beta,db-9,mid,net-1,zeta" {
		t.Errorf("discovery order = %v, want sorted by provider ID", first)
	}
}

// TestDiscoverFiltersByType. §25's `infra discover aws.rds` asks one question of
// a large account, and answering it by fetching everything and discarding most
// is how discovery becomes too slow to use.
func TestDiscoverFiltersByType(t *testing.T) {
	p, path := newTestProvider(t)
	writeCloud(t, path, preexisting())

	got, err := p.Discover(context.Background(), provider.DiscoverRequest{Types: []string{"fake.database"}})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	// Both halves. A count alone passes against a filter that returns nothing,
	// and a presence check alone passes against one that filters nothing.
	if len(got) != 1 || got[0].ProviderID != "db-9" {
		t.Fatalf("filtered discovery = %+v, want only db-9", got)
	}
	for _, r := range got {
		if r.Type == "fake.network" {
			t.Errorf("the filter returned a %s, which was not asked for", r.Type)
		}
	}
}

// TestDiscoverOnAnEmptyCloudFindsNothing — an account with nothing in it is not
// an error, and neither is a project that has never applied anything.
func TestDiscoverOnAnEmptyCloudFindsNothing(t *testing.T) {
	p, _ := newTestProvider(t)
	got, err := p.Discover(context.Background(), provider.DiscoverRequest{})
	if err != nil {
		t.Fatalf("discovering an empty cloud must not be an error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("found %d resources in an empty cloud", len(got))
	}
}

// TestImportReadsARealResourceByID — §26. Import adopts what already exists.
func TestImportReadsARealResourceByID(t *testing.T) {
	p, path := newTestProvider(t)
	writeCloud(t, path, preexisting())

	st, err := p.Import(context.Background(), "fake.database", "db-9")
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if st.ProviderID != "db-9" {
		t.Errorf("ProviderID = %q, want db-9", st.ProviderID)
	}
	if st.Type != "fake.database" {
		t.Errorf("Type = %q, want fake.database", st.Type)
	}
	if engine, _ := st.Attributes["engine"].AsString(); engine != "postgres" {
		t.Errorf("engine = %v, want the provider's value", st.Attributes["engine"])
	}
	if st.Attributes["engine"].Source != value.SourceProvider {
		t.Errorf("engine source = %v, want SourceProvider", st.Attributes["engine"].Source)
	}
	// Sensitivity comes from the schema, decided in one place. An imported
	// secret that reaches state unmarked prints in clear in the next plan.
	if !st.Attributes["password"].Sensitive {
		t.Error("an imported password is not marked sensitive")
	}
}

// TestImportOfAnUnknownIDNamesTheID. A user importing by ID has usually
// mistyped it or is looking in the wrong account; an error that does not repeat
// the ID leaves them unable to tell which.
func TestImportOfAnUnknownIDNamesTheID(t *testing.T) {
	p, path := newTestProvider(t)
	writeCloud(t, path, preexisting())

	_, err := p.Import(context.Background(), "fake.database", "db-404")
	if err == nil {
		t.Fatal("importing an ID that does not exist must be an error")
	}
	if !strings.Contains(err.Error(), "db-404") {
		t.Errorf("the error does not name the ID: %v", err)
	}
}

// TestImportOfTheWrongTypeIsRefused. `import fake.network db-9` names a real
// resource of the wrong type. Adopting it anyway writes state claiming a
// database is a network, and the next plan proposes replacing real
// infrastructure to fix a disagreement the tool invented.
func TestImportOfTheWrongTypeIsRefused(t *testing.T) {
	p, path := newTestProvider(t)
	writeCloud(t, path, preexisting())

	_, err := p.Import(context.Background(), "fake.network", "db-9")
	if err == nil {
		t.Fatal("importing a resource as the wrong type must be an error")
	}
	for _, want := range []string{"db-9", "fake.network", "fake.database"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %q — it must say what was asked for and what is "+
				"actually there: %v", want, err)
		}
	}
}
