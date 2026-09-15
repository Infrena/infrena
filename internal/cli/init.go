package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/infrena/infrena/internal/config"
)

// newInitCommand builds `infrena init [dir]` (spec §37, layout §4.2).
//
// What it writes must pass `infrena validate` immediately. An init whose output
// does not validate is worse than no init at all: it teaches the language
// wrongly at the one moment a user has no way to tell.
//
// The directory defaults to ./infrena rather than the current one, so that
// infrastructure lives beside the application in the same repository. `infrena
// init .` scaffolds in place for a repository that is nothing but
// infrastructure.
func newInitCommand(opts *GlobalOptions) *cobra.Command {
	var provider string

	cmd := &cobra.Command{
		Use:           "init [dir]",
		SilenceUsage:  true,
		SilenceErrors: true,
		Short:         "Create a new project, by default in ./infrena",
		Args:          cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := ProjectDirName
			if len(args) == 1 {
				target = args[0]
			}

			created, err := scaffold(filepath.Join(opts.Dir, target), provider)
			if err != nil {
				return err
			}
			for _, f := range created {
				fmt.Fprintf(cmd.OutOrStdout(), "created %s\n", displayPath(target, f))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "\n%s\n", nextSteps(target))
			return nil
		},
	}

	cmd.Flags().StringVar(&provider, "provider", "",
		"name a provider plugin, and scaffold a live example resource for it instead of a commented-out one")

	return cmd
}

// displayPath names a created file the way the user would type it, relative to
// where they are rather than to the directory init happened to choose.
func displayPath(target, name string) string {
	return filepath.ToSlash(filepath.Join(target, name))
}

// nextSteps names the commands to run after init.
//
// It carries --chdir when the scaffolded directory is not one findProjectRoot
// discovers, which is anything but ./infrena or the current directory. Without
// that, the closing message is advice that does not work from where the user is
// now standing — one level above a project nothing will find.
func nextSteps(target string) string {
	flag := ""
	if clean := filepath.ToSlash(filepath.Clean(target)); clean != ProjectDirName && clean != "." {
		flag = " --chdir " + target
	}
	return "Next: infrena validate" + flag + ", then infrena plan staging" + flag
}

// scaffoldFiles is the project §4.2 describes: infra.yml, a resources
// directory, one vars file per environment plus the shared default, an empty
// modules directory and a .gitignore.
//
// Minimal rather than illustrative — enough to be a working project, not a
// tutorial. §27's spirit, that generated configuration omits what a default
// already provides, applies to what init writes as much as to what import
// generates.
func scaffoldFiles(provider string) map[string]string {
	return map[string]string{
		config.ProjectFileName: projectFile(provider),
		"resources/network.yml": `# Resource declarations live here, one file per area, globbed recursively.
# A ` + "`resources:`" + ` block in this directory means exactly what the same block in
# infra.yml means: the directory is how a project is ORGANISED, not a second
# way to say something.
` + exampleResource(provider),
		"vars/default.yml": `# Values every environment starts from. A file named after an environment
# beside this one overrides any of them, and --var overrides both. PLAN.md §7
# has the full precedence order.
#
# cidr: 10.0.0.0/16
`,
		"vars/production.yml": `# Values for production only. The filename names the environment, so it has
# to match one infra.yml declares. Everything not set here comes from
# default.yml: this file is a set of DIFFERENCES, not a replacement.
`,
		"vars/staging.yml": `# Values for staging only. See production.yml.
`,
		"modules/.gitkeep": "",
		".gitignore": `# State, locks and any plugin downloaded into this project. Local state is a
# working file rather than a record to share: it is rewritten by every apply,
# and two people committing it conflict on every line.
` + StateDirName + `/
`,
	}
}

// projectFile is infra.yml: the project name, the version floor, the providers
// this project uses, and its environments.
//
// The environments are production and staging, matching vars/production.yml and
// vars/staging.yml. They have to match: an environment the vars directory does
// not name, or a vars file no environment declares, is dead configuration in
// the one file a user reads to learn the language.
func projectFile(provider string) string {
	var b strings.Builder
	b.WriteString(`project: my-project

# The oldest infrena that understands this project. Optional, and worth
# keeping: without it, an older binary reports unknown keys one at a time
# instead of saying it is too old.
infrena: ">= 0.7"
`)

	if provider != "" {
		// A LIST, not a mapping: a mapping cannot hold two instances of one
		// plugin, and would lose one silently (§12.1).
		fmt.Fprintf(&b, `
# The providers this project uses. Each entry names a plugin; `+"`name`"+` is
# optional and defaults to the plugin's own name, and the plugin's
# configuration goes beside it.
providers:
  - plugin: %s
`, provider)
	}

	b.WriteString(`
# Environments are first-class: each has its own state and its own lock. They
# hold the same infrastructure and differ only in what their variables say, so
# their values live in vars/production.yml and vars/staging.yml. Removing one
# from this file proposes tearing it down, so plan that environment first to
# see exactly what it would destroy.
environments:

  production: {}

  staging: {}
`)
	return b.String()
}

// exampleResource is the body of resources/network.yml.
//
// The example is COMMENTED OUT by default, and that is a change from the
// scaffold that shipped before.
//
// A shipped infrena carries no provider. The old scaffold declared a live
// `fake.network`, so a fresh init only validated on a machine that happened to
// have the fake plugin installed — and what init writes must pass `infrena
// validate` immediately, or it teaches the language wrongly at the one moment a
// user has no way to tell.
//
// Commented out it validates bare and still shows the shape. With --provider
// the resource is live, because naming a provider is the user saying they have
// one.
func exampleResource(provider string) string {
	if provider == "" {
		return `#
# The example below is commented out on purpose. A shipped infrena carries no
# provider, so a live resource here would make a brand new project fail
# ` + "`infrena validate`" + ` on any machine that has not installed one yet. Install a
# provider, then uncomment this or run ` + "`infrena init --provider <name>`" + `.
#
# resources:
#   network:
#     type: fake.network
#     cidr: 10.0.0.0/16
`
	}
	return fmt.Sprintf(`
resources:

  network:
    type: %s
    cidr: 10.0.0.0/16
`, exampleType(provider))
}

// exampleType names the example resource's type for a provider.
//
// aws is spelled out because a network there is a VPC, and an example naming a
// type the plugin does not serve is an example that does not validate. Every
// other provider falls back to <provider>.network, which is the fake provider's
// own spelling and the likeliest one elsewhere; a user who has installed a
// provider is also in a position to correct it.
func exampleType(provider string) string {
	if provider == "aws" {
		return "aws.vpc"
	}
	return provider + ".network"
}

// scaffold writes the project into dir, refusing to overwrite.
//
// Refusing matters more here than anywhere else in the CLI: init is the command
// someone runs when they are least sure what they are doing, and clobbering an
// existing infra.yml would destroy the one file they cannot regenerate. It is
// checked BEFORE anything is written, so a refusal leaves the directory exactly
// as it was rather than half-scaffolded.
func scaffold(dir, provider string) ([]string, error) {
	files := scaffoldFiles(provider)

	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return nil, fmt.Errorf(
				"%s already exists: this directory is already a project\n\n"+
					"init will not overwrite it. Remove it first if you meant to start over, "+
					"or name a different directory.", displayPath(dir, name))
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}

	var created []string
	for _, name := range names {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(p, []byte(files[name]), 0o644); err != nil {
			return nil, err
		}
		created = append(created, name)
	}
	return created, nil
}
