package cli

import (
	"os"
	"testing"
)

// TestMain supplies the in-process fake provider for every test in this package.
//
// A shipped infrata carries no provider (§31.1): a project installs
// infrata-plugin-fake like any other binary. These tests run commands IN THIS PROCESS,
// so a binary on a search path is not what they need — §31.1's Testing section says the
// unit and fast suites register the fake provider in process, over pluginhost.InProcess,
// which is the same handshake, protocol and trust rules a subprocess gets.
//
// One place rather than per test, because the alternative is every test that runs a
// command remembering to set it, and the ones that forgot would fail with "no plugin
// named fake" — an error about infrastructure, for a test about something else.
//
// tests/integration does the opposite deliberately: it builds and runs the REAL
// infrata-plugin-fake, so the path a user actually takes is proved somewhere.
func TestMain(m *testing.M) {
	builtinsFor = fakeDouble
	os.Exit(m.Run())
}
