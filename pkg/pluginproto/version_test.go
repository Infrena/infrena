package pluginproto

import (
	"encoding/json"
	"slices"
	"testing"
)

// TestTheProtocolVersionIsDeliberate.
//
// Established by sabotage: reverting Version to 1 and Supported to {1} broke NOTHING in
// the whole suite. The protocol version is the one thing standing between a plugin that
// needs a newer host and a silent wrong answer, and until this test existed it could be
// changed — or forgotten — by accident.
//
// The literal is duplicated on purpose. Reading it from the constant would assert that
// the constant equals itself.
func TestTheProtocolVersionIsDeliberate(t *testing.T) {
	if Version != 5 {
		t.Errorf("Version = %d, want 5. Changing it is a deliberate act: raise this literal "+
			"together with the constant, and say in PLAN.md §61 what moved and why", Version)
	}
}

// TestOlderPluginsKeepWorking.
//
// 1 must stay in Supported. A plugin built before PLAN.md §14.1 sends no `optional` and no
// `aliases`, and absent means exactly what it meant then — so nothing about that plugin
// became wrong and nothing about it should stop working. Dropping 1 would orphan every
// plugin in existence, which §61.1 calls a MAJOR and which no change of this kind
// justifies.
func TestOlderPluginsKeepWorking(t *testing.T) {
	if !slices.Contains(Supported, 1) {
		t.Errorf("Supported = %v, and 1 is gone. Every plugin built before today announces 1; "+
			"refusing them is a major version's decision, not a side effect", Supported)
	}
	if !IsSupported(1) {
		t.Error("IsSupported(1) is false, so an existing plugin would be refused at the handshake")
	}
}

// TestThisBuildCanTalkToItself.
//
// The invariant that makes the pair coherent: a host must accept the version its own SDK
// announces, or infrena would refuse a plugin built from the very same tree — which is
// exactly what the integration suite does on every run.
func TestThisBuildCanTalkToItself(t *testing.T) {
	if !IsSupported(Version) {
		t.Fatalf("Supported = %v does not contain Version = %d, so this build refuses a plugin "+
			"built with its own SDK", Supported, Version)
	}
}

// TestSupportedIsNewestFirst. Its doc comment promises the order, and a diagnostic listing
// what a host accepts reads better newest-first — but nothing enforced it, so a version
// appended in the obvious place would have quietly broken the promise.
func TestSupportedIsNewestFirst(t *testing.T) {
	if !slices.IsSortedFunc(Supported, func(a, b int) int { return b - a }) {
		t.Errorf("Supported = %v, want newest first as its doc comment states", Supported)
	}
}

func TestProtocolIsFiveAndStillSpeaksItsPredecessors(t *testing.T) {
	if Version != 5 {
		t.Errorf("Version = %d, want 5 — a plugin-declared concurrency ceiling in the handshake, "+
			"the same additive shape optional/aliases, References and system_owned had", Version)
	}
	for _, v := range []int{5, 4, 3, 2, 1} {
		if !IsSupported(v) {
			t.Errorf("protocol %d must still be supported — Supported is a set so raising the version does not orphan every plugin", v)
		}
	}
	if IsSupported(6) {
		t.Error("an unreleased protocol must not be accepted")
	}
}

// TestAPluginThatDeclaresNoCeilingIsUnaffected is why 4 stays supported, stated
// as a test rather than left to the reasoning in the constant's doc comment.
//
// A protocol 4 plugin sends no max_concurrency, absent decodes as zero, and zero
// means "no claim" — so the host keeps its own conservative default and nothing
// about that plugin becomes wrong. The direction that WOULD have been dangerous
// is the reverse: a ceiling an older host silently ignored would run a plugin
// wider than it asked for, which is the failure this whole field exists to stop.
func TestAPluginThatDeclaresNoCeilingIsUnaffected(t *testing.T) {
	var h Handshake
	if err := json.Unmarshal([]byte(`{"protocol":4,"name":"aws","version":"1.0.0"}`), &h); err != nil {
		t.Fatalf("decoding a protocol 4 handshake: %v", err)
	}
	if h.MaxConcurrency != 0 {
		t.Errorf("MaxConcurrency = %d from a handshake that carries none, want 0 (no claim)", h.MaxConcurrency)
	}
	if !IsSupported(h.Protocol) {
		t.Error("a protocol 4 plugin must still be accepted: it is not wrong, it is quiet")
	}
}

// TestProtocolFourIsSupportedAlongsideItsPredecessors.
//
// 4 carries `system_owned` and `system_owned_reason` on a discovered resource
// (PLAN.md §31.1): a plugin declaring that the CLOUD created and manages a
// resource, which the engine cannot work out for itself without learning about
// AWS. The reason it needs a version is the reason 2 and 3 did — a plugin built
// with this SDK talking to an OLDER host would have both keys silently dropped,
// and a default VPC would be offered for adoption with nothing anywhere saying
// the plugin had flagged it.
func TestProtocolFourIsSupportedAlongsideItsPredecessors(t *testing.T) {
	// This test is about 4 REMAINING SUPPORTED, not about 4 being current. It
	// asserted the version too, which made it a second copy of the deliberate
	// version tripwire above and meant raising the version failed two tests
	// saying different things about the same line. Version 5 left 4 exactly as
	// it was, which is the claim this test should have been making all along.
	if !IsSupported(4) {
		t.Error("protocol 4 must stay supported: system_owned did not become wrong when 5 arrived")
	}
	// A protocol 3 plugin reports nothing and behaves exactly as today.
	// Absence costs what it costs now, which is what makes it safe to add a
	// field before any plugin sets it.
	for _, v := range []int{4, 3, 2, 1} {
		if !slices.Contains(Supported, v) {
			t.Errorf("Supported does not include %d", v)
		}
	}
}

// TestAProtocolThreeDiscoveryStillMeansWhatItMeant.
//
// THE COMPATIBILITY CLAIM, asserted rather than assumed. A plugin built before
// this version sends a discovered resource with neither new key, and the wire
// form is the contract: absent must decode as "the plugin said nothing", which
// is not-system-owned with no reason — exactly the behaviour every plugin in
// existence already gets. Without this, a future reader has only the doc
// comment's word for it.
func TestAProtocolThreeDiscoveryStillMeansWhatItMeant(t *testing.T) {
	var d Discovered
	if err := json.Unmarshal([]byte(`{"type":"aws.vpc","provider_id":"vpc-1"}`), &d); err != nil {
		t.Fatal(err)
	}
	if d.SystemOwned || d.SystemOwnedReason != "" {
		t.Errorf("a protocol 3 discovery decoded as %+v, want no claim at all", d)
	}
}
