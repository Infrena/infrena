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
		binPath = filepath.Join(dir, "infrata")
		cmd := exec.Command("go", "build", "-o", binPath, "github.com/infrata/infrata/cmd/infrata")
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
	cmd := exec.Command(binary(t), append([]string{"--chdir", dir}, args...)...)
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
