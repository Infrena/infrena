package pluginproto

import (
	"encoding/json"
	"slices"
	"testing"
)

// The protocol version is the one thing standing between a plugin that needs a
// newer host and a silent wrong answer, so changing it must be deliberate
// rather than accidental.
//
// The literal is duplicated on purpose: reading it from the constant would
// assert only that the constant equals itself.
func TestTheProtocolVersionIsDeliberate(t *testing.T) {
	if Version != 6 {
		t.Errorf("Version = %d, want 6. Changing it is a deliberate act: raise this literal "+
			"together with the constant, and record what moved and why", Version)
	}
}

// 1 must stay in Supported. A plugin built before `optional` and `aliases`
// existed sends neither, and absent means what it always meant, so nothing
// about that plugin became wrong. Dropping 1 would orphan every plugin in
// existence, which is a major version's decision.
func TestOlderPluginsKeepWorking(t *testing.T) {
	if !slices.Contains(Supported, 1) {
		t.Errorf("Supported = %v, and 1 is gone. Every plugin built before today announces 1; "+
			"refusing them is a major version's decision, not a side effect", Supported)
	}
	if !IsSupported(1) {
		t.Error("IsSupported(1) is false, so an existing plugin would be refused at the handshake")
	}
}

// A host must accept the version its own SDK announces, or infrena would refuse
// a plugin built from the very same tree — which is what the integration suite
// builds on every run.
func TestThisBuildCanTalkToItself(t *testing.T) {
	if !IsSupported(Version) {
		t.Fatalf("Supported = %v does not contain Version = %d, so this build refuses a plugin "+
			"built with its own SDK", Supported, Version)
	}
}

// Supported's doc comment promises newest first, and a diagnostic listing what
// a host accepts reads better that way, but nothing else enforces it: a version
// appended in the obvious place would quietly break the promise.
func TestSupportedIsNewestFirst(t *testing.T) {
	if !slices.IsSortedFunc(Supported, func(a, b int) int { return b - a }) {
		t.Errorf("Supported = %v, want newest first as its doc comment states", Supported)
	}
}

func TestProtocolIsSixAndStillSpeaksItsPredecessors(t *testing.T) {
	if Version != 6 {
		t.Errorf("Version = %d, want 6 — `elem` on a schema attribute, describing a list's "+
			"elements, the same additive shape optional/aliases, References, system_owned and "+
			"max_concurrency had", Version)
	}
	for _, v := range []int{6, 5, 4, 3, 2, 1} {
		if !IsSupported(v) {
			t.Errorf("protocol %d must still be supported — Supported is a set so raising the version does not orphan every plugin", v)
		}
	}
	if IsSupported(7) {
		t.Error("an unreleased protocol must not be accepted")
	}
}

// Why 4 stays supported: a protocol 4 plugin sends no max_concurrency, absent
// decodes as zero, and zero means "no claim", so the host keeps its own
// conservative default. The dangerous direction is the reverse — a ceiling an
// older host silently ignored would run a plugin wider than it asked for, which
// is the failure the field exists to stop.
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

// 4 carries `system_owned` and `system_owned_reason` on a discovered resource:
// a plugin declaring that the cloud created and manages it, which the engine
// cannot work out for itself. It needed a version for the reason 2 and 3 did —
// a plugin built with this SDK talking to an older host would have both keys
// silently dropped, and a default VPC would be offered for adoption with
// nothing saying the plugin had flagged it.
func TestProtocolFourIsSupportedAlongsideItsPredecessors(t *testing.T) {
	// This is about 4 remaining supported, not about 4 being current: the current
	// version has its own tripwire above, and duplicating it here would make one
	// raise fail two tests about the same line.
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

// The compatibility claim, asserted rather than assumed. A plugin built before
// this version sends a discovered resource with neither new key, and the wire
// form is the contract: absent must decode as "the plugin said nothing", which
// is not system-owned and no reason.
func TestAProtocolThreeDiscoveryStillMeansWhatItMeant(t *testing.T) {
	var d Discovered
	if err := json.Unmarshal([]byte(`{"type":"aws.vpc","provider_id":"vpc-1"}`), &d); err != nil {
		t.Fatal(err)
	}
	if d.SystemOwned || d.SystemOwnedReason != "" {
		t.Errorf("a protocol 3 discovery decoded as %+v, want no claim at all", d)
	}
}
