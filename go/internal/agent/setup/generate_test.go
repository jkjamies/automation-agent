package setup

import (
	"context"
	"errors"
	"iter"
	"testing"

	"google.golang.org/adk/model"
)

type fixedLLM struct {
	text string
	err  error
}

func (f fixedLLM) Name() string { return "fixed" }
func (f fixedLLM) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if f.err != nil {
			yield(nil, f.err)
			return
		}
		yield(&model.LLMResponse{Content: AssistantText(f.text)}, nil)
	}
}

func TestGenerateText(t *testing.T) {
	out, err := GenerateText(context.Background(), fixedLLM{text: "the answer"}, "be terse", "question?")
	if err != nil {
		t.Fatalf("GenerateText: %v", err)
	}
	if out != "the answer" {
		t.Errorf("out = %q", out)
	}
}

func TestGenerateTextError(t *testing.T) {
	if _, err := GenerateText(context.Background(), fixedLLM{err: errors.New("model down")}, "s", "u"); err == nil {
		t.Fatal("expected error from model")
	}
}
