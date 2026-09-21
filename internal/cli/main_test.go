package cli

import (
	"os"
	"testing"
)

// TestMain supplies the in-process fake provider for every test in this package,
// and isolates the suite from the developer's own machine.
//
// A shipped infrena carries no provider: a project installs infrena-plugin-fake
// like any other binary. These tests run commands IN THIS PROCESS, so a binary on
// a search path is not what they need. The fake provider is registered in process
// over pluginhost.InProcess, which applies the same handshake, protocol and trust
// rules a subprocess gets.
//
// One place rather than per test, because the alternative is every test that runs a
// command remembering to set it, and the ones that forgot would fail with "no plugin
// named fake" — an error about infrastructure, for a test about something else.
//
// tests/integration does the opposite deliberately: it builds and runs the REAL
// infrena-plugin-fake, so the path a user actually takes is proved somewhere.
func TestMain(m *testing.M) {
	builtinsFor = fakeDouble
	cleanup := isolateFromTheDeveloper()
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// isolateFromTheDeveloper points HOME at an empty directory for the whole
// package and clears INFRENA_PLUGIN_PATH.
//
// WITHOUT THIS THE SUITE READS THE MACHINE IT RUNS ON. pluginhost.DefaultSearch
// searches os.UserHomeDir()/.local/share/infrena/plugins, so a developer who has
// installed a real plugin — which is to say anyone who uses infrena while working
// on it — fails four tests that have nothing to do with their change:
// "nothing installed" finds something, "one instance per plugin" finds three, and
// an error that should name a missing plugin lists two thousand AWS types instead.
// CI passes because CI installs nothing, which is the worst version of this: the
// suite is green exactly where nobody is looking at it.
//
// The whole package, in TestMain rather than t.Setenv per test, for the same
// reason the fake double is registered here: a test that has to remember is a
// test that will forget. infrena-provider-aws does the same thing for the AWS
// SDK's config files (internal/awstest.Isolate) and for the same reason.
//
// os.Setenv, not t.Setenv, because TestMain has no *testing.T — and because the
// isolation must cover every test in the package rather than one at a time.
func isolateFromTheDeveloper() func() {
	home, err := os.MkdirTemp("", "infrena-test-home")
	if err != nil {
		panic("test isolation: " + err.Error())
	}
	previous := map[string]string{}
	for _, key := range []string{"HOME", "INFRENA_PLUGIN_PATH", "XDG_DATA_HOME"} {
		previous[key] = os.Getenv(key)
	}
	_ = os.Setenv("HOME", home)
	_ = os.Unsetenv("INFRENA_PLUGIN_PATH")
	_ = os.Unsetenv("XDG_DATA_HOME")

	return func() {
		for key, was := range previous {
			if was == "" {
				_ = os.Unsetenv(key)
				continue
			}
			_ = os.Setenv(key, was)
		}
		_ = os.RemoveAll(home)
	}
}
