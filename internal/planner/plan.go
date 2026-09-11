// Package planner decides what must change. It compares resolved
// configuration against recorded state and observed provider reality and
// produces a plan: one operation per resource, with the reasons for it.
//
// Everything in this package is a pure function of its inputs. It touches no
// filesystem, no network and no provider. That purity is what makes
// determinism (invariant 6) a property test rather than an aspiration.
package planner

import (
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/infrata/infrata/internal/diag"
	"github.com/infrata/infrata/pkg/address"
	"github.com/infrata/infrata/pkg/resource"
	"github.com/infrata/infrata/pkg/value"
)

// PlanVersion is the schema version of the plan artifact this build writes.
const PlanVersion = 1

// OpKind is the operation a plan proposes for one resource.
type OpKind uint8

const (
	// OpNoOp means the resource already matches configuration.
	OpNoOp OpKind = iota
	// OpCreate means the resource will be created.
	OpCreate
	// OpUpdate means the resource will be changed in place.
	OpUpdate
	// OpReplace means a ForceNew attribute changed, so the resource must be
	// destroyed and recreated.
	OpReplace
	// OpDestroy means the resource will be deleted through its provider.
	OpDestroy
	// OpForget means the resource will be dropped from state without the
	// provider being called.
	OpForget
)

// String returns the operation's name, as written in the plan artifact.
func (k OpKind) String() string {
	switch k {
	case OpNoOp:
		return "noop"
	case OpCreate:
		return "create"
	case OpUpdate:
		return "update"
	case OpReplace:
		return "replace"
	case OpDestroy:
		return "destroy"
	case OpForget:
		return "forget"
	default:
		return "unknown"
	}
}

// Symbol returns the marker the renderer prefixes to the operation, per spec
// §12.3. NoOp has none: an unchanged resource is not marked.
func (k OpKind) Symbol() string {
	switch k {
	case OpCreate:
		return "+"
	case OpUpdate:
		return "~"
	case OpReplace:
		return "-/+"
	case OpDestroy:
		return "-"
	case OpForget:
		return "="
	default:
		return ""
	}
}

// MarshalText writes the operation kind as its name. The artifact is a
// versioned contract, and a numeric kind would change meaning the moment
// someone reordered the constants.
func (k OpKind) MarshalText() ([]byte, error) { return []byte(k.String()), nil }

// ChangeReason explains one attribute's contribution to an operation.
//
// It names attributes and types, never values: sensitivity is per-leaf, so
// even a composite that is not itself marked sensitive may contain a leaf that
// is, and a reason has no way to redact.
type ChangeReason struct {
	Attribute string `json:"attribute,omitempty"`
	// ForceNew records that this attribute is what promoted an update to a
	// replacement, so the plan can say which one forced it.
	ForceNew bool `json:"force_new,omitempty"`
	// Note carries a short explanation such as "known after apply".
	Note string `json:"note,omitempty"`
}

// Operation is the change proposed for a single resource.
//
// Before is nil for Create; After is nil for Destroy and Forget; both are
// populated for NoOp, Update and Replace. After may contain unknown values.
// Spec §12.1.
type Operation struct {
	Address address.Address
	Type    string
	Kind    OpKind
	Before  map[string]value.Value
	After   map[string]value.Value
	Reasons []ChangeReason
	// Dependents are the resources that depend on this one, sorted. Destroying
	// a resource with dependents is the case spec §20 wants called out loudly,
	// and the count is not recoverable from the plan without it.
	Dependents []address.Address
	// Lifecycle is the lifecycle the CONFIGURATION declares for this resource,
	// carried here purely as data for the executor to record in state. It is
	// zero for OpDestroy and OpForget, where the resource has by definition
	// left configuration and there is nothing left to record.
	//
	// It is emphatically not a decision input. Every lifecycle DECISION is
	// already encoded in Kind by removalOperation — retain becomes OpForget,
	// prevent_destroy becomes a plan-time error that produces no operation at
	// all — and that stays the single enforcement point. Nothing downstream of
	// the planner may branch on this field; internal/executor copies it onto
	// the ResourceState it records and never reads it otherwise.
	//
	// It has to travel on the operation rather than being looked up from
	// configuration at execute time because the plan is the executor's
	// complete instruction set: M6 saves a plan and applies it later, and a
	// lifecycle read from configuration at that point could disagree with the
	// plan the user approved.
	//
	// Recording it is what makes the guards work at all. A destroy reads
	// lifecycle from STATE — correctly, since the resource is gone from
	// configuration by then — so a lifecycle that never reaches state is a
	// guard that silently does nothing.
	Lifecycle resource.Lifecycle
	// DependsOn is what the CONFIGURATION says this resource depends on,
	// sorted, carried here for the executor to record in state — the same
	// arrangement, and for the same reasons, as Lifecycle above. It is zero
	// for OpDestroy and OpForget, where the resource has left configuration
	// and there is nothing current to record.
	//
	// It is not a scheduling input: BuildExecution draws its create-side
	// edges from Operation.Dependents, and this field is never read there.
	// What it is for is the NEXT plan. Once a resource leaves configuration,
	// state's Dependencies is the only surviving record of what it depended
	// on (spec §14), and dependentsOf reads exactly that to order destroys.
	// Nothing wrote it: every ResourceState in every state file carried an
	// empty Dependencies, so a destroy of resources already removed from
	// configuration had no ordering edges at all — invariant 4 held for
	// everything still configured and silently did not for the one case
	// where state is the only source.
	DependsOn []address.Address
}

// Plan is what `infra plan` produces and `infra apply` consumes.
type Plan struct {
	Version     int
	CreatedAt   time.Time
	Project     string
	Environment string
	// ConfigHash fingerprints the resolved configuration this plan was made
	// from, so M6 can tell a saved plan has gone stale.
	ConfigHash string
	// StateSerial and StateHash fingerprint the state this plan was made
	// against, for the same reason.
	StateSerial uint64
	StateHash   string
	// Operations are sorted by canonical address, never by execution order:
	// execution order belongs to the graph and depends on operation kind.
	Operations  []Operation
	Diagnostics []diag.Diagnostic
}

// HasChanges reports whether the plan proposes anything at all.
//
// A Forget counts. The provider is never called, but state changes, and
// `infra plan` must exit with ExitChanges so that CI notices.
func (p *Plan) HasChanges() bool {
	if p == nil {
		return false
	}
	for _, op := range p.Operations {
		if op.Kind != OpNoOp {
			return true
		}
	}
	return false
}

// Counts returns how many operations there are of each kind. A kind that does
// not occur is absent from the map, which reads as zero.
func (p *Plan) Counts() map[OpKind]int {
	out := map[OpKind]int{}
	if p == nil {
		return out
	}
	for _, op := range p.Operations {
		out[op.Kind]++
	}
	return out
}

// Canonical returns the plan's deterministic form: identical inputs produce
// byte-identical output.
//
// CreatedAt is excluded. It records when the plan was produced, which is not a
// property of the plan's inputs, so including it would make invariant 6
// unsatisfiable — spec §12.1. This is what determinism compares and what any
// plan fingerprint is taken over.
func (p *Plan) Canonical() ([]byte, error) { return p.encode(false) }

// MarshalJSON writes the plan artifact a user saves and reads, timestamp
// included. The receiver is a value so that marshalling a Plan and a *Plan
// cannot produce two different formats.
func (p Plan) MarshalJSON() ([]byte, error) { return p.encode(true) }

// planWire is the artifact's on-disk shape. It is written by hand rather than
// derived from the structs so that what is and is not persisted is explicit
// and cannot drift when a struct gains a field.
type planWire struct {
	Version     int              `json:"version"`
	CreatedAt   *time.Time       `json:"created_at,omitempty"`
	Project     string           `json:"project"`
	Environment string           `json:"environment"`
	ConfigHash  string           `json:"config_hash"`
	StateSerial uint64           `json:"state_serial"`
	StateHash   string           `json:"state_hash"`
	Operations  []operationWire  `json:"operations"`
	Diagnostics []diagnosticWire `json:"diagnostics,omitempty"`
}

type operationWire struct {
	Address    string                 `json:"address"`
	Type       string                 `json:"type"`
	Kind       OpKind                 `json:"kind"`
	Before     map[string]value.Value `json:"before,omitempty"`
	After      map[string]value.Value `json:"after,omitempty"`
	Reasons    []ChangeReason         `json:"reasons,omitempty"`
	Dependents []string               `json:"dependents,omitempty"`
	// omitzero, not omitempty: encoding/json cannot omit a zero struct any
	// other way, and a plan for a configuration that sets no lifecycle at all
	// must encode byte-for-byte as it did before this field existed
	// (invariant 6).
	Lifecycle resource.Lifecycle `json:"lifecycle,omitzero"`
}

type diagnosticWire struct {
	Severity string   `json:"severity"`
	Summary  string   `json:"summary"`
	Detail   string   `json:"detail,omitempty"`
	Action   string   `json:"action,omitempty"`
	Origin   string   `json:"origin,omitempty"`
	Related  []string `json:"related,omitempty"`
}

// encode renders the plan, with or without its timestamp. Everything that
// crosses a map-to-slice boundary sorts: determinism is invariant 6.
func (p *Plan) encode(withTimestamp bool) ([]byte, error) {
	if p == nil {
		return nil, errors.New("cannot encode a nil plan")
	}

	// Sort a copy. The canonical form must be canonical however the plan was
	// assembled, and encoding must never reorder the caller's plan.
	ops := make([]Operation, len(p.Operations))
	copy(ops, p.Operations)
	sort.SliceStable(ops, func(i, j int) bool {
		return ops[i].Address.String() < ops[j].Address.String()
	})

	w := planWire{
		Version:     p.Version,
		Project:     p.Project,
		Environment: p.Environment,
		ConfigHash:  p.ConfigHash,
		StateSerial: p.StateSerial,
		StateHash:   p.StateHash,
		Operations:  make([]operationWire, 0, len(ops)),
	}
	if withTimestamp {
		created := p.CreatedAt
		w.CreatedAt = &created
	}

	for _, op := range ops {
		// Sort a copy of the dependents too, for the same reason: the
		// canonical form must be canonical however the plan was assembled.
		dependents := make([]address.Address, len(op.Dependents))
		copy(dependents, op.Dependents)
		address.Sort(dependents)

		entry := operationWire{
			Address:   op.Address.String(),
			Type:      op.Type,
			Kind:      op.Kind,
			Before:    op.Before,
			After:     op.After,
			Reasons:   op.Reasons,
			Lifecycle: op.Lifecycle,
		}
		for _, dependent := range dependents {
			entry.Dependents = append(entry.Dependents, dependent.String())
		}
		w.Operations = append(w.Operations, entry)
	}

	for _, d := range p.Diagnostics {
		entry := diagnosticWire{
			Severity: d.Severity.String(),
			Summary:  d.Summary,
			Detail:   d.Detail,
			Action:   d.Action,
		}
		if d.Origin.File != "" {
			entry.Origin = d.Origin.String()
		}
		// Sort a copy, for the same reason Dependents is sorted from a copy
		// above: Related has no ordering contract of its own, and encoding
		// must never reorder the caller's plan.
		related := make([]address.Address, len(d.Related))
		copy(related, d.Related)
		address.Sort(related)
		for _, r := range related {
			entry.Related = append(entry.Related, r.String())
		}
		w.Diagnostics = append(w.Diagnostics, entry)
	}

	return json.Marshal(w)
}
