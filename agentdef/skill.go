package agentdef

import (
	"fmt"
	"strings"
)

// parseSkill splits SKILL.md front matter (--- ... ---) from the body.
// Front matter is flat "key: value" lines; only name and description
// are read, so no YAML parser is needed. id is the contribution id: the
// skill's name, which a name in the front matter must repeat.
func parseSkill(data []byte, id, path string) (*Skill, error) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return nil, fmt.Errorf("agentdef: skill %s: missing front matter", path)
	}
	rest := text[len("---\n"):]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return nil, fmt.Errorf("agentdef: skill %s: unterminated front matter", path)
	}
	var name, description string
	for _, line := range strings.Split(rest[:end], "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
			val = val[1 : len(val)-1]
		}
		switch strings.TrimSpace(key) {
		case "name":
			name = val
		case "description":
			description = val
		}
	}
	body := rest[end+len("\n---"):]
	body = strings.TrimPrefix(body, "\n")
	if name != "" && name != id {
		return nil, fmt.Errorf("agentdef: skill %s: front matter names %q but bundle.yaml names the contribution %q; they must match", path, name, id)
	}
	name = id
	return &Skill{Name: name, Description: description, Body: body}, nil
}
