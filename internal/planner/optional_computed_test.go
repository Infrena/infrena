package planner

import (
	"testing"

	"github.com/infrata/infrata/pkg/address"
	"github.com/infrata/infrata/pkg/resource"
	"github.com/infrata/infrata/pkg/schema"
	"github.com/infrata/infrata/pkg/value"
)

// azDefinition is a resource type with the shape §14.1 adds: an attribute configuration
// MAY set, which the provider picks when configuration does not. `availability_zone` is
// the real case — AWS chooses one if a subnet does not name it — and it is ForceNew,
// which is the awkward pairing worth pinning.
func azDefinition() *schema.ResourceDefinition {
	return &schema.ResourceDefinition{
		Type: "cloud.subnet",
		Attributes: map[string]schema.Attribute{
			"cidr": {Kind: value.KindString, Required: true},
			"availability_zone": {
				Kind: value.KindString, Computed: true, Optional: true, ForceNew: true,
			},
		},
		Capabilities: schema.Capabilities{Create: true, Read: true, Update: true, Delete: true},
	}
}

func subnetState(az string) *resource.ResourceState {
	return &resource.ResourceState{
		Address:    address.Address{Name: "net"},
		Type:       "cloud.subnet",
		Provider:   "cloud",
		ProviderID: "subnet-1",
		Attributes: map[string]value.Value{
			"cidr":              value.String("10.0.0.0/24", value.SourceProvider),
			"availability_zone": value.String(az, value.SourceProvider),
		},
	}
}

// TestAProviderChosenValueIsNotProposedForRemoval.
//
// The bug §14.1 exists to fix, and it was reproduced against the real binary before the
// design: the planner read a returned value that configuration does not set as "removed
// from configuration" and proposed to unset it. So the cloud picks an availability zone,
// infrata proposes unsetting it, the apply succeeds, the cloud picks again — and the
// project never converges. Invariant 2, broken for every attribute of this shape, of
// which a generic AWS provider has one on almost every resource.
func TestAProviderChosenValueIsNotProposedForRemoval(t *testing.T) {
	desired := map[string]value.Value{"cidr": value.String("10.0.0.0/24", value.SourceExplicit)}

	ops, _ := diffAttributes(address.Address{Name: "net"}, azDefinition(), desired,
		subnetState("eu-west-1a").Attributes)
	if len(ops) != 0 {
		t.Errorf("configuration does not set availability_zone, so the provider's choice is "+
			"authoritative and there is nothing to change; got %v", ops)
	}
}

// TestAProviderChosenValueChangingOutsideInfrataIsNotDrift.
//
// The accepted cost, asserted so that it is a decision rather than an accident: an
// attribute infrata does not manage is not infrata's to report. Someone moves the subnet
// or flips a console setting, and a plan says nothing — it is still visible through
// `refresh` and `state show`. Without this test, "no diff" could later be narrowed to
// "no diff only when the values happen to match" and nothing would notice.
func TestAProviderChosenValueChangingOutsideInfrataIsNotDrift(t *testing.T) {
	desired := map[string]value.Value{"cidr": value.String("10.0.0.0/24", value.SourceExplicit)}

	ops, _ := diffAttributes(address.Address{Name: "net"}, azDefinition(), desired,
		subnetState("eu-west-1c").Attributes)
	if len(ops) != 0 {
		t.Errorf("an unset optional+computed attribute is unmanaged, so a change to it is not "+
			"drift infrata reports; got %v", ops)
	}
}

// TestConfigurationSettingItMakesItAnOrdinaryAttribute is the control, and the half that
// matters most: the fix must not have made the attribute unsettable. When configuration
// DOES name a value differing from state, it is an ordinary diff — and because this
// attribute is ForceNew, a replacement rather than an update.
//
// Without this, a planner that ignored the attribute unconditionally would pass both
// tests above and silently refuse to move a subnet the user explicitly moved.
func TestConfigurationSettingItMakesItAnOrdinaryAttribute(t *testing.T) {
	desired := map[string]value.Value{
		"cidr":              value.String("10.0.0.0/24", value.SourceExplicit),
		"availability_zone": value.String("eu-west-1b", value.SourceExplicit),
	}

	ops, _ := diffAttributes(address.Address{Name: "net"}, azDefinition(), desired,
		subnetState("eu-west-1a").Attributes)
	if len(ops) != 1 {
		t.Fatalf("configuration names a different zone, so this is a change; got %v", ops)
	}
	if ops[0].Attribute != "availability_zone" {
		t.Errorf("the change names %q, want availability_zone", ops[0].Attribute)
	}
	if !ops[0].ForceNew {
		t.Error("availability_zone is ForceNew, so setting a different one must replace the " +
			"resource rather than update it in place")
	}
}

// TestAnUnsetProviderChosenValueIsCarriedIntoTheAfterState.
//
// Not diffing it must not mean forgetting it. The value the provider chose has to survive
// into the plan's After, or the executor records a resource whose availability zone is
// absent and the NEXT plan sees an attribute that appeared from nowhere.
func TestAnUnsetProviderChosenValueIsCarriedIntoTheAfterState(t *testing.T) {
	desired := map[string]value.Value{"cidr": value.String("10.0.0.0/24", value.SourceExplicit)}
	after := afterAttributes(azDefinition(), desired, subnetState("eu-west-1a").Attributes, OpNoOp)

	got, ok := after["availability_zone"]
	if !ok {
		t.Fatal("the provider's chosen value is missing from After, so it would be lost from state")
	}
	if s, _ := got.AsString(); s != "eu-west-1a" {
		t.Errorf("After carries availability_zone = %q, want eu-west-1a", s)
	}
}
