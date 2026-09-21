package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/infrena/infrena/internal/config"
	"github.com/infrena/infrena/internal/discovery"
	"github.com/infrena/infrena/internal/generator"
	"github.com/infrena/infrena/internal/providers"
	"github.com/infrena/infrena/internal/registry"
	"github.com/infrena/infrena/internal/state"
	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/provider"
)

// newImportCommand builds `infrena import <environment> [type.id...]`.
//
// Import adopts existing infrastructure into state. The hazard it exists around
// is that a resource in state that no configuration declares is scheduled for
// destruction, so an import that writes state and no configuration arms the
// next apply to delete what was just adopted.
//
// Hence --generate, and hence its ordering: configuration is written first,
// then state. Both or neither.
func newImportCommand(opts *GlobalOptions) *cobra.Command {
	var generate bool
	var instance string
	var as string
	var filters filterFlags

	cmd := &cobra.Command{
		Use:           "import <environment> [type.id...]",
		SilenceUsage:  true,
		SilenceErrors: true,
		Short:         "Adopt existing infrastructure into state",
		Long: "Adopt resources that already exist, so this tool manages them without recreating " +
			"them.\n\nWith no type.id arguments, everything discovery finds is imported. Pass " +
			"`fake.database.db-9` to import one. With two provider instances holding the same " +
			"provider ID, narrow the command with --provider <instance>.\n\nResources this project " +
			"already manages, and resources a provider says the cloud owns, are left out unless a " +
			"selector names one; naming one that is already managed is refused.\n\n--generate additionally writes the " +
			"configuration that declares what was imported, under " + config.DiscoveredDirName +
			"/. Without it you must write that configuration yourself before the next apply: a " +
			"resource in state that no configuration declares is scheduled for destruction.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			environment := args[0]
			// Before a plugin is started, let alone asked: a malformed --tag is
			// a mistake in the command line, not something to discover after a
			// survey of a real account.
			filter, err := filters.filter()
			if err != nil {
				return err
			}
			// The same plugin scope discovery has, since import adopts what
			// discovery found, but with this command's own environment, which
			// discover does not have. Without it a per-environment provider
			// value is unresolvable even though the environment was named on
			// the command line.
			reg, tbl, regDiags, closePlugins := discoveryRegistry(opts, environment, true)
			defer closePlugins()
			if regDiags.HasErrors() {
				regDiags.Render(cmd.ErrOrStderr())
				return errProviderInstances
			}
			backend, closeBackend, err := backendFor(cmd.Context(), opts)
			if err != nil {
				return err
			}
			defer closeBackend()

			return withLockedEnvironment(environment, "import", backend, cmd.ErrOrStderr(),
				func(ctx context.Context) error {
					return runImport(ctx, cmd, opts, reg, tbl, backend, environment, args[1:],
						generate, instance, as, filter)
				})
		},
	}
	cmd.Flags().BoolVar(&generate, "generate", false,
		"also write the configuration declaring what was imported, under "+config.DiscoveredDirName+"/")
	// Needed because a selector is `<type>.<provider id>` and names no instance, while
	// a provider ID is unique within an account rather than across them. It also
	// answers "adopt everything in this one account", which the no-selector form
	// could not say at all.
	cmd.Flags().StringVar(&instance, "provider", "",
		"only adopt resources belonging to this provider instance")
	// Naming is otherwise permanent. Discovery proposes a name from a tag or
	// the provider ID — `network-vpc-0a1b2c3d` — and nothing renames a managed
	// resource afterwards short of editing state by hand. Import time is the
	// one moment renaming is safe: nothing references the resource yet, so
	// there is no dependency edge, no generated file and no other resource's
	// `${...}` to rewrite.
	cmd.Flags().StringVar(&as, "as", "",
		"adopt the single named resource under this name instead of the proposed one")
	// The same three narrowing flags `discover` takes, so the list a user read
	// and the list they adopt are produced by the same question.
	filters.register(cmd)
	return cmd
}

func runImport(
	ctx context.Context, cmd *cobra.Command, opts *GlobalOptions,
	reg *registry.Registry, table providers.Table, backend state.Backend,
	environment string, selectors []string, generate bool, instance, as string,
	filter discovery.Filter,
) error {
	// Refused rather than applied to the first of several: --as names one
	// resource, and an ambiguous rename silently puts a name on something the
	// user was not looking at.
	if as != "" && len(selectors) != 1 {
		return fmt.Errorf("--as renames one resource, so it needs exactly one selector "+
			"(%d given).\nName the resource you mean, as `infrena import %s <type>.<id> --as %s`",
			len(selectors), environment, as)
	}
	managed, err := managedProviderIDs(ctx, backend)
	if err != nil {
		return err
	}

	// Progress covers both halves of an import: the sweep, and the
	// per-resource reads further down. Against a real account that is minutes
	// of silence otherwise — one Discover call over every type an instance
	// serves, then one provider call per resource adopted.
	progress := newProgressRenderer(cmd.OutOrStdout(), time.Now)
	defer progress.Stop()
	// import builds its own renderer rather than going through openRun, so it
	// wraps its own writers the way openRun does: everything below prints
	// beside a live status line. Both streams, because stderr lands on the
	// same terminal and reportSkipped writes there on purpose. Setting them on
	// the command means the call sites below need no rule to remember.
	out := statusSafeWriter(cmd.OutOrStdout(), progress)
	cmd.SetOut(out)
	cmd.SetErr(statusSafeWriter(cmd.ErrOrStderr(), progress))

	selected, skipped, problems, err := selectForImport(ctx, reg, managed, selectors, instance, filter, progress)
	for _, p := range problems {
		fmt.Fprintf(cmd.ErrOrStderr(), "Warning: %v\n", p)
	}
	if err != nil {
		return err
	}
	// After selection, so the name replaces a proposal rather than competing
	// with the matching that found the resource: selectors match on
	// type.provider_id and never on the name.
	if as != "" {
		if len(selected) != 1 {
			// Reachable when the one selector matched nothing: the flag's
			// precondition held on the command line and not on the result.
			return fmt.Errorf("--as %s: the selector matched %d resources, not one", as, len(selected))
		}
		if err := refuseManagedName(as, managed, selected[0]); err != nil {
			return err
		}
		selected[0].Name = as
	}

	reportSkipped(cmd.ErrOrStderr(), skipped)
	if len(selected) == 0 {
		fmt.Fprintln(out, "Nothing to import.")
		return nil
	}

	st, err := backend.Get(ctx, environment)
	if err != nil {
		return err
	}

	// Refuse before writing anything. An import that adopted half a selection
	// and then reported a collision leaves a user reasoning about which half.
	//
	// Two axes, and the second is the dangerous one. A name already in state is
	// the collision a user expects: importing over it would overwrite the
	// provider ID recorded there. A provider ID already in state under another
	// name leaves two addresses managing one real resource, and since the new
	// address declares nothing, the next apply destroys infrastructure the
	// first address still manages and configuration still declares.
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
			"Remove them with `infrena state rm <address>` first, or import the others by name",
			environment, strings.Join(already, ", "))
	}

	if claimed := alreadyManaged(st, selected); len(claimed) > 0 {
		sort.Strings(claimed)
		return fmt.Errorf("already managed in environment %q: %s\n"+
			"Importing it again would leave two addresses managing one resource. The second "+
			"declares nothing, so the next plan would propose destroying it — and destroying "+
			"it deletes the resource the first one manages.\n"+
			"Import something else, or use `infrena state rm <address>` if the existing entry "+
			"is the one you want to replace",
			environment, strings.Join(claimed, ", "))
	}

	// Configuration first, then state. The window between the two writes is the
	// dangerous one: state naming a resource no configuration declares reads as
	// "removed from configuration". Failing after configuration is written
	// leaves a declared resource that is not yet managed, which the next plan
	// proposes creating — visible and refusable. The other order leaves one it
	// proposes destroying.
	//
	// The edges come back with the files for the same reason they are written
	// first: generated configuration that says `network: ${network-vpc-0a1b}`
	// declares a dependency, and state that records none disagrees with it from
	// the moment both are on disk. The next plan would then propose
	// `~ depends_on: [] -> [network-vpc-0a1b]` on a resource nobody touched,
	// and a round trip must not produce a change.
	var edges generator.Edges
	if generate {
		written, generated, err := writeGenerated(opts.Dir, selected, reg, table, environment)
		if err != nil {
			return fmt.Errorf("writing configuration: %w (nothing has been imported)", err)
		}
		edges = generated
		for _, f := range written {
			fmt.Fprintf(out, "Wrote %s\n", f)
		}
	}

	// The adoption half is countable where the sweep above was not: selected
	// is exactly what will be imported.
	progress.SetTotal(len(selected))
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
		// The same edges the configuration just written declares, never a
		// second opinion computed here: generation decides whether an
		// attribute is emitted at all, so working them out again would
		// eventually answer differently. Empty without --generate, where
		// there is no configuration for an edge to match.
		rs.Dependencies = dependencyAddresses(edges[r.Name])
		st.Set(rs)
		progress.Import(r.Name)
		imported++
		fmt.Fprintf(out, "Imported %s as %s\n", r.ProviderID, r.Name)
	}

	if err := backend.Put(ctx, environment, st); err != nil {
		return err
	}

	fmt.Fprintf(out, "\n%d resource%s imported into %q.\n",
		imported, plural(imported), environment)
	if !generate {
		fmt.Fprintln(out,
			"No configuration was written. Declare these resources before the next apply, "+
				"or they will be proposed for destruction.")
	}
	return nil
}

// alreadyManaged reports which selections name a provider ID that some other
// address in this environment's state already manages, as
// "<provider id> (managed by <address>)".
//
// Keyed by type and instance, not by provider ID alone. Two accounts of one
// cloud legitimately hold the same ID — an AWS resource ID is unique within an
// account, not across them — so comparing IDs globally would refuse an import
// that is perfectly correct.
//
// A selection whose name matches the existing entry is not reported here: that
// is the same resource under the same address, which the name check above has
// already refused with a message about overwriting.
func alreadyManaged(st *state.State, selected []discovery.Result) []string {
	type claim struct{ typ, instance, id string }
	owner := map[claim]string{}
	for _, addr := range st.Addresses() {
		r, ok := st.Get(addr)
		if !ok {
			continue
		}
		if r.ProviderID == "" {
			// Nothing to collide with: an entry with no provider ID names no
			// real resource, so it cannot be the thing being adopted twice.
			continue
		}
		owner[claim{r.Type, r.Provider, r.ProviderID}] = r.Address.String()
	}

	var out []string
	for _, r := range selected {
		held, taken := owner[claim{r.Type, r.Provider, r.ProviderID}]
		if !taken || held == (address.Address{Name: r.Name}).String() {
			continue
		}
		out = append(out, r.ProviderID+" (managed by "+held+")")
	}
	return out
}

// selectForImport resolves what to import: everything discovery finds, or the
// specific `type.id` selectors given.
//
// A selector names a type and an ID because an ID alone is not unique across
// types in every provider, and because the type is what the provider needs to
// read the resource.
//
// Nothing is split: `fake.database.db-9` is matched whole against a key built
// as type + "." + provider_id, so the type keeps the dot it already contains
// and the ID keeps any it contains. Splitting at the last dot would mangle an
// ID that holds one — a hostname, an ARN, a resource path.
//
// It also reports what it left out, which the caller prints, because a
// selection that silently shrank is the failure this area exists to avoid.
func selectForImport(
	ctx context.Context, reg *registry.Registry, managed map[string]string,
	selectors []string, instance string, filter discovery.Filter,
	progress *progressRenderer,
) (selected, skipped []discovery.Result, problems []error, err error) {
	found, problems := discovery.Walk(ctx, reg, nil, readRetryPolicy(), progress.Discovery)
	// After the walk, which is after naming: uniquifying a name is
	// order-dependent, so a filter applied earlier would adopt a resource
	// under a different name depending on a flag unrelated to naming.
	found, err = filter.Apply(reg, found)
	if err != nil {
		return nil, nil, problems, err
	}
	kept, err := withoutManaged(found, selectors, managed)
	if err != nil {
		return nil, nil, problems, err
	}
	kept, skipped = withoutSystemOwned(kept, selectors)
	out, err := narrowToSelectors(ctx, reg, kept, selectors, instance)
	return out, skipped, problems, err
}

// withoutSystemOwned drops the resources the plugin says the cloud owns, unless
// a selector names one, and returns what it dropped so the caller can say so.
//
// Never by default and never silently, which is weaker than refusing outright.
// Importing the default VPC and later destroying it when it leaves
// configuration is the worst foot gun in discovery, so it must not happen by
// running `import dev` with no arguments. But adopting one is occasionally
// right, and the engine is not the party to forbid it, so naming it explicitly
// still works.
func withoutSystemOwned(
	found []discovery.Result, selectors []string,
) (kept, skipped []discovery.Result) {
	named := make(map[string]bool, len(selectors))
	for _, s := range selectors {
		named[s] = true
	}

	for _, r := range found {
		if r.SystemOwned && !named[r.Type+"."+r.ProviderID] {
			skipped = append(skipped, r)
			continue
		}
		kept = append(kept, r)
	}
	return kept, skipped
}

// reportSkipped says what was left out and why, in the plugin's own words.
//
// On stderr, with the other diagnostics: what it reports is not part of the
// import, and a frontend reading stdout is reading what happened.
func reportSkipped(w io.Writer, skipped []discovery.Result) {
	if len(skipped) == 0 {
		return
	}
	for _, r := range skipped {
		reason := r.SystemOwnedReason
		if reason == "" {
			// A plugin that flags without explaining. Still reported: a
			// silent skip is the thing being avoided.
			reason = "the provider reports the cloud owns it"
		}
		fmt.Fprintf(w, "Skipped %s %s: %s\n", r.Type, r.ProviderID, reason)
	}
	fmt.Fprintf(w, "%d resource%s skipped as cloud-owned. "+
		"Name one as `<type>.<provider id>` to adopt it anyway.\n",
		len(skipped), plural(len(skipped)))
}

// withoutManaged drops the discovered resources this project already manages
// somewhere, and refuses a selector that explicitly names one.
//
// The two halves are deliberately different. With no selector, import adopts
// what discovery found, and a resource already under management is not part of
// that question — leaving it out is the answer. A selector is a user asking for
// one specific resource, and quietly adopting nothing would report success for
// something that did not happen.
//
// managed is keyed on the provider ID and carries the environment managing it,
// so the refusal can say where to look.
func withoutManaged(
	found []discovery.Result, selectors []string, managed map[string]string,
) ([]discovery.Result, error) {
	named := make(map[string]bool, len(selectors))
	for _, s := range selectors {
		named[s] = true
	}

	var kept []discovery.Result
	var refused []string
	for _, r := range found {
		environment, isManaged := managed[r.ProviderID]
		if !isManaged {
			kept = append(kept, r)
			continue
		}
		if named[r.Type+"."+r.ProviderID] {
			refused = append(refused,
				fmt.Sprintf("%s.%s (environment %q)", r.Type, r.ProviderID, environment))
		}
	}
	if len(refused) > 0 {
		sort.Strings(refused)
		return nil, fmt.Errorf("already managed: %s\n"+
			"Adopting one resource twice puts it under two addresses, and the second declares "+
			"nothing — so the next plan would propose destroying it, and destroying it deletes "+
			"the resource the first address manages.\n"+
			"Import something else, or use `infrena state rm <address>` if the existing entry is "+
			"the one you want to replace",
			strings.Join(refused, ", "))
	}
	return kept, nil
}

// narrowToSelectors picks the discovered resources to import, optionally restricted to
// one provider instance.
//
// A selector names no instance, and that is the problem this function handles.
// `<type>.<provider id>` is the syntax, and a provider ID is unique within an
// account rather than across them — two instances of one plugin can each hold
// `net-1`. Adopting either one arbitrarily would hand the user a resource from
// the wrong account with nothing in the output saying so, so a selector
// matching in two instances is refused.
//
// `--provider` is the way out rather than new selector syntax: it narrows the
// whole command, so it also answers "adopt everything in this one account",
// which is the shape a two-account AWS project wants. Extending the selector
// would leave the no-selector form with no way to say it.
func narrowToSelectors(
	ctx context.Context, reg *registry.Registry,
	found []discovery.Result, selectors []string, instance string,
) ([]discovery.Result, error) {
	if instance != "" {
		var held []discovery.Result
		for _, r := range found {
			if r.Provider == instance {
				held = append(held, r)
			}
		}
		if len(held) == 0 && len(selectors) == 0 {
			// Not an empty import: with no selectors this would otherwise
			// adopt nothing and report success, so a typo in --provider would
			// read as "there was nothing to import".
			//
			// Only with no selectors, since a selector has a way past
			// discovery. An instance whose scan scope covers nothing can
			// still be asked for a resource by name, and refusing here would
			// put the combination that needs the direct fetch most — a narrow
			// scan scope plus an explicitly named resource — beyond reach. A
			// selector that then fails is reported by name below, which says
			// which resource rather than merely which instance.
			names := map[string]bool{}
			for _, r := range found {
				names[r.Provider] = true
			}
			return nil, fmt.Errorf("no discovered resource belongs to provider instance %q\n"+
				"Instances holding something: %s",
				instance, strings.Join(sortedKeys(names), ", "))
		}
		found = held
	}

	if len(selectors) == 0 {
		return found, nil
	}

	byID := map[string][]discovery.Result{}
	for _, r := range found {
		key := r.Type + "." + r.ProviderID
		byID[key] = append(byID[key], r)
	}

	// The names discovery already settled on, so a directly fetched resource
	// cannot be given one of them. discovery.Unique both reads and records
	// here, so this is built once and passed down rather than rebuilt per
	// selector: two directly fetched resources must not collide either.
	taken := make(map[string]string, len(found))
	for _, r := range found {
		taken[r.Name] = r.ProviderID
	}

	var out []discovery.Result
	var missing, unreachable []string
	for _, sel := range selectors {
		candidates := byID[sel]
		switch {
		case len(candidates) == 0:
			missing = append(missing, sel)
		case len(candidates) == 1:
			out = append(out, candidates[0])
		default:
			// Both instances named: without them the suggested action cannot
			// be carried out.
			held := map[string]bool{}
			for _, c := range candidates {
				held[c.Provider] = true
			}
			return nil, fmt.Errorf("%s exists in more than one provider instance: %s\n"+
				"A selector names no instance, and a provider ID is unique within an account "+
				"rather than across them, so this would adopt one of them arbitrarily.\n"+
				"Narrow it with --provider <instance>",
				sel, strings.Join(sortedKeys(held), ", "))
		}
	}
	// A selector discovery did not return is asked for directly, not refused.
	// Discovery is bounded — an AWS instance scans the regions
	// `discover_regions` names — and naming a `type.id` is a statement that the
	// resource exists, which should not be discarded in favour of a survey that
	// was never meant to be exhaustive.
	//
	// Only for selectors that were named. A bare `infrena import` still adopts
	// exactly what discovery returned, because there is no statement to act on.
	for _, sel := range missing {
		r, err := fetchDirectly(ctx, reg, sel, instance, taken)
		if err != nil {
			unreachable = append(unreachable, sel+": "+err.Error())
			continue
		}
		out = append(out, r)
	}
	if len(unreachable) > 0 {
		sort.Strings(unreachable)
		return nil, fmt.Errorf("could not import: %s\n"+
			"Each was named explicitly, so infrena asked its provider for it directly rather "+
			"than relying on discovery. A selector is `<type>.<provider id>`",
			strings.Join(unreachable, "\n  "))
	}
	return out, nil
}

// fetchDirectly asks a provider for one resource by type and ID, for a selector
// discovery did not return.
//
// The type is taken from the registry, not from the dots. A selector is matched
// whole everywhere else in this file because a provider ID may contain dots — a
// hostname, an ARN, a resource path. Here a split is unavoidable, since the
// provider has to be told the type and the ID separately, and the registry is
// the authority on which prefixes are types. Longest match wins, so a plugin
// registering both `x.db` and `x.db.replica` resolves to the more specific one.
//
// System-owned cannot be checked here: a provider reports that flag during
// discovery, and this resource is one discovery never returned. No protection
// is lost, because import already treats a selector as consent —
// withoutSystemOwned drops a cloud-owned resource only when nothing named it.
func fetchDirectly(
	ctx context.Context, reg *registry.Registry, selector, instance string, taken map[string]string,
) (discovery.Result, error) {
	resourceType, providerID, ok := splitSelector(reg, selector)
	if !ok {
		return discovery.Result{}, fmt.Errorf("no registered resource type is named by this selector")
	}

	holder := instance
	if holder == "" {
		var candidates []string
		for _, name := range reg.InstanceNames() {
			if _, ok := reg.ProviderFor(resourceType, name); ok {
				candidates = append(candidates, name)
			}
		}
		switch len(candidates) {
		case 0:
			return discovery.Result{}, fmt.Errorf("no provider instance offers %s", resourceType)
		case 1:
			holder = candidates[0]
		default:
			// The same answer a discovered ID found in two instances gets: a
			// provider ID is unique within an account rather than across
			// them, so asking an arbitrary one could adopt a resource from
			// the wrong account.
			return discovery.Result{}, fmt.Errorf(
				"%s is offered by more than one provider instance (%s); narrow it with --provider <instance>",
				resourceType, strings.Join(candidates, ", "))
		}
	}

	p, ok := reg.ProviderFor(resourceType, holder)
	if !ok {
		return discovery.Result{}, fmt.Errorf("provider instance %q does not offer %s", holder, resourceType)
	}
	rs, err := p.Import(ctx, resourceType, providerID)
	if err != nil {
		return discovery.Result{}, err
	}
	if rs == nil {
		return discovery.Result{}, fmt.Errorf("provider instance %q reports no such resource", holder)
	}

	// Named through the same function discovery names by, against the same
	// taken map, so a directly fetched resource cannot collide with a
	// discovered one and is named as `infrena discover` would have named it.
	dr := provider.DiscoveredResource{
		Type:       resourceType,
		ProviderID: providerID,
		Attributes: rs.Attributes,
	}
	return discovery.Result{
		Name:       discovery.Unique(reg, taken, dr),
		Type:       resourceType,
		ProviderID: providerID,
		Provider:   holder,
		Attributes: rs.Attributes,
	}, nil
}

// splitSelector separates a selector into a registered resource type and the
// provider ID that follows it. See fetchDirectly for why the registry decides
// this rather than the dots.
func splitSelector(reg *registry.Registry, selector string) (resourceType, providerID string, ok bool) {
	for _, t := range reg.Types() {
		if !strings.HasPrefix(selector, t+".") {
			continue
		}
		if len(t) > len(resourceType) {
			resourceType, providerID, ok = t, selector[len(t)+1:], true
		}
	}
	return resourceType, providerID, ok && providerID != ""
}

// sortedKeys lists a set's members in a stable order, so a diagnostic naming
// several instances reads the same way on every run.
func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// writeGenerated renders configuration for what is being imported and merges it
// into discovered/.
//
// It never overwrites: an existing file is read, and only resources it does not
// already declare are appended. Re-running import must be safe, because it is
// the command people run when they are unsure what happened the first time.
//
// It returns the files it wrote and the dependency edges the generated
// configuration declares, which the caller records into state. The two must
// come from one answer, or configuration and state disagree the moment both
// exist.
func writeGenerated(
	dir string, selected []discovery.Result, reg *registry.Registry,
	table providers.Table, environment string,
) ([]string, generator.Edges, error) {
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

	// The instance an imported resource belongs to decides which `defaults:`
	// block minimality measures against, so a value an instance already
	// supplies is not written back out for the user to maintain.
	gopts := generator.MinimalOptions()
	gopts.InstanceDefaults = defaultsByInstance(table)

	files, edges, err := generator.Generate(resources, reg, gopts)
	if err != nil {
		return nil, nil, err
	}

	target := filepath.Join(dir, config.DiscoveredDirName)
	if err := os.MkdirAll(target, 0o755); err != nil {
		return nil, nil, err
	}

	var written []string
	for _, f := range files {
		path := filepath.Join(target, f.Name)
		existing, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return nil, nil, err
		}
		merged, changed := mergeGenerated(existing, f.Bytes)
		if !changed {
			continue
		}
		if err := os.WriteFile(path, merged, 0o644); err != nil {
			return nil, nil, err
		}
		written = append(written, filepath.Join(config.DiscoveredDirName, f.Name))
	}
	// The edges of everything generated, including a resource whose block a
	// file already declared and so was not appended. That block came from this
	// same generator, so its references are these references; recording
	// nothing for it would leave its state disagreeing with the file beside
	// it.
	return written, edges, nil
}

// dependencyAddresses turns generated reference targets into the dependency
// list state records.
//
// Sorted through address.Sort, the same ordering the compiler puts on
// ResolvedResource.DependsOn, because the planner compares the two lists
// positionally: two identical sets in different orders would plan as a change.
func dependencyAddresses(names []string) []address.Address {
	if len(names) == 0 {
		return nil
	}
	out := make([]address.Address, 0, len(names))
	for _, n := range names {
		out = append(out, address.Address{Name: n})
	}
	address.Sort(out)
	return out
}

// mergeGenerated appends the resource blocks of fresh that existing does not
// already declare, returning the merged bytes and whether anything changed.
//
// Textual rather than structural, deliberately: internal/config is the only
// package allowed to parse YAML, and the generator's own output is the only
// input shape this has to handle. A file the user has edited by hand is still
// safe, because the only thing this looks for is whether a resource name is
// already declared; anything it does not recognise it leaves untouched.
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
			// A comment at resource indentation introduces the next resource.
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

// refuseManagedName refuses a --as that collides with a name this project
// already manages.
//
// State is keyed by address, so importing over an existing name would replace
// that resource's entry, and the resource it displaced would still exist in the
// cloud with nothing recording it. That is the same hazard the already-managed
// check exists for, arrived at from the other side.
func refuseManagedName(name string, managed map[string]string, r discovery.Result) error {
	for providerID, address := range managed {
		if address != name {
			continue
		}
		return fmt.Errorf("--as %s: this project already manages a resource called %s (provider ID %s).\n"+
			"Importing %s under that name would replace it in state and leave the displaced resource "+
			"managed by nothing.\nChoose another name.",
			name, name, providerID, r.ProviderID)
	}
	return nil
}
