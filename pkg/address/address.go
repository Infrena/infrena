// Package address defines canonical resource addresses.
//
// The canonical form is the module path plus the logical name. Type is never
// part of an address: "aws.rds.database" cannot be split unambiguously because
// types themselves contain dots. Spec §5.2.
package address

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Address is the canonical identity of a resource: its module path plus its
// logical name.
//
// The JSON tags are part of the state file's versioned on-disk contract, so
// that renaming a Go field stays an ordinary refactor rather than a silent
// format change with no version bump.
type Address struct {
	Module []string `json:"module,omitempty"` // empty at the root
	Name   string   `json:"name"`
	// Key identifies one instance of a resource declared with `for_each`
	// (PLAN.md §40), and is empty for a resource declared once.
	//
	// IDENTITY IS THE KEY, NEVER A POSITION, and that is the whole reason this
	// is a string rather than an index. Terraform's `count` addresses
	// instances by ordinal, so removing the middle element of a list shifts
	// every later one: the third instance becomes the second, and a plan
	// proposes destroying and recreating resources that did not change. A key
	// belongs to the thing it names, so removing one element affects exactly
	// one resource.
	//
	// omitempty, so a resource declared once serialises exactly as it did
	// before this field existed and state written by an older build still
	// decodes — absent means "not an instance", which is what every resource
	// in every existing state file is.
	Key string `json:"key,omitempty"`
}

// String renders the canonical dotted form, e.g. "module.net.database", with a
// for_each instance written as `subnet["eu-west-1a"]`.
//
// The key is BRACKETED AND QUOTED rather than appended with a separator,
// because a key is user data: an availability zone, a tenant name, an
// environment. A dotted `subnet.eu-west-1a` would be ambiguous with a module
// path, and a key containing a dot would be unparseable. Brackets make the
// boundary explicit whatever the key contains, and match how the reference is
// written in configuration.
func (a Address) String() string {
	name := a.Name
	if a.Key != "" {
		name += "[" + strconv.Quote(a.Key) + "]"
	}
	if len(a.Module) == 0 {
		return name
	}
	parts := make([]string, 0, len(a.Module)*2+1)
	for _, m := range a.Module {
		parts = append(parts, "module", m)
	}
	parts = append(parts, name)
	return strings.Join(parts, ".")
}

// WithKey returns the address of one for_each instance of a.
func (a Address) WithKey(key string) Address {
	a.Key = key
	return a
}

// InModule returns the address as seen from inside a parent module
// instantiation. It copies the module slice so callers cannot alias.
func (a Address) InModule(name string) Address {
	next := make([]string, 0, len(a.Module)+1)
	next = append(next, name)
	next = append(next, a.Module...)
	return Address{Module: next, Name: a.Name}
}

// Parse reads an address back from its canonical dotted form.
func Parse(s string) (Address, error) {
	if s == "" {
		return Address{}, fmt.Errorf("empty address")
	}
	parts := strings.Split(s, ".")
	var out Address
	for len(parts) > 0 {
		if parts[0] != "module" {
			break
		}
		if len(parts) < 2 || parts[1] == "" {
			return Address{}, fmt.Errorf("address %q: %q must be followed by a module name", s, "module")
		}
		out.Module = append(out.Module, parts[1])
		parts = parts[2:]
	}
	if len(parts) != 1 || parts[0] == "" {
		return Address{}, fmt.Errorf("address %q: expected a single logical name after any module path", s)
	}
	out.Name = parts[0]
	return out, nil
}

// Sort orders addresses by their canonical string. Plan operations are sorted
// this way so serialized plans are byte-identical across runs. Spec §12.1.
func Sort(addrs []Address) {
	sort.Slice(addrs, func(i, j int) bool {
		return addrs[i].String() < addrs[j].String()
	})
}
