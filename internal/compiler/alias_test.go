package compiler

import (
	"context"
	"strings"
	"testing"

	"github.com/infrata/infrata/internal/diag"
	"github.com/infrata/infrata/internal/registry"
	"github.com/infrata/infrata/pkg/provider"
	"github.com/infrata/infrata/pkg/resource"
	"github.com/infrata/infrata/pkg/schema"
	"github.com/infrata/infrata/pkg/value"
)

// aliasedDef is an attribute with several accepted spellings, the shape an AWS plugin
// generates: CloudFormation's own property name, its snake_case form, and a curated short
// one.
func aliasedDef() *schema.ResourceDefinition {
	return &schema.ResourceDefinition{
		Type: "aws.ec2.vpc",
		Attributes: map[string]schema.Attribute{
			"CidrBlock": {Kind: value.KindString, Required: true, Aliases: []string{"cidr", "cidr_block"}},
		},
		Capabilities: schema.Capabilities{Create: true, Read: true, Delete: true},
	}
}

func explicit(s string) value.Value { return value.String(s, value.SourceExplicit) }

// TestAnAliasIsRewrittenToThePluginsOwnName.
//
// The canonicalisation boundary (PLAN.md §14.1). Everything downstream of it —
// markSensitive, the planner's diff, the host's undeclared-attribute refusal, the
// generator, state, the plan artifact, the plugin wire — looks attributes up by EXACT
// name. Two of those are actively unsafe if an alias slips past: a sensitive attribute
// written under an alias would never be marked sensitive and would print in clear, and
// the planner would see one attribute under two spellings and propose a change forever.
//
// So this asserts the map itself is rewritten, not merely that the name was accepted.
func TestAnAliasIsRewrittenToThePluginsOwnName(t *testing.T) {
	for _, written := range []string{"cidr", "cidr_block", "cidrblock", "CIDR"} {
		attrs := map[string]value.Value{written: explicit("10.0.0.0/16")}
		var ds diag.Diagnostics
		canonicaliseAttributes(attrs, aliasedDef(), &ds)

		if ds.HasErrors() {
			t.Errorf("%q reported an error: %v", written, ds)
			continue
		}
		if _, ok := attrs["CidrBlock"]; !ok {
			t.Errorf("%q was not rewritten; the map holds %v", written, keysOf(attrs))
		}
		if written != "CidrBlock" {
			if _, stale := attrs[written]; stale {
				t.Errorf("%q survived alongside the canonical name, so the attribute is set twice", written)
			}
		}
	}
}

// TestTheCanonicalNameIsLeftAlone is the control: the overwhelmingly common case must not
// be disturbed, and a plugin with no aliases at all must behave exactly as before.
func TestTheCanonicalNameIsLeftAlone(t *testing.T) {
	attrs := map[string]value.Value{"CidrBlock": explicit("10.0.0.0/16")}
	var ds diag.Diagnostics
	canonicaliseAttributes(attrs, aliasedDef(), &ds)

	if ds.HasErrors() {
		t.Fatalf("the canonical name reported an error: %v", ds)
	}
	if len(attrs) != 1 {
		t.Errorf("the map gained or lost keys: %v", keysOf(attrs))
	}
}

// TestAnUnknownNameIsLeftForTheExistingDiagnostic.
//
// Canonicalisation deliberately does NOT report an unresolvable name, so that
// checkConfiguredAttributes reports it once, with the list of attributes the type
// actually has. Reporting in both places would tell a user about one typo twice, in two
// different wordings.
func TestAnUnknownNameIsLeftForTheExistingDiagnostic(t *testing.T) {
	attrs := map[string]value.Value{"nosuchthing": explicit("x")}
	var ds diag.Diagnostics
	canonicaliseAttributes(attrs, aliasedDef(), &ds)

	if ds.HasErrors() {
		t.Errorf("an unknown name must be left for checkConfiguredAttributes: %v", ds)
	}
	if _, ok := attrs["nosuchthing"]; !ok {
		t.Error("the unknown name was removed, so the later diagnostic cannot name what the user wrote")
	}
}

// TestTwoSpellingsOfOneAttributeAreRefusedNamingBoth.
//
// Same shape as §4.1's name-declared-twice rule, and for the same reason: last-one-wins
// would apply one value and silently discard the other, and which one won would depend on
// map order. The message has to name both spellings or the reader cannot see the pair.
func TestTwoSpellingsOfOneAttributeAreRefusedNamingBoth(t *testing.T) {
	attrs := map[string]value.Value{
		"cidr":      explicit("10.0.0.0/16"),
		"CidrBlock": explicit("10.1.0.0/16"),
	}
	var ds diag.Diagnostics
	canonicaliseAttributes(attrs, aliasedDef(), &ds)

	if !ds.HasErrors() {
		t.Fatal("one attribute set twice under two spellings must be refused, not resolved by map order")
	}
	var out strings.Builder
	ds.Render(&out)
	for _, want := range []string{"cidr", "CidrBlock"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the diagnostic does not name %q:\n%s", want, out.String())
		}
	}
}

// TestAnOptionalComputedAttributeMaySetInConfiguration.
//
// The compiler used to refuse EVERY computed attribute with "is computed and cannot be
// set". §14.1 adds the third state, so only a computed attribute that is not Optional is
// refused — and the control below keeps that refusal honest.
func TestAnOptionalComputedAttributeMaySetInConfiguration(t *testing.T) {
	def := &schema.ResourceDefinition{
		Type: "cloud.subnet",
		Attributes: map[string]schema.Attribute{
			"availability_zone": {Kind: value.KindString, Computed: true, Optional: true},
			"arn":               {Kind: value.KindString, Computed: true},
		},
		Capabilities: schema.Capabilities{Create: true, Read: true, Delete: true},
	}

	var ds diag.Diagnostics
	checkConfiguredAttributes(map[string]value.Value{"availability_zone": explicit("eu-west-1a")},
		def, value.Origin{}, &ds)
	if ds.HasErrors() {
		t.Errorf("configuration may set an optional+computed attribute: %v", ds)
	}

	var refused diag.Diagnostics
	checkConfiguredAttributes(map[string]value.Value{"arn": explicit("arn:aws:...")},
		def, value.Origin{}, &refused)
	if !refused.HasErrors() {
		t.Error("a purely computed attribute must still be refused, or the provider's output " +
			"becomes something a user can contradict")
	}
}

func keysOf(m map[string]value.Value) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// awsish is a provider whose schema has the two things §14.1 adds, so the WIRING can be
// tested rather than only the helpers. stubProvider in validate_test.go covers optional
// requirements; this one covers aliases and provider-chosen values.
type awsish struct{}

func (awsish) Name() string { return "aws" }
func (awsish) Definitions() []*schema.ResourceDefinition {
	return []*schema.ResourceDefinition{{
		Type: "aws.ec2.vpc",
		Attributes: map[string]schema.Attribute{
			"CidrBlock": {Kind: value.KindString, Required: true, Aliases: []string{"cidr", "cidr_block"}},
			"VpcId":     {Kind: value.KindString, Computed: true, Aliases: []string{"id"}},
			"InstanceTenancy": {
				Kind: value.KindString, Computed: true, Optional: true, Aliases: []string{"tenancy"},
			},
		},
		Capabilities: schema.Capabilities{Create: true, Read: true, Update: true, Delete: true},
	}}
}
func (awsish) ClassifyError(error) provider.Retryability { return provider.NotSafeToRetry }
func (awsish) Read(context.Context, *resource.ResourceState) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (awsish) Create(context.Context, *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (awsish) Update(context.Context, *resource.ResourceState, *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (awsish) Delete(context.Context, *resource.ResourceState) error {
	return provider.ErrNotImplemented
}
func (awsish) Discover(context.Context, provider.DiscoverRequest) ([]provider.DiscoveredResource, error) {
	return nil, provider.ErrNotImplemented
}
func (awsish) Import(context.Context, string, string) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}

func awsRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	reg := registry.New()
	if err := reg.Register("aws", awsish{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return reg
}

// TestAliasesResolveThroughTheWholeCompile.
//
// THE WIRING, not the helper. Established by sabotage: removing the canonicaliseAttributes
// and canonicaliseRefs calls from the pipeline broke NOTHING, because every other test in
// this file calls those functions directly. A helper that works and is never called is the
// same defect as a helper that does not work.
//
// One compile exercises both boundaries: `cidr:` is an aliased attribute key, and
// `${vpc.id}` is an aliased attribute inside a REFERENCE, which is resolved in stage 6
// against the definition because the evaluator downstream has no schema in reach.
func TestAliasesResolveThroughTheWholeCompile(t *testing.T) {
	files := loadFiles(t, `
project: myapp
resources:
  vpc:
    type: aws.ec2.vpc
    cidr: 10.0.0.0/16
    tenancy: default
  other:
    type: aws.ec2.vpc
    cidr_block: ${vpc.id}
`)
	cfg, ds := Compile(files, awsRegistry(t), Options{Environment: "dev"})
	if ds.HasErrors() {
		t.Fatalf("aliases must compile: %v", ds)
	}

	vpc := cfg.Resources["vpc"]
	if _, ok := vpc.Attrs["CidrBlock"]; !ok {
		t.Errorf("`cidr:` was not canonicalised; vpc holds %v", keysOf(vpc.Attrs))
	}
	// An optional+computed attribute set through an alias: both new features at once.
	if _, ok := vpc.Attrs["InstanceTenancy"]; !ok {
		t.Errorf("`tenancy:` was not canonicalised; vpc holds %v", keysOf(vpc.Attrs))
	}

	// The reference: `${vpc.id}` must have become `${vpc.VpcId}`, or the executor's
	// exact lookup finds nothing and the attribute silently never resolves.
	other := cfg.Resources["other"]
	expr := other.Attrs["CidrBlock"].Expr
	if expr == nil {
		t.Fatalf("other.CidrBlock carries no expression: %+v", other.Attrs["CidrBlock"])
	}
	refs := expr.References()
	if len(refs) != 1 {
		t.Fatalf("expected one reference, got %v", refs)
	}
	if refs[0].Attribute != "VpcId" {
		t.Errorf("the reference still reads %q; it must be canonicalised to VpcId in stage 6, "+
			"because expressions.ResourceScope resolves against a plain map with no schema",
			refs[0].Attribute)
	}
}

// TestAPurelyComputedAttributeIsStillRefusedThroughTheCompile is the control for the
// wiring: `id` aliases VpcId, which is computed and NOT optional, so setting it must still
// be refused — and the refusal must survive alias resolution rather than being skipped
// because the written name did not match.
func TestAPurelyComputedAttributeIsStillRefusedThroughTheCompile(t *testing.T) {
	files := loadFiles(t, `
project: myapp
resources:
  vpc:
    type: aws.ec2.vpc
    cidr: 10.0.0.0/16
    id: vpc-0123
`)
	_, ds := Compile(files, awsRegistry(t), Options{Environment: "dev"})
	if !ds.HasErrors() {
		t.Fatal("`id:` aliases a computed attribute, so setting it must be refused")
	}
	var out strings.Builder
	ds.Render(&out)
	if !strings.Contains(out.String(), "computed") {
		t.Errorf("the refusal does not explain that it is computed:\n%s", out.String())
	}
}
