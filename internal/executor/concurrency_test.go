package executor

import "testing"

// TestADeclaredCeilingOverridesTheDefault, and the floor under it.
//
// The number now comes from a third party, so "a plugin got its own arithmetic
// wrong" stopped being hypothetical. A ceiling of zero would admit no work at
// all and the run would hang holding a state lock, which is worse than ignoring
// the claim.
func TestADeclaredCeilingOverridesTheDefault(t *testing.T) {
	opts := Options{PerProvider: 8, ProviderLimits: map[string]int{
		"slowcloud": 2,
		"broken":    0,
		"negative":  -1,
	}}

	for name, want := range map[string]int{
		"slowcloud": 2, // declared, honoured
		"broken":    1, // declared nonsense, floored rather than deadlocking
		"negative":  1,
		"quiet":     8, // made no claim, keeps the host's default
	} {
		if got := providerCeiling(opts, name); got != want {
			t.Errorf("providerCeiling(%q) = %d, want %d", name, got, want)
		}
	}
}

// TestNoDeclarationsAtAllKeepsTheDefault. The map is nil for every project
// whose plugins predate protocol 5, which is all of them today.
func TestNoDeclarationsAtAllKeepsTheDefault(t *testing.T) {
	if got := providerCeiling(Options{PerProvider: 8}, "aws"); got != 8 {
		t.Errorf("providerCeiling with no declarations = %d, want the default 8", got)
	}
}
