package compiler

import (
	"strings"
	"testing"
)

// A reference to a SKIPPED resource says so. "No such resource" would send a
// reader hunting for a typo in a name that is right there in the file, three
// lines above the reference.

const skipEnvs = `
project: p
environments:
  dev: {}
  production: {}
`

// TestAReferenceToAGenuinelyMissingResourceStillSaysNoSuchResource is written
// FIRST, and it is the one that matters most.
//
// The cheapest implementation of the other three tests reports EVERY unresolved
// name as "skipped in this environment", which would make every typo in the
// language misleading. This is that implementation's failure mode.
func TestAReferenceToAGenuinelyMissingResourceStillSaysNoSuchResource(t *testing.T) {
	files := loadFiles(t, skipEnvs+`
resources:
  a:
    type: fake.network
    cidr: ${nosuch.id}
`)
	_, ds := Compile(files, testRegistry(t), Options{Environment: "dev"})
	if !ds.HasErrors() {
		t.Fatal("a reference to a name nothing declares must be an error")
	}
	out := rendered(ds)
	if strings.Contains(strings.ToLower(out), "skip") {
		t.Errorf("a name that was never declared is reported as skipped, which makes every typo "+
			"in the language misleading:\n%s", out)
	}
	if !strings.Contains(out, "undeclared") {
		t.Errorf("want the undeclared-resource diagnostic:\n%s", out)
	}
}

// TestAReferenceToASkippedResourceSaysSo.
func TestAReferenceToASkippedResourceSaysSo(t *testing.T) {
	files := loadFiles(t, skipEnvs+`
resources:
  debug_box:
    type: fake.network
    cidr: 10.9.0.0/16
    only: production
  web:
    type: fake.network
    cidr: ${debug_box.id}
`)
	_, ds := Compile(files, testRegistry(t), Options{Environment: "dev"})
	if !ds.HasErrors() {
		t.Fatal("a live resource referring to a skipped one must be an error: the value it needs " +
			"will never exist in this environment")
	}
	out := rendered(ds)
	for _, want := range []string{"debug_box", "skipped", "dev"} {
		if !strings.Contains(out, want) {
			t.Errorf("the diagnostic does not mention %q — a reader needs the name, the fact that "+
				"it was skipped, and which environment:\n%s", want, out)
		}
	}
	// And it must point at the key that excluded it, not only at the reference.
	// Finding the reference is easy; finding the `only:` three resources away is
	// what the reader actually needs.
	if !strings.Contains(out, "only") {
		t.Errorf("the diagnostic does not name the key that excluded it:\n%s", out)
	}
}

// TestDependsOnASkippedResourceSaysSo — the same rule through the other door.
// depends_on is resolved separately from attribute references, so this is a
// second code path rather than a second spelling of one test.
func TestDependsOnASkippedResourceSaysSo(t *testing.T) {
	files := loadFiles(t, skipEnvs+`
resources:
  debug_box:
    type: fake.network
    cidr: 10.9.0.0/16
    only: production
  web:
    type: fake.network
    cidr: 10.0.0.0/16
    depends_on: [debug_box]
`)
	_, ds := Compile(files, testRegistry(t), Options{Environment: "dev"})
	if !ds.HasErrors() {
		t.Fatal("depends_on naming a skipped resource must be an error")
	}
	out := rendered(ds)
	for _, want := range []string{"debug_box", "skipped"} {
		if !strings.Contains(out, want) {
			t.Errorf("the diagnostic does not mention %q:\n%s", want, out)
		}
	}
}

// TestASkippedResourceMayReferToALiveOne is the boundary, and the half that
// keeps the rule from being "any mention of a skipped resource is an error".
//
// The edge points the harmless way: the skipped resource is the one leaving, so
// nothing that survives depends on anything missing.
func TestASkippedResourceMayReferToALiveOne(t *testing.T) {
	files := loadFiles(t, skipEnvs+`
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  debug_box:
    type: fake.database
    engine: postgres
    network: ${net.id}
    only: production
`)
	cfg, ds := Compile(files, testRegistry(t), Options{Environment: "dev"})
	if ds.HasErrors() {
		t.Fatalf("a skipped resource referring to a live one is fine — it is the one leaving:\n%s",
			rendered(ds))
	}
	// And it really is gone from the result.
	if _, present := cfg.Resources["debug_box"]; present {
		t.Error("the skipped resource reached ResolvedConfig")
	}
	if _, present := cfg.Resources["net"]; !present {
		t.Error("the live resource it referred to was dropped too")
	}
}

// TestReferringToASkippedResourceIsFineInTheEnvironmentItExistsIn. The filter is
// per environment, so the same configuration must compile where the resource is
// kept — otherwise the rule above would make `only:` unusable.
func TestReferringToASkippedResourceIsFineInTheEnvironmentItExistsIn(t *testing.T) {
	body := skipEnvs + `
resources:
  debug_box:
    type: fake.network
    cidr: 10.9.0.0/16
    only: production
  web:
    type: fake.network
    cidr: ${debug_box.id}
`
	if _, ds := Compile(loadFiles(t, body), testRegistry(t), Options{Environment: "production"}); ds.HasErrors() {
		t.Fatalf("production keeps debug_box, so the reference must resolve there:\n%s", rendered(ds))
	}
	// The dev half is TestAReferenceToASkippedResourceSaysSo above; asserting
	// only this one would pass against a build that ignored `only` entirely.
}

// TestTheSameConfigurationPlansDifferentlyPerEnvironment — the milestone in one
// assertion. One file, two environments, no variables involved beyond the
// filters themselves.
func TestTheSameConfigurationPlansDifferentlyPerEnvironment(t *testing.T) {
	body := skipEnvs + `
resources:
  shared:
    type: fake.network
    cidr: 10.0.0.0/16
  prod_only:
    type: fake.network
    cidr: 10.1.0.0/16
    only: production
  dev_only:
    type: fake.network
    cidr: 10.2.0.0/16
    skip: [production]
`
	for _, tc := range []struct {
		env     string
		present []string
		absent  []string
	}{
		{"dev", []string{"shared", "dev_only"}, []string{"prod_only"}},
		{"production", []string{"shared", "prod_only"}, []string{"dev_only"}},
	} {
		cfg, ds := Compile(loadFiles(t, body), testRegistry(t), Options{Environment: tc.env})
		if ds.HasErrors() {
			t.Fatalf("%s: %s", tc.env, rendered(ds))
		}
		for _, name := range tc.present {
			if _, ok := cfg.Resources[name]; !ok {
				t.Errorf("%s is missing %q; got %v", tc.env, name, sortedTargetsOf(cfg))
			}
		}
		// The absence half. Asserting only presence passes against a build that
		// ignores the filters entirely.
		for _, name := range tc.absent {
			if _, ok := cfg.Resources[name]; ok {
				t.Errorf("%s still has %q, which is filtered out of it", tc.env, name)
			}
		}
	}
}

// TestPreventDestroyStillRefusesASkippedResource. The two features meet here and
// nothing special happens, which is the point: a skipped resource is an ordinary
// removal, so the ordinary guard applies. Asserting it keeps someone from
// "fixing" the interaction later.
func TestPreventDestroyStillRefusesASkippedResource(t *testing.T) {
	cfg, ds := Compile(loadFiles(t, skipEnvs+`
resources:
  guarded:
    type: fake.network
    cidr: 10.0.0.0/16
    only: production
    lifecycle:
      prevent_destroy: true
`), testRegistry(t), Options{Environment: "dev"})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics:\n%s", rendered(ds))
	}
	// It is gone from desired state, exactly like any other removal. The GUARD
	// lives in the planner, which sees "in state, absent from configuration"
	// and refuses — there is nothing for the compiler to do differently, and a
	// compiler that special-cased it would be the bug.
	if _, present := cfg.Resources["guarded"]; present {
		t.Error("a skipped resource with prevent_destroy stayed in desired state; the guard " +
			"belongs to the planner, not to the filter")
	}
}

// TestASkippedResourceIsStillChecked is what the MARKING buys, and the only
// non-circular reason for it.
//
// Dropping a skipped resource outright in stage 5 would still leave the
// reference rule working, because Scope.skipped carries the NAMES and that is
// what the rule reads. What marking buys instead is this: a skipped resource's
// own attributes are still BOUND, so a mistake inside a `production`-only
// resource is reported when you plan `dev`. Drop it in stage 5 and that mistake
// stays invisible until someone plans production, which is the run where finding
// out is most expensive.
func TestASkippedResourceIsStillChecked(t *testing.T) {
	files := loadFiles(t, skipEnvs+`
resources:
  prod_only:
    type: fake.network
    cidr: ${nosuch.id}
    only: production
`)
	_, ds := Compile(files, testRegistry(t), Options{Environment: "dev"})
	if !ds.HasErrors() {
		t.Fatal("a broken reference inside a production-only resource must be reported when " +
			"planning dev; otherwise it is invisible until the production run")
	}
	if !strings.Contains(rendered(ds), "nosuch") {
		t.Errorf("the diagnostic does not name the bad reference:\n%s", rendered(ds))
	}
}

// TestASkippedResourceMayReferToAnotherSkippedOne — the boundary of the test
// above. Two resources leaving together is not a problem to report, and a check
// that fired here would make `only:` unusable for any group of resources.
func TestASkippedResourceMayReferToAnotherSkippedOne(t *testing.T) {
	files := loadFiles(t, skipEnvs+`
resources:
  prod_net:
    type: fake.network
    cidr: 10.1.0.0/16
    only: production
  prod_db:
    type: fake.database
    engine: postgres
    network: ${prod_net.id}
    only: production
`)
	if _, ds := Compile(files, testRegistry(t), Options{Environment: "dev"}); ds.HasErrors() {
		t.Fatalf("two resources excluded from the same environment refer to each other legitimately:\n%s",
			rendered(ds))
	}
}

// Per-leaf REDACTION is deliberately not tested here: at compile time
// sensitivity comes from the provider schema (schema.go's markSensitive), and a
// reference to a sensitive attribute evaluates to a plain unknown — bind.go does
// not mention Sensitive at all. The secret only arrives at apply. The rule is
// covered where it is reachable: in internal/expressions for the walk, and in
// the integration suite end to end through a real apply.
