// Package planner decides what must change. It compares resolved
// configuration against recorded state and observed provider reality and
// produces a plan: one operation per resource, with the reasons for it.
//
// Everything in this package is a pure function of its inputs. It touches no
// filesystem, no network and no provider. That purity is what makes
// determinism (invariant 6) a property test rather than an aspiration.
package planner

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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

// UnmarshalText reads a kind back by NAME, the pair MarshalText needs to make the
// artifact readable rather than merely writable — without it the format was
// write-only, which is what reading a saved plan back discovered.
//
// A name it does not know is an ERROR rather than OpNoOp. A kind that fell back to
// "no operation" would turn a create from a newer build into a silent skip: the
// apply would report success and do nothing, which is the worst available outcome
// and the reason this is not a lenient decode.
func (k *OpKind) UnmarshalText(text []byte) error {
	for _, candidate := range []OpKind{OpNoOp, OpCreate, OpUpdate, OpReplace, OpDestroy, OpForget} {
		if candidate.String() == string(text) {
			*k = candidate
			return nil
		}
	}
	return fmt.Errorf("unknown operation kind %q", text)
}

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
	// Provider is the instance this operation runs against (PLAN.md §12.1).
	//
	// Taken from the DESIRED resource where there is one, and from STATE where
	// there is not — a destroy has only state, and dispatching it from
	// configuration would leave a removed resource with no account to delete from.
	Provider string
	Before   map[string]value.Value
	After    map[string]value.Value
	Reasons  []ChangeReason
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
	//
	// THE FINGERPRINT IS OF STATE AS LOADED, before any in-memory change a command
	// makes to it. `infrata plan` hashes what came off disk; `apply` stamps the project
	// name into state before planning (see cli.computePlan), which changes the bytes.
	// So a command comparing a saved plan's hash against state must take its own hash
	// BEFORE mutating anything, or it compares two different states and reports a
	// change nobody made. That cost one debugging round the first time --plan ran.
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

// EVERY resource appears here, `OpNoop` included: the artifact describes the whole
// plan, not only what changes. A consumer counting `operations` is not counting
// changes — Plan.HasChanges() is. Noted after infrata-provider-fake's e2e suite read
// it the other way.
type operationWire struct {
	Address string `json:"address"`
	Type    string `json:"type"`
	// Provider is the instance the operation acts on (PLAN.md §12.1).
	//
	// It is here because the artifact is the format a saved plan is read back FROM,
	// and a destroy read back without its instance would be dispatched to whichever
	// account happened to be consulted. Reading a plan back is not implemented yet
	// (§50); writing the field now is what keeps it possible.
	//
	// Unlike Lifecycle's omitzero, this changes the bytes of essentially every plan
	// artifact: every operation names an instance, the implicit one included, so the
	// key is always written. That does not touch invariant 6 — determinism is "same
	// inputs, equivalent plan", not byte-stability across builds of infra — and
	// version exists so the format can gain a field. A consumer decoding into a
	// struct ignores one it does not know.
	//
	// omitempty only for the case where no instance exists at all, which is a
	// registry serving nothing and a plan with no operations to speak of.
	Provider   string                 `json:"provider,omitempty"`
	Kind       OpKind                 `json:"kind"`
	Before     map[string]value.Value `json:"before,omitempty"`
	After      map[string]value.Value `json:"after,omitempty"`
	Reasons    []ChangeReason         `json:"reasons,omitempty"`
	Dependents []string               `json:"dependents,omitempty"`
	// DependsOn is what the configuration says this resource depends on.
	//
	// Here for the same reason as Provider above, and it is the field whose absence
	// would have made reading a plan back QUIETLY WRONG rather than merely
	// incomplete. The executor copies Operation.DependsOn onto the state it records,
	// and state's Dependencies is the only surviving record of what a resource
	// depended on once it leaves configuration — which is what orders a later
	// destroy (spec §14, and see Operation.DependsOn). An operation decoded without
	// it records no dependencies, so a plan saved and applied would satisfy
	// invariant 4 for itself and silently break it for the NEXT plan's destroys.
	// That is the exact bug Operation.DependsOn's comment describes as fixed, and it
	// would have come back through this door alone.
	//
	// Distinct from Dependents, which is the other direction and is data for the
	// renderer: Dependents says who would break if this went away, DependsOn says
	// what this needs. Both are carried because neither is derivable from the other
	// once the plan has left the process that made it.
	DependsOn []string `json:"depends_on,omitempty"`
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
			Provider:  op.Provider,
			Kind:      op.Kind,
			Before:    op.Before,
			After:     op.After,
			Reasons:   op.Reasons,
			Lifecycle: op.Lifecycle,
		}
		for _, dependent := range dependents {
			entry.Dependents = append(entry.Dependents, dependent.String())
		}

		// Sorted from a copy, like Dependents, so the canonical form is canonical
		// however the plan was assembled.
		dependsOn := make([]address.Address, len(op.DependsOn))
		copy(dependsOn, op.DependsOn)
		address.Sort(dependsOn)
		for _, d := range dependsOn {
			entry.DependsOn = append(entry.DependsOn, d.String())
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

// DecodePlan reads a plan artifact back.
//
// It is deliberately NOT the inverse of encode: it recovers exactly what the executor
// needs to carry out the plan, and nothing that is only there for a human reader.
// Diagnostics are dropped, because a saved plan's warnings were addressed to whoever
// reviewed it, and re-printing them at apply time would present a decision already made
// as one still open.
//
// The version is checked before anything else, the same probe-then-decode shape
// state.Decode and pluginmanifest.Parse use: a plan from a future build carries
// operations this one may not understand, and refusing by version gives a message that
// names the problem instead of one about an unknown field.
func DecodePlan(data []byte) (*Plan, error) {
	var w planWire
	dec := json.NewDecoder(bytes.NewReader(data))
	// Numbers keep their exact text on the way through, so a large integer attribute
	// survives a save-and-apply round trip rather than becoming a float64. The same
	// hazard internal/state hit, and the same fix.
	dec.UseNumber()
	if err := dec.Decode(&w); err != nil {
		return nil, fmt.Errorf("this is not a plan artifact: %w", err)
	}
	if w.Version != PlanVersion {
		return nil, fmt.Errorf("this plan is version %d and this infrata writes version %d\n"+
			"A plan artifact is not portable across format versions. Re-run `infrata plan` to "+
			"produce one this build can apply", w.Version, PlanVersion)
	}

	p := &Plan{
		Version:     w.Version,
		Project:     w.Project,
		Environment: w.Environment,
		ConfigHash:  w.ConfigHash,
		StateSerial: w.StateSerial,
		StateHash:   w.StateHash,
	}
	if w.CreatedAt != nil {
		p.CreatedAt = *w.CreatedAt
	}

	for _, entry := range w.Operations {
		addr, err := address.Parse(entry.Address)
		if err != nil {
			return nil, fmt.Errorf("plan operation has an unreadable address %q: %w", entry.Address, err)
		}
		op := Operation{
			Address:   addr,
			Type:      entry.Type,
			Kind:      entry.Kind,
			Provider:  entry.Provider,
			Before:    entry.Before,
			After:     entry.After,
			Reasons:   entry.Reasons,
			Lifecycle: entry.Lifecycle,
		}
		for _, field := range []struct {
			raw  []string
			into *[]address.Address
		}{
			{entry.Dependents, &op.Dependents},
			{entry.DependsOn, &op.DependsOn},
		} {
			for _, s := range field.raw {
				parsed, err := address.Parse(s)
				if err != nil {
					return nil, fmt.Errorf("plan operation %q names an unreadable address %q: %w",
						entry.Address, s, err)
				}
				*field.into = append(*field.into, parsed)
			}
		}
		p.Operations = append(p.Operations, op)
	}
	return p, nil
}

// StaleError says a saved plan cannot be applied because what it was made from has
// moved. Its four causes are separate because they are four different things for a
// user to do about it.
type StaleError struct {
	// Reason is the human sentence, already complete.
	Reason string
	// Action is what to do, per §44.
	Action string
}

func (e *StaleError) Error() string { return e.Reason + "\n" + e.Action }

// CheckApplicable refuses a saved plan that does not belong to what is in front of it.
//
// REFUSED OUTRIGHT, with no override flag. Decided 2026-09-13: an escape hatch on "the
// state moved under you" is an escape hatch on the single guarantee a saved plan exists
// to provide. A user who wants to apply against moved state wants a NEW plan, and
// saying so is both shorter and true.
//
// Four checks because they are four different mistakes. Collapsing them into "this plan
// is stale" would be accurate and useless: applying dev's plan to production, applying a
// plan after editing configuration, and applying one after a colleague applied theirs
// need completely different sentences.
func (p *Plan) CheckApplicable(project, environment, configHash string, st staleState) error {
	if err := p.CheckIdentity(project, environment); err != nil {
		return err
	}
	switch {
	case configHash != "" && p.ConfigHash != "" && p.ConfigHash != configHash:
		return &StaleError{
			Reason: "the configuration has changed since this plan was made, so the plan no " +
				"longer describes what the project asks for",
			Action: "Re-run `infrata plan " + environment + "` and review the new plan.",
		}
	case p.StateSerial != st.Serial || (p.StateHash != "" && st.Hash != "" && p.StateHash != st.Hash):
		return &StaleError{
			Reason: fmt.Sprintf("the state has changed since this plan was made (serial %d, now %d) "+
				"— something else has applied in the meantime, so this plan's before-values are "+
				"no longer what is out there", p.StateSerial, st.Serial),
			Action: "Re-run `infrata plan " + environment + "` and review the new plan.",
		}
	}
	return nil
}

// A saved plan CAN now carry an expression, and that is what lifted the restriction this
// file used to hold. An UnappliableFromFile method here refused any plan whose operations
// referred to one another, because `value.Value.Expr` was not serialised: such a plan
// decoded to unknowns with no expressions, which are indistinguishable from computed
// attributes, so the executor dropped them and the apply reported success having left
// them unset. pkg/value now serialises the expression (see pkg/value/exprwire.go), the
// executor resolves it exactly as it does on the normal path, and the refusal is gone
// rather than merely relaxed.
//
// The invariant that refusal protected did not disappear with it: state must never
// persist a pending expression, and internal/state.Encode now refuses one outright —
// where the rule is about state, rather than in Value, where it constrained every
// consumer including plans.

// CheckIdentity refuses a plan that belongs to a different project or environment.
//
// SEPARATE FROM THE REST because of WHEN it can be checked. Identity cannot change
// under the caller, so it is answerable immediately, before the plan is even rendered —
// and it should be, since rendering another environment's plan and then refusing it asks
// a reader to study a plan that was never going to run. Staleness is the opposite: it
// must be checked as late as possible, inside the lock, because that is precisely what
// can change between deciding and executing.
func (p *Plan) CheckIdentity(project, environment string) error {
	switch {
	case p.Project != project:
		return &StaleError{
			Reason: fmt.Sprintf("this plan was made for project %q and this is %q", p.Project, project),
			Action: "Apply it where it was made, or re-run `infrata plan` here.",
		}
	case p.Environment != environment:
		return &StaleError{
			Reason: fmt.Sprintf("this plan was made for environment %q, not %q",
				p.Environment, environment),
			Action: "Apply it to " + p.Environment + ", or re-run `infrata plan " + environment + "`.",
		}
	}
	return nil
}

// staleState is the fingerprint of the state a plan is about to be applied against,
// taken as a parameter so that this package keeps touching no filesystem.
type staleState struct {
	Serial uint64
	Hash   string
}

// StateFingerprint pairs a state serial with its hash, for CheckApplicable.
func StateFingerprint(serial uint64, hash string) staleState {
	return staleState{Serial: serial, Hash: hash}
}
