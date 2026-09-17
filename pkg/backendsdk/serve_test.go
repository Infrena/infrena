package backendsdk

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/infrena/infrena/pkg/backend"
	"github.com/infrena/infrena/pkg/backendproto"
)

// pkg/pluginsdk is "the whole of a plugin's main()". This is the same promise
// for a backend: an author writes seven methods and one call.
func TestServeAnswersEveryOperationOverThePipe(t *testing.T) {
	in, out := pipePair(t)
	go Serve(&memoryBackend{}, in.reader, out.writer)

	handshake(t, in, out)

	if got := call(t, in, out, "lock", `{"environment":"dev"}`); !strings.Contains(got, `"user"`) {
		t.Errorf("lock response carries no holder: %s", got)
	}
	call(t, in, out, "put", `{"environment":"dev","state":"eyJ2ZXJzaW9uIjoxfQ=="}`)
	if got := call(t, in, out, "get", `{"environment":"dev"}`); !strings.Contains(got, "eyJ2ZXJzaW9uIjoxfQ==") {
		t.Errorf("get did not return what put stored: %s", got)
	}
	if got := call(t, in, out, "list", `{}`); !strings.Contains(got, `"dev"`) {
		t.Errorf("list does not name the environment put stored: %s", got)
	}
	if got := call(t, in, out, "inspect", `{"environment":"dev"}`); !strings.Contains(got, `"locked":true`) {
		t.Errorf("inspect does not report the held lock: %s", got)
	}
	call(t, in, out, "unlock", `{"environment":"dev"}`)
	if got := call(t, in, out, "inspect", `{"environment":"dev"}`); strings.Contains(got, `"locked":true`) {
		t.Errorf("lock survived unlock: %s", got)
	}
}

// THE RULE THE PROTOCOL EXISTS TO ENFORCE. A backend that does not lock is
// refused when it loads, not when an apply reaches for it, so the SDK must
// make an unlocking backend impossible to write rather than merely
// discouraged.
func TestABackendThatCannotLockCannotBeServed(t *testing.T) {
	// memoryBackend implements all seven. A type missing Lock must not
	// compile, which is asserted by the interface itself rather than at
	// runtime — this test documents the intent and pins the interface shape.
	var _ backend.Backend = (*memoryBackend)(nil)
}

// A lock conflict has to arrive as something the host can turn back into
// ErrLocked. It cannot be a string match on the message, because the message
// is the backend author's to write.
func TestALockConflictIsClassifiedOnTheWire(t *testing.T) {
	in, out := pipePair(t)
	go Serve(&memoryBackend{}, in.reader, out.writer)
	handshake(t, in, out)

	call(t, in, out, "lock", `{"environment":"dev"}`)

	resp := send(t, in, out, "lock", `{"environment":"dev"}`)
	if resp.Error == nil {
		t.Fatal("a second lock on a held environment succeeded")
	}
	if resp.Error.Kind != backendproto.KindLocked {
		t.Errorf("Kind = %q, want %q (message was %q)", resp.Error.Kind, backendproto.KindLocked, resp.Error.Message)
	}
}

// Configuration a backend cannot use must be refused rather than ignored: a
// user who spells `bucket:` wrong has written a line that does nothing, and
// silence there means state lands somewhere they did not ask for.
func TestConfigurationAnUnconfigurableBackendCannotUseIsRefused(t *testing.T) {
	in, out := pipePair(t)
	go Serve(&memoryBackend{}, in.reader, out.writer)

	line := readLine(t, out)
	var hs backendproto.Handshake
	if err := json.Unmarshal([]byte(line), &hs); err != nil {
		t.Fatal(err)
	}

	resp := send(t, in, out, "configure", `{"config":{"bucket":"acme"}}`)
	if resp.Error == nil {
		t.Fatal("configuration the backend cannot read was accepted")
	}
	if !strings.Contains(resp.Error.Message, "bucket") {
		t.Errorf("error does not name the key: %s", resp.Error.Message)
	}
}

// memoryBackend is the reference implementation an author reads: a map, a
// mutex, and the seven methods. It stores bytes it never parses, and it
// refuses a write for an environment nothing holds a lock on, because that
// refusal is a contract on every backend rather than a detail of the built-in
// local one.
type memoryBackend struct {
	mu     sync.Mutex
	states map[string][]byte
	locks  map[string]backend.Lock
}

func (m *memoryBackend) Get(ctx context.Context, environment string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// A missing environment is nothing stored, not an error: a project that
	// has never applied anything is the ordinary case.
	return m.states[environment], nil
}

func (m *memoryBackend) Put(ctx context.Context, environment string, state []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, held := m.locks[environment]; !held {
		return fmt.Errorf("refusing to write state for %q: no lock is held; call Lock first: %w", environment, backend.ErrNotLocked)
	}
	if m.states == nil {
		m.states = map[string][]byte{}
	}
	m.states[environment] = state
	return nil
}

func (m *memoryBackend) List(ctx context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	envs := make([]string, 0, len(m.states))
	for env := range m.states {
		envs = append(envs, env)
	}
	slices.Sort(envs)
	return envs, nil
}

func (m *memoryBackend) Inspect(ctx context.Context, environment string) (backend.Lock, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	held, ok := m.locks[environment]
	return held, ok, nil
}

func (m *memoryBackend) ForceUnlock(ctx context.Context, environment string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.locks[environment]; !ok {
		return fmt.Errorf("environment %q is not locked", environment)
	}
	delete(m.locks, environment)
	return nil
}

func (m *memoryBackend) Lock(ctx context.Context, environment string) (backend.Lock, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if held, ok := m.locks[environment]; ok {
		return backend.Lock{}, fmt.Errorf("environment %q is locked: held by %s on %s (pid %d), running %q: %w",
			environment, held.User, held.Host, held.PID, held.Operation, backend.ErrLocked)
	}
	// HolderFrom gives the run the host says is asking, with anything it did
	// not send filled in from this process.
	lock := backend.HolderFrom(ctx)
	lock.Environment = environment
	if m.locks == nil {
		m.locks = map[string]backend.Lock{}
	}
	m.locks[environment] = lock
	return lock, nil
}

func (m *memoryBackend) Unlock(ctx context.Context, environment string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.locks[environment]; !ok {
		return fmt.Errorf("environment %q is not locked", environment)
	}
	delete(m.locks, environment)
	return nil
}

// pipe is one direction of the stdio pair the host would own.
type pipe struct {
	reader *io.PipeReader
	writer *io.PipeWriter
	lines  *bufio.Reader
	nextID uint64
}

// pipePair returns the two halves Serve sits between: the test writes requests
// to in.writer and reads responses from out.reader, which is exactly what the
// host does over a process's stdin and stdout.
func pipePair(t *testing.T) (*pipe, *pipe) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	in := &pipe{reader: inR, writer: inW}
	out := &pipe{reader: outR, writer: outW, lines: bufio.NewReader(outR)}
	t.Cleanup(func() {
		inW.Close()
		outW.Close()
	})
	return in, out
}

// handshake reads the backend's first message and configures it, which is what
// the host does before it asks for anything.
func handshake(t *testing.T, in, out *pipe) {
	t.Helper()
	var hs backendproto.Handshake
	if err := json.Unmarshal([]byte(readLine(t, out)), &hs); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if hs.Protocol != backendproto.Version {
		t.Fatalf("Protocol = %d, want %d", hs.Protocol, backendproto.Version)
	}
	call(t, in, out, "configure", `{}`)
}

// call makes one request and fails the test if it came back an error.
func call(t *testing.T, in, out *pipe, method, params string) string {
	t.Helper()
	resp := send(t, in, out, method, params)
	if resp.Error != nil {
		t.Fatalf("%s: %s", method, resp.Error.Message)
	}
	return string(resp.Result)
}

// send makes one request and hands back whatever came of it.
func send(t *testing.T, in, out *pipe, method, params string) backendproto.Response {
	t.Helper()
	in.nextID++
	req := fmt.Sprintf(`{"id":%d,"method":%q,"params":%s}`+"\n", in.nextID, method, params)
	if _, err := io.WriteString(in.writer, req); err != nil {
		t.Fatalf("writing %s: %v", method, err)
	}
	var resp backendproto.Response
	if err := json.Unmarshal([]byte(readLine(t, out)), &resp); err != nil {
		t.Fatalf("decoding %s response: %v", method, err)
	}
	if resp.ID != in.nextID {
		t.Fatalf("response id = %d, want %d", resp.ID, in.nextID)
	}
	return resp
}

func readLine(t *testing.T, out *pipe) string {
	t.Helper()
	line, err := out.lines.ReadString('\n')
	if err != nil {
		t.Fatalf("reading from the backend: %v", err)
	}
	return line
}
