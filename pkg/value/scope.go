package value

import (
	"fmt"
	"strconv"
)

// Scope records which precedence level supplied a value; Source records what
// kind of thing it is. The two are orthogonal and both are needed: a value can
// be SourceVariable from either a file or the command line, and a plan that
// cannot tell them apart cannot explain itself.
//
// The constants are the precedence chain in order, lowest first — later scopes
// win. Resolution builds its scope stack in exactly this order and a resolved
// value's Scope records which entry won, so what a plan claims about a value's
// origin cannot drift away from the rule that produced it.
//
// ScopeUnset must remain the zero value, so a Value constructed before scopes
// existed — and one read back from a state file written then — stays valid
// rather than claiming a precedence level it never had.
//
// Two placements are settled and worth stating, because neither is obvious:
//
//   - A provider instance's `defaults:` is ScopeInstanceDefault, one rung above
//     the plugin's schema default and below everything else. Explicit on the
//     resource beats the instance's `defaults:` beats whatever the plugin ships.
//   - A variable's `default:` is ScopeBaseConfig with SourceDefault. It lives in
//     the base configuration file, at the level everything in that file sits at,
//     and Source already distinguishes it as a default, so no extra constant is
//     needed. It is stamped where the default wins; a declaration is not a
//     resolution, so VariableDecl.Default leaves Scope unset.
type Scope uint8

const (
	// ScopeUnset means no precedence level was recorded.
	ScopeUnset Scope = iota
	// ScopeProviderDefault is a plugin's own schema default.
	ScopeProviderDefault
	// ScopeInstanceDefault is a provider instance's `defaults:` block.
	ScopeInstanceDefault
	// ScopeBaseConfig is the base configuration file.
	ScopeBaseConfig
	// ScopeScopedVars is a vars file under a resource directory.
	ScopeScopedVars
	// ScopeModuleDefault is a default declared by a module.
	ScopeModuleDefault
	// ScopeEnvironmentInherit is a value an environment inherited.
	ScopeEnvironmentInherit
	// ScopeEnvironmentVar is the selected environment's own configuration.
	ScopeEnvironmentVar
	// ScopeCLIOverride is a value supplied on the command line.
	ScopeCLIOverride
)

// String names a scope for display and for diagnostics.
//
// It is a switch with an explicit default rather than a map lookup with a
// fallback, so an unrecognised value reports as unrecognised rather than
// collapsing into a real level. A value silently attributed to the wrong
// precedence level is worse than one attributed to none, because a user would
// act on it.
//
// ScopeCLIOverride renders as "--var" because that is what the user typed, and
// an error should name something the user can act on. ScopeEnvironmentVar
// renders as "environment config" rather than "environment variable", which
// almost every reader takes to mean an OS environment variable rather than the
// selected environment's own configuration.
func (s Scope) String() string {
	switch s {
	case ScopeUnset:
		return "unset"
	case ScopeProviderDefault:
		return "provider default"
	case ScopeInstanceDefault:
		return "provider instance default"
	case ScopeBaseConfig:
		return "base config"
	case ScopeScopedVars:
		return "directory vars"
	case ScopeModuleDefault:
		return "module default"
	case ScopeEnvironmentInherit:
		return "environment inheritance"
	case ScopeEnvironmentVar:
		return "environment config"
	case ScopeCLIOverride:
		return "--var"
	default:
		return "Scope(" + strconv.Itoa(int(s)) + ")"
	}
}

// WithScope returns a copy of the value recorded as supplied by scope s.
func (v Value) WithScope(s Scope) Value {
	v.Scope = s
	return v
}

// scopeWireNames is the frozen on-disk spelling of every Scope.
//
// It is separate from Scope.String() because these are three different
// contracts with three different consumers:
//
//   - Kind.String() is the configuration language — a user writes it as `type:`
//     in infrena.yml. Renaming one breaks every configuration file that used it.
//   - Scope.String() is user-visible plan output, so it is freer, but renaming
//     it still changes what a user reads in a plan.
//   - scopeWireNames, like kindWireNames, is the on-disk contract, versioned,
//     with its own migration path independent of both.
//
// Persisting the uint8 would be worse still: Scope is an iota, so a level
// inserted mid-chain would silently reinterpret every state file ever written.
// Nothing compares two Scopes by ordering — they are only ever compared for
// equality or looked up here — so inserting one is safe in memory too.
//
// ScopeUnset is deliberately absent. It encodes to the empty string and is
// omitted from the wire entirely, so a state file written before scopes existed
// stays byte-identical and reads back as ScopeUnset.
var scopeWireNames = map[Scope]string{
	ScopeProviderDefault:    "provider_default",
	ScopeInstanceDefault:    "instance_default",
	ScopeBaseConfig:         "base_config",
	ScopeScopedVars:         "scoped_vars",
	ScopeModuleDefault:      "module_default",
	ScopeEnvironmentInherit: "environment_inherit",
	ScopeEnvironmentVar:     "environment_var",
	ScopeCLIOverride:        "cli_override",
}

// scopeToWireName returns the persisted name for a Scope. ScopeUnset maps to
// the empty string, which json omits. An unrecognised scope is an error rather
// than a fallback: refusing to write it turns an unrepresentable value into a
// failed save rather than a state file that reads back wrong.
func scopeToWireName(s Scope) (string, error) {
	if s == ScopeUnset {
		return "", nil
	}
	name, ok := scopeWireNames[s]
	if !ok {
		return "", fmt.Errorf("cannot encode value of scope %s", s)
	}
	return name, nil
}

// scopeFromWireName resolves a persisted scope name back to its Scope. The
// empty string is ScopeUnset, which is how a state file written before scopes
// existed reads back, and is not an error.
func scopeFromWireName(s string) (Scope, error) {
	if s == "" {
		return ScopeUnset, nil
	}
	for scope, name := range scopeWireNames {
		if name == s {
			return scope, nil
		}
	}
	return ScopeUnset, fmt.Errorf("unknown value scope %q", s)
}
