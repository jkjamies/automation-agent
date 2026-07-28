package setup

import (
	"context"
	"iter"

	"google.golang.org/adk/v2/model"
)

// LLMLimiter bounds how many model calls run concurrently across the whole process.
//
// The bound lives on the model rather than at the call sites because the call sites are
// exactly where it gets forgotten: the fixers fan out one analyzer per file, the reviewer
// fans out one agent per category lens, and both do it through the agent framework's
// parallel primitives, which schedule sub-agents without knowing what a model call costs.
// Wrapping the model once at startup makes the limit unbypassable — a new fan-out added
// later inherits it — and lets a single limiter be shared by the base and code models, which
// is what a shared backend (one GPU, one project quota) actually needs.
//
// Nothing here nests: the tools an agent can call (read_file, list_dir, get_rule) are local,
// so a model call never waits on another model call and the semaphore cannot deadlock.
type LLMLimiter struct {
	sem chan struct{}
}

// NewLLMLimiter returns a limiter admitting at most n concurrent calls. An n below 1
// yields nil, which Wrap treats as "no limit" — so a caller that does not want bounding
// does not have to special-case it.
func NewLLMLimiter(n int) *LLMLimiter {
	if n < 1 {
		return nil
	}
	return &LLMLimiter{sem: make(chan struct{}, n)}
}

// Wrap returns inner bounded by this limiter. A nil limiter (or a nil inner) returns inner
// unchanged, so wrapping is always safe to apply unconditionally.
func (l *LLMLimiter) Wrap(inner model.LLM) model.LLM {
	if l == nil || inner == nil {
		return inner
	}
	return &limitedLLM{inner: inner, sem: l.sem}
}

// limitedLLM holds a slot for the whole generation, not just its start. A streamed response
// occupies the backend until the last token, so releasing at first byte would let unbounded
// generations overlap and defeat the limit.
type limitedLLM struct {
	inner model.LLM
	sem   chan struct{}
}

var _ model.LLM = (*limitedLLM)(nil)

func (m *limitedLLM) Name() string { return m.inner.Name() }

func (m *limitedLLM) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		// Wait for a slot, but stay cancellable: under a burst the queue can be long, and a
		// caller whose context expires (a Cloud Tasks dispatch deadline, a shutdown) must not
		// be stuck behind it.
		select {
		case m.sem <- struct{}{}:
		case <-ctx.Done():
			yield(nil, ctx.Err())
			return
		}
		// Released on every exit: normal completion, an error, or the consumer breaking early.
		defer func() { <-m.sem }()

		for resp, err := range m.inner.GenerateContent(ctx, req, stream) {
			if !yield(resp, err) {
				return
			}
		}
	}
}
