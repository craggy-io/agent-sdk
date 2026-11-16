package agentdef

import (
	"context"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/craggy-io/foundation-kit/bundle"
	"github.com/craggy-io/foundation-kit/bundle/compile"
	"github.com/craggy-io/foundation-kit/kind"
)

// Slots: what an agent seeks, and what fills it.
//
// An agent says what it wants by the name of a slot, never by the bundle
// that provides it: seeks = ["memory"]. A slot filler is a contribution
// (SlotFillerKind) of whatever bundle provides that kind of thing: the
// tools that belong to it, a prompt that teaches an agent to use them,
// the skills it brings and a priority among the fillers of the same slot.
// Which filler a run gets is the host's choice (Fillers ranks them); the
// run is then resolved with it (ResolveFilled): its prompt is added after
// the definition's own fragments, its skills to the agent's, its tools
// are always offered, and the tools of every other filler of that slot
// are left out.

// Seek is one slot an agent asks to have filled when it runs.
type Seek struct {
	// Slot names what the agent wants, such as memory.
	Slot string `json:"slot"`
	// Optional runs the agent without it when nothing installed fills it.
	Optional bool `json:"optional,omitempty"`
	// Prefer is the filler the agent wants when it is installed, by its
	// contribution id. Otherwise the filler of the highest priority.
	Prefer string `json:"prefer,omitempty"`
}

// Filler is one catalogued slot filler.
type Filler struct {
	// Name is its contribution id.
	Name        string `json:"name"`
	Slot        string `json:"slot"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	// Priority ranks the fillers of one slot: the highest is chosen when
	// nothing else decides.
	Priority int `json:"priority,omitempty"`
	// Tools are the tool names it brings, each of which may end in *.
	Tools []string `json:"tools,omitempty"`
	// Prompt is the text of its prompt file, read when it is catalogued.
	Prompt string `json:"prompt,omitempty"`
	// PromptFile is that file's path in the bundle.
	PromptFile string   `json:"promptFile,omitempty"`
	Skills     []string `json:"skills,omitempty"`
	// Seed is a capability the host may call when a conversation starts,
	// whose text the agent is given with its first task.
	Seed string `json:"seed,omitempty"`
}

// Fill is one slot filled for a run: the slot and the filler chosen.
type Fill struct {
	Slot   string `json:"slot"`
	Filler string `json:"filler"`
}

// SlotFillerKind is a YAML file describing one slot filler:
//
//	slot: memory
//	title: Agent Memory (Basic)
//	priority: 10
//	tools: [memory_*]
//	prompt: activation.md
//	skills: [memory-notebook]
//	seed: memory.seed
var SlotFillerKind = kind.New[*Filler]("agentsdk.slot-filler", 1, kind.Global())

// CodeSlot: a slot filler is malformed, or a seek names a slot badly.
const CodeSlot bundle.Code = "agentsdk.slot"

// slotName is what a slot may be called: lower-case words joined by
// hyphens.
var slotName = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// ValidSlot rejects a slot name that is not lower-case words joined by
// hyphens.
func ValidSlot(slot string) error {
	if !slotName.MatchString(slot) {
		return fmt.Errorf("slot %q: a slot is lower-case words joined by hyphens, such as memory", slot)
	}
	return nil
}

type fillerFile struct {
	Slot        string   `json:"slot"`
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Priority    int      `json:"priority,omitempty"`
	Tools       []string `json:"tools,omitempty"`
	Prompt      string   `json:"prompt,omitempty"`
	Skills      []string `json:"skills,omitempty"`
	Seed        string   `json:"seed,omitempty"`
}

func fillerCompiler() bundle.AnyCompiler {
	return bundle.Compile(SlotFillerKind, func(ctx context.Context, c bundle.Contribution) (bundle.Compiled[*Filler], []bundle.Diagnostic) {
		fail := func(err error) (bundle.Compiled[*Filler], []bundle.Diagnostic) {
			return bundle.Compiled[*Filler]{}, []bundle.Diagnostic{{Code: CodeSlot, Severity: bundle.SeverityError,
				Bundle: c.Bundle.ID, Contribution: c.Ref.ID, Path: c.Path, Message: err.Error()}}
		}
		file, diags := compile.YAML[fillerFile]()(ctx, c)
		if len(diags) > 0 {
			return bundle.Compiled[*Filler]{}, diags
		}
		f := file.Value
		if err := ValidSlot(f.Slot); err != nil {
			return fail(err)
		}
		if strings.TrimSpace(f.Title) == "" {
			return fail(fmt.Errorf("slot filler %s: a title is required: what a person chooses it by", c.Ref.ID))
		}
		if f.Prompt == "" && len(f.Tools) == 0 && len(f.Skills) == 0 {
			return fail(fmt.Errorf("slot filler %s brings nothing: give it tools, a prompt or skills", c.Ref.ID))
		}
		out := &Filler{Name: c.Ref.ID, Slot: f.Slot, Title: f.Title, Description: f.Description, Priority: f.Priority,
			Tools: f.Tools, Skills: f.Skills, Seed: f.Seed}
		if f.Prompt != "" {
			out.PromptFile = path.Join(path.Dir(c.Path), f.Prompt)
			b, err := fs.ReadFile(c.Files, out.PromptFile)
			if err != nil {
				return fail(fmt.Errorf("slot filler %s: prompt %q: %w", c.Ref.ID, f.Prompt, err))
			}
			out.Prompt = string(b)
		}
		return bundle.Compiled[*Filler]{Value: out}, nil
	})
}

// Fillers are the catalogued fillers of a slot, the highest priority first,
// then by name.
func Fillers(cat *bundle.Catalog, slot string) []*Filler {
	var out []*Filler
	for _, def := range bundle.All(cat, SlotFillerKind) {
		if def.Value.Slot == slot {
			out = append(out, def.Value)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Fillers are the fillers of a slot in the loader's catalog, ranked.
func (l *Loader) Fillers(slot string) []*Filler { return Fillers(l.Catalog(), slot) }

// Filled is a slot a resolved run has filled, and with what.
type Filled struct {
	Slot   string `json:"slot"`
	Filler string `json:"filler"`
}

// applyFills adds what each filler brings to a resolution in progress:
// its prompt as the fragment slot.<slot>, its skills, its tools pinned,
// and the tools of the other fillers of its slot left out.
func applyFills(cat *bundle.Catalog, r *Resolved, frags []fragment, fills []Fill) ([]fragment, error) {
	seen := map[string]bool{}
	for _, f := range fills {
		if seen[f.Slot] {
			return nil, fmt.Errorf("agentdef: %s: slot %q is filled twice", r.Name, f.Slot)
		}
		seen[f.Slot] = true
		filler, ok := lookup(cat, SlotFillerKind, f.Filler)
		if !ok {
			return nil, fmt.Errorf("agentdef: %s: unknown slot filler %q", r.Name, f.Filler)
		}
		if filler.Slot != f.Slot {
			return nil, fmt.Errorf("agentdef: %s: %q fills %q, not %q", r.Name, f.Filler, filler.Slot, f.Slot)
		}
		r.Filled = append(r.Filled, Filled{Slot: f.Slot, Filler: f.Filler})
		r.Provenance["slot."+f.Slot] = f.Filler
		if filler.Prompt != "" {
			name := "slot." + f.Slot
			frags = append(frags, fragment{name: name, text: filler.Prompt, source: f.Filler})
			r.Provenance["prompt."+name] = f.Filler
		}
		r.Skills = unionStrings(r.Skills, filler.Skills)
		r.Pinned = unionStrings(r.Pinned, filler.Tools)
		mine := map[string]bool{}
		for _, t := range filler.Tools {
			mine[t] = true
		}
		for _, other := range Fillers(cat, f.Slot) {
			if other.Name == filler.Name {
				continue
			}
			for _, t := range other.Tools {
				if !mine[t] {
					r.Tools.Exclude = unionStrings(r.Tools.Exclude, []string{t})
				}
			}
		}
	}
	return frags, nil
}
