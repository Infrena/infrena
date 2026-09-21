package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/infrena/infrena/internal/refresh"
	"github.com/infrena/infrena/internal/state"
	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/resource"
	"github.com/infrena/infrena/pkg/value"
	testprovider "github.com/infrena/infrena/providers/test"
)

func TestRefreshRemovesAResourceTheProviderReportsGone(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources: {}
`)
	ctx := context.Background()
	cloudPath := filepath.Join(dir, testprovider.DefaultCloudPath)
	prov := testprovider.New(cloudPath)
	rs, err := prov.Create(ctx, &resource.DesiredResource{
		Address: address.Address{Name: "network"},
		Type:    "fake.network",
		Attrs: map[string]value.Value{
			"cidr": value.String("10.20.0.0/16", value.SourceExplicit),
		},
	})
	if err != nil {
		t.Fatalf("seeding the fake cloud: %v", err)
	}
	st := state.New("myapp", "dev")
	st.Set(rs)
	seedState(t, dir, "dev", st)

	// Delete it from the fake cloud directly, standing in for someone deleting
	// it outside infrena entirely.
	cloud, err := testprovider.LoadCloud(cloudPath)
	if err != nil {
		t.Fatalf("loading fake cloud: %v", err)
	}
	delete(cloud.Resources, rs.ProviderID)
	if err := cloud.Save(cloudPath); err != nil {
		t.Fatalf("saving fake cloud: %v", err)
	}

	opts := &GlobalOptions{Dir: dir, Parallelism: 4}
	cmd := newRefreshCommand(opts)
	cmd.SetArgs([]string{"dev"})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() = %v, want nil", err)
	}
	if !strings.Contains(stdout.String(), "no longer exists") {
		t.Errorf("stdout does not report the removal:\n%s", stdout.String())
	}

	after, gerr := localBackend(dir).Get(ctx, "dev")
	if gerr != nil {
		t.Fatalf("reading state: %v", gerr)
	}
	if _, ok := after.Get(address.Address{Name: "network"}); ok {
		t.Error("network is still in state after refresh observed it gone")
	}
}

func TestRefreshPersistsDriftedAttributes(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources: {}
`)
	ctx := context.Background()
	cloudPath := filepath.Join(dir, testprovider.DefaultCloudPath)
	prov := testprovider.New(cloudPath)
	rs, err := prov.Create(ctx, &resource.DesiredResource{
		Address: address.Address{Name: "network"},
		Type:    "fake.network",
		Attrs: map[string]value.Value{
			"cidr": value.String("10.20.0.0/16", value.SourceExplicit),
		},
	})
	if err != nil {
		t.Fatalf("seeding the fake cloud: %v", err)
	}
	st := state.New("myapp", "dev")
	st.Set(rs)
	seedState(t, dir, "dev", st)

	// Someone resizes it by hand, outside infrena.
	cloud, err := testprovider.LoadCloud(cloudPath)
	if err != nil {
		t.Fatalf("loading fake cloud: %v", err)
	}
	cloud.Resources[rs.ProviderID].Attributes["cidr"] = "10.99.0.0/16"
	if err := cloud.Save(cloudPath); err != nil {
		t.Fatalf("saving fake cloud: %v", err)
	}

	opts := &GlobalOptions{Dir: dir, Parallelism: 4}
	cmd := newRefreshCommand(opts)
	cmd.SetArgs([]string{"dev"})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() = %v, want nil", err)
	}

	data, rerr := os.ReadFile(filepath.Join(dir, ".infrena", "state", "dev.json"))
	if rerr != nil {
		t.Fatalf("reading state file: %v", rerr)
	}
	var doc struct {
		Resources map[string]struct {
			Attributes map[string]struct {
				Raw any `json:"raw"`
			} `json:"attributes"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decoding state: %v", err)
	}
	if got := doc.Resources["network"].Attributes["cidr"].Raw; got != "10.99.0.0/16" {
		t.Errorf("state cidr = %v, want the drifted value %q — refresh must persist what it observed", got, "10.99.0.0/16")
	}
}

// TestRefreshNeverTreatsAReadErrorAsDeletion injects a read failure rather
// than an absence, and asserts the resource survives in state UNCHANGED. A
// refresh that collapsed Observation.Err != nil into the same branch as
// State == nil would delete the resource from state here — the worst thing
// refresh can get wrong.
func TestRefreshNeverTreatsAReadErrorAsDeletion(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources: {}
`)
	ctx := context.Background()
	cloudPath := filepath.Join(dir, testprovider.DefaultCloudPath)
	prov := testprovider.New(cloudPath)
	rs, err := prov.Create(ctx, &resource.DesiredResource{
		Address: address.Address{Name: "network"},
		Type:    "fake.network",
		Attrs: map[string]value.Value{
			"cidr": value.String("10.20.0.0/16", value.SourceExplicit),
		},
	})
	if err != nil {
		t.Fatalf("seeding the fake cloud: %v", err)
	}
	st := state.New("myapp", "dev")
	st.Set(rs)
	seedState(t, dir, "dev", st)

	cloud, err := testprovider.LoadCloud(cloudPath)
	if err != nil {
		t.Fatalf("loading fake cloud: %v", err)
	}
	cloud.Failures = append(cloud.Failures, testprovider.FailureRule{
		Op:           "read",
		Address:      "network",
		Nth:          1,
		Retryability: testprovider.RetryNotSafe,
		Message:      "simulated provider outage",
	})
	if err := cloud.Save(cloudPath); err != nil {
		t.Fatalf("saving fake cloud: %v", err)
	}

	opts := &GlobalOptions{Dir: dir, Parallelism: 4}
	cmd := newRefreshCommand(opts)
	cmd.SetArgs([]string{"dev"})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err = cmd.Execute()
	if err == nil {
		t.Fatal("Execute() = nil, want an error — a read failure must fail the command")
	}
	if !strings.Contains(stderr.String(), "network") {
		t.Errorf("stderr does not name the resource that failed to read: %s", stderr.String())
	}

	after, gerr := localBackend(dir).Get(ctx, "dev")
	if gerr != nil {
		t.Fatalf("reading state: %v", gerr)
	}
	got, ok := after.Get(address.Address{Name: "network"})
	if !ok {
		t.Fatal("network was removed from state after a read ERROR — a read error must never be treated as deletion")
	}
	if got.Attributes["cidr"].Raw != "10.20.0.0/16" {
		t.Errorf("network's recorded cidr changed after a failed read: got %v, want unchanged %q", got.Attributes["cidr"].Raw, "10.20.0.0/16")
	}
}

func TestRefreshLockConflictNamesTheHolder(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources: {}
`)
	backend := localBackend(dir)
	lockCtx := state.WithOperation(context.Background(), "some-other-run")
	if _, err := backend.Lock(lockCtx, "dev"); err != nil {
		t.Fatalf("pre-locking dev: %v", err)
	}
	defer backend.ForceUnlock(context.Background(), "dev")

	opts := &GlobalOptions{Dir: dir, Parallelism: 4}
	cmd := newRefreshCommand(opts)
	cmd.SetArgs([]string{"dev"})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() = nil, want an error — the environment is already locked")
	}
	if !strings.Contains(err.Error(), "some-other-run") {
		t.Errorf("error does not name the holder's operation: %v", err)
	}
}

// TestRefreshInterruptedBySignalReleasesTheLock guards the runInterruptible
// wrapper around refresh's body. Refresh takes the environment lock before
// anything else and holds it for the whole run, so without that wrapper a
// SIGINT would hit Go's default disposition and terminate the process without
// running `defer releaseLock(...)`, leaving a stale lock to clear by hand.
// Asserting only that Execute() returns an error would pass against the broken
// version too; what catches it is a SECOND, independent lock attempt after
// Execute() returns, proving the lock was actively released rather than never
// contended.
//
// The fake provider's cloud-file latency (LatencyMS) stands in for a slow
// provider Read, giving the signal a real window to land mid-refresh. The test
// waits for the lock FILE to appear rather than sleeping a fixed duration,
// which would be the kind of timing assumption that makes a test flaky under
// load.
func TestRefreshInterruptedBySignalReleasesTheLock(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources: {}
`)
	ctx := context.Background()
	cloudPath := filepath.Join(dir, testprovider.DefaultCloudPath)
	prov := testprovider.New(cloudPath)
	rs, err := prov.Create(ctx, &resource.DesiredResource{
		Address: address.Address{Name: "network"},
		Type:    "fake.network",
		Attrs: map[string]value.Value{
			"cidr": value.String("10.20.0.0/16", value.SourceExplicit),
		},
	})
	if err != nil {
		t.Fatalf("seeding the fake cloud: %v", err)
	}
	st := state.New("myapp", "dev")
	st.Set(rs)
	seedState(t, dir, "dev", st)

	cloud, err := testprovider.LoadCloud(cloudPath)
	if err != nil {
		t.Fatalf("loading fake cloud: %v", err)
	}
	cloud.LatencyMS = 2000
	if err := cloud.Save(cloudPath); err != nil {
		t.Fatalf("saving fake cloud: %v", err)
	}

	opts := &GlobalOptions{Dir: dir, Parallelism: 4}
	cmd := newRefreshCommand(opts)
	cmd.SetArgs([]string{"dev"})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	resultErr := make(chan error, 1)
	go func() { resultErr <- cmd.Execute() }()

	lockPath := filepath.Join(dir, ".infrena", "state", "dev.lock")
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, statErr := os.Stat(lockPath); statErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("refresh never took the lock (lock file never appeared)")
		}
		time.Sleep(5 * time.Millisecond)
	}

	self, ferr := os.FindProcess(os.Getpid())
	if ferr != nil {
		t.Fatalf("finding own process: %v", ferr)
	}
	if serr := self.Signal(syscall.SIGINT); serr != nil {
		t.Fatalf("signaling self: %v", serr)
	}

	select {
	case execErr := <-resultErr:
		if execErr == nil {
			t.Fatal("Execute() = nil after SIGINT, want an interruption error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("refresh did not return within 3s of receiving SIGINT")
	}

	// The assertion that matters: a fresh lock attempt must succeed,
	// proving refresh's lock was actually released rather than left
	// behind. Pre-locking as another holder (as
	// TestRefreshLockConflictNamesTheHolder does) is not needed here — the
	// lock under test IS the one refresh itself took; this call either
	// finds it already released, or fails exactly as a stale lock would.
	backend := localBackend(dir)
	lockCtx := state.WithOperation(context.Background(), "post-interrupt-check")
	if _, lerr := backend.Lock(lockCtx, "dev"); lerr != nil {
		t.Fatalf("lock was not released after SIGINT: %v", lerr)
	}
	if uerr := backend.Unlock(lockCtx, "dev"); uerr != nil {
		t.Fatalf("unlocking after check: %v", uerr)
	}
}

// TestApplyObservationsLeavesStateAloneOnAMissingObservation pins the miss
// case: `obs[addr.String()]` indexed with no ", ok" returns Observation's zero
// value, Err == nil and State == nil, the exact shape the o.State == nil
// branch reads as "the provider affirmatively reports this gone". A missing
// observation is the absence of a report, not a report, and must be handled as
// an unknown-reality read error is: state left untouched.
//
// refresh.Refresh always returns one Observation per address in st.Addresses(),
// so this cannot occur through the real command path. Hence the direct test
// against applyObservations with a hand-built, deliberately incomplete map.
func TestApplyObservationsLeavesStateAloneOnAMissingObservation(t *testing.T) {
	st := state.New("myapp", "dev")
	st.Set(&resource.ResourceState{
		Address:    address.Address{Name: "network"},
		Type:       "fake.network",
		ProviderID: "net-1",
		Attributes: map[string]value.Value{
			"cidr": value.String("10.20.0.0/16", value.SourceExplicit),
		},
	})

	// Deliberately empty: no entry at all for "network".
	obs := refresh.Observations{}

	var out bytes.Buffer
	wrote := applyObservations(st, obs, &out)

	if wrote {
		t.Error("applyObservations reported a write from a missing observation alone; want no write")
	}
	got, ok := st.Get(address.Address{Name: "network"})
	if !ok {
		t.Fatal("network was removed from state on a missing observation — a missing observation is not an affirmative report of anything")
	}
	if got.Attributes["cidr"].Raw != "10.20.0.0/16" {
		t.Errorf("network's recorded cidr changed on a missing observation: got %v, want unchanged", got.Attributes["cidr"].Raw)
	}
	if !strings.Contains(out.String(), "network") {
		t.Errorf("output does not mention the resource with the missing observation: %s", out.String())
	}
}

// TestRefreshLockNamesItselfAsTheHolder proves
// state.WithOperation(ctx, "refresh") is actually applied to the lock refresh
// itself takes, not just that *a* lock is held.
// TestRefreshLockConflictNamesTheHolder covers the reverse case, refresh
// reporting someone ELSE's held lock; this one catches refresh's own operation
// label regressing to "unknown", which a contending user would be shown instead
// of the name of what is running.
//
// It reuses the LatencyMS plus lock-file-polling technique from
// TestRefreshInterruptedBySignalReleasesTheLock to get a reliable window where
// refresh is known to be holding the lock.
func TestRefreshLockNamesItselfAsTheHolder(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources: {}
`)
	ctx := context.Background()
	cloudPath := filepath.Join(dir, testprovider.DefaultCloudPath)
	prov := testprovider.New(cloudPath)
	rs, err := prov.Create(ctx, &resource.DesiredResource{
		Address: address.Address{Name: "network"},
		Type:    "fake.network",
		Attrs: map[string]value.Value{
			"cidr": value.String("10.20.0.0/16", value.SourceExplicit),
		},
	})
	if err != nil {
		t.Fatalf("seeding the fake cloud: %v", err)
	}
	st := state.New("myapp", "dev")
	st.Set(rs)
	seedState(t, dir, "dev", st)

	cloud, err := testprovider.LoadCloud(cloudPath)
	if err != nil {
		t.Fatalf("loading fake cloud: %v", err)
	}
	cloud.LatencyMS = 2000
	if err := cloud.Save(cloudPath); err != nil {
		t.Fatalf("saving fake cloud: %v", err)
	}

	opts := &GlobalOptions{Dir: dir, Parallelism: 4}
	cmd := newRefreshCommand(opts)
	cmd.SetArgs([]string{"dev"})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))

	done := make(chan struct{})
	go func() {
		cmd.Execute()
		close(done)
	}()

	lockPath := filepath.Join(dir, ".infrena", "state", "dev.lock")
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, statErr := os.Stat(lockPath); statErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("refresh never took the lock (lock file never appeared)")
		}
		time.Sleep(5 * time.Millisecond)
	}

	backend := localBackend(dir)
	contendCtx := state.WithOperation(context.Background(), "contender")
	_, lockErr := backend.Lock(contendCtx, "dev")
	if lockErr == nil {
		backend.ForceUnlock(context.Background(), "dev")
		t.Fatal("Lock() = nil while refresh held the lock, want a conflict error")
	}
	if !strings.Contains(lockErr.Error(), `running "refresh"`) {
		t.Errorf("lock conflict error does not name refresh as the holder's operation: %v", lockErr)
	}

	<-done // let the slow refresh finish before the temp dir is cleaned up
}

// TestRefreshWithNoDriftStillWritesAndBumpsSerial pins a decision rather than
// an accident: a refresh that observes no drift at all still calls Put, and Put
// advances Serial on every write unconditionally. refresh's job is to record
// that a read happened, not only that a read changed something — see
// newRefreshCommand's doc comment.
//
// Asserting the opposite would have been just as easy to write and would have
// pinned a behaviour nobody decided was correct. This exists so a change that
// makes refresh skip the write on no drift fails here, at the decision point,
// rather than downstream as a Plan.StateSerial that does not advance.
func TestRefreshWithNoDriftStillWritesAndBumpsSerial(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources: {}
`)
	ctx := context.Background()
	cloudPath := filepath.Join(dir, testprovider.DefaultCloudPath)
	prov := testprovider.New(cloudPath)
	rs, err := prov.Create(ctx, &resource.DesiredResource{
		Address: address.Address{Name: "network"},
		Type:    "fake.network",
		Attrs: map[string]value.Value{
			"cidr": value.String("10.20.0.0/16", value.SourceExplicit),
		},
	})
	if err != nil {
		t.Fatalf("seeding the fake cloud: %v", err)
	}
	st := state.New("myapp", "dev")
	st.Set(rs)
	seedState(t, dir, "dev", st)

	before, gerr := localBackend(dir).Get(ctx, "dev")
	if gerr != nil {
		t.Fatalf("reading state before refresh: %v", gerr)
	}
	beforeSerial := before.Serial

	// The fake cloud is untouched from here: "network" still exists,
	// unchanged, exactly as prov.Create left it — this is the no-drift
	// case.

	opts := &GlobalOptions{Dir: dir, Parallelism: 4}
	cmd := newRefreshCommand(opts)
	cmd.SetArgs([]string{"dev"})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() = %v, want nil", err)
	}

	after, gerr := localBackend(dir).Get(ctx, "dev")
	if gerr != nil {
		t.Fatalf("reading state after refresh: %v", gerr)
	}
	if after.Serial != beforeSerial+1 {
		t.Errorf("Serial = %d after a no-drift refresh, want %d — refresh writes on every successful read, not only when something changed", after.Serial, beforeSerial+1)
	}
}

// TestRefreshTakesVarForAProviderInstance.
//
// `refresh --var` was once refused because refresh never calls config.Load or
// compiler.Compile, so a variable had nothing to interpolate into. It does read
// `providers:` though, and an instance's configuration interpolates variables —
// refresh has to construct that instance to call Provider.Read against the right
// account. Refusing the flag left an instance configured
// `cloud: ${var.cloud_file}` refreshable by nothing.
func TestRefreshTakesVarForAProviderInstance(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources: {}
`)
	for _, tc := range []struct {
		name string
		opts *GlobalOptions
	}{
		{"--var", &GlobalOptions{Dir: dir, Parallelism: 4, Vars: []string{"cidr=10.0.0.0/16"}}},
		// --var-file too: a guard checking len(opts.Vars) alone would survive a
		// test that asserted only the first.
		{"--var-file", &GlobalOptions{Dir: dir, Parallelism: 4, VarFiles: []string{"vars.yml"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newRefreshCommand(tc.opts)
			cmd.SetArgs([]string{"dev"})
			var stdout, stderr bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)

			err := cmd.Execute()
			if err != nil && strings.Contains(err.Error(), "does not take --var") {
				t.Errorf("%s is still refused: %v", tc.name, err)
			}
		})
	}
}
