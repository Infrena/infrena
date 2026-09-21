package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/infrena/infrena/internal/compiler"
	"github.com/infrena/infrena/internal/config"
	"github.com/infrena/infrena/pkg/resource"
)

// newGraphCommand builds `infrena graph <environment>`.
//
// It compiles exactly as `plan` does and renders the dependency structure
// instead of diffing it, so what it shows is what the executor will obey rather
// than a second model of it. It does not refresh: the graph is a property of the
// configuration, so drawing it needs no network.
func newGraphCommand(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:           "graph <environment>",
		SilenceUsage:  true,
		SilenceErrors: true,
		Short:         "Show what depends on what, as a tree",
		Args:          cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			copts, cds := compilerOptions(opts, args[0])
			if cds.HasErrors() {
				cds.Render(cmd.ErrOrStderr())
				return errNotValid
			}
			files, err := config.Load(opts.Dir)
			if err != nil {
				return err
			}
			reg, loader := buildRegistryWithLoader(opts)
			defer loader.Close()
			cfg, ds := compiler.Compile(files, reg, copts)
			ds.Extend(cds)
			if ds.HasErrors() {
				ds.Render(cmd.ErrOrStderr())
				// graph has no runOutput: it takes no --output and prints
				// straight to stdout, so that is where the offer goes too.
				return configurationIsNotValid(cmd, opts, cmd.OutOrStdout(), loader)
			}
			renderGraph(cmd.OutOrStdout(), cfg.Resources)
			return nil
		},
	}
}

// renderGraph draws the dependency tree.
//
// Edges point from a resource to what it depends on, so the tree is drawn the
// other way round: a root is something nothing depends on, and its children are
// the things that depend on it. That is the order the executor creates them in.
//
// A resource reachable from more than one root appears under each rather than
// being deduplicated, because it genuinely sits beneath both.
func renderGraph(w io.Writer, resources map[string]*resource.ResolvedResource) {
	if len(resources) == 0 {
		fmt.Fprintln(w, "No resources are declared.")
		return
	}

	// dependents[x] = things that depend on x.
	dependents := map[string][]string{}
	hasDep := map[string]bool{}
	for addr, r := range resources {
		for _, d := range r.DependsOn {
			dependents[d.String()] = append(dependents[d.String()], addr)
			hasDep[addr] = true
		}
	}
	for k := range dependents {
		sort.Strings(dependents[k])
	}

	roots := make([]string, 0, len(resources))
	for addr := range resources {
		if !hasDep[addr] {
			roots = append(roots, addr)
		}
	}
	sort.Strings(roots)

	for _, root := range roots {
		fmt.Fprintln(w, label(resources, root))
		drawChildren(w, resources, dependents, root, "", map[string]bool{root: true})
	}
}

func drawChildren(
	w io.Writer,
	resources map[string]*resource.ResolvedResource,
	dependents map[string][]string,
	node, prefix string,
	onPath map[string]bool,
) {
	kids := dependents[node]
	for i, kid := range kids {
		last := i == len(kids)-1
		branch, carry := " ├── ", " │   "
		if last {
			branch, carry = " └── ", "     "
		}
		if onPath[kid] {
			// A compiled configuration cannot contain a cycle; this is marked
			// rather than followed so that a caller handing in a hand-built
			// map gets a line of output instead of unbounded recursion.
			fmt.Fprintf(w, "%s%s%s (cycle)\n", prefix, branch, kid)
			continue
		}
		fmt.Fprintf(w, "%s%s%s\n", prefix, branch, label(resources, kid))
		onPath[kid] = true
		drawChildren(w, resources, dependents, kid, prefix+carry, onPath)
		delete(onPath, kid)
	}
}

// label renders a node as the plan renders it: type first, then address, so the
// two commands name the same thing the same way.
func label(resources map[string]*resource.ResolvedResource, addr string) string {
	r, ok := resources[addr]
	if !ok {
		return addr
	}
	return strings.TrimSpace(r.Type + "." + addr)
}
