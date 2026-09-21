package test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/infrena/infrena/pkg/provider"
	"github.com/infrena/infrena/pkg/value"
)

// cloudPathOf builds an instance and reports which file it opens, which is the only
// thing this plugin's configuration decides.
func cloudPathOf(t *testing.T, dir, instance string, config map[string]value.Value) string {
	t.Helper()
	p, err := NewPlugin(dir).New(provider.Config{Instance: instance, Values: config})
	if err != nil {
		t.Fatalf("New(%q): %v", instance, err)
	}
	return p.(*Provider).cloudPath
}

// Against the project directory, not the process's working directory, which --chdir
// moves out from under us.
func TestAnExplicitCloudPathIsResolvedAgainstTheProjectDirectory(t *testing.T) {
	dir := t.TempDir()
	got := cloudPathOf(t, dir, "main", map[string]value.Value{
		"cloud": value.String("clouds/one.json", value.SourceVariable),
	})
	if want := filepath.Join(dir, "clouds", "one.json"); got != want {
		t.Errorf("cloudPath = %q, want %q", got, want)
	}
}

// The default is the behaviour: `cloud:` is this provider's stand-in for an account,
// so two instances sharing one file would be the same mistake as two AWS instances
// sharing one set of credentials. Asserted here as well as end to end, because
// collapsing the two paths into one breaks nothing else in the suite.
func TestTwoInstancesNamingNoCloudGetDifferentFiles(t *testing.T) {
	dir := t.TempDir()
	main := cloudPathOf(t, dir, "main", nil)
	acct2 := cloudPathOf(t, dir, "acct2", nil)
	if main == acct2 {
		t.Fatalf("both instances opened %q, so they are two names for one account", main)
	}
	for _, tc := range []struct{ instance, path string }{{"main", main}, {"acct2", acct2}} {
		if !strings.Contains(tc.path, tc.instance) {
			t.Errorf("%s opened %q, which does not identify it", tc.instance, tc.path)
		}
	}
}

// The implicit instance is called `fake` rather than `test`, and its pairing with the
// state migration is the point. A project written before the rename has
// `.infrena/fake-cloud.json` on disk and state recording `provider: test`; the
// migration in internal/state/migrations.go rewrites that to `fake`, which is what
// makes the existing cloud file still be the one found. Special-case `test` here
// instead and every migrated project looks at `fake-cloud-fake.json` — an empty cloud,
// so the first plan proposes recreating everything that already exists.
func TestTheImplicitInstanceKeepsTheHistoricalPath(t *testing.T) {
	dir := t.TempDir()
	if got, want := cloudPathOf(t, dir, "fake", nil), filepath.Join(dir, DefaultCloudPath); got != want {
		t.Errorf("cloudPath = %q, want the historical %q", got, want)
	}
	// And an instance a user named `test` is ordinary: it gets its own file, because
	// after the rename `test` carries no special meaning to this plugin.
	if got := cloudPathOf(t, dir, "test", nil); got == filepath.Join(dir, DefaultCloudPath) {
		t.Error("an instance named `test` still claims the implicit instance's cloud file")
	}
}

// Fail closed. A misspelled `clowd:` quietly ignored means an instance silently
// sharing another's account, and the first sign of it is a plan proposing to destroy
// resources somebody else owns.
func TestAnUnknownConfigurationKeyIsRefused(t *testing.T) {
	_, err := NewPlugin(t.TempDir()).New(provider.Config{Instance: "main", Values: map[string]value.Value{
		"clowd": value.String("other.json", value.SourceExplicit),
	}})
	if err == nil {
		t.Fatal("an unknown configuration key must be refused")
	}
	for _, want := range []string{"clowd", "cloud"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q — the reader needs the key they wrote and "+
				"the one they meant: %v", want, err)
		}
	}
}

// Refused rather than silently formatted into a path.
func TestACloudPathOfTheWrongKindIsRefused(t *testing.T) {
	_, err := NewPlugin(t.TempDir()).New(provider.Config{Instance: "main", Values: map[string]value.Value{
		"cloud": value.Int(7, value.SourceExplicit),
	}})
	if err == nil {
		t.Fatal("`cloud: 7` must be refused")
	}
	if !strings.Contains(err.Error(), "cloud") {
		t.Errorf("the error does not name the key: %v", err)
	}
}

// filepath.Join with "" silently yields the project directory, so the instance would
// open a directory and report an unhelpful I/O error much later.
func TestAnEmptyCloudPathIsRefused(t *testing.T) {
	_, err := NewPlugin(t.TempDir()).New(provider.Config{Instance: "main", Values: map[string]value.Value{
		"cloud": value.String("", value.SourceExplicit),
	}})
	if err == nil {
		t.Fatal("an empty `cloud:` must be refused")
	}
	// And it says what omitting the key would have given, which is what the user
	// probably wanted.
	if !strings.Contains(err.Error(), "fake-cloud-main.json") {
		t.Errorf("the error does not name the default it would otherwise have used: %v", err)
	}
}
