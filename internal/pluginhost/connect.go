package pluginhost

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/infrena/infrena/pkg/pluginproto"
	"github.com/infrena/infrena/pkg/pluginsdk"
	"github.com/infrena/infrena/pkg/provider"
)

// One code path, two ways to connect it: InProcess and Launch differ only in
// what the pipes are attached to. Everything past the handshake — framing, the
// trust rules, cancellation, crash handling — is the same code. There is
// deliberately no third way in, and no way to register a provider that skips the
// adapter, because a second path is where the trust rules would stop holding.

// InProcess runs a plugin over an in-memory pipe, with no process involved.
//
// Every call is still encoded, decoded and passed through the trust rules, so a
// misbehaving plugin can be written in Go, in a test file, and still be refused
// by exactly the code that would refuse a real binary.
func InProcess(ctx context.Context, p provider.Plugin, dir string) (*Plugin, error) {
	hostReader, pluginWriter := io.Pipe()
	pluginReader, hostWriter := io.Pipe()

	served := make(chan struct{})
	go func() {
		defer close(served)
		// The SDK's own Serve, not a reimplementation of it: a second
		// implementation would drift, and the drift would be invisible because
		// each side would still agree with itself.
		_ = pluginsdk.Serve(p, pluginReader, pluginWriter)
		_ = pluginWriter.Close()
	}()

	c := &Client{
		w:       hostWriter,
		pending: map[uint64]chan pluginproto.Response{},
		done:    make(chan struct{}),
		stop: func() {
			_ = pluginReader.Close()
			<-served
		},
	}
	return finish(ctx, c, hostReader, p.Name(), dir)
}

// Launch starts a plugin binary and connects to it.
func Launch(ctx context.Context, name, path, dir string, verbose io.Writer) (*Plugin, error) {
	cookie, err := cookie()
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(path)
	cmd.Env = append(os.Environ(), pluginproto.CookieEnv+"="+cookie)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting plugin %s at %s: %w", name, path, err)
	}

	tail := newStderrTail(20)
	go forwardStderr(stderr, name, tail, verbose)

	c := &Client{
		path:    path,
		w:       stdin,
		pending: map[uint64]chan pluginproto.Response{},
		done:    make(chan struct{}),
		stderr:  tail,
		stop: func() {
			// stdin is already closed by Client.Close, which is the plugin's cue
			// to exit. Wait for it rather than killing: a plugin mid-Create that
			// is killed can orphan a resource it really created.
			_ = cmd.Wait()
		},
	}
	p, err := finish(ctx, c, stdout, name, dir)
	if err != nil {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, err
	}
	return p, nil
}

// finish completes a connection: handshake, then schemas.
//
// Schemas are fetched at connection time because every command needs them —
// `validate` and `explain` as much as `apply` — and validating them once, on
// load, is what lets every later lookup assume they are sound.
func finish(ctx context.Context, c *Client, r io.Reader, name, dir string) (*Plugin, error) {
	if err := c.start(r, name); err != nil {
		if c.stop != nil {
			c.stop()
		}
		return nil, err
	}
	p := &Plugin{client: c, dir: dir}
	if err := p.loadSchemas(ctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	return p, nil
}

// forwardStderr prefixes the plugin's log with its name.
//
// stderr is the plugin's log and stdout is the protocol. Shown only under
// --verbose, but collected always, because the lines that explain a crash are
// written before anyone knew to ask for them.
func forwardStderr(r io.Reader, name string, tail *stderrTail, verbose io.Writer) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 8*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		tail.add(line)
		if verbose != nil {
			fmt.Fprintf(verbose, "[%s] %s\n", name, line)
		}
	}
}

// BinaryName is the file a plugin is looked up as.
func BinaryName(plugin string) string {
	if os.PathSeparator == '\\' {
		return "infrena-plugin-" + plugin + ".exe"
	}
	return "infrena-plugin-" + plugin
}

// Find locates a plugin binary, first match wins, and reports every place it
// looked so a missing plugin says where to put it.
func Find(plugin string, opts SearchOptions) (string, []string, error) {
	name := BinaryName(plugin)
	var searched []string

	for _, dir := range opts.dirs() {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, name)
		searched = append(searched, candidate)
		if isExecutable(candidate) {
			return candidate, searched, nil
		}
	}
	if path, err := exec.LookPath(name); err == nil {
		return path, append(searched, path), nil
	}
	searched = append(searched, "$PATH")
	return "", searched, &NotFoundError{Plugin: plugin, Binary: name, Searched: searched}
}

// SearchOptions is where to look, in order.
type SearchOptions struct {
	// Explicit is --plugin-dir, then INFRENA_PLUGIN_PATH. Highest precedence so a
	// developer can point at a freshly built binary without installing it.
	Explicit []string
	// ProjectDir contributes <project>/.infrena/plugins, so a project can pin its
	// own plugins alongside its state.
	ProjectDir string
	// HomeDir contributes the per-user install location, which is where
	// `infrena plugins install` will write.
	HomeDir string
}

// Dirs is where to look, in order.
//
// Exported because a state backend lives in the same directories:
// internal/backendhost searches exactly these for a binary whose only difference
// is its name. A copied second list is how a backend ends up findable somewhere
// a provider is not.
func (o SearchOptions) Dirs() []string { return o.dirs() }

func (o SearchOptions) dirs() []string {
	out := make([]string, 0, len(o.Explicit)+2)
	for _, dir := range o.Explicit {
		out = append(out, absolute(dir))
	}
	if o.ProjectDir != "" {
		out = append(out, absolute(filepath.Join(o.ProjectDir, ".infrena", "plugins")))
	}
	if o.HomeDir != "" {
		out = append(out, filepath.Join(o.HomeDir, ".local", "share", "infrena", "plugins"))
	}
	return out
}

// absolute resolves a search directory against the working directory.
//
// Not cosmetic. `--plugin-dir .` joins to a name with no separator in it, which
// os/exec treats as a $PATH lookup rather than a path — so the binary sitting in
// the named directory would never be run, and the error would blame $PATH. It
// also makes the "looked in" list say where it actually looked. A failure leaves
// the path as written, since an unreadable working directory is no reason to
// refuse a search that might still succeed.
func absolute(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	return abs
}

// NotFoundError is a plugin whose binary is nowhere to be found.
type NotFoundError struct {
	// Plugin is the name configuration asked for, and Binary the file that
	// name resolves to.
	Plugin, Binary string
	// Searched is every path that was tried, in order.
	Searched []string
}

// Error names the plugin, the binary, and everywhere it was looked for.
func (e *NotFoundError) Error() string {
	return fmt.Sprintf(
		"no binary for plugin %q\n"+
			"  `providers:` names plugin %q, so infrena looked for %s in:\n%s\n"+
			"Put the binary in one of those directories, or pass --plugin-dir.",
		e.Plugin, e.Plugin, e.Binary, indent(strings.Join(e.Searched, "\n")))
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	// On Windows there is no execute bit; the extension is the contract.
	if os.PathSeparator == '\\' {
		return true
	}
	return info.Mode()&0o111 != 0
}
