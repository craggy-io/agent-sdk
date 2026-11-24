package flow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/craggy-io/foundation-kit/canonjson"
)

type datasetTypeFile struct {
	Type        string                `json:"type"`
	Title       string                `json:"title,omitempty"`
	Collections []datasetTypeCollFile `json:"collections"`
}

type datasetTypeCollFile struct {
	Name    string                      `json:"name"`
	Key     string                      `json:"key"`
	Aliases map[string]datasetTypeAlias `json:"aliases,omitempty"`
	Fields  map[string]datasetTypeField `json:"fields,omitempty"`
	Text    []string                    `json:"text,omitempty"`
}

type datasetTypeField struct {
	Type  string `json:"type"`
	Index bool   `json:"index,omitempty"`
}

type datasetTypeAlias struct {
	Path    string `json:"path"`
	Pattern string `json:"pattern,omitempty"`
}

// DatasetTypeJSON generates the type document for one pack. A schema-file
// pipeline carries its original type intact, including attributes that have
// no equivalent in an inline step. All collections must agree on that type.
func DatasetTypeJSON(cols []DatasetCollection) ([]byte, error) {
	if len(cols) == 0 || cols[0].Type == "" {
		return nil, fmt.Errorf("dataset has no declared type")
	}
	f := datasetTypeFile{Type: cols[0].Type, Title: cols[0].TypeTitle}
	var exact []byte
	for _, c := range cols {
		if c.Type != f.Type || c.TypeTitle != f.Title {
			return nil, fmt.Errorf("inconsistent dataset types")
		}
		if (len(cols[0].DeclaredSchema) > 0) != (len(c.DeclaredSchema) > 0) {
			return nil, fmt.Errorf("%s: mixed schema files and inline declarations", c.Collection)
		}
		if len(c.DeclaredSchema) > 0 {
			body, err := canonjson.Normalize(c.DeclaredSchema)
			if err != nil {
				return nil, err
			}
			if exact != nil && !bytes.Equal(body, exact) {
				return nil, fmt.Errorf("%s: inconsistent schema files", c.Collection)
			}
			exact = body
		}
		coll := datasetTypeCollFile{Name: c.Collection, Key: c.Key, Text: c.Text, Fields: map[string]datasetTypeField{}, Aliases: map[string]datasetTypeAlias{}}
		for _, field := range c.Fields {
			coll.Fields[field.Name] = datasetTypeField{Type: field.Type, Index: field.Index}
		}
		for _, a := range c.Aliases {
			coll.Aliases[a.Kind] = datasetTypeAlias{Path: a.Path, Pattern: a.Pattern}
		}
		f.Collections = append(f.Collections, coll)
	}
	if exact == nil {
		return json.MarshalIndent(f, "", "  ")
	}
	var schema struct {
		Type        string
		Title       string
		Collections []struct {
			Name    string
			Key     string
			Text    []string
			Fields  map[string]datasetTypeField
			Aliases json.RawMessage
		}
	}
	if err := json.Unmarshal(exact, &schema); err != nil {
		return nil, err
	}
	if schema.Type != f.Type || schema.Title != f.Title || len(schema.Collections) != len(cols) {
		return nil, fmt.Errorf("dataset does not cover the declared schema %s", f.Type)
	}
	byName := map[string]datasetTypeCollFile{}
	for _, c := range f.Collections {
		byName[c.Name] = c
	}
	for _, c := range schema.Collections {
		projected, ok := byName[c.Name]
		if !ok || c.Key != projected.Key || !slices.Equal(c.Text, projected.Text) || len(c.Fields) != len(projected.Fields) {
			return nil, fmt.Errorf("collection %s differs from the declared schema", c.Name)
		}
		for k, v := range c.Fields {
			if actual, ok := projected.Fields[k]; !ok || actual != v {
				return nil, fmt.Errorf("field %s.%s differs from the declared schema", c.Name, k)
			}
		}
		aliases, err := ParseDatasetAliases(string(c.Aliases))
		if err != nil {
			return nil, err
		}
		if len(aliases) != len(projected.Aliases) {
			return nil, fmt.Errorf("aliases of %s differ from the declared schema", c.Name)
		}
		for _, a := range aliases {
			if v, ok := projected.Aliases[a.Kind]; !ok || a.Path != v.Path || a.Pattern != v.Pattern {
				return nil, fmt.Errorf("alias %s.%s differs from the declared schema", c.Name, a.Kind)
			}
		}
		delete(byName, c.Name)
	}
	return exact, nil
}
