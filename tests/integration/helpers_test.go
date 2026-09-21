package integration

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	buildOnce sync.Once
	binPath   string
	buildErr  error
)

// binary builds the CLI once per test run and returns its path.
func binary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "infrena-bin-*")
		if err != nil {
			buildErr = err
			return
		}
		binPath = filepath.Join(dir, "infrena")
		cmd := exec.Command("go", "build", "-o", binPath, "github.com/infrena/infrena/cmd/infrena")
		cmd.Dir = repoRoot(t)
		out, err := cmd.CombinedOutput()
		if err != nil {
			buildErr = err
			binPath = string(out)
		}
	})
	if buildErr != nil {
		t.Fatalf("building the CLI: %v\n%s", buildErr, binPath)
	}
	return binPath
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	return filepath.Dir(filepath.Dir(wd)) // tests/integration → repo root
}

type result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

func (r result) combined() string { return r.Stdout + r.Stderr }

// run executes the CLI inside dir.
func run(t *testing.T, dir string, args ...string) result {
	t.Helper()
	// EVERY command gets --plugin-dir. The binary under test carries no provider, so
	// without it nothing loads and every test fails with "no binary for plugin fake" —
	// which is true, and says nothing about the test that was running.
	full := append([]string{"--chdir", dir, "--plugin-dir", fakePluginDir(t)}, args...)
	cmd := exec.Command(binary(t), full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("running infrena %v: %v", args, err)
		}
		code = exitErr.ExitCode()
	}
	return result{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: code}
}

// project writes a project directory containing infrena.yml.
func project(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "infrena.yml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write infrena.yml: %v", err)
	}
	return dir
}

func requireContains(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("output does not contain %q\n---\n%s", needle, haystack)
	}
}

// stateVersion is the state schema version this build writes.
//
// Duplicated from internal/state rather than imported, because this suite deliberately
// imports no infrena package — it shells out to the built binary, which is what makes it
// an integration suite. The test below is what keeps the copy honest.
const stateVersion = 2

// TestTheStateVersionThisSuiteWritesIsCurrent catches the duplication above going stale.
//
// A fixture at an older version is MIGRATED on load, so a stale constant here would turn
// every hand-written fixture in this suite into a silently migrated one.
func TestTheStateVersionThisSuiteWritesIsCurrent(t *testing.T) {
	out := run(t, t.TempDir(), "version", "--output", filepath.Join(t.TempDir(), "v.json"))
	if out.ExitCode != 0 {
		t.Fatalf("version exit = %d:\n%s", out.ExitCode, out.combined())
	}
	// Read it back from the binary itself, which is the only source of truth this suite
	// is allowed to consult.
	reported := reportedStateVersion(t)
	if reported != stateVersion {
		t.Fatalf("this suite writes state version %d, but the binary writes %d — every "+
			"hand-written fixture here is being migrated on load", stateVersion, reported)
	}
}

// reportedStateVersion asks the built binary which state version it writes.
func reportedStateVersion(t *testing.T) int {
	t.Helper()
	path := filepath.Join(t.TempDir(), "v.json")
	if r := run(t, t.TempDir(), "version", "--output", path); r.ExitCode != 0 {
		t.Fatalf("version --output: %s", r.combined())
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the version artifact: %v", err)
	}
	var info struct {
		Formats []struct {
			Name     string `json:"name"`
			Versions []int  `json:"versions"`
		} `json:"formats"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		t.Fatalf("decoding the version artifact: %v", err)
	}
	for _, f := range info.Formats {
		if f.Name == "state" && len(f.Versions) == 1 {
			return f.Versions[0]
		}
	}
	t.Fatalf("the version artifact reports no single state format version: %s", body)
	return 0
}

// stampedBinary builds the CLI with a version stamped in, the way the release
// workflow does.
//
// A development build is EXEMPT from a project's `infrena:` floor, and the binary
// every other test here uses is a development build reporting 0.0.0-dev. A test
// about the floor being enforced must stamp a version or it cannot fail.
//
// The ldflags path is duplicated from the release workflow. If the two diverge the
// release binaries report 0.0.0-dev and this still passes; the workflow's own
// verification step is what catches that.
func stampedBinary(t *testing.T, version string) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "infrena")
	cmd := exec.Command("go", "build",
		"-ldflags", "-X github.com/infrena/infrena/internal/version.version="+version,
		"-o", bin, "github.com/infrena/infrena/cmd/infrena")
	cmd.Dir = repoRoot(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building a stamped CLI: %v\n%s", err, out)
	}
	return bin
}

// runBinary is run for a binary other than the shared one.
func runBinary(t *testing.T, bin, dir string, args ...string) result {
	t.Helper()
	full := append([]string{"--chdir", dir, "--plugin-dir", fakePluginDir(t)}, args...)
	cmd := exec.Command(bin, full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	code := 0
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("running %s %v: %v", bin, args, err)
		}
		code = exitErr.ExitCode()
	}
	return result{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: code}
}

// readPlanArtifactBytes reads a file written by `infrena plan --output` and
// returns the plan artifact itself.
//
// That file is a REPORT STREAM, not a bare JSON document: one NDJSON line per
// event, with the artifact riding on the `plan` line so a frontend tails one
// format for every command. Decoding the whole file as a document fails, and a
// test that greps the raw bytes instead matches text from the meta line and the
// envelope too, so it inspects the wrong level and answers confidently wrong.
//
// Every test here that wants the artifact goes through this one parser, so an
// envelope change breaks one place rather than degrading several separately.
//
// The returned bytes are compacted, so a substring assertion against them is
// about the artifact's content rather than about how the producer indents it.
func readPlanArtifactBytes(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the plan report at %s: %v", path, err)
	}
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var pl struct {
			Type string          `json:"type"`
			Plan json.RawMessage `json:"plan"`
		}
		if err := json.Unmarshal(line, &pl); err != nil || pl.Type != "plan" {
			continue
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, pl.Plan); err != nil {
			t.Fatalf("the plan line in %s does not carry valid JSON: %v", path, err)
		}
		return compact.Bytes()
	}
	t.Fatalf("%s carries no plan line:\n%s", path, data)
	return nil
}

// applyEventWindow returns the interval an `apply --output` run actually spent
// working: the timestamp of its first `started` event to that of its last
// `succeeded` one.
//
// It exists so that a test can assert two runs OVERLAPPED rather than infer it
// from how long they took together. Wall-clock totals cannot tell a serialised
// run on a fast machine from a concurrent one on a slow machine; two absolute
// intervals either intersect or they do not, whatever the machine was doing.
//
// It reads the report stream rather than stdout because `--output` silences
// stdout by design, and because the human rendering never prints these
// timestamps at all.
func applyEventWindow(t *testing.T, label, path string) (first, last time.Time) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the %s apply report at %s: %v", label, path, err)
	}
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var e struct {
			Type  string    `json:"type"`
			Event string    `json:"event"`
			At    time.Time `json:"at"`
		}
		if err := json.Unmarshal(line, &e); err != nil || e.Type != "event" {
			continue
		}
		switch e.Event {
		case "started":
			if first.IsZero() || e.At.Before(first) {
				first = e.At
			}
		case "succeeded":
			if last.IsZero() || e.At.After(last) {
				last = e.At
			}
		}
	}
	if first.IsZero() || last.IsZero() {
		t.Fatalf("the %s apply report at %s has no started/succeeded event pair, so it did no work:\n%s",
			label, path, data)
	}
	return first, last
}
