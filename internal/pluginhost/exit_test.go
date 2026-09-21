package pluginhost

import (
	"io"
	"strings"
	"testing"
)

// io.EOF is what the reader saw, not what happened, and "EOF" asks a user to know that a
// plugin exiting closes its stdout. This pins both halves of exitMessage: the sentinel is
// replaced, and the plugin's own last words are quoted, because the sentence alone is
// still useless — a plugin that died of a missing credential already said so on stderr.
func TestAPluginThatJustExitsGetsASentenceNotAGoSentinel(t *testing.T) {
	c := &Client{}
	if got := c.exitMessage(io.EOF); strings.Contains(got, "EOF") {
		t.Errorf("exitMessage(io.EOF) = %q; a Go sentinel is not an explanation", got)
	}
	if got := c.exitMessage(io.EOF); !strings.Contains(got, "stopped responding") {
		t.Errorf("exitMessage(io.EOF) = %q, want a sentence", got)
	}

	// A real error is kept — it says more than the generic sentence does.
	if got := c.exitMessage(errorsNew("broken pipe")); !strings.Contains(got, "broken pipe") {
		t.Errorf("exitMessage lost the underlying error: %q", got)
	}

	// And the plugin's last words are quoted, which is what makes it actionable.
	withLog := &Client{stderr: newStderrTail(5)}
	withLog.stderr.add("HCLOUD_TOKEN is not set")
	got := withLog.exitMessage(io.EOF)
	if !strings.Contains(got, "HCLOUD_TOKEN is not set") {
		t.Errorf("exitMessage did not quote the plugin's own last output: %q", got)
	}
	if !strings.Contains(got, "last output") {
		t.Errorf("nothing labels the quoted text as the plugin's: %q", got)
	}
}
