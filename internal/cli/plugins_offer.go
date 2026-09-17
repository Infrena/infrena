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

// PLAN.md section 31.3's interactive offer, and the one-line answer that
// replaces it when nobody is there.
//
// WHERE THIS RUNS IS WHY IT CANNOT CONTINUE THE COMMAND. The offer hangs off
// the path a command takes when its configuration did not compile, which for a
// missing plugin it never can. So "install, then stop and say so" is not a rule
// this code remembers to follow - by the time anything is installed the command
// has already failed and its only remaining act is to return. Installing
// mid-plan would mean the first half of a run happened under conditions the
// second half never saw; here there is no second half to get wrong.
//
// AND IT IS THE ONLY PLACE ON THESE COMMANDS THAT MAY REACH THE NETWORK. A
// person answering a question is not `plan` reaching out on its own, which is
// the distinction section 31.3 draws; every gate below exists to keep the two
// apart, and each is asserted by a test that counts connections rather than
// output.

// pluginSearchOffVar turns the offer off entirely.
//
// For the machine that has a terminal and still must not reach out: a
// developer on a train, an air-gapped runner that allocates a tty, a shell
// where the question is simply unwelcome. Set to anything non-empty, no search
// is made at all - not a search whose answer is discarded.
const pluginSearchOffVar = "INFRENA_NO_PLUGIN_SEARCH"

// stdinIsTerminal reports whether a person could answer a question.
//
// A CHARACTER DEVICE, MINUS /dev/null, and the second half is not a nicety.
// Telling a tty from any other character device needs an ioctl, and the
// libraries that wrap one are outside the two-package budget - but the
// character device a process is actually handed when nobody is there is
// /dev/null, every time: `go test` does it, and so do cron, systemd and most CI
// runners. Written 2026-09-16 without that exclusion, this package's own suite
// caught it immediately, by making a real request to api.github.com from a
// failing `plan`. Which is the whole rule this gate exists to hold: the network
// is never on the hot path, and a run with nobody at the terminal must not
// reach out.
//
// What is left uncovered is a character device that is neither a terminal nor
// /dev/null, which nothing hands a process by accident.
//
// A VARIABLE because it is the seam this package's tests replace - there is no
// terminal inside `go test` to stand one up against. Everything it must answer
// no to is real in the test that covers it.
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

// configurationIsNotValid is that error, with section 31.3's offer made first.
//
// out is the run's own writer, so --output silences the conversation along with
// everything else on stdout; the hint goes to stderr, which it does not.
func configurationIsNotValid(cmd *cobra.Command, opts *GlobalOptions, out io.Writer, loader *pluginhost.Loader) error {
	// A nil loader is a command that failed BEFORE it built a registry - a
	// variable file that would not parse, an environment that is not declared.
	// Nothing was asked for, so nothing can be missing.
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
				// A FAILED OFFER IS A NOTE, NOT THE COMMAND'S ERROR. The user
				// already has a failure to read; the rate limit that stopped
				// infrena looking a plugin up is context for it rather than a
				// second thing that went wrong.
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

// canOfferToInstall is the three conditions section 31.3 sets, and no fourth.
func canOfferToInstall(cmd *cobra.Command, opts *GlobalOptions) bool {
	// --output means a frontend is reading a file and nothing is watching
	// stdout (spec 2.1), so a question written there would reach nobody. Same
	// test apply uses before asking for approval, for the same reason.
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
// IT SHOWS EVERYTHING AND THEN MAY REFUSE TO ASK. A plugin that exists but
// publishes no build for this machine, and two owners publishing one name, are
// both worth printing and neither is installable - the first because there is
// nothing to install, the second because infrena does not choose between
// sources. renderSearch says both in the table, so the question is simply not
// put.
func offerToInstall(cmd *cobra.Command, opts *GlobalOptions, out io.Writer, name string) (bool, error) {
	sources, untrusted, err := searchSources(opts.Dir, name)
	if err != nil {
		return false, err
	}

	// Said BEFORE the request, because the request is the slow part and a
	// terminal that goes quiet after an error looks like one that has hung.
	fmt.Fprintf(out, "\nThe %s plugin is not installed. Looking for it...\n\n", name)

	fetcher := searchFetcher(false)
	found, problems := plugins.Search(cmd.Context(), fetcher, sources, name, searchEnvironment(opts.Dir, name))
	renderSearchWarnings(cmd.ErrOrStderr(), problems, found)
	renderSearch(out, name, found, untrusted, len(problems) > 0, fetcher.authenticated())

	// A PROVIDER, ALWAYS. This offer is made when a resource type names a
	// plugin that is not on the search path, which is a provider by
	// construction; a missing backend is backendhost's error and a different
	// conversation. Saying so keeps a backend of the same name out of the
	// count, where it would look like a second publisher.
	providers := candidatesWithRole(found, plugins.RoleProvider)
	if _, err := soleCandidate(name, "", providers, len(problems) > 0, fetcher.authenticated()); err != nil {
		// The table above has already said which candidates there are and why
		// each unusable one is unusable, so asking a question install would
		// only refuse would be asking it twice.
		return false, nil
	}

	prompt := fmt.Sprintf("\nInstall %s and stop, so you can re-run your command? Type yes: ", name)
	if confirm(cmd.InOrStdin(), out, prompt, "yes") != approvalGranted {
		fmt.Fprintln(out)
		return false, nil
	}

	// The install itself, which is the same code path `infrena plugins install`
	// takes - trust prompt included. A second way to install a plugin is a
	// second set of rules about what may be installed.
	if err := runInstall(cmd, opts, out, name, false, plugins.RoleProvider.String()); err != nil {
		return false, err
	}
	return true, nil
}

// printInstallHint is the no-terminal half: the existing error, plus the one
// line that fixes it.
//
// ON STDERR, beside the diagnostic it belongs to, and printed even when the
// offer was made and declined - somebody who wanted to read about a publisher
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
