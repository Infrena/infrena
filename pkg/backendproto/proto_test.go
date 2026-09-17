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

// Section 61: every boundary carries exactly one version, and this one is
// independent of the provider protocol. A state format change must not force
// a provider release, and vice versa.
func TestVersionIsIndependentOfThePluginProtocol(t *testing.T) {
	if Version != 1 {
		t.Errorf("Version = %d, want 1", Version)
	}
	if !slices.Contains(Supported, Version) {
		t.Error("Supported does not include Version")
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
// string. Spelling it twice is only safe if the two cannot drift, which is
// what this test is: the host sets ONE variable when it launches a child, and
// a backend that looked for a different name would sit waiting for input on a
// pipe nobody is writing to.
func TestTheCookieIsTheSameVariableTheHostAlreadySets(t *testing.T) {
	if CookieEnv != pluginproto.CookieEnv {
		t.Errorf("CookieEnv = %q, want %q", CookieEnv, pluginproto.CookieEnv)
	}
}
