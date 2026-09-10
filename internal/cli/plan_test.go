package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlanProposesCreatesOnFreshProject(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: 10.20.0.0/16
`)
	opts := &GlobalOptions{Dir: dir, Parallelism: 4}
	cmd := newPlanCommand(opts)
	cmd.SetArgs([]string{"dev"})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := cmd.Execute()
	if !errors.Is(err, errChanges) {
		t.Fatalf("Execute() = %v, want errChanges — a fresh project always has changes", err)
	}
	if !strings.Contains(stdout.String(), "test.network.network") {
		t.Errorf("stdout does not mention the proposed resource:\n%s", stdout.String())
	}
}

func TestPlanRendersDiagnosticsToStderrOnInvalidConfig(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  database:
    type: aws.rds
    engine: postgres
`)
	opts := &GlobalOptions{Dir: dir, Parallelism: 4}
	cmd := newPlanCommand(opts)
	cmd.SetArgs([]string{"dev"})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := cmd.Execute()
	if err == nil || errors.Is(err, errChanges) {
		t.Fatalf("Execute() = %v, want a plain error for invalid configuration", err)
	}
	if !strings.Contains(stderr.String(), "aws.rds") {
		t.Errorf("stderr does not name the offending type:\n%s", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("no plan should reach stdout when configuration fails to compile, got:\n%s", stdout.String())
	}
}

func TestPlanNeverWritesStateOrTakesTheLock(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: 10.20.0.0/16
`)
	opts := &GlobalOptions{Dir: dir, Parallelism: 4}
	cmd := newPlanCommand(opts)
	cmd.SetArgs([]string{"dev"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	_ = cmd.Execute()

	if _, err := os.Stat(filepath.Join(dir, ".infra", "state", "dev.json")); !os.IsNotExist(err) {
		t.Error("plan must never write state")
	}
	if _, err := os.Stat(filepath.Join(dir, ".infra", "state", "dev.lock")); !os.IsNotExist(err) {
		t.Error("plan must never take the environment lock")
	}
}

func TestPlanOutputWritesA0600JSONFile(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: 10.20.0.0/16
`)
	outPath := filepath.Join(t.TempDir(), "plan.json")
	opts := &GlobalOptions{Dir: dir, Parallelism: 4, Output: outPath}
	cmd := newPlanCommand(opts)
	cmd.SetArgs([]string{"dev"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	_ = cmd.Execute()

	info, err := os.Stat(outPath)
	if err != nil {
		t.Fatalf("--output did not write a file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("plan file mode = %v, want 0600", info.Mode().Perm())
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading plan file: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("--output file is not valid JSON: %v", err)
	}
}
