package pluginhost

import (
	"fmt"

	"github.com/infrena/infrena/pkg/semver"
	"strconv"
	"strings"
	"sync"
)

// The errors a user meets when a plugin is wrong. Each says what is wrong,
// where, what was expected, and what to do about it.

// IncompatibleError is a plugin speaking a protocol version this build does not.
type IncompatibleError struct {
	// Plugin is the plugin's name, and Path where its binary was loaded from.
	Plugin string
	Path   string
	// Theirs is the version the plugin speaks; Supported is what this build
	// speaks.
	Theirs    int
	Supported []int
}

// Error names both protocol versions and which side to upgrade.
func (e *IncompatibleError) Error() string {
	versions := make([]string, len(e.Supported))
	for i, v := range e.Supported {
		versions[i] = strconv.Itoa(v)
	}
	// Which side to upgrade, said plainly: a user reading "protocol 2 vs 1" has
	// to work out which of two programs is behind.
	advice := "Upgrade infrena."
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
		"the %s plugin speaks protocol version %d, which this build of infrena does not understand\n"+
			"  loaded from: %s\n"+
			"  infrena speaks: %s\n"+
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
	// Expected is the name asked for, Actual the one the handshake gave, and
	// Path where the binary was loaded from.
	Expected, Actual, Path string
}

// Error names both plugins and offers the two ways to reconcile them.
func (e *WrongPluginError) Error() string {
	return fmt.Sprintf(
		"the binary for plugin %q says it is %q\n"+
			"  loaded from: %s\n"+
			"A renamed or mis-copied binary serves the wrong schemas, and the first sign of that "+
			"is a plan proposing something nobody asked for.\n"+
			"Either rename the binary to infrena-plugin-%s, or write `plugin: %s` in your "+
			"`providers:` block.",
		e.Expected, e.Actual, e.Path, e.Expected, e.Actual)
}

// LockError is a binary that is not the one plugins.lock recorded.
//
// A type rather than a wrapped string, because the caller reporting this has to
// tell it apart from a plugin that is not installed at all: summarising a
// corrupted binary as "the aws plugin is not available" advises the user to
// install something already sitting on their disk.
type LockError struct {
	// Plugin is the plugin's name, and Path the binary that was checked.
	Plugin, Path string
	// Err is the lockfile's own explanation, which already names the plugin,
	// the platform, both checksums and what to do.
	Err error
}

// Error repeats the lockfile's explanation and names the binary that was
// checked.
func (e *LockError) Error() string {
	return fmt.Sprintf("%v\n\nThe binary checked was %s.", e.Err, e.Path)
}

// Unwrap exposes the lockfile's own error.
func (e *LockError) Unwrap() error { return e.Err }

// stderrTail keeps the last few lines a plugin logged, for a crash message.
//
// Bounded, because a plugin that logs enthusiastically before dying would
// otherwise put its whole session into one error. The last lines rather than the
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

// exitMessage explains a connection that ended, quoting the plugin's own last
// words.
//
// io.EOF is replaced, because a closed pipe is what the reader saw rather than
// what happened, and a Go sentinel asks the user to know that a plugin exiting
// closes its stdout. The stderr tail is then appended: a plugin that died of a
// missing credential has already said so on its log, and quoting it is the
// difference between an error a user can act on and one that reports only that
// something ended.
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

// VersionError is a plugin outside the range a project's `plugins:` block
// accepts.
type VersionError struct {
	// Plugin is the plugin's name.
	Plugin string
	// Reported is the version the plugin gave.
	Reported string
	// Constraint is the range the project asked for.
	Constraint semver.Constraint
	// Path is where the binary was loaded from, empty for an in-process plugin.
	Path string
	// Unversioned distinguishes "this plugin does not report a version" from
	// "this version is wrong". The SDK answers 0.0.0 for a plugin that does not
	// implement Version(), and telling its author that 0.0.0 fails `>= 0.3.0`
	// sends them looking for a version they never set.
	Unversioned bool
}

// Error names the version, the constraint, and where the plugin was loaded from.
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
