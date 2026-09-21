package backendproto

import (
	"bytes"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/infrena/infrena/pkg/backend"
	"github.com/infrena/infrena/pkg/pluginproto"
)

// State crosses this wire once per operation during an apply, and the local
// backend already serialises the whole state every time. Carrying it as a
// nested JSON object or a base64 string would add a second full encode to
// every operation. Raw bytes, once.
func TestStateCrossesTheWireAsRawBytesNotNestedJSON(t *testing.T) {
	raw := []byte(`{"version":1,"serial":7,"resources":{}}`)
	data, err := json.Marshal(PutRequest{Environment: "dev", State: raw})
	if err != nil {
		t.Fatal(err)
	}

	var got PutRequest
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.State, raw) {
		t.Errorf("State = %q, want %q", got.State, raw)
	}
	// encoding/json renders []byte as base64, which is ONE encode and is what
	// we accept. What must not appear is the state's own field names, which
	// would mean it had been re-marshalled as a structure.
	if bytes.Contains(data, []byte(`"serial"`)) {
		t.Errorf("state was re-encoded as JSON structure, not carried as bytes:\n%s", data)
	}
}

// Every boundary carries exactly one version, and this one is independent of
// the provider protocol: a state format change must not force a provider
// release, and vice versa.
func TestVersionIsIndependentOfThePluginProtocol(t *testing.T) {
	// The literal is the tripwire: raising it must be a deliberate edit here as
	// well as there, because a bump obliges every backend author to read what
	// changed. 2 added the optional `validate` method.
	if Version != 2 {
		t.Errorf("Version = %d, want 2", Version)
	}
	if !slices.Contains(Supported, Version) {
		t.Error("Supported does not include Version")
	}
}

// `Supported` is a set so that a backend built against an older protocol keeps
// working. It matters for 2 in particular, whose only addition is optional: a
// protocol-1 backend is never asked to validate, so dropping 1 here would
// orphan every backend in existence to gain a method nobody must implement.
func TestEveryEarlierVersionStaysSupported(t *testing.T) {
	for v := 1; v <= Version; v++ {
		if !IsSupported(v) {
			t.Errorf("protocol %d is no longer supported, which orphans every backend speaking it", v)
		}
	}
}

// A lock crosses the wire whole, so a conflict can name its holder.
func TestALockRoundTripsAcrossTheWire(t *testing.T) {
	want := backend.Lock{
		Environment: "production", PID: 4211, Host: "host-3",
		User: "alice", Operation: "apply", At: time.Unix(1700000000, 0).UTC(),
	}
	data, err := json.Marshal(LockResponse{Held: want})
	if err != nil {
		t.Fatal(err)
	}
	var got LockResponse
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !got.Held.At.Equal(want.At) || got.Held.User != want.User || got.Held.PID != want.PID {
		t.Errorf("Held = %+v, want %+v", got.Held, want)
	}
}

// The cookie is spelled out here rather than imported from pluginproto, so a
// backend author's binary does not link the provider protocol to read one
// string. Spelling it twice is only safe while the two cannot drift: the host
// sets one variable when it launches a child, and a backend looking for a
// different name would wait forever on a pipe nobody writes to.
func TestTheCookieIsTheSameVariableTheHostAlreadySets(t *testing.T) {
	if CookieEnv != pluginproto.CookieEnv {
		t.Errorf("CookieEnv = %q, want %q", CookieEnv, pluginproto.CookieEnv)
	}
}
