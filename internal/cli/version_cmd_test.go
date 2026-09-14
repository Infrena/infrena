package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infrena/infrena/internal/planner"
	"github.com/infrena/infrena/internal/state"
	"github.com/infrena/infrena/internal/version"
	"github.com/infrena/infrena/pkg/pluginproto"
	"github.com/infrena/infrena/pkg/report"
)

// PLAN.md §61.4. The formats matter more than the version number: "which version do I
// need" is otherwise answerable only by reading source.

// TestVersionReportsEveryFormatThisBuildSpeaks, by reading each from the package that
// owns it. A format added without this list following is the failure — the list is the
// only thing that makes the set discoverable.
func TestVersionReportsEveryFormatThisBuildSpeaks(t *testing.T) {
	out, err := versionOut(t)
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	for _, name := range []string{"state", "plugin protocol", "plan artifact", "report"} {
		if !strings.Contains(out, name) {
			t.Errorf("the output does not mention the %s format:\n%s", name, out)
		}
	}
	if !strings.Contains(out, version.Version()) {
		t.Errorf("the output does not state the build's own version:\n%s", out)
	}
}

// TestTheReportedFormatVersionsAreTheRealOnes. A hand-written list would drift, and
// drift here is invisible: the numbers would still look plausible.
func TestTheReportedFormatVersionsAreTheRealOnes(t *testing.T) {
	got := map[string][]int{}
	for _, f := range formats() {
		got[f.Name] = f.Versions
	}
	for name, want := range map[string][]int{
		"state":         {state.CurrentVersion},
		"plan artifact": {planner.PlanVersion},
		"report":        {report.Version},
	} {
		if len(got[name]) != 1 || got[name][0] != want[0] {
			t.Errorf("%s = %v, want %v", name, got[name], want)
		}
	}
	// The plugin protocol is a SET, because the handshake negotiates: a build can
	// keep talking to plugins written against an older protocol.
	if len(got["plugin protocol"]) != len(pluginproto.Supported) {
		t.Errorf("plugin protocol = %v, want every supported version %v",
			got["plugin protocol"], pluginproto.Supported)
	}
}

// TestVersionCopiesTheSupportedSet. Returning the package's own slice would let a
// caller mutate what every later handshake compares against.
func TestVersionCopiesTheSupportedSet(t *testing.T) {
	if len(pluginproto.Supported) == 0 {
		t.Skip("nothing to alias")
	}
	f := formats()
	var protocol []int
	for _, entry := range f {
		if entry.Name == "plugin protocol" {
			protocol = entry.Versions
		}
	}
	protocol[0] = 9999
	if pluginproto.Supported[0] == 9999 {
		t.Error("formats() aliases pluginproto.Supported, so a caller can change which " +
			"protocol versions this build claims to speak")
	}
}

// TestVersionJSONIsOneObjectACIJobCanAssertOn.
func TestVersionJSONIsOneObjectACIJobCanAssertOn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v.json")
	if _, err := versionOutTo(t, path); err != nil {
		t.Fatalf("version --output: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the artifact: %v", err)
	}
	var info version.Info
	if err := json.Unmarshal(body, &info); err != nil {
		t.Fatalf("the artifact is not one JSON object: %v\n%s", err, body)
	}
	if info.Version == "" || info.Go == "" || info.Platform == "" {
		t.Errorf("incomplete: %+v", info)
	}
	if len(info.Formats) != len(formats()) {
		t.Errorf("got %d formats, want %d", len(info.Formats), len(formats()))
	}
	// 0600, matching every other artifact this tool writes.
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
}

func versionOut(t *testing.T) (string, error) {
	t.Helper()
	return runVersion(t, nil)
}

func versionOutTo(t *testing.T, path string) (string, error) {
	t.Helper()
	return runVersion(t, []string{"--output", path})
}

func runVersion(t *testing.T, extra []string) (string, error) {
	t.Helper()
	cmd := NewRootCommand()
	var sb strings.Builder
	cmd.SetOut(&sb)
	cmd.SetErr(&sb)
	cmd.SetArgs(append([]string{"version"}, extra...))
	err := cmd.Execute()
	return sb.String(), err
}
