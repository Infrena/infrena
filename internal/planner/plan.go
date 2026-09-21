// Package planner decides what must change. It compares resolved
// configuration against recorded state and observed provider reality and
// produces a plan: one operation per resource, with the reasons for it.
//
// Everything in this package is a pure function of its inputs. It touches no
// filesystem, no network and no provider, which is what makes determinism
// testable as a property rather than an aspiration.
package planner

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/resource"
	"github.com/infrena/infrena/pkg/value"
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
	// OpDestroyDeposed removes an object a create_before_destroy replacement
	// set aside and then failed to delete.
	//
	// It is not OpDestroy, and that distinction is why it exists: the address
	// is not going anywhere. The resource is there, healthy and described by
	// configuration; what is removed is a previous incarnation that outlived
	// the run meant to delete it. Reported as a destroy, it would tell a
	// reader their database is about to be deleted.
	//
	// Without it a deposed object sits in state forever: real, billed, and
	// named by nothing anybody runs.
	OpDestroyDeposed
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
	case OpDestroyDeposed:
		return "destroy_deposed"
	default:
		return "unknown"
	}
}

// Symbol returns the marker the renderer prefixes to the operation. NoOp has
// none: an unchanged resource is not marked.
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
	case OpDestroyDeposed:
		// The same marker a destroy gets, because something really is being
		// deleted. The renderer names which object beside it.
		return "-"
	default:
		return ""
	}
}

// MarshalText writes the operation kind as its name. The artifact is a
// versioned contract, and a numeric kind would change meaning the moment
// someone reordered the constants.
func (k OpKind) MarshalText() ([]byte, error) { return []byte(k.String()), nil }

// UnmarshalText reads a kind back by name, the counterpart MarshalText needs
// for the artifact to be readable rather than merely writable.
//
// An unknown name is an error, not OpNoOp. Falling back to "no operation"
// would turn a create written by a newer build into a silent skip, with the
// apply reporting success having done nothing.
func (k *OpKind) UnmarshalText(text []byte) error {
	for _, candidate := range []OpKind{OpNoOp, OpCreate, OpUpdate, OpReplace, OpDestroy, OpForget, OpDestroyDeposed} {
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
	// Attribute names what changed.
	Attribute string `json:"attribute,omitempty"`
	// ForceNew records that this attribute is what promoted an update to a
	// replacement, so the plan can say which one forced it.
	ForceNew bool `json:"force_new,omitempty"`
	// Note carries a short explanation such as "known after apply".
	Note string `json:"note,omitempty"`
}

// Operation is the change proposed for a single resource.
//
// Before is nil for a create; After is nil for a destroy and a forget; both
// are populated for a no-op, an update and a replace. After may contain
// unknown values.
type Operation struct {
	// Address identifies the resource.
	Address address.Address
	// Type is the resource type.
	Type string
	// Kind is the change proposed.
	Kind OpKind
	// Provider is the provider instance this operation runs against.
	//
	// Taken from the desired resource where there is one and from state where
	// there is not: a destroy has only state, and dispatching it from
	// configuration would leave a removed resource with no account to delete
	// from.
	Provider string
	// Before is the resource's attributes as they now stand.
	Before map[string]value.Value
	// After is what they will be.
	After map[string]value.Value
	// Reasons explain the operation, attribute by attribute.
	Reasons []ChangeReason
	// Dependents are the resources that depend on this one, sorted.
	// Destroying a resource with dependents is worth calling out loudly, and
	// the count cannot be recovered from the plan without this.
	Dependents []address.Address
	// Lifecycle is what the configuration declares for this resource, carried
	// as data for the executor to record in state. It is zero for a destroy
	// or a forget, where the resource has left configuration.
	//
	// It is not a decision input. Every lifecycle decision is already encoded
	// in Kind — retain becomes a forget, prevent_destroy becomes a plan-time
	// error and no operation at all — and the planner stays the single
	// enforcement point. Nothing downstream may branch on this field.
	//
	// It travels on the operation rather than being read from configuration
	// at execute time because the plan is the executor's complete instruction
	// set: a saved plan applied later could otherwise pick up a lifecycle
	// that disagrees with the plan the user approved.
	//
	// Recording it is what makes the guards work. A destroy reads lifecycle
	// from state, the resource having left configuration by then, so a
	// lifecycle that never reaches state is a guard that does nothing.
	Lifecycle resource.Lifecycle
	// DependsOn is what the configuration says this resource depends on,
	// sorted, carried for the executor to record in state — the same
	// arrangement as Lifecycle, and for the same reasons. It is zero for a
	// destroy or a forget.
	//
	// It is not a scheduling input; BuildExecution draws create-side edges
	// from Dependents. It is for the next plan: once a resource leaves
	// configuration, state's Dependencies is the only surviving record of
	// what it depended on, and that is what orders a later destroy.
	DependsOn []address.Address
}

// Plan is what `infrena plan` produces and `infrena apply` consumes.
type Plan struct {
	Version     int
	CreatedAt   time.Time
	Project     string
	Environment string
	// ConfigHash fingerprints the resolved configuration this plan was made
	// from, so a saved plan can be told to have gone stale.
	ConfigHash string
	// StateSerial and StateHash fingerprint the state this plan was made
	// against, for the same reason.
	//
	// The fingerprint is of state as loaded, before any in-memory change a
	// command makes to it: apply stamps the project name into state before
	// planning, which changes the bytes. A command comparing a saved plan's
	// hash against state must therefore take its own hash before mutating
	// anything, or it compares two different states and reports a change
	// nobody made.
	StateSerial uint64
	StateHash   string
	// Operations are sorted by canonical address, never by execution order:
	// execution order belongs to the graph and depends on operation kind.
	Operations []Operation
	// Diagnostics are what planning had to say about the configuration.
	Diagnostics []diag.Diagnostic
}

// HasChanges reports whether the plan proposes anything at all.
//
// A forget counts. The provider is never called, but state changes, and the
// plan must exit with the changes status so that CI notices.
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
// CreatedAt is excluded: it records when the plan was produced, which is not
// a property of the plan's inputs, so including it would make determinism
// unachievable. This is what determinism compares and what any plan
// fingerprint is taken over.
func (p *Plan) Canonical() ([]byte, error) { return p.encode(false) }

// MarshalJSON writes the plan artifact a user saves and reads, timestamp
// included. The receiver is a value so that marshalling a Plan and a *Plan
// cannot produce two different formats.
func (p Plan) MarshalJSON() ([]byte, error) { return p.encode(true) }

// planWire is the artifact's on-disk shape. It is written by hand rather than
// derived from the structs so that what is and is not persisted is explicit
// and cannot drift when a struct gains a field.
type planWire struct {
	Version int `json:"version"`
	// Type is never set on a plan artifact. It exists here only so DecodePlan
	// can refuse a report line, the one document that would otherwise decode
	// cleanly: unknown fields are ignored, so a report stream's meta line
	// yields a plan with no operations, which applies nothing, silently.
	//
	// Do not delete this because the version check appears to catch it. The
	// two version numbers are independent and happen to differ today; they
	// may collide again. The version check is also the wrong message, telling
	// the user to re-run plan over a format mismatch that is not the problem.
	Type        string           `json:"type,omitempty"`
	CreatedAt   *time.Time       `json:"created_at,omitempty"`
	Project     string           `json:"project"`
	Environment string           `json:"environment"`
	ConfigHash  string           `json:"config_hash"`
	StateSerial uint64           `json:"state_serial"`
	StateHash   string           `json:"state_hash"`
	Operations  []operationWire  `json:"operations"`
	Diagnostics []diagnosticWire `json:"diagnostics,omitempty"`
}

// operationWire is one operation's on-disk shape.
//
// Every resource appears in the artifact, no-ops included: it describes the
// whole plan, not only what changes. A consumer counting operations is not
// counting changes — Plan.HasChanges is.
type operationWire struct {
	Address string `json:"address"`
	Type    string `json:"type"`
	// Provider is the instance the operation acts on. A destroy read back
	// without it would be dispatched to whichever account happened to be
	// consulted.
	//
	// omitempty covers only the case where no instance exists at all, which
	// is a registry serving nothing.
	Provider   string                 `json:"provider,omitempty"`
	Kind       OpKind                 `json:"kind"`
	Before     map[string]value.Value `json:"before,omitempty"`
	After      map[string]value.Value `json:"after,omitempty"`
	Reasons    []ChangeReason         `json:"reasons,omitempty"`
	Dependents []string               `json:"dependents,omitempty"`
	// DependsOn is what the configuration says this resource depends on.
	//
	// Its absence would make reading a plan back quietly wrong rather than
	// merely incomplete: the executor copies it into state, and state is the
	// only surviving record of what a resource depended on once it leaves
	// configuration, which is what orders a later destroy. An operation
	// decoded without it records no dependencies at all.
	//
	// Distinct from Dependents, the other direction: Dependents says who
	// would break if this went away, DependsOn says what this needs. Neither
	// is derivable from the other once the plan leaves the process that made
	// it.
	DependsOn []string `json:"depends_on,omitempty"`
	// omitzero, not omitempty: a zero struct cannot be omitted any other way,
	// and a plan for a configuration that sets no lifecycle must encode
	// exactly as it did before this field existed.
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
// crosses a map-to-slice boundary sorts, so the output is deterministic.
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
// It is deliberately not the inverse of encode: it recovers what the executor
// needs to carry out the plan and nothing that is only there for a reader.
// Diagnostics are dropped, because a saved plan's warnings were addressed to
// whoever reviewed it, and re-printing them at apply time would present a
// decision already made as one still open.
//
// The version is checked before anything else. A plan from a future build
// carries operations this one may not understand, and refusing by version
// names the problem instead of complaining about an unknown field.
func DecodePlan(data []byte) (*Plan, error) {
	var w planWire
	dec := json.NewDecoder(bytes.NewReader(data))
	// Numbers keep their exact text on the way through, so a large integer
	// attribute survives a save-and-apply round trip rather than becoming a
	// float64.
	dec.UseNumber()
	if err := dec.Decode(&w); err != nil {
		return nil, fmt.Errorf("this is not a plan artifact: %w", err)
	}
	if w.Type != "" {
		return nil, fmt.Errorf(
			"this is a %q line from a report stream, not a plan artifact\n"+
				"Point --plan at the file `infrena plan --output` wrote, not at a line from it",
			w.Type)
	}
	if w.Version != PlanVersion {
		return nil, fmt.Errorf("this plan is version %d and this infrena writes version %d\n"+
			"A plan artifact is not portable across format versions. Re-run `infrena plan` to "+
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

// StaleError says a saved plan cannot be applied because what it was made
// from has moved. Its causes are kept separate because they are different
// things for a user to do about it.
type StaleError struct {
	// Reason is the complete human sentence.
	Reason string
	// Action is what to do about it.
	Action string
}

// Error renders the reason and the suggested action on separate lines.
func (e *StaleError) Error() string { return e.Reason + "\n" + e.Action }

// CheckApplicable refuses a saved plan that does not belong to what is in front of it.
//
// Refused outright, with no override flag: an escape hatch on "the state
// moved under you" is an escape hatch on the single guarantee a saved plan
// exists to provide. A user who wants to apply against moved state wants a
// new plan.
//
// The checks are kept separate because they are different mistakes.
// Collapsing them into "this plan is stale" would be accurate and useless:
// applying one environment's plan to another, applying a plan after editing
// configuration, and applying one after a colleague applied theirs need
// completely different sentences.
func (p *Plan) CheckApplicable(project, environment, configHash string, st staleState) error {
	if err := p.CheckIdentity(project, environment); err != nil {
		return err
	}
	switch {
	case configHash != "" && p.ConfigHash != "" && p.ConfigHash != configHash:
		return &StaleError{
			Reason: "the configuration has changed since this plan was made, so the plan no " +
				"longer describes what the project asks for",
			Action: "Re-run `infrena plan " + environment + "` and review the new plan.",
		}
	case p.StateSerial != st.Serial || (p.StateHash != "" && st.Hash != "" && p.StateHash != st.Hash):
		return &StaleError{
			Reason: fmt.Sprintf("the state has changed since this plan was made (serial %d, now %d) "+
				"— something else has applied in the meantime, so this plan's before-values are "+
				"no longer what is out there", p.StateSerial, st.Serial),
			Action: "Re-run `infrena plan " + environment + "` and review the new plan.",
		}
	}
	return nil
}

// CheckIdentity refuses a plan that belongs to a different project or environment.
//
// Separate from the staleness checks because of when it can be run. Identity
// cannot change under the caller, so it is answerable immediately, before the
// plan is even rendered — and it should be, since rendering another
// environment's plan and then refusing it asks a reader to study a plan that
// was never going to run. Staleness is the opposite: it must be checked as
// late as possible, inside the lock, because that is exactly what can change
// between deciding and executing.
func (p *Plan) CheckIdentity(project, environment string) error {
	switch {
	case p.Project != project:
		return &StaleError{
			Reason: fmt.Sprintf("this plan was made for project %q and this is %q", p.Project, project),
			Action: "Apply it where it was made, or re-run `infrena plan` here.",
		}
	case p.Environment != environment:
		return &StaleError{
			Reason: fmt.Sprintf("this plan was made for environment %q, not %q",
				p.Environment, environment),
			Action: "Apply it to " + p.Environment + ", or re-run `infrena plan " + environment + "`.",
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
