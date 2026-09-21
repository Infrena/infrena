package generator

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/infrena/infrena/internal/registry"
	"github.com/infrena/infrena/pkg/schema"
	"github.com/infrena/infrena/pkg/value"
)

// refIndex maps a resource type, then one of its attribute values, to the
// configuration name of the resource holding that value.
//
// The inner key pairs the canonical attribute name with the literal, rather
// than being the literal alone. A type's two attributes can easily hold the same
// string — an id repeated in an arn, a name repeated in a path — and an index
// keyed on the value alone would answer a question about VpcId with a resource
// that merely happened to carry that text somewhere else.
type refIndex map[string]map[string]string

// refKey is the inner key: the attribute a References declaration names, and
// the literal it holds.
func refKey(attribute, literal string) string { return attribute + "\x00" + literal }

// buildRefIndex maps each discovered resource's referenceable attribute values
// to the configuration name that will hold them.
//
// The plugin decides and this file only reads: schema.Attribute.References says
// that, say, aws.subnet's VpcId holds an aws.vpc's VpcId, and the engine never
// infers that a property ending in Id points anywhere. An attribute with no
// declaration keeps its literal, so partial plugin coverage costs nothing.
//
// Only the resources passed in are indexed, which is what keeps a generated
// ${vpc-app1} from being a compile error in a file the user never wrote: a
// reference is emitted only where its target is in the same generated set.
//
// Every known string attribute is indexed, including computed ones, which is the
// ordinary case: an id is exactly the attribute a plugin points a References at,
// and exactly the one generation omits from the target's own file. What the
// target's file says and what the target can be found by are different
// questions.
func buildRefIndex(resources []Resource, reg *registry.Registry) refIndex {
	ix := refIndex{}
	for _, r := range resources {
		if r.Name == "" {
			continue
		}
		def, known := reg.Definition(r.Type)
		for name, v := range r.Attributes {
			literal, ok := stringLiteral(v)
			if !ok {
				continue
			}
			canonical := name
			if known {
				// The plugin's own spelling, because a References
				// declaration names the canonical attribute and never an
				// alias. A provider that reported an alias would otherwise
				// index under a key no lookup asks for.
				if c, matched := def.Canonical(name); matched {
					canonical = c
				}
			}
			byValue := ix[r.Type]
			if byValue == nil {
				byValue = map[string]string{}
				ix[r.Type] = byValue
			}
			key := refKey(canonical, literal)
			if _, taken := byValue[key]; taken {
				// Two discovered resources of one type holding one id is a
				// provider reporting something impossible. Neither answer is
				// right, so the reference is left unresolved and the literal
				// survives, which is the same outcome as a target that was
				// never discovered.
				byValue[key] = ""
				continue
			}
			byValue[key] = r.Name
		}
	}
	return ix
}

// Edges are the dependency edges the generated configuration implies, keyed by
// the generating resource's configuration name and holding the names its emitted
// `${...}` references point at, sorted and deduplicated.
//
// A reference is a dependency edge, so `import --generate` has to record the same
// edges into state or the first plan after an import proposes
// `~ depends_on: [] -> [network-vpc-0a1b]`.
//
// They are collected as the files are rendered rather than recomputed by the
// importer, which is the whole point of the type: whether a reference is emitted
// depends on every omission rule in renderResource — computed, sensitive, equal
// to a default, equal to an instance default, hidden leaf — so a second pass
// would answer a different question than the file on disk.
type Edges map[string][]string

// record notes that name references target, ignoring a repeat.
func (e Edges) record(name string, targets []string) {
	if len(targets) == 0 {
		return
	}
	seen := make(map[string]bool, len(e[name])+len(targets))
	for _, t := range e[name] {
		seen[t] = true
	}
	for _, t := range targets {
		if t == "" || t == name || seen[t] {
			// Self is dropped rather than recorded: the compiler refuses a
			// resource that depends on itself, so recording one would put
			// state permanently at odds with configuration that cannot
			// exist.
			continue
		}
		seen[t] = true
		e[name] = append(e[name], t)
	}
}

// sorted puts every edge list in one order, because the planner compares
// dependencies positionally against the compiler's own sorted list, and an
// import whose edges happened to be recorded in another order would plan a
// change that is not one.
func (e Edges) sorted() Edges {
	for name := range e {
		sort.Strings(e[name])
	}
	return e
}

// resolve reports the configuration name a declared reference points at, where
// that target is in the generated set.
func (ix refIndex) resolve(ref *schema.Reference, v value.Value) (name string, ok bool) {
	if ref == nil {
		return "", false
	}
	literal, isString := stringLiteral(v)
	if !isString {
		return "", false
	}
	name, ok = ix[ref.Type][refKey(ref.Attribute, literal)]
	if name == "" {
		return "", false
	}
	return name, ok
}

// stringLiteral is the text a reference is matched on: a known, non-sensitive
// string.
//
// Sensitive is refused here rather than at the call site so that no path
// through this file can put a secret into an index a comment is later built
// from.
func stringLiteral(v value.Value) (string, bool) {
	if !v.Known || v.Sensitive {
		return "", false
	}
	return v.AsString()
}

// project renders an attribute that declares a reference, replacing each literal
// whose target is in the generated set with ${name}.
//
// It returns the node to emit, the names it actually referenced, a note for
// whatever did not resolve, and whether the value was a shape a reference
// applies to at all. A false ok means the caller emits the value the ordinary
// way: a reference declared on something that is not a string or a list of them
// is a plugin saying something this cannot act on, and dropping the value over
// it would lose it.
//
// The referenced names are returned rather than worked out again later, because
// every `${name}` below is a dependency edge the importer has to record into
// state, and reporting them from the one place that decides them is what keeps
// the two answers from drifting.
//
// The unresolved case keeps the literal and says so: `${vpc-app1}` naming a
// resource no generated file declares is a compile error in a file the user
// never wrote, which is worse than the id they would have had anyway.
func (ix refIndex) project(ref *schema.Reference, v value.Value) (node *yaml.Node, refs []string, note string, ok bool) {
	switch v.Kind {
	case value.KindString:
		literal, isString := stringLiteral(v)
		if !isString {
			return nil, nil, "", false
		}
		if name, hit := ix.resolve(ref, v); hit {
			return scalarNode("${" + name + "}"), []string{name}, "", true
		}
		return scalarNode(literal), nil, unmatchedNote(ref, []string{literal}), true

	case value.KindList:
		items, isList := v.Raw.([]value.Value)
		if !isList {
			return nil, nil, "", false
		}
		seq := &yaml.Node{Kind: yaml.SequenceNode}
		var unmatched []string
		for _, item := range items {
			literal, isString := stringLiteral(item)
			if !isString {
				// A list of ids with something else in it is not a shape this
				// can project entry by entry without inventing a rule for the
				// rest, so the whole attribute is emitted as it stands.
				return nil, nil, "", false
			}
			if name, hit := ix.resolve(ref, item); hit {
				seq.Content = append(seq.Content, scalarNode("${"+name+"}"))
				refs = append(refs, name)
				continue
			}
			seq.Content = append(seq.Content, scalarNode(literal))
			unmatched = append(unmatched, literal)
		}
		if len(seq.Content) == 0 {
			return nil, nil, "", false
		}
		return seq, refs, unmatchedNote(ref, unmatched), true

	default:
		return nil, nil, "", false
	}
}

// unmatchedNote states, at the point a reader meets the literal, that the
// resource it names is not one of the files they were just handed.
//
// A silent literal reads as a value somebody chose. This is the same reason the
// omitted-secret path writes a comment, and the reason this package emits
// yaml.Node rather than marshalling.
func unmatchedNote(ref *schema.Reference, unmatched []string) string {
	if len(unmatched) == 0 {
		return ""
	}
	return fmt.Sprintf("%s %s not discovered as %s, so the %s — a reference to a "+
		"resource outside this import set would not compile",
		strings.Join(unmatched, ", "),
		plural(len(unmatched), "was", "were"),
		plural(len(unmatched), "an "+ref.Type, ref.Type+" resources"),
		plural(len(unmatched), "literal stays", "literals stay"))
}

func scalarNode(s string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s}
}
