package pluginhost

import (
	"path/filepath"
	"testing"
)

// TestARelativePluginDirIsResolved.
//
// `--plugin-dir .` used to join to "infrena-plugin-<name>" — a name with no
// separator in it — and os/exec treats such a name as a PATH LOOKUP rather
// than a path. So a binary sitting in the named directory was never run, and
// the error blamed $PATH for a directory the user had given explicitly.
//
// Asserting on the SEARCHED PATHS rather than on a launch, because the two
// have one cause: a path with no separator cannot be exec'd and cannot be
// reported usefully either.
func TestARelativePluginDirIsResolved(t *testing.T) {
	for _, dir := range []string{".", "./plugins", "plugins"} {
		got := SearchOptions{Explicit: []string{dir}}.Dirs()
		if len(got) != 1 {
			t.Fatalf("Dirs() = %v, want one entry", got)
		}
		if !filepath.IsAbs(got[0]) {
			t.Errorf("Dirs() for %q = %q, which is not absolute — joined to a binary name it "+
				"produces a path os/exec looks up in $PATH instead of running", dir, got[0])
		}
	}
}

// TestAnAbsolutePluginDirIsUnchanged, so resolving does not rewrite a path
// somebody gave in full.
func TestAnAbsolutePluginDirIsUnchanged(t *testing.T) {
	want := filepath.Join(string(filepath.Separator), "opt", "infrena", "plugins")
	got := SearchOptions{Explicit: []string{want}}.Dirs()
	if len(got) != 1 || got[0] != want {
		t.Errorf("Dirs() = %v, want [%s]", got, want)
	}
}
