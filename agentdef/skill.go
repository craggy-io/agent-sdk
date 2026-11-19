package agentdef

import (
	"fmt"
	"path"
	"strings"
)

// parseSkill splits SKILL.md front matter (--- ... ---) from the body.
// Front matter is flat "key: value" lines; only name, description and
// for_tools are read, so no YAML parser is needed. id is the
// contribution id: the skill's name, which a name in the front matter
// must repeat.
func parseSkill(data []byte, id, file string) (*Skill, error) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return nil, fmt.Errorf("agentdef: skill %s: missing front matter", file)
	}
	rest := text[len("---\n"):]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return nil, fmt.Errorf("agentdef: skill %s: unterminated front matter", file)
	}
	var name, description string
	var forTools []string
	for _, line := range strings.Split(rest[:end], "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		val = unquote(strings.TrimSpace(val))
		switch strings.TrimSpace(key) {
		case "name":
			name = val
		case "description":
			description = val
		case "for_tools":
			var err error
			if forTools, err = toolPatterns(val); err != nil {
				return nil, fmt.Errorf("agentdef: skill %s: for_tools: %w", file, err)
			}
		}
	}
	body := rest[end+len("\n---"):]
	body = strings.TrimPrefix(body, "\n")
	if name != "" && name != id {
		return nil, fmt.Errorf("agentdef: skill %s: front matter names %q but bundle.yaml names the contribution %q; they must match", file, name, id)
	}
	return &Skill{Name: id, Description: description, Body: body, ForTools: forTools}, nil
}

// unquote strips one pair of matching single or double quotes.
func unquote(val string) string {
	if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
		return val[1 : len(val)-1]
	}
	return val
}

// toolPatterns reads a for_tools value: one pattern, or a flow list such
// as "[boards.*, boards_*]". Each is a tool-name glob as tool filters
// use; "*" alone is refused, since a skill for every tool is one an
// agent should name.
func toolPatterns(val string) ([]string, error) {
	val = strings.TrimSuffix(strings.TrimPrefix(val, "["), "]")
	var out []string
	for _, p := range strings.Split(val, ",") {
		p = unquote(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		if p == "*" {
			return nil, fmt.Errorf("%q would give the skill to every agent; name the tools it explains, or list the skill in the agents that need it", p)
		}
		if _, err := path.Match(p, ""); err != nil {
			return nil, fmt.Errorf("%q is not a tool-name pattern: %w", p, err)
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("names no tools")
	}
	return out, nil
}
