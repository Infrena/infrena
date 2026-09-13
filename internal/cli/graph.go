package cli

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/infrata/infrata/internal/compiler"
	"github.com/infrata/infrata/internal/config"
	"github.com/infrata/infrata/pkg/resource"
)

// newGraphCommand builds `infrata graph <environment>` (spec §40).
//
// It compiles exactly as `plan` does and renders the dependency structure
// instead of diffing it — so what it shows is what the executor will obey, not
// a second model of it. It does NOT refresh: the graph is a property of the
// configuration, and reaching a provider to draw a tree would make a read-only
// command depend on the network.
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
				return errors.New("configuration is not valid")
			}
			files, err := config.Load(opts.Dir)
			if err != nil {
				return err
			}
			reg, closePlugins := buildRegistry(opts)
			defer closePlugins()
			cfg, ds := compiler.Compile(files, reg, copts)
			ds.Extend(cds)
			if ds.HasErrors() {
				ds.Render(cmd.ErrOrStderr())
				return errors.New("configuration is not valid")
			}
			renderGraph(cmd.OutOrStdout(), cfg.Resources)
			return nil
		},
	}
}

// renderGraph draws §40's tree.
//
// Edges point from a resource to what it depends on, so the tree is drawn the
// other way round: a root is something nothing depends on, and its children are
// the things that depend on IT. That is the order the executor creates them in,
// which is the question a reader of this tree is actually asking.
//
// A resource reachable from more than one root appears under each. That is not
// a defect to deduplicate: the tree answers "what must exist before this", and
// a shared dependency genuinely sits beneath both.
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
			// A cycle cannot reach here from a compiled configuration — stage 6
			// refuses a self-reference and the graph refuses a cycle before any
			// plan exists. Marked rather than followed so that a future caller
			// handing this function a hand-built map gets a line of output
			// instead of an unbounded recursion.
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
