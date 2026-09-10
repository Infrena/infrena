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

	"infra/internal/state"
	"infra/pkg/address"
	"infra/pkg/resource"
	"infra/pkg/value"
	testprovider "infra/providers/test"
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
		Type:    "test.network",
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

	// Delete it from the fake cloud directly, standing in for someone
	// deleting it outside infra entirely.
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

	after, gerr := backendFor(dir).Get(ctx, "dev")
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
		Type:    "test.network",
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

	// Someone resizes it by hand, outside infra.
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

	data, rerr := os.ReadFile(filepath.Join(dir, ".infra", "state", "dev.json"))
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

// TestRefreshNeverTreatsAReadErrorAsDeletion is the test the brief names as
// guarding "the worst bug available in this milestone". It injects a read
// failure rather than an absence, and asserts the resource survives in
// state UNCHANGED. A refresh implementation that collapsed
// Observation.Err != nil into the same branch as State == nil would delete
// the resource from state here and this test would catch it: the address
// would be gone from state instead of present with its original
// attributes.
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
		Type:    "test.network",
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

	after, gerr := backendFor(dir).Get(ctx, "dev")
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
	backend := backendFor(dir)
	lockCtx := state.WithOperation(context.Background(), "some-other-run")
	if _, err := backend.Lock(lockCtx, "dev"); err != nil {
		t.Fatalf("pre-locking dev: %v", err)
	}
	defer backend.ForceUnlock("dev")

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

// TestRefreshInterruptedBySignalReleasesTheLock is the test named in this
// task's brief: refresh takes the environment lock before doing anything
// else and holds it for the whole run, so if refresh's body were not
// wrapped in runInterruptible, a real SIGINT would hit Go's default,
// uncaught disposition and terminate the process without ever running the
// `defer releaseLock(...)` — leaving a stale lock the user has to clear by
// hand with `infra state unlock`. Asserting only that Execute() returns an
// error would pass even against that broken version (the process would
// simply be gone, taking the test binary down with it); the assertion that
// actually catches the defect is a SECOND, independent lock attempt made
// after Execute() returns, proving the lock was actively released rather
// than merely never having been contended.
//
// The fake provider's cloud-file latency (LatencyMS) stands in for a slow
// provider Read, giving the signal a real window to land mid-refresh
// instead of racing a read that completes instantly. The test waits for
// the lock FILE to appear on disk, rather than sleeping a fixed duration,
// as its signal that refresh has progressed far enough to be worth
// interrupting — a fixed sleep would be exactly the kind of timing
// assumption that makes a test flaky under load.
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
		Type:    "test.network",
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

	lockPath := filepath.Join(dir, ".infra", "state", "dev.lock")
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
	backend := backendFor(dir)
	lockCtx := state.WithOperation(context.Background(), "post-interrupt-check")
	if _, lerr := backend.Lock(lockCtx, "dev"); lerr != nil {
		t.Fatalf("lock was not released after SIGINT: %v", lerr)
	}
	if uerr := backend.Unlock(lockCtx, "dev"); uerr != nil {
		t.Fatalf("unlocking after check: %v", uerr)
	}
}
