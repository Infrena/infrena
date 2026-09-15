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
		dir, err := os.MkdirTemp("", "infra-bin-*")
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
			t.Fatalf("running infra %v: %v", args, err)
		}
		code = exitErr.ExitCode()
	}
	return result{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: code}
}

// project writes a project directory containing infra.yml.
func project(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "infra.yml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write infra.yml: %v", err)
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
// Duplicated from internal/state rather than imported, because tests/integration
// deliberately imports no infra package — it shells out to the built binary, which is
// what makes it an integration suite. The cost is this constant, and the check below is
// what keeps it honest.
const stateVersion = 2

// TestTheStateVersionThisSuiteWritesIsCurrent catches the duplication above going stale.
//
// A fixture at an older version is MIGRATED on load, so a stale constant here would turn
// every hand-written fixture into a silently migrated one — which is exactly what
// happened when CurrentVersion moved to 2 and these fixtures still said 1.
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
// A development build is EXEMPT from a project's `infrena:` floor (§61.2), and the
// binary every other test in this file uses is a development build reporting
// 0.0.0-dev. So a test about the floor being enforced must stamp one, or it passes
// against a build that never checks anything — which is the shape of a test that
// cannot fail.
//
// The ldflags path is duplicated from .github/workflows/release.yml deliberately:
// if they diverge, the release binaries report 0.0.0-dev and this test still passes.
// What catches that is the workflow's own verification step, which runs the built
// binary and reads its version back.
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
// event, the artifact riding on the `plan` line so a frontend tails one format
// for every command (spec 2.4). Decoding the whole file as a document fails
// with "invalid character '{' after top-level value", and — worse — a test that
// only greps the raw bytes still matches text from the meta line and the
// stream's own envelope, so it inspects the wrong level and reports a confident
// wrong answer.
//
// Every test in this package that wants the artifact goes through here. Six
// inline parsers is how the next envelope change breaks five of them and
// silently degrades the sixth.
//
// The returned bytes are compacted, so a substring assertion against them is
// about the artifact's content rather than about whether the producer happens
// to indent it.
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
