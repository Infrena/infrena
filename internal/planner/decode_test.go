package planner

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/resource"
	"github.com/infrena/infrena/pkg/value"
)

// TestASavedPlanSurvivesTheRoundTrip.
//
// Every field the executor acts on has to come back, and the one that matters most is
// the one that was missing: DependsOn. The executor copies it onto the state it records,
// and state's Dependencies is the only surviving record of what a resource depended on
// once it leaves configuration — which is what orders a LATER destroy. An operation
// decoded without it would satisfy invariant 4 for its own apply and silently break it
// for the next plan's destroys.
func TestASavedPlanSurvivesTheRoundTrip(t *testing.T) {
	before := samplePlan(time.Now())
	before.Diagnostics = []diag.Diagnostic{{Severity: diag.SeverityWarning, Summary: "reviewed already"}}
	for i := range before.Operations {
		before.Operations[i].Provider = "main"
		before.Operations[i].Lifecycle = resource.Lifecycle{PreventDestroy: true}
		before.Operations[i].Dependents = []address.Address{{Name: "dependent"}}
		before.Operations[i].DependsOn = []address.Address{{Name: "prerequisite"}}
	}

	data, err := json.Marshal(before)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	after, err := DecodePlan(data)
	if err != nil {
		t.Fatalf("DecodePlan: %v", err)
	}

	if len(after.Operations) != len(before.Operations) {
		t.Fatalf("decoded %d operations, want %d", len(after.Operations), len(before.Operations))
	}
	// Matched by ADDRESS, not by index: encode sorts operations canonically, so the
	// decoded order is the sorted one and an index comparison would fail on a correct
	// decoder.
	originals := map[string]Operation{}
	for _, op := range before.Operations {
		originals[op.Address.String()] = op
	}
	for i, op := range after.Operations {
		want, ok := originals[op.Address.String()]
		if !ok {
			t.Fatalf("decoded an operation for %q, which was not in the plan", op.Address)
		}
		if op.Type != want.Type || op.Kind != want.Kind {
			t.Errorf("operation %q = type %q kind %v, want type %q kind %v",
				op.Address, op.Type, op.Kind, want.Type, want.Kind)
		}
		if op.Provider != "main" {
			t.Errorf("operation %d lost its instance: %q — a destroy would go to whichever account was consulted", i, op.Provider)
		}
		if len(op.DependsOn) != 1 || op.DependsOn[0].Name != "prerequisite" {
			t.Errorf("operation %d lost DependsOn: %v — state would record no dependencies and a later destroy would have no ordering edges", i, op.DependsOn)
		}
		if len(op.Dependents) != 1 || op.Dependents[0].Name != "dependent" {
			t.Errorf("operation %d lost Dependents: %v", i, op.Dependents)
		}
		if !op.Lifecycle.PreventDestroy {
			t.Errorf("operation %d lost its lifecycle, so a guard that never reaches state does nothing", i)
		}
	}
	if after.Project != before.Project || after.Environment != before.Environment {
		t.Errorf("identity lost: %q/%q", after.Project, after.Environment)
	}
	if after.StateSerial != before.StateSerial || after.ConfigHash != before.ConfigHash {
		t.Error("the fingerprints did not survive, so staleness cannot be detected at all")
	}

	// Diagnostics are deliberately NOT recovered: they were addressed to whoever
	// reviewed the plan, and reprinting them at apply time presents a decision
	// already made as one still open.
	if len(after.Diagnostics) != 0 {
		t.Errorf("diagnostics came back: %v", after.Diagnostics)
	}
}

// TestALargeIntegerSurvivesASavedPlan.
//
// The hazard internal/state hit and fixed with UseNumber: a JSON number decoded through
// `any` becomes float64, and 2^53+1 does not fit. An attribute value that changed
// between plan and apply is an attribute applied wrongly, silently.
func TestALargeIntegerSurvivesASavedPlan(t *testing.T) {
	const exact = "9007199254740993" // 2^53 + 1
	raw := `{"version":1,"project":"p","environment":"dev","config_hash":"c","state_serial":1,` +
		`"state_hash":"h","operations":[{"address":"db","type":"fake.database","kind":"create",` +
		`"after":{"size":{"kind":"integer","known":true,"raw":` + exact + `}}}]}`

	p, err := DecodePlan([]byte(raw))
	if err != nil {
		t.Fatalf("DecodePlan: %v", err)
	}
	got := p.Operations[0].After["size"]
	if out := value.Format(got, value.ProseFormatOptions); !strings.Contains(out, exact) {
		t.Errorf("size came back as %s, want %s — the value applied would not be the value planned", out, exact)
	}
}

// TestAPlanFromAnotherFormatVersionIsRefusedByVersion.
//
// Probe the version before decoding the rest, so a future plan produces a message about
// the format rather than about an unknown field.
func TestAPlanFromAnotherFormatVersionIsRefusedByVersion(t *testing.T) {
	_, err := DecodePlan([]byte(`{"version":99,"project":"p","environment":"dev","operations":[]}`))
	if err == nil {
		t.Fatal("a plan from a newer format must be refused")
	}
	if !strings.Contains(err.Error(), "version 99") {
		t.Errorf("the error does not name the version it found: %v", err)
	}
}

// TestEachStalenessCauseHasItsOwnMessage.
//
// Four checks because they are four different mistakes, and collapsing them into "this
// plan is stale" would be accurate and useless: applying dev's plan to production,
// applying after editing configuration, and applying after a colleague applied theirs
// need completely different sentences. The table asserts the sentence, not merely that
// something was refused — a single shared message would pass a test that only counted
// failures.
func TestEachStalenessCauseHasItsOwnMessage(t *testing.T) {
	base := func() *Plan {
		return &Plan{
			Version: PlanVersion, Project: "myapp", Environment: "dev",
			ConfigHash: "cfg", StateSerial: 7, StateHash: "st",
		}
	}

	for _, tc := range []struct {
		name                 string
		project, environment string
		configHash           string
		serial               uint64
		hash                 string
		wantSubstring        string
	}{
		{"wrong project", "other", "dev", "cfg", 7, "st", `project "myapp"`},
		{"wrong environment", "myapp", "production", "cfg", 7, "st", `environment "dev"`},
		{"configuration edited", "myapp", "dev", "different", 7, "st", "configuration has changed"},
		{"someone else applied", "myapp", "dev", "cfg", 9, "moved", "state has changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := base().CheckApplicable(tc.project, tc.environment, tc.configHash,
				StateFingerprint(tc.serial, tc.hash))
			if err == nil {
				t.Fatalf("%s must be refused", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantSubstring) {
				t.Errorf("the message does not say %q:\n%v", tc.wantSubstring, err)
			}
			// §44: every refusal says what to do next.
			if !strings.Contains(err.Error(), "infrena plan") {
				t.Errorf("the refusal offers no action:\n%v", err)
			}
		})
	}
}

// TestAPlanThatMatchesIsApplicable is the control. Without it, every test above passes
// against a CheckApplicable that refuses everything.
func TestAPlanThatMatchesIsApplicable(t *testing.T) {
	p := &Plan{
		Version: PlanVersion, Project: "myapp", Environment: "dev",
		ConfigHash: "cfg", StateSerial: 7, StateHash: "st",
	}
	if err := p.CheckApplicable("myapp", "dev", "cfg", StateFingerprint(7, "st")); err != nil {
		t.Errorf("a plan matching its project, environment, configuration and state must apply: %v", err)
	}
}

// TestAnUnknownOperationKindIsRefusedNotTreatedAsNoOp.
//
// Established by measurement: making UnmarshalText fall back to OpNoOp broke NOTHING in
// the suite. That fallback is the worst available outcome — a create from a newer build
// would decode as "no operation", the apply would report success, and nothing would be
// created. A plan that silently does less than it says is worse than one that refuses.
//
// The kind is a NAME on the wire precisely so a reordered constant cannot change an
// operation's meaning (see MarshalText); this is the other half of that guarantee, and
// without it the names are checked in one direction only.
func TestAnUnknownOperationKindIsRefusedNotTreatedAsNoOp(t *testing.T) {
	raw := `{"version":1,"project":"p","environment":"dev","operations":` +
		`[{"address":"db","type":"fake.database","kind":"transmute"}]}`

	p, err := DecodePlan([]byte(raw))
	if err == nil {
		t.Fatalf("an unknown kind must be refused; decoded %d operations as %v",
			len(p.Operations), p.Operations[0].Kind)
	}
	if !strings.Contains(err.Error(), "transmute") {
		t.Errorf("the error does not name the kind it could not read: %v", err)
	}
}

// TestEveryKindNameRoundTripsThroughTheWire.
//
// One assertion per kind, because UnmarshalText walks a hand-written list of the
// constants and a kind left out of that list would decode as an error rather than
// itself — which is the same silent-skip hazard as the test above, arriving through
// forgetfulness rather than through a newer build.
func TestEveryKindNameRoundTripsThroughTheWire(t *testing.T) {
	for _, kind := range []OpKind{OpNoOp, OpCreate, OpUpdate, OpReplace, OpDestroy, OpForget} {
		name, err := kind.MarshalText()
		if err != nil {
			t.Fatalf("MarshalText(%v): %v", kind, err)
		}
		var got OpKind
		if err := got.UnmarshalText(name); err != nil {
			t.Errorf("%q does not decode back, so a plan containing it cannot be applied: %v", name, err)
			continue
		}
		if got != kind {
			t.Errorf("%q decoded as %v, want %v", name, got, kind)
		}
	}
}
