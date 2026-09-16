package cli

import (
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/infrena/infrena/pkg/provider"
)

// PLAN.md §31.3's `infrena plugins list`: what am I actually running, answered
// without a single network request.

func TestPluginsListReportsWhatIsInstalled(t *testing.T) {
	dir := newProjectFixture(t)

	stdout, _, code := runCommand(t, dir, "plugins", "list")

	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{"NAME", "VERSION", "PATH", "fake"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output missing %q:\n%s", want, stdout)
		}
	}
}

// "Nothing installed" is a real answer and must not look like a failure. It is
// also the state a fresh machine is in, which is exactly when a clear message
// matters most.
func TestPluginsListWithNothingInstalledSaysSoAndExitsZero(t *testing.T) {
	dir := newProjectFixture(t)

	stdout, _, code := runCommandWithoutPlugins(t, dir, "plugins", "list")

	if code != ExitOK {
		t.Errorf("exit = %d, want %d", code, ExitOK)
	}
	if !strings.Contains(stdout, "No plugins installed") {
		t.Errorf("output does not say nothing is installed:\n%s", stdout)
	}
	if !strings.Contains(stdout, "plugins install") {
		t.Errorf("output does not name what to do next:\n%s", stdout)
	}
}

// Unit A's rule, which every command obeys.
func TestPluginsListOutputModeLeavesStdoutByteEmpty(t *testing.T) {
	dir := newProjectFixture(t)
	out := filepath.Join(t.TempDir(), "run.ndjson")

	stdout, _, _ := runCommand(t, dir, "plugins", "list", "--output", out)

	if stdout != "" {
		t.Errorf("stdout = %q, want nothing", stdout)
	}
}

// THE HARD RULE. This command must never reach the network, and neither may
// anything it calls.
func TestPluginsListMakesNoNetworkRequest(t *testing.T) {
	dir := newProjectFixture(t)
	// Point every proxy variable at a listener that fails the test if dialled,
	// and unset anything that could bypass it.
	blocked := blockNetwork(t)

	runCommand(t, dir, "plugins", "list")

	if n := blocked.Attempts(); n != 0 {
		t.Errorf("plugins list made %d network attempts, want 0", n)
	}
}

// runCommandWithoutPlugins is runCommand on a machine with no provider
// installed. A shipped infrena carries none (§31.1); this package's TestMain
// injects one for every other test, so removing it is what a fresh machine
// looks like.
func runCommandWithoutPlugins(t *testing.T, dir string, args ...string) (string, string, int) {
	t.Helper()
	previous := builtinsFor
	builtinsFor = func(string) map[string]provider.Plugin { return nil }
	t.Cleanup(func() { builtinsFor = previous })
	return runCommand(t, dir, args...)
}

// networkBlocker counts connections made to the proxy every network-using
// library in this process has been pointed at.
type networkBlocker struct{ attempts atomic.Int64 }

// Attempts is how many connections were made. Zero is the only passing answer.
func (b *networkBlocker) Attempts() int { return int(b.attempts.Load()) }

// blockNetwork points HTTP_PROXY, HTTPS_PROXY and ALL_PROXY at a listener that
// counts whatever dials it, and clears NO_PROXY so nothing can opt out.
//
// A SMOKE ALARM, NOT A PROOF: a caller that dials an address directly, ignoring
// the proxy variables, would slip past it. internal/plugins carries the other
// half — a dependency check that fails if net/http is ever imported below this
// layer at all — and the two catch different mistakes.
func blockNetwork(t *testing.T) *networkBlocker {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	b := &networkBlocker{}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			b.attempts.Add(1)
			_ = conn.Close()
		}
	}()

	proxy := "http://" + ln.Addr().String()
	for _, name := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"} {
		t.Setenv(name, proxy)
	}
	for _, name := range []string{"NO_PROXY", "no_proxy"} {
		t.Setenv(name, "")
	}
	return b
}
