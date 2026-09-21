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
// Two shapes, because saved plans written by an earlier release already exist on
// disk:
//
//   - A bare JSON document, what every release before this one wrote.
//   - A report stream whose `plan` line carries the artifact, what this one writes,
//     so a frontend tails one format for every command.
//
// Anything else is refused by name rather than degraded, because a silent
// misinterpretation here is the worst outcome available on this path.
//
// The sniff is on the first line's `type` field, and planner.DecodePlan carries an
// independent guard refusing any document that has one. Both are needed: without the
// second, a caller that reaches DecodePlan without coming through here decodes a
// meta line into an empty plan and applies nothing.
func readSavedPlan(path string) (*planner.Plan, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("%s is empty: it holds no plan to apply", path)
	}

	// A pretty-printed bare artifact's first line is "{", which does not
	// unmarshal, and that is the signal rather than an error: only a stream's
	// first line is a complete JSON object carrying a type.
	first, _, _ := bytes.Cut(bytes.TrimSpace(data), []byte("\n"))
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(first, &probe); err != nil || probe.Type == "" {
		// The bare document. DecodePlan carries its own guard against a typed
		// one, so a half-written stream cannot slip through here either.
		return planner.DecodePlan(data)
	}

	sc := bufio.NewScanner(bytes.NewReader(data))
	// The 64 KiB default is not enough for a plan line on a real environment,
	// and its failure mode is a truncated read reported as a clean end of
	// input, which surfaces as "no plan line" on a file that has one.
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
		// The second guard on the stream's shape. A `plan` line is taken only
		// out of a file that reports on `plan`: apply and destroy write a plan
		// of their own, redacted and typed so this scan cannot see it, and this
		// is what keeps them apart if a later line kind — or a file assembled
		// by hand — carries the other name. Applying a redacted plan would
		// write "<sensitive>" into infrastructure.
		//
		// It sits inside the loop so that a file with no plan line at all keeps
		// the more specific refusal below.
		if command := commandOf(data); command != "plan" {
			return nil, fmt.Errorf(
				"%s is a report of a %q run, so the plan line in it is not a saved plan\n"+
					"Point --plan at a file written by `infrena plan --output`",
				path, command)
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
// It exists for one error message, where "this is a report of an apply run" turns a
// bare "wrong file" into the thing the user actually did. Every stream starts with
// the meta line, so only the first non-empty line is examined: a `command` field
// further down belongs to some other line kind and is not the file's provenance.
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
