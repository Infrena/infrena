package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/infrata/infrata/internal/config"
	"github.com/infrata/infrata/internal/discovery"
	"github.com/infrata/infrata/internal/generator"
	"github.com/infrata/infrata/internal/providers"
	"github.com/infrata/infrata/internal/registry"
	"github.com/infrata/infrata/internal/state"
	"github.com/infrata/infrata/pkg/address"
)

// newImportCommand builds `infrata import <environment> [type.id...]` (spec §26).
//
// Import adopts existing infrastructure into state. The hazard it exists to
// avoid is the one invariant 1 creates: a resource in state that no
// configuration declares is scheduled for DESTRUCTION, so an import that writes
// state and no configuration arms the next apply to delete what was just
// adopted.
//
// Hence --generate, and hence its ordering. Configuration is written FIRST,
// then state. Both or neither.
func newImportCommand(opts *GlobalOptions) *cobra.Command {
	var generate bool

	cmd := &cobra.Command{
		Use:           "import <environment> [type.id...]",
		SilenceUsage:  true,
		SilenceErrors: true,
		Short:         "Adopt existing infrastructure into state",
		Long: "Adopt resources that already exist, so this tool manages them without recreating " +
			"them.\n\nWith no type.id arguments, everything discovery finds is imported. Pass " +
			"`test.database.db-9` to import one.\n\n--generate additionally writes the " +
			"configuration that declares what was imported, under " + config.DiscoveredDirName +
			"/. Without it you must write that configuration yourself before the next apply: a " +
			"resource in state that no configuration declares is scheduled for destruction.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			environment := args[0]
			// The same scope discovery has: import adopts what discovery found, so
			// it must be able to ask the same plugins.
			reg, tbl, regDiags, closePlugins := discoveryRegistry(opts)
			defer closePlugins()
			if regDiags.HasErrors() {
				regDiags.Render(cmd.ErrOrStderr())
				return errProviderInstances
			}
			backend := backendFor(opts.Dir)

			return withLockedEnvironment(environment, "import", backend, cmd.ErrOrStderr(),
				func(ctx context.Context) error {
					return runImport(ctx, cmd, opts, reg, tbl, backend, environment, args[1:], generate)
				})
		},
	}
	cmd.Flags().BoolVar(&generate, "generate", false,
		"also write the configuration declaring what was imported, under "+config.DiscoveredDirName+"/")
	return cmd
}

func runImport(
	ctx context.Context, cmd *cobra.Command, opts *GlobalOptions,
	reg *registry.Registry, table providers.Table, backend *state.Local,
	environment string, selectors []string, generate bool,
) error {
	selected, problems, err := selectForImport(ctx, reg, selectors)
	for _, p := range problems {
		fmt.Fprintf(cmd.ErrOrStderr(), "Warning: %v\n", p)
	}
	if err != nil {
		return err
	}
	if len(selected) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "Nothing to import.")
		return nil
	}

	st, err := backend.Get(ctx, environment)
	if err != nil {
		return err
	}

	// Refuse before writing anything. An import that adopted half a selection
	// and then reported a collision leaves a user reasoning about which half.
	var already []string
	for _, r := range selected {
		if _, exists := st.Get(address.Address{Name: r.Name}); exists {
			already = append(already, r.Name)
		}
	}
	if len(already) > 0 {
		sort.Strings(already)
		return fmt.Errorf("already in the state of environment %q: %s\n"+
			"Importing would overwrite what is recorded there, including the provider IDs. "+
			"Remove them with `infrata state rm <address>` first, or import the others by name",
			environment, strings.Join(already, ", "))
	}

	// CONFIGURATION FIRST, then state. The window between the two writes is the
	// dangerous one: state naming a resource no configuration declares is
	// exactly what invariant 1 reads as "removed from configuration". Failing
	// after configuration is written leaves a declared resource that is not yet
	// managed, which the next plan proposes CREATING — visible and refusable.
	// The other order leaves one it proposes DESTROYING.
	if generate {
		written, err := writeGenerated(opts.Dir, selected, reg, table, environment)
		if err != nil {
			return fmt.Errorf("writing configuration: %w (nothing has been imported)", err)
		}
		for _, f := range written {
			fmt.Fprintf(cmd.OutOrStdout(), "Wrote %s\n", f)
		}
	}

	imported := 0
	for _, r := range selected {
		p, ok := reg.ProviderFor(r.Type, r.Provider)
		if !ok {
			return fmt.Errorf("no provider offers %s", r.Type)
		}
		rs, err := p.Import(ctx, r.Type, r.ProviderID)
		if err != nil {
			return fmt.Errorf("importing %s %s: %w", r.Type, r.ProviderID, err)
		}
		rs.Address = address.Address{Name: r.Name}
		st.Set(rs)
		imported++
		fmt.Fprintf(cmd.OutOrStdout(), "Imported %s as %s\n", r.ProviderID, r.Name)
	}

	if err := backend.Put(ctx, environment, st); err != nil {
		return err
	}

	fmt.Fprintf(cmd.OutOrStdout(), "\n%d resource%s imported into %q.\n",
		imported, plural(imported), environment)
	if !generate {
		fmt.Fprintln(cmd.OutOrStdout(),
			"No configuration was written. Declare these resources before the next apply, "+
				"or they will be proposed for destruction.")
	}
	return nil
}

// selectForImport resolves what to import: everything discovery finds, or the
// specific `type.id` selectors given.
//
// A selector names a TYPE and an ID because an ID alone is not unique across
// types in every provider, and because the type is what the provider needs to
// read the resource. `test.database.db-9` splits at the LAST dot: a type
// already contains one.
func selectForImport(ctx context.Context, reg *registry.Registry, selectors []string) ([]discovery.Result, []error, error) {
	found, problems := discovery.Walk(ctx, reg, nil)
	if len(selectors) == 0 {
		return found, problems, nil
	}

	byID := map[string]discovery.Result{}
	for _, r := range found {
		byID[r.Type+"."+r.ProviderID] = r
	}

	var out []discovery.Result
	var missing []string
	for _, sel := range selectors {
		r, ok := byID[sel]
		if !ok {
			missing = append(missing, sel)
			continue
		}
		out = append(out, r)
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, problems, fmt.Errorf("not found by discovery: %s\n"+
			"Run `infrata discover` to see what exists. A selector is `<type>.<provider id>`",
			strings.Join(missing, ", "))
	}
	return out, problems, nil
}

// writeGenerated renders configuration for what is being imported and merges it
// into discovered/.
//
// NEVER OVERWRITES. An existing file is read, and only resources it does not
// already declare are appended. Re-running import must be safe, because it is
// the command people run when they are unsure what happened the first time.
func writeGenerated(
	dir string, selected []discovery.Result, reg *registry.Registry,
	table providers.Table, environment string,
) ([]string, error) {
	resources := make([]generator.Resource, 0, len(selected))
	for _, r := range selected {
		resources = append(resources, generator.Resource{
			Name:       r.Name,
			Type:       r.Type,
			ProviderID: r.ProviderID,
			Provider:   r.Provider,
			Attributes: r.Attributes,
		})
	}

	// The instance an imported resource belongs to decides which `defaults:` block
	// minimality measures against, so a value an instance already supplies is not
	// written back out for the user to maintain (PLAN.md §12.1, §27).
	gopts := generator.MinimalOptions()
	gopts.InstanceDefaults = defaultsByInstance(table)

	files, err := generator.Generate(resources, reg, gopts)
	if err != nil {
		return nil, err
	}

	target := filepath.Join(dir, config.DiscoveredDirName)
	if err := os.MkdirAll(target, 0o755); err != nil {
		return nil, err
	}

	var written []string
	for _, f := range files {
		path := filepath.Join(target, f.Name)
		existing, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		merged, changed := mergeGenerated(existing, f.Bytes)
		if !changed {
			continue
		}
		if err := os.WriteFile(path, merged, 0o644); err != nil {
			return nil, err
		}
		written = append(written, filepath.Join(config.DiscoveredDirName, f.Name))
	}
	return written, nil
}

// mergeGenerated appends the resource blocks of fresh that existing does not
// already declare, returning the merged bytes and whether anything changed.
//
// Textual rather than structural, and deliberately so: this package must not
// parse YAML (spec §7 — internal/config is the only package permitted to), and
// the generator's own output is the only input shape it has to handle. A file
// the user has edited by hand is still safe, because the only thing this looks
// for is whether a resource NAME is already declared; anything it does not
// recognise it leaves untouched.
func mergeGenerated(existing, fresh []byte) ([]byte, bool) {
	if len(strings.TrimSpace(string(existing))) == 0 {
		return fresh, true
	}

	declared := declaredNames(string(existing))
	var add []string
	for _, block := range resourceBlocks(string(fresh)) {
		if declared[block.name] {
			continue
		}
		add = append(add, block.text)
	}
	if len(add) == 0 {
		return existing, false
	}

	out := strings.TrimRight(string(existing), "\n") + "\n" + strings.Join(add, "")
	return []byte(out), true
}

type genBlock struct {
	name string
	text string
}

// resourceBlocks splits generated output into one chunk per resource, keeping
// each resource's leading comments with it.
func resourceBlocks(s string) []genBlock {
	var out []genBlock
	var cur *genBlock
	var pending []string

	for line := range strings.SplitSeq(s, "\n") {
		if name, ok := resourceKey(line); ok {
			out = append(out, genBlock{})
			cur = &out[len(out)-1]
			cur.name = name
			cur.text = strings.Join(pending, "") + line + "\n"
			pending = nil
			continue
		}
		if cur == nil {
			// Before the first resource: the file header and `resources:`.
			// A comment here may belong to the first resource, so it is held
			// rather than discarded.
			if strings.HasPrefix(strings.TrimSpace(line), "#") && strings.HasPrefix(line, "  ") {
				pending = append(pending, line+"\n")
			}
			continue
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "#") && strings.HasPrefix(line, "  ") &&
			!strings.HasPrefix(line, "    ") {
			// A comment at resource indentation introduces the NEXT resource.
			pending = append(pending, line+"\n")
			cur = nil
			continue
		}
		cur.text += line + "\n"
	}
	return out
}

// resourceKey reports the name a line declares, if it is a resource key: two
// spaces of indentation, an identifier, a colon, and nothing after it.
func resourceKey(line string) (string, bool) {
	if !strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "   ") {
		return "", false
	}
	trimmed := strings.TrimSpace(line)
	if !strings.HasSuffix(trimmed, ":") {
		return "", false
	}
	name := strings.TrimSuffix(trimmed, ":")
	if name == "" || !config.ValidResourceName(name) {
		return "", false
	}
	return name, true
}

func declaredNames(s string) map[string]bool {
	out := map[string]bool{}
	for line := range strings.SplitSeq(s, "\n") {
		if name, ok := resourceKey(line); ok {
			out[name] = true
		}
	}
	return out
}

func projectName(dir string) string {
	files, err := config.Load(dir)
	if err != nil {
		return ""
	}
	decl, ds := config.Decode(files)
	if ds.HasErrors() {
		return ""
	}
	return decl.Project
}
