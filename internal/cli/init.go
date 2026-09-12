package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/spf13/cobra"

	"github.com/infrata/infrata/internal/config"
)

// newInitCommand builds `infrata init` (spec §37, layout §4).
//
// What it writes must pass `infra validate` immediately. An init whose output
// does not validate is worse than no init at all: it teaches the language
// wrongly at the one moment a user has no way to tell.
func newInitCommand(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:           "init",
		SilenceUsage:  true,
		SilenceErrors: true,
		Short:         "Create a new project in the current directory",
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			created, err := scaffold(opts.Dir)
			if err != nil {
				return err
			}
			for _, f := range created {
				fmt.Fprintf(cmd.OutOrStdout(), "created %s\n", f)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "\nNext: infrata validate, then infrata plan dev\n")
			return nil
		},
	}
}

// scaffoldFiles is the project §4 describes, minimal rather than illustrative:
// enough to be a working project, not a tutorial. §27's spirit — generated
// configuration omits what a default already provides — applies to what init
// writes as much as to what import generates.
func scaffoldFiles() map[string]string {
	return map[string]string{
		config.ProjectFileName: `project: my-project

# Environments are first-class: each has its own state and its own lock. Add as
# many as you like — they hold the same infrastructure and differ only in what
# their variables say.
environments:

  dev: {}

  production: {}

resources:

  network:
    type: test.network
    cidr: 10.0.0.0/16
`,
		"variables.yml": `# Values shared by every environment. An environment may override any of
# them, and --var overrides both. See PLAN.md §7 for the full order.
{}
`,
		"modules/.gitkeep": "",
	}
}

// scaffold writes the project, refusing to overwrite.
//
// Refusing matters more here than anywhere else in the CLI: init is the command
// someone runs when they are least sure what they are doing, and clobbering an
// existing infra.yml would destroy the one file they cannot regenerate. It is
// checked BEFORE anything is written, so a refusal leaves the directory exactly
// as it was rather than half-scaffolded.
func scaffold(dir string) ([]string, error) {
	files := scaffoldFiles()

	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return nil, fmt.Errorf(
				"%s already exists: this directory is already a project\n\n"+
					"init will not overwrite it. Remove it first if you meant to start over.", name)
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
