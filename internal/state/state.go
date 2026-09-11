// Package state holds the record of what infra manages. Spec §9.
package state

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/infrata/infrata/pkg/address"
	"github.com/infrata/infrata/pkg/resource"
)

// CurrentVersion is the state schema version this build writes.
const CurrentVersion = 1

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
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// Migration transforms a decoded state document from one version to the next.
// It operates on the generic document rather than the typed struct, because the
// typed struct only ever describes CurrentVersion.
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
// migration path, which is the only thing that needs it: JSON numbers become
// float64 in that intermediate, so routing every load through it silently
// rounds any integer beyond 2^53 — in the one file whose whole job is fidelity.
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

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("state is not valid JSON: %w", err)
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
		raw["version"] = float64(version)
	}

	normalised, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	return decodeCurrent(normalised)
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
