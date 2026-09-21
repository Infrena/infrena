package test

import (
	"github.com/infrena/infrena/pkg/schema"
	"github.com/infrena/infrena/pkg/value"
)

// definitions returns the resource definitions the fake provider supports.
// Their shapes are chosen to exercise the engine between them: ForceNew,
// computed, sensitive and composite attributes, requirements, aliases,
// references, and both declared and open maps.
func definitions() []*schema.ResourceDefinition {
	return []*schema.ResourceDefinition{
		{
			Type:        "fake.network",
			Description: "A fake network. Has no dependencies.",
			Attributes: map[string]schema.Attribute{
				"cidr": {Kind: value.KindString, Required: true, ForceNew: true, Description: "Address range"},
				"id":   {Kind: value.KindString, Computed: true, Description: "Assigned network identifier"},
			},
			Capabilities: schema.Capabilities{Create: true, Read: true, Update: true, Delete: true, Import: true},
			ImportID:     schema.ImportSpec{Description: "the network identifier, e.g. net-1"},
		},
		{
			Type:        "fake.database",
			Description: "A fake database. Requires a network.",
			Attributes: map[string]schema.Attribute{
				"engine": {Kind: value.KindString, Required: true, ForceNew: true, Description: "Database engine"},
				// A default is one datum, not a function: it has to cross the
				// plugin protocol.
				"size":     {Kind: value.KindInt, Description: "Storage in GB", Default: int64(10)},
				"password": {Kind: value.KindString, Sensitive: true, Description: "Administrator password"},
				"network":  {Kind: value.KindString, Description: "Network this database sits in"},
				// A composite attribute is deliberately present: without one,
				// nothing exercises the conversion between a typed Value and
				// the plain JSON the hand-editable cloud file must hold.
				"tags":     {Kind: value.KindMap, Description: "Free-form labels"},
				"endpoint": {Kind: value.KindString, Computed: true, Description: "Connection endpoint"},
			},
			Requirements: []schema.Requirement{{
				Name:        "network",
				Types:       []string{"fake.network"},
				Description: "A database must sit inside a network",
			}},
			Capabilities: schema.Capabilities{Create: true, Read: true, Update: true, Delete: true, Import: true},
			ImportID:     schema.ImportSpec{Description: "the database identifier, e.g. db-1"},
		},
		{
			Type:        "fake.vpc",
			Description: "A fake VPC. Has no dependencies.",
			Attributes: map[string]schema.Attribute{
				"id": {Kind: value.KindString, Computed: true, Description: "Assigned VPC identifier"},
				// A declared map: a path into it is key-checked at compile time.
				"meta": {Kind: value.KindMap, Description: "Known metadata",
					Fields: map[string]schema.Attribute{"name": {Kind: value.KindString}}},
				// An open map, deliberately with no Fields: like AWS tags, it takes
				// any key, so a path into it is checked at apply rather than at
				// compile time.
				"tags": {Kind: value.KindMap, Description: "Free-form labels"},
			},
			Capabilities: schema.Capabilities{Create: true, Read: true, Update: true, Delete: true, Import: true},
			ImportID:     schema.ImportSpec{Description: "the VPC identifier, e.g. vpc-1"},
		},
		{
			Type:        "fake.subnet",
			Description: "A fake subnet. vpc_id refers to a fake.vpc; cidr declares no reference.",
			Attributes: map[string]schema.Attribute{
				"vpc_id": {Kind: value.KindString, Description: "VPC this subnet sits in",
					// An alias, so a test can write the attribute the way a user of a
					// real plugin would: a canonical name plus the spellings people
					// reach for.
					Aliases:    []string{"vpc"},
					References: &schema.Reference{Type: "fake.vpc", Attribute: "id"}},
				"cidr": {Kind: value.KindString, Description: "Address range"},
			},
			Capabilities: schema.Capabilities{Create: true, Read: true, Update: true, Delete: true, Import: true},
			ImportID:     schema.ImportSpec{Description: "the subnet identifier, e.g. subnet-1"},
		},
		{
			Type:        "fake.application",
			Description: "A fake application. Requires a database.",
			Attributes: map[string]schema.Attribute{
				"image":        {Kind: value.KindString, Required: true, Description: "Container image"},
				"replicas":     {Kind: value.KindInt, Description: "Instance count", Default: int64(1)},
				"database_url": {Kind: value.KindString, Description: "Connection string"},
				"url":          {Kind: value.KindString, Computed: true, Description: "Public URL"},
			},
			Requirements: []schema.Requirement{{
				Name:        "database",
				Types:       []string{"fake.database"},
				Description: "An application must have a database",
			}},
			Capabilities: schema.Capabilities{Create: true, Read: true, Update: true, Delete: true, Import: true},
			ImportID:     schema.ImportSpec{Description: "the application identifier, e.g. app-1"},
		},
	}
}
