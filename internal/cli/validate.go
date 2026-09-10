package cli

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"infra/internal/config"
	"infra/internal/diag"
	"infra/internal/registry"
	"infra/pkg/schema"
)

// newValidateCommand builds the `infra validate` command.
func newValidateCommand(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "Check configuration for errors without contacting providers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ds := validateProject(opts.Dir, buildRegistry(opts.Dir))
			ds.Render(cmd.ErrOrStderr())

			if ds.HasErrors() {
				return errors.New("configuration is not valid")
			}
			fmt.Fprintln(cmd.OutOrStdout(), "✓ Configuration valid")
			return nil
		},
	}
}

// validateProject runs compiler stages 1 and 2, then the checks that need only
// the registry. Kind checking, defaults, references and requirements arrive
// with stage 7 in M2.
func validateProject(dir string, reg *registry.Registry) diag.Diagnostics {
	var ds diag.Diagnostics

	files, err := config.Load(dir)
	if err != nil {
		ds.Add(diag.Diagnostic{Severity: diag.SeverityError, Summary: err.Error()})
		return ds
	}

	project, decodeDiags := config.Decode(files)
	ds.Extend(decodeDiags)

	for _, r := range project.Resources {
		if r.Type == "" {
			continue // already reported by Decode
		}

		def, ok := reg.Definition(r.Type)
		if !ok {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "unknown resource type " + strconv.Quote(r.Type),
				Detail:   "Known types:\n  " + strings.Join(reg.Types(), "\n  "),
				Action:   "Correct the `type`, or check that the provider offering it is available.",
				Origin:   r.Origin,
			})
			continue
		}

		for name, attr := range r.Attributes {
			schemaAttr, known := def.Attribute(name)
			if !known {
				ds.Add(diag.Diagnostic{
					Severity: diag.SeverityError,
					Summary:  fmt.Sprintf("%s has no attribute %s", r.Type, strconv.Quote(name)),
					Detail:   "Attributes of " + r.Type + ":\n  " + strings.Join(attributeNames(def), "\n  "),
					Action:   "Remove the attribute, or correct its name — the list above is the complete set.",
					Origin:   attr.Origin,
				})
				continue
			}
			if schemaAttr.Computed {
				ds.Add(diag.Diagnostic{
					Severity: diag.SeverityError,
					Summary:  fmt.Sprintf("%s is computed and cannot be set", strconv.Quote(name)),
					Detail:   "The provider determines this value. It can be referenced by other resources, but not configured.",
					Origin:   attr.Origin,
				})
			}
		}
	}

	return ds
}

// attributeNames lists a type's attributes in sorted order, for the "did you
// mean" detail on an unknown-attribute diagnostic.
func attributeNames(def *schema.ResourceDefinition) []string {
	names := make([]string, 0, len(def.Attributes))
	for name := range def.Attributes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
