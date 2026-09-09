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
