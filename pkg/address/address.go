// Package address defines canonical resource addresses.
//
// The canonical form is the module path plus the logical name. Type is never
// part of an address: "aws.rds.database" cannot be split unambiguously because
// types themselves contain dots.
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
// The JSON tags are part of the state file's versioned on-disk contract, so that
// renaming a Go field stays an ordinary refactor rather than a silent format
// change with no version bump.
type Address struct {
	// Module is the module instantiation chain, outermost first.
	Module []string `json:"module,omitempty"` // empty at the root
	Name   string   `json:"name"`
	// Key identifies one instance of a resource declared with `for_each`, and is
	// empty for a resource declared once.
	//
	// Identity is the key, never a position, which is why this is a string
	// rather than an index. Addressing instances by ordinal means removing the
	// middle element of a list shifts every later one, so a plan proposes
	// destroying and recreating resources that did not change. A key belongs to
	// the thing it names, so removing one element affects exactly one resource.
	//
	// omitempty, so a resource declared once serialises exactly as it did before
	// this field existed and state written by an older build still decodes:
	// absent means "not an instance", which is what every resource in every
	// existing state file is.
	Key string `json:"key,omitempty"`
}

// String renders the canonical dotted form, e.g. "module.net.database", with a
// for_each instance written as `subnet["eu-west-1a"]`.
//
// The key is bracketed and quoted rather than appended with a separator, because
// a key is user data: an availability zone, a tenant name, an environment. A
// dotted `subnet.eu-west-1a` would be ambiguous with a module path, and a key
// containing a dot would be unparseable. Brackets make the boundary explicit
// whatever the key contains, and match how the reference is written in
// configuration.
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
//
// The key is carried, not dropped: a keyed address that lost its key on the way
// into a module would be a different resource in state.
func (a Address) InModule(name string) Address {
	next := make([]string, 0, len(a.Module)+1)
	next = append(next, name)
	next = append(next, a.Module...)
	return Address{Module: next, Name: a.Name, Key: a.Key}
}

// Parse reads an address back from its canonical dotted form. It is the exact
// inverse of String, Key included.
//
// It splits on dots outside brackets, which is the whole difficulty. A for_each
// key is user data and routinely contains a dot — a hostname, a bucket name, a
// version — and a plain strings.Split turns subnet["eu.west.1a"] into three
// segments and refuses it. Everything that reads an address back from text
// depends on this: `apply --plan`, `state show`, `state rm`, `depends_on` and
// import ids.
func Parse(s string) (Address, error) {
	if s == "" {
		return Address{}, fmt.Errorf("empty address")
	}
	parts, err := splitSegments(s)
	if err != nil {
		return Address{}, fmt.Errorf("address %q: %w", s, err)
	}
	var out Address
	for len(parts) > 0 {
		if parts[0] != "module" {
			break
		}
		if len(parts) < 2 || parts[1] == "" {
			return Address{}, fmt.Errorf("address %q: %q must be followed by a module name", s, "module")
		}
		// Verbatim, brackets and all. A keyed module call renders as one level
		// named `store["orders"]`, so keeping the text is what makes String and
		// Parse inverses at every level rather than only at the last one.
		out.Module = append(out.Module, parts[1])
		parts = parts[2:]
	}
	if len(parts) != 1 || parts[0] == "" {
		return Address{}, fmt.Errorf("address %q: expected a single logical name after any module path", s)
	}
	name, key, err := splitKey(parts[0])
	if err != nil {
		return Address{}, fmt.Errorf("address %q: %w", s, err)
	}
	out.Name, out.Key = name, key
	return out, nil
}

// splitSegments splits on dots that are outside brackets and outside quotes.
//
// Quotes are tracked as well as brackets because a key is rendered with
// strconv.Quote, so it may legitimately contain a `]` — `subnet["a]b"]` — and
// counting brackets alone would end the key early. Escapes are tracked because
// it may contain a quote.
func splitSegments(s string) ([]string, error) {
	var (
		out     []string
		cur     strings.Builder
		depth   int
		inQuote bool
		escaped bool
	)
	for _, r := range s {
		switch {
		case escaped:
			escaped = false
		case inQuote && r == '\\':
			escaped = true
		case r == '"':
			inQuote = !inQuote
		case inQuote:
			// Any other character inside a key is content, including a dot.
		case r == '[':
			depth++
		case r == ']':
			depth--
			if depth < 0 {
				return nil, fmt.Errorf("unbalanced `]`")
			}
		case r == '.' && depth == 0:
			out = append(out, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteRune(r)
	}
	if inQuote {
		return nil, fmt.Errorf("unterminated quote")
	}
	if depth != 0 {
		return nil, fmt.Errorf("unbalanced `[`")
	}
	return append(out, cur.String()), nil
}

// splitKey separates a resource name from a for_each instance key.
func splitKey(segment string) (name, key string, err error) {
	if !strings.HasSuffix(segment, "]") {
		return segment, "", nil
	}
	open := strings.IndexByte(segment, '[')
	if open < 0 {
		return "", "", fmt.Errorf("unbalanced `]`")
	}
	quoted := segment[open+1 : len(segment)-1]
	k, uerr := strconv.Unquote(quoted)
	if uerr != nil {
		// A numeric index is the likely mistake, and it is refused on purpose:
		// an instance's identity is its key, never its position.
		return "", "", fmt.Errorf("%s is not a quoted for_each key — an instance is named, not numbered", quoted)
	}
	return segment[:open], k, nil
}

// Sort orders addresses by their canonical string. Plan operations are sorted
// this way so that serialized plans are byte-identical across runs.
func Sort(addrs []Address) {
	sort.Slice(addrs, func(i, j int) bool {
		return addrs[i].String() < addrs[j].String()
	})
}
