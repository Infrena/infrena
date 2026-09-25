// Package backendproto is the wire contract between infrena and a state
// backend plugin.
//
// It is separate from pkg/pluginproto deliberately. The two evolve for
// unrelated reasons: a change to how state is stored must not force every
// provider to cut a release, and a change to how resources are described must
// not force every backend to.
//
// The transport is shared — newline-delimited JSON over stdio, the same
// handshake and the same cookie — because that part has no reason to differ and
// a second transport would be a second thing to get wrong. Read pkg/pluginproto
// alongside this file: Handshake, Request, Response, Encode and Decode are the
// same shapes doing the same jobs, and every place the two differ says why.
//
// The wire form is the compatibility contract, not the Go types. A backend
// built against an older SDK keeps working for as long as its protocol version
// is supported, so this package changes additively.
package backendproto

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/infrena/infrena/pkg/backend"
)

// Version is the backend protocol version this build speaks.
//
// It is its own number rather than pluginproto.Version. A backend speaks about
// environments, bytes and locks; a provider speaks about resources and schemas.
// Nothing said about one is a statement about the other, so raising one must
// never oblige anybody to re-release the other. A single shared number would
// mean a new attribute flag on a provider schema orphaning every backend in
// existence for no reason at all.
const Version = 2

// Supported lists every backend protocol version this build can talk to,
// newest first. A set rather than a number, like pluginproto.Supported, so
// raising Version does not immediately orphan every backend in existence.
var Supported = []int{2, 1}

// IsSupported reports whether a backend's protocol version can be spoken here.
func IsSupported(v int) bool {
	return slices.Contains(Supported, v)
}

// MaxMessageBytes is the longest protocol line either side reads. A whole
// state travels as one base64 line, so this is a ceiling on state size. It
// matches pluginproto.MaxMessageBytes without importing it, for the same
// reason CookieEnv is spelled out below.
const MaxMessageBytes = 64 * 1024 * 1024

// CookieEnv is the environment variable the host sets when launching a
// backend, and it is the same variable pluginproto names.
//
// One variable, because it answers one question — "was I started by infrena?"
// — and the host launching a child has no reason to ask it differently
// depending on what the child turned out to be. A backend binary run by hand
// without it prints a line saying what it is and exits non-zero, rather than
// sitting silently waiting for protocol input on a terminal.
//
// It is spelled out rather than imported so a backend author's binary does not
// link the provider protocol, and every type it drags in, to read one string.
// This package's tests pin the two spellings together so they cannot drift.
const CookieEnv = "INFRENA_PLUGIN_COOKIE"

// Method names. Every request carries one.
//
// MethodGet through MethodForceUnlock are the seven methods of backend.Backend,
// one for one. There is no `cancel` here, unlike pluginproto: the host calls a
// backend one operation at a time on one pipe (lock, get, put, unlock, in that
// order, for one apply), so there is nothing to multiplex and therefore nothing
// to cancel out of turn. Cancellation is the context on the host side giving up
// on the read.
const (
	// MethodConfigure hands the backend the project's `backend:` block.
	MethodConfigure = "configure"
	// MethodValidate asks whether a `backend:` block could be read, without
	// contacting anything. Protocol 2, and optional.
	MethodValidate = "validate"
	// MethodGet loads one environment's state.
	MethodGet = "get"
	// MethodPut writes one environment's state.
	MethodPut = "put"
	// MethodList names the environments this backend holds state for.
	MethodList = "list"
	// MethodLock takes an environment's lock.
	MethodLock = "lock"
	// MethodUnlock releases a lock this run holds.
	MethodUnlock = "unlock"
	// MethodInspect reports an environment's lock holder without taking it.
	MethodInspect = "inspect"
	// MethodForceUnlock removes a lock regardless of who holds it.
	MethodForceUnlock = "force_unlock"
	// MethodShutdown asks the backend to answer and then exit.
	MethodShutdown = "shutdown"
)

// Handshake is a backend's first message, sent before it serves any request.
//
// The same three fields pluginproto.Handshake carries: Protocol is this
// package's Version rather than that one's, Name is the backend's own name, and
// Version is its release version.
type Handshake struct {
	Protocol int    `json:"protocol"`
	Name     string `json:"name"`
	Version  string `json:"version"`
}

// Request is one call from host to backend.
//
// ID is unique within a session, and a response says which request it answers.
// A backend answers in order, so the id also catches a reply that has slipped
// out of step rather than letting the host read the wrong answer to the right
// question.
type Request struct {
	ID     uint64          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Response is one reply. Exactly one of Result and Error is set.
type Response struct {
	ID     uint64          `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

// ErrorKind classifies a failure so the host can rebuild the typed error a
// caller already tests for.
type ErrorKind string

// The kinds. Absent means an ordinary failure: the store was unreachable, the
// credentials were refused, the bucket does not exist.
const (
	// KindLocked is a lock conflict: somebody else holds this environment.
	// The host turns it back into state.ErrLocked, so every existing
	// errors.Is(err, state.ErrLocked) keeps giving the same answer once
	// state has gone remote. Losing this distinction would mean an apply
	// blocked by a colleague's lock reporting a storage outage.
	KindLocked ErrorKind = "locked"
	// KindNotLocked is a write refused because the caller does not hold the
	// lock. It maps back to state.ErrNotLocked.
	KindNotLocked ErrorKind = "not_locked"
	// KindUnsupported is "I do not implement this method", which is a
	// different fact from "the answer is no" and must not be reported as a
	// failure. Protocol 2 added `validate` as optional, so a backend that
	// does not offer it says so here and the host carries on exactly as it
	// did before the method existed. Without the distinction, every backend
	// that had not implemented it yet would fail every `infrena validate`.
	KindUnsupported ErrorKind = "unsupported"
)

// Error is a failure crossing the wire, carrying its own classification.
//
// pluginproto.Error also carries a retryability, and has to ask the plugin to
// compute it, because retryability is knowledge only a provider has. Here the
// classification needs nobody's opinion: the two error values that matter are
// backend.ErrLocked and backend.ErrNotLocked, both in the public package both
// sides already import, so the SDK answers it with errors.Is and the host
// rebuilds the wrapping.
type Error struct {
	Message string    `json:"message"`
	Kind    ErrorKind `json:"kind,omitempty"`
}

// Error returns the message, so a decoded wire error can be returned as one.
func (e *Error) Error() string { return e.Message }

// The payload types below are named after their operation — GetRequest,
// GetResponse — where pluginproto spells the same idea Params and Result. These
// carry a whole operation each, rather than one shape shared by three methods,
// so naming them for the method they serve is what a reader is looking for.

// ConfigureParams hands the backend the project's `backend:` block, minus
// `plugin:`, exactly as the user wrote it.
//
// There is no instance or handle, unlike pluginproto.ConfigureParams. A
// provider process can hold several configured instances — two AWS accounts in
// one binary — so every later call names which one it means. A project has
// exactly one backend: one process, one configuration, nothing to tell apart.
//
// Config holds plain Go values decoded from YAML (string, int, bool, []any,
// map[string]any) rather than value.Value, because `backend:` cannot
// interpolate: there are no unknowns here to track, only literals. It is read
// before anything is compiled, which is the whole reason a variable in it can
// never be resolved.
type ConfigureParams struct {
	Config map[string]any `json:"config,omitempty"`
}

// ValidateParams asks a backend whether a `backend:` block is one it could
// read, without contacting anything. Protocol 2.
//
// It exists because `infrena validate` promises to check configuration without
// reaching the network, and Configure cannot keep that promise: a backend
// configures by building a client, and the s3 backend's Configure also proves
// the store supports conditional writes, which is a round trip. Without this
// the one check that would catch a long-lived access key committed to infrena.yml
// is the one that does not run in CI, where committed credentials arrive.
//
// Offline-only is a contract rather than a hint: a backend that dials here
// turns the cheap gate back into the expensive one and breaks the promise
// validate makes to every project, not just its own.
//
// Same shape as ConfigureParams deliberately. This answers "could you read
// this?" about the very bytes Configure would later receive, and two shapes
// would let them drift into disagreeing about what was checked.
type ValidateParams struct {
	Config map[string]any `json:"config,omitempty"`
}

// GetRequest asks for one environment's state.
type GetRequest struct {
	Environment string `json:"environment"`
}

// GetResponse carries the stored state as raw bytes.
//
// Empty means the backend holds nothing for this environment, which is not an
// error: a project that has never applied anything is the ordinary case. The
// host turns it into a new empty state. Absent and empty are the same thing
// here because a zero-byte state file is not something a backend can
// legitimately hold — every state the engine writes is at minimum a JSON
// object — so there is no third case for a flag to distinguish.
type GetResponse struct {
	State []byte `json:"state,omitempty"`
}

// PutRequest writes one environment's state.
//
// State is raw bytes and the backend must store them unread. Put is called once
// per operation during an apply and carries the whole state each time, so the
// local backend's single serialisation is already paid per operation; nesting
// that JSON inside this JSON would pay for a second full encode and decode
// every time, to produce a structure neither side is allowed to look at.
// encoding/json renders []byte as base64 — one encode, and one the host can
// swap for a framed binary body later without any of this changing shape.
//
// A backend that parsed these bytes would be a second reader of state, free to
// disagree with the first about what a state file means.
type PutRequest struct {
	Environment string `json:"environment"`
	State       []byte `json:"state"`
}

// ListRequest asks which environments this backend holds state for. It carries
// nothing: the backend serves one project and knows its own prefix.
type ListRequest struct{}

// ListResponse names every environment, in a stable order. A backend holding
// none returns an empty list and no error.
type ListResponse struct {
	Environments []string `json:"environments"`
}

// LockRequest asks for an exclusive lock on one environment.
//
// Holder is who the host says is asking, rather than something the backend
// invents. A backend plugin is a child of the infrena process, so a lock it
// stamped with its own identity would name a PID that stops existing the moment
// the run ends — and `infrena state unlock` prints that PID for a user to go
// and check. Operation ("apply", "destroy") is not knowable inside the plugin
// at all: it lives on the host's context, and without it a conflict cannot say
// what the holder is doing.
//
// Holder may be omitted, and the SDK then fills it from the serving process.
// That is for a backend driven over a pipe by hand; the host always sends one.
type LockRequest struct {
	Environment string       `json:"environment"`
	Holder      backend.Lock `json:"holder,omitzero"`
}

// LockResponse reports the lock as it was recorded.
//
// The whole lock, not an acknowledgement, because the caller shows it: the
// holder it names is what a later conflict prints. A conflict itself is an
// Error with KindLocked whose message already names the holder, the same way
// the local backend's message does.
type LockResponse struct {
	Held backend.Lock `json:"held"`
}

// UnlockRequest releases a lock this run holds. A backend must refuse to
// release one held by anyone else; that is what makes ForceUnlock mean
// something.
type UnlockRequest struct {
	Environment string `json:"environment"`
}

// InspectRequest asks who holds an environment's lock, without taking it.
type InspectRequest struct {
	Environment string `json:"environment"`
}

// InspectResponse reports the holder, if there is one.
//
// Locked distinguishes "no lock is held" from "a lock is held", and it is a
// separate field rather than an empty Held because a caller that conflated an
// unreadable or absent answer with a free environment would let a second apply
// start. An error is the third case and is reported as one.
type InspectResponse struct {
	Held   backend.Lock `json:"held,omitzero"`
	Locked bool         `json:"locked"`
}

// ForceUnlockRequest removes a lock regardless of who holds it, for
// `infrena state unlock` after it has told the user whose lock it is dropping.
// A lock that was not there must come back as an error rather than a silent
// success: a typo in an environment name must not look like it worked.
type ForceUnlockRequest struct {
	Environment string `json:"environment"`
}

// Encode marshals params for a request, wrapping the failure with the method so
// a malformed call says which one it was.
func Encode(method string, params any) (json.RawMessage, error) {
	if params == nil {
		return nil, nil
	}
	b, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("encoding %s params: %w", method, err)
	}
	return b, nil
}

// Decode unmarshals into out, naming the method on failure.
func Decode(method string, raw json.RawMessage, out any) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decoding %s: %w", method, err)
	}
	return nil
}
