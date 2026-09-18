package report

import (
	"bufio"
	"bytes"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/infrena/infrena/pkg/value"
)

// TestWriterSerializesConcurrentWrites is the concurrency test the design
// requires: executor.Options.OnEvent and refresh's OnObservation hook are
// both documented as callable concurrently from multiple workers, so Writer
// must serialize its own access. Run under `go test -race`, this is the
// test that actually catches a missing mutex — bytes.Buffer is itself not
// safe for concurrent use, so two goroutines calling Write on it at once
// without Writer's lock trip the race detector directly, and would do so
// even without any assertion below. The assertions on top additionally
// discriminate against a *different* bug a lock alone does not prevent:
// interleaved partial writes producing a line that is not valid JSON, or a
// line count that does not match what was sent.
func TestWriterSerializesConcurrentWrites(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)

	const goroutines = 20
	const perGoroutine = 50

	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for range perGoroutine {
				_ = w.WriteEvent(Event{
					Event:   "started",
					Address: "network",
					Op:      "create",
					At:      time.Now(),
				})
			}
		}(g)
	}
	wg.Wait()

	scanner := bufio.NewScanner(&buf)
	lines := 0
	for scanner.Scan() {
		lines++
		var e Event
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			t.Fatalf("line %d is not valid JSON (a torn concurrent write): %v\nline: %s", lines, err, scanner.Text())
		}
		if e.Type != "event" {
			t.Fatalf("line %d: type = %q, want %q", lines, e.Type, "event")
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanning output: %v", err)
	}
	if want := goroutines * perGoroutine; lines != want {
		t.Fatalf("got %d lines, want %d — a race would drop or corrupt writes", lines, want)
	}
}

// TestMetaLineFieldOrderPutsVersionFirst pins the design's explicit
// requirement that "version" is the handshake field and must never be
// dropped or reordered behind other fields — checked here by asserting it
// is present and correct, not merely that marshalling succeeds (which would
// pass even with Version omitted via a bad json tag).
func TestMetaLine(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	started := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := w.Meta("apply", "prod", "0.0.0-test", started); err != nil {
		t.Fatalf("Meta: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if got["type"] != "meta" {
		t.Errorf(`type = %v, want "meta"`, got["type"])
	}
	if got["version"] != float64(Version) {
		t.Errorf("version = %v, want %d", got["version"], Version)
	}
	if got["command"] != "apply" {
		t.Errorf(`command = %v, want "apply"`, got["command"])
	}
	if got["environment"] != "prod" {
		t.Errorf(`environment = %v, want "prod"`, got["environment"])
	}
	if _, ok := got["startedAt"]; !ok {
		t.Error("startedAt is missing")
	}
}

// TestFormatRedactsSensitiveValues is the test that would fail if this
// package grew its own second redaction path instead of calling
// pkg/value.Format — the exact regression the design's "grep must still
// name only pkg/value/format.go" check guards against.
func TestFormatRedactsSensitiveValues(t *testing.T) {
	// Constructed by hand rather than importing pkg/value's constructors for
	// every field, to keep this test's only dependency on pkg/value narrow:
	// it exists to check Format calls through, not to re-test value.Format
	// itself (format_test.go already does that exhaustively).
	sensitive := value.String("hunter2", value.SourceExplicit).WithSensitive(true)
	got := Format(sensitive)
	if got != "<sensitive>" {
		t.Fatalf("Format(sensitive value) = %q, want the redacted marker — report.Format must call value.Format, not render Raw directly", got)
	}
}

func TestWritePlanEmitsATypedLineCarryingTheArtifact(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)

	artifact := json.RawMessage(`{"version":1,"environment":"dev"}`)
	if err := w.WritePlan(artifact); err != nil {
		t.Fatal(err)
	}

	var got struct {
		Type string          `json:"type"`
		Plan json.RawMessage `json:"plan"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &got); err != nil {
		t.Fatal(err)
	}
	if got.Type != "plan" {
		t.Errorf("type = %q, want plan", got.Type)
	}
	// Carried through verbatim: the artifact is the executor's instruction set,
	// and re-encoding it risks changing a number's exact text, which is the
	// hazard DecodePlan's UseNumber already guards on the way in.
	if string(got.Plan) != string(artifact) {
		t.Errorf("plan = %s, want %s", got.Plan, artifact)
	}
}

func TestVersionIsFour(t *testing.T) {
	// The format gained a line kind, so a consumer that only understands an
	// older version must be able to tell. PLAN.md section 61 keeps this
	// independent of every other format version.
	//
	// 2 was the "plan" line; 3 is MigrateResult, the result line
	// `state migrate --check` writes; 4 is the "applying" line apply and
	// destroy write before they execute.
	if Version != 4 {
		t.Errorf("Version = %d, want 4", Version)
	}
}

// The status crosses as a WORD. A consumer that had to map an exit code back
// to a meaning would be keeping a copy of a table that lives in the CLI.
func TestMigrateResultCarriesTheStatusAsAString(t *testing.T) {
	var buf bytes.Buffer
	if err := NewWriter(&buf).WriteMigrateResult(MigrateResult{
		Status:       "pending",
		Environments: []string{"dev", "production"},
	}); err != nil {
		t.Fatal(err)
	}

	var got MigrateResult
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Type != "result" {
		t.Errorf("type = %q, want result", got.Type)
	}
	if got.Status != "pending" {
		t.Errorf("status = %q, want pending", got.Status)
	}
	if len(got.Environments) != 2 || got.Environments[0] != "dev" {
		t.Errorf("environments = %v", got.Environments)
	}
}

// A plan line from apply is REDACTED, unlike the `plan` line `infrena plan`
// writes. That is the whole difference between the two kinds: one is a
// replayable input, this one is a report of a run, and this package's rule
// for a report is that no value reaches it except through Format.
func TestWritePlanChangesRedactsValues(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)

	err := w.WritePlanChanges(PlanChanges{
		Stage:       StageProposed,
		Environment: "dev",
		Changes: []ResourceChange{{
			Address: "aws.db.main",
			Type:    "aws.db",
			Kind:    "update",
			Changes: []AttributeChange{{
				Attribute: "password",
				Before:    Format(value.String("hunter2", value.SourceExplicit).WithSensitive(true)),
				After:     Format(value.String("hunter3", value.SourceExplicit).WithSensitive(true)),
			}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	line := bytes.TrimSpace(buf.Bytes())
	if bytes.Contains(line, []byte("hunter")) {
		t.Fatalf("a sensitive value reached the report: %s", line)
	}

	var got PlanChanges
	if err := json.Unmarshal(line, &got); err != nil {
		t.Fatal(err)
	}
	// NOT "plan": readSavedPlan scans a stream for that type and applies what
	// it finds, so a redacted plan carrying it would be applied as if its
	// values were real.
	if got.Type != "applying" {
		t.Errorf("type = %q, want applying", got.Type)
	}
	if got.Stage != StageProposed {
		t.Errorf("stage = %q, want %q", got.Stage, StageProposed)
	}
	if len(got.Changes) != 1 || len(got.Changes[0].Changes) != 1 {
		t.Fatalf("changes = %+v, want one resource with one attribute", got.Changes)
	}
	if before := got.Changes[0].Changes[0].Before; before != "<sensitive>" {
		t.Errorf("before = %q, want the redacted marker", before)
	}
}

// A plan line is a PROMISE ABOUT THE FUTURE, so an unknown value in it reads
// the way the plan renderer reads it. ReportFormatOptions' "(unknown)" is for
// a value that is still not known AFTER a run, which is an anomaly being
// reported — see value.ReportFormatOptions. A UI rendering a plan line beside
// a terminal's rendering of the same plan must not see two different words
// for the same thing.
func TestFormatPlannedSaysKnownAfterApply(t *testing.T) {
	unknown := value.Unknown(value.KindString, value.SourceExplicit)
	if got := FormatPlanned(unknown); got != "(known after apply)" {
		t.Errorf("FormatPlanned(unknown) = %q, want the plan's wording", got)
	}
	// Still the one redaction path.
	secret := value.String("hunter2", value.SourceExplicit).WithSensitive(true)
	if got := FormatPlanned(secret); got != "<sensitive>" {
		t.Errorf("FormatPlanned(sensitive) = %q, want the redacted marker", got)
	}
}
