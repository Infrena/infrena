// Package plugintest lets a provider plugin's own tests run it the way infrena does.
//
// A plugin is a separate binary, so the interesting question for its author is not
// "does my Provider return the right thing" — that is an ordinary unit test — but
// "does it still behave once it has been through the protocol". Encoding, decoding,
// schema validation and every rule the host enforces sit between a plugin and the
// engine, and none of them are exercised by calling a method directly.
//
//	host, err := plugintest.Open(ctx, myplugin.New(), t.TempDir())
//	if err != nil { t.Fatal(err) }        // schemas failed validation
//	defer host.Close()
//
//	prov, err := host.Configure(provider.Config{Instance: "main"})
//	if err != nil { t.Fatal(err) }
//	got, err := prov.Read(ctx, existing)  // through the real protocol
//
// No process is started: one end of an in-memory pipe runs pluginsdk.Serve, the other
// runs infrena's own host. It is the same host code a subprocess talks to, so
// there is no second implementation to drift from the first — a drift that would
// be invisible, with each side still agreeing with itself.
//
// It is exported because the host itself lives under internal/ and is therefore
// unreachable from another module, which would make "test against the real host"
// advice nobody could follow.
package plugintest

import (
	"context"

	"github.com/infrena/infrena/internal/pluginhost"
	"github.com/infrena/infrena/pkg/provider"
	"github.com/infrena/infrena/pkg/schema"
)

// Host is a connected plugin, as infrena sees it.
type Host struct {
	plugin *pluginhost.Plugin
}

// Open connects a plugin and fetches its schemas.
//
// It FAILS if the schemas do not pass the checks infrena applies on load — a type
// outside the plugin's own name prefix, an attribute named after a lifecycle option,
// a malformed definition, a default that is not the kind its attribute declares, and
// a References naming a type or attribute the plugin does not declare.
// Which makes `Open` worth a test of its own: it is the cheapest possible check that
// a plugin is loadable at all.
//
// dir is the project directory a configured instance is given, for resolving a
// relative path a user wrote. t.TempDir() is the usual answer.
func Open(ctx context.Context, p provider.Plugin, dir string) (*Host, error) {
	host, err := pluginhost.InProcess(ctx, p, dir)
	if err != nil {
		return nil, err
	}
	return &Host{plugin: host}, nil
}

// Configure returns one configured instance, reached through the protocol.
//
// The returned Provider is the host's own adapter, so every rule the engine applies
// to a plugin's answers applies here: bookkeeping re-attached, sensitivity forced
// from the schema, provenance overwritten, an undeclared attribute refused, and a
// (nil, nil) from Create or Update turned into an error.
func (h *Host) Configure(cfg provider.Config) (provider.Provider, error) {
	return h.plugin.New(cfg)
}

// Definitions are the schemas the plugin sent, after validation.
//
// Worth asserting against directly: these are what `infrena explain` renders and what
// the compiler checks configuration against, and they have been through JSON, so a
// default that does not survive the round trip shows up here.
func (h *Host) Definitions() []*schema.ResourceDefinition {
	return h.plugin.Definitions()
}

// Version is what the plugin reported in its handshake.
func (h *Host) Version() string { return h.plugin.Version() }

// Close shuts the plugin down.
func (h *Host) Close() error { return h.plugin.Close() }
