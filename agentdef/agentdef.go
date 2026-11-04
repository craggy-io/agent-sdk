// Package agentdef loads Starlark agent definitions and resolves
// inheritance: ordered roots with shadowing, single-parent extends,
// prompt fragment append/replace/remove, tool-filter union (max_effect
// only ever tightens), template rendering with missingkey=error, and the
// skills catalogue appended last.
//
// A definition file is Starlark. Its top level calls agent(...) once per
// agent and may call skill(...); skills may also come from
// skills/<name>/SKILL.md files. Workflows are catalogued from
// workflows/<name>.star and executed by package workflow.
//
//	agent(
//	    name = "reviewer",
//	    extends = "base",
//	    profile = "deep",
//	    prompt = [
//	        {"review": "Review the proposal..."},
//	        {"replace": "conduct", "text": "Be strict."},
//	        {"remove": "investigation"},
//	    ],
//	    tools = tools_filter(max_effect = "read-only"),
//	    skills = ["example-review"],
//	    loop = loop_spec(max_steps = 40, completion = "structured",
//	                     output = schema.object({"verdict": schema.string()})),
//	)
//
// Tools inherit by default: a definition's filter unions with its
// parent's unless tools_filter(inherit = False) starts from nothing. A
// definition with no tools at all always inherits.
package agentdef

import (
	"encoding/json"
	"time"

	"github.com/craggy-io/agent-sdk/model"
)

// PromptEntry is one prompt list entry. Exactly one form: File (append,
// contents loaded at catalogue time), Replace+File/Text (substitute in
// place), Remove (delete), Name+Text (named inline fragment), or Text
// (anonymous inline fragment, mainly dynamic definitions). A File
// fragment's name is its file stem.
type PromptEntry struct {
	File    string `json:"file,omitempty"`
	Name    string `json:"name,omitempty"`
	Replace string `json:"replace,omitempty"`
	Remove  string `json:"remove,omitempty"`
	Text    string `json:"text,omitempty"`
}

// LoopSpec is per-definition loop policy: inherited field by field, child
// overrides; Output is replaced, never merged. Pointer fields record
// presence. Output is an inline JSON Schema (Starlark schema.* values and
// schema files are converted to JSON at catalogue time).
type LoopSpec struct {
	MaxSteps          *int           `json:"max_steps,omitempty"`
	Completion        *string        `json:"completion,omitempty"` // text | structured
	Output            *string        `json:"output,omitempty"`
	ToolResultBytes   *int           `json:"tool_result_bytes,omitempty"`
	WaitJobs          *time.Duration `json:"wait_jobs,omitempty"`
	CompactAt         *float64       `json:"compact_at,omitempty"`
	KeepRecentTokens  *int           `json:"keep_recent_tokens,omitempty"`
	SummaryTokens     *int           `json:"summary_tokens,omitempty"`
	ProgressiveTools  *bool          `json:"progressive_tools,omitempty"`
	ClearResultsAt    *float64       `json:"clear_results_at,omitempty"`
	KeepRecentResults *int           `json:"keep_recent_results,omitempty"`
}

// Definition is one agent definition: from a Starlark file, from Go, or
// dynamic. It names a profile, never a model.
type Definition struct {
	Name        string           `json:"name"`
	Extends     string           `json:"extends,omitempty"`
	Description string           `json:"description,omitempty"`
	Profile     string           `json:"profile,omitempty"`
	Prompt      []PromptEntry    `json:"prompt,omitempty"`
	Tools       model.FilterSpec `json:"tools,omitempty"`
	Skills      []string         `json:"skills,omitempty"`
	Delegates   []string         `json:"delegates,omitempty"`
	Workflows   bool             `json:"workflows,omitempty"`
	Loop        LoopSpec         `json:"loop,omitempty"`
	// Source is the catalogue path that declared this definition
	// (for example "agents/reviewers.star"); empty for Go-built ones.
	Source string `json:"source,omitempty"`
	// SourceText is the exact declaring file's bytes when loaded.
	SourceText []byte `json:"-"`
}

// Skill is one loaded skill: name/description plus body. Only the
// catalogue (name+description) enters the system prompt; the body is
// fetched on demand through the loop's skill built-in.
type Skill struct {
	Name        string
	Description string
	Body        string
}

// Resolved is the immutable output of inheritance resolution, with
// Provenance recording which ancestor supplied each field.
type Resolved struct {
	Name        string           `json:"name"`
	Extends     string           `json:"extends,omitempty"`
	Description string           `json:"description,omitempty"`
	Profile     string           `json:"profile,omitempty"`
	Tools       model.FilterSpec `json:"tools,omitempty"`
	Skills      []string         `json:"skills,omitempty"`
	Delegates   []string         `json:"delegates,omitempty"`
	Workflows   bool             `json:"workflows,omitempty"`
	Loop        LoopSpec         `json:"loop,omitempty"`

	// SystemPrompt is the single rendered system message: fragments
	// joined with a blank line, skills catalogue last.
	SystemPrompt string `json:"system_prompt"`
	// OutputSchema is the resolved loop.output schema, if any.
	OutputSchema json.RawMessage `json:"output_schema,omitempty"`
	// Fragments are the prompt fragment names in order.
	Fragments []string `json:"fragments,omitempty"`
	// Provenance maps field ("profile", "loop.max_steps",
	// "prompt.<fragment>", ...) to the definition name that supplied it.
	Provenance map[string]string `json:"provenance,omitempty"`
	// Chain is the inheritance chain, root ancestor first.
	Chain []string `json:"chain,omitempty"`
}
