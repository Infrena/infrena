package pluginhost

import (
	"path/filepath"
	"testing"
)

// `--plugin-dir .` joined to "infrena-plugin-<name>" yields a name with no
// separator in it, and os/exec treats such a name as a PATH lookup rather than
// a path: the binary sitting in the named directory is never run, and the error
// blames $PATH for a directory the user gave explicitly.
//
// Asserting on the searched paths rather than on a launch, because the two have
// one cause: a path with no separator cannot be exec'd and cannot be reported
// usefully either.
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

// Resolving must not rewrite a path somebody gave in full.
func TestAnAbsolutePluginDirIsUnchanged(t *testing.T) {
	want := filepath.Join(string(filepath.Separator), "opt", "infrena", "plugins")
	got := SearchOptions{Explicit: []string{want}}.Dirs()
	if len(got) != 1 || got[0] != want {
		t.Errorf("Dirs() = %v, want [%s]", got, want)
	}
}
