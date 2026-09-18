package backendhost

import (
	"context"
	"strings"
	"testing"
)

// ValidateConfig is protocol 2's `validate`: it asks the backend whether a
// `backend:` block is readable, and it is the ONLY way the engine can ask,
// because a backend's settings are understood by that backend alone.
//
// It closes the case Verify leaves open. Verify resolves a binary and stops;
// the block's contents used to be checked only by Configure, which builds a
// client and — in the s3 backend — proves the store honours conditional writes.
// Those are round trips, and `validate` promises it will make none. So a
// credential committed into infra.yml passed `validate` and was refused at
// `plan`: the cheap CI gate approving exactly the mistake that reaches a
// repository through CI.

func TestValidateConfigRefusesABlockTheBackendCannotRead(t *testing.T) {
	dir := buildFakeBackend(t)

	err := ValidateConfig(context.Background(), "picky", "", []string{dir},
		map[string]any{"secret": "AKIAIOSFODNN7EXAMPLE"})
	if err == nil {
		t.Fatal("a credential in the block was accepted")
	}
	if !strings.Contains(err.Error(), "credential") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

func TestValidateConfigAcceptsABlockTheBackendCanRead(t *testing.T) {
	dir := buildFakeBackend(t)

	if err := ValidateConfig(context.Background(), "picky", "", []string{dir},
		map[string]any{"bucket": "myapp-state"}); err != nil {
		t.Fatalf("a readable block was refused: %v", err)
	}
}

// TestValidateConfigNeverCallsConfigure is what keeps validate's promise.
//
// The picky backend's Configure fails outright on `explode`, and its
// ValidateConfig does not look at that key at all. A pass here therefore proves
// the host asked the OFFLINE question — if validate were implemented by
// configuring, this block would be refused and the network would have been
// contacted to do it.
func TestValidateConfigNeverCallsConfigure(t *testing.T) {
	dir := buildFakeBackend(t)

	if err := ValidateConfig(context.Background(), "picky", "", []string{dir},
		map[string]any{"explode": true}); err != nil {
		t.Fatalf("validate went through Configure: %v", err)
	}
}

// TestValidateConfigIsSilentWithoutTheOptionalMethod. A backend that reads
// configuration but has no offline opinion about it answers UNSUPPORTED, and
// the host must treat that as "ask no further" rather than as a failure —
// otherwise adding the method would break every backend that has not
// implemented it, which is precisely what `Supported` being a set promises will
// not happen.
func TestValidateConfigIsSilentWithoutTheOptionalMethod(t *testing.T) {
	dir := buildFakeBackend(t)

	if err := ValidateConfig(context.Background(), "configonly", "", []string{dir},
		map[string]any{"anything": "at all"}); err != nil {
		t.Fatalf("a backend without the optional method reported a failure: %v", err)
	}
}

// TestValidateConfigRefusesConfigurationABackendCannotTakeAtAll. A backend
// implementing neither optional method still has one offline answer worth
// giving: a key it will silently ignore. That refusal used to wait for
// Configure, and a key that does nothing is a user who believes their state is
// in one place while it is written to another.
func TestValidateConfigRefusesConfigurationABackendCannotTakeAtAll(t *testing.T) {
	dir := buildFakeBackend(t)

	err := ValidateConfig(context.Background(), "memory", "", []string{dir},
		map[string]any{"bucket": "myapp-state"})
	if err == nil {
		t.Fatal("configuration sent to a backend that takes none was accepted")
	}
	if !strings.Contains(err.Error(), "takes no configuration") {
		t.Errorf("the refusal does not explain itself: %v", err)
	}

	// And an empty block is fine, which is the ordinary case for such a backend.
	if err := ValidateConfig(context.Background(), "memory", "", []string{dir}, nil); err != nil {
		t.Errorf("an empty block was refused: %v", err)
	}
}

// TestValidateConfigReportsAMissingBackend rather than passing quietly: the
// binary has to be found before anything can be asked of it.
func TestValidateConfigReportsAMissingBackend(t *testing.T) {
	err := ValidateConfig(context.Background(), "nosuchbackend", "", []string{t.TempDir()}, nil)
	if err == nil {
		t.Fatal("a backend that is not installed validated successfully")
	}
}
