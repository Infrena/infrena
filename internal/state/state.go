// Package state holds the record of what infra manages. Spec §9.
package state

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/resource"
	"github.com/infrena/infrena/pkg/value"
)

// CurrentVersion is the state schema version this build writes.
//
// 2 since 2026-09-13: the fake provider became a separately distributed plugin named
// `fake`, so the types in state moved from `test.*` to `fake.*` (PLAN.md §21.1, §31.2).
// internal/state/migrations.go carries the step.
//
// This is the FIRST bump, and it was deferred twice for a reason that no longer holds:
// Decode routed every older file through map[string]any and rounded any integer past
// 2^53, so bumping cost silent precision loss on every existing file. UseNumber fixed
// that, which is what made this an ordinary change.
const CurrentVersion = 2

// State represents the record of what infra manages.
type State struct {
	Version     int                                `json:"version"`
	Serial      uint64                             `json:"serial"`
	Project     string                             `json:"project"`
	Environment string                             `json:"environment"`
	Resources   map[string]*resource.ResourceState `json:"resources"`
	UpdatedAt   time.Time                          `json:"updated_at"`
}

// New creates a new State for a project and environment.
func New(project, environment string) *State {
	return &State{
		Version:     CurrentVersion,
		Project:     project,
		Environment: environment,
		Resources:   map[string]*resource.ResourceState{},
	}
}

// Get retrieves a resource by address, returning it and a boolean indicating presence.
func (s *State) Get(addr address.Address) (*resource.ResourceState, bool) {
	r, ok := s.Resources[addr.String()]
	return r, ok
}

// Set stores a resource in state by its address.
func (s *State) Set(r *resource.ResourceState) {
	if s.Resources == nil {
		s.Resources = map[string]*resource.ResourceState{}
	}
	s.Resources[r.Address.String()] = r
}

// Remove deletes a resource from state by its address.
func (s *State) Remove(addr address.Address) {
	delete(s.Resources, addr.String())
}

// Addresses returns every managed address in sorted order.
func (s *State) Addresses() []address.Address {
	out := make([]address.Address, 0, len(s.Resources))
	for _, r := range s.Resources {
		out = append(out, r.Address)
	}
	address.Sort(out)
	return out
}

// Encode serialises state. Output is byte-stable for identical input: keys are
// sorted by encoding/json for maps, and indentation is fixed, so a state file
// only changes when the state actually changed.
func (s *State) Encode() ([]byte, error) {
	if err := s.checkNothingPending(); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// checkNothingPending refuses to write a state file carrying a pending expression.
//
// AN UNKNOWN VALUE IS FINE and deliberately not refused: a computed attribute a provider
// has not reported is legitimately unknown in state, which the golden fixture pins. An
// unknown carrying an EXPRESSION is different — that is a promise to evaluate something
// later, and nothing downstream ever will. Written into state it becomes a dangling
// reference the next load resurrects, naming a resource that may be gone by then.
//
// The first version of this check refused every unknown and the golden test caught it
// immediately, which is the distinction worth recording: state stores values it does not
// know, and never instructions for finding them out.
//
// This used to be guaranteed by pkg/value refusing to serialise an expression at all.
// That made it impossible for a PLAN ARTIFACT to carry one either, and a plan's whole
// job is to record work not yet done — so the expression now travels and the rule lives
// here, where it is about state rather than about every consumer of a Value.
//
// An engine defect rather than a user error, and worded as one: nothing a user writes
// can reach this, because the executor resolves every deferred value before recording it
// (internal/executor.resolveAfter). Failing the WRITE is the point — a corrupt state file
// is far more expensive than a failed apply, and the alternative is discovering it on the
// next load, when the run that produced it is over.
func (s *State) checkNothingPending() error {
	for _, addr := range s.Addresses() {
		r, ok := s.Get(addr)
		if !ok {
			continue
		}
		for _, name := range sortedNames(r.Attributes) {
			v := r.Attributes[name]
			if v.Expr != nil {
				return fmt.Errorf("refusing to write state: %s attribute %q still carries the "+
					"expression %s, which means it was recorded before it was resolved. "+
					"This is an engine defect; please report it",
					addr, name, v.Expr.String())
			}
		}
	}
	return nil
}

// sortedNames lists a map's keys in order, so a refusal names the same attribute on
// every run of the same failure.
func sortedNames(m map[string]value.Value) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Migration transforms a decoded state document from one version to the next.
// It operates on the generic document rather than the typed struct, because the
// typed struct only ever describes CurrentVersion.
//
// NUMBERS ARRIVE AS json.Number, not float64. The document is decoded with
// json.Decoder.UseNumber() so that an integer beyond float64's exact range —
// 2^53 and up — survives the round trip through this representation; see Decode.
// A migration inspecting one calls its Int64/Float64/String methods rather than
// asserting float64. Assigning a plain int or float64 is fine: only the values
// a migration LEAVES ALONE need to be exact, and those are the ones already
// carrying json.Number.
type Migration struct {
	From  int
	To    int
	Apply func(raw map[string]any) error
}

var migrations []Migration

// RegisterMigration adds a migration to the chain.
func RegisterMigration(m Migration) { migrations = append(migrations, m) }

// Decode reads a state document, applying migrations until it is current.
//
// A document already at CurrentVersion is unmarshalled straight into the typed
// struct. The generic map[string]any representation is reserved for the
// migration path, which is the only thing that needs it.
//
// That path decodes with UseNumber, so a number becomes a json.Number — its
// original text — rather than a float64. Without it, re-marshalling the document
// rounds every integer beyond 2^53, in the one file whose whole job is fidelity,
// and does it silently: the value comes back one less, a plan proposes an update
// to a number the user never changed, and applying it writes the rounded value
// back as though it were desired.
//
// This was a recorded defect for two milestones. M7 and M11 each declined to bump
// CurrentVersion because doing so would route every existing file through here;
// with UseNumber that cost is gone, and a version bump is now an ordinary change.
func Decode(data []byte) (*State, error) {
	var probe struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("state is not valid JSON: %w", err)
	}
	if probe.Version > CurrentVersion {
		return nil, fmt.Errorf("state version %d was written by a newer version of infra; this build understands up to version %d", probe.Version, CurrentVersion)
	}
	if probe.Version == CurrentVersion {
		return decodeCurrent(data)
	}

	raw, err := decodeGeneric(data)
	if err != nil {
		return nil, err
	}
	version := probe.Version

	for version < CurrentVersion {
		m, ok := migrationFrom(version)
		if !ok {
			return nil, fmt.Errorf("no migration registered from state version %d to %d", version, version+1)
		}
		// A migration that does not advance the version would loop forever.
		// Refuse it: a hang while loading state is far worse than an error,
		// because it gives the user nothing to act on.
		if m.To <= version {
			return nil, fmt.Errorf("migration from state version %d declares To=%d, which does not advance the version", m.From, m.To)
		}
		if err := m.Apply(raw); err != nil {
			return nil, fmt.Errorf("migrating state from version %d to %d: %w", m.From, m.To, err)
		}
		version = m.To
		// A plain int, deliberately. UseNumber is about numbers that came FROM the
		// document, whose text must survive untouched; a Go int marshals exactly at
		// any magnitude, so there is nothing here to preserve. Measured: writing it
		// as a json.Number instead changes no behaviour, so the simpler form wins.
		raw["version"] = version
	}

	normalised, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	return decodeCurrent(normalised)
}

// decodeGeneric decodes a state document into the generic representation
// migrations operate on, preserving every number's exact text.
//
// json.Unmarshal has no equivalent: the option lives on the Decoder. Which is why
// this is a function rather than one line inline — the two-step dance is the whole
// point, and a later edit reaching for json.Unmarshal because it is shorter would
// silently restore the rounding.
func decodeGeneric(data []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var raw map[string]any
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("state is not valid JSON: %w", err)
	}
	return raw, nil
}

// decodeCurrent unmarshals a document already at CurrentVersion into the typed
// struct.
func decodeCurrent(data []byte) (*State, error) {
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	if s.Resources == nil {
		s.Resources = map[string]*resource.ResourceState{}
	}
	s.Version = CurrentVersion
	return &s, nil
}

func migrationFrom(version int) (Migration, bool) {
	candidates := make([]Migration, 0, len(migrations))
	for _, m := range migrations {
		if m.From == version {
			candidates = append(candidates, m)
		}
	}
	if len(candidates) == 0 {
		return Migration{}, false
	}
	// Stable, so two migrations registered with identical From and To resolve
	// in registration order rather than arbitrarily.
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].To < candidates[j].To })
	return candidates[0], true
}
