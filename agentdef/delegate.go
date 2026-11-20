package agentdef

import (
	"encoding/json"

	"go.starlark.net/syntax"
)

// Delegate is the agent a chat workflow hands each message to, when that
// is all it does: its run is only `return agent("name", task =
// input.message)`, it takes nothing but the message, and any role it
// declares is that agent's one seat. A chat running such a workflow is a
// one-to-one chat with that agent, so a host can offer the one in place
// of the other.
func (w *Workflow) Delegate() (string, bool) {
	if !w.Chat || w.Unit == nil || !onlyMessage(w.InputSchema) {
		return "", false
	}
	name, ok := delegateOf(w.Source())
	if !ok {
		return "", false
	}
	for _, role := range w.Roles {
		if len(w.Roles) != 1 || role.Seats != 1 || role.Agent != name {
			return "", false
		}
	}
	return name, true
}

// onlyMessage reports whether an input schema takes no property but the
// message: a workflow with settings is more than its agent.
func onlyMessage(schema json.RawMessage) bool {
	if len(schema) == 0 {
		return true
	}
	var s struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if json.Unmarshal(schema, &s) != nil {
		return false
	}
	for name := range s.Properties {
		if name != "message" {
			return false
		}
	}
	return true
}

// delegateOf is the agent a workflow's source hands the message to, when
// its run does nothing else.
func delegateOf(source string) (string, bool) {
	file, err := (&syntax.FileOptions{}).Parse("workflow.star", source, 0)
	if err != nil {
		return "", false
	}
	var run *syntax.DefStmt
	for _, stmt := range file.Stmts {
		if def, ok := stmt.(*syntax.DefStmt); ok && def.Name.Name == "run" {
			run = def
		}
	}
	if run == nil || len(run.Params) != 1 || len(run.Body) != 1 {
		return "", false
	}
	param, ok := run.Params[0].(*syntax.Ident)
	if !ok {
		return "", false
	}
	ret, ok := run.Body[0].(*syntax.ReturnStmt)
	if !ok {
		return "", false
	}
	call, ok := ret.Result.(*syntax.CallExpr)
	if !ok {
		return "", false
	}
	if fn, ok := call.Fn.(*syntax.Ident); !ok || fn.Name != "agent" || len(call.Args) != 2 {
		return "", false
	}
	name, ok := call.Args[0].(*syntax.Literal)
	if !ok || name.Token != syntax.STRING {
		return "", false
	}
	task := call.Args[1]
	if kw, ok := task.(*syntax.BinaryExpr); ok && kw.Op == syntax.EQ {
		if key, ok := kw.X.(*syntax.Ident); !ok || key.Name != "task" {
			return "", false
		}
		task = kw.Y
	}
	if !isMessage(task, param.Name) {
		return "", false
	}
	return name.Value.(string), true
}

// isMessage reports whether e is `<input>.message`.
func isMessage(e syntax.Expr, input string) bool {
	dot, ok := e.(*syntax.DotExpr)
	if !ok || dot.Name.Name != "message" {
		return false
	}
	x, ok := dot.X.(*syntax.Ident)
	return ok && x.Name == input
}
