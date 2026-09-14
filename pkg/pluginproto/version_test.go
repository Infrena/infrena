package pluginproto

import (
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
	if Version != 2 {
		t.Errorf("Version = %d, want 2. Changing it is a deliberate act: raise this literal "+
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
// announces, or infrata would refuse a plugin built from the very same tree — which is
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
