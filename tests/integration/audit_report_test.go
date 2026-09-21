package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTheReportDescribesItself pins that a report names the build that produced
// it and the state serial the run produced.
//
// A report is archived and read long after the run. The serial is the strongest
// link it can carry: it says which state this run MADE, so two reports can be
// ordered against each other, a gap shows a run nobody archived, and a report
// can be matched against a state file rather than believed on its own.
func TestTheReportDescribesItself(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
`)
	reportPath := filepath.Join(t.TempDir(), "run.json")
	if r := run(t, dir, "apply", "dev", "--auto-approve",
		"--approved-by", "PR #42", "--output", reportPath); r.ExitCode != 2 {
		t.Fatalf("apply exit = %d\n%s", r.ExitCode, r.combined())
	}

	var meta, result map[string]any
	data, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("reading the report: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("report line is not JSON: %v\n%s", err, line)
		}
		switch m["type"] {
		case "meta":
			meta = m
		case "result":
			result = m
		}
	}
	if meta == nil || result == nil {
		t.Fatalf("report has no meta and/or result line:\n%s", data)
	}

	if _, ok := meta["infrena"]; !ok {
		t.Error("the meta line does not say which build produced the report")
	}
	if result["approved_by"] != "PR #42" {
		t.Errorf("approved_by = %v, want the value passed on the command line", result["approved_by"])
	}

	// The serial must be the one state actually holds, not merely present.
	serial, ok := result["state_serial"].(float64)
	if !ok || serial == 0 {
		t.Fatalf("result carries no state_serial: %v", result)
	}
	stateBytes, err := os.ReadFile(filepath.Join(dir, ".infrena", "state", "dev.json"))
	if err != nil {
		t.Fatalf("reading state: %v", err)
	}
	var st struct {
		Serial float64 `json:"serial"`
	}
	if err := json.Unmarshal(stateBytes, &st); err != nil {
		t.Fatalf("decoding state: %v", err)
	}
	if st.Serial != serial {
		t.Errorf("report says state_serial %v, state says %v — a report that names the wrong "+
			"state version is worse than one that names none", serial, st.Serial)
	}
}
