package flow

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// Env is what validation checks a pipeline against: the host it will run
// in.
type Env struct {
	// Host names the host in problems: "requires x@1, which <Host> doesn't
	// offer".
	Host string
	// Served reports a capability ref the host serves to pipelines.
	Served func(ref string) bool
	// Forbid are capability namespaces, such as "studio.", that a pipeline
	// may never name in any .star file, outside comments.
	Forbid []string
	// Model reports an allowed model and its output-token cap.
	Model func(id string) (maxOutput int, ok bool)
	// Dataset reports a craggy-data dataset that exists.
	Dataset func(name string) bool
	// SourceOnly checks pipeline.star alone, as an editor holding only
	// the source does: the files a step names are not looked for.
	SourceOnly bool
}

// Problem is one thing wrong with a pipeline. Validation is free and runs on
// every save.
type Problem struct {
	File    string `json:"file"`
	Step    string `json:"step,omitempty"`
	Message string `json:"message"`
}

func (p Problem) Error() string {
	if p.Step != "" {
		return fmt.Sprintf("%s: step %s: %s", p.File, p.Step, p.Message)
	}
	return p.File + ": " + p.Message
}

// Validate checks a bundle: it parses, names only served capabilities and
// existing datasets, its steps are complete, their files exist, their
// schemas compile and their models are allowed.
func Validate(b Bundle, env Env) []Problem {
	var out []Problem
	add := func(file, step, format string, args ...any) {
		out = append(out, Problem{File: file, Step: step, Message: fmt.Sprintf(format, args...)})
	}
	host := env.Host
	if host == "" {
		host = "this host"
	}
	forbid := make([]*regexp.Regexp, len(env.Forbid))
	for i, ns := range env.Forbid {
		forbid[i] = regexp.MustCompile(`(^|[^A-Za-z0-9_.])` + regexp.QuoteMeta(ns) + `[a-z]`)
	}
	for _, p := range b.Paths() {
		if err := CheckPath(p); err != nil {
			add(p, "", "%v", err)
		}
		if !strings.HasSuffix(p, ".star") {
			continue
		}
		code := stripComments(b[p])
		for i, re := range forbid {
			if re.MatchString(code) {
				add(p, "", "a pipeline can't call %s* capabilities here", env.Forbid[i])
			}
		}
	}
	src, ok := b[FileName]
	if !ok {
		add(FileName, "", "missing")
		return out
	}
	h, err := ReadHeader([]byte(src))
	if err != nil {
		add(FileName, "", "%v", err)
		return out
	}
	if strings.TrimSpace(h.Title) == "" {
		add(FileName, "", "pipeline() needs a title")
	}
	required := map[string]bool{}
	for _, r := range h.Requires {
		required[r] = true
		if env.Served != nil && !env.Served(r) {
			add(FileName, "", "requires %s, which %s doesn't offer", r, host)
		}
	}
	for _, name := range h.InputOrder {
		in := h.Inputs[name]
		if env.Dataset != nil && !env.Dataset(in.Dataset) {
			add(FileName, "", "input %s reads %s, which craggy-data doesn't have", name, in.Dataset)
		}
	}
	fl, err := ReadFlow([]byte(src))
	if err != nil {
		add(FileName, "", "%v", err)
		return out
	}
	if !fl.HasSteps() {
		return out // a hand-written run(): one Code step, checked when it runs
	}
	vars := map[string]bool{}
	ids := map[string]bool{}
	var check func(steps []*Step, inLoop bool)
	check = func(steps []*Step, inLoop bool) {
		for _, s := range steps {
			id := s.ID()
			if ids[id] {
				add(FileName, id, "another step has this id")
			}
			ids[id] = true
			k, _ := KindOf(s.Kind)
			for _, p := range k.Params {
				v, has := s.Get(p.Name)
				if p.Required && !has {
					add(FileName, id, "needs %s", strings.ToLower(p.Label))
					continue
				}
				if !has {
					continue
				}
				str, _ := v.(string)
				switch p.Type {
				case "var":
					if p.Name == "out" || str == "" {
						break
					}
					root, _, _ := strings.Cut(str, ".")
					if !vars[root] {
						add(FileName, id, "reads %s, which no earlier step makes", str)
					}
				case "file":
					if str == "" || env.SourceOnly {
						break
					}
					text, ok := b[str]
					if !ok {
						add(FileName, id, "%s doesn't exist", str)
						break
					}
					if p.Name == "schema" {
						if err := compileSchema(text); err != nil {
							add(str, id, "the schema doesn't compile: %v", err)
						}
					}
				case "model":
					if env.Model != nil {
						if _, ok := env.Model(str); !ok {
							add(FileName, id, "model %s isn't offered", str)
						}
					}
				case "op":
					if !contains(Ops, str) {
						add(FileName, id, "unknown test %q", str)
					}
				case "fn":
					if r, ok := v.(Ref); !ok || fl.Functions[string(r)] == "" {
						add(FileName, id, "fn must name a function defined in pipeline.star")
					}
				case "capability":
					if err := CheckRef(str); err != nil {
						add(FileName, id, "%v", err)
					}
				case "binds":
					binds, ok := v.(map[string]any)
					if !ok {
						add(FileName, id, "bind must be a dict of argument names to variables")
						break
					}
					for _, name := range sortedKeys(binds) {
						ref, _ := binds[name].(string)
						root, _, _ := strings.Cut(ref, ".")
						if ref == "" || !vars[root] {
							add(FileName, id, "binds %s to %q, which no earlier step makes", name, ref)
						}
					}
				}
			}
			if s.Kind == "read" {
				if _, ok := h.Inputs[s.String("input")]; !ok {
					add(FileName, id, "no input named %s", s.String("input"))
				}
			}
			if s.Kind == "ask" && env.Model != nil {
				if limit, ok := env.Model(s.String("model")); ok {
					if n, _ := s.Get("max_tokens"); n != nil {
						if mt, _ := n.(int64); mt > int64(limit) {
							add(FileName, id, "max_tokens %d is over %s's cap of %d", mt, s.String("model"), limit)
						}
					}
				}
			}
			if c := s.Calls(); c != "" && CheckRef(c) == nil && !required[c] {
				add(FileName, id, "%s steps call %s: add it to requires", k.Label, c)
			}
			if s.Kind == Call {
				if args, ok := s.Get("args"); ok && args != nil {
					if _, isDict := args.(map[string]any); !isDict {
						add(FileName, id, "args must be a dict of argument names to values")
					}
				}
			}
			if s.Kind == "for_each" {
				if inLoop {
					add(FileName, id, "a For each inside a For each is not supported yet")
				}
				vars[s.String("item")] = true
				check(s.Steps(), true)
				delete(vars, s.String("item"))
			}
			if o := s.Output(); o != "" {
				vars[o] = true
			}
		}
	}
	check(fl.Steps, false)
	return out
}

func compileSchema(text string) error {
	var s jsonschema.Schema
	if err := json.Unmarshal([]byte(text), &s); err != nil {
		return err
	}
	_, err := s.Resolve(nil)
	return err
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// stripComments drops # comments and docstrings so a mention of a forbidden
// namespace in prose isn't a call.
func stripComments(src string) string {
	var b strings.Builder
	inTriple := false
	for _, line := range strings.Split(src, "\n") {
		if strings.Count(line, `"""`)%2 == 1 {
			inTriple = !inTriple
			continue
		}
		if inTriple {
			continue
		}
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}
