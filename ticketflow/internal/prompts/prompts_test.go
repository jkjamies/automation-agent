package prompts

import (
	"strings"
	"testing"
)

func TestEveryPromptLoads(t *testing.T) {
	for _, name := range []string{"planner.md", "implementer.md", "reviewer.md"} {
		if strings.TrimSpace(Get(name)) == "" {
			t.Errorf("%s is empty", name)
		}
	}
}

func TestRender(t *testing.T) {
	out, err := Render("jira_bounce.md", map[string]any{"Problems": []string{"needs AC"}, "TriggerLabel": "agent-ready"})
	if err != nil || !strings.Contains(out, "- needs AC") || !strings.Contains(out, `"agent-ready"`) {
		t.Errorf("Render = %q, %v", out, err)
	}
	if _, err := Render("missing.md", nil); err == nil {
		t.Error("rendering a missing template succeeded")
	}
}

func TestGetPanicsOnUnknownName(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Get(missing) did not panic")
		}
	}()
	Get("missing.md")
}
