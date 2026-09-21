package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/infrena/infrena/internal/compiler"
	"github.com/infrena/infrena/internal/config"
	"github.com/infrena/infrena/internal/state"
)

// disposition is what one command invocation makes of an environment name. A
// name is reachable if it is declared or it has state.
type disposition int

const (
	// declaredEnvironment: compile the configuration as usual.
	declaredEnvironment disposition = iota

	// orphanedEnvironment: no declaration, but state lists resources, so the
	// desired configuration is empty and the plan is a teardown.
	//
	// That empty configuration must be supplied, never obtained by compiling.
	// `resources:` is declared globally rather than per environment, so
	// compiling an undeclared environment yields every resource and would plan
	// a full create against an environment that already holds them — the
	// opposite of the intended teardown.
	orphanedEnvironment

	// unknownEnvironment: no declaration and no state, so a typo. Treating it
	// as empty instead would have `infrena plan devv` plan nothing and exit 0,
	// and the diagnostic is the only thing that tells a user why their apply
	// did nothing.
	unknownEnvironment
)

// dispositionOf classifies environment. declared is every environment the
// configuration names, sorted, for the diagnostic.
//
// A configuration that does not decode returns declaredEnvironment so the
// caller's own compile reports the real problem; guessing from a broken file
// would report "unknown environment" on top of a syntax error.
func dispositionOf(files []config.File, environment string, st *state.State) (disposition, []string) {
	decl, ds := config.Decode(files)
	if ds.HasErrors() {
		return declaredEnvironment, nil
	}

	// A project that declares no environments at all still plans, with any
	// name: there is nothing for a name to be a typo against until at least
	// one is declared.
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
// environment. Project comes from state, because configuration is not being
// consulted and the plan header still has to name what it is tearing down.
func teardownConfig(st *state.State, environment string) compiler.ResolvedConfig {
	return compiler.ResolvedConfig{Project: st.Project, Environment: environment}
}

// teardownNotice is printed above a teardown plan.
//
// The reason for the destruction is not derivable from the diff: what was
// removed is the environment, and that is the one thing a plan does not
// otherwise show. Without this the output reads as the tool deciding to delete
// infrastructure on its own.
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
