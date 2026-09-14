// Package pluginhost is infrena's side of the plugin protocol: it launches a
// plugin, speaks to it, and — most of the work here — refuses to trust it.
//
// PLAN.md §31.1. Several guarantees used to rest on each provider following a
// doc comment. A binary somebody else built cannot be held to a comment, so each
// one moved here, where it holds for every plugin including the official ones.
package pluginhost

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/infrena/infrena/pkg/pluginproto"
	"github.com/infrena/infrena/pkg/provider"
)

// Client is a connection to one running plugin process.
//
// One process per plugin, not per instance: two accounts of one cloud means one
// process holding two configured clients, told apart by handle.
type Client struct {
	name string
	// path is where the binary was loaded from, for diagnostics. Empty for an
	// in-process client.
	path string

	handshake pluginproto.Handshake

	w       io.WriteCloser
	writeMu sync.Mutex

	nextID atomic.Uint64

	mu      sync.Mutex
	pending map[uint64]chan pluginproto.Response
	// closed records why the connection ended, so a call that arrives after a
	// crash reports the crash rather than "use of closed file".
	closed error

	done chan struct{}

	// stderr is the plugin's log, kept so a crash can quote its last words.
	stderr *stderrTail

	// stop releases whatever owns the process or pipe.
	stop func()
}

// Name is the plugin's own name, as it declared it in the handshake.
func (c *Client) Name() string { return c.name }

// Path is where the binary was loaded from, or "" in process.
func (c *Client) Path() string { return c.path }

// PluginVersion is the version the plugin reported.
func (c *Client) PluginVersion() string { return c.handshake.Version }

// start reads the handshake and then serves responses until the stream ends.
func (c *Client) start(r io.Reader, expectName string) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	if !sc.Scan() {
		err := sc.Err()
		if err == nil {
			err = errors.New("the plugin exited without saying hello")
		}
		return fmt.Errorf("%s: no handshake: %w", expectName, err)
	}
	if err := json.Unmarshal(sc.Bytes(), &c.handshake); err != nil {
		return fmt.Errorf("%s: the first line was not a handshake: %w", expectName, err)
	}
	if err := c.checkHandshake(expectName); err != nil {
		return err
	}
	c.name = c.handshake.Name

	go c.serve(sc)
	return nil
}

// checkHandshake refuses a plugin this build cannot talk to.
func (c *Client) checkHandshake(expectName string) error {
	if !pluginproto.IsSupported(c.handshake.Protocol) {
		return &IncompatibleError{
			Plugin:    expectName,
			Path:      c.path,
			Theirs:    c.handshake.Protocol,
			Supported: pluginproto.Supported,
		}
	}
	// A renamed or mis-copied binary otherwise serves the wrong schemas, and the
	// first sign of that is a plan proposing something nobody asked for.
	if c.handshake.Name != expectName {
		return &WrongPluginError{
			Expected: expectName,
			Actual:   c.handshake.Name,
			Path:     c.path,
		}
	}
	return nil
}

func (c *Client) serve(sc *bufio.Scanner) {
	defer close(c.done)
	for sc.Scan() {
		var resp pluginproto.Response
		if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
			// A plugin that wrote to stdout corrupts the stream. The SDK points
			// os.Stdout at stderr to stop it, but a plugin that writes to fd 1
			// directly still can, and a silent hang would be the alternative.
			c.fail(fmt.Errorf("%s wrote something that is not a protocol message: %w", c.name, err))
			return
		}
		c.mu.Lock()
		ch := c.pending[resp.ID]
		delete(c.pending, resp.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- resp
		}
	}
	err := sc.Err()
	if err == nil {
		err = io.EOF
	}
	c.fail(err)
}

// fail ends every pending call, and every later one.
//
// A plugin that exits mid-apply must not leave a caller blocked forever: the
// executor treats this like any other failed operation, and state is written
// incrementally, so everything that completed before the exit is recorded.
func (c *Client) fail(err error) {
	c.mu.Lock()
	if c.closed == nil {
		c.closed = err
	}
	pending := c.pending
	c.pending = map[uint64]chan pluginproto.Response{}
	c.mu.Unlock()

	for id, ch := range pending {
		ch <- pluginproto.Response{ID: id, Error: &pluginproto.Error{
			Message:      c.exitMessage(err),
			Retryability: uint8(provider.NotSafeToRetry),
		}}
	}
}

// call sends one request and waits for its response.
//
// A cancelled context sends `cancel` and KEEPS WAITING. Killing a plugin during
// Create can orphan a resource that was really created — the case the
// non-nil-on-success rule exists to prevent — so the host always waits for the
// plugin's own answer about what happened.
func (c *Client) call(ctx context.Context, method string, params any, out any) error {
	raw, err := pluginproto.Encode(method, params)
	if err != nil {
		return err
	}

	id := c.nextID.Add(1)
	ch := make(chan pluginproto.Response, 1)

	c.mu.Lock()
	if c.closed != nil {
		err := c.closed
		c.mu.Unlock()
		return fmt.Errorf("%s: %s", c.name, c.exitMessage(err))
	}
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.write(pluginproto.Request{ID: id, Method: method, Params: raw}); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return err
	}

	select {
	case resp := <-ch:
		return decodeResponse(resp, method, out)
	case <-ctx.Done():
		_ = c.write(pluginproto.Request{ID: id, Method: pluginproto.MethodCancel})
		// AND KEEP WAITING. The host always learns what the plugin actually did.
		resp := <-ch
		// If the plugin answered successfully, THAT IS THE TRUTH and this call
		// succeeded: the resource really was created, and returning ctx.Err()
		// instead would tell the executor the operation failed, which is how a
		// created resource ends up recorded nowhere. The caller's own context is
		// already cancelled and it will see that for itself.
		//
		// internal/executor made the same call from the other direction: it
		// dispatches every provider call on a context.WithoutCancel so that "an
		// in-flight create must not be told to abort mid-call" (operationContext).
		// So in practice nothing cancels a call mid-flight today, and this path is
		// what keeps that true for a caller that does.
		return decodeResponse(resp, method, out)
	}
}

func decodeResponse(resp pluginproto.Response, method string, out any) error {
	if resp.Error != nil {
		return &ProviderError{
			Message:      resp.Error.Message,
			Retryability: provider.Retryability(resp.Error.Retryability),
		}
	}
	if out == nil {
		return nil
	}
	return pluginproto.Decode(method, resp.Result, out)
}

func (c *Client) write(req pluginproto.Request) error {
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.w.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("%s: %w", c.name, err)
	}
	return nil
}

// Close asks the plugin to shut down, then releases it.
func (c *Client) Close() error {
	_ = c.write(pluginproto.Request{ID: c.nextID.Add(1), Method: pluginproto.MethodShutdown})
	_ = c.w.Close()
	if c.stop != nil {
		c.stop()
	}
	return nil
}

// ProviderError is a failure the plugin reported, carrying its classification.
//
// §35's retry logic reads Retryability from here rather than calling
// ClassifyError, which cannot cross a process boundary.
type ProviderError struct {
	Message      string
	Retryability provider.Retryability
}

func (e *ProviderError) Error() string { return e.Message }
