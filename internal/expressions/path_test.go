package expressions

import (
	"strings"
	"testing"

	"github.com/infrena/infrena/pkg/value"
)

func mapVal(m map[string]value.Value) value.Value {
	return value.Value{Kind: value.KindMap, Raw: m, Known: true, Source: value.SourceVariable}
}

func listVal(items ...value.Value) value.Value {
	return value.Value{Kind: value.KindList, Raw: items, Known: true, Source: value.SourceVariable}
}

type oneVar struct {
	name string
	val  value.Value
}

func (s oneVar) Variable(n string) (value.Value, bool) {
	if n == s.name {
		return s.val, true
	}
	return value.Value{}, false
}
func (s oneVar) Attribute(value.Reference) (value.Value, bool) { return value.Value{}, false }

func TestAPathReadsAKeyAndAnIndex(t *testing.T) {
	scope := oneVar{"cfg", mapVal(map[string]value.Value{
		"azs": listVal(value.String("us-east-1a", value.SourceVariable),
			value.String("us-east-1b", value.SourceVariable)),
	})}
	e, ds := Parse("${var.cfg.azs[1]}", value.Origin{})
	if ds.HasErrors() {
		t.Fatalf("parse: %v", ds)
	}
	got, eds := Evaluate(e, scope)
	if eds.HasErrors() {
		t.Fatalf("evaluate: %v", eds)
	}
	s, _ := got.AsString()
	if s != "us-east-1b" {
		t.Errorf("got %q, want %q", s, "us-east-1b")
	}
}

func TestAPathOutOfAContainerMarkedSensitiveStaysSensitive(t *testing.T) {
	// The whole reason §3.1 exists. CarrySensitivity sets Sensitive on the
	// CONTAINER as well as its leaves, so a sensitive map variable holds the
	// flag at the top and may hold nothing on the leaf. Returning the leaf as
	// found declassifies it, and value.Format then prints it in clear.
	inner := mapVal(map[string]value.Value{
		"password": value.String("hunter2", value.SourceVariable),
	})
	inner.Sensitive = true
	scope := oneVar{"creds", inner}

	e, _ := Parse("${var.creds.password}", value.Origin{})
	got, eds := Evaluate(e, scope)
	if eds.HasErrors() {
		t.Fatalf("evaluate: %v", eds)
	}
	if !got.Sensitive {
		t.Fatal("extracting from a sensitive container must stay sensitive — otherwise the secret prints in a plan")
	}
}

func TestAMissingKeyNamesTheKeysThatExist(t *testing.T) {
	scope := oneVar{"tags", mapVal(map[string]value.Value{
		"team":    value.String("payments", value.SourceVariable),
		"project": value.String("billing", value.SourceVariable),
	})}
	e, _ := Parse("${var.tags.tema}", value.Origin{})
	_, eds := Evaluate(e, scope)
	if !eds.HasErrors() {
		t.Fatal("a missing key must be an error, not an unknown: it can never become present")
	}
	d := eds[0]
	if !strings.Contains(d.Detail, "team") || !strings.Contains(d.Detail, "project") {
		t.Errorf("Detail = %q, want it to list the keys that exist", d.Detail)
	}
}

func TestAnOutOfRangeIndexGivesTheLength(t *testing.T) {
	scope := oneVar{"azs", listVal(
		value.String("a", value.SourceVariable),
		value.String("b", value.SourceVariable),
		value.String("c", value.SourceVariable))}
	e, _ := Parse("${var.azs[5]}", value.Origin{})
	_, eds := Evaluate(e, scope)
	if !eds.HasErrors() {
		t.Fatal("an out-of-range index must be an error")
	}
	if !strings.Contains(eds[0].Summary, "3") {
		t.Errorf("Summary = %q, want it to give the length", eds[0].Summary)
	}
}

func TestIndexingAMapAndKeyingAListEachPointAtTheOtherForm(t *testing.T) {
	m := oneVar{"tags", mapVal(map[string]value.Value{"team": value.String("p", value.SourceVariable)})}
	e, _ := Parse("${var.tags[0]}", value.Origin{})
	_, eds := Evaluate(e, m)
	if !eds.HasErrors() || !strings.Contains(eds[0].Action, "team") {
		t.Errorf("indexing a map must point at keying it, got %v", eds)
	}

	l := oneVar{"azs", listVal(value.String("a", value.SourceVariable))}
	e2, _ := Parse("${var.azs.first}", value.Origin{})
	_, eds2 := Evaluate(e2, l)
	if !eds2.HasErrors() || !strings.Contains(eds2[0].Action, "[0]") {
		t.Errorf("keying a list must point at indexing it, got %v", eds2)
	}
}

func TestAStepIntoAScalarNamesTheKind(t *testing.T) {
	scope := oneVar{"region", value.String("us-east-1", value.SourceVariable)}
	e, _ := Parse("${var.region.x}", value.Origin{})
	_, eds := Evaluate(e, scope)
	if !eds.HasErrors() || !strings.Contains(eds[0].Summary, "string") {
		t.Errorf("Summary must name the kind, got %v", eds)
	}
}
