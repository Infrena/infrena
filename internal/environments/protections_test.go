package environments

import (
	"testing"

	"github.com/infrena/infrena/internal/config"
)

// TestProtectionsResolveDownTheChain: a child inherits by saying nothing and
// drops by saying false, and the diagnostic can name where it came from.
func TestProtectionsResolveDownTheChain(t *testing.T) {
	decls := []config.EnvironmentDecl{
		{Name: "base", RequireApproval: true, RequireApprovalSet: true, PreventDestroy: true, PreventDestroySet: true},
		{Name: "inherits", Extends: "base"},
		{Name: "opts-out", Extends: "base", PreventDestroy: false, PreventDestroySet: true},
	}

	for _, tc := range []struct {
		env                       string
		approval, destroy         bool
		approvalFrom, destroyFrom string
	}{
		{"base", true, true, "base", "base"},
		{"inherits", true, true, "base", "base"},
		{"opts-out", true, false, "base", "opts-out"},
	} {
		chain, ds := Resolve(decls, tc.env)
		if ds.HasErrors() {
			t.Fatalf("%s: %+v", tc.env, ds)
		}
		if chain.RequireApproval != tc.approval || chain.PreventDestroy != tc.destroy {
			t.Errorf("%s: approval=%v destroy=%v, want %v/%v",
				tc.env, chain.RequireApproval, chain.PreventDestroy, tc.approval, tc.destroy)
		}
		if chain.RequireApprovalFrom != tc.approvalFrom || chain.PreventDestroyFrom != tc.destroyFrom {
			t.Errorf("%s: declared on %q/%q, want %q/%q — a diagnostic naming the wrong file sends the user to the wrong place",
				tc.env, chain.RequireApprovalFrom, chain.PreventDestroyFrom, tc.approvalFrom, tc.destroyFrom)
		}
	}
}
