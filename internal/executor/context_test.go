package executor

import (
	"context"
	"testing"
	"time"

	"infra/internal/diag"
	"infra/internal/planner"
	"infra/internal/registry"
	"infra/internal/state"
	"infra/pkg/address"
	"infra/pkg/resource"
	"infra/pkg/value"
	testprovider "infra/providers/test"
)

// TestApplySurvivesCancellationDuringDispatch is the proof that dispatch's
// provider calls run under operationContext, not Apply's own ctx. The fake
// provider's own delay is ctx-aware (providers/test/provider.go selects on
// ctx.Done()), so this exercises the exact failure mode operationContext
// exists to prevent: cancel while a create is genuinely in flight
// (LatencyMS holds it there for 400ms) and confirm it still finishes,
// rather than returning early with a context.Canceled-derived error. The
// cancellation fires at 100ms, well inside the 400ms window — canceling
// only after the create would already have finished could not tell "it
// survived cancellation" apart from "it was never affected".
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
	if err := reg.Register(testprovider.New(cloudPath)); err != nil {
		t.Fatalf("Register: %v", err)
	}

	p := &planner.Plan{
		Version: planner.PlanVersion,
		Operations: []planner.Operation{
			{Address: address.Address{Name: "network"}, Type: "test.network", Kind: planner.OpCreate,
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

// TestDispatchSurvivesCancellationForEveryProviderCall closes a gap the
// test above leaves open on purpose by only exercising a create:
// operationContext is wired into THREE call sites in dispatch.go (Create,
// Update, Delete), and nothing about the create-only proof establishes
// that the other two are wired too — a dispatch.go edit that wraps Create
// but leaves Update or Delete passing ctx straight through would pass
// TestApplySurvivesCancellationDuringDispatch untouched. This drives
// dispatch directly (as dispatch_test.go's other tests do) for all three
// kinds, each against a provider whose delay is genuinely in flight when
// the context is cancelled, so each failure mode has to be caught on its
// own rather than assumed from Create's.
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
				Address: node.Address, Type: "test.network",
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
				Address: address.Address{Name: "net"}, Type: "test.network",
				Attrs: map[string]value.Value{"cidr": value.String("10.0.0.0/16", value.SourceExplicit)},
			})
			if err != nil {
				t.Fatalf("seeding resource: %v", err)
			}
			setLatency(t, cloudPath, int(latency.Milliseconds()))

			node := planner.OpNode{Address: current.Address, Kind: planner.OpUpdate, Phase: planner.PhaseCreate}
			desired := &resource.DesiredResource{
				Address: current.Address, Type: "test.network",
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
				Address: address.Address{Name: "net"}, Type: "test.network",
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
