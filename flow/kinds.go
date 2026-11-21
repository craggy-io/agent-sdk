package flow

import (
	"fmt"
	"regexp"
)

// Param is one argument of a step kind, in the order flow.star declares it.
type Param struct {
	Name string `json:"name"`
	// Type is string, var, file, model, number, bool, steps, template, op,
	// any, fn, capability or binds (a dict of argument names to variables).
	Type     string `json:"type"`
	Default  any    `json:"default,omitempty"`
	Required bool   `json:"required,omitempty"`
	Label    string `json:"label"`
}

// Kind is one kind of step: a function in flow.star.
type Kind struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	// Capability is the capability it calls; empty for plain Starlark and
	// for call, whose step names its own (Step.Calls).
	Capability string  `json:"capability,omitempty"`
	Spends     bool    `json:"spends"`
	Tone       string  `json:"tone"` // info, accent, neutral, success: the chip colour
	Detail     string  `json:"detail"`
	Params     []Param `json:"params"`
}

func p(name, typ, label string, def any) Param {
	return Param{Name: name, Type: typ, Label: label, Default: def}
}
func req(name, typ, label string) Param {
	return Param{Name: name, Type: typ, Label: label, Required: true}
}

var (
	idParam    = req("id", "string", "Step ID")
	titleParam = p("title", "string", "Title", "")
)

// Call is the kind that calls any capability.
const Call = "call"

// Kinds are the step kinds, in the order an Add a step menu lists them.
var Kinds = []Kind{
	{Name: "read", Label: "Read data", Capability: "data.remote.sql@1", Tone: "info", Detail: "Rows of one of the pipeline's inputs.",
		Params: []Param{idParam, titleParam, req("input", "string", "Input"), p("out", "var", "Output", "")}},
	{Name: "for_each", Label: "For each", Tone: "neutral", Detail: "Repeats its steps for every item of a list.",
		Params: []Param{idParam, titleParam, req("over", "var", "Over"), req("item", "string", "Item name"), req("steps", "steps", "Steps")}},
	{Name: "agent", Label: "Agent", Capability: "core.workflows.run@1", Spends: true, Tone: "accent", Detail: "An agent with a prompt, returning structured output.",
		Params: []Param{idParam, titleParam, req("prompt", "file", "Prompt"), req("input", "var", "Input"), req("out", "var", "Output"),
			p("model", "model", "Model", "med"), p("schema", "file", "Structured output", ""), p("many", "bool", "A list of them", false),
			p("budget", "number", "Budget per item (credits)", int64(200)), p("max_turns", "number", "Turns at most", int64(8))}},
	{Name: "ask", Label: "Ask a model", Capability: "core.models.complete@1", Spends: true, Tone: "accent", Detail: "One model call: text, or JSON matching a schema.",
		Params: []Param{idParam, titleParam, req("prompt", "file", "Prompt"), req("input", "var", "Input"), req("out", "var", "Output"),
			p("model", "model", "Model", "low"), p("schema", "file", "Structured output", ""), p("many", "bool", "A list of them", false),
			p("max_tokens", "number", "Output tokens at most", int64(2000))}},
	{Name: "embed", Label: "Embed", Capability: "core.models.embed@1", Spends: true, Tone: "accent", Detail: "Adds an embedding to each item.",
		Params: []Param{idParam, titleParam, req("over", "var", "Over"), req("field", "string", "Field"), req("out", "var", "Output"), p("model", "model", "Model", "embed")}},
	{Name: "keep_if", Label: "Keep if", Tone: "neutral", Detail: "Keeps the items whose field passes a test. Free.",
		Params: []Param{idParam, titleParam, req("over", "var", "Over"), req("field", "string", "Field"), req("op", "op", "Test"), req("value", "any", "Value")}},
	{Name: "group", Label: "Group", Tone: "neutral", Detail: "Groups items by a field. Free.",
		Params: []Param{idParam, titleParam, req("over", "var", "Over"), req("by", "string", "By"), req("out", "var", "Output")}},
	{Name: "sort", Label: "Sort", Tone: "neutral", Detail: "Sorts items by a field. Free.",
		Params: []Param{idParam, titleParam, req("over", "var", "Over"), req("by", "string", "By"), p("desc", "bool", "Largest first", false)}},
	{Name: "save", Label: "Save", Capability: "core.packs.write@1", Tone: "success", Detail: "Writes a file into the pack.",
		Params: []Param{idParam, titleParam, req("path", "template", "Path"), req("text", "var", "Text")}},
	{Name: Call, Label: "Call a capability", Tone: "neutral", Detail: "Calls any capability the host serves, with fixed arguments and arguments read from earlier steps.",
		Params: []Param{idParam, titleParam, req("capability", "capability", "Capability"), p("args", "any", "Arguments", nil),
			p("bind", "binds", "Arguments from steps", nil), p("out", "var", "Output", "")}},
	{Name: "code", Label: "Code", Tone: "neutral", Detail: "A Starlark function from the Code tab.",
		Params: []Param{idParam, titleParam, req("fn", "fn", "Function"), p("out", "var", "Output", "")}},
}

// KindOf finds a step kind.
func KindOf(name string) (Kind, bool) {
	for _, k := range Kinds {
		if k.Name == name {
			return k, true
		}
	}
	return Kind{}, false
}

// Ops are the tests Keep if offers.
var Ops = []string{">=", ">", "<=", "<", "==", "!=", "in", "contains"}

// Output is the variable a step writes, as flow.star's _output says.
func (s *Step) Output() string {
	switch s.Kind {
	case "keep_if", "sort":
		return s.String("over")
	case "read":
		if o := s.String("out"); o != "" {
			return o
		}
		return s.String("input")
	case "agent", "ask", "embed", "group", "code", Call:
		return s.String("out")
	}
	return ""
}

// Calls is the capability a step calls: its kind's, or a call step's own.
// Read steps call none here: the host reads inputs before the run.
func (s *Step) Calls() string {
	if s.Kind == Call {
		return s.String("capability")
	}
	if s.Kind == "read" {
		return ""
	}
	k, _ := KindOf(s.Kind)
	return k.Capability
}

// capRef is a capability reference: name@version.
var capRef = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_-]*)+@[1-9][0-9]*$`)

// CheckRef refuses what isn't a capability reference such as
// artifacts.get@1.
func CheckRef(ref string) error {
	if !capRef.MatchString(ref) {
		return fmt.Errorf("%q isn't a capability reference such as artifacts.get@1", ref)
	}
	return nil
}
