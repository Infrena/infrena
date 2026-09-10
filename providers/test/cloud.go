// Package test implements a fake provider whose world lives in a
// hand-editable JSON file, so drift can be induced by a person or a test with
// equal ease. PLAN.md §33 and §48; spec §8.4.
package test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"infra/pkg/provider"
)

// DefaultCloudPath is where the fake cloud lives inside a project.
const DefaultCloudPath = ".infra/fake-cloud.json"

// CloudResource is one object in the fake cloud. Attributes are plain JSON
// because this models an external system, not internal configuration.
type CloudResource struct {
	Type       string         `json:"type"`
	Address    string         `json:"address,omitempty"`
	Attributes map[string]any `json:"attributes"`
}

// Retryability is how a failure rule names one of the three
// provider.Retryability classifications in the cloud file.
//
// A boolean cannot express the middle category, and spec §15 gives all three
// materially different executor behaviour: Create is never retried on an
// ambiguous failure, Delete is retried only when the provider says it is safe.
// Spec §18 requires a test per category, and this provider is the only thing
// that will ever produce those errors.
type Retryability string

const (
	// RetryNotSafe means retrying could duplicate or corrupt the resource.
	RetryNotSafe Retryability = "not_safe"
	// RetryConditional means the outcome is ambiguous: the operation may or
	// may not have taken effect.
	RetryConditional Retryability = "conditional"
	// RetrySafe means the operation provably did not take effect.
	RetrySafe Retryability = "safe"
)

// Classify maps a rule's classification onto the provider constant. An absent
// value is the conservative NotSafeToRetry.
func (r Retryability) Classify() provider.Retryability {
	switch r {
	case RetrySafe:
		return provider.SafeToRetry
	case RetryConditional:
		return provider.ConditionallyRetryable
	default:
		return provider.NotSafeToRetry
	}
}

// valid reports whether r is a recognised classification. The empty string is
// valid and means "absent".
func (r Retryability) valid() bool {
	switch r {
	case "", RetryNotSafe, RetryConditional, RetrySafe:
		return true
	default:
		return false
	}
}

// FailureRule injects a failure. Nth counts from 1; the rule fires once.
type FailureRule struct {
	Op      string `json:"op"` // create, read, update, delete
	Address string `json:"address"`
	Nth     int    `json:"nth"`
	// Retryability is absent by default, meaning not safe to retry.
	Retryability Retryability `json:"retryability,omitempty"`
	Message      string       `json:"message,omitempty"`

	// Seen and Fired are bookkeeping and must be exported and persisted: the
	// provider reloads the cloud file on every operation, so an unexported
	// (and therefore JSON-dropped) counter would reset to zero on every load.
	// An Nth: 1 rule would then fire on every attempt instead of once, and an
	// Nth greater than 1 could never be reached at all.
	Seen  int  `json:"seen,omitempty"`
	Fired bool `json:"fired,omitempty"`
}

// Cloud represents the state of the fake infrastructure.
type Cloud struct {
	Resources map[string]*CloudResource `json:"resources"`
	Failures  []FailureRule             `json:"failures,omitempty"`
	LatencyMS int                       `json:"latency_ms,omitempty"`
	NextID    int                       `json:"next_id,omitempty"`
}

// LoadCloud reads the cloud file. A missing file is an empty cloud, not an
// error: a project that has never applied anything has no infrastructure.
func LoadCloud(path string) (*Cloud, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &Cloud{Resources: map[string]*CloudResource{}}, nil
	}
	if err != nil {
		return nil, err
	}

	var c Cloud
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.Resources == nil {
		c.Resources = map[string]*CloudResource{}
	}
	for i, rule := range c.Failures {
		// Rejected rather than defaulted: a typo that silently classifies as
		// not-safe is a rule whose retry behaviour is never what the person
		// editing this file intended, and nothing would ever say so.
		if !rule.Retryability.valid() {
			return nil, fmt.Errorf("%s: failure rule %d: unknown retryability %q; use %q, %q or %q", path, i, string(rule.Retryability), RetryNotSafe, RetryConditional, RetrySafe)
		}
	}
	return &c, nil
}

// Save writes the cloud file indented, because a human edits it.
//
// The write is atomic — temporary file in the same directory, then a rename —
// mirroring internal/state/local.go's Put. A plain os.WriteFile truncates in
// place, and the provider rewrites this file on every operation including Read,
// so a concurrent reader would see a half-written file and report "unexpected
// end of JSON input", which looks like a provider failure rather than a harness
// bug. Spec §10's refresh reads every resource concurrently.
func (c *Cloud) Save(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("%s: %w", dir, err)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, ".fake-cloud-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename has succeeded

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// ShouldFail reports whether an injected failure applies to this operation.
func (c *Cloud) ShouldFail(op, addr string) (*FailureRule, bool) {
	for i := range c.Failures {
		rule := &c.Failures[i]
		if rule.Fired || rule.Op != op || rule.Address != addr {
			continue
		}
		rule.Seen++
		nth := rule.Nth
		if nth <= 0 {
			nth = 1
		}
		if rule.Seen == nth {
			rule.Fired = true
			return rule, true
		}
	}
	return nil, false
}

// Delay returns the latency duration for this cloud.
func (c *Cloud) Delay() time.Duration {
	return time.Duration(c.LatencyMS) * time.Millisecond
}

// AllocateID returns a stable, increasing provider ID.
func (c *Cloud) AllocateID(prefix string) string {
	c.NextID++
	return fmt.Sprintf("%s-%d", prefix, c.NextID)
}
