package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/infrata/infrata/pkg/schema"
)

// newExplainCommand builds `infrata explain <resource-type>` (spec §30).
//
// It renders from the REGISTRY, which is the whole point: §30 asks for a
// resource type to be discoverable without consulting documentation, and
// rendering from the same definitions the compiler validates against is what
// stops the documentation drifting from the schema it describes. There is no
// second source of truth to keep in step.
func newExplainCommand(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:           "explain <resource-type>",
		SilenceUsage:  true,
		SilenceErrors: true,
		Short:         "Describe a resource type: its attributes, requirements and capabilities",
		Args:          cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg := buildRegistry(opts.Dir)
			def, ok := reg.Definition(args[0])
			if !ok {
				// Listing what DOES exist is the difference between an error a
				// user can act on and one that only says no.
				return fmt.Errorf("unknown resource type %q\n\nKnown types:\n  %s",
					args[0], strings.Join(reg.Types(), "\n  "))
			}
			explain(cmd.OutOrStdout(), def)
			return nil
		},
	}
}

// explain renders one definition.
func explain(w io.Writer, def *schema.ResourceDefinition) {
	fmt.Fprintf(w, "%s\n", def.Type)
	if def.Description != "" {
		fmt.Fprintf(w, "  %s\n", def.Description)
	}

	// Grouped as §30 groups them. An attribute is in exactly one group, so a
	// reader counting them sees the whole surface.
	var required, optional, computed []string
	for _, name := range sortedAttrNames(def.Attributes) {
		a := def.Attributes[name]
		switch {
		case a.Computed:
			computed = append(computed, name)
		case a.Required:
			required = append(required, name)
		default:
			optional = append(optional, name)
		}
	}

	section(w, "Required", required, def)
	section(w, "Optional", optional, def)
	section(w, "Computed", computed, def)

	if len(def.Requirements) > 0 {
		fmt.Fprintf(w, "\nRequires:\n")
		for _, r := range def.Requirements {
			fmt.Fprintf(w, "  %-16s %s\n", strings.Join(r.Types, ", "), r.Description)
		}
	}

	fmt.Fprintf(w, "\nCapabilities:\n  %s\n", strings.Join(capabilities(def.Capabilities), ", "))

	if def.ImportID.Description != "" {
		fmt.Fprintf(w, "\nImport ID:\n  %s\n", def.ImportID.Description)
	}
}

func section(w io.Writer, title string, names []string, def *schema.ResourceDefinition) {
	if len(names) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%s:\n", title)
	for _, name := range names {
		a := def.Attributes[name]
		line := fmt.Sprintf("  %-16s %-8s", name, a.Kind)
		var notes []string
		if a.Sensitive {
			notes = append(notes, "sensitive")
		}
		if a.ForceNew {
			notes = append(notes, "replaces on change")
		}
		if d := describeDefault(a, def.Type); d != "" {
			notes = append(notes, d)
		}
		if a.Description != "" {
			line += " " + a.Description
		}
		if len(notes) > 0 {
			line += "  (" + strings.Join(notes, "; ") + ")"
		}
		fmt.Fprintln(w, line)
	}
}

// describeDefault renders an attribute's default.
//
// A default is a FUNCTION of the environment, not a value — test.database's
// size is 100 in production and 10 everywhere else. Printing one number would
// be true in one environment and a lie in the other, so this probes both and
// says so when they differ. §30's example shows a bare `default: 1`, which is
// only honest for a default that does not vary.
func describeDefault(a schema.Attribute, typ string) string {
	if a.Default == nil {
		return ""
	}
	base := schema.DefaultContext{EnvironmentType: "development", Type: typ}
	prod := schema.DefaultContext{EnvironmentType: "production", Type: typ}

	dv, dok := a.Default(base)
	pv, pok := a.Default(prod)
	switch {
	case !dok && !pok:
		return ""
	case dok && pok && fmt.Sprint(dv) == fmt.Sprint(pv):
		return fmt.Sprintf("default: %v", dv)
	case dok && pok:
		return fmt.Sprintf("default: %v, or %v in production", dv, pv)
	case pok:
		return fmt.Sprintf("default: %v in production only", pv)
	default:
		return fmt.Sprintf("default: %v outside production", dv)
	}
}

func capabilities(c schema.Capabilities) []string {
	var out []string
	for _, p := range []struct {
		on   bool
		name string
	}{
		{c.Create, "create"}, {c.Read, "read"}, {c.Update, "update"},
		{c.Delete, "delete"}, {c.Import, "import"},
	} {
		if p.on {
			out = append(out, p.name)
		}
	}
	if len(out) == 0 {
		return []string{"none"}
	}
	return out
}

func sortedAttrNames(attrs map[string]schema.Attribute) []string {
	out := make([]string, 0, len(attrs))
	for n := range attrs {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
