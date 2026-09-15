package generator

import (
	"context"
	"strings"
	"testing"

	"github.com/infrena/infrena/internal/registry"
	"github.com/infrena/infrena/pkg/provider"
	"github.com/infrena/infrena/pkg/resource"
	"github.com/infrena/infrena/pkg/schema"
	"github.com/infrena/infrena/pkg/value"
)

// refStubProvider is the minimum a registry needs: this file's subject is what
// the generator does with a DECLARED relationship, and nothing here ever calls
// the provider.
type refStubProvider struct {
	defs []*schema.ResourceDefinition
}

func (s refStubProvider) Name() string                              { return "aws" }
func (s refStubProvider) Definitions() []*schema.ResourceDefinition { return s.defs }
func (s refStubProvider) ClassifyError(error) provider.Retryability { return provider.NotSafeToRetry }
func (s refStubProvider) Read(context.Context, *resource.ResourceState) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (s refStubProvider) Create(context.Context, *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (s refStubProvider) Update(context.Context, *resource.ResourceState, *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (s refStubProvider) Delete(context.Context, *resource.ResourceState) error {
	return provider.ErrNotImplemented
}
func (s refStubProvider) Discover(context.Context, provider.DiscoverRequest) ([]provider.DiscoveredResource, error) {
	return nil, provider.ErrNotImplemented
}
func (s refStubProvider) Import(context.Context, string, string) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}

// registryWithTypes builds a registry holding one plugin that declares every
// given type. ONE plugin, because a References declaration must name a type the
// same plugin declares and the registry refuses it otherwise.
func registryWithTypes(t *testing.T, types map[string]map[string]schema.Attribute) *registry.Registry {
	t.Helper()
	defs := make([]*schema.ResourceDefinition, 0, len(types))
	for name, attrs := range types {
		defs = append(defs, &schema.ResourceDefinition{Type: name, Attributes: attrs})
	}
	reg := registry.New()
	if err := reg.Register("aws", refStubProvider{defs: defs}); err != nil {
		t.Fatalf("registering the stub provider: %v", err)
	}
	return reg
}

// fileNamed returns the generated file with the given name.
func fileNamed(t *testing.T, files []File, name string) string {
	t.Helper()
	for _, f := range files {
		if f.Name == name {
			return string(f.Bytes)
		}
	}
	t.Fatalf("no file named %q was generated; got %d files", name, len(files))
	return ""
}

func TestGeneratedSubnetReferencesTheDiscoveredVPC(t *testing.T) {
	reg := registryWithTypes(t, map[string]map[string]schema.Attribute{
		"aws.vpc":    {"VpcId": {Kind: value.KindString, Computed: true}},
		"aws.subnet": {"VpcId": {Kind: value.KindString, References: &schema.Reference{Type: "aws.vpc", Attribute: "VpcId"}}},
	})
	resources := []Resource{
		{Name: "vpc-app1", Type: "aws.vpc", ProviderID: "vpc-1023902339", Attributes: map[string]value.Value{
			"VpcId": value.String("vpc-1023902339", value.SourceProvider)}},
		{Name: "subnet-app1a", Type: "aws.subnet", ProviderID: "subnet-1", Attributes: map[string]value.Value{
			"VpcId": value.String("vpc-1023902339", value.SourceProvider)}},
	}

	files, err := Generate(resources, reg, MinimalOptions())
	if err != nil {
		t.Fatal(err)
	}

	got := fileNamed(t, files, FileName("aws.subnet"))
	if !strings.Contains(got, "${vpc-app1}") {
		t.Errorf("subnet does not reference the VPC:\n%s", got)
	}
	if strings.Contains(got, "vpc-1023902339") {
		t.Errorf("subnet still carries the literal:\n%s", got)
	}
}

// A reference whose target was not discovered would be a compile error in a
// file the user never wrote. The literal survives, and the omission is stated
// where a reader meets it.
func TestAnUnmatchedReferenceKeepsItsLiteralAndSaysSo(t *testing.T) {
	reg := registryWithTypes(t, map[string]map[string]schema.Attribute{
		"aws.vpc":    {"VpcId": {Kind: value.KindString, Computed: true}},
		"aws.subnet": {"VpcId": {Kind: value.KindString, References: &schema.Reference{Type: "aws.vpc", Attribute: "VpcId"}}},
	})
	resources := []Resource{
		{Name: "subnet-app1a", Type: "aws.subnet", ProviderID: "subnet-1", Attributes: map[string]value.Value{
			"VpcId": value.String("vpc-not-discovered", value.SourceProvider)}},
	}

	files, err := Generate(resources, reg, MinimalOptions())
	if err != nil {
		t.Fatal(err)
	}

	got := fileNamed(t, files, FileName("aws.subnet"))
	if !strings.Contains(got, "vpc-not-discovered") {
		t.Errorf("literal was dropped:\n%s", got)
	}
	if !strings.Contains(got, "not discovered") {
		t.Errorf("omission is not stated in the file:\n%s", got)
	}
}

// The AWS plugin emits 97 list edges, so a list of ids must project too.
func TestAListOfReferencesProjectsEachEntry(t *testing.T) {
	reg := registryWithTypes(t, map[string]map[string]schema.Attribute{
		"aws.subnet": {"SubnetId": {Kind: value.KindString, Computed: true}},
		"aws.lb": {"SubnetIds": {Kind: value.KindList,
			References: &schema.Reference{Type: "aws.subnet", Attribute: "SubnetId"}}},
	})
	resources := []Resource{
		{Name: "subnet-a", Type: "aws.subnet", ProviderID: "subnet-a", Attributes: map[string]value.Value{
			"SubnetId": value.String("subnet-a", value.SourceProvider)}},
		{Name: "lb-app1", Type: "aws.lb", ProviderID: "lb-1", Attributes: map[string]value.Value{
			"SubnetIds": value.List([]value.Value{value.String("subnet-a", value.SourceProvider)}, value.SourceProvider)}},
	}

	files, err := Generate(resources, reg, MinimalOptions())
	if err != nil {
		t.Fatal(err)
	}

	if got := fileNamed(t, files, FileName("aws.lb")); !strings.Contains(got, "${subnet-a}") {
		t.Errorf("list entry did not project:\n%s", got)
	}
}

// Invariant 3. The reference resolves at compile time to the same literal the
// attribute held, so the round trip still plans clean.
func TestReferencesDoNotBreakTheRoundTrip(t *testing.T) {
	// Build the project from the generated files and assert planner.Compute
	// returns zero operations, following the existing round-trip test in
	// tests/integration. Read it and extend it rather than writing a second.
	t.Skip("implemented as an extension of the existing round-trip integration test in Task 9")
}
