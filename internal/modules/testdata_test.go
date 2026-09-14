package modules

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infrena/infrena/internal/config"
	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/internal/modules/source"
)

// writeTree writes a fixture project under dir, creating parent directories.
// Keys are slash-separated paths relative to dir.
func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// fixture writes a project and runs stages 1 and 2 over it, FAILING if either
// rejects it.
//
// That check is the point of the helper, not a convenience. M4 shipped a test
// whose fixture was rejected by an earlier stage, so the stage-5 guard it named
// was never reached and deleting that guard changed nothing. A fixture that
// does not decode cannot pin anything in this package.
func fixture(t *testing.T, files map[string]string) (*config.ProjectDecl, string) {
	t.Helper()
	dir := t.TempDir()
	writeTree(t, dir, files)

	loaded, err := config.Load(dir)
	if err != nil {
		t.Fatalf("stage 1 rejected the fixture, so no stage-5 guard is reached: %v", err)
	}
	decl, ds := config.Decode(loaded)
	if ds.HasErrors() {
		t.Fatalf("stage 2 rejected the fixture, so no stage-5 guard is reached: %+v", ds)
	}
	return decl, dir
}

// paths is the Resolver every fixture here uses: it joins a local path against
// the base directory and does nothing else.
//
// Injecting it is what keeps stage 5 testable with no network and no cache
// directory. It also makes a fixture that reached for a remote source fail
// loudly rather than quietly fetching — no test in this package should ever
// need one, because Author D's Tasks 11-14 own that behaviour and test it
// against the real cache.
type paths struct{}

func (paths) Resolve(s source.Source, baseDir string) (source.Resolution, diag.Diagnostics) {
	var ds diag.Diagnostics
	if s.Kind != source.KindPath {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "this test resolver handles local paths only, got " + s.String(),
			Origin:   s.Origin,
		})
		return source.Resolution{}, ds
	}
	// No Commit: a local path has no revision to pin, which is why 10a requires
	// a pin on remote sources and not on these.
	return source.Resolution{Dir: filepath.Join(baseDir, s.Location)}, ds
}

// summaries collects every diagnostic summary, for assertions that one
// particular message is ABSENT.
func summaries(ds diag.Diagnostics) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.Summary)
	}
	return out
}

// hasFragment reports whether any diagnostic's Summary or Detail contains want.
func hasFragment(ds diag.Diagnostics, want string) bool {
	for _, d := range ds {
		if strings.Contains(d.Summary, want) || strings.Contains(d.Detail, want) {
			return true
		}
	}
	return false
}
