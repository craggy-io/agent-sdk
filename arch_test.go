package agentsdk_test

import (
	"testing"

	"github.com/craggy-io/foundation-kit/archtest"
)

// below returns a rule that keeps the from packages from importing any of the
// layers above them ("Dependencies point one way", AGENTS.md "The import
// rule").
func below(name string, from []string, above ...string) archtest.Rule {
	return archtest.Rule{
		Name:   name,
		Why:    "dependencies point one way (AGENTS.md, The import rule): move the code down a layer or pass it in",
		From:   from,
		Forbid: above,
	}
}

// TestArchitecture enforces "The import rule" in AGENTS.md (restated in
// docs/design.md §10) for the root module. archtest reads direct imports of
// non-test files only, and sees neither the module graph nor the nested
// modules (cmd/craggy-agent, adapters/*, examples/host), for which AGENTS.md
// states no import rules.
//
// Config.Packages is left unset: the rule places model, transcript, config,
// trace, loop, host, agentdef, workflow and agentcap, but not memory,
// openrouter, defs, agenttest or internal/jschema.
func TestArchitecture(t *testing.T) {
	archtest.Check(t, archtest.Config{
		Rules: []archtest.Rule{
			{
				// "model, transcript, config and trace import nothing else
				// from this module (beyond model)". trace's subpackages
				// (memory, query, sinktest) are read as part of trace.
				Name:   "bottom layer imports only model",
				Why:    "model, transcript, config and trace import nothing else from this module beyond model (AGENTS.md, The import rule)",
				From:   []string{"./model", "./transcript", "./config", "./trace/..."},
				Forbid: []string{"./..."},
				Allow:  []string{"./model", "./trace/..."},
			},
			// "loop builds on model, transcript and trace. host builds on all
			// of them plus agentdef; workflow and agentcap build on host."
			// "Builds on" is read as an ordering, not an exhaustive allowlist:
			// loop also imports config and internal/jschema today. The rule
			// doesn't order loop and agentdef, so neither is kept from the
			// other here.
			below("loop sits below host",
				[]string{"./loop"}, "./host", "./workflow", "./agentcap"),
			below("agentdef sits below host",
				[]string{"./agentdef"}, "./host", "./workflow", "./agentcap"),
			below("host sits below workflow and agentcap",
				[]string{"./host"}, "./workflow", "./agentcap"),
			{
				Name:   "capability-services only in host and agentcap",
				Why:    "domain packages never import capability-services; only host (tool view over a registry view) and agentcap (the bridge) do (AGENTS.md, The import rule)",
				From:   []string{"./..."},
				Except: []string{"./host", "./agentcap"},
				Forbid: []string{"github.com/craggy-io/capability-services/..."},
			},
			{
				// "This module never imports a consumer." Consumers aren't
				// listed anywhere, so this approximates the rule: the only
				// craggy-io modules allowed are the two the README names as
				// dependencies. A consumer reached through go.mod alone, or
				// from a _test.go file, isn't seen here.
				Name:   "never import a consumer",
				Why:    "this module never imports a consumer (AGENTS.md, The import rule); its craggy-io dependencies are script-services and capability-services",
				From:   []string{"./..."},
				Forbid: []string{"github.com/craggy-io/..."},
				Allow: []string{
					"./...",
					"github.com/craggy-io/script-services/...",
					"github.com/craggy-io/capability-services/...",
				},
			},
		},
	})
}
