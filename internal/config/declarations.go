// Package config implements compiler stages 1 (load) and 2 (decode).
//
// Stage 2 is the only place in the engine permitted to touch yaml.Node. Every
// later stage works with the typed declarations defined here. Spec §7.
package config

import "infra/pkg/value"

// AttributeDecl is one configured attribute.
//
// In M1, Value holds the literal datum, and any string containing "${" is kept
// verbatim with HasExpressions set. M2's stage 6 replaces this with a parsed
// expression tree.
type AttributeDecl struct {
	Name           string
	Value          value.Value
	HasExpressions bool
	Origin         value.Origin
}

// LifecycleDecl configures how a resource is created and destroyed.
type LifecycleDecl struct {
	PreventDestroy bool
	Retain         bool
}

// ResourceDecl is one declared resource, decoded but not yet resolved.
type ResourceDecl struct {
	Name       string
	Type       string
	Attributes map[string]AttributeDecl
	DependsOn  []string
	Lifecycle  LifecycleDecl
	Origin     value.Origin
}

// ProjectDecl is the decoded, still-unresolved configuration.
type ProjectDecl struct {
	Project   string
	Resources []*ResourceDecl // sorted by Name
	Origin    value.Origin
}
