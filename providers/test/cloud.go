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

// FailureRule injects a failure. Nth counts from 1; the rule fires once.
type FailureRule struct {
	Op        string `json:"op"` // create, read, update, delete
	Address   string `json:"address"`
	Nth       int    `json:"nth"`
	Retryable bool   `json:"retryable,omitempty"`
	Message   string `json:"message,omitempty"`

	seen  int
	fired bool
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
	return &c, nil
}

// Save writes the cloud file indented, because a human edits it.
func (c *Cloud) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

// ShouldFail reports whether an injected failure applies to this operation.
func (c *Cloud) ShouldFail(op, addr string) (*FailureRule, bool) {
	for i := range c.Failures {
		rule := &c.Failures[i]
		if rule.fired || rule.Op != op || rule.Address != addr {
			continue
		}
		rule.seen++
		nth := rule.Nth
		if nth <= 0 {
			nth = 1
		}
		if rule.seen == nth {
			rule.fired = true
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
