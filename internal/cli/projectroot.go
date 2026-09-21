package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/infrena/infrena/internal/config"
)

// ProjectDirName is the conventional directory a project lives in when the
// infrastructure sits beside the application rather than in a repository of its
// own. It is a convention, not a requirement: a repository that is nothing but
// infrastructure puts infrena.yml at its root and is found just the same.
const ProjectDirName = "infrena"

// findProjectRoot resolves where a command actually runs: dir itself when it
// holds infrena.yml, else dir/infrena when that does.
//
// It deliberately does not walk up. Walking up means a command run in a
// subdirectory silently operates on a project the user may not have realised
// they were in, and state mutation is the wrong place for that convenience.
//
// An explicit --chdir never reaches here; a user who named a directory has
// already answered this question.
func findProjectRoot(dir string) (string, error) {
	for _, candidate := range []string{dir, filepath.Join(dir, ProjectDirName)} {
		// Every stat error is treated as "not here" rather than returned: the
		// message below names both paths tried, which explains more than a raw
		// errno about one of them would.
		if info, err := os.Stat(filepath.Join(candidate, config.ProjectFileName)); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf(
		"no project found in %s\n\n"+
			"Looked for %s and %s/%s, in that order, and did not search any parent directory.\n"+
			"Run `infrena init` to create a project here, or pass --chdir to name the directory that holds one.",
		dir, config.ProjectFileName, ProjectDirName, config.ProjectFileName)
}

// projectOptionalCommands run with a project when there is one and without a
// project when there is not: each answers a question that never mentioned a
// project. They still get the search, because a project's `plugins:` block is
// worth honouring when one happens to be there.
//
// `init` is not in this map. It is exempt from the search entirely, below,
// because resolving into ./infrena first would make it scaffold into
// infrena/infrena.
var projectOptionalCommands = map[string]bool{
	"version":    true,
	"explain":    true,
	"discover":   true,
	"help":       true,
	"completion": true,
}

// isProjectOptional reports whether a command answers a question that never
// mentioned a project.
//
// `plugins list` is matched on its parent rather than its own name, because
// `state list` shares the leaf name and is emphatically about a project.
func isProjectOptional(cmd *cobra.Command) bool {
	if projectOptionalCommands[cmd.Name()] {
		return true
	}
	// `plan --show` reads a saved plan and nothing else. The artifact is
	// complete by construction, and the point of it is a reviewer reading the
	// plan on a machine that has never seen the project.
	//
	// Keyed on the flag rather than the command name, because `plan` without
	// it does need a project.
	if cmd.Name() == "plan" && cmd.Flags().Changed("show") {
		return true
	}
	return cmd.Parent() != nil && cmd.Parent().Name() == "plugins"
}

// resolveProjectRoot points opts.Dir at the project, so that infrastructure can
// live in ./infrena beside an application without every command needing
// --chdir. It is called where --chdir is already handled, so a command added
// later inherits it.
func resolveProjectRoot(cmd *cobra.Command, opts *GlobalOptions) error {
	// An explicit --chdir answers the question, so the search must not run.
	// Checked on Changed rather than on the value, because "." is also the
	// default and a user who typed it meant it.
	if cmd.Flags().Changed("chdir") {
		return nil
	}
	// init runs where no project exists yet, which is the one command whose
	// purpose is the absence this function reports.
	if cmd.Name() == "init" {
		return nil
	}

	root, err := findProjectRoot(opts.Dir)
	if err != nil {
		if isProjectOptional(cmd) {
			return nil
		}
		return err
	}
	opts.Dir = root
	return nil
}
