// Package state holds the record of what infrena manages.
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

// CurrentVersion is the state schema version this build writes. Bumping it
// requires a matching step in migrations.go, because every older file is then
// routed through Decode's migration chain.
const CurrentVersion = 2

// State represents the record of what infrena manages.
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
// State stores values it does not know, and never instructions for finding them
// out. An unknown value is fine: a computed attribute the provider has not
// reported yet is legitimately unknown. An unknown carrying an expression is
// not — nothing downstream will ever evaluate it, so it becomes a dangling
// reference the next load resurrects, naming a resource that may be gone.
//
// The rule lives here rather than in pkg/value because a plan artifact must be
// able to carry an expression; a state file must not.
//
// Nothing a user writes can reach this, because the executor resolves every
// deferred value before recording it, so the message says engine defect.
// Failing the write is the point: a corrupt state file costs far more than a
// failed apply, and the alternative is discovering it on the next load, after
// the run that produced it is over.
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
// Numbers arrive as json.Number, not float64, so that an integer beyond 2^53
// survives the round trip; see Decode. A migration inspecting one calls its
// Int64/Float64/String methods rather than asserting float64. Assigning a plain
// int or float64 is fine — only the values a migration leaves alone need to
// stay exact.
type Migration struct {
	// From and To are the schema versions this step reads and produces. To
	// must be greater than From; Decode refuses a step that would not
	// advance.
	From int
	To   int
	// Apply rewrites the decoded document in place.
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
// That path decodes with UseNumber, so a number keeps its original text rather
// than becoming a float64. Without it, re-marshalling silently rounds every
// integer beyond 2^53: the value comes back one less, a plan proposes an update
// to a number the user never changed, and applying it writes the rounded value
// back as though it were desired.
func Decode(data []byte) (*State, error) {
	var probe struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("state is not valid JSON: %w", err)
	}
	if probe.Version > CurrentVersion {
		return nil, fmt.Errorf("state version %d was written by a newer version of infrena; this build understands up to version %d", probe.Version, CurrentVersion)
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
		// A plain int, deliberately: UseNumber protects numbers that came
		// from the document, and a Go int marshals exactly at any magnitude.
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
// It is a function rather than an inline call because json.Unmarshal has no
// equivalent of UseNumber — that option lives on the Decoder — and an edit
// reaching for the shorter form would silently restore the rounding.
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
