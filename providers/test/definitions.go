package test

import (
	"github.com/infrata/infrata/pkg/schema"
	"github.com/infrata/infrata/pkg/value"
)

// definitions returns the resource definitions the fake provider supports.
// Their shapes are chosen to exercise the engine: test.network has no
// dependencies, test.database has a ForceNew attribute plus a computed one
// plus a sensitive one plus a composite one and requires a network, and
// test.application requires a database.
func definitions() []*schema.ResourceDefinition {
	return []*schema.ResourceDefinition{
		{
			Type:        "test.network",
			Description: "A fake network. Has no dependencies.",
			Attributes: map[string]schema.Attribute{
				"cidr": {Kind: value.KindString, Required: true, ForceNew: true, Description: "Address range"},
				"id":   {Kind: value.KindString, Computed: true, Description: "Assigned network identifier"},
			},
			Capabilities: schema.Capabilities{Create: true, Read: true, Update: true, Delete: true, Import: true},
			ImportID:     schema.ImportSpec{Description: "the network identifier, e.g. net-1"},
		},
		{
			Type:        "test.database",
			Description: "A fake database. Requires a network.",
			Attributes: map[string]schema.Attribute{
				"engine": {Kind: value.KindString, Required: true, ForceNew: true, Description: "Database engine"},
				// ONE value, not a function of the environment. PLAN.md §13 is
				// withdrawn: anything that should differ between environments is
				// a variable, which a reader can see in the configuration.
				"size": {Kind: value.KindInt, Description: "Storage in GB",
					Default: func(schema.DefaultContext) (any, bool) { return int64(10), true }},
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
				Types:       []string{"test.network"},
				Description: "A database must sit inside a network",
			}},
			Capabilities: schema.Capabilities{Create: true, Read: true, Update: true, Delete: true, Import: true},
			ImportID:     schema.ImportSpec{Description: "the database identifier, e.g. db-1"},
		},
		{
			Type:        "test.application",
			Description: "A fake application. Requires a database.",
			Attributes: map[string]schema.Attribute{
				"image":        {Kind: value.KindString, Required: true, Description: "Container image"},
				"replicas":     {Kind: value.KindInt, Description: "Instance count", Default: func(schema.DefaultContext) (any, bool) { return int64(1), true }},
				"database_url": {Kind: value.KindString, Description: "Connection string"},
				"url":          {Kind: value.KindString, Computed: true, Description: "Public URL"},
			},
			Requirements: []schema.Requirement{{
				Name:        "database",
				Types:       []string{"test.database"},
				Description: "An application must have a database",
			}},
			Capabilities: schema.Capabilities{Create: true, Read: true, Update: true, Delete: true, Import: true},
			ImportID:     schema.ImportSpec{Description: "the application identifier, e.g. app-1"},
		},
	}
}
