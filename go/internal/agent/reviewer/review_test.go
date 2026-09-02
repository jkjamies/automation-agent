package reviewer

import (
	"context"
	"iter"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"

	"automation-agent/internal/agent/setup"
	"automation-agent/internal/githubapi"
)

// fakeLLM is a model.LLM that returns one canned response, so the fan-out wiring can be driven
// without a real model. Structure/glue tests assert orchestration, never LLM output content.
// When in/out are set the response also reports that token usage, as a real adapter's final
// chunk does.
type fakeLLM struct {
	json    string
	in, out int
}

func (fakeLLM) Name() string { return "fake" }

func (m fakeLLM) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		resp := setup.FinalTextResponse(m.json)
		if m.in > 0 || m.out > 0 {
			setup.ReportUsage(resp, m.in, m.out)
		}
		yield(resp, nil)
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

	out, err := e.review(context.Background(), files, nil, nil)
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	card := out.card
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
	out, err := reviewEngine(canned).review(context.Background(), []githubapi.PRFile{{Path: "main.go", Patch: "+x"}}, nil, nil)
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if card := out.card; card.total != 0 || card.overall != levelGreen {
		t.Errorf("low-confidence critical leaked: total=%d overall=%v", card.total, card.overall)
	}
}

func TestReviewPipelineEmptyFindings(t *testing.T) {
	out, err := reviewEngine("[]").review(context.Background(), []githubapi.PRFile{{Path: "main.go", Patch: "+x"}}, nil, nil)
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if card := out.card; card.total != 0 || card.overall != levelGreen {
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

// A diff is per-event text the reviewer bakes into the agents' instructions. The ADK treats a
// plain Instruction string as a template — every `{identifier}` is a session-state lookup that
// errors when the key is absent — so code that merely mentions a placeholder (an f-string, a
// route pattern, a templated config) must not be able to fail the review. The instruction is
// therefore handed over through a provider, which the ADK does not template.
func TestReviewInstructionIsNotTemplated(t *testing.T) {
	files := []githubapi.PRFile{{
		Path:   "app.py",
		Status: "modified",
		Patch:  "@@ -1 +1,2 @@\n+greeting = f\"hello {user}\"\n+route = \"/items/{item_id}\"",
	}}
	if _, err := reviewEngine("[]").review(context.Background(), files, nil, nil); err != nil {
		t.Fatalf("review over a diff containing {placeholders}: %v", err)
	}
}

// The review reports one status row per lens that ran — every selected category, then glue — with
// the model it ran on and the usage its agent reported, attributed by agent name.
func TestReviewReportsLensStats(t *testing.T) {
	files := []githubapi.PRFile{{Path: "main.go", Status: "modified", Patch: "@@ -1 +1 @@\n+x"}}
	e := reviewEngine("[]", func(d *Deps) {
		d.BaseLLM = fakeLLM{json: "[]", in: 50, out: 4}
		d.CodeLLM = fakeLLM{json: "[]", in: 80, out: 6}
	})
	out, err := e.review(context.Background(), files, nil, nil)
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	cats := selectCategories(files)
	if len(out.lenses) != len(cats)+1 {
		t.Fatalf("lenses = %d, want %d categories + glue", len(out.lenses), len(cats))
	}
	for i, c := range cats {
		if out.lenses[i].lens.name != c.name {
			t.Errorf("lens[%d] = %s, want %s (category order)", i, out.lenses[i].lens.name, c.name)
		}
	}
	if last := out.lenses[len(out.lenses)-1]; last.lens.name != glueLens.name {
		t.Errorf("last lens = %s, want glue", last.lens.name)
	}
	for _, l := range out.lenses {
		wantIn, wantOut := 50, 4
		if l.lens.tier == tierCode {
			wantIn, wantOut = 80, 6
		}
		if !l.ran || !l.usage || l.tokensIn != wantIn || l.tokensOut != wantOut {
			t.Errorf("%s: ran %v usage %v tokens %d/%d, want ran, usage, %d/%d", l.lens.name, l.ran, l.usage, l.tokensIn, l.tokensOut, wantIn, wantOut)
		}
		if l.model != "fake" {
			t.Errorf("%s: model = %q, want the configured model's name when the adapter reports none", l.lens.name, l.model)
		}
		if l.elapsed <= 0 {
			t.Errorf("%s: elapsed = %v, want > 0", l.lens.name, l.elapsed)
		}
	}
}

// A lens that produced no output at all (no state key written) is reported as not having run,
// rather than reading as a clean lens, and does not fail the review.
func TestReviewMarksSilentLens(t *testing.T) {
	// A model that yields nothing writes no OutputKey, so every category is silent; glue's output
	// is blank text, which counts as silent too.
	e := reviewEngine("", func(d *Deps) { d.BaseLLM = silentLLM{}; d.CodeLLM = silentLLM{} })
	out, err := e.review(context.Background(), []githubapi.PRFile{{Path: "main.go", Patch: "+x"}}, nil, nil)
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	for _, l := range out.lenses {
		if l.ran {
			t.Errorf("%s reported as ran with no output", l.lens.name)
		}
	}
}

// silentLLM never yields a response, like a lens whose generation produced nothing.
type silentLLM struct{}

func (silentLLM) Name() string { return "silent" }

func (silentLLM) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(func(*model.LLMResponse, error) bool) {}
}
