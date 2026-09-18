// Package backendsdk is what a state backend plugin's main() calls.
//
// A backend is an ordinary Go program:
//
//	func main() { backendsdk.Main(&myBackend{}) }
//
// Everything about the transport — framing, the handshake, classifying a lock
// conflict, keeping stdout clean — is handled here, so an author writes
// backend.Backend and nothing else. Seven methods and one call, which is the
// promise pkg/pluginsdk already makes a provider author.
//
// This file is deliberately pkg/pluginsdk/serve.go with the provider parts
// taken out: same loop, same malformed-line handling, same cookie check, same
// write. Every place the two differ says why.
package backendsdk

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/infrena/infrena/pkg/backend"
	"github.com/infrena/infrena/pkg/backendproto"
)

// Main is the whole of a backend's main(). It never returns.
//
// It refuses to run without the host's cookie, because a backend started by
// hand would otherwise sit silently waiting for protocol input on a terminal —
// which looks exactly like a hang, and is the first thing anyone does with a
// new binary.
func Main(b backend.Backend) {
	if os.Getenv(backendproto.CookieEnv) == "" {
		fmt.Fprintf(os.Stderr,
			"%s is an infrena state backend plugin: it is run by infrena, not directly.\n"+
				"Put it where infrena looks for plugins and name it in your `backend:` block.\n",
			nameOf(b))
		os.Exit(2)
	}
	// STDOUT IS THE PROTOCOL, and this is the guard that makes that
	// survivable. The real stdout is captured first and handed to Serve; the
	// process-wide os.Stdout is then pointed at stderr, so a stray
	// fmt.Println anywhere in the backend — or in an SDK it uses to talk to a
	// cloud, which the author may not even know about — lands in the log
	// instead of corrupting the stream. A cloud SDK printing a credential
	// warning is a likelier accident here than in a provider, not a rarer
	// one.
	//
	// A direct write to fd 1 still escapes this. Nothing in Go can stop that,
	// which is why the host also treats an unparseable line as a fatal
	// protocol error naming the backend rather than hanging.
	stream := os.Stdout
	os.Stdout = os.Stderr

	if err := Serve(b, os.Stdin, stream); err != nil && !errors.Is(err, io.EOF) {
		fmt.Fprintf(os.Stderr, "%s: %v\n", nameOf(b), err)
		os.Exit(1)
	}
}

// Serve speaks the protocol over one pair of streams until in closes.
//
// Separate from Main, and taking a reader and a writer rather than reaching
// for os.Stdin and os.Stdout, for the reason pluginsdk.Serve does: it can then
// be driven over an in-memory pipe, so the tested code path and the shipped
// one are the same one. The signature matches pluginsdk.Serve's on purpose.
func Serve(b backend.Backend, in io.Reader, out io.Writer) error {
	s := &server{backend: b, out: out}
	return s.run(in)
}

type server struct {
	backend backend.Backend
	out     io.Writer
}

// run answers requests in the order they arrive, one at a time.
//
// NO GOROUTINE PER REQUEST and no writeMu, which is the one real structural
// difference from pluginsdk. A provider serves many resources at once because
// --parallelism asks it to, so its responses have to be multiplexed by id and
// its writes serialised. A backend is asked for one thing at a time — lock,
// get, put, unlock, in that order, for one apply — so there is nothing to
// overlap. Serving in order also means an author's Put cannot be re-entered
// while it is half way through writing, which is one less way to lose a state
// file.
//
// The host must therefore not pipeline: one request, one response, before the
// next. Request.ID is still carried and echoed, so a reply that has slipped
// out of step is caught rather than read as the answer to a different call.
func (s *server) run(in io.Reader) error {
	if err := s.write(backendproto.Handshake{
		Protocol: backendproto.Version,
		Name:     nameOf(s.backend),
		Version:  versionOf(s.backend),
	}); err != nil {
		return err
	}

	sc := bufio.NewScanner(in)
	// A whole state, base64'd, is far past bufio's 64KiB default, and the
	// failure mode of hitting that limit is a truncated line that decodes as
	// garbage rather than a clear error. The same 16MiB pluginsdk allows for
	// a schema list: carrying state as raw bytes rather than nested JSON is
	// part of what keeps a real state inside it.
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var req backendproto.Request
		if err := json.Unmarshal(line, &req); err != nil {
			// A malformed line has no id to answer, so there is nothing to
			// reply to. Say so on stderr, which is the backend's log, and
			// keep serving.
			fmt.Fprintf(os.Stderr, "malformed request: %v\n", err)
			continue
		}
		if req.Method == backendproto.MethodShutdown {
			s.respond(req.ID, nil, nil)
			return nil
		}
		// context.Background rather than a cancellable one per request:
		// there is no `cancel` notification to act on, because requests do
		// not overlap and a second line cannot arrive while this one is
		// being served. A host that gives up stops reading and closes the
		// pipe, which ends the loop.
		result, err := s.dispatch(context.Background(), req)
		s.respond(req.ID, result, err)
	}
	return sc.Err()
}

func (s *server) dispatch(ctx context.Context, req backendproto.Request) (any, error) {
	switch req.Method {
	case backendproto.MethodConfigure:
		var p backendproto.ConfigureParams
		if err := backendproto.Decode(req.Method, req.Params, &p); err != nil {
			return nil, err
		}
		return nil, s.configure(ctx, p.Config)

	case backendproto.MethodValidate:
		var p backendproto.ValidateParams
		if err := backendproto.Decode(req.Method, req.Params, &p); err != nil {
			return nil, err
		}
		return nil, s.validate(p.Config)

	case backendproto.MethodGet:
		var p backendproto.GetRequest
		if err := backendproto.Decode(req.Method, req.Params, &p); err != nil {
			return nil, err
		}
		data, err := s.backend.Get(ctx, p.Environment)
		if err != nil {
			return nil, err
		}
		return backendproto.GetResponse{State: data}, nil

	case backendproto.MethodPut:
		var p backendproto.PutRequest
		if err := backendproto.Decode(req.Method, req.Params, &p); err != nil {
			return nil, err
		}
		return nil, s.backend.Put(ctx, p.Environment, p.State)

	case backendproto.MethodList:
		envs, err := s.backend.List(ctx)
		if err != nil {
			return nil, err
		}
		if envs == nil {
			// An empty list, not null: a backend holding no state at all is
			// the ordinary case and must not read as a missing answer.
			envs = []string{}
		}
		return backendproto.ListResponse{Environments: envs}, nil

	case backendproto.MethodLock:
		var p backendproto.LockRequest
		if err := backendproto.Decode(req.Method, req.Params, &p); err != nil {
			return nil, err
		}
		held, err := s.backend.Lock(backend.WithHolder(ctx, p.Holder), p.Environment)
		if err != nil {
			return nil, err
		}
		return backendproto.LockResponse{Held: held}, nil

	case backendproto.MethodUnlock:
		var p backendproto.UnlockRequest
		if err := backendproto.Decode(req.Method, req.Params, &p); err != nil {
			return nil, err
		}
		return nil, s.backend.Unlock(ctx, p.Environment)

	case backendproto.MethodInspect:
		var p backendproto.InspectRequest
		if err := backendproto.Decode(req.Method, req.Params, &p); err != nil {
			return nil, err
		}
		held, locked, err := s.backend.Inspect(ctx, p.Environment)
		if err != nil {
			return nil, err
		}
		return backendproto.InspectResponse{Held: held, Locked: locked}, nil

	case backendproto.MethodForceUnlock:
		var p backendproto.ForceUnlockRequest
		if err := backendproto.Decode(req.Method, req.Params, &p); err != nil {
			return nil, err
		}
		return nil, s.backend.ForceUnlock(ctx, p.Environment)
	}
	return nil, fmt.Errorf("unknown method %q", req.Method)
}

// configure hands the `backend:` block to a backend that can read one.
//
// Configuration a backend cannot read is REFUSED rather than dropped. A key
// that silently does nothing is a user who believes their state is in one
// place while it is written to another, and the first they hear of it is a
// colleague's apply finding an empty environment. §44: what is wrong, where,
// and what to do about it.
func (s *server) configure(ctx context.Context, config map[string]any) error {
	if c, ok := s.backend.(backend.Configurable); ok {
		return c.Configure(ctx, config)
	}
	return s.refuseUnreadableConfig(config)
}

// validate answers protocol 2's `validate`: can this `backend:` block be read,
// decided WITHOUT CONTACTING ANYTHING.
//
// Three outcomes, and the third is the one that makes the method addable at
// all. A backend implementing backend.Validator answers for itself. A backend
// that takes no configuration can still answer the one question it has —
// whether the block set keys it will ignore — which is worth keeping, because a
// key that silently does nothing is a user who believes their state is in one
// place while it is written to another. Anything else reports UNSUPPORTED,
// which the host treats as "ask no further", not as a failure.
func (s *server) validate(config map[string]any) error {
	if v, ok := s.backend.(backend.Validator); ok {
		return v.ValidateConfig(config)
	}
	if _, ok := s.backend.(backend.Configurable); ok {
		// It reads configuration but has no offline opinion about it. Saying
		// "valid" here would be a promise this backend never made, and the
		// block is still checked at Configure exactly as before.
		return &backendproto.Error{
			Kind:    backendproto.KindUnsupported,
			Message: "this backend does not validate its configuration offline",
		}
	}
	// Takes no configuration at all: the refusal below is decidable from the
	// bytes, so it belongs in validate rather than waiting for Configure.
	return s.refuseUnreadableConfig(config)
}

// refuseUnreadableConfig is the "this backend takes no configuration" refusal,
// shared by configure and validate so the two cannot come to disagree about
// which keys are acceptable.
func (s *server) refuseUnreadableConfig(config map[string]any) error {
	if len(config) == 0 {
		return nil
	}
	keys := make([]string, 0, len(config))
	for k := range config {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return fmt.Errorf("backend %q takes no configuration, but the project's `backend:` block sets %s; remove %s from `backend:`, or name a backend that reads them",
		nameOf(s.backend), strings.Join(keys, ", "), strings.Join(keys, ", "))
}

func (s *server) respond(id uint64, result any, err error) {
	resp := backendproto.Response{ID: id}
	if err != nil {
		resp.Error = &backendproto.Error{Message: err.Error(), Kind: kindOf(err)}
	} else if result != nil {
		encoded, encErr := json.Marshal(result)
		if encErr != nil {
			resp.Error = &backendproto.Error{Message: "encoding result: " + encErr.Error()}
		} else {
			resp.Result = encoded
		}
	}
	if writeErr := s.write(resp); writeErr != nil {
		fmt.Fprintf(os.Stderr, "writing response: %v\n", writeErr)
	}
}

// kindOf classifies an error so the host can rebuild the wrapping a caller
// already tests for.
//
// pluginsdk has to ASK the plugin to classify its error, because retryability
// is knowledge only a provider has. Here there is nobody to ask: the two error
// values that matter are in pkg/backend, both sides import it, and errors.Is
// is the whole of the answer. A backend that returns a bare "already locked"
// without wrapping ErrLocked is one whose conflicts reach the user as storage
// failures, which is why the interface documents the wrapping.
func kindOf(err error) backendproto.ErrorKind {
	// An error that already carries a kind keeps it. Without this the
	// classification is recomputed from scratch and anything not recognised
	// below silently becomes the empty kind — which would erase
	// KindUnsupported and turn "I do not implement validate" into "your
	// configuration is invalid", failing every backend that has not
	// implemented the optional method.
	var known *backendproto.Error
	if errors.As(err, &known) && known.Kind != "" {
		return known.Kind
	}
	switch {
	case errors.Is(err, backend.ErrLocked):
		return backendproto.KindLocked
	case errors.Is(err, backend.ErrNotLocked):
		return backendproto.KindNotLocked
	}
	return ""
}

func (s *server) write(msg any) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if _, err := s.out.Write(append(b, '\n')); err != nil {
		return err
	}
	if f, ok := s.out.(interface{ Sync() error }); ok {
		_ = f.Sync()
	}
	return nil
}

// nameOf reports the backend's own name if it declares one.
//
// provider.Plugin requires a Name(); backend.Backend does not, because the
// name here is only ever a label in the handshake and in a diagnostic, and
// requiring a method for it would put an eighth thing in an interface whose
// whole claim is seven. The fallback is the binary's own name, which is what
// the host looked for on disk to start this process (infrena-backend-s3), so
// it is right by construction rather than by an author remembering to repeat
// it.
func nameOf(b backend.Backend) string {
	if n, ok := b.(interface{ Name() string }); ok {
		return n.Name()
	}
	return filepath.Base(os.Args[0])
}

// versionOf reports the backend's own version if it declares one.
//
// Optional for the reason it is optional in pluginsdk: a version is a release
// concern, not a protocol one. A backend that does not track versions still
// works, and only a project constraining it in `plugins:` needs one.
func versionOf(b backend.Backend) string {
	if v, ok := b.(interface{ Version() string }); ok {
		return v.Version()
	}
	return "0.0.0"
}
