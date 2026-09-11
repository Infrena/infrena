package value

import (
	"encoding/json"
	"fmt"
)

// wireValue is the on-disk shape of a Value. Kind is written explicitly so that
// integers do not come back as float64 and composites keep per-leaf provenance.
type wireValue struct {
	Kind       string          `json:"kind"`
	Known      bool            `json:"known"`
	Raw        json.RawMessage `json:"raw,omitempty"`
	Source     ValueSource     `json:"source"`
	Scope      string          `json:"scope,omitempty"`
	Sensitive  bool            `json:"sensitive,omitempty"`
	Origin     *Origin         `json:"origin,omitempty"`
	SuppliedBy string          `json:"supplied_by,omitempty"`
}

// kindWireNames is the frozen on-disk spelling of every Kind.
//
// It is deliberately separate from Kind.String(). Since ParseKind exists,
// Kind.String()'s spellings are the CONFIGURATION language and change only
// with the same care; this table is the ON-DISK language. They remain
// independent contracts with different consumers and different migration
// paths. A state file is a versioned contract: renaming a spelling on either
// side must not silently invalidate every state file ever written, and only a
// table nothing else consults can guarantee that. Entries here change only
// alongside a CurrentVersion bump and a migration.
var kindWireNames = map[Kind]string{
	KindString: "string",
	KindInt:    "integer",
	KindFloat:  "float",
	KindBool:   "boolean",
	KindList:   "list",
	KindMap:    "map",
}

// kindToWireName returns the persisted name for a Kind. KindInvalid has none:
// refusing to write it turns an unrepresentable value into a failed save rather
// than a state file that cannot be read back.
func kindToWireName(k Kind) (string, error) {
	name, ok := kindWireNames[k]
	if !ok {
		return "", fmt.Errorf("cannot encode value of kind %s", k)
	}
	return name, nil
}

// kindFromWireName resolves a persisted kind name back to its Kind.
func kindFromWireName(s string) (Kind, error) {
	for k, name := range kindWireNames {
		if name == s {
			return k, nil
		}
	}
	return KindInvalid, fmt.Errorf("unknown value kind %q", s)
}

// MarshalJSON writes a Value in its on-disk form.
func (v Value) MarshalJSON() ([]byte, error) {
	kindName, err := kindToWireName(v.Kind)
	if err != nil {
		return nil, err
	}
	scopeName, err := scopeToWireName(v.Scope)
	if err != nil {
		return nil, err
	}
	w := wireValue{
		Kind:       kindName,
		Known:      v.Known,
		Source:     v.Source,
		Scope:      scopeName,
		Sensitive:  v.Sensitive,
		SuppliedBy: v.SuppliedBy,
	}
	if v.Origin.File != "" || v.Origin.Line != 0 || v.Origin.Column != 0 || v.Origin.Module != nil {
		o := v.Origin
		w.Origin = &o
	}
	if v.Known {
		raw, err := json.Marshal(v.Raw)
		if err != nil {
			return nil, err
		}
		w.Raw = raw
	}
	return json.Marshal(w)
}

// UnmarshalJSON reads a Value from its on-disk form.
func (v *Value) UnmarshalJSON(data []byte) error {
	var w wireValue
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	kind, err := kindFromWireName(w.Kind)
	if err != nil {
		return err
	}
	scope, err := scopeFromWireName(w.Scope)
	if err != nil {
		return err
	}

	out := Value{Kind: kind, Known: w.Known, Source: w.Source, Scope: scope, Sensitive: w.Sensitive, SuppliedBy: w.SuppliedBy}
	if w.Origin != nil {
		out.Origin = *w.Origin
	}

	if w.Known {
		switch kind {
		case KindString:
			var s string
			if err := json.Unmarshal(w.Raw, &s); err != nil {
				return err
			}
			out.Raw = s
		case KindInt:
			var i int64
			if err := json.Unmarshal(w.Raw, &i); err != nil {
				return err
			}
			out.Raw = i
		case KindFloat:
			var f float64
			if err := json.Unmarshal(w.Raw, &f); err != nil {
				return err
			}
			out.Raw = f
		case KindBool:
			var b bool
			if err := json.Unmarshal(w.Raw, &b); err != nil {
				return err
			}
			out.Raw = b
		case KindList:
			var items []Value
			if err := json.Unmarshal(w.Raw, &items); err != nil {
				return err
			}
			out.Raw = items
		case KindMap:
			var items map[string]Value
			if err := json.Unmarshal(w.Raw, &items); err != nil {
				return err
			}
			out.Raw = items
		default:
			return fmt.Errorf("cannot decode value of kind %s", kind)
		}
	}

	*v = out
	return nil
}
