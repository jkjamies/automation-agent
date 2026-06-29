package reviewer

import (
	"context"
	"iter"
	"strings"
	"testing"

	"google.golang.org/adk/model"

	"automation-agent/internal/agent/setup"
	"automation-agent/internal/githubapi"
)

// fakeLLM is a model.LLM that returns one canned response, so the fan-out wiring can be driven
// without a real model. Structure/glue tests assert orchestration, never LLM output content.
type fakeLLM struct{ json string }

func (fakeLLM) Name() string { return "fake" }

func (m fakeLLM) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(setup.FinalTextResponse(m.json), nil)
	}
}

func reviewEngine(json string, mut ...func(*Deps)) *Engine {
	llm := fakeLLM{json: json}
	d := Deps{Enabled: true, GH: &fakeGH{}, BaseLLM: llm, CodeLLM: llm, MinConfidence: 0.6}
	for _, m := range mut {
		m(&d)
	}
	return NewEngine(d)
}

func TestReviewPipeline(t *testing.T) {
	canned := `[{"file":"main.go","line":10,"dimension":"runtime_safety","severity":"major","message":"nil deref","confidence":0.9}]`
	e := reviewEngine(canned)
	files := []githubapi.PRFile{{Path: "main.go", Patch: "@@ -1 +1 @@\n+x", Status: "modified"}}

	card, _, err := e.review(context.Background(), files)
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	// Every lens + glue returns the same finding (same fingerprint), so dedup collapses it to
	// one; one runtime_safety major scores the dimension — and thus overall — yellow.
	if card.total != 1 {
		t.Errorf("total = %d, want 1 after dedup", card.total)
	}
	if card.overall != levelYellow {
		t.Errorf("overall = %v, want yellow", card.overall)
	}
}

func TestReviewPipelineDropsLowConfidence(t *testing.T) {
	// All findings below the 0.6 gate -> dropped -> green, no findings.
	canned := `[{"file":"main.go","line":10,"dimension":"security","severity":"critical","message":"x","confidence":0.2}]`
	card, _, err := reviewEngine(canned).review(context.Background(), []githubapi.PRFile{{Path: "main.go", Patch: "+x"}})
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if card.total != 0 || card.overall != levelGreen {
		t.Errorf("low-confidence critical leaked: total=%d overall=%v", card.total, card.overall)
	}
}

func TestReviewPipelineEmptyFindings(t *testing.T) {
	card, _, err := reviewEngine("[]").review(context.Background(), []githubapi.PRFile{{Path: "main.go", Patch: "+x"}})
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if card.total != 0 || card.overall != levelGreen {
		t.Errorf("empty review = total %d overall %v, want clean green", card.total, card.overall)
	}
}

// Kickoff drives the full enabled path through to a scored review on a normal PR.
func TestKickoffReviewPath(t *testing.T) {
	canned := `[{"file":"main.go","line":1,"dimension":"performance","severity":"medium","message":"slow","confidence":0.9}]`
	llm := fakeLLM{json: canned}
	gh := &fakeGH{files: []githubapi.PRFile{{Path: "main.go", Patch: "@@\n+x", Status: "modified"}}}
	e := NewEngine(Deps{Enabled: true, GH: gh, BaseLLM: llm, CodeLLM: llm, MinConfidence: 0.6, SkipDrafts: true})
	body := `{"action":"opened","pull_request":{"number":7,"head":{"ref":"feature/x"},"base":{"ref":"main"}},"repository":{"full_name":"o/r"}}`
	if err := e.Kickoff(context.Background(), []byte(body)); err != nil {
		t.Fatalf("Kickoff: %v", err)
	}
	if gh.calls != 1 {
		t.Errorf("ListPRFiles calls = %d, want 1", gh.calls)
	}
}

func TestFormatDiff(t *testing.T) {
	out := formatDiff([]githubapi.PRFile{
		{Path: "a.go", Status: "modified", Patch: "@@ -1 +1 @@\n-old\n+new"},
		{Path: "logo.png", Status: "added", Patch: ""},
	})
	if !strings.Contains(out, "### a.go (modified)") || !strings.Contains(out, "+new") {
		t.Errorf("patch file not rendered:\n%s", out)
	}
	if !strings.Contains(out, "### logo.png (added)") || !strings.Contains(out, "(no textual diff available)") {
		t.Errorf("patchless file not noted:\n%s", out)
	}
}
