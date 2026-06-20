package setup

import (
	"testing"
	"testing/fstest"
)

func TestPromptsGet(t *testing.T) {
	fsys := fstest.MapFS{
		"prompts/summary.md": &fstest.MapFile{Data: []byte("  Summarize this.\n")},
	}
	p := NewPrompts(fsys)

	got, err := p.Get("summary")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "Summarize this." {
		t.Errorf("Get = %q, want %q", got, "Summarize this.")
	}

	if _, err := p.Get("missing"); err == nil {
		t.Error("expected error for missing prompt")
	}
}

func TestPromptsMustGet(t *testing.T) {
	fsys := fstest.MapFS{"prompts/x.md": &fstest.MapFile{Data: []byte("ok")}}
	p := NewPrompts(fsys)
	if p.MustGet("x") != "ok" {
		t.Error("MustGet returned wrong value")
	}

	defer func() {
		if recover() == nil {
			t.Error("MustGet should panic on missing prompt")
		}
	}()
	p.MustGet("nope")
}
