// Package flow is what a pipeline is: a bundle of files with one
// pipeline.star, read as a header (title, inputs, requires) and a flow of
// steps that the Starlark library Lib runs, each over one capability. It
// reads, edits, checks and describes flows; hosts run them (docs/flow.md).
package flow

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"go.starlark.net/syntax"

	"github.com/craggy-io/script-services/dialect"
)

// FileName is the pipeline's program.
const FileName = "pipeline.star"

// Module is what a pipeline loads Lib as.
const Module = "@craggy/flow.star"

// InputsFile holds a test run's inputs: {"<input>": [rows...]}.
const InputsFile = "testdata/inputs.json"

// Bundle is a pipeline's files, by path: pipeline.star, prompts/*.md,
// schemas/*.json, lib/*.star, testdata/*, bundle.yaml.
type Bundle map[string]string

// Limits on a bundle.
const (
	MaxFiles     = 200
	MaxFileBytes = 256 << 10
)

// Digest is the bundle's content address: a SHA-256 over its paths and
// files in path order. Two bundles with the same files have the same digest.
func (b Bundle) Digest() string {
	h := sha256.New()
	for _, p := range b.Paths() {
		fmt.Fprintf(h, "%d:%s%d:", len(p), p, len(b[p]))
		h.Write([]byte(b[p]))
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// Paths lists the files in order.
func (b Bundle) Paths() []string {
	out := make([]string, 0, len(b))
	for p := range b {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// ReadDir reads a folder as a bundle, with / paths and \n line ends,
// leaving out every file or folder whose name starts with a dot.
func ReadDir(dir string) (Bundle, error) {
	b := Bundle{}
	err := filepath.WalkDir(dir, func(at string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if at != dir && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, at)
		if err != nil {
			return err
		}
		if len(b) >= MaxFiles {
			return fmt.Errorf("flow: a pipeline holds at most %d files", MaxFiles)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() > MaxFileBytes {
			return fmt.Errorf("flow: %s is over %d bytes", filepath.ToSlash(rel), MaxFileBytes)
		}
		raw, err := os.ReadFile(at)
		if err != nil {
			return err
		}
		b[filepath.ToSlash(rel)] = strings.ReplaceAll(string(raw), "\r\n", "\n")
		return nil
	})
	return b, err
}

// Flow is what a run does: every file but testdata/, so adding or
// recording tests doesn't change it.
func (b Bundle) Flow() Bundle {
	out := make(Bundle, len(b))
	for k, v := range b {
		if !strings.HasPrefix(k, "testdata/") {
			out[k] = v
		}
	}
	return out
}

// Clone copies a bundle.
func (b Bundle) Clone() Bundle {
	out := make(Bundle, len(b))
	for k, v := range b {
		out[k] = v
	}
	return out
}

// CheckPath refuses a path that escapes the bundle or names a hidden file.
func CheckPath(p string) error {
	if p == "" || strings.ContainsAny(p, "\\\x00:") || strings.HasPrefix(p, "/") || path.Clean(p) != p || strings.HasPrefix(p, "..") {
		return fmt.Errorf("bad file path %q", p)
	}
	for _, part := range strings.Split(p, "/") {
		if strings.HasPrefix(part, ".") {
			return fmt.Errorf("bad file path %q", p)
		}
	}
	return nil
}

// Input is a named source a pipeline declares (flow.input).
type Input struct {
	Dataset string         `json:"dataset"`
	Key     string         `json:"key"`
	Where   string         `json:"where,omitempty"`
	Recipe  string         `json:"recipe,omitempty"`
	Params  map[string]any `json:"params,omitempty"`
	Limit   int64          `json:"limit,omitempty"`
}

// Header is what pipeline(...) declares.
type Header struct {
	Title       string           `json:"title"`
	Description string           `json:"description,omitempty"`
	Inputs      map[string]Input `json:"inputs"`
	InputOrder  []string         `json:"inputOrder"`
	Requires    []string         `json:"requires"`
}

// ReadHeader reads the pipeline(...) call at the top of pipeline.star.
func ReadHeader(src []byte) (Header, error) {
	f, err := dialect.Parse(FileName, src)
	if err != nil {
		return Header{}, err
	}
	for _, stmt := range f.Stmts {
		es, ok := stmt.(*syntax.ExprStmt)
		if !ok {
			continue
		}
		call, ok := es.X.(*syntax.CallExpr)
		if !ok {
			continue
		}
		if id, ok := call.Fn.(*syntax.Ident); !ok || id.Name != "pipeline" {
			continue
		}
		return header(call)
	}
	return Header{}, fmt.Errorf("pipeline.star: no pipeline(...) declaration")
}

func header(call *syntax.CallExpr) (Header, error) {
	h := Header{Inputs: map[string]Input{}}
	for _, a := range call.Args {
		bin, ok := a.(*syntax.BinaryExpr)
		if !ok || bin.Op != syntax.EQ {
			return h, posErr(a, "pipeline() takes keyword arguments")
		}
		name := bin.X.(*syntax.Ident).Name
		switch name {
		case "inputs":
			d, ok := bin.Y.(*syntax.DictExpr)
			if !ok {
				return h, posErr(bin.Y, "inputs must be a dict of flow.input(...)")
			}
			for _, el := range d.List {
				kv := el.(*syntax.DictEntry)
				k, err := literal(kv.Key)
				key, ok := k.(string)
				if err != nil || !ok {
					return h, posErr(kv.Key, "input names must be strings")
				}
				in, err := readInput(kv.Value)
				if err != nil {
					return h, err
				}
				h.Inputs[key] = in
				h.InputOrder = append(h.InputOrder, key)
			}
		default:
			v, err := literal(bin.Y)
			if err != nil {
				return h, err
			}
			switch name {
			case "title":
				h.Title, _ = v.(string)
			case "description":
				h.Description, _ = v.(string)
			case "requires":
				list, _ := v.([]any)
				for _, r := range list {
					if s, ok := r.(string); ok {
						h.Requires = append(h.Requires, s)
					}
				}
			}
		}
	}
	return h, nil
}

func readInput(e syntax.Expr) (Input, error) {
	call, ok := e.(*syntax.CallExpr)
	dot, _ := callDot(call)
	if !ok || dot != "flow.input" {
		return Input{}, posErr(e, "an input must be flow.input(...)")
	}
	in := Input{}
	for i, a := range call.Args {
		if bin, ok := a.(*syntax.BinaryExpr); ok && bin.Op == syntax.EQ {
			v, err := literal(bin.Y)
			if err != nil {
				return in, err
			}
			switch bin.X.(*syntax.Ident).Name {
			case "dataset":
				in.Dataset, _ = v.(string)
			case "key":
				in.Key, _ = v.(string)
			case "where":
				in.Where, _ = v.(string)
			case "recipe":
				in.Recipe, _ = v.(string)
			case "params":
				in.Params, _ = v.(map[string]any)
			case "limit":
				in.Limit, _ = v.(int64)
			}
			continue
		}
		v, err := literal(a)
		if err != nil {
			return in, err
		}
		s, _ := v.(string)
		switch i {
		case 0:
			in.Dataset = s
		case 1:
			in.Key = s
		}
	}
	if in.Dataset == "" || in.Key == "" {
		return in, posErr(e, "flow.input needs a dataset and a key")
	}
	return in, nil
}

func callDot(call *syntax.CallExpr) (string, bool) {
	if call == nil {
		return "", false
	}
	dot, ok := call.Fn.(*syntax.DotExpr)
	if !ok {
		return "", false
	}
	x, ok := dot.X.(*syntax.Ident)
	if !ok {
		return "", false
	}
	return x.Name + "." + dot.Name.Name, true
}

// SetHeader rewrites the pipeline(...) call, keeping the rest of the file.
func SetHeader(src []byte, h Header) ([]byte, error) {
	f, err := dialect.Parse(FileName, src)
	if err != nil {
		return nil, err
	}
	lines := lineOffsets(src)
	for _, stmt := range f.Stmts {
		es, ok := stmt.(*syntax.ExprStmt)
		if !ok {
			continue
		}
		call, ok := es.X.(*syntax.CallExpr)
		if id, isID := callIdent(call); !ok || !isID || id != "pipeline" {
			continue
		}
		a, b := es.Span()
		start, end := offset(src, lines, a), offset(src, lines, b)
		out := append([]byte{}, src[:start]...)
		out = append(out, RenderHeader(h)...)
		return append(out, src[end:]...), nil
	}
	return nil, fmt.Errorf("pipeline.star: no pipeline(...) declaration")
}

func callIdent(call *syntax.CallExpr) (string, bool) {
	if call == nil {
		return "", false
	}
	id, ok := call.Fn.(*syntax.Ident)
	if !ok {
		return "", false
	}
	return id.Name, true
}

// RenderHeader writes pipeline(...) canonically.
func RenderHeader(h Header) string {
	var b strings.Builder
	b.WriteString("pipeline(\n")
	b.WriteString(indentUnit + "title = " + quote(h.Title) + ",\n")
	if h.Description != "" {
		b.WriteString(indentUnit + "description = " + quote(h.Description) + ",\n")
	}
	names := h.InputOrder
	if len(names) != len(h.Inputs) {
		names = names[:0:0]
		for k := range h.Inputs {
			names = append(names, k)
		}
		sort.Strings(names)
	}
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, quote(n)+": "+renderInput(h.Inputs[n]))
	}
	b.WriteString(indentUnit + "inputs = {" + strings.Join(parts, ", ") + "},\n")
	reqs := make([]any, len(h.Requires))
	for i, r := range h.Requires {
		reqs[i] = r
	}
	b.WriteString(indentUnit + "requires = " + renderValue(reqs) + ",\n")
	b.WriteString(")")
	return b.String()
}

func renderInput(in Input) string {
	s := "flow.input(" + quote(in.Dataset) + ", key = " + quote(in.Key)
	if in.Where != "" {
		s += ", where = " + quote(in.Where)
	}
	if in.Recipe != "" {
		s += ", recipe = " + quote(in.Recipe)
	}
	if len(in.Params) > 0 {
		s += ", params = " + renderValue(in.Params)
	}
	if in.Limit > 0 {
		s += ", limit = " + renderValue(in.Limit)
	}
	return s + ")"
}

// Template is a new pipeline.star: the header and an empty flow.
func Template(h Header) string {
	return "load(" + quote(Module) + ", \"flow\")\n\n" + RenderHeader(h) + "\n\nSTEPS = []\n\ndef run(ctx):\n    return flow.run(ctx, STEPS)\n"
}
