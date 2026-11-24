package flow

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Dataset is the step kind that saves its items as one collection of a
// dataset the run produces: a JSON Lines file of typed, keyed rows and,
// beside it, the collection's declaration. A host that serves datasets
// (Craggy Studio, the harness) reads those files out of the pack; a host
// that doesn't still runs the step, and the files are plain pack files.
const Dataset = "dataset"

// DatasetDir is where a dataset step writes in the pack: one
// <collection>.jsonl of rows and one <collection>.schema.json declaring
// it, per collection.
const DatasetDir = "datasets/"

// FieldTypes are the types a dataset field may declare: data-services'
// collection vocabulary, the same closed list a craggy.dataset@1 type
// is checked against in the harness.
var FieldTypes = []string{"string", "int", "float", "bool", "time", "date"}

var (
	collectionName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	datasetTypeRef = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*@\d+$`)
)

// DatasetField is one declared field of a dataset collection. Index
// asks whoever holds the rows to index the field, as a
// craggy.dataset@1 type's index flag does.
type DatasetField struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Index bool   `json:"index,omitempty"`
}

// DatasetAlias is one cross-source link key of a collection: values
// at Path, fully matching Pattern when one is given, link rows to
// other sources under Kind - data-services' alias vocabulary.
type DatasetAlias struct {
	Kind    string `json:"kind"`
	Path    string `json:"path"`
	Pattern string `json:"pattern,omitempty"`
}

// DatasetCollection is one collection's declaration, as its
// datasets/<name>.schema.json file in a pack says it. Type is the
// dataset type every collection of one dataset shares, TypeTitle
// that type's title (declared by the steps, so a host generating the
// type never takes a title from the pack around it), Order where
// the collection stands among its type's collections (the step
// order; 0 in a pack written before orders), and File the pack path
// of its rows.
type DatasetCollection struct {
	Type       string         `json:"type,omitempty"`
	TypeTitle  string         `json:"type_title,omitempty"`
	Collection string         `json:"collection"`
	Key        string         `json:"key"`
	Fields     []DatasetField `json:"fields"`
	Aliases    []DatasetAlias `json:"aliases,omitempty"`
	Text       []string       `json:"text,omitempty"`
	Order      int            `json:"order,omitempty"`
	Rows       int            `json:"rows"`
	File       string         `json:"file"`
}

// ParseDatasetFields reads a dataset step's fields argument:
// "slug:string, score:float, pack:string:index". A name with no type
// is a string; a third part names a flag, of which index is the only
// one.
func ParseDatasetFields(s string) ([]DatasetField, error) {
	var out []DatasetField
	seen := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		bits := strings.Split(part, ":")
		if len(bits) > 3 {
			return nil, fmt.Errorf("field %q: a field is name, name:type or name:type:index", part)
		}
		for i := range bits {
			bits[i] = strings.TrimSpace(bits[i])
		}
		name, typ := bits[0], "string"
		if len(bits) > 1 && bits[1] != "" {
			typ = bits[1]
		}
		index := false
		if len(bits) == 3 {
			if bits[2] != "index" {
				return nil, fmt.Errorf("field %q: %q isn't a field flag (index is the only one)", name, bits[2])
			}
			index = true
		}
		if name == "" || strings.HasPrefix(name, "_") {
			return nil, fmt.Errorf("field %q: a field name may not be empty or start with _", part)
		}
		if seen[name] {
			return nil, fmt.Errorf("field %q is named twice", name)
		}
		seen[name] = true
		if !contains(FieldTypes, typ) {
			return nil, fmt.Errorf("field %q: type %q isn't one of %s", name, typ, strings.Join(FieldTypes, ", "))
		}
		out = append(out, DatasetField{Name: name, Type: typ, Index: index})
	}
	return out, nil
}

// ParseDatasetAliases reads a dataset step's aliases argument: a
// JSON object in the aliases spelling of a craggy.dataset@1 type,
// {"cwe": {"path": "cwe", "pattern": "CWE-[0-9]+"}} (a bare string
// is a patternless path). The pattern is JSON's own quoting rather
// than a new mini-language, so a pattern may hold any character.
func ParseDatasetAliases(s string) ([]DatasetAlias, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil, fmt.Errorf("aliases %q: not a JSON object of alias kinds: %v", s, err)
	}
	kinds := make([]string, 0, len(raw))
	for k := range raw {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	var out []DatasetAlias
	for _, kind := range kinds {
		a := DatasetAlias{Kind: kind}
		v := raw[kind]
		if len(v) > 0 && v[0] == '"' {
			if err := json.Unmarshal(v, &a.Path); err != nil {
				return nil, fmt.Errorf("alias %q: %v", kind, err)
			}
		} else {
			var body struct {
				Path    string `json:"path"`
				Pattern string `json:"pattern,omitempty"`
			}
			dec := json.NewDecoder(strings.NewReader(string(v)))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&body); err != nil {
				return nil, fmt.Errorf("alias %q: %v", kind, err)
			}
			a.Path, a.Pattern = body.Path, body.Pattern
		}
		if err := a.check(); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

func (a DatasetAlias) check() error {
	if a.Kind == "" || a.Path == "" {
		return fmt.Errorf("alias %q: an alias needs a kind and a path", a.Kind)
	}
	if a.Pattern != "" {
		if _, err := regexp.Compile(a.Pattern); err != nil {
			return fmt.Errorf("alias %q: its pattern doesn't compile: %v", a.Kind, err)
		}
	}
	return nil
}

// datasetProblems are what a dataset step declares wrongly: a
// collection that isn't a collection name, a missing key (or one
// declared with a type a key can't have), a bad field or type, a
// type that isn't name@version, a bad alias. The key and the text
// fields are paths into the rows, as they are in a craggy.dataset@1
// type: the declared fields are projections, and neither the key
// nor a text path need be one of them.
func datasetProblems(s *Step) []string {
	var out []string
	if c := s.String("collection"); !collectionName.MatchString(c) {
		out = append(out, fmt.Sprintf("collection %q isn't a collection name (a lower-case letter, then letters, digits or _)", c))
	}
	if t := s.String("type"); t != "" && !datasetTypeRef.MatchString(t) {
		out = append(out, fmt.Sprintf("type %q isn't a dataset type (name@version)", t))
	}
	if _, err := ParseDatasetAliases(s.String("aliases")); err != nil {
		out = append(out, err.Error())
	}
	fields, err := ParseDatasetFields(s.String("fields"))
	if err != nil {
		out = append(out, err.Error())
		return out
	}
	key := s.String("key")
	if key == "" {
		out = append(out, "a dataset collection needs a key: the field its rows are told apart by")
	} else {
		for _, f := range fields {
			if f.Name == key && f.Type != "string" && f.Type != "int" {
				out = append(out, fmt.Sprintf("key %q is a %s; a key is a string or an int", key, f.Type))
			}
		}
	}
	return out
}

// ReadDatasetFiles reads the dataset one pack carries: every
// datasets/<collection>.schema.json, with its rows file present,
// every collection sharing one type and one type title, in the
// order their steps ran (a pack written before orders reads in
// file order). A pack with no dataset files reads as none and no
// error.
func ReadDatasetFiles(files map[string]string) ([]DatasetCollection, error) {
	var paths []string
	for p := range files {
		if strings.HasPrefix(p, DatasetDir) && strings.HasSuffix(p, ".schema.json") {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	var out []DatasetCollection
	typ, title := "", ""
	orders := map[int]string{}
	for _, p := range paths {
		var c DatasetCollection
		if err := json.Unmarshal([]byte(files[p]), &c); err != nil {
			return nil, fmt.Errorf("%s: not a dataset declaration: %w", p, err)
		}
		want := DatasetDir + c.Collection + ".schema.json"
		if p != want {
			return nil, fmt.Errorf("%s declares collection %q; its file should be %s", p, c.Collection, want)
		}
		if !collectionName.MatchString(c.Collection) || c.Key == "" || len(c.Fields) == 0 {
			return nil, fmt.Errorf("%s: a collection needs a name, a key and fields", p)
		}
		if c.Type != "" && !datasetTypeRef.MatchString(c.Type) {
			return nil, fmt.Errorf("%s: type %q isn't a dataset type (name@version)", p, c.Type)
		}
		if typ == "" {
			typ = c.Type
		} else if c.Type != typ {
			return nil, fmt.Errorf("%s: type %q, but another collection of this dataset is %q", p, c.Type, typ)
		}
		if c.TypeTitle != "" {
			if title == "" {
				title = c.TypeTitle
			} else if c.TypeTitle != title {
				return nil, fmt.Errorf("%s: type title %q, but another collection of this dataset titles it %q", p, c.TypeTitle, title)
			}
		}
		if c.Order < 0 {
			return nil, fmt.Errorf("%s: its order %d is negative", p, c.Order)
		}
		if c.Order > 0 {
			if other, dup := orders[c.Order]; dup {
				return nil, fmt.Errorf("%s and %s both stand at order %d among this dataset's collections", p, other, c.Order)
			}
			orders[c.Order] = p
		}
		seenField := map[string]bool{}
		for _, f := range c.Fields {
			if f.Name == "" || strings.HasPrefix(f.Name, "_") {
				return nil, fmt.Errorf("%s: a field name may not be empty or start with _", p)
			}
			if seenField[f.Name] {
				return nil, fmt.Errorf("%s: field %q is named twice", p, f.Name)
			}
			seenField[f.Name] = true
			if !contains(FieldTypes, f.Type) {
				return nil, fmt.Errorf("%s: field %q has type %q", p, f.Name, f.Type)
			}
			if f.Name == c.Key && f.Type != "string" && f.Type != "int" {
				return nil, fmt.Errorf("%s: key %q is a %s; a key is a string or an int", p, c.Key, f.Type)
			}
		}
		seenAlias := map[string]bool{}
		for _, a := range c.Aliases {
			if err := a.check(); err != nil {
				return nil, fmt.Errorf("%s: %v", p, err)
			}
			if seenAlias[a.Kind] {
				return nil, fmt.Errorf("%s: alias %q is named twice", p, a.Kind)
			}
			seenAlias[a.Kind] = true
		}
		if c.File == "" {
			c.File = DatasetDir + c.Collection + ".jsonl"
		}
		if _, ok := files[c.File]; !ok {
			return nil, fmt.Errorf("%s: its rows file %s is not in the pack", p, c.File)
		}
		out = append(out, c)
	}
	if len(orders) > 0 {
		// The collections in their steps' order, so the dataset
		// type a host generates from them stands as its author
		// declared it. An unordered collection (order 0) keeps its
		// file-order place after the ordered ones.
		sort.SliceStable(out, func(i, j int) bool {
			a, b := out[i].Order, out[j].Order
			if a == 0 {
				a = 1 << 30
			}
			if b == 0 {
				b = 1 << 30
			}
			return a < b
		})
	}
	if title != "" {
		for i := range out {
			out[i].TypeTitle = title
		}
	}
	return out, nil
}

// DatasetTypeOf is the type the flow's dataset steps declare, "" when
// the flow saves no dataset or names none. Two steps naming different
// types is a problem the caller reports.
func DatasetSteps(fl *Flow) []*Step {
	var out []*Step
	var walk func(steps []*Step)
	walk = func(steps []*Step) {
		for _, s := range steps {
			if s.Kind == Dataset {
				out = append(out, s)
			}
			walk(s.Steps())
		}
	}
	walk(fl.Steps)
	return out
}
