package host

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/craggy-io/agent-sdk/model"
	"github.com/craggy-io/agent-sdk/trace"
)

// Needs is what an agent about to start needs from whoever runs it: the
// tools its definition names by exact name that the host's view doesn't
// hold, and the tools it was given. A host's Options.Needs is asked
// before the node starts, and the node waits for its answer: that is
// where a host pauses an agent to have a person install what it needs or
// make the place it works in.
type Needs struct {
	RunID  string `json:"run_id"`
	NodeID string `json:"node_id"`
	// Agent is the definition's name, and Description what it says it does.
	Agent       string `json:"agent"`
	Description string `json:"description,omitempty"`
	// Missing is every tool the definition includes by exact name (not a
	// glob) that the host's view doesn't hold, in the definition's order.
	Missing []string `json:"missing,omitempty"`
	// Tools is the names of the tools the agent was given.
	Tools []string `json:"tools"`
}

// NeedsAnswer is a host's answer to Needs.
type NeedsAnswer struct {
	// Again asks for the agent's tools to be worked out again: something
	// was installed while it waited.
	Again bool
	// Note is added to the agent's instructions, for what the host knows
	// and the agent should: why a tool will fail here, say.
	Note string
}

// NeedsFunc is a host's Options.Needs. It runs on the starting node's
// context, so it may block until a person answers; it returns when ctx
// ends.
type NeedsFunc func(ctx context.Context, n Needs) NeedsAnswer

// missingTools is every tool f includes by exact name that view does not
// hold and f does not exclude, in f's order.
func missingTools(view ToolView, f model.FilterSpec) []string {
	if len(f.Include) == 0 {
		return nil
	}
	held := map[string]bool{}
	for _, t := range view.Tools() {
		held[t.Name] = true
	}
	var out []string
	for _, name := range f.Include {
		if strings.ContainsAny(name, `*?[\`) || held[name] || model.MatchAny(f.Exclude, name) || slices.Contains(out, name) {
			continue
		}
		out = append(out, name)
	}
	return out
}

// missingNote is what an agent is told of the tools its definition names
// that it was not given, so it neither plans around them nor claims what
// they would have found.
func missingNote(missing []string) string {
	if len(missing) == 0 {
		return ""
	}
	return fmt.Sprintf("## Tools you don't have\n\nYour definition names tools that are not installed here, and the person did not install them: %s. "+
		"Don't plan around them and don't call them. Do what you can with the tools you have, and say plainly in your answer "+
		"what you could not do without them, so the person can install them and ask again.", strings.Join(missing, ", "))
}

// prepareNode asks the host what the node needs before it starts, waits
// for the answer, and works the node's view out again when something was
// installed meanwhile. It returns the view to run with and what to add to
// the agent's instructions: the host's note, and what is still missing.
func (s *Host) prepareNode(ctx context.Context, n Needs, filter model.FilterSpec, narrowed func() ToolView, view ToolView) (ToolView, string) {
	missing := n.Missing
	var note string
	if s.needs != nil {
		_ = s.emit(ctx, trace.Event{Type: "node.needs", RunID: n.RunID, NodeID: n.NodeID,
			Data: mustJSON(map[string]any{"def": n.Agent, "missing": n.Missing})})
		answer := s.needs(ctx, n)
		note = answer.Note
		if answer.Again {
			view = narrowed()
			missing = missingTools(s.view, filter)
		}
	}
	if len(missing) > 0 {
		_ = s.emit(ctx, trace.Event{Type: "node.tools_missing", RunID: n.RunID, NodeID: n.NodeID,
			Data: mustJSON(map[string]any{"def": n.Agent, "missing": missing})})
	}
	return view, strings.Join(nonEmptyNotes(note, missingNote(missing)), "\n\n")
}

func nonEmptyNotes(notes ...string) []string {
	var out []string
	for _, n := range notes {
		if strings.TrimSpace(n) != "" {
			out = append(out, n)
		}
	}
	return out
}
