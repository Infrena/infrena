package value

import (
	"encoding/json"
	"fmt"
)

// wireValue is the on-disk shape of a Value. Kind is written explicitly so that
// integers do not come back as float64 and composites keep per-leaf provenance.
type wireValue struct {
	Kind      string          `json:"kind"`
	Known     bool            `json:"known"`
	Raw       json.RawMessage `json:"raw,omitempty"`
	Source    ValueSource     `json:"source"`
	Sensitive bool            `json:"sensitive,omitempty"`
	Origin    *Origin         `json:"origin,omitempty"`
}

// kindWireNames is the frozen on-disk spelling of every Kind.
//
// It is deliberately separate from Kind.String(), which is a diagnostic string
// and free to change. A state file is a versioned contract: renaming "integer"
// to "int" to make one error message read better must not silently invalidate
// every state file ever written, and only a table nothing else consults can
// guarantee that. Entries here change only alongside a CurrentVersion bump and
// a migration.
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
	w := wireValue{
		Kind:      kindName,
		Known:     v.Known,
		Source:    v.Source,
		Sensitive: v.Sensitive,
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

	out := Value{Kind: kind, Known: w.Known, Source: w.Source, Sensitive: w.Sensitive}
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
