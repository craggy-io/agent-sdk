// Package defs embeds the built-in agent, skill and workflow
// definitions shipped with the SDK. Catalogue FS as the earliest
// agentdef root; user and project roots shadow these by name.
package defs

import (
	"embed"
	"io/fs"

	"github.com/craggy-io/agent-sdk/agentdef"
)

//go:embed agents skills workflows
var embedded embed.FS

// FS is the built-in catalogue filesystem (agents/, skills/, workflows/).
func FS() fs.FS { return embedded }

// Builtin returns a loader over just the built-in catalogue. Compose
// with agentdef.Roots(defs.FS(), userFS, projectFS) for shadowing.
func Builtin() *agentdef.Loader { return agentdef.Roots(embedded) }
