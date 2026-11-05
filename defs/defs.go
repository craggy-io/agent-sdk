// Package defs embeds the agent, skill and workflow definitions shipped
// with the SDK: one bundle, whose bundle.yaml (metadata.id
// agentdef.ShippedID) lists every agent, skill and workflow. Hosts use it
// as the shipped layer, agentdef.Options.Shipped; user and project layers
// replace its definitions by name.
package defs

import (
	"embed"

	"github.com/craggy-io/foundation-kit/bundle"

	"github.com/craggy-io/agent-sdk/agentdef"
)

//go:embed bundle.yaml agents skills workflows
var embedded embed.FS

// Source is the shipped bundle, for agentdef.Options.Shipped.
func Source() bundle.Source { return bundle.Embedded(agentdef.ShippedID, embedded) }
