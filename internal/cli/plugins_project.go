package cli

import (
	"fmt"
	"slices"

	"github.com/infrena/infrena/internal/config"
	"github.com/infrena/infrena/internal/plugins"
)

// Reading what a project DECLARES it needs, for the commands that install it.
//
// DECODE, NEVER COMPILE. This is the same rule backendFor follows, and it is
// not an optimisation: a project with a broken resource must still be able to
// install the plugin that would fix it. Compiling would refuse to answer "which
// plugins does this project name" for exactly the project whose owner is trying
// to get to a state where it compiles at all.
//
// A PROJECT NAMES A PLUGIN; IT NEVER TRUSTS A SOURCE. Nothing here widens that
// - it answers a name and a role, and every source question is still settled by
// searchSources and ensureTrusted.

// declaredPlugin is one plugin a project names, and which kind it names it as.
type declaredPlugin struct {
	name string
	role plugins.Role
}

// declaredRoles is which roles this project declares for one name: a provider
// when `providers:` or a resource type asks for it, a backend when `backend:`
// names it, both when both, and nothing when neither or when there is no
// project here to ask.
//
// BOTH IS AN ANSWER, not an ambiguity. A project that names s3 as a provider
// and as a backend needs two binaries, and installing both is doing what was
// asked rather than choosing between them.
func declaredRoles(dir, name string) ([]plugins.Role, error) {
	declared, err := declaredPlugins(dir)
	if err != nil {
		return nil, err
	}
	var roles []plugins.Role
	for _, d := range declared {
		if d.name == name {
			roles = append(roles, d.role)
		}
	}
	return roles, nil
}

// declaredPlugins is every plugin this project names, providers first in name
// order and then the backend, which is the order a bare `plugins install`
// works through.
//
// An empty list and no error is a project that needs no plugins, which is a
// real state: `infrena init` produces one.
func declaredPlugins(dir string) ([]declaredPlugin, error) {
	decl, ok, err := projectDeclaration(dir)
	if err != nil || !ok {
		return nil, err
	}

	var out []declaredPlugin
	for _, name := range decl.NeededPlugins() {
		out = append(out, declaredPlugin{name: name, role: plugins.RoleProvider})
	}
	if b := decl.Backend.Plugin; b != "" {
		out = append(out, declaredPlugin{name: b, role: plugins.RoleBackend})
	}
	return out, nil
}

// projectDeclaration decodes this project's configuration, and reports false
// when there is no project here rather than calling that a failure.
//
// DECODE DIAGNOSTICS ARE IGNORED, the same concession pluginConstraints and
// backendDecl already make, and here it is the point rather than a compromise:
// `providers:` and `backend:` are read off the parts that did decode, so a
// project with one unreadable resource can still say which plugins it needs -
// which is very often the plugin whose absence is why it does not read.
//
// A FILE THAT WILL NOT LOAD AT ALL IS STILL AN ERROR. That is not a broken
// resource, it is a file infrena cannot read, and answering "this project
// declares nothing" would send install off to guess.
func projectDeclaration(dir string) (*config.ProjectDecl, bool, error) {
	if !hasProject(dir) {
		return nil, false, nil
	}
	files, err := config.Load(dir)
	if err != nil {
		return nil, false, fmt.Errorf("this project's configuration could not be read, so infrena cannot tell which plugins it needs: %w.\n\nSuggested action:\n  Fix the file, or name the plugin and its kind: `infrena plugins install <name> --kind provider`.", err)
	}
	decl, _ := config.Decode(files)
	return decl, true, nil
}

// rolesAmong is the distinct roles a search actually answered with, in a fixed
// order so a refusal reads the same way twice.
func rolesAmong(found []plugins.Candidate) []plugins.Role {
	var out []plugins.Role
	for _, role := range []plugins.Role{plugins.RoleProvider, plugins.RoleBackend} {
		if slices.ContainsFunc(found, func(c plugins.Candidate) bool { return c.Role == role }) {
			out = append(out, role)
		}
	}
	return out
}

// parseRole reads the `--kind` flag.
//
// THE FLAG IS `--kind` AND THE TYPE IS Role, deliberately: kind is the word a
// person types, and plugins.Kind already means something else entirely in that
// package - whether a source names an owner or one repository.
func parseRole(kind string) (plugins.Role, error) {
	switch kind {
	case plugins.RoleProvider.String():
		return plugins.RoleProvider, nil
	case plugins.RoleBackend.String():
		return plugins.RoleBackend, nil
	default:
		return 0, fmt.Errorf("--kind %q is not a kind of plugin: the two kinds are `provider`, which serves resources, and `backend`, which stores state.\n\nSuggested action:\n  Pass --kind provider or --kind backend, or leave it off inside a project and let the project say which it needs.", kind)
	}
}
