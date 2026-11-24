package flow

import (
	"fmt"
	"regexp"
	"strings"
)

// Param is one argument of a step kind, in the order flow.star declares it.
type Param struct {
	Name string `json:"name"`
	// Type is string, var, file, model, number, bool, steps, template, op,
	// any, fn, capability, binds (a dict of argument names to variables),
	// agent (an installed agent's id), workflow (an installed workflow's
	// name) or choice (one of Options).
	Type     string `json:"type"`
	Default  any    `json:"default,omitempty"`
	Required bool   `json:"required,omitempty"`
	Label    string `json:"label"`
	// Options are what a choice may be, in the order a menu offers them.
	Options []string `json:"options,omitempty"`
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
func choice(name, label string, options []string) Param {
	return Param{Name: name, Type: "choice", Label: label, Default: options[0], Options: options}
}

// Formats are what an export step writes: a JSON array, JSON Lines, CSV
// with a header row, XML or YAML.
var Formats = []string{"json", "jsonl", "csv", "xml", "yaml"}

// Decisions are the shapes a decide step asks for, as core.models.decide
// names them: yes or no, one of the options, several of them, a score
// from min to max, or every option in order.
var Decisions = []string{"bool", "choice", "choices", "score", "rank"}

var (
	idParam    = req("id", "string", "Step ID")
	titleParam = p("title", "string", "Title", "")
)

// Call is the kind that calls any capability.
const Call = "call"

// Approve is the kind that stops a run until a person answers.
const Approve = "approve"

// ApproveRef is what an approve step calls: the host's answer to it. A
// host that can't wait for a person doesn't serve it.
const ApproveRef = "core.pipelines.approve@1"

// Kinds are the step kinds, in the order an Add a step menu lists them.
var Kinds = []Kind{
	{Name: "read", Label: "Read data", Capability: "data.remote.sql@1", Tone: "info", Detail: "Rows of one of the pipeline's inputs.",
		Params: []Param{idParam, titleParam, req("input", "string", "Input"), p("out", "var", "Output", "")}},
	{Name: "for_each", Label: "For each", Tone: "neutral", Detail: "Repeats its steps for every item of a list.",
		Params: []Param{idParam, titleParam, req("over", "var", "Over"), req("item", "string", "Item name"), req("steps", "steps", "Steps")}},
	{Name: "agent", Label: "Agent", Capability: "core.workflows.run@1", Spends: true, Tone: "accent", Detail: "An agent with a prompt, returning structured output: one made for the step, or an installed agent with its own tools.",
		Params: []Param{idParam, titleParam, p("agent", "agent", "Installed agent", ""), req("prompt", "file", "Prompt"), req("input", "var", "Input"), req("out", "var", "Output"),
			p("model", "model", "Model", "med"), p("schema", "file", "Structured output", ""), p("many", "bool", "A list of them", false),
			p("budget", "number", "Budget per item (credits)", int64(200)), p("max_turns", "number", "Turns at most", int64(8))}},
	{Name: "ask", Label: "Ask a model", Capability: "core.models.complete@1", Spends: true, Tone: "accent", Detail: "One model call: text, or JSON matching a schema.",
		Params: []Param{idParam, titleParam, req("prompt", "file", "Prompt"), req("input", "var", "Input"), req("out", "var", "Output"),
			p("model", "model", "Model", "low"), p("schema", "file", "Structured output", ""), p("many", "bool", "A list of them", false),
			p("max_tokens", "number", "Output tokens at most", int64(2000))}},
	{Name: "decide", Label: "Decide", Capability: "core.models.decide@1", Spends: true, Tone: "accent", Detail: "One checked decision about an earlier step's output: yes or no, one or several of some options, a score, or the options in order, with the model's confidence and, if asked, its reason.",
		Params: []Param{idParam, titleParam, req("question", "template", "Question"), req("input", "var", "About"), req("out", "var", "Output"),
			choice("decision", "Decision", Decisions), p("options", "string", "Options (comma-separated)", ""),
			p("min", "number", "Lowest score", int64(0)), p("max", "number", "Highest score", int64(1)),
			p("criteria", "string", "How to decide", ""), p("explain", "bool", "With a reason", false), p("model", "model", "Model", "low")}},
	{Name: "workflow", Label: "Run a workflow", Capability: "core.workflows.run@1", Spends: true, Tone: "accent", Detail: "Runs an installed workflow with an earlier step's output as its input.",
		Params: []Param{idParam, titleParam, req("workflow", "workflow", "Workflow"), p("input", "var", "Input", ""), req("out", "var", "Output")}},
	{Name: Approve, Label: "Ask a person", Capability: ApproveRef, Tone: "warning", Detail: "Stops the run until a person answers: Yes goes on, No ends the run.",
		Params: []Param{idParam, titleParam, req("prompt", "template", "Question"), p("show", "var", "Show them", "")}},
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
	{Name: "export", Label: "Export", Capability: "core.packs.write@1", Tone: "success", Detail: "Writes a list of items into the pack as one data file: JSON, JSON Lines, CSV, XML or YAML. Fields leaves out all but the comma-separated fields named; fields starting with _ are always left out.",
		Params: []Param{idParam, titleParam, req("over", "var", "Over"), choice("format", "Format", Formats), req("path", "template", "Path"), p("fields", "string", "Fields", "")}},
	{Name: Dataset, Label: "Save as dataset", Capability: "core.packs.write@1", Tone: "success", Detail: "Saves its items as one collection of a dataset: whole rows keyed by one field, with their declaration beside them, so a host can serve them as data. The declared fields are projections over the rows (undeclared fields ride along whole). Fields names each field and its type (name:type, name:type:index to index it); left empty, the fields and types are read from the items. Aliases names cross-source link keys as JSON ({\"cwe\": {\"path\": \"cwe\", \"pattern\": \"CWE-[0-9]+\"}}). Every item needs its key, and no two may share one.",
		Params: []Param{idParam, titleParam, req("over", "var", "Over"), req("collection", "string", "Collection"), req("key", "string", "Key field"), p("type", "string", "Dataset type", ""), p("type_title", "string", "Dataset type title", ""), p("fields", "string", "Fields (name:type[:index])", ""), p("text", "string", "Text fields", ""), p("aliases", "string", "Aliases (JSON)", "")}},
	{Name: Call, Label: "Call a capability", Tone: "neutral", Detail: "Calls any capability the host serves, with fixed arguments and arguments read from earlier steps.",
		Params: []Param{idParam, titleParam, req("capability", "capability", "Capability"), p("args", "any", "Arguments", nil),
			p("bind", "binds", "Arguments from steps", nil), p("out", "var", "Output", "")}},
	{Name: "code", Label: "Code", Tone: "neutral", Detail: "A Starlark function from the Code tab.",
		Params: []Param{idParam, titleParam, req("fn", "fn", "Function"), p("out", "var", "Output", "")}},
}

// Kinds are the step kinds a host's Add a step menu offers: those whose
// capability it serves, a workflow step only where it has installed
// workflows, and an agent step without its installed-agent choice where it
// has no installed agents. A nil Served offers every capability.
func (e Env) Kinds() []Kind {
	var out []Kind
	for _, k := range Kinds {
		if k.Capability != "" && e.Served != nil && !e.Served(k.Capability) {
			continue
		}
		if k.Name == "workflow" && e.Workflow == nil {
			continue
		}
		if k.Name == "agent" && e.Agent == nil {
			params := make([]Param, 0, len(k.Params))
			for _, p := range k.Params {
				if p.Type != "agent" {
					params = append(params, p)
				}
			}
			k.Params = params
		}
		out = append(out, k)
	}
	return out
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
	case "agent", "ask", "decide", "embed", "group", "code", "workflow", Call:
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

// Reads are the variables a step reads, each by its root name ("skill" for
// skill.slug), in the order of its kind's parameters: its var parameters
// but its output, the names in a template's {braces}, and what a call
// step binds. A For each's own steps are not included.
func (s *Step) Reads() []string {
	var out []string
	add := func(ref string) {
		root, _, _ := strings.Cut(strings.TrimSpace(ref), ".")
		if root != "" && !contains(out, root) {
			out = append(out, root)
		}
	}
	k, _ := KindOf(s.Kind)
	for _, p := range k.Params {
		v, ok := s.Get(p.Name)
		if !ok {
			continue
		}
		switch p.Type {
		case "var":
			if p.Name != "out" {
				str, _ := v.(string)
				add(str)
			}
		case "template":
			str, _ := v.(string)
			for {
				i := strings.Index(str, "{")
				j := strings.Index(str, "}")
				if i < 0 || j < i {
					break
				}
				add(str[i+1 : j])
				str = str[j+1:]
			}
		case "binds":
			binds, _ := v.(map[string]any)
			for _, name := range sortedKeys(binds) {
				ref, _ := binds[name].(string)
				add(ref)
			}
		}
	}
	return out
}
