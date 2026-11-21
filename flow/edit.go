package flow

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Op is one flow edit, as an editor or an agent sends it.
//
//	{"op": "add", "step": {...}, "after": "<id>"|"", "parent": "<for each id>"|""}
//	{"op": "edit", "id": "<id>", "set": {"model": "low"}}
//	{"op": "move", "id": "<id>", "after": "<id>"|"", "parent": "<for each id>"|""}
//	{"op": "remove", "id": "<id>"}
//	{"op": "file", "path": "prompts/find.md", "text": "..."}   (text null removes it)
//	{"op": "input", "name": "posts", "input": {...}}           (input null removes it)
//	{"op": "title", "title": "..."}
type Op struct {
	Op     string                     `json:"op"`
	ID     string                     `json:"id,omitempty"`
	Step   *Step                      `json:"step,omitempty"`
	After  string                     `json:"after,omitempty"`
	Parent string                     `json:"parent,omitempty"`
	Set    map[string]json.RawMessage `json:"set,omitempty"`
	Path   string                     `json:"path,omitempty"`
	Text   *string                    `json:"text,omitempty"`
	Name   string                     `json:"name,omitempty"`
	Input  *Input                     `json:"input,omitempty"`
	Title  string                     `json:"title,omitempty"`
}

// Apply returns the bundle with the ops applied, and a one-line summary of
// what changed. The bundle passed in is unchanged.
func Apply(b Bundle, ops []Op) (Bundle, string, error) {
	out := b.Clone()
	src := []byte(out[FileName])
	fl, err := ReadFlow(src)
	if err != nil {
		return nil, "", err
	}
	steps := fl.Steps
	stepsChanged := false
	var head *Header
	var said []string
	for i, op := range ops {
		var err error
		switch op.Op {
		case "add":
			if op.Step == nil {
				return nil, "", fmt.Errorf("op %d: add needs a step", i)
			}
			if Find(steps, op.Step.ID()) != nil {
				return nil, "", fmt.Errorf("op %d: a step %q already exists", i, op.Step.ID())
			}
			steps, err = insert(steps, op.Parent, op.After, op.Step)
			said = append(said, "added "+label(op.Step))
			stepsChanged = true
		case "edit":
			s := Find(steps, op.ID)
			if s == nil {
				return nil, "", fmt.Errorf("op %d: no step %q", i, op.ID)
			}
			was := label(s)
			var names []string
			for name, raw := range op.Set {
				if name == "kind" || name == "steps" {
					return nil, "", fmt.Errorf("op %d: %s can't be edited; remove and add the step", i, name)
				}
				v, err := fromJSON(raw)
				if err != nil {
					return nil, "", fmt.Errorf("op %d: %s: %w", i, name, err)
				}
				if v == nil {
					s.Args = removeArg(s.Args, name)
				} else {
					s.Set(name, v)
				}
				names = append(names, name)
			}
			said = append(said, "changed the "+strings.Join(sorted(names), ", ")+" of "+was)
			stepsChanged = true
		case "move":
			s := Find(steps, op.ID)
			if s == nil {
				return nil, "", fmt.Errorf("op %d: no step %q", i, op.ID)
			}
			steps = remove(steps, op.ID)
			steps, err = insert(steps, op.Parent, op.After, s)
			said = append(said, "moved "+label(s))
			stepsChanged = true
		case "remove":
			s := Find(steps, op.ID)
			if s == nil {
				return nil, "", fmt.Errorf("op %d: no step %q", i, op.ID)
			}
			steps = remove(steps, op.ID)
			said = append(said, "removed "+label(s))
			stepsChanged = true
		case "file":
			if err := CheckPath(op.Path); err != nil {
				return nil, "", fmt.Errorf("op %d: %w", i, err)
			}
			if op.Path == FileName {
				return nil, "", fmt.Errorf("op %d: change pipeline.star with a patch or flow ops", i)
			}
			if op.Text == nil {
				delete(out, op.Path)
				said = append(said, "removed "+op.Path)
			} else {
				if len(*op.Text) > MaxFileBytes {
					return nil, "", fmt.Errorf("op %d: %s is too large", i, op.Path)
				}
				_, existed := out[op.Path]
				out[op.Path] = *op.Text
				if existed {
					said = append(said, "rewrote "+op.Path)
				} else {
					said = append(said, "wrote "+op.Path)
				}
			}
		case "input", "title":
			if head == nil {
				h, err := ReadHeader(src)
				if err != nil {
					return nil, "", err
				}
				head = &h
			}
			if op.Op == "title" {
				head.Title = op.Title
				said = append(said, "renamed it")
				break
			}
			if op.Input == nil {
				delete(head.Inputs, op.Name)
				head.InputOrder = without(head.InputOrder, op.Name)
				said = append(said, "removed input "+op.Name)
			} else {
				if _, ok := head.Inputs[op.Name]; !ok {
					head.InputOrder = append(head.InputOrder, op.Name)
				}
				head.Inputs[op.Name] = *op.Input
				said = append(said, "set input "+op.Name)
			}
		default:
			return nil, "", fmt.Errorf("op %d: unknown op %q", i, op.Op)
		}
		if err != nil {
			return nil, "", fmt.Errorf("op %d: %w", i, err)
		}
	}
	if stepsChanged {
		src = fl.Source(steps)
		// The steps' capabilities join requires, so a flow edit never
		// leaves the header behind. Nothing is removed: a Code step may
		// call what no step kind does.
		if head == nil {
			h, err := ReadHeader(src)
			if err != nil {
				return nil, "", err
			}
			head = &h
		}
		head.Requires = Needs(steps, head.Requires)
	}
	if head != nil {
		if src, err = SetHeader(src, *head); err != nil {
			return nil, "", err
		}
	}
	out[FileName] = string(src)
	if len(out) > MaxFiles {
		return nil, "", fmt.Errorf("a pipeline has at most %d files", MaxFiles)
	}
	return out, summarize(said), nil
}

func label(s *Step) string {
	if t := s.String("title"); t != "" {
		return t
	}
	return s.ID()
}

func summarize(said []string) string {
	if len(said) == 0 {
		return "no change"
	}
	s := strings.Join(said, ", ")
	if len(said) > 3 {
		s = strings.Join(said[:3], ", ") + fmt.Sprintf(" and %d more changes", len(said)-3)
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func removeArg(args []Arg, name string) []Arg {
	out := args[:0:0]
	for _, a := range args {
		if a.Name != name {
			out = append(out, a)
		}
	}
	return out
}

func without(list []string, s string) []string {
	out := list[:0:0]
	for _, x := range list {
		if x != s {
			out = append(out, x)
		}
	}
	return out
}

func sorted(s []string) []string {
	out := append([]string{}, s...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// insert puts s after the step `after` (first when empty) in the top list
// or in the For each `parent`.
func insert(steps []*Step, parent, after string, s *Step) ([]*Step, error) {
	if parent == "" {
		return insertIn(steps, after, s)
	}
	p := Find(steps, parent)
	if p == nil || p.Kind != "for_each" {
		return nil, fmt.Errorf("no For each %q", parent)
	}
	inner, err := insertIn(p.Steps(), after, s)
	if err != nil {
		return nil, err
	}
	p.Set("steps", inner)
	return steps, nil
}

func insertIn(list []*Step, after string, s *Step) ([]*Step, error) {
	at := 0
	if after != "" {
		at = -1
		for i, x := range list {
			if x.ID() == after {
				at = i + 1
			}
		}
		if at < 0 {
			return nil, fmt.Errorf("no step %q here to put it after", after)
		}
	}
	out := append([]*Step{}, list[:at]...)
	out = append(out, s)
	return append(out, list[at:]...), nil
}

// remove takes the step with an id out of wherever it is.
func remove(steps []*Step, id string) []*Step {
	out := steps[:0:0]
	for _, s := range steps {
		if s.ID() == id {
			continue
		}
		if inner := s.Steps(); inner != nil {
			s.Set("steps", remove(inner, id))
		}
		out = append(out, s)
	}
	return out
}

// Needs is requires with every capability the steps call added, in the
// order they are first used. Read steps are left out: the host reads
// inputs itself.
func Needs(steps []*Step, requires []string) []string {
	out := append([]string{}, requires...)
	Walk(steps, func(s *Step, _ *Step) {
		if c := s.Calls(); c != "" && CheckRef(c) == nil && !contains(out, c) {
			out = append(out, c)
		}
	})
	return out
}
