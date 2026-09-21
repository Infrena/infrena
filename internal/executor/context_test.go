package executor

import (
	"context"
	"testing"
	"time"

	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/internal/planner"
	"github.com/infrena/infrena/internal/registry"
	"github.com/infrena/infrena/internal/state"
	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/resource"
	"github.com/infrena/infrena/pkg/value"
	testprovider "github.com/infrena/infrena/providers/test"
)

// Dispatch's provider calls run under operationContext rather than Apply's
// own ctx, so a create that is genuinely in flight finishes instead of
// failing with a context.Canceled-derived error. The cancellation fires well
// inside the provider's latency window: cancelling after the create would
// already have finished could not tell "survived cancellation" apart from
// "never affected".
func TestApplySurvivesCancellationDuringDispatch(t *testing.T) {
	cloudPath := t.TempDir() + "/fake-cloud.json"
	cloud := &testprovider.Cloud{
		Resources: map[string]*testprovider.CloudResource{},
		LatencyMS: 400,
	}
	if err := cloud.Save(cloudPath); err != nil {
		t.Fatalf("seeding fake cloud: %v", err)
	}

	reg := registry.New()
	if err := reg.Register("test", testprovider.New(cloudPath)); err != nil {
		t.Fatalf("Register: %v", err)
	}

	p := &planner.Plan{
		Version: planner.PlanVersion,
		Operations: []planner.Operation{
			{Provider: "test", Address: address.Address{Name: "network"}, Type: "fake.network", Kind: planner.OpCreate,
				After: map[string]value.Value{"cidr": value.String("10.0.0.0/24", value.SourceExplicit)}},
		},
	}
	g, err := planner.BuildExecution(p, func(address.Address) []address.Address { return nil })
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}

	backend := state.NewLocal(t.TempDir())
	st, err := backend.Get(context.Background(), "dev")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	opts := Options{
		Parallelism: 1, PerProvider: 1, Registry: reg, Backend: backend,
		Environment: "dev", Retry: RetryPolicy{MaxAttempts: 1}, Now: time.Now,
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond) // well inside the 400ms delay
		cancel()
	}()

	type outcome struct {
		res Result
		ds  diag.Diagnostics
	}
	done := make(chan outcome, 1)
	go func() {
		res, ds := Apply(ctx, p, g, st, opts)
		done <- outcome{res, ds}
	}()

	select {
	case got := <-done:
		if len(got.res.Applied) != 1 || got.res.Applied[0].String() != "network" {
			t.Fatalf("Applied = %v, Failed = %v — the create did not survive cancellation; "+
				"dispatch is passing ctx straight to the provider instead of operationContext(ctx)",
				got.res.Applied, got.res.Failed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Apply did not return within 3s of a single operation with 400ms latency")
	}
}

// operationContext is wired into three call sites in dispatch (Create, Update
// and Delete). The create-only test above would still pass if an edit left
// Update or Delete passing ctx straight through, so each kind is cancelled
// mid-call here on its own.
func TestDispatchSurvivesCancellationForEveryProviderCall(t *testing.T) {
	const latency = 300 * time.Millisecond

	setLatency := func(t *testing.T, cloudPath string, ms int) {
		t.Helper()
		c, err := testprovider.LoadCloud(cloudPath)
		if err != nil {
			t.Fatalf("LoadCloud: %v", err)
		}
		c.LatencyMS = ms
		if err := c.Save(cloudPath); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}

	cancelPartway := func() (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(latency / 3) // well inside the delay window
			cancel()
		}()
		return ctx, cancel
	}

	run := func(t *testing.T, name string, fn func(t *testing.T)) {
		t.Helper()
		done := make(chan struct{})
		go func() {
			defer close(done)
			fn(t)
		}()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatalf("%s did not return within 3s of a cancellation during a %s-latency call", name, latency)
		}
	}

	t.Run("create", func(t *testing.T) {
		run(t, "create", func(t *testing.T) {
			cloudPath := t.TempDir() + "/fake-cloud.json"
			if err := (&testprovider.Cloud{Resources: map[string]*testprovider.CloudResource{}}).Save(cloudPath); err != nil {
				t.Fatalf("seeding fake cloud: %v", err)
			}
			prov := testprovider.New(cloudPath)
			setLatency(t, cloudPath, int(latency.Milliseconds()))

			node := planner.OpNode{Address: address.Address{Name: "net"}, Kind: planner.OpCreate, Phase: planner.PhaseCreate}
			desired := &resource.DesiredResource{
				Address: node.Address, Type: "fake.network",
				Attrs: map[string]value.Value{"cidr": value.String("10.0.0.0/16", value.SourceExplicit)},
			}
			ctx, cancel := cancelPartway()
			defer cancel()
			got, err := dispatch(ctx, prov, node, nil, desired)
			if err != nil {
				t.Fatalf("dispatch: %v (create did not survive cancellation — Create is not wrapped in operationContext)", err)
			}
			if got == nil || got.ProviderID == "" {
				t.Fatalf("got = %+v, want a resource state with a provider ID", got)
			}
		})
	})

	t.Run("update", func(t *testing.T) {
		run(t, "update", func(t *testing.T) {
			cloudPath := t.TempDir() + "/fake-cloud.json"
			if err := (&testprovider.Cloud{Resources: map[string]*testprovider.CloudResource{}}).Save(cloudPath); err != nil {
				t.Fatalf("seeding fake cloud: %v", err)
			}
			prov := testprovider.New(cloudPath)
			current, err := prov.Create(context.Background(), &resource.DesiredResource{
				Address: address.Address{Name: "net"}, Type: "fake.network",
				Attrs: map[string]value.Value{"cidr": value.String("10.0.0.0/16", value.SourceExplicit)},
			})
			if err != nil {
				t.Fatalf("seeding resource: %v", err)
			}
			setLatency(t, cloudPath, int(latency.Milliseconds()))

			node := planner.OpNode{Address: current.Address, Kind: planner.OpUpdate, Phase: planner.PhaseCreate}
			desired := &resource.DesiredResource{
				Address: current.Address, Type: "fake.network",
				Attrs: map[string]value.Value{"cidr": value.String("10.1.0.0/16", value.SourceExplicit)},
			}
			ctx, cancel := cancelPartway()
			defer cancel()
			got, err := dispatch(ctx, prov, node, current, desired)
			if err != nil {
				t.Fatalf("dispatch: %v (update did not survive cancellation — Update is not wrapped in operationContext)", err)
			}
			if got.ProviderID != current.ProviderID {
				t.Errorf("ProviderID = %q, want unchanged %q", got.ProviderID, current.ProviderID)
			}
		})
	})

	t.Run("delete", func(t *testing.T) {
		run(t, "delete", func(t *testing.T) {
			cloudPath := t.TempDir() + "/fake-cloud.json"
			if err := (&testprovider.Cloud{Resources: map[string]*testprovider.CloudResource{}}).Save(cloudPath); err != nil {
				t.Fatalf("seeding fake cloud: %v", err)
			}
			prov := testprovider.New(cloudPath)
			current, err := prov.Create(context.Background(), &resource.DesiredResource{
				Address: address.Address{Name: "net"}, Type: "fake.network",
				Attrs: map[string]value.Value{"cidr": value.String("10.0.0.0/16", value.SourceExplicit)},
			})
			if err != nil {
				t.Fatalf("seeding resource: %v", err)
			}
			setLatency(t, cloudPath, int(latency.Milliseconds()))

			node := planner.OpNode{Address: current.Address, Kind: planner.OpDestroy, Phase: planner.PhaseDestroy}
			ctx, cancel := cancelPartway()
			defer cancel()
			if _, err := dispatch(ctx, prov, node, current, nil); err != nil {
				t.Fatalf("dispatch: %v (delete did not survive cancellation — Delete is not wrapped in operationContext)", err)
			}
		})
	})
}
