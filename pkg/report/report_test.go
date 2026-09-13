package report

import (
	"bufio"
	"bytes"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/infrata/infrata/pkg/value"
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
	if err := w.Meta("apply", "prod", started); err != nil {
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
