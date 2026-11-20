package agentdef

import (
	"fmt"
	"slices"
	"strings"

	"github.com/craggy-io/foundation-kit/bundle"
	"go.starlark.net/starlark"
)

// Role is a part a chat workflow's agents play (workflow(roles=[...])).
// The workflow calls it by name; each turn may cast other installed agents
// in it (host.WithCast), and the ones it names here play it otherwise.
type Role struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Agent plays the role unless a turn casts another: an agent of any
	// bundle, usually the workflow's own. "" leaves it to the turn.
	Agent string `json:"agent,omitempty"`
	// Seats is how many agents play the role unless a turn casts a
	// different number; SeatsAll means every installed agent but the
	// workflow's own.
	Seats int `json:"seats"`
}

// SeatsAll is the Seats of a role every installed agent plays, but the
// workflow's own, unless a turn casts others.
const SeatsAll = -1

// ChatRoles are the roles of a chat workflow whose bundle is owner: the
// ones it declares, or, when it declares none, one for each agent of its
// own bundle, played by that agent.
func (w *Workflow) ChatRoles(cat *bundle.Catalog, owner string) []Role {
	if len(w.Roles) > 0 || !w.Chat {
		return slices.Clone(w.Roles)
	}
	var out []Role
	for _, def := range bundle.All(cat, AgentKind) {
		if def.Owner.ID == owner {
			out = append(out, Role{Name: def.Value.Name, Description: def.Value.Description, Agent: def.Value.Name, Seats: 1})
		}
	}
	slices.SortFunc(out, func(a, b Role) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// roleValue is what role(...) returns, read back by ParseRoles.
type roleValue struct{ role Role }

func (r *roleValue) String() string        { return fmt.Sprintf("role(%q)", r.role.Name) }
func (r *roleValue) Type() string          { return "role" }
func (r *roleValue) Freeze()               {}
func (r *roleValue) Truth() starlark.Bool  { return starlark.True }
func (r *roleValue) Hash() (uint32, error) { return 0, fmt.Errorf("unhashable: role") }

// RoleBuiltin is role(name, description, agent, seats), which declares a
// role of a chat workflow: seats is a number of agents, or "all".
func RoleBuiltin() *starlark.Builtin {
	return starlark.NewBuiltin("role", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		r := Role{Seats: 1}
		var seats starlark.Value
		if err := starlark.UnpackArgs("role", args, kwargs, "name", &r.Name, "description?", &r.Description, "agent?", &r.Agent, "seats?", &seats); err != nil {
			return nil, err
		}
		switch v := seats.(type) {
		case nil:
		case starlark.String:
			if v != "all" {
				return nil, fmt.Errorf("role %q: seats is a number or \"all\", not %q", r.Name, string(v))
			}
			r.Seats = SeatsAll
		case starlark.Int:
			n, ok := v.Int64()
			if !ok || n < 1 {
				return nil, fmt.Errorf("role %q: seats must be at least 1", r.Name)
			}
			r.Seats = int(n)
		default:
			return nil, fmt.Errorf("role %q: seats is a number or \"all\", not %s", r.Name, seats.Type())
		}
		if r.Name == "" {
			return nil, fmt.Errorf("role: a role needs a name")
		}
		return &roleValue{role: r}, nil
	})
}

// ParseRoles reads workflow(roles=...): a list of role(...), each named
// once, and only for a chat workflow.
func ParseRoles(v starlark.Value, chat bool) ([]Role, error) {
	if v == nil || v == starlark.None {
		return nil, nil
	}
	list, ok := v.(*starlark.List)
	if !ok {
		return nil, fmt.Errorf("workflow: roles is a list of role(...), not %s", v.Type())
	}
	if !chat {
		return nil, fmt.Errorf("workflow: roles need chat = True")
	}
	var out []Role
	for i := range list.Len() {
		r, ok := list.Index(i).(*roleValue)
		if !ok {
			return nil, fmt.Errorf("workflow: roles[%d] is not a role(...)", i)
		}
		if slices.ContainsFunc(out, func(o Role) bool { return o.Name == r.role.Name }) {
			return nil, fmt.Errorf("workflow: two roles are named %q", r.role.Name)
		}
		out = append(out, r.role)
	}
	return out, nil
}
