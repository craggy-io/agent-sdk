package agentdef_test

import (
	"strings"
	"testing"
	"testing/fstest"
)

// A skill's for_tools names the tools it explains, one pattern or a
// list; a resolution carries it only once AttachToolSkills sees one.
func TestSkillForTools(t *testing.T) {
	l := open(t, fstest.MapFS{
		"bundle.yaml": file(`apiVersion: craggy.io/bundle/v1
metadata: {id: agentsdk.fortools, version: 1.0.0}
contributions:
  - {id: a, kind: agentsdk.agent@1, path: a.star}
  - {id: boards, kind: agentsdk.skill@1, path: boards/SKILL.md}
  - {id: notes, kind: agentsdk.skill@1, path: notes/SKILL.md}
`),
		"a.star":          file(`agent(name = "a", prompt = [{"p": "Hi."}])`),
		"boards/SKILL.md": file("---\nname: boards\ndescription: Boards.\nfor_tools: [\"boards.*\", boards_*]\n---\nPlace cards.\n"),
		"notes/SKILL.md":  file("---\nname: notes\ndescription: Notes.\nfor_tools: notes.write\n---\nWrite notes.\n"),
	})
	if sk, _ := l.Skill("boards"); len(sk.ForTools) != 2 || sk.ForTools[0] != "boards.*" || sk.ForTools[1] != "boards_*" {
		t.Fatalf("boards for_tools = %q", sk.ForTools)
	}
	r, err := l.Resolve("a", nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.SystemPrompt != "Hi." || len(r.Skills) != 0 {
		t.Fatalf("skills before any tools: %v %q", r.Skills, r.SystemPrompt)
	}
	if got := r.AttachToolSkills([]string{"echo", "boards_place", "boards.move"}); len(got) != 1 || got[0] != "boards" {
		t.Fatalf("attached %v", got)
	}
	if r.SkillBodies["boards"] != "Place cards.\n" || r.Provenance["skills.boards"] != "tool boards_place" || !strings.HasSuffix(r.SystemPrompt, "- `boards`: Boards.") {
		t.Fatalf("resolved = %+v", r)
	}
	// Candidates are used once: a run attaches against its one view.
	if got := r.AttachToolSkills([]string{"notes.write"}); len(got) != 0 {
		t.Fatalf("a second attach added %v", got)
	}
}
