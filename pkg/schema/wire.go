package schema

import (
	"encoding/json"
	"fmt"

	"github.com/infrena/infrena/pkg/value"
)

// A schema's JSON form, which is what crosses the pipe to a plugin.
//
// Written by hand rather than derived from the structs, so that what is and is
// not persisted is explicit and cannot drift when a struct gains a field.
//
// The one thing that needs code is Default. It is declared `any` so a plugin
// author writes `Default: int64(10)` rather than constructing a Value, and `any`
// through encoding/json is lossy: a number decodes as float64, so
// `Default: int64(10)` on an integer attribute came back as a float and the
// compiler reported the plugin's own schema as a provider bug. Encoding it as a
// value.Value — which has an exact wire form and carries its kind — is what
// makes the round trip faithful.

type attributeWire struct {
	// Kind by name, never by number. Kind is an iota, so persisting the number
	// means a constant inserted into that list silently reinterprets every
	// schema every plugin ever sent. The names come from Kind.String() and
	// ParseKind, already a frozen contract because they are what a user writes
	// as `type:`.
	Kind      string   `json:"kind"`
	Required  bool     `json:"required,omitempty"`
	Computed  bool     `json:"computed,omitempty"`
	Sensitive bool     `json:"sensitive,omitempty"`
	ForceNew  bool     `json:"force_new,omitempty"`
	Optional  bool     `json:"optional,omitempty"`
	Aliases   []string `json:"aliases,omitempty"`
	// Reference has no custom wire form of its own — Type and Attribute are both
	// plain strings — so the struct crosses as-is. Fields recurses through
	// Attribute's own MarshalJSON and UnmarshalJSON, the same way a top-level
	// attribute does, since encoding/json calls a nested value's own methods.
	References  *Reference           `json:"references,omitempty"`
	Fields      map[string]Attribute `json:"fields,omitempty"`
	Elem        *Attribute           `json:"elem,omitempty"`
	Default     *value.Value         `json:"default,omitempty"`
	Description string               `json:"description,omitempty"`
}

// MarshalJSON writes an attribute, converting its default against its declared
// kind.
//
// A default that does not match the kind fails here, which turns a provider bug
// into a plugin that will not load rather than a resource whose default is
// quietly a float. The compiler reports the same mistake for an in-process
// provider; this is that check at the only other place a schema can arrive.
func (a Attribute) MarshalJSON() ([]byte, error) {
	kind := a.Kind.String()
	if _, ok := value.ParseKind(kind); !ok {
		// KindInvalid, which ParseKind deliberately refuses. Failing here turns
		// an unset Kind into a plugin that will not load, rather than a schema
		// that reads back as something else.
		return nil, fmt.Errorf("cannot encode an attribute of kind %s", a.Kind)
	}
	w := attributeWire{
		Kind:        kind,
		Required:    a.Required,
		Computed:    a.Computed,
		Sensitive:   a.Sensitive,
		ForceNew:    a.ForceNew,
		Optional:    a.Optional,
		Aliases:     a.Aliases,
		References:  a.References,
		Fields:      a.Fields,
		Elem:        a.Elem,
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
// indistinguishable from one declared in Go — including to DatumValue, which
// every caller of Default goes through.
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
		Optional:    w.Optional,
		Aliases:     w.Aliases,
		References:  w.References,
		Fields:      w.Fields,
		Elem:        w.Elem,
		Description: w.Description,
	}
	if w.Default != nil {
		a.Default = w.Default.Raw
	}
	return nil
}
