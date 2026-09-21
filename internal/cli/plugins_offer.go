package cli

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/infrena/infrena/internal/pluginhost"
	"github.com/infrena/infrena/internal/plugins"
)

// The interactive offer to install a plugin a command could not find, and the
// one-line hint that replaces it when nobody is there to answer.
//
// This runs only off the path a command takes when its configuration did not
// compile, so an accepted offer installs and stops: the command has already
// failed, and its only remaining act is to return. That keeps a run from
// happening half under one set of installed plugins and half under another.
//
// It is also the only place on those commands that may reach the network. A
// person answering a question is not `plan` reaching out on its own, and every
// gate below exists to keep the two apart.

// pluginSearchOffVar turns the offer off entirely, for the machine that has a
// terminal and still must not reach out: an air-gapped runner that allocates a
// tty, or a shell where the question is unwelcome. Set to anything non-empty,
// no search is made at all — not a search whose answer is discarded.
const pluginSearchOffVar = "INFRENA_NO_PLUGIN_SEARCH"

// stdinIsTerminal reports whether a person could answer a question.
//
// A character device, minus /dev/null. Telling a tty from any other character
// device needs an ioctl, and the libraries that wrap one are outside this
// project's dependency budget — but /dev/null is the character device a process
// is handed when nobody is there: `go test` does it, and so do cron, systemd
// and most CI runners. Without that exclusion a failing `plan` under `go test`
// reaches api.github.com, which is exactly the rule this gate holds: a run with
// nobody at the terminal must not reach out.
//
// A variable because it is the seam the tests replace; there is no terminal
// inside `go test` to stand one up against.
var stdinIsTerminal = func(in io.Reader) bool {
	f, ok := in.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	if null, err := os.Stat(os.DevNull); err == nil && os.SameFile(info, null) {
		return false
	}
	return true
}

// errNotValid is what a command whose diagnostics carried an error returns.
var errNotValid = errors.New("configuration is not valid")

// configurationIsNotValid returns errNotValid, offering to install any missing
// plugin first.
//
// out is the run's own writer, so --output silences the conversation along with
// everything else on stdout; the hint goes to stderr, which it does not.
func configurationIsNotValid(cmd *cobra.Command, opts *GlobalOptions, out io.Writer, loader *pluginhost.Loader) error {
	// A nil loader is a command that failed before it built a registry, so
	// nothing was asked for and nothing can be missing.
	if loader == nil {
		return errNotValid
	}
	offerMissingPlugins(cmd, opts, out, loader.Missing())
	return errNotValid
}

// offerMissingPlugins asks about every plugin that could not be found, or, when
// there is nobody to ask, prints the command that would fix it.
func offerMissingPlugins(cmd *cobra.Command, opts *GlobalOptions, out io.Writer, missing []string) {
	if len(missing) == 0 {
		return
	}

	if canOfferToInstall(cmd, opts) {
		var installed []string
		for _, name := range missing {
			ok, err := offerToInstall(cmd, opts, out, name)
			if err != nil {
				// A failed offer is a note, not the command's error: the
				// caller already has a failure to read, and a rate limit that
				// stopped the lookup is context for it rather than a second
				// thing that went wrong.
				fmt.Fprintf(cmd.ErrOrStderr(), "could not offer to install %s: %v\n", name, err)
				continue
			}
			if ok {
				installed = append(installed, name)
			}
		}
		if len(installed) > 0 {
			was := "is"
			if len(installed) > 1 {
				was = "are"
			}
			fmt.Fprintf(out, "\nNothing else has run. Re-run your command now that %s %s installed.\n",
				joinNames(installed), was)
			return
		}
	}

	printInstallHint(cmd.ErrOrStderr(), missing)
}

// canOfferToInstall reports whether a question may be put at all.
func canOfferToInstall(cmd *cobra.Command, opts *GlobalOptions) bool {
	// --output means a frontend is reading a file and nothing is watching
	// stdout, so a question written there would reach nobody. Same test apply
	// uses before asking for approval, for the same reason.
	if opts.Output != "" {
		return false
	}
	if os.Getenv(pluginSearchOffVar) != "" {
		return false
	}
	return stdinIsTerminal(cmd.InOrStdin())
}

// offerToInstall searches for one missing plugin, shows what is published, and
// asks.
//
// It shows everything and may then refuse to ask. A plugin that publishes no
// build for this machine, and two owners publishing one name, are both worth
// printing and neither is installable — the first because there is nothing to
// install, the second because infrena does not choose between sources.
// renderSearch says so in the table, so the question is simply not put.
func offerToInstall(cmd *cobra.Command, opts *GlobalOptions, out io.Writer, name string) (bool, error) {
	sources, untrusted, err := searchSources(opts.Dir, name)
	if err != nil {
		return false, err
	}

	// Said before the request, because the request is the slow part and a
	// terminal that goes quiet after an error looks like one that has hung.
	fmt.Fprintf(out, "\nThe %s plugin is not installed. Looking for it...\n\n", name)

	fetcher := searchFetcher(false)
	found, problems := plugins.Search(cmd.Context(), fetcher, sources, name, searchEnvironment(opts.Dir, name))
	renderSearchWarnings(cmd.ErrOrStderr(), problems, found)
	renderSearch(out, name, found, untrusted, len(problems) > 0, fetcher.authenticated())

	// Always a provider: this offer is made when a resource type names a
	// plugin that is not on the search path, and a missing backend is
	// backendhost's error instead. Saying so keeps a backend of the same name
	// out of the count, where it would look like a second publisher.
	providers := candidatesWithRole(found, plugins.RoleProvider)
	if _, err := soleCandidate(name, "", providers, len(problems) > 0, fetcher.authenticated()); err != nil {
		// The table above has already said why each unusable candidate is
		// unusable, so asking a question install would only refuse would be
		// saying it twice.
		return false, nil
	}

	prompt := fmt.Sprintf("\nInstall %s and stop, so you can re-run your command? Type yes: ", name)
	if confirm(cmd.InOrStdin(), out, prompt, "yes") != approvalGranted {
		fmt.Fprintln(out)
		return false, nil
	}

	// The same code path `infrena plugins install` takes, trust prompt
	// included. A second way to install a plugin would be a second set of
	// rules about what may be installed.
	if err := runInstall(cmd, opts, out, name, false, plugins.RoleProvider.String()); err != nil {
		return false, err
	}
	return true, nil
}

// printInstallHint writes the one line that would install each missing plugin.
//
// On stderr, beside the diagnostic it belongs to, and printed even when the
// offer was made and declined — somebody who wanted to read about a publisher
// before installing from it still needs the command.
func printInstallHint(w io.Writer, missing []string) {
	fmt.Fprintln(w)
	for _, name := range missing {
		fmt.Fprintf(w, "To install the %s plugin: infrena plugins install %s\n", name, name)
	}
}

// joinNames lists names in a sentence.
func joinNames(names []string) string {
	switch len(names) {
	case 1:
		return names[0]
	case 2:
		return names[0] + " and " + names[1]
	}
	out := ""
	for i, n := range names[:len(names)-1] {
		if i > 0 {
			out += ", "
		}
		out += n
	}
	return out + " and " + names[len(names)-1]
}
