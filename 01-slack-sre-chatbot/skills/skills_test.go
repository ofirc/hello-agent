package skills

import (
	"strings"
	"testing"

	"google.golang.org/adk/v2/tool/skilltoolset/skill"
)

func readSkill(t *testing.T, dir string) string {
	t.Helper()
	b, err := FS.ReadFile(dir + "/SKILL.md")
	if err != nil {
		t.Fatalf("read %s/SKILL.md: %v", dir, err)
	}
	return string(b)
}

func TestEmbeddedSkillsAreValid(t *testing.T) {
	for _, dir := range []string{"drift-triage", "gitops-fix"} {
		t.Run(dir, func(t *testing.T) {
			fm, _, err := skill.ParseBytes([]byte(readSkill(t, dir)))
			if err != nil {
				t.Fatalf("ParseBytes: %v", err)
			}
			if err := skill.Validate(fm); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if fm.Name != dir {
				t.Errorf("name = %q, want %q", fm.Name, dir)
			}
		})
	}
}

func TestSkillsMentionTheToolsTheAgentHas(t *testing.T) {
	fix := readSkill(t, "gitops-fix")
	for _, want := range []string{"propose_fix", "decline_fix", "get_overlay"} {
		if !strings.Contains(fix, want) {
			t.Errorf("gitops-fix/SKILL.md does not mention %q", want)
		}
	}
	if !strings.Contains(readSkill(t, "drift-triage"), "/data") {
		t.Error("drift-triage/SKILL.md does not mention /data")
	}
}

// The debug task text says "Argo CD was asked to refresh these apps just before this
// run" when the handler already refreshed; the skill must key its own refresh off that.
func TestTriageSkipsARefreshTheHandlerAlreadyDid(t *testing.T) {
	triage := readSkill(t, "drift-triage")
	for _, want := range []string{"just refreshed", "refresh_app", "in parallel"} {
		if !strings.Contains(triage, want) {
			t.Errorf("drift-triage/SKILL.md does not mention %q", want)
		}
	}
}
