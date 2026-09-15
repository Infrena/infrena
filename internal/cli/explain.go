package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/infrena/infrena/pkg/schema"
)

// newExplainCommand builds `infrena explain <resource-type>` (spec §30).
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
			reg, closePlugins := buildRegistry(opts)
			defer closePlugins()

			// THE TYPE NAMES THE PLUGIN. A plugin serves `<name>.*` and nothing
			// else, so `explain aws.instance` is itself the instruction to load
			// infrena-plugin-aws — and explain therefore works with no project and
			// no configuration at all, which is most of what it is for.
			//
			// The project's own plugins are loaded too, so `explain` in a project
			// can list everything available there when the type is wrong.
			ds := loadConfiguredPlugins(reg, opts.Dir)
			if prefix, _, ok := strings.Cut(args[0], "."); ok && prefix != "" {
				if err := reg.EnsurePlugin(cmd.Context(), prefix); err != nil && len(reg.Types()) == 0 {
					// Only when nothing else loaded either: a project that uses
					// other plugins should hear what its types ARE, rather than a
					// failure about one it does not use.
					return fmt.Errorf("cannot describe %q: %w", args[0], err)
				}
			}
			_ = ds

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
	// An attribute is in exactly one group, so a reader counting them sees the whole
	// surface. An optional+computed attribute (§14.1) gets its OWN group rather than
	// being filed under Optional or Computed, because it is neither: a reader told
	// "Computed" would not know they may set it, and one told "Optional" would not know
	// a value appears if they do not.
	var required, optional, chosen, computed []string
	for _, name := range sortedAttrNames(def.Attributes) {
		a := def.Attributes[name]
		switch {
		case a.Computed && a.Optional:
			chosen = append(chosen, name)
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
	section(w, "Optional, chosen by the provider if unset", chosen, def)
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
		// PLAN.md §14.3: a declared reference is what lets `${vpc}` project to the
		// right attribute instead of a user guessing id versus arn. `explain` is
		// where that declaration must be discoverable without reading the
		// plugin's source.
		if a.References != nil {
			notes = append(notes, fmt.Sprintf("refers to %s.%s", a.References.Type, a.References.Attribute))
		}
		if d := describeDefault(a); d != "" {
			notes = append(notes, d)
		}
		if a.Description != "" {
			line += " " + a.Description
		}
		// Every accepted spelling. `explain` is where a user finds out what they may
		// write, and an alias nothing lists is an alias nobody uses.
		if spellings := def.Spellings(name); len(spellings) > 1 {
			notes = append(notes, "also "+strings.Join(spellings[1:], ", "))
		}
		if len(notes) > 0 {
			line += "  (" + strings.Join(notes, "; ") + ")"
		}
		fmt.Fprintln(w, line)
	}
}

// describeDefault renders an attribute's default.
//
// ONE value, stated once. Until M9 this probed two DefaultContexts and rendered
// "default: 10, or 100 in production", because a default was a function of the
// environment. PLAN.md §13 withdraws that: a provider default is a single value
// per attribute, and anything that should differ between environments is a
// variable the reader can see in the configuration. The whole reason this
// function was complicated is gone with it.
func describeDefault(a schema.Attribute) string {
	if a.Default == nil {
		return ""
	}
	return fmt.Sprintf("default: %v", a.Default)
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
