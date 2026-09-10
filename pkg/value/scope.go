package value

import (
	"fmt"
	"strconv"
)

// Scope records WHICH PRECEDENCE LEVEL supplied a value. Source records what
// KIND of thing it is. The two are orthogonal and both are needed: a value can
// be Source=variable from either a file or the command line, and a plan that
// cannot tell them apart cannot explain itself.
//
// The constants are the precedence chain from PLAN.md §7 in order, lowest
// first — later scopes win. Stages 3 and 4 build a scope stack in exactly this
// order and a resolved value's Scope is a record of which entry won, so there
// is one implementation and what a plan claims about a value's origin cannot
// drift away from the rule that produced it (spec §7.1).
//
// ScopeUnset MUST remain the zero value. Every Value constructed before M4 —
// and every Value read back from a state file written before M4 — then stays
// valid rather than claiming a precedence level it never had.
//
// WHERE A VARIABLE'S `default:` SITS: ScopeBaseConfig, with Source
// SourceDefault. Ruled 2026-09-10; binding. A default written in infra.yml's
// `variables:` block IS base configuration — it lives in the base
// configuration file, at the level everything in that file sits at — and
// Source already distinguishes it as a default, so no eighth constant is
// needed. Stage 4 stamps it when the default wins. Stage 2 leaves Scope unset
// on VariableDecl.Default, because a declaration is not a resolution and only
// one place may decide which level won.
type Scope uint8

const (
	ScopeUnset              Scope = iota // provenance not recorded
	ScopeProviderDefault                 // provider defaults
	ScopeBaseConfig                      // base configuration
	ScopeModuleDefault                   // module defaults (M5 populates this)
	ScopeEnvironmentInherit              // environment inheritance
	ScopeEnvironmentVar                  // environment variables
	ScopeCLIOverride                     // CLI overrides
)

// String names a scope for display and for diagnostics.
//
// It is a switch with an explicit default rather than a map lookup with a
// fallback, for the reason diag.Severity.String() spells out: an unrecognised
// value must report as unrecognised rather than collapsing into a real level.
// A value silently attributed to the wrong precedence level is worse than one
// attributed to none, because a user would act on it.
//
// ScopeCLIOverride renders as "--var" rather than "CLI override" because that
// is what the user typed, and PLAN.md §44 requires an error to name something
// the user can act on.
func (s Scope) String() string {
	switch s {
	case ScopeUnset:
		return "unset"
	case ScopeProviderDefault:
		return "provider default"
	case ScopeBaseConfig:
		return "base config"
	case ScopeModuleDefault:
		return "module default"
	case ScopeEnvironmentInherit:
		return "environment inheritance"
	case ScopeEnvironmentVar:
		return "environment variable"
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
// Deliberately separate from Scope.String(), exactly as kindWireNames is
// separate from Kind.String(): String() is a diagnostic string and free to
// change, whereas a state file is a versioned contract. Persisting the uint8
// would be worse still — M5 inserts ScopeModuleDefault's population and any
// future level inserted mid-chain would silently reinterpret every state file
// ever written.
//
// ScopeUnset is deliberately ABSENT. It encodes to the empty string and is
// omitted from the wire entirely, so state files written before M4 stay
// byte-identical and read back as ScopeUnset.
var scopeWireNames = map[Scope]string{
	ScopeProviderDefault:    "provider_default",
	ScopeBaseConfig:         "base_config",
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
// empty string is ScopeUnset — that is the pre-M4 state file case and it is
// not an error.
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
