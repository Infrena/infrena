// Package backendhost is infrena's side of the state backend protocol: it
// finds a backend binary, launches it, and presents it as an ordinary
// state.Backend so that nothing upstream learns state became remote.
//
// It is internal/pluginhost's shape with the provider parts taken out, and the
// places the two differ each say why. The largest difference: a backend serves
// one request at a time, so there is no reader goroutine and no map of pending
// calls — a call writes a line and reads the next one, under a mutex that holds
// the host to the same rule.
//
// The error types are the point of this package. A lock conflict has to arrive
// as state.ErrLocked, because every caller tests for it with errors.Is and would
// silently stop working the day state went remote; and a backend that dies
// mid-run has to produce an error naming the backend, because the executor
// writes state once per operation and that write is where a dead backend is met.
package backendhost

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/infrena/infrena/internal/pluginhost"
	"github.com/infrena/infrena/internal/plugins"
	"github.com/infrena/infrena/internal/state"
	"github.com/infrena/infrena/pkg/backend"
	"github.com/infrena/infrena/pkg/backendproto"
)

// Open launches the named backend and returns it as an ordinary state.Backend,
// together with the closer that shuts it down.
//
// The closer is returned rather than hidden behind a finaliser because the
// caller owns the lifetime: a command holds one backend for its whole run, and
// a process left behind after a failed apply is a process holding a lock.
//
// projectDir is where plugins.lock is read from. It is a parameter rather than
// something this package works out, because a backend binary has to be verified
// against that lock before it runs: a backend reads and writes the whole of a
// project's state, every secret recorded in it included.
//
// config is the project's `backend:` block minus `plugin:`, passed through
// untouched. Configure is sent even when it is empty, so a backend has one place
// to open a connection on startup, and can refuse keys it cannot read there
// rather than at the first write.
func Open(ctx context.Context, name, projectDir string, dirs []string, config map[string]any) (state.Backend, func() error, error) {
	path, err := find(name, dirs)
	if err != nil {
		return nil, nil, err
	}

	// Before the process starts, because after it has started is after its
	// code has run.
	if err := checkLock(name, projectDir, path); err != nil {
		return nil, nil, err
	}

	c, err := launch(name, path)
	if err != nil {
		return nil, nil, err
	}
	if err := c.call(ctx, backendproto.MethodConfigure, backendproto.ConfigureParams{Config: config}, nil); err != nil {
		_ = c.close()
		return nil, nil, err
	}
	return &hosted{client: c}, c.close, nil
}

// Verify resolves a backend's binary and checks it against plugins.lock, and
// stops there. It starts no process and contacts nothing.
//
// It is Open without the last two steps, written as a prefix of it rather than
// as a second lookup, so the two cannot drift about where a backend lives or how
// a lockfile entry is read.
//
// It exists for `validate`, which would otherwise pass a project whose
// `backend:` names a plugin that is not installed. Resolving a binary is a
// filesystem lookup, so it stays inside validate's promise not to contact
// providers. The block's contents are ValidateConfig's job.
func Verify(name, projectDir string, dirs []string) error {
	path, err := find(name, dirs)
	if err != nil {
		return err
	}
	return checkLock(name, projectDir, path)
}

// ValidateConfig asks a backend whether it could read this `backend:` block,
// without letting it contact anything.
//
// It closes the hole Verify leaves. Only the backend understands its own
// configuration, and the natural place to check it — Configure — can mean a
// round trip to the store, which `validate` promises not to make. So this does
// start the process, unlike Verify, but the method's contract is offline-only.
//
// It is silent on a backend that cannot answer: a protocol older than the
// method, or one that omits the optional Validator and reports KindUnsupported.
// No backend author is obliged to implement a method to keep working.
func ValidateConfig(ctx context.Context, name, projectDir string, dirs []string, config map[string]any) error {
	path, err := find(name, dirs)
	if err != nil {
		return err
	}
	if err := checkLock(name, projectDir, path); err != nil {
		return err
	}

	c, err := launch(name, path)
	if err != nil {
		return err
	}
	defer func() { _ = c.close() }()

	if c.handshake.Protocol < 2 {
		return nil
	}
	err = c.call(ctx, backendproto.MethodValidate, backendproto.ValidateParams{Config: config}, nil)
	// backendhost.Error, not backendproto.Error: the wire type is rebuilt into
	// this package's own on the way out of call, which is what makes a lock
	// conflict arrive as state.ErrLocked. The Kind is carried across, so this
	// asks the type callers actually receive.
	var known *Error
	if errors.As(err, &known) && known.Kind == backendproto.KindUnsupported {
		return nil
	}
	return err
}

// Search names the directories a backend binary is looked for in.
//
// Exactly the directories a provider plugin is looked for in — only the
// binary's name differs — which is why this delegates rather than listing them
// again. A backend searched somewhere a provider is not would be a second
// install mechanism arrived at by accident.
func Search(projectDir string, explicit []string) []string {
	return pluginhost.DefaultSearch(projectDir, explicit).Dirs()
}

// BinaryName is the file a backend is looked up as.
func BinaryName(name string) string {
	if os.PathSeparator == '\\' {
		return "infrena-backend-" + name + ".exe"
	}
	return "infrena-backend-" + name
}

// LockKey is how a backend is spelled in plugins.lock.
//
// Not the bare name a provider is recorded under: plugins.lock is keyed by one
// string, and a provider `s3` and a backend `s3` are different artifacts from
// different repositories. Two binaries under one key would leave the lock either
// refusing one of them on every command, or recording whichever was installed
// last and verifying neither. The repository name already tells them apart.
//
// Deliberately without the platform's executable extension, unlike BinaryName:
// plugins.lock is committed and read on other machines, and a key that said
// `.exe` on Windows alone would be a lock that checks nothing for half a team.
func LockKey(name string) string { return "infrena-backend-" + name }

// checkLock refuses a backend binary that is not the one plugins.lock recorded.
//
// It agrees deliberately with internal/pluginhost's Loader.checkLock on every
// answer: an unreadable lock refuses, a lock that does not mention this backend
// passes, and a mismatch is a typed error so the caller can tell "installed and
// wrong" from "not installed". The code cannot be shared, because that one hangs
// off a Loader holding a cached lockfile and a map of running plugins.
func checkLock(name, projectDir, path string) error {
	if projectDir == "" {
		return nil
	}

	lock, err := plugins.ReadLockfile(projectDir)
	if err != nil {
		// A lock nobody can read refuses, rather than being treated as
		// absent: reading it permissively would turn a corrupt file into a
		// silent loss of the only check there is.
		return err
	}
	if lock == nil {
		return nil
	}

	// Lockfile.Check is the authority on what an absent entry means: an entry
	// the lock does not carry passes, so a hand-placed backend keeps working.
	// Asking first only saves hashing a binary for no reason.
	key := LockKey(name)
	if _, recorded := lock.Plugins[key]; !recorded {
		return nil
	}

	sum, err := plugins.FileChecksum(path)
	if err != nil {
		return err
	}
	if err := lock.Check(key, plugins.PlatformKey(), sum); err != nil {
		return &LockError{Backend: name, Path: path, Err: err}
	}
	return nil
}

// LockError is a backend binary that is not the one plugins.lock recorded.
//
// A type rather than a wrapped string, for the reason pluginhost.LockError is
// one: the caller reporting this has to tell it apart from a backend that is not
// installed at all, and the two have opposite advice.
type LockError struct {
	// Backend is what the project called it, and Path the binary that was
	// checked.
	Backend, Path string
	// Err is the lockfile's own explanation, which already names the entry,
	// the platform, both checksums and what to do.
	Err error
}

// Error repeats the lockfile's explanation and names the binary that was
// checked.
func (e *LockError) Error() string {
	return fmt.Sprintf("%v\n\nThe binary checked was %s, the state backend %q.", e.Err, e.Path, e.Backend)
}

// Unwrap exposes the lockfile's own error.
func (e *LockError) Unwrap() error { return e.Err }

// find locates a backend binary, first match wins, and reports every place it
// looked so a missing backend says where to put it.
func find(name string, dirs []string) (string, error) {
	binary := BinaryName(name)
	var searched []string
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, binary)
		searched = append(searched, candidate)
		if isExecutable(candidate) {
			return candidate, nil
		}
	}
	if path, err := exec.LookPath(binary); err == nil {
		return path, nil
	}
	return "", &NotFoundError{Backend: name, Binary: binary, Searched: append(searched, "$PATH")}
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

// NotFoundError is a backend whose binary is nowhere to be found.
//
// A type rather than a string, for the reason pluginhost.NotFoundError is one:
// the caller that meets this has to tell "not installed" apart from "installed
// and broken", and the two have different advice.
type NotFoundError struct {
	// Backend is what the project called it, and Binary the file that name
	// resolves to.
	Backend, Binary string
	// Searched is every path that was tried, in order.
	Searched []string
}

// Error names the backend, the binary, and everywhere it was looked for.
func (e *NotFoundError) Error() string {
	return fmt.Sprintf(
		"no binary for state backend %q\n"+
			"  `backend:` names plugin %q, so infrena looked for %s in:\n%s\n"+
			"Install it with `infrena plugins install %s`, or put the binary in one of those directories.",
		e.Backend, e.Backend, e.Binary, indent(strings.Join(e.Searched, "\n")), e.Backend)
}

// Error is a failure the backend itself reported.
//
// Unwrap is what makes this worth a type. The wire carries a Kind rather than an
// error value, and every caller asks errors.Is(err, state.ErrLocked), so the
// host rebuilds the wrapping here once and a plugin's lock conflict answers that
// question exactly as the local backend's does.
type Error struct {
	// Backend is what the project called it.
	Backend string
	// Message is what the backend said went wrong.
	Message string
	// Kind is the wire classification, which decides what Unwrap returns.
	Kind backendproto.ErrorKind
}

// Error names the backend and repeats what it reported.
func (e *Error) Error() string { return fmt.Sprintf("backend %s: %s", e.Backend, e.Message) }

// Unwrap maps a lock-related Kind onto the sentinel callers already test for.
func (e *Error) Unwrap() error {
	switch e.Kind {
	case backendproto.KindLocked:
		return state.ErrLocked
	case backendproto.KindNotLocked:
		return state.ErrNotLocked
	}
	return nil
}

// client is a connection to one running backend process.
type client struct {
	// name is what the project called this backend, which is the name every
	// diagnostic uses: it is what the user wrote and what they can change.
	name string
	// path is the binary it was launched from, for diagnostics.
	path string

	handshake backendproto.Handshake

	// mu serialises calls, which is the host half of the protocol's one
	// real rule: one request, one response, before the next. The SDK serves
	// in order and does not multiplex by id, so two calls in flight would
	// have the second reading the first's answer.
	mu sync.Mutex
	w  io.WriteCloser
	sc *bufio.Scanner
	// stdout is what sc reads, kept so fail can drain it.
	stdout io.Reader
	nextID uint64
	// closed records why the connection ended, so a call arriving after a
	// crash reports the crash rather than "file already closed".
	closed error

	stderr     *stderrTail
	stderrDone chan struct{}

	stopOnce sync.Once
	stop     func()
}

// maxMessage is backendproto.MaxMessageBytes, as a variable so a test can
// reach the limit without writing 64MiB to do it.
var maxMessage = backendproto.MaxMessageBytes

// launch starts a backend binary, reads its handshake and checks it.
//
// No context, unlike the call path. A context here could only be used to kill
// the child, and killing a backend part way through a write is how a state
// object is left half-written; the connection is ended by closing stdin, which
// is the backend's own cue to exit.
func launch(name, path string) (*client, error) {
	cookie, err := cookie()
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(path)
	// The same variable pluginhost sets, because it answers the same
	// question: was I started by infrena?
	cmd.Env = append(os.Environ(), backendproto.CookieEnv+"="+cookie)

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
		return nil, fmt.Errorf("starting state backend %s at %s: %w", name, path, err)
	}

	c := &client{
		name:       name,
		path:       path,
		w:          stdin,
		sc:         bufio.NewScanner(stdout),
		stdout:     stdout,
		stderr:     newStderrTail(20),
		stderrDone: make(chan struct{}),
	}
	// A whole state, base64'd, is far past bufio's 64KiB default, and the
	// failure mode of hitting it is a truncated line that decodes as garbage
	// rather than a clear error.
	c.sc.Buffer(make([]byte, 0, 64*1024), maxMessage)
	c.stop = func() {
		// The stderr reader finishes first: cmd.Wait closes that pipe, and
		// a read still running when it does loses the lines that explain
		// the exit — which are the lines a crash message quotes.
		<-c.stderrDone
		// Wait rather than kill. A backend mid-Put that is killed can
		// leave a half-written object behind, and stdin is already closed,
		// which is its cue to exit.
		_ = cmd.Wait()
	}

	go c.forwardStderr(stderr)

	if err := c.readHandshake(); err != nil {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		<-c.stderrDone
		_ = cmd.Wait()
		return nil, err
	}
	return c, nil
}

// readHandshake reads the backend's first line and refuses one this build
// cannot talk to, or one that is not the backend it was asked for.
func (c *client) readHandshake() error {
	if !c.sc.Scan() {
		err := c.sc.Err()
		if err == nil {
			err = fmt.Errorf("it exited without saying hello")
		}
		return fmt.Errorf("state backend %s at %s sent no handshake: %s", c.name, c.path, c.withLastOutput(err.Error()))
	}
	if err := json.Unmarshal(c.sc.Bytes(), &c.handshake); err != nil {
		return fmt.Errorf("state backend %s at %s: the first line was not a handshake: %w", c.name, c.path, err)
	}
	if !backendproto.IsSupported(c.handshake.Protocol) {
		return fmt.Errorf(
			"the %s state backend speaks backend protocol version %d, which this build of infrena does not understand\n"+
				"  loaded from: %s\n"+
				"  infrena speaks: %s\n"+
				"%s",
			c.name, c.handshake.Protocol, c.path, joinInts(backendproto.Supported), upgradeAdvice(c.handshake.Protocol))
	}
	// A renamed or mis-copied binary otherwise stores state somewhere nobody
	// asked for, first seen as a colleague's apply finding an empty
	// environment. Either name is accepted, because a backend that declares
	// none falls back in the SDK to its own filename, which is right by
	// construction rather than by an author remembering to repeat it.
	if c.handshake.Name != c.name && c.handshake.Name != BinaryName(c.name) {
		return fmt.Errorf(
			"the binary for state backend %q says it is %q\n"+
				"  loaded from: %s\n"+
				"A renamed or mis-copied binary writes state somewhere nobody asked for.\n"+
				"Either rename the binary to %s, or write `plugin: %s` in your `backend:` block.",
			c.name, c.handshake.Name, c.path, BinaryName(c.name), c.handshake.Name)
	}
	return nil
}

// call sends one request and reads its response.
//
// No pending map and no reader goroutine, unlike pluginhost.Client: the SDK
// serves one request at a time, so the answer is the next line on the pipe and
// mu keeps that true from this side. The id is still checked, so a reply out of
// step is caught rather than read as the answer to a different question.
//
// A cancelled context is checked before writing, never after. There is no
// `cancel` notification in this protocol, so abandoning a read in flight would
// leave an unread answer on the pipe and every later call reading the wrong one.
// A host that must give up closes the connection, which ends the backend.
func (c *client) call(ctx context.Context, method string, params, out any) error {
	raw, err := backendproto.Encode(method, params)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed != nil {
		return c.transportError(c.closed)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("backend %s: %w", c.name, err)
	}

	c.nextID++
	id := c.nextID
	if err := c.write(backendproto.Request{ID: id, Method: method, Params: raw}); err != nil {
		return c.fail(err)
	}

	resp, err := c.read()
	if err != nil {
		return c.fail(err)
	}
	if resp.ID != id {
		// Out of step, which means every later answer would be read
		// against the wrong question. End the connection rather than
		// guess which of the two is wrong.
		return c.fail(fmt.Errorf("it answered request %d with a response to request %d", id, resp.ID))
	}
	if resp.Error != nil {
		return &Error{Backend: c.name, Message: resp.Error.Message, Kind: resp.Error.Kind}
	}
	if out == nil {
		return nil
	}
	return backendproto.Decode(method, resp.Result, out)
}

// read returns the next response on the pipe.
func (c *client) read() (backendproto.Response, error) {
	var resp backendproto.Response
	for {
		if !c.sc.Scan() {
			if err := c.sc.Err(); errors.Is(err, bufio.ErrTooLong) {
				return resp, fmt.Errorf("it sent a reply longer than the protocol allows (%d bytes); "+
					"a state this large cannot be stored through a backend plugin", maxMessage)
			} else if err != nil {
				return resp, err
			}
			return resp, io.EOF
		}
		if len(c.sc.Bytes()) == 0 {
			continue
		}
		if err := json.Unmarshal(c.sc.Bytes(), &resp); err != nil {
			// A backend that wrote to stdout has corrupted the
			// stream. The SDK points os.Stdout at stderr to stop
			// that, but a direct write to fd 1 still escapes it, and
			// a silent hang would be the alternative.
			return resp, fmt.Errorf("it wrote something that is not a protocol message: %w", err)
		}
		return resp, nil
	}
}

func (c *client) write(req backendproto.Request) error {
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	_, err = c.w.Write(append(b, '\n'))
	return err
}

// fail records why the connection ended and reports it. Called with mu held.
//
// Every later call fails the same way. A backend that exited must not leave the
// next operation blocked on a pipe nobody is reading: the executor writes state
// once per operation, so a hang there is a run that never finishes and never
// says why.
//
// It also drains stdout from here on. Nothing reads it after a failure, and a
// backend whose stdout nobody reads blocks on its next write and never exits,
// so close, which waits for the exit rather than kill a backend mid-Put, would
// wait forever. A reply over the line limit is exactly that: the backend is
// still writing the rest of it.
func (c *client) fail(err error) error {
	if c.closed == nil {
		c.closed = err
		if c.stdout != nil {
			go func() { _, _ = io.Copy(io.Discard, c.stdout) }()
		}
	}
	return c.transportError(c.closed)
}

// transportError explains a connection that ended, quoting the backend's own
// last words.
//
// Naming the backend is the whole job. "EOF" is what the reader saw rather than
// what happened, and a user reading it has to know that a process exiting closes
// its stdout. The name tells them which of their plugins died, and the stderr
// tail tells them why.
func (c *client) transportError(err error) error {
	msg := "the backend stopped responding"
	if err != nil && err.Error() != "EOF" {
		msg = err.Error()
	}
	return fmt.Errorf("state backend %s at %s failed: %s", c.name, c.path, c.withLastOutput(msg))
}

// withLastOutput appends what the backend logged before it stopped.
//
// It waits for the stderr reader, briefly, because the two pipes close
// independently: a backend that logs the reason and exits can have its stdout
// seen here before its last line has been read, and the message would then be
// the useless half of itself. Bounded because this is only ever a message — a
// backend that closed stdout and lives on must not turn a failed call into a
// hang.
func (c *client) withLastOutput(msg string) string {
	select {
	case <-c.stderrDone:
	case <-time.After(500 * time.Millisecond):
	}
	if tail := c.stderr.String(); tail != "" {
		return msg + "\nIts last output was:\n" + indent(tail)
	}
	return msg
}

// close asks the backend to shut down, then releases it.
//
// Best effort, and it does not report the shutdown's own failure: by the time
// this runs the work is done, and a backend that will not exit cleanly is not a
// reason to report an apply as failed.
func (c *client) close() error {
	c.mu.Lock()
	c.nextID++
	_ = c.write(backendproto.Request{ID: c.nextID, Method: backendproto.MethodShutdown})
	_ = c.w.Close()
	c.mu.Unlock()

	c.stopOnce.Do(func() {
		if c.stop != nil {
			c.stop()
		}
	})
	return nil
}

// forwardStderr collects the backend's log.
//
// stderr is the log and stdout is the protocol. Collected always rather than
// only under --verbose, because the lines that explain a crash are written
// before anybody knew to ask for them.
func (c *client) forwardStderr(r io.Reader) {
	defer close(c.stderrDone)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 8*1024), 1024*1024)
	for sc.Scan() {
		c.stderr.add(strings.TrimRight(sc.Text(), "\r"))
	}
	// A line over the limit ends the scanner. Keep reading anyway, or the
	// backend blocks on its next log write and never answers.
	if err := sc.Err(); err != nil {
		c.stderr.add(fmt.Sprintf("(log output dropped from here on: %v)", err))
		_, _ = io.Copy(io.Discard, r)
	}
}

// hosted adapts a client to state.Backend, so nothing upstream can tell a
// remote backend from the local one.
//
// The marshalling lives here, not in the plugin. The wire carries state as raw
// bytes because a backend stores bytes and does not interpret them; a backend
// that parsed state would be a second reader of it, free to disagree with the
// first about what a state file means.
type hosted struct {
	client *client
}

var _ state.Backend = (*hosted)(nil)

// Get loads an environment's state. Nothing stored is an empty state and not an
// error, which is the same answer Local gives for a project that has never
// applied anything.
func (h *hosted) Get(ctx context.Context, environment string) (*state.State, error) {
	var resp backendproto.GetResponse
	if err := h.client.call(ctx, backendproto.MethodGet, backendproto.GetRequest{Environment: environment}, &resp); err != nil {
		return nil, err
	}
	if len(resp.State) == 0 {
		return state.New("", environment), nil
	}
	s, err := state.Decode(resp.State)
	if err != nil {
		return nil, fmt.Errorf("state backend %s returned state for %q that infrena cannot read: %w", h.client.name, environment, err)
	}
	return s, nil
}

// Put writes the whole state for an environment.
//
// It stamps the serial, the environment and the timestamp, and restores them on
// any failure, as Local.Put does: a serial that advanced past what was stored
// would make a retry skip a value, and the serial is what detects a stale plan.
// It happens here rather than in the plugin because the plugin never sees a
// state, only bytes.
func (h *hosted) Put(ctx context.Context, environment string, s *state.State) error {
	prevSerial, prevEnv, prevUpdated := s.Serial, s.Environment, s.UpdatedAt
	committed := false
	defer func() {
		if !committed {
			s.Serial, s.Environment, s.UpdatedAt = prevSerial, prevEnv, prevUpdated
		}
	}()

	s.Serial++
	s.Environment = environment
	s.UpdatedAt = time.Now().UTC()

	data, err := s.Encode()
	if err != nil {
		return err
	}
	if err := h.client.call(ctx, backendproto.MethodPut, backendproto.PutRequest{Environment: environment, State: data}, nil); err != nil {
		return err
	}
	committed = true
	return nil
}

// List names every environment this backend holds state for.
func (h *hosted) List(ctx context.Context) ([]string, error) {
	var resp backendproto.ListResponse
	if err := h.client.call(ctx, backendproto.MethodList, backendproto.ListRequest{}, &resp); err != nil {
		return nil, err
	}
	return resp.Environments, nil
}

// Inspect reports who holds an environment's lock, without taking it.
func (h *hosted) Inspect(ctx context.Context, environment string) (state.Lock, bool, error) {
	var resp backendproto.InspectResponse
	if err := h.client.call(ctx, backendproto.MethodInspect, backendproto.InspectRequest{Environment: environment}, &resp); err != nil {
		return state.Lock{}, false, err
	}
	return resp.Held, resp.Locked, nil
}

// ForceUnlock removes a lock regardless of who holds it.
func (h *hosted) ForceUnlock(ctx context.Context, environment string) error {
	return h.client.call(ctx, backendproto.MethodForceUnlock, backendproto.ForceUnlockRequest{Environment: environment}, nil)
}

// Lock acquires an exclusive lock on an environment.
//
// The host fills the holder, which is why LockRequest carries one at all. A
// backend plugin is a child of this process, so a lock it stamped with its own
// identity would name a PID that stops existing the moment the run ends — and
// `infrena state unlock` prints that PID for a user to go and check. The
// operation is not knowable inside the plugin at all, because it lives on this
// context, and without it a conflict cannot say what the holder is doing.
func (h *hosted) Lock(ctx context.Context, environment string) (state.Lock, error) {
	// HolderFrom fills the process fields from whichever process calls it,
	// which here is the right one. Operation comes from the context label
	// the CLI already sets.
	holder := backend.HolderFrom(ctx)
	holder.Environment = environment
	holder.Operation = state.OperationFrom(ctx)

	var resp backendproto.LockResponse
	req := backendproto.LockRequest{Environment: environment, Holder: holder}
	if err := h.client.call(ctx, backendproto.MethodLock, req, &resp); err != nil {
		return state.Lock{}, err
	}
	return resp.Held, nil
}

// Unlock releases a lock this run holds.
func (h *hosted) Unlock(ctx context.Context, environment string) error {
	return h.client.call(ctx, backendproto.MethodUnlock, backendproto.UnlockRequest{Environment: environment}, nil)
}

// cookie is a fresh random value per launch.
//
// Its only job is to tell a backend it was started by infrena rather than typed
// at a shell. It is not a security boundary — a backend holds the credentials to
// the state store by design, and the user chose to run it — so nothing verifies
// it beyond presence. pluginhost has its own for the same reason.
func cookie() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating a backend cookie: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// stderrTail keeps the last few lines a backend logged, for a crash message.
//
// Bounded, because a backend that logs enthusiastically before dying would
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

func indent(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return strings.Join(lines, "\n")
}

func joinInts(vs []int) string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = fmt.Sprint(v)
	}
	return strings.Join(out, ", ")
}

// upgradeAdvice says which of the two programs is behind, because a user reading
// "protocol 2 vs 1" has to work that out for themselves.
func upgradeAdvice(theirs int) string {
	newest := 0
	for _, v := range backendproto.Supported {
		if v > newest {
			newest = v
		}
	}
	if theirs < newest {
		return "Upgrade the backend."
	}
	return "Upgrade infrena."
}
