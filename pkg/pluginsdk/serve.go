// Package pluginsdk is what a provider plugin's main() calls.
//
// A plugin is an ordinary Go program:
//
//	func main() { pluginsdk.Main(myplugin.New()) }
//
// Everything about the transport — framing, multiplexing, cancellation,
// keeping stdout clean — is handled here, so a plugin author implements
// provider.Plugin and provider.Provider and nothing else.
package pluginsdk

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/pluginproto"
	"github.com/infrena/infrena/pkg/provider"
	"github.com/infrena/infrena/pkg/resource"
)

// Main is the whole of a plugin's main(). It never returns.
//
// It refuses to run without the host's cookie, because a plugin started by
// hand would otherwise sit silently waiting for protocol input on a terminal,
// which looks exactly like a hang.
func Main(p provider.Plugin) {
	if os.Getenv(pluginproto.CookieEnv) == "" {
		fmt.Fprintf(os.Stderr,
			"%s is an infrena provider plugin: it is run by infrena, not directly.\n"+
				"Put it where infrena looks for plugins and name it in your `providers:` block.\n",
			p.Name())
		os.Exit(2)
	}
	// Stdout is the protocol stream. The real one is captured here and handed
	// to Serve, and the process-wide os.Stdout is pointed at stderr, so that a
	// stray fmt.Println anywhere in the plugin — or in a library it uses —
	// lands in the log instead of corrupting the stream.
	//
	// A direct write to file descriptor 1 still escapes this, which is why the
	// host treats an unparseable line as a fatal protocol error naming the
	// plugin rather than waiting on it.
	stream := os.Stdout
	os.Stdout = os.Stderr

	if err := Serve(p, os.Stdin, stream); err != nil && !errors.Is(err, io.EOF) {
		fmt.Fprintf(os.Stderr, "%s: %v\n", p.Name(), err)
		os.Exit(1)
	}
}

// Serve speaks the protocol over one pair of streams until in closes. Requests
// are served concurrently, and responses may be written in any order.
//
// It is separate from Main so that a plugin can be driven over an in-memory
// pipe, which keeps the tested code path and the shipped one the same one.
func Serve(p provider.Plugin, in io.Reader, out io.Writer) error {
	s := &server{
		plugin:    p,
		out:       out,
		instances: map[string]provider.Provider{},
		inflight:  map[uint64]context.CancelFunc{},
	}
	return s.run(in)
}

type server struct {
	plugin provider.Plugin

	// writeMu serialises writes. Requests are served concurrently, so without it
	// two responses interleave mid-line and the stream is corrupt from then on.
	writeMu sync.Mutex
	out     io.Writer

	mu        sync.Mutex
	instances map[string]provider.Provider
	nextID    uint64
	inflight  map[uint64]context.CancelFunc
}

func (s *server) run(in io.Reader) error {
	if err := s.write(pluginproto.Handshake{
		Protocol:       pluginproto.Version,
		Name:           s.plugin.Name(),
		Version:        versionOf(s.plugin),
		MaxConcurrency: maxConcurrencyOf(s.plugin),
	}); err != nil {
		return err
	}

	sc := bufio.NewScanner(in)
	// A schema list is easily past bufio's 64KiB default, and hitting that
	// limit gives a truncated line that decodes as garbage rather than a clear
	// error.
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	var wg sync.WaitGroup
	defer wg.Wait()

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var req pluginproto.Request
		if err := json.Unmarshal(line, &req); err != nil {
			// A malformed line carries no id, so there is nothing to reply to.
			// Log it and keep serving.
			fmt.Fprintf(os.Stderr, "malformed request: %v\n", err)
			continue
		}
		if req.Method == pluginproto.MethodCancel {
			s.cancel(req.ID)
			continue
		}
		if req.Method == pluginproto.MethodShutdown {
			s.respond(req.ID, nil, nil)
			return nil
		}

		ctx, cancel := context.WithCancel(context.Background())
		s.mu.Lock()
		s.inflight[req.ID] = cancel
		s.mu.Unlock()

		wg.Add(1)
		go func(req pluginproto.Request) {
			defer wg.Done()
			defer func() {
				s.mu.Lock()
				delete(s.inflight, req.ID)
				s.mu.Unlock()
				cancel()
			}()
			result, err := s.dispatch(ctx, req)
			s.respond(req.ID, result, err)
		}(req)
	}
	return sc.Err()
}

// cancel cancels one in-flight request's context.
//
// It is a message, not a kill: the host still waits for the response. Killing
// a plugin part-way through a create would orphan a resource that was really
// created.
func (s *server) cancel(id uint64) {
	s.mu.Lock()
	cancel := s.inflight[id]
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *server) dispatch(ctx context.Context, req pluginproto.Request) (any, error) {
	switch req.Method {
	case pluginproto.MethodSchemas:
		return pluginproto.SchemasResult{Definitions: s.plugin.Definitions()}, nil

	case pluginproto.MethodConfigure:
		var p pluginproto.ConfigureParams
		if err := pluginproto.Decode(req.Method, req.Params, &p); err != nil {
			return nil, err
		}
		prov, err := s.plugin.New(provider.Config{
			Instance:   p.Instance,
			Values:     p.Config,
			ProjectDir: p.Dir,
		})
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		s.nextID++
		handle := fmt.Sprintf("%s-%d", p.Instance, s.nextID)
		s.instances[handle] = prov
		s.mu.Unlock()
		return pluginproto.ConfigureResult{Handle: handle}, nil

	case pluginproto.MethodRead, pluginproto.MethodCreate, pluginproto.MethodDelete:
		var p pluginproto.ResourceParams
		if err := pluginproto.Decode(req.Method, req.Params, &p); err != nil {
			return nil, err
		}
		prov, err := s.instance(p.Handle)
		if err != nil {
			return nil, err
		}
		return s.oneResource(ctx, req.Method, prov, p)

	case pluginproto.MethodUpdate:
		var p pluginproto.UpdateParams
		if err := pluginproto.Decode(req.Method, req.Params, &p); err != nil {
			return nil, err
		}
		prov, err := s.instance(p.Handle)
		if err != nil {
			return nil, err
		}
		got, err := prov.Update(ctx, stateOf(p.Current), desiredOf(p.Desired))
		if err != nil {
			return nil, err
		}
		return resultOf(got), nil

	case pluginproto.MethodDiscover:
		var p pluginproto.DiscoverParams
		if err := pluginproto.Decode(req.Method, req.Params, &p); err != nil {
			return nil, err
		}
		prov, err := s.instance(p.Handle)
		if err != nil {
			return nil, err
		}
		found, err := prov.Discover(ctx, provider.DiscoverRequest{Types: p.Types})
		if err != nil {
			return nil, err
		}
		out := pluginproto.DiscoverResult{Found: make([]pluginproto.Discovered, 0, len(found))}
		for _, f := range found {
			out.Found = append(out.Found, pluginproto.Discovered{
				Type: f.Type, ProviderID: f.ProviderID, Attributes: f.Attributes,
				SystemOwned: f.SystemOwned, SystemOwnedReason: f.SystemOwnedReason,
			})
		}
		return out, nil

	case pluginproto.MethodImport:
		var p pluginproto.ImportParams
		if err := pluginproto.Decode(req.Method, req.Params, &p); err != nil {
			return nil, err
		}
		prov, err := s.instance(p.Handle)
		if err != nil {
			return nil, err
		}
		got, err := prov.Import(ctx, p.Type, p.ID)
		if err != nil {
			return nil, err
		}
		return resultOf(got), nil
	}
	return nil, fmt.Errorf("unknown method %q", req.Method)
}

// oneResource runs the three methods whose params and result have the same
// shape.
func (s *server) oneResource(
	ctx context.Context, method string, prov provider.Provider, p pluginproto.ResourceParams,
) (any, error) {
	switch method {
	case pluginproto.MethodRead:
		got, err := prov.Read(ctx, stateOf(p))
		if err != nil {
			return nil, err
		}
		return resultOf(got), nil
	case pluginproto.MethodCreate:
		got, err := prov.Create(ctx, desiredOf(p))
		if err != nil {
			return nil, err
		}
		return resultOf(got), nil
	default:
		return nil, prov.Delete(ctx, stateOf(p))
	}
}

func (s *server) instance(handle string) (provider.Provider, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prov, ok := s.instances[handle]
	if !ok {
		return nil, fmt.Errorf("no configured instance %q; configure must be called first", handle)
	}
	return prov, nil
}

func (s *server) respond(id uint64, result any, err error) {
	resp := pluginproto.Response{ID: id}
	if err != nil {
		resp.Error = &pluginproto.Error{
			Message:      err.Error(),
			Retryability: uint8(s.plugin2Retryability(err)),
		}
	} else if result != nil {
		encoded, encErr := json.Marshal(result)
		if encErr != nil {
			resp.Error = &pluginproto.Error{Message: "encoding result: " + encErr.Error()}
		} else {
			resp.Result = encoded
		}
	}
	if writeErr := s.write(resp); writeErr != nil {
		fmt.Fprintf(os.Stderr, "writing response: %v\n", writeErr)
	}
}

// plugin2Retryability asks the plugin to classify its own error.
//
// It happens on the plugin's side because provider.ClassifyError takes an
// error value, and an error does not cross a pipe. Only a configured provider
// can classify, so before any instance exists the answer is the safe one.
func (s *server) plugin2Retryability(err error) provider.Retryability {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, prov := range s.instances {
		return prov.ClassifyError(err)
	}
	return provider.NotSafeToRetry
}

func (s *server) write(msg any) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := s.out.Write(append(b, '\n')); err != nil {
		return err
	}
	if f, ok := s.out.(interface{ Sync() error }); ok {
		_ = f.Sync()
	}
	return nil
}

// versionOf reports the plugin's own version if it declares one.
//
// Optional because a version is a release concern, not a protocol one: only a
// project constraining the plugin in `plugins:` needs one.
func versionOf(p provider.Plugin) string {
	if v, ok := p.(interface{ Version() string }); ok {
		return v.Version()
	}
	return "0.0.0"
}

func stateOf(p pluginproto.ResourceParams) *resource.ResourceState {
	return &resource.ResourceState{
		Address:    address.Address{Name: p.Address},
		Type:       p.Type,
		ProviderID: p.ProviderID,
		Attributes: p.Attributes,
	}
}

func desiredOf(p pluginproto.ResourceParams) *resource.DesiredResource {
	return &resource.DesiredResource{
		Address: address.Address{Name: p.Address},
		Type:    p.Type,
		Attrs:   p.Attributes,
	}
}

func resultOf(rs *resource.ResourceState) pluginproto.ResourceResult {
	if rs == nil {
		return pluginproto.ResourceResult{Absent: true}
	}
	return pluginproto.ResourceResult{
		ProviderID: rs.ProviderID,
		Attributes: rs.Attributes,
	}
}

// ConcurrencyLimiter is an optional interface a provider.Plugin may also
// implement to declare how many operations its API tolerates at once. A plugin
// that does not implement it makes no claim, and the host keeps its own
// conservative default.
//
// Implement it only for a real number — a documented rate limit, a measured
// one, an account quota. A guessed value is worth less than silence, because
// silence is visibly a non-claim.
type ConcurrencyLimiter interface {
	// MaxConcurrency is how many operations may be in flight against this
	// plugin at once. Zero or negative is treated as no claim.
	MaxConcurrency() int
}

// maxConcurrencyOf reads a plugin's declared ceiling, or 0 when it makes no
// claim. A negative is folded to 0 rather than passed on, since a ceiling
// below one would stop work entirely.
func maxConcurrencyOf(p provider.Plugin) int {
	l, ok := p.(ConcurrencyLimiter)
	if !ok {
		return 0
	}
	if n := l.MaxConcurrency(); n > 0 {
		return n
	}
	return 0
}
