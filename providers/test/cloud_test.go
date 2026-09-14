package test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/resource"
	"github.com/infrena/infrena/pkg/value"
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
		"db-1": {Type: "fake.database", Attributes: map[string]any{"engine": "postgres", "size": float64(10)}},
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
	c := &Cloud{Resources: map[string]*CloudResource{"db-1": {Type: "fake.database"}}}
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

// TestConcurrentCreatesDoNotLoseUpdates exercises the load-mutate-save cycle
// from several goroutines at once.
//
// begin() loads the cloud, advances failure bookkeeping and saves on every
// operation — the read path included — and each CRUD method then saves again.
// With no guard, concurrent callers interleave and lose each other's writes.
// Spec §10 has refresh reading every resource in state concurrently, so the
// first concurrent caller arrives in M3.
func TestConcurrentCreatesDoNotLoseUpdates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fake-cloud.json")
	p := New(path)

	const n = 24
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() {
			_, errs[i] = p.Create(context.Background(), &resource.DesiredResource{
				Address: address.Address{Name: fmt.Sprintf("net%02d", i)},
				Type:    "fake.network",
				Attrs: map[string]value.Value{
					"cidr": value.String("10.0.0.0/16", value.SourceExplicit),
				},
			})
		})
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}

	c, err := LoadCloud(path)
	if err != nil {
		t.Fatalf("LoadCloud: %v", err)
	}
	if len(c.Resources) != n {
		t.Errorf("cloud holds %d resources after %d concurrent creates; writes were lost", len(c.Resources), n)
	}
	if c.NextID != n {
		t.Errorf("NextID = %d after %d concurrent creates, want %d", c.NextID, n, n)
	}
}

// TestConcurrentReadsSeeWholeFiles pairs concurrent readers with concurrent
// writers. begin() saves on the read path too, so without atomicity a reader
// can observe a truncated file and report it as a provider failure.
func TestConcurrentReadsSeeWholeFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fake-cloud.json")
	p := New(path)

	seed, err := p.Create(context.Background(), &resource.DesiredResource{
		Address: address.Address{Name: "net"},
		Type:    "fake.network",
		Attrs:   map[string]value.Value{"cidr": value.String("10.0.0.0/16", value.SourceExplicit)},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 32)
	for i := range errs {
		wg.Go(func() {
			st, err := p.Read(context.Background(), seed)
			if err != nil {
				errs[i] = err
				return
			}
			if st == nil {
				errs[i] = fmt.Errorf("concurrent Read reported the resource gone")
			}
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("Read %d: %v", i, err)
		}
	}
}

// TestSaveIsAtomicAndPrivate mirrors internal/state's guard on the same
// discipline: mode 0600, and no temporary file left behind.
func TestSaveIsAtomicAndPrivate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "fake-cloud.json")
	c := &Cloud{Resources: map[string]*CloudResource{"db-1": {Type: "fake.database"}}}
	if err := c.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("cloud file mode = %v, want 0600", info.Mode().Perm())
	}

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != filepath.Base(path) {
			t.Errorf("Save left %s behind", e.Name())
		}
	}
}
