package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/infrata/infrata/internal/compiler"
	"github.com/infrata/infrata/internal/config"
	"github.com/infrata/infrata/internal/state"
)

// disposition is how PLAN.md §6.1's rule resolves for one command invocation:
// an environment is reachable if it is DECLARED or it HAS STATE.
type disposition int

const (
	// declaredEnvironment: compile the configuration as usual.
	declaredEnvironment disposition = iota

	// orphanedEnvironment: no declaration, but state lists resources. The
	// desired configuration is EMPTY and the plan is a teardown.
	//
	// The empty configuration must be SUPPLIED, never obtained by compiling.
	// `resources:` is declared globally rather than per environment, so
	// compiling an undeclared environment yields every resource and would plan
	// a full CREATE against an environment that already holds them — the exact
	// opposite of the intended teardown. Anyone "simplifying" this by letting
	// the compile proceed turns a teardown into a re-creation.
	orphanedEnvironment

	// unknownEnvironment: no declaration and no state. A typo.
	//
	// This is why §6.1 is a disjunction rather than "any name is reachable".
	// Were an unknown name simply treated as empty, `infra plan devv` would
	// plan nothing and exit 0, and the diagnostic is the only thing that tells
	// a user why their apply did nothing at all.
	unknownEnvironment
)

// dispositionOf applies §6.1. declared is every environment the configuration
// names, sorted, for the diagnostic.
//
// A configuration that does not decode returns declaredEnvironment: the caller's
// own compile then reports the real problem. Guessing a disposition from a
// broken file would report "unknown environment" on top of a syntax error and
// send the reader after the wrong thing.
func dispositionOf(files []config.File, environment string, st *state.State) (disposition, []string) {
	decl, ds := config.Decode(files)
	if ds.HasErrors() {
		return declaredEnvironment, nil
	}

	// A project that has not adopted environments AT ALL still plans, with any
	// name: environments.Resolve has said so since M2 and the integration suite
	// depends on it. §6.1's rule only begins to bite once a project declares at
	// least one — before that there is nothing for a name to be a typo against.
	if len(decl.Environments) == 0 {
		return declaredEnvironment, nil
	}

	declared := make([]string, 0, len(decl.Environments))
	for _, e := range decl.Environments {
		if e.Name == environment {
			return declaredEnvironment, nil
		}
		declared = append(declared, e.Name)
	}
	sort.Strings(declared)

	if st != nil && len(st.Addresses()) > 0 {
		return orphanedEnvironment, declared
	}
	return unknownEnvironment, declared
}

// teardownConfig is the empty desired configuration for an orphaned
// environment. Project comes from STATE, because configuration is not being
// consulted — the plan header still has to name the project it is tearing down.
func teardownConfig(st *state.State, environment string) compiler.ResolvedConfig {
	return compiler.ResolvedConfig{Project: st.Project, Environment: environment}
}

// teardownNotice is printed ABOVE a teardown plan.
//
// Without it the output is a plan destroying everything, with every resource
// still visible in the configuration and no stated reason — which reads as the
// tool having decided to delete infrastructure on its own. The reason is not
// derivable from the diff, because what was removed is the ENVIRONMENT, and the
// environment is the one thing a plan does not otherwise show.
func teardownNotice(environment string, declared []string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Environment %q is no longer declared in the configuration.\n", environment)
	fmt.Fprintf(&sb, "Its state is still here, so everything recorded in it is proposed for destruction.\n")
	if len(declared) > 0 {
		fmt.Fprintf(&sb, "Declared environments: %s\n", strings.Join(declared, ", "))
	}
	fmt.Fprintf(&sb, "To keep it, add %q back under `environments:`.\n\n", environment)
	return sb.String()
}

// unknownEnvironmentError refuses a name that is neither declared nor stateful.
func unknownEnvironmentError(environment string, declared []string) error {
	if len(declared) == 0 {
		return fmt.Errorf("unknown environment %q: the configuration declares none, so there is "+
			"nothing to plan or apply.\nAdd one under `environments:`", environment)
	}
	return fmt.Errorf("unknown environment %q, and no state exists under that name.\n"+
		"Declared environments: %s\n"+
		"Check the spelling, or add %q under `environments:`",
		environment, strings.Join(declared, ", "), environment)
}
