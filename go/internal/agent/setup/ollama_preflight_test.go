package setup

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// tagServer serves /api/tags with the given model names, the shape Ollama reports its locally
// pulled models in.
func tagServer(t *testing.T, names ...string) string {
	t.Helper()
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = `{"name":"` + n + `"}`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"models":[` + strings.Join(quoted, ",") + `]}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestVerifyOllamaModelsPresent(t *testing.T) {
	host := tagServer(t, "gemma4:12b", "gemma4:26b", "qwen2.5-coder:32b")
	if err := VerifyOllamaModels(context.Background(), host, "gemma4:12b", "gemma4:26b"); err != nil {
		t.Fatalf("VerifyOllamaModels: %v", err)
	}
}

// The point of the check is the message: a tag that was never pulled must produce something a
// human can act on without going and reading the code.
func TestVerifyOllamaModelsNamesTheMissingTagAndTheFix(t *testing.T) {
	host := tagServer(t, "gemma3:12b", "gemma3:27b")

	err := VerifyOllamaModels(context.Background(), host, "gemma4:12b", "gemma3:12b")
	if err == nil {
		t.Fatal("a tag the server does not have must be an error")
	}
	if errors.Is(err, ErrOllamaUnreachable) {
		t.Fatal("a reachable server missing a model must not read as unreachable")
	}
	msg := err.Error()
	for _, want := range []string{
		`"gemma4:12b"`,           // the tag that is wrong
		"ollama pull gemma4:12b", // how to fix it
		"gemma3:12b",             // what the server does have — usually makes the right tag obvious
		"gemma3:27b",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q: %s", want, msg)
		}
	}
	if strings.Contains(msg, `"gemma3:12b"`) {
		t.Errorf("a model that IS present must not be reported missing: %s", msg)
	}
}

// A server that is up but has nothing pulled is the first-run case, and "available: " with an
// empty list would read as a bug rather than an explanation.
func TestVerifyOllamaModelsEmptyServer(t *testing.T) {
	err := VerifyOllamaModels(context.Background(), tagServer(t), "gemma4:12b")
	if err == nil {
		t.Fatal("expected an error when the server has no models")
	}
	if !strings.Contains(err.Error(), "no models pulled at all") {
		t.Errorf("error should say the server is empty: %v", err)
	}
}

// Unreachable is a distinct outcome, because the caller treats it differently: Ollama not being
// up yet is ordinary in local development and must not block startup, while a reachable server
// missing a model is a configuration error that will fail every run.
func TestVerifyOllamaModelsUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	host := srv.URL
	srv.Close() // nothing is listening now

	err := VerifyOllamaModels(context.Background(), host, "gemma4:12b")
	if !errors.Is(err, ErrOllamaUnreachable) {
		t.Fatalf("err = %v, want ErrOllamaUnreachable", err)
	}
	if !strings.Contains(err.Error(), host) {
		t.Errorf("error should name the host it tried: %v", err)
	}
}

// Ollama's implicit ":latest" has to be applied on both sides, or a pulled model reads as
// missing purely because of how the tag was written.
func TestVerifyOllamaModelsAppliesImplicitLatest(t *testing.T) {
	if err := VerifyOllamaModels(context.Background(), tagServer(t, "llama3.3:latest"), "llama3.3"); err != nil {
		t.Errorf("a bare name should match the server's :latest entry: %v", err)
	}
	if err := VerifyOllamaModels(context.Background(), tagServer(t, "llama3.3"), "llama3.3:latest"); err != nil {
		t.Errorf("an explicit :latest should match a bare server entry: %v", err)
	}
}

// Callers pass the base and code tiers straight through; those are commonly the same tag, and
// the code tier may be unset. Neither should produce a duplicate complaint or a spurious one.
func TestVerifyOllamaModelsSkipsBlanksAndDuplicates(t *testing.T) {
	if err := VerifyOllamaModels(context.Background(), tagServer(t, "gemma4:12b"), "gemma4:12b", "gemma4:12b", ""); err != nil {
		t.Errorf("duplicate and empty tags should be ignored: %v", err)
	}
	// With nothing to check, the server is never contacted — so an unreachable host is fine.
	if err := VerifyOllamaModels(context.Background(), "http://127.0.0.1:1", "", "  "); err != nil {
		t.Errorf("no tags to verify should be a no-op, got %v", err)
	}
}

func TestVerifyOllamaModelsRejectsBadHost(t *testing.T) {
	if err := VerifyOllamaModels(context.Background(), "://not a url", "gemma4:12b"); err == nil {
		t.Fatal("an unparseable host must be an error")
	}
}
