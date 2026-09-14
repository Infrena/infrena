package planner

import (
	"strings"
	"testing"

	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/resource"
	"github.com/infrena/infrena/pkg/schema"
	"github.com/infrena/infrena/pkg/value"
)

func taskDef() *schema.ResourceDefinition {
	return &schema.ResourceDefinition{
		Type: "cloud.service",
		Attributes: map[string]schema.Attribute{
			"image":         {Kind: value.KindString, Required: true},
			"task_revision": {Kind: value.KindInt, Aliases: []string{"revision"}},
		},
		Capabilities: schema.Capabilities{Create: true, Read: true, Update: true, Delete: true},
	}
}

// TestAnIgnoredAttributeProducesNoChange.
//
// The case `ignore_changes` exists for: something outside infrena owns one attribute. A CI
// pipeline sets an ECS service's task revision on every deploy, so a plan computed from
// configuration would revert it on the next apply and undo the deployment. The user says
// "that one is not mine" and the planner stops arguing about it.
func TestAnIgnoredAttributeProducesNoChange(t *testing.T) {
	desired := map[string]value.Value{
		"image":         value.String("app:1.0", value.SourceExplicit),
		"task_revision": value.Int(7, value.SourceExplicit),
	}
	actual := map[string]value.Value{
		"image":         value.String("app:1.0", value.SourceProvider),
		"task_revision": value.Int(42, value.SourceProvider), // what CI deployed
	}

	reasons, ds := diffAttributes(address.Address{Name: "svc"}, taskDef(), desired, actual,
		[]string{"task_revision"})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %v", ds)
	}
	if len(reasons) != 0 {
		t.Errorf("the pipeline's revision must not be proposed for reversion; got %v", reasons)
	}
}

// TestIgnoringOneAttributeDoesNotIgnoreTheOthers is the control. Without it, a planner
// that ignored everything would pass the test above and quietly stop managing the
// resource — which is the failure a user would not notice until something needed changing.
func TestIgnoringOneAttributeDoesNotIgnoreTheOthers(t *testing.T) {
	desired := map[string]value.Value{
		"image":         value.String("app:2.0", value.SourceExplicit),
		"task_revision": value.Int(7, value.SourceExplicit),
	}
	actual := map[string]value.Value{
		"image":         value.String("app:1.0", value.SourceProvider),
		"task_revision": value.Int(42, value.SourceProvider),
	}

	reasons, _ := diffAttributes(address.Address{Name: "svc"}, taskDef(), desired, actual,
		[]string{"task_revision"})
	if len(reasons) != 1 || reasons[0].Attribute != "image" {
		t.Errorf("image changed and is not ignored, so it must still be proposed; got %v", reasons)
	}
}

// TestAnIgnoredAttributeKeepsWhatIsReallyThere.
//
// The half that is easy to miss and expensive to get wrong. Not diffing an attribute is
// not enough: if the operation carried CONFIGURATION's value into After, the executor
// would record it and state would claim a revision the service is not running. The plan
// would say nothing changed while quietly rewriting the record of what did.
func TestAnIgnoredAttributeKeepsWhatIsReallyThere(t *testing.T) {
	desired := map[string]value.Value{"task_revision": value.Int(7, value.SourceExplicit)}
	actual := map[string]value.Value{"task_revision": value.Int(42, value.SourceProvider)}

	after := afterAttributes(taskDef(), desired, actual, OpUpdate, []string{"task_revision"})
	got, _ := after["task_revision"].AsInt()
	if got != 42 {
		t.Errorf("After records %d, want 42 — state must keep what the pipeline deployed, "+
			"not what configuration wishes", got)
	}
}

// TestACreateUsesConfigurationEvenForAnIgnoredAttribute.
//
// There is no actual resource yet, so configuration's value is the only one there is.
// Ignoring it on a create would build the resource with the attribute missing — the
// opposite of what the user asked for, and the reason the carry-over is skipped for
// creates and replacements.
func TestACreateUsesConfigurationEvenForAnIgnoredAttribute(t *testing.T) {
	desired := map[string]value.Value{"task_revision": value.Int(7, value.SourceExplicit)}

	after := afterAttributes(taskDef(), desired, nil, OpCreate, []string{"task_revision"})
	got, ok := after["task_revision"].AsInt()
	if !ok || got != 7 {
		t.Errorf("After records %v, want configuration's 7: a create has nothing else to use", after)
	}
}

// TestThePlanSaysWhatItIgnoredAndDoesNotContradictItself.
//
// Two lines that must never appear together, which the first version of this printed:
// "[change ignored]" beside "size: 500 -> 10". Both were half true — an update ignores it,
// a REPLACEMENT rebuilds the resource from configuration and resets it — and a reader
// cannot resolve the pair. An update says it was ignored; a replacement says it is being
// reset, which is the surprise worth naming: someone who ignored a task revision can still
// lose it to a change they made somewhere else entirely.
func TestThePlanSaysWhatItIgnoredAndDoesNotContradictItself(t *testing.T) {
	opts := RenderOptions{
		Definition: func(string) (*schema.ResourceDefinition, bool) { return taskDef(), true },
	}
	lifecycle := resource.Lifecycle{IgnoreChanges: []string{"task_revision"}}

	update := Operation{
		Address: address.Address{Name: "svc"}, Type: "cloud.service", Kind: OpUpdate,
		Lifecycle: lifecycle,
		Before:    map[string]value.Value{"image": value.String("app:1.0", value.SourceProvider)},
		After:     map[string]value.Value{"image": value.String("app:2.0", value.SourceExplicit)},
	}
	out := strings.Join(renderOperationLines(update, nil, opts), "\n")
	if !strings.Contains(out, "[change ignored]") {
		t.Errorf("an update must say what it ignored, or `ignore_changes` is invisible:\n%s", out)
	}

	replace := Operation{
		Address: address.Address{Name: "svc"}, Type: "cloud.service", Kind: OpReplace,
		Lifecycle: lifecycle,
		Before:    map[string]value.Value{"task_revision": value.Int(42, value.SourceProvider)},
		After:     map[string]value.Value{"task_revision": value.Int(7, value.SourceExplicit)},
	}
	out = strings.Join(renderOperationLines(replace, nil, opts), "\n")
	if strings.Contains(out, "[change ignored]") {
		t.Errorf("a replacement does NOT ignore it — it resets it — so this claim is false:\n%s", out)
	}
	if !strings.Contains(out, "replacement resets it") {
		t.Errorf("a replacement must say the ignored attribute is being reset:\n%s", out)
	}
}
