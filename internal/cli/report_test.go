package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infrata/infrata/internal/state"
	"github.com/infrata/infrata/pkg/address"
	"github.com/infrata/infrata/pkg/resource"
	"github.com/infrata/infrata/pkg/value"
	testprovider "github.com/infrata/infrata/providers/test"
)

// decodeNDJSON parses --output's contents into one map per line, failing the
// test outright if any line is not valid JSON or lacks the "type" field
// every line in this wire format is required to self-describe with — a
// weaker check here (e.g. only validating the LAST line) would miss a
// torn or malformed line buried in the middle of a real run's output.
func decodeNDJSON(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	trimmed := strings.TrimRight(string(data), "\n")
	if trimmed == "" {
		t.Fatal("--output is empty")
	}
	var out []map[string]any
	for i, line := range strings.Split(trimmed, "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("line %d is not valid JSON: %v\nline: %s", i+1, err, line)
		}
		if _, ok := m["type"]; !ok {
			t.Fatalf("line %d has no \"type\" field: %s", i+1, line)
		}
		out = append(out, m)
	}
	return out
}

func linesOfType(lines []map[string]any, typ string) []map[string]any {
	var out []map[string]any
	for _, l := range lines {
		if l["type"] == typ {
			out = append(out, l)
		}
	}
	return out
}

func readNDJSON(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading --output: %v", err)
	}
	return decodeNDJSON(t, data)
}

// TestValidateOutputWritesMetaAndResult pins the minimal three-line shape
// validate's own doc comment promises: meta, then whatever diagnostics
// applied (none here), then result — and specifically that "version" is the
// literal wire version, not merely present, which a looser assertion (e.g.
// checking the field exists) would not catch if a future change forgot to
// stamp it from report.Version.
func TestValidateOutputWritesMetaAndResult(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  network:
    type: fake.network
    cidr: 10.20.0.0/16
`)
	outPath := filepath.Join(t.TempDir(), "out.ndjson")
	opts := &GlobalOptions{Dir: dir, Parallelism: 4, Output: outPath}
	cmd := newValidateCommand(opts)
	cmd.SetArgs([]string{"dev"})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() = %v, want nil", err)
	}

	lines := readNDJSON(t, outPath)
	meta := lines[0]
	if meta["type"] != "meta" {
		t.Fatalf("first line type = %v, want \"meta\"", meta["type"])
	}
	if meta["version"] != float64(1) {
		t.Errorf("meta.version = %v, want 1", meta["version"])
	}
	if meta["command"] != "validate" {
		t.Errorf("meta.command = %v, want \"validate\"", meta["command"])
	}
	if meta["environment"] != "dev" {
		t.Errorf("meta.environment = %v, want \"dev\"", meta["environment"])
	}
	if _, ok := meta["startedAt"]; !ok {
		t.Error("meta.startedAt is missing")
	}

	result := lines[len(lines)-1]
	if result["type"] != "result" || result["valid"] != true {
		t.Errorf("last line = %v, want {type: result, valid: true}", result)
	}
}

// TestValidateOutputReportsDiagnosticWhenInvalid is the test that would
// fail if validate's RunE were refactored back to the pre-instrumentation
// shape that rendered diagnostics to stderr and returned before ever
// touching --output: it asserts on the CONTENT of a diagnostic line, not
// merely that Execute() returned an error, and separately asserts
// result.valid is false rather than merely absent.
func TestValidateOutputReportsDiagnosticWhenInvalid(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  database:
    type: aws.rds
    engine: postgres
`)
	outPath := filepath.Join(t.TempDir(), "out.ndjson")
	opts := &GlobalOptions{Dir: dir, Parallelism: 4, Output: outPath}
	cmd := newValidateCommand(opts)
	cmd.SetArgs([]string{"dev"})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))

	if err := cmd.Execute(); err == nil {
		t.Fatal("Execute() = nil, want an error for invalid configuration")
	}

	lines := readNDJSON(t, outPath)
	diags := linesOfType(lines, "diagnostic")
	if len(diags) == 0 {
		t.Fatal("no diagnostic line was written for invalid configuration")
	}
	found := false
	for _, d := range diags {
		if d["severity"] != "error" {
			t.Errorf("diagnostic severity = %v, want \"error\"", d["severity"])
		}
		if s, _ := d["summary"].(string); strings.Contains(s, "aws.rds") {
			found = true
		}
	}
	if !found {
		t.Errorf("no diagnostic named the offending type aws.rds: %v", diags)
	}

	result := lines[len(lines)-1]
	if result["type"] != "result" || result["valid"] != false {
		t.Errorf("last line = %v, want {type: result, valid: false}", result)
	}
}

// TestApplyOutputWritesEventsAndResult exercises a clean, successful apply
// with changes: it checks that per-operation events actually name the right
// address and op (not merely that SOME event lines exist), and — the
// assertion that would fail against a version of finishApply that folded
// errChanges into result.error the way it does for every other error —
// that result.error is ABSENT on a successful apply that had changes.
func TestApplyOutputWritesEventsAndResult(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  network:
    type: fake.network
    cidr: 10.20.0.0/16
`)
	outPath := filepath.Join(t.TempDir(), "out.ndjson")
	opts := &GlobalOptions{Dir: dir, Parallelism: 4, AutoApprove: true, Output: outPath}
	cmd := newApplyCommand(opts)
	cmd.SetArgs([]string{"dev"})
	cmd.SetIn(strings.NewReader(""))
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))

	if err := cmd.Execute(); !errors.Is(err, errChanges) {
		t.Fatalf("Execute() = %v, want errChanges", err)
	}

	lines := readNDJSON(t, outPath)
	if lines[0]["type"] != "meta" || lines[0]["command"] != "apply" {
		t.Fatalf("first line = %v, want meta for apply", lines[0])
	}

	events := linesOfType(lines, "event")
	if len(events) == 0 {
		t.Fatal("no event lines were written")
	}
	var sawStarted, sawSucceeded bool
	for _, e := range events {
		if e["address"] != "network" {
			t.Errorf("event for unexpected address: %v", e)
		}
		if e["op"] != "create" {
			t.Errorf("event.op = %v, want \"create\"", e["op"])
		}
		switch e["event"] {
		case "started":
			sawStarted = true
		case "succeeded":
			sawSucceeded = true
		}
	}
	if !sawStarted || !sawSucceeded {
		t.Errorf("missing started/succeeded events: %v", events)
	}

	result := lines[len(lines)-1]
	if result["type"] != "result" {
		t.Fatalf("last line = %v, want the result line", result)
	}
	if _, hasError := result["error"]; hasError {
		t.Errorf("result.error = %v, want absent — errChanges is success, not failure", result["error"])
	}
	applied, _ := result["applied"].([]any)
	if len(applied) != 1 || applied[0] != "network" {
		t.Errorf("result.applied = %v, want [\"network\"]", result["applied"])
	}
}

// TestApplyOutputReportsFailedEventAndResult is the failing-apply case the
// design specifically calls out for manual verification; this is its
// automated, discriminating form: it checks the failed event carries a
// non-empty error string (not merely that event=="failed" appears), and
// that result.error is PRESENT for a genuinely failed run — the opposite
// assertion from the errChanges case above, which is what makes the two
// tests together discriminate the "error folded in unless errChanges" rule
// in both directions.
func TestApplyOutputReportsFailedEventAndResult(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  network:
    type: fake.network
    cidr: 10.20.0.0/16
`)
	cloud := testprovider.Cloud{
		Resources: map[string]*testprovider.CloudResource{},
		Failures: []testprovider.FailureRule{
			{Op: "create", Address: "network", Nth: 1, Message: "injected failure"},
		},
	}
	data, err := json.MarshalIndent(&cloud, "", "  ")
	if err != nil {
		t.Fatalf("marshaling fake cloud: %v", err)
	}
	cloudPath := filepath.Join(dir, testprovider.DefaultCloudPath)
	if err := os.MkdirAll(filepath.Dir(cloudPath), 0o755); err != nil {
		t.Fatalf("creating cloud dir: %v", err)
	}
	if err := os.WriteFile(cloudPath, data, 0o600); err != nil {
		t.Fatalf("seeding the fake cloud with a failure rule: %v", err)
	}

	outPath := filepath.Join(t.TempDir(), "out.ndjson")
	opts := &GlobalOptions{Dir: dir, Parallelism: 4, AutoApprove: true, Output: outPath}
	cmd := newApplyCommand(opts)
	cmd.SetArgs([]string{"dev"})
	cmd.SetIn(strings.NewReader(""))
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))

	execErr := cmd.Execute()
	if execErr == nil || errors.Is(execErr, errChanges) {
		t.Fatalf("Execute() = %v, want a plain error", execErr)
	}

	lines := readNDJSON(t, outPath)

	failedEvents := linesOfType(lines, "event")
	count := 0
	for _, e := range failedEvents {
		if e["event"] != "failed" {
			continue
		}
		count++
		errStr, _ := e["error"].(string)
		if errStr == "" {
			t.Errorf("failed event has no error text: %v", e)
		}
	}
	if count != 1 {
		t.Fatalf("got %d failed events, want 1", count)
	}

	result := lines[len(lines)-1]
	errStr, _ := result["error"].(string)
	if errStr == "" {
		t.Error("result.error is absent for a failed apply")
	}
	failed, _ := result["failed"].(map[string]any)
	if len(failed) != 1 {
		t.Errorf("result.failed = %v, want exactly one entry", result["failed"])
	}
}

// TestDestroyOutputWritesEventsAndResult is destroy's counterpart to
// TestApplyOutputWritesEventsAndResult: destroy shares finishApply,
// applyResultFrom and the executor.OnEvent wiring with apply (report.go's
// finishApply doc comment says so explicitly), so this checks that sharing
// actually reaches destroy's own command — an address destroyed reports
// op:"destroy", not "create", and command:"destroy" in the meta line, not
// "apply".
func TestDestroyOutputWritesEventsAndResult(t *testing.T) {
	dir := seedOneNetwork(t, "dev")

	outPath := filepath.Join(t.TempDir(), "out.ndjson")
	opts := &GlobalOptions{Dir: dir, Parallelism: 4, Output: outPath}
	cmd := newDestroyCommand(opts)
	cmd.SetArgs([]string{"dev"})
	cmd.SetIn(strings.NewReader("dev\n"))
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))

	if err := cmd.Execute(); !errors.Is(err, errChanges) {
		t.Fatalf("Execute() = %v, want errChanges", err)
	}

	lines := readNDJSON(t, outPath)
	if lines[0]["type"] != "meta" || lines[0]["command"] != "destroy" {
		t.Fatalf("first line = %v, want meta for destroy", lines[0])
	}

	events := linesOfType(lines, "event")
	if len(events) == 0 {
		t.Fatal("no event lines were written")
	}
	var sawStarted, sawSucceeded bool
	for _, e := range events {
		if e["address"] != "network" {
			t.Errorf("event for unexpected address: %v", e)
		}
		if e["op"] != "destroy" {
			t.Errorf("event.op = %v, want \"destroy\"", e["op"])
		}
		switch e["event"] {
		case "started":
			sawStarted = true
		case "succeeded":
			sawSucceeded = true
		}
	}
	if !sawStarted || !sawSucceeded {
		t.Errorf("missing started/succeeded events: %v", events)
	}

	result := lines[len(lines)-1]
	if _, hasError := result["error"]; hasError {
		t.Errorf("result.error = %v, want absent — errChanges is success, not failure", result["error"])
	}
	applied, _ := result["applied"].([]any)
	if len(applied) != 1 || applied[0] != "network" {
		t.Errorf("result.applied = %v, want [\"network\"] — destroy's removal still reports as Applied, matching executor.Result", result["applied"])
	}
}

// TestDestroyOutputReportsFailedEventAndResult is destroy's counterpart to
// TestApplyOutputReportsFailedEventAndResult.
func TestDestroyOutputReportsFailedEventAndResult(t *testing.T) {
	dir := seedOneNetwork(t, "dev")

	cloudPath := filepath.Join(dir, testprovider.DefaultCloudPath)
	cloud, err := testprovider.LoadCloud(cloudPath)
	if err != nil {
		t.Fatalf("loading fake cloud: %v", err)
	}
	cloud.Failures = append(cloud.Failures, testprovider.FailureRule{
		Op: "delete", Address: "network", Nth: 1, Message: "injected failure",
	})
	if err := cloud.Save(cloudPath); err != nil {
		t.Fatalf("saving fake cloud with a failure rule: %v", err)
	}

	outPath := filepath.Join(t.TempDir(), "out.ndjson")
	opts := &GlobalOptions{Dir: dir, Parallelism: 4, Output: outPath}
	cmd := newDestroyCommand(opts)
	cmd.SetArgs([]string{"dev"})
	cmd.SetIn(strings.NewReader("dev\n"))
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))

	execErr := cmd.Execute()
	if execErr == nil || errors.Is(execErr, errChanges) {
		t.Fatalf("Execute() = %v, want a plain error", execErr)
	}

	lines := readNDJSON(t, outPath)
	count := 0
	for _, e := range linesOfType(lines, "event") {
		if e["event"] != "failed" {
			continue
		}
		count++
		errStr, _ := e["error"].(string)
		if errStr == "" {
			t.Errorf("failed event has no error text: %v", e)
		}
	}
	if count != 1 {
		t.Fatalf("got %d failed events, want 1", count)
	}

	result := lines[len(lines)-1]
	destroyErrStr, _ := result["error"].(string)
	if destroyErrStr == "" {
		t.Error("result.error is absent for a failed destroy")
	}
	destroyFailed, _ := result["failed"].(map[string]any)
	if len(destroyFailed) != 1 {
		t.Errorf("result.failed = %v, want exactly one entry", result["failed"])
	}
}

// TestRefreshOutputRedactsSensitiveDrift is this task's core redaction
// test: it plants a sensitive attribute, drifts it outside infra, and
// checks BOTH that the raw --output bytes never contain either cleartext
// value anywhere in the file (a check strong enough to catch a leak in a
// field this test does not otherwise inspect) AND that the specific
// "password" change is reported with both sides equal to the exact redacted
// marker — a test that only checked the byte-absence of the secret would
// pass even if the attribute were silently dropped from Changes entirely,
// which is a different bug this second half of the test catches.
func TestRefreshOutputRedactsSensitiveDrift(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources: {}
`)
	ctx := context.Background()
	cloudPath := filepath.Join(dir, testprovider.DefaultCloudPath)
	prov := testprovider.New(cloudPath)
	rs, err := prov.Create(ctx, &resource.DesiredResource{
		Address: address.Address{Name: "database"},
		Type:    "fake.database",
		Attrs: map[string]value.Value{
			"engine":   value.String("postgres", value.SourceExplicit),
			"password": value.String("hunter2", value.SourceExplicit).WithSensitive(true),
		},
	})
	if err != nil {
		t.Fatalf("seeding the fake cloud: %v", err)
	}
	st := state.New("myapp", "dev")
	st.Set(rs)
	seedState(t, dir, "dev", st)

	cloud, err := testprovider.LoadCloud(cloudPath)
	if err != nil {
		t.Fatalf("loading fake cloud: %v", err)
	}
	cloud.Resources[rs.ProviderID].Attributes["password"] = "swordfish"
	if err := cloud.Save(cloudPath); err != nil {
		t.Fatalf("saving fake cloud: %v", err)
	}

	outPath := filepath.Join(t.TempDir(), "out.ndjson")
	opts := &GlobalOptions{Dir: dir, Parallelism: 4, Output: outPath}
	cmd := newRefreshCommand(opts)
	cmd.SetArgs([]string{"dev"})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() = %v, want nil", err)
	}

	raw, rerr := os.ReadFile(outPath)
	if rerr != nil {
		t.Fatalf("reading --output: %v", rerr)
	}
	if strings.Contains(string(raw), "hunter2") || strings.Contains(string(raw), "swordfish") {
		t.Fatalf("--output leaked a sensitive value:\n%s", raw)
	}

	lines := decodeNDJSON(t, raw)
	obsLines := linesOfType(lines, "observation")
	if len(obsLines) != 1 {
		t.Fatalf("got %d observation lines, want 1", len(obsLines))
	}
	o := obsLines[0]
	if o["status"] != "changed" {
		t.Fatalf("observation.status = %v, want \"changed\"", o["status"])
	}
	changes, _ := o["changes"].([]any)
	foundPassword := false
	for _, c := range changes {
		cm, ok := c.(map[string]any)
		if !ok || cm["attribute"] != "password" {
			continue
		}
		foundPassword = true
		if cm["before"] != "<sensitive>" || cm["after"] != "<sensitive>" {
			t.Errorf("password change = %v, want both sides redacted", cm)
		}
	}
	if !foundPassword {
		t.Fatalf("no attribute change reported for password: %v", changes)
	}

	result := lines[len(lines)-1]
	drifted, _ := result["drifted"].([]any)
	if len(drifted) != 1 || drifted[0] != "database" {
		t.Errorf("result.drifted = %v, want [\"database\"]", result["drifted"])
	}
}

// TestRefreshOutputCountsUnchangedRatherThanListingThem pins the team-lead
// ruling that RefreshResult.Unchanged is a COUNT, not a list of addresses —
// the observation stream already named every one of them as it ran. Two
// resources are seeded identically to the fake cloud (no drift) and one is
// drifted, so a version of buildRefreshResult that still appended addresses
// to a []string here would produce a JSON array, which this test's type
// assertion to float64 would fail against directly — not merely "the count
// is wrong", but "the wire shape itself regressed to a list".
func TestRefreshOutputCountsUnchangedRatherThanListingThem(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources: {}
`)
	ctx := context.Background()
	cloudPath := filepath.Join(dir, testprovider.DefaultCloudPath)
	prov := testprovider.New(cloudPath)

	st := state.New("myapp", "dev")
	for _, name := range []string{"a", "b"} {
		rs, err := prov.Create(ctx, &resource.DesiredResource{
			Address: address.Address{Name: name},
			Type:    "fake.network",
			Attrs: map[string]value.Value{
				"cidr": value.String("10.20.0.0/16", value.SourceExplicit),
			},
		})
		if err != nil {
			t.Fatalf("seeding the fake cloud: %v", err)
		}
		st.Set(rs)
	}
	seedState(t, dir, "dev", st)

	outPath := filepath.Join(t.TempDir(), "out.ndjson")
	opts := &GlobalOptions{Dir: dir, Parallelism: 4, Output: outPath}
	cmd := newRefreshCommand(opts)
	cmd.SetArgs([]string{"dev"})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() = %v, want nil", err)
	}

	lines := readNDJSON(t, outPath)
	result := lines[len(lines)-1]
	if result["type"] != "result" {
		t.Fatalf("last line = %v, want the result line", result)
	}
	if _, isList := result["unchanged"].([]any); isList {
		t.Fatalf("result.unchanged = %v, is a list — want a bare count", result["unchanged"])
	}
	count, ok := result["unchanged"].(float64)
	if !ok || count != 2 {
		t.Errorf("result.unchanged = %v, want the number 2", result["unchanged"])
	}
	if _, hasDrifted := result["drifted"]; hasDrifted {
		t.Errorf("result.drifted = %v, want absent — nothing drifted", result["drifted"])
	}
}

// TestRefreshOutputReportsRemoval is the "removed" counterpart to the
// drift test above, over the same observation/result machinery.
func TestRefreshOutputReportsRemoval(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources: {}
`)
	ctx := context.Background()
	cloudPath := filepath.Join(dir, testprovider.DefaultCloudPath)
	prov := testprovider.New(cloudPath)
	rs, err := prov.Create(ctx, &resource.DesiredResource{
		Address: address.Address{Name: "network"},
		Type:    "fake.network",
		Attrs: map[string]value.Value{
			"cidr": value.String("10.20.0.0/16", value.SourceExplicit),
		},
	})
	if err != nil {
		t.Fatalf("seeding the fake cloud: %v", err)
	}
	st := state.New("myapp", "dev")
	st.Set(rs)
	seedState(t, dir, "dev", st)

	cloud, err := testprovider.LoadCloud(cloudPath)
	if err != nil {
		t.Fatalf("loading fake cloud: %v", err)
	}
	delete(cloud.Resources, rs.ProviderID)
	if err := cloud.Save(cloudPath); err != nil {
		t.Fatalf("saving fake cloud: %v", err)
	}

	outPath := filepath.Join(t.TempDir(), "out.ndjson")
	opts := &GlobalOptions{Dir: dir, Parallelism: 4, Output: outPath}
	cmd := newRefreshCommand(opts)
	cmd.SetArgs([]string{"dev"})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() = %v, want nil", err)
	}

	lines := readNDJSON(t, outPath)
	obsLines := linesOfType(lines, "observation")
	if len(obsLines) != 1 || obsLines[0]["status"] != "removed" {
		t.Fatalf("observation lines = %v, want one with status \"removed\"", obsLines)
	}

	result := lines[len(lines)-1]
	removed, _ := result["removed"].([]any)
	if len(removed) != 1 || removed[0] != "network" {
		t.Errorf("result.removed = %v, want [\"network\"]", result["removed"])
	}
}

// TestApplyOutputIncludesVarFileWarning pins that a --var-file WARNING (not
// an error) still reaches --output as a diagnostic line on an otherwise
// successful apply — the same non-obvious case
// TestPlanRendersVarFileWarningsEvenWithoutErrors guards for plan's stderr
// rendering (varfiles_test.go), here for the NDJSON stream: a version of
// renderDiagnostics gated on ds.HasErrors() would drop this line silently.
func TestApplyOutputIncludesVarFileWarning(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  network:
    type: fake.network
    cidr: 10.20.0.0/16
`)
	writeFile(t, dir, "vars.yml", "resources: 3\n")

	outPath := filepath.Join(t.TempDir(), "out.ndjson")
	opts := &GlobalOptions{
		Dir: dir, Parallelism: 4, AutoApprove: true, Output: outPath,
		VarFiles: []string{"vars.yml"},
	}
	cmd := newApplyCommand(opts)
	cmd.SetArgs([]string{"dev"})
	cmd.SetIn(strings.NewReader(""))
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))

	if err := cmd.Execute(); !errors.Is(err, errChanges) {
		t.Fatalf("Execute() = %v, want errChanges — a warning must not fail the apply", err)
	}

	lines := readNDJSON(t, outPath)
	warnings := 0
	for _, d := range linesOfType(lines, "diagnostic") {
		if d["severity"] != "warning" {
			continue
		}
		if s, _ := d["summary"].(string); strings.Contains(s, "resources") {
			warnings++
		}
	}
	if warnings == 0 {
		t.Errorf("the --var-file warning was not written to --output: %v", lines)
	}
}
