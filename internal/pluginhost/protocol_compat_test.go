package pluginhost

import (
	"bufio"
	"context"
	"testing"

	"github.com/infrena/infrena/pkg/pluginproto"
	"github.com/infrena/infrena/pkg/provider"
)

// TestAProtocolThreePluginStillLoadsAndDiscovers.
//
// THE COMPATIBILITY CLAIM OF PROTOCOL 4, asserted against the host rather than
// against a constant. Raising Version to 4 for `system_owned` is only safe
// because Supported is a SET: a plugin built before it announces 3, sends a
// discovery answer with neither new key, and must load and behave exactly as it
// did yesterday. A version bump that quietly dropped such a plugin is not what
// this was.
//
// Written at the wire rather than through the SDK on purpose. The SDK in this
// tree announces 4 and always will, so a test that used it could never produce
// the message an older plugin sends — which is the only message worth checking.
func TestAProtocolThreePluginStillLoadsAndDiscovers(t *testing.T) {
	hostReader, pluginWriter := ioPipe()
	pluginReader, hostWriter := ioPipe()

	requests := bufio.NewScanner(pluginReader)
	go func() {
		defer pluginWriter.Close()
		// Protocol 3: what every plugin released before today announces.
		if _, err := pluginWriter.Write([]byte(
			`{"protocol":3,"name":"old","version":"0.1.0"}` + "\n")); err != nil {
			return
		}
		if !requests.Scan() {
			return
		}
		schemas := `{"id":1,"result":{"definitions":[{"Type":"old.thing",` +
			`"Attributes":{"name":{"kind":"string","required":true}},` +
			`"Capabilities":{"Create":true,"Read":true,"Delete":true}}]}}` + "\n"
		if _, err := pluginWriter.Write([]byte(schemas)); err != nil {
			return
		}
		if !requests.Scan() {
			return
		}
		// No system_owned, no system_owned_reason: the message shape a
		// protocol 3 plugin produces.
		found := `{"id":2,"result":{"found":[{"type":"old.thing","provider_id":"t-1"}]}}` + "\n"
		_, _ = pluginWriter.Write([]byte(found))
	}()

	c := newTestClient(hostWriter)
	defer pluginReader.Close()
	if err := c.start(hostReader, "old"); err != nil {
		t.Fatalf("a protocol 3 plugin was refused at the handshake: %v", err)
	}
	p := &Plugin{client: c}
	if err := p.loadSchemas(context.Background()); err != nil {
		t.Fatalf("loadSchemas: %v", err)
	}

	r := &remoteProvider{plugin: p, handle: "h", instance: "old"}
	found, err := r.Discover(context.Background(), provider.DiscoverRequest{})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(found) != 1 || found[0].ProviderID != "t-1" {
		t.Fatalf("Discover = %+v, want the one resource the plugin reported", found)
	}
	// Absence is a claim of nothing, which is exactly what it meant before the
	// field existed. That is what makes it safe to add before any plugin sets it.
	if found[0].SystemOwned || found[0].SystemOwnedReason != "" {
		t.Errorf("a protocol 3 plugin's resource came back as %+v, want no system-owned claim",
			found[0])
	}
	if !pluginproto.IsSupported(3) {
		t.Error("IsSupported(3) is false, so this plugin would not have loaded at all")
	}
}
