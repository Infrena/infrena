package pluginhost

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/infrata/infrata/pkg/address"
	"github.com/infrata/infrata/pkg/provider"
	"github.com/infrata/infrata/pkg/resource"
	"github.com/infrata/infrata/pkg/schema"
	"github.com/infrata/infrata/pkg/value"
)

// PLAN.md §31.1: what the engine stops trusting a plugin with.
//
// Every rule here used to rest on a provider following a doc comment. A binary
// somebody else built cannot be held to a comment, so each one is enforced in the
// host adapter — and each is tested against a plugin that deliberately breaks it,
// written in Go, in this file, connected through the SAME code path a real
// subprocess uses.

// badPlugin is a plugin that misbehaves in whichever way a test asks for.
type badPlugin struct {
	// dropBookkeeping returns a state with no Lifecycle or Dependencies, as a
	// provider that forgot to carry them forward would.
	dropBookkeeping bool
	// unflaggedSecret returns the sensitive attribute without marking it.
	unflaggedSecret bool
	// claimExplicit marks a value as though the user had written it.
	claimExplicit bool
	// undeclared returns an attribute its own schema does not declare.
	undeclared bool
	// nothingOnCreate returns (nil, nil) from Create.
	nothingOnCreate bool
	// badPrefix declares a type outside its own namespace.
	badPrefix bool
	// wrongName lies about its name in the handshake.
	wrongName string
	// slowCreate blocks until its context is cancelled, then succeeds anyway.
	slowCreate bool
}

func (p *badPlugin) Name() string {
	if p.wrongName != "" {
		return p.wrongName
	}
	return "bad"
}

func (p *badPlugin) Definitions() []*schema.ResourceDefinition {
	typ := "bad.thing"
	if p.badPrefix {
		typ = "aws.instance"
	}
	return []*schema.ResourceDefinition{{
		Type: typ,
		Attributes: map[string]schema.Attribute{
			"name":     {Kind: value.KindString, Required: true},
			"password": {Kind: value.KindString, Sensitive: true},
			"id":       {Kind: value.KindString, Computed: true},
		},
		Capabilities: schema.Capabilities{Create: true, Read: true, Update: true, Delete: true},
	}}
}

func (p *badPlugin) New(string, map[string]value.Value) (provider.Provider, error) {
	return &badProvider{p: p}, nil
}

type badProvider struct{ p *badPlugin }

func (b *badProvider) Name() string { return b.p.Name() }
func (b *badProvider) Definitions() []*schema.ResourceDefinition {
	return b.p.Definitions()
}
func (b *badProvider) ClassifyError(error) provider.Retryability { return provider.SafeToRetry }

func (b *badProvider) attrs() map[string]value.Value {
	out := map[string]value.Value{
		"name": value.String("widget", value.SourceProvider),
	}
	if b.p.unflaggedSecret {
		// Sensitive in the SCHEMA, unflagged on the value.
		out["password"] = value.String("hunter2", value.SourceProvider)
	}
	if b.p.claimExplicit {
		out["name"] = value.String("widget", value.SourceExplicit).WithScope(value.ScopeCLIOverride)
	}
	if b.p.undeclared {
		out["surprise"] = value.String("!", value.SourceProvider)
	}
	return out
}

func (b *badProvider) Read(_ context.Context, current *resource.ResourceState) (*resource.ResourceState, error) {
	out := &resource.ResourceState{
		Type:       current.Type,
		ProviderID: current.ProviderID,
		Attributes: b.attrs(),
	}
	if !b.p.dropBookkeeping {
		out.Lifecycle = current.Lifecycle
		out.Dependencies = current.Dependencies
	}
	return out, nil
}

func (b *badProvider) Create(ctx context.Context, d *resource.DesiredResource) (*resource.ResourceState, error) {
	if b.p.nothingOnCreate {
		return nil, nil
	}
	if b.p.slowCreate {
		<-ctx.Done()
	}
	return &resource.ResourceState{Type: d.Type, ProviderID: "thing-1", Attributes: b.attrs()}, nil
}

func (b *badProvider) Update(_ context.Context, c *resource.ResourceState, d *resource.DesiredResource) (*resource.ResourceState, error) {
	return &resource.ResourceState{Type: d.Type, ProviderID: c.ProviderID, Attributes: b.attrs()}, nil
}
func (b *badProvider) Delete(context.Context, *resource.ResourceState) error { return nil }
func (b *badProvider) Discover(context.Context, provider.DiscoverRequest) ([]provider.DiscoveredResource, error) {
	return nil, provider.ErrNotImplemented
}
func (b *badProvider) Import(context.Context, string, string) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}

// connect wires a plugin up in process and returns one configured instance.
func connect(t *testing.T, p provider.Plugin) (*Plugin, provider.Provider) {
	t.Helper()
	host, err := InProcess(context.Background(), p)
	if err != nil {
		t.Fatalf("InProcess: %v", err)
	}
	t.Cleanup(func() { _ = host.Close() })
	prov, err := host.New("main", nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return host, prov
}

// managed is a resource the host already holds bookkeeping for.
func managed() *resource.ResourceState {
	return &resource.ResourceState{
		Type:         "bad.thing",
		ProviderID:   "thing-1",
		Provider:     "main",
		Dependencies: []address.Address{{Name: "net"}},
		Lifecycle:    resource.Lifecycle{PreventDestroy: true},
		Attributes:   map[string]value.Value{"name": value.String("widget", value.SourceProvider)},
	}
}

// TestBookkeepingSurvivesAPluginThatDropsIt is the most important rule here.
//
// `infra refresh` persists whatever Read returns. A provider that silently dropped
// Lifecycle used to make a configured prevent_destroy guard vanish with no error at
// all — the worst failure mode this product has — and the only thing preventing it
// was a paragraph in a doc comment.
//
// Now the fields are NEVER SENT, so they cannot be dropped: the host re-attaches
// them from what it already holds.
func TestBookkeepingSurvivesAPluginThatDropsIt(t *testing.T) {
	_, prov := connect(t, &badPlugin{dropBookkeeping: true})

	got, err := prov.Read(context.Background(), managed())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !got.Lifecycle.PreventDestroy {
		t.Error("prevent_destroy was lost, so removing this resource would destroy it silently")
	}
	if len(got.Dependencies) != 1 || got.Dependencies[0].Name != "net" {
		t.Errorf("Dependencies = %v, want [net] — destroy ordering comes from nowhere else", got.Dependencies)
	}
	if got.Provider != "main" {
		t.Errorf("Provider = %q, want main — state is the only thing that says which account", got.Provider)
	}
}

// TestASchemaSensitiveValueIsRedactedEvenUnflagged. A plugin that forgets the flag
// must not be able to put a password into a plan, a report or a generated file.
func TestASchemaSensitiveValueIsRedactedEvenUnflagged(t *testing.T) {
	_, prov := connect(t, &badPlugin{unflaggedSecret: true})

	got, err := prov.Read(context.Background(), managed())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !got.Attributes["password"].Sensitive {
		t.Error("the value is not marked sensitive, so it would print in clear")
	}
	// Through the one redaction path, which is what a user actually sees.
	if out := value.Format(got.Attributes["password"], value.FormatOptions{}); strings.Contains(out, "hunter2") {
		t.Errorf("the secret survives formatting: %s", out)
	}
}

// TestAPluginCannotClaimAValueWasWrittenByTheUser. Provenance is the host's: a
// plugin reports a fact about the remote system, so the value came from the
// provider. A plugin claiming SourceExplicit would make a plan credit the wrong
// file, and `[--var]` beside a value nobody passed is worse than no provenance.
func TestAPluginCannotClaimAValueWasWrittenByTheUser(t *testing.T) {
	_, prov := connect(t, &badPlugin{claimExplicit: true})

	got, err := prov.Read(context.Background(), managed())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	v := got.Attributes["name"]
	if v.Source != value.SourceProvider {
		t.Errorf("Source = %s, want provider", v.Source)
	}
	if v.Scope != value.ScopeUnset {
		t.Errorf("Scope = %s, want unset — a plugin has no place on the precedence chain", v.Scope)
	}
}

// TestAnUndeclaredAttributeIsRefused, rather than persisted into state where it
// would show up in a plan as a change nothing can explain.
func TestAnUndeclaredAttributeIsRefused(t *testing.T) {
	_, prov := connect(t, &badPlugin{undeclared: true})

	_, err := prov.Read(context.Background(), managed())
	if err == nil {
		t.Fatal("an attribute the plugin's own schema does not declare must be refused")
	}
	for _, want := range []string{"surprise", "does not declare", "name"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}

// TestNilFromCreateBecomesAnError, saying the resource may exist untracked.
//
// A nil result with no error is indistinguishable from "nothing happened" to
// everything above the provider interface. If the call did take effect, that
// resource is real and tracked nowhere.
func TestNilFromCreateBecomesAnError(t *testing.T) {
	_, prov := connect(t, &badPlugin{nothingOnCreate: true})

	_, err := prov.Create(context.Background(), &resource.DesiredResource{
		Type:  "bad.thing",
		Attrs: map[string]value.Value{"name": value.String("widget", value.SourceExplicit)},
	})
	if err == nil {
		t.Fatal("(nil, nil) from Create must be an error")
	}
	// The message has to tell the user what to go and check, because infrata
	// cannot find out for them.
	for _, want := range []string{"exists", "not in state"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not warn that the resource may exist untracked: %v", err)
		}
	}
}

// TestAPluginServingAnotherPluginsNamespaceIsRefused. Without the prefix rule a
// plugin could serve `aws.instance` and quietly take over another plugin's
// resources; the registry's duplicate-type check would then refuse whichever
// loaded second, making the outcome depend on ordering.
func TestAPluginServingAnotherPluginsNamespaceIsRefused(t *testing.T) {
	_, err := InProcess(context.Background(), &badPlugin{badPrefix: true})
	if err == nil {
		t.Fatal("a plugin declaring a type outside its own prefix must be refused")
	}
	for _, want := range []string{"aws.instance", "bad"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}

// TestAPluginThatIsNotWhatItSaysIsRefused — a renamed or mis-copied binary.
func TestAPluginThatIsNotWhatItSaysIsRefused(t *testing.T) {
	// The handshake says "imposter"; the host asked for "bad".
	p := &badPlugin{wrongName: "imposter"}
	hostReader, pluginWriter := ioPipe()
	pluginReader, hostWriter := ioPipe()
	go func() {
		_ = serveFor(p, pluginReader, pluginWriter)
		_ = pluginWriter.Close()
	}()
	c := newTestClient(hostWriter)
	err := c.start(hostReader, "bad")
	_ = pluginReader.Close()
	if err == nil {
		t.Fatal("a binary whose handshake name differs from the requested plugin must be refused")
	}
	for _, want := range []string{"imposter", "bad"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}

// TestACancelledCallStillRecordsWhatHappened.
//
// Cancellation is a MESSAGE, not a kill: the host sends `cancel` and keeps waiting,
// so it always learns what the plugin actually did. If the plugin created the
// resource anyway, the call SUCCEEDED — that resource exists, and reporting a
// failure would tell the executor to record nothing, which is how a created
// resource ends up tracked nowhere.
//
// The first version of this test expected the context error alongside the result,
// and the adapter discarded the result to return it. internal/executor had already
// settled the question from the other side: it dispatches provider calls on a
// context.WithoutCancel precisely so "an in-flight create must not be told to abort
// mid-call".
func TestACancelledCallStillRecordsWhatHappened(t *testing.T) {
	_, prov := connect(t, &badPlugin{slowCreate: true})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	got, err := prov.Create(ctx, &resource.DesiredResource{
		Type:  "bad.thing",
		Attrs: map[string]value.Value{"name": value.String("widget", value.SourceExplicit)},
	})
	if err != nil {
		t.Fatalf("the plugin completed the create, so the call succeeded: %v", err)
	}
	if got == nil {
		t.Fatal("the create completed, so its result must reach the caller; otherwise the " +
			"resource it created is orphaned")
	}
	if got.ProviderID != "thing-1" {
		t.Errorf("ProviderID = %q, want the created resource's", got.ProviderID)
	}
}

// TestRetryabilityTravelsWithTheError. ClassifyError takes an `error`, which does
// not cross a pipe, so §35's retry logic reads what the plugin sent instead.
func TestRetryabilityTravelsWithTheError(t *testing.T) {
	_, prov := connect(t, &badPlugin{})

	_, err := prov.Import(context.Background(), "bad.thing", "x")
	if err == nil {
		t.Fatal("the stub returns ErrNotImplemented")
	}
	// badProvider classifies everything SafeToRetry, so this proves the answer came
	// from the PLUGIN rather than from a host-side default.
	if got := prov.ClassifyError(err); got != provider.SafeToRetry {
		t.Errorf("ClassifyError = %v, want the plugin's own answer (SafeToRetry)", got)
	}
}

// TestAHostSideFailureIsNeverSafeToRetry is the boundary of the rule above. A
// broken pipe or a dead plugin is exactly the case where retrying a Create could
// create a second resource.
func TestAHostSideFailureIsNeverSafeToRetry(t *testing.T) {
	_, prov := connect(t, &badPlugin{})
	if got := prov.ClassifyError(errorsNew("the plugin stopped responding")); got != provider.NotSafeToRetry {
		t.Errorf("ClassifyError = %v, want NotSafeToRetry for an error the plugin did not send", got)
	}
}
