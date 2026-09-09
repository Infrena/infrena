// Package address defines canonical resource addresses.
//
// The canonical form is the module path plus the logical name. Type is never
// part of an address: "aws.rds.database" cannot be split unambiguously because
// types themselves contain dots. Spec §5.2.
package address

import (
	"fmt"
	"sort"
	"strings"
)

type Address struct {
	Module []string // empty at the root
	Name   string
}

func (a Address) String() string {
	if len(a.Module) == 0 {
		return a.Name
	}
	parts := make([]string, 0, len(a.Module)*2+1)
	for _, m := range a.Module {
		parts = append(parts, "module", m)
	}
	parts = append(parts, a.Name)
	return strings.Join(parts, ".")
}

// InModule returns the address as seen from inside a parent module
// instantiation. It copies the module slice so callers cannot alias.
func (a Address) InModule(name string) Address {
	next := make([]string, 0, len(a.Module)+1)
	next = append(next, name)
	next = append(next, a.Module...)
	return Address{Module: next, Name: a.Name}
}

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
