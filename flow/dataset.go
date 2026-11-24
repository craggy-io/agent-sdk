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

// DatasetField is one declared field of a dataset collection.
type DatasetField struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// DatasetCollection is one collection's declaration, as its
// datasets/<name>.schema.json file in a pack says it. Type is the
// dataset type every collection of one dataset shares; File is the
// pack path of its rows.
type DatasetCollection struct {
	Type       string         `json:"type,omitempty"`
	Collection string         `json:"collection"`
	Key        string         `json:"key"`
	Fields     []DatasetField `json:"fields"`
	Text       []string       `json:"text,omitempty"`
	Rows       int            `json:"rows"`
	File       string         `json:"file"`
}

// ParseDatasetFields reads a dataset step's fields argument:
// "slug:string, score:float". A name with no type is a string.
func ParseDatasetFields(s string) ([]DatasetField, error) {
	var out []DatasetField
	seen := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, typ, _ := strings.Cut(part, ":")
		name, typ = strings.TrimSpace(name), strings.TrimSpace(typ)
		if typ == "" {
			typ = "string"
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
		out = append(out, DatasetField{Name: name, Type: typ})
	}
	return out, nil
}

// datasetProblems are what a dataset step declares wrongly: a
// collection that isn't a collection name, a key that isn't one of its
// declared fields, a bad field or type, a type that isn't name@version,
// a text field it doesn't declare.
func datasetProblems(s *Step) []string {
	var out []string
	if c := s.String("collection"); !collectionName.MatchString(c) {
		out = append(out, fmt.Sprintf("collection %q isn't a collection name (a lower-case letter, then letters, digits or _)", c))
	}
	if t := s.String("type"); t != "" && !datasetTypeRef.MatchString(t) {
		out = append(out, fmt.Sprintf("type %q isn't a dataset type (name@version)", t))
	}
	fields, err := ParseDatasetFields(s.String("fields"))
	if err != nil {
		out = append(out, err.Error())
		return out
	}
	key := s.String("key")
	if key == "" {
		out = append(out, "a dataset collection needs a key: the field its rows are told apart by")
	} else if len(fields) > 0 {
		found := false
		for _, f := range fields {
			if f.Name == key {
				found = true
				if f.Type != "string" && f.Type != "int" {
					out = append(out, fmt.Sprintf("key %q is a %s; a key is a string or an int", key, f.Type))
				}
			}
		}
		if !found {
			out = append(out, fmt.Sprintf("key %q isn't one of the fields named", key))
		}
	}
	if len(fields) > 0 {
		for _, t := range strings.Split(s.String("text"), ",") {
			if t = strings.TrimSpace(t); t == "" {
				continue
			}
			found := false
			for _, f := range fields {
				if f.Name == t {
					found = true
				}
			}
			if !found {
				out = append(out, fmt.Sprintf("text field %q isn't one of the fields named", t))
			}
		}
	}
	return out
}

// ReadDatasetFiles reads the dataset one pack carries: every
// datasets/<collection>.schema.json, with its rows file present and
// every collection sharing one type. A pack with no dataset files
// reads as none and no error.
func ReadDatasetFiles(files map[string]string) ([]DatasetCollection, error) {
	var paths []string
	for p := range files {
		if strings.HasPrefix(p, DatasetDir) && strings.HasSuffix(p, ".schema.json") {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	var out []DatasetCollection
	typ := ""
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
		keyFound := false
		for _, f := range c.Fields {
			if !contains(FieldTypes, f.Type) {
				return nil, fmt.Errorf("%s: field %q has type %q", p, f.Name, f.Type)
			}
			if f.Name == c.Key {
				keyFound = true
			}
		}
		if !keyFound {
			return nil, fmt.Errorf("%s: key %q isn't one of its fields", p, c.Key)
		}
		if c.File == "" {
			c.File = DatasetDir + c.Collection + ".jsonl"
		}
		if _, ok := files[c.File]; !ok {
			return nil, fmt.Errorf("%s: its rows file %s is not in the pack", p, c.File)
		}
		out = append(out, c)
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
