package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"

	"github.com/infrena/infrena/internal/planner"
	"github.com/infrena/infrena/pkg/report"
)

// readSavedPlan reads a plan artifact from either envelope.
//
// TWO SHAPES, and accepting both is a compatibility requirement rather than a
// courtesy: saved plans already exist on disk, and PLAN.md section 37's
// saved-plan path is refused-not-degraded everywhere else, so a silent
// misinterpretation here would be the worst outcome available.
//
//   - A bare JSON document: what every infrena before this release wrote.
//   - A report stream whose `plan` line carries the artifact: what this one
//     writes, so a frontend tails one format for every command (spec 2.4).
//
// The sniff is on the first line's `type` field, and planner.DecodePlan
// carries an independent guard refusing any document that has one. Both are
// needed: without the second, a caller that reaches DecodePlan without coming
// through here decodes a meta line into an empty plan and applies nothing.
func readSavedPlan(path string) (*planner.Plan, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("%s is empty: it holds no plan to apply", path)
	}

	// The sniff. A pretty-printed bare artifact's first line is "{", which
	// does not unmarshal, and that is the signal rather than an error: only a
	// stream's first line is a complete JSON object carrying a type.
	first, _, _ := bytes.Cut(bytes.TrimSpace(data), []byte("\n"))
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(first, &probe); err != nil || probe.Type == "" {
		// The old shape, written by every infrena before this release.
		// DecodePlan carries its own guard against a typed document, so a
		// half-written stream cannot slip through here either.
		return planner.DecodePlan(data)
	}

	sc := bufio.NewScanner(bytes.NewReader(data))
	// The 64 KiB default is not enough for a plan line on a real environment,
	// and its failure mode is a TRUNCATED READ reported as a clean end of
	// input — which would surface as "no plan line" on a file that has one.
	sc.Buffer(make([]byte, 0, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var pl report.PlanLine
		if err := json.Unmarshal(line, &pl); err != nil || pl.Type != "plan" {
			continue
		}
		return planner.DecodePlan(pl.Plan)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return nil, fmt.Errorf(
		"%s is a report of a %q run and carries no plan line\n"+
			"Point --plan at a file written by `infrena plan --output`",
		path, commandOf(data))
}

// commandOf reads a report stream's meta line and returns the command it says
// the file reports on, or "unknown" when the line is missing or unreadable.
//
// It exists for one error message, and it earns its place there: "this is a
// report of an apply run" is what turns a bare "wrong file" into the thing
// the user actually did, which is point at the apply report instead of the
// plan. Every stream starts with the meta line, so only the first non-empty
// line is examined — a `command` field further down belongs to some other
// line kind and is not this file's provenance.
func commandOf(data []byte) string {
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var meta struct {
			Type    string `json:"type"`
			Command string `json:"command"`
		}
		if err := json.Unmarshal(line, &meta); err != nil || meta.Type != "meta" || meta.Command == "" {
			return "unknown"
		}
		return meta.Command
	}
	return "unknown"
}
