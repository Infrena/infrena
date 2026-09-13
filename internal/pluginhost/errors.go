package pluginhost

import (
	"fmt"

	"github.com/infrata/infrata/pkg/semver"
	"strconv"
	"strings"
	"sync"
)

// The errors a user actually meets when a plugin is wrong. Each follows §44:
// what is wrong, where, what was expected, and what to do about it.

// IncompatibleError is a plugin speaking a protocol version this build does not.
type IncompatibleError struct {
	Plugin    string
	Path      string
	Theirs    int
	Supported []int
}

func (e *IncompatibleError) Error() string {
	versions := make([]string, len(e.Supported))
	for i, v := range e.Supported {
		versions[i] = strconv.Itoa(v)
	}
	// Which side to upgrade, said plainly: a user reading "protocol 2 vs 1" has to
	// work out which of two programs is behind, and will guess wrong half the time.
	advice := "Upgrade infrata."
	newest := 0
	for _, v := range e.Supported {
		if v > newest {
			newest = v
		}
	}
	if e.Theirs < newest {
		advice = "Upgrade the plugin."
	}
	return fmt.Sprintf(
		"the %s plugin speaks protocol version %d, which this build of infrata does not understand\n"+
			"  loaded from: %s\n"+
			"  infrata speaks: %s\n"+
			"%s",
		e.Plugin, e.Theirs, e.pathOrUnknown(), strings.Join(versions, ", "), advice)
}

func (e *IncompatibleError) pathOrUnknown() string {
	if e.Path == "" {
		return "(in process)"
	}
	return e.Path
}

// WrongPluginError is a binary that is not the plugin its name says it is.
type WrongPluginError struct {
	Expected, Actual, Path string
}

func (e *WrongPluginError) Error() string {
	return fmt.Sprintf(
		"the binary for plugin %q says it is %q\n"+
			"  loaded from: %s\n"+
			"A renamed or mis-copied binary serves the wrong schemas, and the first sign of that "+
			"is a plan proposing something nobody asked for.\n"+
			"Either rename the binary to infrata-plugin-%s, or write `plugin: %s` in your "+
			"`providers:` block.",
		e.Expected, e.Actual, e.Path, e.Expected, e.Actual)
}

// stderrTail keeps the last few lines a plugin logged, for a crash message.
//
// Bounded, because a plugin that logs enthusiastically before dying would
// otherwise put its whole session into one error. The LAST lines rather than the
// first: what a process says immediately before exiting is what explains it.
type stderrTail struct {
	mu    sync.Mutex
	lines []string
	max   int
}

func newStderrTail(max int) *stderrTail { return &stderrTail{max: max} }

func (t *stderrTail) add(line string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lines = append(t.lines, line)
	if len(t.lines) > t.max {
		t.lines = t.lines[len(t.lines)-t.max:]
	}
}

func (t *stderrTail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.Join(t.lines, "\n")
}

// exitMessage explains a connection that ended, quoting the plugin's own last words.
//
// TWO SEPARATE THINGS happen here, and the comment used to describe only the absence of
// both — it said a plugin dying "reports as EOF", which this function's own first line
// has already made untrue. Corrected 2026-09-13 after infrata-provider-fake's review
// found the stale wording; it had been copied into that repository's authoring guide.
//
// First, io.EOF is replaced. A closed pipe is what the READER saw, not what happened:
// "EOF" is a Go sentinel, and showing it to a user asks them to know that a plugin
// exiting closes its stdout. "the plugin stopped responding" is the same fact in a
// sentence.
//
// Second, the stderr tail is appended. That sentence is still useless on its own — a
// plugin that died of a missing credential has already SAID so, on the stream the host
// was told to treat as a log — and quoting its last words is the difference between an
// error a user can act on and one that only reports that something ended.
func (c *Client) exitMessage(err error) string {
	msg := "the plugin stopped responding"
	if err != nil && err.Error() != "EOF" {
		msg = err.Error()
	}
	if c.stderr == nil {
		return msg
	}
	if tail := c.stderr.String(); tail != "" {
		return msg + "\nIts last output was:\n" + indent(tail)
	}
	return msg
}

func indent(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return strings.Join(lines, "\n")
}

// VersionError is a plugin outside the range a project's `plugins:` block accepts.
type VersionError struct {
	Plugin     string
	Reported   string
	Constraint semver.Constraint
	Path       string
	// Unversioned distinguishes "this plugin does not report a version" from "this
	// version is wrong". The SDK answers 0.0.0 for a plugin that does not implement
	// Version(), and telling its author that 0.0.0 fails `>= 0.3.0` sends them looking
	// for a version they never set.
	Unversioned bool
}

func (e *VersionError) Error() string {
	where := e.Path
	if where == "" {
		where = "(in process)"
	}
	if e.Unversioned {
		return fmt.Sprintf(
			"the %s plugin does not report a version, so it cannot satisfy %s\n"+
				"  loaded from: %s\n"+
				"A plugin reports its version by implementing Version() string; without one there "+
				"is nothing to check a constraint against.",
			e.Plugin, e.Constraint, where)
	}
	return fmt.Sprintf(
		"the %s plugin is version %s, which does not satisfy %s\n"+
			"  loaded from: %s",
		e.Plugin, e.Reported, e.Constraint, where)
}
