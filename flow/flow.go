package flow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"

	"go.starlark.net/syntax"

	"github.com/craggy-io/script-services/dialect"
)

// Step is one step of a flow: a call to flow.<kind>(...) in STEPS. Args keep
// the order they were written in, so a flow read from a file writes back
// byte for byte.
type Step struct {
	Kind string
	Args []Arg
	// Line is where the step is written in pipeline.star, from 1; 0 for a
	// step that came as JSON.
	Line int
}

// Arg is one keyword argument. Value is a string, int64, float64, bool, nil,
// []any, map[string]any, Ref, or []*Step for a For each's steps.
type Arg struct {
	Name  string
	Value any
}

// Ref names a function defined in pipeline.star: a Code step's fn.
type Ref string

// Get returns an argument's value.
func (s *Step) Get(name string) (any, bool) {
	for _, a := range s.Args {
		if a.Name == name {
			return a.Value, true
		}
	}
	return nil, false
}

// String returns a string argument, or "".
func (s *Step) String(name string) string {
	v, _ := s.Get(name)
	str, _ := v.(string)
	return str
}

// ID is the step's id.
func (s *Step) ID() string { return s.String("id") }

// Steps are a For each's steps.
func (s *Step) Steps() []*Step {
	v, _ := s.Get("steps")
	steps, _ := v.([]*Step)
	return steps
}

// Set sets an argument, appending it in the kind's order when it is new.
func (s *Step) Set(name string, v any) {
	for i := range s.Args {
		if s.Args[i].Name == name {
			s.Args[i].Value = v
			return
		}
	}
	s.Args = append(s.Args, Arg{name, v})
	s.order()
}

// order sorts arguments into the kind's parameter order, unknown ones last.
func (s *Step) order() {
	k, _ := KindOf(s.Kind)
	rank := map[string]int{}
	for i, p := range k.Params {
		rank[p.Name] = i
	}
	sort.SliceStable(s.Args, func(i, j int) bool {
		ri, ok := rank[s.Args[i].Name]
		if !ok {
			ri = len(rank)
		}
		rj, ok := rank[s.Args[j].Name]
		if !ok {
			rj = len(rank)
		}
		return ri < rj
	})
}

// Flow is pipeline.star read as steps.
type Flow struct {
	// Steps is STEPS. Nil when the file has no STEPS list: the whole run()
	// is then one Code step.
	Steps []*Step `json:"steps"`
	// Functions are the functions the file defines, by name, with their
	// source: what a Code step shows.
	Functions map[string]string `json:"functions"`
	// span is where STEPS sits in the source, in bytes.
	start, end int
	src        []byte
}

// ErrNoSteps is a file without a STEPS list.
var ErrNoSteps = fmt.Errorf("pipeline: no STEPS list")

// ReadFlow parses pipeline.star and reads its STEPS list.
func ReadFlow(src []byte) (*Flow, error) {
	f, err := dialect.Parse(FileName, src)
	if err != nil {
		return nil, err
	}
	fl := &Flow{Functions: map[string]string{}, src: src, start: -1}
	lines := lineOffsets(src)
	for _, stmt := range f.Stmts {
		switch st := stmt.(type) {
		case *syntax.DefStmt:
			a, b := st.Span()
			fl.Functions[st.Name.Name] = string(src[offset(src, lines, a):offset(src, lines, b)])
		case *syntax.AssignStmt:
			id, ok := st.LHS.(*syntax.Ident)
			if !ok || id.Name != "STEPS" || st.Op != syntax.EQ {
				continue
			}
			list, ok := st.RHS.(*syntax.ListExpr)
			if !ok {
				return nil, fmt.Errorf("pipeline: STEPS must be a list of flow steps")
			}
			steps, err := readSteps(list)
			if err != nil {
				return nil, err
			}
			a, b := st.Span()
			fl.Steps, fl.start, fl.end = steps, offset(src, lines, a), offset(src, lines, b)
		}
	}
	return fl, nil
}

func readSteps(list *syntax.ListExpr) ([]*Step, error) {
	steps := make([]*Step, 0, len(list.List))
	for _, e := range list.List {
		s, err := readStep(e)
		if err != nil {
			return nil, err
		}
		steps = append(steps, s)
	}
	return steps, nil
}

func readStep(e syntax.Expr) (*Step, error) {
	call, ok := e.(*syntax.CallExpr)
	if !ok {
		return nil, posErr(e, "each step must be a flow.<kind>(...) call")
	}
	dot, ok := call.Fn.(*syntax.DotExpr)
	if !ok {
		return nil, posErr(e, "each step must be a flow.<kind>(...) call")
	}
	if x, ok := dot.X.(*syntax.Ident); !ok || x.Name != "flow" {
		return nil, posErr(e, "each step must be a flow.<kind>(...) call")
	}
	start, _ := call.Span()
	s := &Step{Kind: dot.Name.Name, Line: int(start.Line)}
	if _, ok := KindOf(s.Kind); !ok {
		return nil, posErr(e, "unknown step kind flow.%s", s.Kind)
	}
	for _, a := range call.Args {
		bin, ok := a.(*syntax.BinaryExpr)
		if !ok || bin.Op != syntax.EQ {
			return nil, posErr(a, "step arguments must be keywords: name = value")
		}
		name := bin.X.(*syntax.Ident).Name
		var v any
		var err error
		if name == "steps" {
			list, ok := bin.Y.(*syntax.ListExpr)
			if !ok {
				return nil, posErr(bin.Y, "steps must be a list of flow steps")
			}
			v, err = readSteps(list)
		} else {
			v, err = literal(bin.Y)
		}
		if err != nil {
			return nil, err
		}
		s.Args = append(s.Args, Arg{name, v})
	}
	if s.ID() == "" {
		return nil, posErr(e, "a step needs an id")
	}
	return s, nil
}

// literal reads a constant expression: what step arguments may be.
func literal(e syntax.Expr) (any, error) {
	switch x := e.(type) {
	case *syntax.Literal:
		switch v := x.Value.(type) {
		case string:
			return v, nil
		case int64:
			return v, nil
		case *big.Int:
			return nil, posErr(e, "number too large")
		case float64:
			return v, nil
		}
	case *syntax.Ident:
		switch x.Name {
		case "True":
			return true, nil
		case "False":
			return false, nil
		case "None":
			return nil, nil
		}
		return Ref(x.Name), nil
	case *syntax.UnaryExpr:
		if x.Op == syntax.MINUS {
			v, err := literal(x.X)
			switch n := v.(type) {
			case int64:
				return -n, err
			case float64:
				return -n, err
			}
		}
	case *syntax.ListExpr:
		out := []any{}
		for _, el := range x.List {
			v, err := literal(el)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case *syntax.DictExpr:
		out := map[string]any{}
		for _, el := range x.List {
			kv := el.(*syntax.DictEntry)
			k, err := literal(kv.Key)
			ks, ok := k.(string)
			if err != nil || !ok {
				return nil, posErr(kv.Key, "dict keys must be strings")
			}
			v, err := literal(kv.Value)
			if err != nil {
				return nil, err
			}
			out[ks] = v
		}
		return out, nil
	}
	return nil, posErr(e, "a step argument must be a constant (move computed values into a Code step)")
}

func posErr(n syntax.Node, format string, args ...any) error {
	p, _ := n.Span()
	return fmt.Errorf("%s:%d:%d: %s", FileName, p.Line, p.Col, fmt.Sprintf(format, args...))
}

// lineOffsets are the byte offsets where each line starts.
func lineOffsets(src []byte) []int {
	out := []int{0}
	for i, b := range src {
		if b == '\n' {
			out = append(out, i+1)
		}
	}
	return out
}

// offset turns a position (1-based line, 1-based rune column) into a byte
// offset.
func offset(src []byte, lines []int, p syntax.Position) int {
	i := lines[p.Line-1]
	for col := int32(1); col < p.Col && i < len(src); col++ {
		_, n := decodeRune(src[i:])
		i += n
	}
	return i
}

func decodeRune(b []byte) (rune, int) {
	r := []rune(string(b[:min(4, len(b))]))
	if len(r) == 0 {
		return 0, 1
	}
	return r[0], len(string(r[0]))
}

// Source is the file with STEPS replaced by steps written canonically. A file
// without STEPS gets the list appended. Everything else is kept byte for byte.
func (fl *Flow) Source(steps []*Step) []byte {
	text := "STEPS = " + renderSteps(steps, 0)
	var b bytes.Buffer
	if fl.start < 0 {
		b.Write(bytes.TrimRight(fl.src, "\n"))
		b.WriteString("\n\n" + text + "\n")
		return b.Bytes()
	}
	b.Write(fl.src[:fl.start])
	b.WriteString(text)
	b.Write(fl.src[fl.end:])
	return b.Bytes()
}

// HasSteps reports a file with a STEPS list.
func (fl *Flow) HasSteps() bool { return fl.start >= 0 }

const indentUnit = "    "

func renderSteps(steps []*Step, depth int) string {
	if len(steps) == 0 {
		return "[]"
	}
	in := strings.Repeat(indentUnit, depth+1)
	var b strings.Builder
	b.WriteString("[\n")
	for _, s := range steps {
		b.WriteString(in + renderStep(s, depth+1) + ",\n")
	}
	b.WriteString(strings.Repeat(indentUnit, depth) + "]")
	return b.String()
}

// renderStep writes a step on one line, or one argument per line when it
// holds steps of its own.
func renderStep(s *Step, depth int) string {
	nested := false
	for _, a := range s.Args {
		if _, ok := a.Value.([]*Step); ok {
			nested = true
		}
	}
	if !nested {
		parts := make([]string, len(s.Args))
		for i, a := range s.Args {
			parts[i] = a.Name + " = " + renderValue(a.Value)
		}
		return "flow." + s.Kind + "(" + strings.Join(parts, ", ") + ")"
	}
	in := strings.Repeat(indentUnit, depth+1)
	var b strings.Builder
	b.WriteString("flow." + s.Kind + "(\n")
	for _, a := range s.Args {
		v := ""
		if steps, ok := a.Value.([]*Step); ok {
			v = renderSteps(steps, depth+1)
		} else {
			v = renderValue(a.Value)
		}
		b.WriteString(in + a.Name + " = " + v + ",\n")
	}
	b.WriteString(strings.Repeat(indentUnit, depth) + ")")
	return b.String()
}

func renderValue(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case string:
		return quote(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case int:
		return strconv.Itoa(x)
	case float64:
		if x == float64(int64(x)) && x < 1e15 && x > -1e15 {
			return strconv.FormatFloat(x, 'f', 1, 64)
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	case Ref:
		return string(x)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = renderValue(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = quote(k) + ": " + renderValue(x[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return quote(fmt.Sprint(v))
}

// quote writes a Starlark string literal.
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\x%02x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// MarshalJSON writes a step as {"kind": ..., <args in order>}.
func (s *Step) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(`{"kind":`)
	k, _ := json.Marshal(s.Kind)
	b.Write(k)
	for _, a := range s.Args {
		if a.Name == "kind" {
			continue
		}
		name, _ := json.Marshal(a.Name)
		v, err := json.Marshal(jsonValue(a.Value))
		if err != nil {
			return nil, err
		}
		b.WriteByte(',')
		b.Write(name)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func jsonValue(v any) any {
	switch x := v.(type) {
	case Ref:
		return map[string]string{"$ref": string(x)}
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = jsonValue(e)
		}
		return out
	case map[string]any:
		out := map[string]any{}
		for k, e := range x {
			out[k] = jsonValue(e)
		}
		return out
	}
	return v
}

// UnmarshalJSON reads a step from the UI or the assistant: arguments are put
// in the kind's order, and those equal to their default are left out.
func (s *Step) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if err := json.Unmarshal(raw["kind"], &s.Kind); err != nil {
		return fmt.Errorf("a step needs a kind")
	}
	k, ok := KindOf(s.Kind)
	if !ok {
		return fmt.Errorf("unknown step kind %q", s.Kind)
	}
	defaults := map[string]any{}
	for _, p := range k.Params {
		defaults[p.Name] = p.Default
	}
	s.Args = nil
	for name, msg := range raw {
		if name == "kind" {
			continue
		}
		var v any
		if name == "steps" {
			var steps []*Step
			if err := json.Unmarshal(msg, &steps); err != nil {
				return err
			}
			v = steps
		} else {
			var err error
			if v, err = fromJSON(msg); err != nil {
				return err
			}
		}
		if d, ok := defaults[name]; ok && d != nil && equal(d, v) {
			continue
		}
		if name == "title" && v == "" {
			continue
		}
		s.Args = append(s.Args, Arg{name, v})
	}
	s.order()
	return nil
}

func fromJSON(msg json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(msg))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return normal(v), nil
}

// normal turns decoded JSON into step values: integers as int64, {"$ref"}
// as a Ref.
func normal(v any) any {
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i
		}
		f, _ := x.Float64()
		return f
	case []any:
		for i := range x {
			x[i] = normal(x[i])
		}
		return x
	case map[string]any:
		if r, ok := x["$ref"].(string); ok && len(x) == 1 {
			return Ref(r)
		}
		for k := range x {
			x[k] = normal(x[k])
		}
		return x
	}
	return v
}

func equal(a, b any) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return bytes.Equal(ja, jb)
}

// Walk visits every step, For each steps after their parent.
func Walk(steps []*Step, visit func(s *Step, parent *Step)) {
	var walk func([]*Step, *Step)
	walk = func(ss []*Step, parent *Step) {
		for _, s := range ss {
			visit(s, parent)
			walk(s.Steps(), s)
		}
	}
	walk(steps, nil)
}

// Find returns the step with an id.
func Find(steps []*Step, id string) *Step {
	var found *Step
	Walk(steps, func(s *Step, _ *Step) {
		if found == nil && s.ID() == id {
			found = s
		}
	})
	return found
}
