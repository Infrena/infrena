package schema

import (
	"encoding/json"
	"fmt"

	"github.com/infrata/infrata/pkg/value"
)

// A schema's JSON form, which is what crosses the pipe to a plugin (PLAN.md §31.1).
//
// Written by hand rather than derived from the structs, for the reason
// internal/planner's plan artifact gives: what is and is not persisted should be
// explicit, and cannot then drift when a struct gains a field.
//
// THE ONE THING THAT NEEDS CODE IS `Default`. It is declared `any` so a plugin
// author writes `Default: int64(10)` rather than constructing a Value, and `any`
// through encoding/json is exactly the lossy path internal/state was fixed for: a
// number decodes as float64, so `Default: int64(10)` on an integer attribute came
// back as a float and the compiler reported the plugin's own schema as a provider
// bug. Encoding it as a value.Value — which has an exact, tested wire form and
// carries its kind — is what makes the round trip faithful.

type attributeWire struct {
	// Kind by NAME, never by number. Kind is an iota, and persisting the number
	// means a constant inserted into that list silently reinterprets every schema
	// every plugin ever sent — the hazard pkg/value keeps its own wire-name table
	// to avoid. The names come from Kind.String()/ParseKind, which are already a
	// frozen contract because they are what a user writes as `type:`.
	Kind        string       `json:"kind"`
	Required    bool         `json:"required,omitempty"`
	Computed    bool         `json:"computed,omitempty"`
	Sensitive   bool         `json:"sensitive,omitempty"`
	ForceNew    bool         `json:"force_new,omitempty"`
	Default     *value.Value `json:"default,omitempty"`
	Description string       `json:"description,omitempty"`
}

// MarshalJSON writes an attribute, converting its default against its declared kind.
//
// A default that does not match the kind FAILS HERE, which turns a provider bug into
// a plugin that will not load rather than a resource whose default is quietly a
// float. The compiler reports the same mistake for an in-process provider; this is
// the same check at the only other place a schema can arrive.
func (a Attribute) MarshalJSON() ([]byte, error) {
	kind := a.Kind.String()
	if _, ok := value.ParseKind(kind); !ok {
		// KindInvalid, which ParseKind deliberately refuses. Failing here turns an
		// unset Kind into a plugin that will not load, rather than a schema that
		// reads back as something else.
		return nil, fmt.Errorf("cannot encode an attribute of kind %s", a.Kind)
	}
	w := attributeWire{
		Kind:        kind,
		Required:    a.Required,
		Computed:    a.Computed,
		Sensitive:   a.Sensitive,
		ForceNew:    a.ForceNew,
		Description: a.Description,
	}
	if a.Default != nil {
		v, ok := DatumValue(a.Default, a.Kind)
		if !ok {
			return nil, fmt.Errorf(
				"the default %v (%T) is not a %s, which is the kind this attribute declares",
				a.Default, a.Default, a.Kind)
		}
		w.Default = &v
	}
	return json.Marshal(w)
}

// UnmarshalJSON reads an attribute back, restoring the default as a plain datum.
//
// Raw rather than the Value itself, so an attribute that made the round trip is
// indistinguishable from one declared in Go — including to DatumValue, which every
// caller of Default goes through.
func (a *Attribute) UnmarshalJSON(b []byte) error {
	var w attributeWire
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	kind, ok := value.ParseKind(w.Kind)
	if !ok {
		return fmt.Errorf("unknown attribute kind %q", w.Kind)
	}
	*a = Attribute{
		Kind:        kind,
		Required:    w.Required,
		Computed:    w.Computed,
		Sensitive:   w.Sensitive,
		ForceNew:    w.ForceNew,
		Description: w.Description,
	}
	if w.Default != nil {
		a.Default = w.Default.Raw
	}
	return nil
}
