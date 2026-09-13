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

	"github.com/infrata/infrata/pkg/pluginproto"
	"github.com/infrata/infrata/pkg/pluginsdk"
	"github.com/infrata/infrata/pkg/provider"
)

// ONE CODE PATH, TWO WAYS TO CONNECT IT.
//
// InProcess and Launch differ only in what the pipes are attached to. Everything
// past the handshake — framing, the trust rules, cancellation, crash handling — is
// the same code, which is what keeps the rules that make a plugin safe from
// quietly not applying to the fake provider the whole suite runs against.
//
// There is deliberately no third way in, and no way to register a provider that
// skips the adapter. A second path is where the trust rules would stop holding.

// InProcess runs a plugin over an in-memory pipe, with no process involved.
//
// Every call is still encoded, decoded and passed through the trust rules. This is
// what unit tests and the fast integration suite use, so a misbehaving plugin can
// be written in Go, in a test file, and still be refused by exactly the code that
// would refuse a real binary.
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
// Schemas are fetched HERE, at connection time, because every command needs them —
// `validate` and `explain` as much as `apply` — and because validating them once,
// on load, is what lets every later lookup assume they are sound.
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
// stderr is the plugin's log and stdout is the protocol; this is the half of that
// split a user sees. Shown only under --verbose, but COLLECTED always, because the
// lines that explain a crash are written before anyone knew to ask for them.
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
		return "infrata-plugin-" + plugin + ".exe"
	}
	return "infrata-plugin-" + plugin
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
	// Explicit is --plugin-dir, then INFRATA_PLUGIN_PATH. Highest precedence so a
	// developer can point at a freshly built binary without installing it.
	Explicit []string
	// ProjectDir contributes <project>/.infra/plugins, so a project can pin its
	// own plugins alongside its state.
	ProjectDir string
	// HomeDir contributes the per-user install location, which is where
	// `infrata plugins install` will write.
	HomeDir string
}

func (o SearchOptions) dirs() []string {
	out := append([]string{}, o.Explicit...)
	if o.ProjectDir != "" {
		out = append(out, filepath.Join(o.ProjectDir, ".infra", "plugins"))
	}
	if o.HomeDir != "" {
		out = append(out, filepath.Join(o.HomeDir, ".local", "share", "infrata", "plugins"))
	}
	return out
}

// NotFoundError is a plugin whose binary is nowhere to be found.
type NotFoundError struct {
	Plugin, Binary string
	Searched       []string
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf(
		"no binary for plugin %q\n"+
			"  `providers:` names plugin %q, so infrata looked for %s in:\n%s\n"+
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
