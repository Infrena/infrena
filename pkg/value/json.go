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

func kindFromString(s string) (Kind, error) {
	for _, k := range []Kind{KindString, KindInt, KindFloat, KindBool, KindList, KindMap} {
		if k.String() == s {
			return k, nil
		}
	}
	return KindInvalid, fmt.Errorf("unknown value kind %q", s)
}

func (v Value) MarshalJSON() ([]byte, error) {
	w := wireValue{
		Kind:      v.Kind.String(),
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

func (v *Value) UnmarshalJSON(data []byte) error {
	var w wireValue
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	kind, err := kindFromString(w.Kind)
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
