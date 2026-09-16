package cli

import (
	"context"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/infrena/infrena/internal/pluginhost"
)

// newPluginsCommand builds `infrena plugins` (PLAN.md §31.3).
//
// `list` ANSWERS A QUESTION WITH NO NETWORK: what am I actually running. That
// is deliberate and is the rule the whole subsection is built on — searching
// happens in `plugins search` and `plugins install`, and in one interactive
// prompt, never anywhere a plan or an apply can reach.
//
// A parent with one subcommand today, because `search`, `install` and `verify`
// land beside it in the units that follow and moving `list` under a parent
// later would be a product API change.
func newPluginsCommand(opts *GlobalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plugins",
		Short: "Inspect the provider plugins on this machine",
	}
	cmd.AddCommand(newPluginsListCommand(opts))
	return cmd
}

// newPluginsListCommand reports every plugin that could be loaded, the version
// it reports and where it was loaded from.
func newPluginsListCommand(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:           "list",
		Short:         "List the installed provider plugins",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ro, closeRun, err := openRun(cmd, opts, "plugins list", "")
			if err != nil {
				return err
			}
			defer closeRun()

			loader := listLoader(opts)
			defer loader.Close()

			renderPluginList(cmd.Context(), ro, loader)
			return nil
		},
	}
}

// listLoader builds the loader `plugins list` reports through.
//
// NO CONSTRAINTS, unlike every other command's loader. The project's `plugins:`
// floor decides whether a version is ACCEPTABLE, and this command is asked what
// is INSTALLED. A plugin that violates the constraint is exactly the one whose
// version the user is trying to read, and refusing to load it would withhold
// the answer they came for.
func listLoader(opts *GlobalOptions) *pluginhost.Loader {
	return &pluginhost.Loader{
		Search:  pluginhost.DefaultSearch(opts.Dir, opts.PluginDirs),
		Dir:     opts.Dir,
		Verbose: verboseWriter(opts),
		Builtin: builtinsFor(opts.Dir),
	}
}

// renderPluginList prints the table, or the sentence that stands in for it.
func renderPluginList(ctx context.Context, ro *runOutput, loader *pluginhost.Loader) {
	names := loader.Available()
	if len(names) == 0 {
		// AN ANSWER, NOT A FAILURE, and exit 0 says so. A fresh machine has no
		// plugins, and telling someone in that position that something went
		// wrong is both untrue and unactionable.
		fmt.Fprintln(ro.Out(), "No plugins installed.")
		fmt.Fprintln(ro.Out(), "Run `infrena plugins install <name>` to install one.")
		return
	}

	w := tabwriter.NewWriter(ro.Out(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tVERSION\tPATH")
	for _, name := range names {
		version, path := pluginRow(ctx, loader, name)
		fmt.Fprintf(w, "%s\t%s\t%s\n", name, version, path)
	}
	_ = w.Flush()
}

// pluginRow loads a plugin and reads back what it turned out to be.
//
// LOADING IS HOW A VERSION IS LEARNED: it comes from the handshake, so there is
// no way to report it without starting the plugin. Still no network — starting
// a binary that is already on disk is not a request.
func pluginRow(ctx context.Context, loader *pluginhost.Loader, name string) (version, path string) {
	if _, err := loader.Load(ctx, name); err != nil {
		// Listed anyway. A plugin that will not start is the most important row
		// in this table, and omitting it would answer "what is installed" with
		// a list that does not mention the thing that is broken.
		return "unavailable", "-"
	}
	path, version, ok := loader.Resolved(name)
	if !ok {
		return "unavailable", "-"
	}
	if version == "" {
		// The SDK's answer for a plugin that does not implement Version().
		version = "0.0.0"
	}
	if path == "" {
		// A builtin is served inside this process and has no binary. Saying so
		// is better than an empty cell a reader would take for a bug.
		path = "(in process)"
	}
	return version, path
}
