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

	files, _, err := Generate(resources, reg, MinimalOptions())
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

	files, _, err := Generate(resources, reg, MinimalOptions())
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

	files, _, err := Generate(resources, reg, MinimalOptions())
	if err != nil {
		t.Fatal(err)
	}

	if got := fileNamed(t, files, FileName("aws.lb")); !strings.Contains(got, "${subnet-a}") {
		t.Errorf("list entry did not project:\n%s", got)
	}
}

// Invariant 3. The reference resolves at compile time to the same literal the
// attribute held, so the round trip still plans clean.
//
// The end-to-end half of this claim — discover, import, generate, plan — is
// TestTheImportRoundTripPlansClean in tests/integration, because only the real
// binary can make it. What is provable here is the part that broke it: a
// reference IS a dependency edge, so the edges reported must be exactly the
// references emitted.
func TestEveryEmittedReferenceIsReportedAsAnEdge(t *testing.T) {
	reg := registryWithTypes(t, map[string]map[string]schema.Attribute{
		"aws.vpc":    {"VpcId": {Kind: value.KindString, Computed: true}},
		"aws.subnet": {"SubnetId": {Kind: value.KindString, Computed: true}, "VpcId": {Kind: value.KindString, References: &schema.Reference{Type: "aws.vpc", Attribute: "VpcId"}}},
		"aws.lb": {"SubnetIds": {Kind: value.KindList,
			References: &schema.Reference{Type: "aws.subnet", Attribute: "SubnetId"}}},
	})
	resources := []Resource{
		{Name: "vpc-app1", Type: "aws.vpc", ProviderID: "vpc-1", Attributes: map[string]value.Value{
			"VpcId": value.String("vpc-1", value.SourceProvider)}},
		{Name: "subnet-app1a", Type: "aws.subnet", ProviderID: "subnet-a", Attributes: map[string]value.Value{
			"SubnetId": value.String("subnet-a", value.SourceProvider),
			"VpcId":    value.String("vpc-1", value.SourceProvider)}},
		{Name: "lb-app1", Type: "aws.lb", ProviderID: "lb-1", Attributes: map[string]value.Value{
			"SubnetIds": value.List([]value.Value{value.String("subnet-a", value.SourceProvider)}, value.SourceProvider)}},
	}

	files, edges, err := Generate(resources, reg, MinimalOptions())
	if err != nil {
		t.Fatal(err)
	}

	// Read back out of the BYTES, which is what the compiler will do, rather
	// than trusting the same pass twice.
	emitted := map[string][]string{
		"subnet-app1a": {"vpc-app1"},
		"lb-app1":      {"subnet-app1a"},
	}
	for name, want := range emitted {
		got := strings.Join(edges[name], ",")
		if got != strings.Join(want, ",") {
			t.Errorf("edges[%q] = %v, want %v", name, edges[name], want)
		}
	}
	if len(edges) != len(emitted) {
		t.Errorf("edges = %v, want an entry only for the two resources that reference something", edges)
	}
	for _, f := range files {
		for _, name := range []string{"vpc-app1", "subnet-app1a"} {
			if !strings.Contains(string(f.Bytes), "${"+name+"}") {
				continue
			}
			// Whatever file it landed in, some resource in edges must claim it.
			claimed := false
			for _, targets := range edges {
				for _, target := range targets {
					if target == name {
						claimed = true
					}
				}
			}
			if !claimed {
				t.Errorf("%s emits ${%s} but no edge reports it:\n%s", f.Name, name, f.Bytes)
			}
		}
	}
}

// An edge the file does not declare is the same bug in the other direction:
// state would carry a dependency configuration never mentions, and the first
// plan would propose removing it.
func TestAnUnresolvedReferenceIsNotAnEdge(t *testing.T) {
	reg := registryWithTypes(t, map[string]map[string]schema.Attribute{
		"aws.vpc":    {"VpcId": {Kind: value.KindString, Computed: true}},
		"aws.subnet": {"VpcId": {Kind: value.KindString, References: &schema.Reference{Type: "aws.vpc", Attribute: "VpcId"}}},
	})
	resources := []Resource{
		{Name: "subnet-app1a", Type: "aws.subnet", ProviderID: "subnet-1", Attributes: map[string]value.Value{
			"VpcId": value.String("vpc-not-discovered", value.SourceProvider)}},
	}

	_, edges, err := Generate(resources, reg, MinimalOptions())
	if err != nil {
		t.Fatal(err)
	}

	if len(edges) != 0 {
		t.Errorf("edges = %v, want none: the literal survived, so the file declares no dependency", edges)
	}
}

// THE REASON THE EDGES COME FROM GENERATION rather than from a second pass over
// the same attributes. An attribute equal to its default is not written at all,
// so the file declares no reference and state must record no edge — a recomputed
// answer that did not know about minimality would record one and the round trip
// would propose removing it.
func TestAnAttributeOmittedAsADefaultIsNotAnEdge(t *testing.T) {
	reg := registryWithTypes(t, map[string]map[string]schema.Attribute{
		"aws.vpc": {"VpcId": {Kind: value.KindString, Computed: true}},
		"aws.subnet": {"VpcId": {Kind: value.KindString, Default: "vpc-1",
			References: &schema.Reference{Type: "aws.vpc", Attribute: "VpcId"}}},
	})
	resources := []Resource{
		{Name: "vpc-app1", Type: "aws.vpc", ProviderID: "vpc-1", Attributes: map[string]value.Value{
			"VpcId": value.String("vpc-1", value.SourceProvider)}},
		{Name: "subnet-app1a", Type: "aws.subnet", ProviderID: "subnet-1", Attributes: map[string]value.Value{
			"VpcId": value.String("vpc-1", value.SourceProvider)}},
	}

	files, edges, err := Generate(resources, reg, MinimalOptions())
	if err != nil {
		t.Fatal(err)
	}

	if got := fileNamed(t, files, FileName("aws.subnet")); strings.Contains(got, "${") {
		t.Fatalf("the fixture no longer omits the attribute, so it proves nothing:\n%s", got)
	}
	if len(edges) != 0 {
		t.Errorf("edges = %v, want none: nothing was written, so nothing is depended on", edges)
	}
}
