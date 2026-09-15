package cli

import (
	"bytes"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
)

// commandWithBuffers builds a bare command wired to buffers, so a test can
// read exactly what reached stdout. The commands in this package all take
// their writers from cobra (cmd.OutOrStdout()), which is the seam every
// other test in this package already uses; this only gives that seam a name
// for the tests that care about the writer rather than about a command.
func commandWithBuffers(t *testing.T) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	cmd := &cobra.Command{Use: "test"}
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	return cmd, &stdout
}

func TestOpenRunSilencesStdoutWhenOutputIsSet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "run.ndjson")
	cmd, stdout := commandWithBuffers(t)

	ro, closeRun, err := openRun(cmd, &GlobalOptions{Output: path}, "apply", "dev")
	if err != nil {
		t.Fatal(err)
	}
	defer closeRun()

	fmt.Fprint(ro.Out(), "this must not reach the terminal")
	if stdout.Len() != 0 {
		t.Errorf("stdout got %q, want nothing", stdout.String())
	}
	if ro.Report() == nil {
		t.Error("Report() is nil with --output set")
	}
}

func TestOpenRunWritesToStdoutWhenOutputIsAbsent(t *testing.T) {
	cmd, stdout := commandWithBuffers(t)

	ro, closeRun, err := openRun(cmd, &GlobalOptions{}, "apply", "dev")
	if err != nil {
		t.Fatal(err)
	}
	defer closeRun()

	fmt.Fprint(ro.Out(), "hello")
	if stdout.String() != "hello" {
		t.Errorf("stdout = %q, want hello", stdout.String())
	}
	if ro.Report() != nil {
		t.Error("Report() is non-nil with no --output")
	}
}
