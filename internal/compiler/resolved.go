// Package compiler turns typed configuration declarations into resolved
// configuration: the seam of the system, which everything above produces and
// everything below consumes (spec §5.3).
package compiler

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"

	"infra/pkg/address"
	"infra/pkg/resource"
	"infra/pkg/value"
)

// ResolvedConfig is fully-resolved desired state for one environment.
type ResolvedConfig struct {
	Project     string
	Environment string
	Resources   map[string]*resource.ResolvedResource // keyed by Address.String()
}

// Options carries what compilation needs beyond the files themselves.
type Options struct {
	Environment string
	Region      string
	Account     string
	Vars        map[string]string // from --var; the variable system proper is M4
}

// Get returns one resolved resource.
func (c ResolvedConfig) Get(addr address.Address) (*resource.ResolvedResource, bool) {
	r, ok := c.Resources[addr.String()]
	return r, ok
}

// Addresses returns every configured address, sorted.
func (c ResolvedConfig) Addresses() []address.Address {
	out := make([]address.Address, 0, len(c.Resources))
	for _, r := range c.Resources {
		out = append(out, r.Address)
	}
	address.Sort(out)
	return out
}

// Hash returns a canonical fingerprint of desired state.
//
// It covers attribute values, their provenance, and lifecycle. It excludes
// Origin: moving a resource between lines of a file is not a change in desired
// state (spec §12.1). The encoding is written by hand rather than via
// encoding/json so that what is and is not covered is explicit and cannot
// drift when a struct gains a field.
func (c ResolvedConfig) Hash() (string, error) {
	h := sha256.New()

	write := func(parts ...string) {
		for _, p := range parts {
			// Length-prefix every field so that concatenation is unambiguous:
			// without it, {"ab","c"} and {"a","bc"} would hash identically.
			fmt.Fprintf(h, "%d:%s", len(p), p)
		}
	}

	write("project", c.Project, "environment", c.Environment)

	for _, addr := range c.Addresses() {
		r := c.Resources[addr.String()]
		write("resource", addr.String(), "type", r.Type)
		write("prevent_destroy", strconv.FormatBool(r.Lifecycle.PreventDestroy))
		write("retain", strconv.FormatBool(r.Lifecycle.Retain))

		deps := make([]string, 0, len(r.DependsOn))
		for _, d := range r.DependsOn {
			deps = append(deps, d.String())
		}
		sort.Strings(deps)
		for _, d := range deps {
			write("depends_on", d)
		}

		names := make([]string, 0, len(r.Attrs))
		for name := range r.Attrs {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			write("attr", name)
			if err := hashValue(h, r.Attrs[name], write); err != nil {
				return "", err
			}
		}
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

func hashValue(h interface{ Write([]byte) (int, error) }, v value.Value, write func(...string)) error {
	write("kind", v.Kind.String(), "source", string(v.Source))
	write("known", strconv.FormatBool(v.Known), "sensitive", strconv.FormatBool(v.Sensitive))
	if !v.Known {
		return nil
	}

	switch v.Kind {
	case value.KindList:
		items, _ := v.Raw.([]value.Value)
		write("list", strconv.Itoa(len(items)))
		for _, item := range items {
			if err := hashValue(h, item, write); err != nil {
				return err
			}
		}
	case value.KindMap:
		m, _ := v.Raw.(map[string]value.Value)
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		write("map", strconv.Itoa(len(keys)))
		for _, k := range keys {
			write("key", k)
			if err := hashValue(h, m[k], write); err != nil {
				return err
			}
		}
	default:
		write("raw", fmt.Sprintf("%v", v.Raw))
	}
	return nil
}
