package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file covers spec §15's two lifecycle guards END TO END: from the YAML a
// user writes, through apply, into state, and back out at destroy time.
//
// Every test here seeds state by RUNNING APPLY. That is the whole point. The
// guards were already covered by tests that hand-built a ResourceState with
// Lifecycle already set (internal/cli/destroy_test.go,
// internal/planner/planner_test.go), and those tests passed throughout the
// period in which both guards were completely inert in production: the
// executor recorded resource.Lifecycle{} on every create, so no configured
// lifecycle ever reached state, and destroy — which correctly reads lifecycle
// from state, the resource having left configuration by then — always saw
// false. The fixtures manufactured the one precondition production never
// established, and so proved only that enforcement works IF state says so,
// never that state ever says so.
//
// A test in this file that constructs state directly would be that same test
// again. Nothing here may call writeM2State, state.New, or write
// .infra/state/*.json; state is only ever read.

const preventDestroyProject = `
project: pd
resources:
  guarded:
    type: fake.network
    cidr: 10.0.0.0/16
    lifecycle:
      prevent_destroy: true
`

const retainProject = `
project: rt
resources:
  keeper:
    type: fake.network
    cidr: 10.1.0.0/16
    lifecycle:
      retain: true
`

const unguardedProject = `
project: pd
resources:
  guarded:
    type: fake.network
    cidr: 10.0.0.0/16
`

// stateLifecycle returns the lifecycle state records for one resource. It
// decodes the on-disk document rather than any in-process type, because the
// state file is what a later destroy actually reads.
func stateLifecycle(t *testing.T, dir, environment, name string) (preventDestroy, retain bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, ".infra", "state", environment+".json"))
	if err != nil {
		t.Fatalf("reading state: %v", err)
	}
	var doc struct {
		Resources map[string]struct {
			Lifecycle struct {
				PreventDestroy bool `json:"prevent_destroy"`
				Retain         bool `json:"retain"`
			} `json:"lifecycle"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decoding state: %v", err)
	}
	r, ok := doc.Resources[name]
	if !ok {
		t.Fatalf("resource %q not found in state:\n%s", name, data)
	}
	return r.Lifecycle.PreventDestroy, r.Lifecycle.Retain
}

// requireCloudAddresses asserts exactly which addresses still exist at the
// provider. Survival is the claim a retain test has to make: state no longer
// listing a resource proves only that state changed, and would be equally true
// of the deletion retain exists to prevent.
func requireCloudAddresses(t *testing.T, dir string, want ...string) {
	t.Helper()
	var got []string
	for _, r := range readFakeCloudResources(t, dir) {
		if addr, ok := r["address"].(string); ok {
			got = append(got, addr)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("provider holds %v, want %v", got, want)
	}
	for _, w := range want {
		found := false
		for _, g := range got {
			if g == w {
				found = true
			}
		}
		if !found {
			t.Fatalf("provider holds %v, want %v", got, want)
		}
	}
}

func requireStateResources(t *testing.T, dir, environment string, want ...string) {
	t.Helper()
	doc := readStateFile(t, dir, environment)
	resources, _ := doc["resources"].(map[string]any)
	if len(resources) != len(want) {
		t.Fatalf("state records %v, want %v", resources, want)
	}
	for _, w := range want {
		if _, ok := resources[w]; !ok {
			t.Fatalf("state records %v, want %v", resources, want)
		}
	}
}

// TestApplyRecordsPreventDestroyAndDestroyRefuses is the regression test for
// the reported defect, in its first form.
//
// Against the unfixed executor it fails at the state assertion — state records
// prevent_destroy: false after an apply of a configuration that sets it true —
// and, if that assertion is removed, again at the destroy, which succeeds and
// deletes the guarded resource at the provider.
func TestApplyRecordsPreventDestroyAndDestroyRefuses(t *testing.T) {
	dir := project(t, preventDestroyProject)
	applied(t, dir)

	preventDestroy, retain := stateLifecycle(t, dir, "dev", "guarded")
	if !preventDestroy {
		t.Fatalf("apply recorded prevent_destroy: false for a resource configured prevent_destroy: true — " +
			"the guard is in configuration but not in state, and destroy reads it from state")
	}
	if retain {
		t.Errorf("apply recorded retain: true for a resource that does not configure it")
	}

	res := runStdin(t, dir, "dev\n", "destroy", "dev")
	if res.ExitCode == 0 {
		t.Fatalf("destroy exited 0 for a prevent_destroy resource:\n%s", res.combined())
	}
	requireContains(t, res.combined(), "protected by prevent_destroy")
	requireContains(t, res.combined(), "clear prevent_destroy if you really mean to destroy it")

	requireCloudAddresses(t, dir, "guarded")
	requireStateResources(t, dir, "dev", "guarded")
}

// TestApplyRecordsRetainAndDestroyForgets is the same defect in its second
// form. §15: retain "removes it from management without deleting the external
// resource" — so the assertion that matters is the one about the provider.
func TestApplyRecordsRetainAndDestroyForgets(t *testing.T) {
	dir := project(t, retainProject)
	applied(t, dir)

	preventDestroy, retain := stateLifecycle(t, dir, "dev", "keeper")
	if !retain {
		t.Fatalf("apply recorded retain: false for a resource configured retain: true — " +
			"the guard is in configuration but not in state, and destroy reads it from state")
	}
	if preventDestroy {
		t.Errorf("apply recorded prevent_destroy: true for a resource that does not configure it")
	}

	res := runStdin(t, dir, "dev\n", "destroy", "dev")
	requireContains(t, res.Stdout, "1 to forget")
	requireContains(t, res.Stdout, "0 to destroy")
	if strings.Contains(res.Stdout, "1 to destroy") {
		t.Fatalf("destroy planned a destroy for a retained resource:\n%s", res.Stdout)
	}

	// Dropped from management...
	requireStateResources(t, dir, "dev")
	// ...but emphatically still there.
	requireCloudAddresses(t, dir, "keeper")
}

// TestAddingLifecycleToAnExistingResourceReachesState covers the other half of
// the same hole, and the one every real user hits: the resource already exists
// and the guard is added afterwards. Nothing about the resource's attributes
// changes, so before this fix the planner reported "No changes", state was
// never rewritten, and the guard the user had just written down was inert —
// the identical silent failure, one apply later.
func TestAddingLifecycleToAnExistingResourceReachesState(t *testing.T) {
	dir := project(t, unguardedProject)
	applied(t, dir)

	if preventDestroy, _ := stateLifecycle(t, dir, "dev", "guarded"); preventDestroy {
		t.Fatalf("state records prevent_destroy: true for a configuration that never set it")
	}

	writeProject(t, dir, preventDestroyProject)

	plan := run(t, dir, "plan", "dev")
	if plan.ExitCode != 2 {
		t.Fatalf("plan exit code %d, want 2 (changes) after adding prevent_destroy to configuration:\n%s",
			plan.ExitCode, plan.combined())
	}
	// The change has to be visible, not merely proposed: an update rendered
	// as a bare header would ask for approval without saying what for.
	requireContains(t, plan.Stdout, "lifecycle.prevent_destroy: false -> true")

	applied(t, dir)
	if preventDestroy, _ := stateLifecycle(t, dir, "dev", "guarded"); !preventDestroy {
		t.Fatalf("applying an added prevent_destroy did not record it in state")
	}

	// Converged: the same configuration now proposes nothing (invariant 2).
	if res := run(t, dir, "plan", "dev"); res.ExitCode != 0 {
		t.Fatalf("plan exit code %d, want 0 after applying the lifecycle change:\n%s", res.ExitCode, res.combined())
	}

	res := runStdin(t, dir, "dev\n", "destroy", "dev")
	if res.ExitCode == 0 {
		t.Fatalf("destroy exited 0 for a resource whose guard was added after creation:\n%s", res.combined())
	}
	requireCloudAddresses(t, dir, "guarded")
}

// TestAddingRetainToAnExistingResourceReachesState is the same path for the
// other flag. It is a separate test rather than a case of the one above
// because retain's proof is different in kind: prevent_destroy is proven by a
// destroy that REFUSES, retain by one that succeeds while the resource SURVIVES
// at the provider. It is also the only test that reaches the retain half of
// lifecycleReasons — a create records retain without ever diffing it, so
// deleting that half broke nothing until this existed.
func TestAddingRetainToAnExistingResourceReachesState(t *testing.T) {
	dir := project(t, `
project: rt
resources:
  keeper:
    type: fake.network
    cidr: 10.1.0.0/16
`)
	applied(t, dir)
	if _, retain := stateLifecycle(t, dir, "dev", "keeper"); retain {
		t.Fatalf("state records retain: true for a configuration that never set it")
	}

	writeProject(t, dir, retainProject)

	plan := run(t, dir, "plan", "dev")
	if plan.ExitCode != 2 {
		t.Fatalf("plan exit code %d, want 2 (changes) after adding retain to configuration:\n%s",
			plan.ExitCode, plan.combined())
	}
	requireContains(t, plan.Stdout, "lifecycle.retain: false -> true")

	applied(t, dir)
	if _, retain := stateLifecycle(t, dir, "dev", "keeper"); !retain {
		t.Fatalf("applying an added retain did not record it in state")
	}

	res := runStdin(t, dir, "dev\n", "destroy", "dev")
	requireContains(t, res.Stdout, "1 to forget")
	requireStateResources(t, dir, "dev")
	requireCloudAddresses(t, dir, "keeper")
}

// TestClearingLifecycleInConfigurationTakesEffect pins the precedence
// decision: for a resource that is in configuration, CONFIGURATION wins over
// what state records. If state won instead, prevent_destroy could be switched
// on but never off, and removalOperation's own suggested action — "clear
// prevent_destroy if you really mean to destroy it" — would be advice that
// does not work.
func TestClearingLifecycleInConfigurationTakesEffect(t *testing.T) {
	dir := project(t, preventDestroyProject)
	applied(t, dir)
	if preventDestroy, _ := stateLifecycle(t, dir, "dev", "guarded"); !preventDestroy {
		t.Fatalf("apply did not record the configured prevent_destroy")
	}

	writeProject(t, dir, unguardedProject)

	plan := run(t, dir, "plan", "dev")
	if plan.ExitCode != 2 {
		t.Fatalf("plan exit code %d, want 2 (changes) after clearing prevent_destroy:\n%s", plan.ExitCode, plan.combined())
	}
	requireContains(t, plan.Stdout, "lifecycle.prevent_destroy: true -> false")

	applied(t, dir)
	if preventDestroy, _ := stateLifecycle(t, dir, "dev", "guarded"); preventDestroy {
		t.Fatalf("state still records prevent_destroy after configuration cleared it — " +
			"the guard would be unremovable")
	}

	res := runStdin(t, dir, "dev\n", "destroy", "dev")
	requireContains(t, res.Stdout, "1 to destroy")
	requireStateResources(t, dir, "dev")
	requireCloudAddresses(t, dir)
}

// TestRefreshDoesNotEraseAGuardFromState covers the one remaining way a
// recorded guard can silently disappear. `infra refresh` persists whatever
// Provider.Read returns (internal/cli/refresh.go: st.Set on the observation,
// then Put), so a provider whose Read forgot to carry Lifecycle forward would
// not leave state merely stale — it would erase the guard, and the next
// destroy would find nothing to refuse. pkg/provider's Read contract requires
// the carry-forward for exactly this reason; nothing measured it end to end.
func TestRefreshDoesNotEraseAGuardFromState(t *testing.T) {
	dir := project(t, preventDestroyProject)
	applied(t, dir)
	if preventDestroy, _ := stateLifecycle(t, dir, "dev", "guarded"); !preventDestroy {
		t.Fatalf("apply did not record the configured prevent_destroy")
	}

	if res := run(t, dir, "refresh", "dev"); res.ExitCode != 0 {
		t.Fatalf("refresh exit code %d, want 0:\n%s", res.ExitCode, res.combined())
	}

	if preventDestroy, _ := stateLifecycle(t, dir, "dev", "guarded"); !preventDestroy {
		t.Fatalf("refresh erased prevent_destroy from state")
	}
	res := runStdin(t, dir, "dev\n", "destroy", "dev")
	if res.ExitCode == 0 {
		t.Fatalf("destroy exited 0 after a refresh:\n%s", res.combined())
	}
	requireCloudAddresses(t, dir, "guarded")
}

// TestPlanForConfigurationWithoutLifecycleCarriesNoLifecycle guards invariant
// 6 against the fix itself: a configuration that sets no lifecycle must plan
// exactly as it did before Operation gained the field, so the plan artifact
// must not grow a lifecycle key for it. This is what makes `omitzero` on
// operationWire.Lifecycle load-bearing rather than cosmetic.
func TestPlanForConfigurationWithoutLifecycleCarriesNoLifecycle(t *testing.T) {
	dir := project(t, unguardedProject)
	out := filepath.Join(dir, "plan.json")
	if res := run(t, dir, "plan", "dev", "--output", out); res.ExitCode != 2 {
		t.Fatalf("plan exit code %d, want 2:\n%s", res.ExitCode, res.combined())
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading plan: %v", err)
	}
	if strings.Contains(string(data), "lifecycle") {
		t.Fatalf("a plan for a configuration with no lifecycle mentions lifecycle:\n%s", data)
	}

	// And the field is genuinely persisted when it is set, so the check above
	// is measuring omission rather than a field that never encodes at all.
	guarded := project(t, preventDestroyProject)
	outGuarded := filepath.Join(guarded, "plan.json")
	if res := run(t, guarded, "plan", "dev", "--output", outGuarded); res.ExitCode != 2 {
		t.Fatalf("plan exit code %d, want 2:\n%s", res.ExitCode, res.combined())
	}
	guardedData, err := os.ReadFile(outGuarded)
	if err != nil {
		t.Fatalf("reading plan: %v", err)
	}
	var doc struct {
		Operations []struct {
			Address   string `json:"address"`
			Lifecycle struct {
				PreventDestroy bool `json:"prevent_destroy"`
			} `json:"lifecycle"`
		} `json:"operations"`
	}
	if err := json.Unmarshal(guardedData, &doc); err != nil {
		t.Fatalf("decoding plan: %v", err)
	}
	if len(doc.Operations) != 1 || !doc.Operations[0].Lifecycle.PreventDestroy {
		t.Fatalf("a plan for a prevent_destroy resource does not carry it:\n%s", guardedData)
	}
}
